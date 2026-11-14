package web

import (
	"encoding/json"
	"fmt"
	"net/http"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/Maoyangui/m-ui/database"
	"github.com/Maoyangui/m-ui/database/model"
	"github.com/Maoyangui/m-ui/ext"
	"github.com/Maoyangui/m-ui/runner"
)

const i5v1 = `proxies:
  - {name: '剩余流量:120.53 GB', type: ss, server: 127.0.0.1, port: 1, cipher: aes-128-gcm, password: x}
  - {name: '香港 01', type: ss, server: 1.2.3.4, port: 443, cipher: aes-128-gcm, password: x}
  - {name: '日本 01', type: ss, server: 1.2.3.5, port: 443, cipher: aes-128-gcm, password: x}
`

// 刷新后:日本 01 暂时下线(机场维护 / 这次只抓到一部分),信息节点名字变了
const i5v2 = `proxies:
  - {name: '剩余流量:118.07 GB', type: ss, server: 127.0.0.1, port: 1, cipher: aes-128-gcm, password: x}
  - {name: '香港 01', type: ss, server: 1.2.3.4, port: 443, cipher: aes-128-gcm, password: x}
`

// 再刷新:日本 01 回来了
const i5v3 = `proxies:
  - {name: '剩余流量:117.90 GB', type: ss, server: 127.0.0.1, port: 1, cipher: aes-128-gcm, password: x}
  - {name: '香港 01', type: ss, server: 1.2.3.4, port: 443, cipher: aes-128-gcm, password: x}
  - {name: '日本 01', type: ss, server: 1.2.3.5, port: 443, cipher: aes-128-gcm, password: x}
`

func setCache(s *Server, body string, at int64) {
	s.db.Model(&model.ExtNode{}).Where("id = ?", 1).Updates(map[string]interface{}{"cache": body, "last_fetch": at, "content_sig": ext.ContentSig(ext.Parse(body))})
}

// outputNames 订阅里实际输出的节点(和 sub/server.go 的 externalFor 同一条路径),Clash 与链接两侧都看。
func outputNames(t *testing.T, s *Server) []string {
	t.Helper()
	var e model.ExtNode
	s.db.First(&e, 1)
	it := ext.Output(e)
	out := []string{}
	for _, p := range it.Clash() {
		out = append(out, p["name"].(string))
	}
	var links []string
	for _, l := range it.Links() {
		links = append(links, ext.LinkName(l))
	}
	if fmt.Sprint(links) != fmt.Sprint(out) && !(len(links) == 0 && len(out) == 0) {
		t.Fatalf("链接侧与 Clash 侧输出不一致: %v vs %v", links, out)
	}
	return out
}

// savePrefs 按面板的做法发一次增量(带当前版本号)。
func savePrefs(t *testing.T, s *Server, kind string, ops ...string) map[string]interface{} {
	t.Helper()
	_, cur := extCall(t, s, http.MethodGet, "1/nodes", "")
	body := fmt.Sprintf(`{"rev":%v,%q:[%s]}`, cur["rev"], kind, strings.Join(ops, ","))
	code, out := extCall(t, s, http.MethodPut, "1/nodes/prefs", body)
	if code != http.StatusOK {
		t.Fatalf("保存失败 %d %v (%s)", code, out, body)
	}
	return out
}

func opFor(t *testing.T, s *Server, name, to string) string {
	t.Helper()
	for _, n := range listNodes(t, s) {
		if n["name"] == name {
			return fmt.Sprintf(`{"name":%q,"key":%q,"to":%q}`, name, n["key"], to)
		}
	}
	t.Fatalf("列表里没有 %s", name)
	return ""
}

// 审计 I5:以前前端用当前展开的列表重建整份隐藏名单、服务端整份替换,列表里暂时不在的隐藏项(日本 01 下线了一轮)
// 下一次随便保存点什么就被抹掉,节点回来后又输出;信息节点换了名也重新冒出来。现在合并保存、按指纹认。
func TestExtPrefsMergeKeepsOffListEntries(t *testing.T) {
	s := extNodesServer(t)
	setCache(s, i5v1, 1000)
	savePrefs(t, s, "hide", opFor(t, s, "剩余流量:120.53 GB", ""), opFor(t, s, "日本 01", ""))
	if got := outputNames(t, s); fmt.Sprint(got) != "[香港 01]" {
		t.Fatalf("v1 输出: %v", got)
	}
	setCache(s, i5v2, 2800) // 自动刷新:日本 01 这次没有,信息节点改了名
	if got := outputNames(t, s); fmt.Sprint(got) != "[香港 01]" {
		t.Fatalf("v2 信息节点换了名也应继续隐藏: %v", got)
	}
	savePrefs(t, s, "rename", opFor(t, s, "香港 01", "HK")) // 这期间随便改了点别的
	var e model.ExtNode
	s.db.First(&e, 1)
	if !strings.Contains(string(e.Hidden), "日本 01") {
		t.Fatalf("列表外的隐藏项不该被抹掉: hidden=%s keys=%s", e.Hidden, e.HiddenKeys)
	}
	setCache(s, i5v3, 4600) // 日本 01 回来了
	if got := outputNames(t, s); fmt.Sprint(got) != "[HK]" {
		t.Fatalf("v3 日本 01 与信息节点都应继续隐藏: %v", got)
	}
	_, out := extCall(t, s, http.MethodGet, "1/nodes", "")
	if out["stale"] != float64(1) { // 第一次隐藏信息节点时记下的名字已对不上
		t.Fatalf("应有 1 条对不上的记录: %v", out["stale"])
	}
}

// 多个标签页:版本号对不上返回 409(前端重新拉取后再试),老页面整份提交的格式也拒绝,不再整份覆盖。
func TestExtPrefsConcurrentRev(t *testing.T) {
	s := extNodesServer(t)
	a := opFor(t, s, "a", "")
	b := opFor(t, s, "b", "")
	if code, out := extCall(t, s, http.MethodPut, "1/nodes/prefs", `{"rev":0,"hide":[`+a+`]}`); code != http.StatusOK || out["rev"] != float64(1) {
		t.Fatalf("第一次保存应成功: %d %v", code, out)
	}
	if code, out := extCall(t, s, http.MethodPut, "1/nodes/prefs", `{"rev":0,"hide":[`+b+`]}`); code != http.StatusConflict {
		t.Fatalf("版本号过期应 409: %d %v", code, out)
	}
	if code, _ := extCall(t, s, http.MethodPut, "1/nodes/prefs", `{"hidden":["b"],"renames":{}}`); code != http.StatusConflict {
		t.Fatalf("老格式应 409: %d", code)
	}
	var e model.ExtNode
	s.db.First(&e, 1)
	if strings.Contains(string(e.Hidden), `"b"`) || !strings.Contains(string(e.Hidden), `"a"`) {
		t.Fatalf("被拒的请求不该生效: %s", e.Hidden)
	}
	if code, out := extCall(t, s, http.MethodPut, "1/nodes/prefs", `{"rev":1,"hide":[`+b+`]}`); code != http.StatusOK || len(outputNames(t, s)) != 0 {
		t.Fatalf("新版本号应能保存: %d %v", code, out)
	}
}

// 未匹配的记录:展开时报个数,点"清理"才删;记录有上限。
func TestExtNodesStaleEntries(t *testing.T) {
	s := extNodesServer(t)
	savePrefs(t, s, "hide", opFor(t, s, "a", ""))
	setCache(s, `proxies:
  - {name: b, type: tuic, server: 1.2.3.4, port: 444, uuid: 00000000-0000-0000-0000-000000000000, password: p, sni: x.example.com}
`, 2000)
	_, out := extCall(t, s, http.MethodGet, "1/nodes", "")
	if out["stale"] != float64(2) { // a 的名字与指纹
		t.Fatalf("应有 2 条对不上: %v", out["stale"])
	}
	code, out := extCall(t, s, http.MethodPut, "1/nodes/prefs", fmt.Sprintf(`{"rev":%v,"clearStale":true}`, out["rev"]))
	if code != http.StatusOK || out["stale"] != float64(0) {
		t.Fatalf("清理后应为 0: %d %v", code, out)
	}
	var e model.ExtNode
	s.db.First(&e, 1)
	if string(e.Hidden) != "[]" || string(e.HiddenKeys) != "{}" {
		t.Fatalf("清理后库里应为空: %s %s", e.Hidden, e.HiddenKeys)
	}
	many := make(map[string]string, ext.MaxPrefEntries+1)
	for i := 0; i <= ext.MaxPrefEntries; i++ {
		many[fmt.Sprint(i)] = ""
	}
	raw, _ := json.Marshal(many)
	s.db.Model(&model.ExtNode{}).Where("id = 1").Update("hidden_keys", raw)
	_, out = extCall(t, s, http.MethodGet, "1/nodes", "")
	code, out = extCall(t, s, http.MethodPut, "1/nodes/prefs", fmt.Sprintf(`{"rev":%v,"hide":[%s]}`, out["rev"], opFor(t, s, "b", "")))
	if code != http.StatusBadRequest || !strings.Contains(out["error"].(string), "超过") {
		t.Fatalf("超过上限应拒绝: %d %v", code, out)
	}
}

// "测速全部"默认只测会输出的节点(信息节点不占测速);勾"含已隐藏"才连隐藏的一起测。返回实际测的节点指纹。
func TestExtTestSkipsHiddenByDefault(t *testing.T) {
	s := extNodesServer(t)
	savePrefs(t, s, "hide", opFor(t, s, "a", ""))
	keyB := nodeKeyAt(t, s, 1)
	code, out := extCall(t, s, http.MethodPost, "1/nodes/test", `{}`)
	if code != http.StatusOK {
		t.Fatalf("起任务失败: %d %v", code, out)
	}
	if keys, _ := out["keys"].([]interface{}); len(keys) != 1 || keys[0] != keyB {
		t.Fatalf("默认只测输出的节点 b: %v", out["keys"])
	}
	waitJob(t, s, out["job"].(string))
	code, out = extCall(t, s, http.MethodPost, "1/nodes/test", `{"includeHidden":true}`)
	if keys, _ := out["keys"].([]interface{}); code != http.StatusOK || len(keys) != 2 {
		t.Fatalf("含已隐藏应测两个: %d %v", code, out)
	}
	waitJob(t, s, out["job"].(string))
	// 按指纹选的节点:列表刷新、换了顺序也认得;指纹已经没了就 409
	code, out = extCall(t, s, http.MethodPost, "1/nodes/test", `{"keys":["`+keyB+`"]}`)
	if code != http.StatusOK {
		t.Fatalf("按指纹测失败: %d %v", code, out)
	}
	waitJob(t, s, out["job"].(string))
	if code, _ := extCall(t, s, http.MethodPost, "1/nodes/test", `{"keys":["0000000000000000"]}`); code != http.StatusConflict {
		t.Fatalf("没有的指纹应 409: %d", code)
	}
}

func waitShort() { time.Sleep(20 * time.Millisecond) }

func waitJob(t *testing.T, s *Server, job string) map[string]interface{} {
	t.Helper()
	var st map[string]interface{}
	for i := 0; i < 100; i++ {
		_, st = extCall(t, s, http.MethodGet, "1/nodes/test/"+job, "")
		if st["done"] == true {
			return st
		}
		waitShort()
	}
	t.Fatalf("任务没结束: %v", st)
	return nil
}

// 协议转不成出站的节点测速格记"测不了"(不是"不通"),原因写清楚。
func TestExtTestUnsupportedIsNotFail(t *testing.T) {
	s := extNodesServer(t)
	setCache(s, `proxies:
  - {name: s, type: snell, server: 1.2.3.4, port: 1, psk: k}
`, 2000)
	code, out := extCall(t, s, http.MethodPost, "1/nodes/test", `{}`)
	if code != http.StatusOK {
		t.Fatalf("起任务失败: %d %v", code, out)
	}
	st := waitJob(t, s, out["job"].(string))
	for _, row := range st["results"].(map[string]interface{}) {
		for _, c := range row.(map[string]interface{}) {
			cell := c.(map[string]interface{})
			if cell["state"] != cellUnsupported || !strings.Contains(cell["error"].(string), "snell") {
				t.Fatalf("应记测不了并说明协议: %v", cell)
			}
		}
	}
}

// 添加为上游按指纹选节点:列表刷新、换了顺序也加对;出站和测速同一份无损转换(ss 的插件不丢)。
func TestExtAddUpstreamByKeyLossless(t *testing.T) {
	s := extNodesServer(t)
	setCache(s, `proxies:
  - {name: o, type: ss, server: 1.2.3.4, port: 8388, cipher: aes-128-gcm, password: x, plugin: obfs, plugin-opts: {mode: tls, host: bing.com}}
  - {name: v, type: vmess, server: 1.2.3.5, port: 443, uuid: 00000000-0000-0000-0000-000000000000, alterId: 0, cipher: auto, tls: true, skip-cert-verify: true, servername: v.example.com}
`, 2000)
	keyO, keyV := nodeKeyAt(t, s, 0), nodeKeyAt(t, s, 1)
	setCache(s, `proxies:
  - {name: v, type: vmess, server: 1.2.3.5, port: 443, uuid: 00000000-0000-0000-0000-000000000000, alterId: 0, cipher: auto, tls: true, skip-cert-verify: true, servername: v.example.com}
  - {name: o, type: ss, server: 1.2.3.4, port: 8388, cipher: aes-128-gcm, password: x, plugin: obfs, plugin-opts: {mode: tls, host: bing.com}}
`, 3000) // 对方调换了顺序
	code, out := extCall(t, s, http.MethodPost, "1/nodes/add-upstream", `{"items":[{"key":"`+keyO+`","name":"up-o"},{"key":"`+keyV+`","name":"up-v"},{"key":"ffffffffffffffff","name":"gone"}]}`)
	if code != http.StatusOK {
		t.Fatalf("应 200: %d %v", code, out)
	}
	rs := out["results"].([]interface{})
	if rs[0].(map[string]interface{})["ok"] != true || rs[1].(map[string]interface{})["ok"] != true || rs[2].(map[string]interface{})["ok"] == true {
		t.Fatalf("前两个应成功,没有的指纹应失败: %v", rs)
	}
	var ups []model.Upstream
	s.db.Order("id asc").Find(&ups)
	if len(ups) != 2 || ups[0].Name != "up-o" || ups[0].Type != "shadowsocks" || ups[1].Type != "vmess" {
		t.Fatalf("上游没建对: %+v", ups)
	}
	if !strings.Contains(string(ups[0].Options), `"plugin":"obfs-local"`) || !strings.Contains(string(ups[0].Options), `obfs=tls;obfs-host=bing.com`) {
		t.Fatalf("ss 插件不能丢: %s", ups[0].Options)
	}
	if !strings.Contains(string(ups[1].Options), `"insecure":true`) {
		t.Fatalf("vmess 的跳过证书校验不能丢: %s", ups[1].Options)
	}
}

// 大陆连通:同一时间只跑一个(和服务器检测共用测点额度),在跑时再发起直接拒绝,不重复扣额度;
// UDP 协议(hysteria2 / tuic)只能 ping。
func TestExtReachSingleJob(t *testing.T) {
	run, err := runner.New(filepath.Join(t.TempDir(), "m.db"))
	if err != nil {
		t.Fatal(err)
	}
	defer database.Close(run.DB())
	s := NewServer(run)
	run.DB().Create(&model.ExtNode{Name: "机场A", Type: "sub", Value: "https://example.com/sub", Enabled: true, Cache: extNodesYAML})
	key := nodeKeyAt(t, s, 0)
	release, err := run.Reach().BeginExt() // 另一个外部节点的检测正在跑
	if err != nil {
		t.Fatal(err)
	}
	code, out := extCall(t, s, http.MethodPost, "1/nodes/reach", `{"keys":["`+key+`"]}`)
	if code != http.StatusBadRequest || !strings.Contains(out["error"].(string), "正在检测") {
		t.Fatalf("已有检测在跑时应拒绝: %d %v", code, out)
	}
	if err := run.Reach().Run(nil); err == nil {
		t.Fatal("外部节点检测在跑时,服务器检测也要等")
	}
	release()
	if _, err := run.Reach().BeginExt(); err != nil {
		t.Fatalf("结束后应能再测: %v", err)
	}
	if code, _ := extCall(t, s, http.MethodPost, "1/nodes/reach", `{"keys":[]}`); code != http.StatusBadRequest {
		t.Fatalf("没选节点应 400: %d", code)
	}
	for typ, udp := range map[string]bool{"hysteria2": true, "tuic": true, "hysteria": true, "vless": false, "trojan": false} {
		if udpTypes[typ] != udp {
			t.Errorf("%s 是否按 UDP 处理: %v", typ, udpTypes[typ])
		}
	}
}

// 老库升级上来 prefs_rev 是 NULL(读出来是 0):按 0 比,照样能保存(否则 NULL = 0 不成立,永远 409)。
func TestExtPrefsUpgradedRowNullRev(t *testing.T) {
	s := extNodesServer(t)
	s.db.Exec("UPDATE ext_nodes SET prefs_rev = NULL, node_total = NULL, last_try = NULL, hidden_keys = NULL, rename_keys = NULL WHERE id = 1")
	_, out := extCall(t, s, http.MethodGet, "1/nodes", "")
	if out["rev"] != float64(0) {
		t.Fatalf("NULL 应读成 0: %v", out["rev"])
	}
	if code, out := extCall(t, s, http.MethodPut, "1/nodes/prefs", `{"rev":0,"hide":[`+opFor(t, s, "a", "")+`]}`); code != http.StatusOK || out["rev"] != float64(1) {
		t.Fatalf("升级上来的老行应能保存: %d %v", code, out)
	}
}
