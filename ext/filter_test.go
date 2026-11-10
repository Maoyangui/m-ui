package ext

import (
	"encoding/json"
	"fmt"
	"strings"
	"testing"

	"github.com/Maoyangui/m-ui/upstream"
)

func clashNames(it Items) []string {
	out := []string{}
	for _, p := range it.Clash() {
		n, _ := p["name"].(string)
		out = append(out, n)
	}
	return out
}

func linkNames(it Items) []string {
	out := []string{}
	for _, l := range it.Links() {
		out = append(out, LinkName(l))
	}
	return out
}

// newFilter 空的筛选条件(测试里直接调 Hide / Show / Rename)。
func newFilter(exclude string) Filter { return ParseFilter(nil, nil, nil, nil, exclude) }

func TestFilterHidesAndRenames(t *testing.T) {
	vmess := `vmess://eyJ2IjoiMiIsInBzIjoi6aaZ5rivIiwiYWRkIjoiMS4xLjEuMSIsInBvcnQiOiI0NDMiLCJpZCI6ImEifQ==`
	it := Parse("trojan://p@1.1.1.1:1#%E5%AE%98%E7%BD%91\ntrojan://p@1.1.1.1:2#%E6%97%A5%E6%9C%AC\ntrojan://p@1.1.1.1:3#US\n" + vmess)
	if fmt.Sprint(clashNames(it)) != "[官网 日本 US 香港]" {
		t.Fatalf("解析出的名字不对: %v", clashNames(it))
	}
	hidden, _ := json.Marshal([]string{"US"})
	renames, _ := json.Marshal(map[string]string{"日本": "JP 高速", "香港": "HK"})
	f := ParseFilter(hidden, renames, nil, nil, "官网, 到期")
	got := f.Apply(it)
	if fmt.Sprint(linkNames(got)) != "[JP 高速 HK]" {
		t.Fatalf("链接筛选不对: %v", linkNames(got))
	}
	if fmt.Sprint(clashNames(got)) != "[JP 高速 HK]" {
		t.Fatalf("Clash 筛选不对: %v", clashNames(got))
	}
	// 原数据不能被改:抓取缓存解析一次会用很多回
	if it.Nodes[1].Clash["name"] != "日本" || it.Nodes[1].Name != "日本" {
		t.Fatal("改名不能改到原数据")
	}
	// 改过名的 vmess 还是合法链接
	if p, err := upstream.ParseLink(got.Links()[1]); err != nil || p.Name != "HK" {
		t.Fatalf("vmess 改名失败: %v %s", err, got.Links()[1])
	}
}

func TestFilterEmptyKeepsEverything(t *testing.T) {
	it := Parse("trojan://p@1.1.1.1:1#a")
	got := ParseFilter(nil, nil, nil, nil, " ").Apply(it)
	if len(got.Links()) != 1 || len(got.Clash()) != 1 {
		t.Fatalf("没有筛选条件不该动: %+v", got)
	}
	// 关键词不分大小写、中英文分隔符都认
	f := ParseFilter(nil, nil, nil, nil, "EXPIRE，官网|到期、流量")
	r := Roster{Names: []string{"expire 2026", "官网 xx", "到期:明天", "剩余流量", "日本 01"}, Keys: []string{"1", "2", "3", "4", "5"}}
	for i, d := range f.Decide(r) {
		if want := i < 4; d.Hidden != want || d.Keyword != want {
			t.Errorf("%s: 关键词排除应为 %v,得到 %+v", r.Names[i], want, d)
		}
	}
}

const infoBefore = "proxies:\n" +
	"  - {name: '剩余流量:120.53 GB', type: ss, server: 127.0.0.1, port: 1, cipher: aes-128-gcm, password: x}\n" +
	"  - {name: '距离下次重置剩余:12 天', type: ss, server: 127.0.0.1, port: 2, cipher: aes-128-gcm, password: x}\n" +
	"  - {name: '香港 01', type: ss, server: 1.2.3.4, port: 443, cipher: aes-128-gcm, password: x}\n"

// 审计 I5:信息节点的名字里带剩余流量 / 天数,每次刷新都变。以前只按原名记,30 分钟后名字一变就重新输出;
// 现在按指纹也记着,名字变了照样隐藏;按名字隐藏的普通节点不受影响。
func TestFilterKeyMatchSurvivesRename(t *testing.T) {
	after := strings.NewReplacer("120.53", "118.07", "12 天", "11 天").Replace(infoBefore)
	it := Parse(infoBefore)
	f := newFilter("")
	r := RosterOf(it)
	f.Hide(r, it.Nodes[0].Name, it.Nodes[0].Key)
	f.Hide(r, it.Nodes[1].Name, it.Nodes[1].Key)
	if got := clashNames(f.Apply(it)); fmt.Sprint(got) != "[香港 01]" {
		t.Fatalf("刷新前应只剩 香港 01: %v", got)
	}
	// 落库再读回来(和面板保存、订阅服务读库同一条路径)
	h, rn, hk, rk := f.Encode()
	f2 := ParseFilter(h, rn, hk, rk, "")
	got := f2.Apply(Parse(after))
	if fmt.Sprint(clashNames(got)) != "[香港 01]" || fmt.Sprint(linkNames(got)) != "[香港 01]" {
		t.Fatalf("刷新后信息节点改了名,按指纹仍应隐藏: clash=%v links=%v", clashNames(got), linkNames(got))
	}
	// 普通节点按名字隐藏:换了地址(指纹变了)照样认得名字
	f3 := newFilter("")
	f3.Hide(RosterOf(it), "香港 01", it.Nodes[2].Key)
	moved := strings.Replace(after, "1.2.3.4", "5.6.7.8", 1)
	if got := clashNames(f3.Apply(Parse(moved))); fmt.Sprint(got) != "[剩余流量:118.07 GB 距离下次重置剩余:11 天]" {
		t.Fatalf("换了地址、名字没变的节点应按名字继续隐藏: %v", got)
	}
}

// 同名的两个节点(地址不同):只隐藏 / 改名其中一个,另一个不受牵连;以前按名字记,两个一起隐藏,单独显示一个也存不住。
func TestFilterDuplicateNamesByKey(t *testing.T) {
	y := "proxies:\n" +
		"  - {name: '香港 IPLC', type: ss, server: 1.1.1.1, port: 1, cipher: aes-128-gcm, password: x}\n" +
		"  - {name: '香港 IPLC', type: ss, server: 2.2.2.2, port: 2, cipher: aes-128-gcm, password: x}\n"
	it := Parse(y)
	r := RosterOf(it)
	if it.Nodes[0].Key == it.Nodes[1].Key {
		t.Fatal("地址不同的节点指纹应不同")
	}
	f := newFilter("")
	f.Hide(r, "香港 IPLC", it.Nodes[0].Key)
	if len(f.Hidden) != 0 {
		t.Fatalf("同名节点不该按名字记: %v", f.Hidden)
	}
	got := f.Apply(it)
	if len(got.Nodes) != 1 || got.Nodes[0].Key != it.Nodes[1].Key {
		t.Fatalf("只应隐藏第一个: %+v", got.Nodes)
	}
	f.Rename(r, "香港 IPLC", it.Nodes[1].Key, "HK-2")
	if got := clashNames(f.Apply(it)); fmt.Sprint(got) != "[HK-2]" {
		t.Fatalf("只应改第二个的名字: %v", got)
	}
	// 老数据按名字隐藏了这两个:单独显示其中一个,另一个拆成按指纹的记录继续隐藏
	legacy, _ := json.Marshal([]string{"香港 IPLC"})
	old := ParseFilter(legacy, nil, nil, nil, "")
	old.Show(r, "香港 IPLC", it.Nodes[1].Key)
	got = old.Apply(it)
	if len(got.Nodes) != 1 || got.Nodes[0].Key != it.Nodes[1].Key {
		t.Fatalf("单独显示第二个后应只输出它: %+v", got.Nodes)
	}
	if old.Hidden["香港 IPLC"] || old.HiddenKeys[it.Nodes[0].Key] != "" {
		t.Fatalf("名字记录应拆成指纹记录: hidden=%v keys=%v", old.Hidden, old.HiddenKeys)
	}
	if _, ok := old.HiddenKeys[it.Nodes[0].Key]; !ok {
		t.Fatalf("第一个应按指纹继续隐藏: %v", old.HiddenKeys)
	}
}

// 机场删掉一个节点后重新编号:"香港 04" 改叫 "香港 03",原来的 "香港 05" 改叫 "香港 04"。
// 隐藏跟着节点走(按指纹),"香港 04" 这个名字不再扣到另一个节点头上。
func TestFilterRenumberDoesNotHideNeighbour(t *testing.T) {
	before := "proxies:\n" +
		"  - {name: '香港 03', type: trojan, server: hk3.example.com, port: 443, password: p}\n" +
		"  - {name: '香港 04', type: trojan, server: hk4.example.com, port: 443, password: p}\n" +
		"  - {name: '香港 05', type: trojan, server: hk5.example.com, port: 443, password: p}\n"
	after := "proxies:\n" +
		"  - {name: '香港 03', type: trojan, server: hk4.example.com, port: 443, password: p}\n" +
		"  - {name: '香港 04', type: trojan, server: hk5.example.com, port: 443, password: p}\n"
	it := Parse(before)
	f := newFilter("")
	f.Hide(RosterOf(it), "香港 04", it.Nodes[1].Key)
	got := f.Apply(Parse(after))
	if fmt.Sprint(clashNames(got)) != "[香港 04]" {
		t.Fatalf("应只隐藏原来那个节点(现在叫 香港 03),得到 %v", clashNames(got))
	}
	if got.Nodes[0].Clash["server"] != "hk5.example.com" {
		t.Fatalf("留下的应是 hk5: %v", got.Nodes[0].Clash)
	}
	// 机场重置了订阅、密码全换(指纹全变):名字还在,按名字照样隐藏
	rotated := strings.ReplaceAll(before, "password: p", "password: q")
	if got := clashNames(f.Apply(Parse(rotated))); fmt.Sprint(got) != "[香港 03 香港 05]" {
		t.Fatalf("指纹全变后应按名字认出: %v", got)
	}
}

// 审计 I5:链接侧和 Clash 侧取名一致。无名链接(名字回落为服务器地址)、名字里带原样 #、vmess 的数字 ps,
// 以前 Clash 订阅里隐藏了,链接订阅(v2rayN / 小火箭 / sing-box)里还在。
func TestApplyLinksUseSameNameAsClash(t *testing.T) {
	vmNum := "vmess://" + b64(`{"v":"2","ps":123,"add":"9.9.9.9","port":"443","id":"a"}`)
	raw := "trojan://pw@1.2.3.4:443?security=tls&sni=a.example.com\n" +
		"trojan://pw@5.6.7.8:443#HK#01\n" +
		vmNum + "\n" +
		"vless://00000000-0000-0000-0000-000000000000@7.7.7.7:443?security=tls#VL"
	it := Parse(b64(raw))
	if fmt.Sprint(clashNames(it)) != "[1.2.3.4 HK#01 123 VL]" || fmt.Sprint(linkNames(it)) != fmt.Sprint(clashNames(it)) {
		t.Fatalf("两侧名字应一致: clash=%v links=%v", clashNames(it), linkNames(it))
	}
	r := RosterOf(it)
	f := newFilter("")
	for i := 0; i < 3; i++ {
		f.Hide(r, it.Nodes[i].Name, it.Nodes[i].Key)
	}
	got := f.Apply(it)
	if fmt.Sprint(clashNames(got)) != "[VL]" || fmt.Sprint(linkNames(got)) != "[VL]" {
		t.Fatalf("两侧都只应剩 VL: clash=%v links=%v", clashNames(got), linkNames(got))
	}
	// 改名也两侧一致,链接仍能解析
	f2 := newFilter("")
	f2.Rename(r, "HK#01", it.Nodes[1].Key, "香港#1")
	got = f2.Apply(it)
	p, err := upstream.ParseLink(got.Links()[1])
	if err != nil || p.Name != "香港#1" || got.Clash()[1]["name"] != "香港#1" {
		t.Fatalf("改名两侧不一致: %v %v %v", err, p, got.Clash()[1]["name"])
	}
}

// 合并保存的几种操作:改名 / 恢复原名、清理对不上的记录、上限。
func TestFilterRenameStaleAndCap(t *testing.T) {
	it := Parse(infoBefore)
	r := RosterOf(it)
	f := newFilter("")
	f.Rename(r, "香港 01", it.Nodes[2].Key, " HK ")
	if f.Renames["香港 01"] != "HK" || f.RenameKeys[it.Nodes[2].Key].To != "HK" {
		t.Fatalf("改名应同时按名字和指纹记(去掉两端空白): %v %v", f.Renames, f.RenameKeys)
	}
	f.Rename(r, "香港 01", it.Nodes[2].Key, "香港 01")
	if len(f.Renames) != 0 || len(f.RenameKeys) != 0 {
		t.Fatalf("改回原名应清掉记录: %v %v", f.Renames, f.RenameKeys)
	}
	// 只剩一个节点的列表里,别的记录都对不上
	f.Hide(r, it.Nodes[0].Name, it.Nodes[0].Key)
	f.Rename(r, "香港 01", it.Nodes[2].Key, "HK")
	small := Parse("proxies:\n  - {name: '香港 01', type: ss, server: 1.2.3.4, port: 443, cipher: aes-128-gcm, password: x}\n")
	sr := RosterOf(small)
	if n := f.Stale(sr); n != 2 {
		t.Fatalf("应有 2 条对不上(信息节点的名字与指纹): %d", n)
	}
	f.DropStale(sr)
	if len(f.Hidden)+len(f.HiddenKeys) != 0 || f.Renames["香港 01"] != "HK" {
		t.Fatalf("清理应只删对不上的: %+v", f)
	}
	for i := 0; i <= MaxPrefEntries; i++ {
		f.HiddenKeys[fmt.Sprint(i)] = ""
	}
	if f.Check() == nil {
		t.Fatal("超过上限应报错")
	}
}
