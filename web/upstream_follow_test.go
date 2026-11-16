package web

import (
	"bytes"
	"encoding/json"
	"errors"
	"net/http/httptest"
	"path/filepath"
	"strconv"
	"testing"
	"time"

	"github.com/Maoyangui/m-ui/database"
	"github.com/Maoyangui/m-ui/database/model"
	"github.com/Maoyangui/m-ui/ext"
	"github.com/Maoyangui/m-ui/runner"
)

func TestAllFailing(t *testing.T) {
	sv := func(state string, fails int) upServer { return upServer{State: state, Fails: fails} }
	cases := []struct {
		name string
		row  upRow
		want bool
	}{
		{"没人用", upRow{Unused: true}, false},
		{"没有结果", upRow{}, false},
		{"全坏且连着几轮", upRow{Servers: []upServer{sv("fail", 3), sv("fail", 2)}}, true},
		{"有一台通着", upRow{Servers: []upServer{sv("fail", 5), sv("ok", 0)}}, false},
		{"有一台还没测到", upRow{Servers: []upServer{sv("fail", 5), sv("pending", 0)}}, false},
		{"有一台失联(不知道通不通)", upRow{Servers: []upServer{sv("fail", 5), sv("stale", 0)}}, false},
		{"刚坏一轮", upRow{Servers: []upServer{sv("fail", 1), sv("fail", 4)}}, false},
		{"只有一台,连着坏", upRow{Servers: []upServer{sv("fail", 2)}}, true},
	}
	for _, c := range cases {
		if got := allFailing(c.row, 2); got != c.want {
			t.Errorf("%s: 得到 %v,应为 %v", c.name, got, c.want)
		}
	}
	// 阈值设成 1 也至少要连着两轮;设得更高就按更高的
	if allFailing(upRow{Servers: []upServer{sv("fail", 1)}}, 1) {
		t.Error("阈值 1 时刚坏一轮不该算")
	}
	if allFailing(upRow{Servers: []upServer{sv("fail", 3)}}, 4) {
		t.Error("阈值 4 时连坏 3 轮不该算")
	}
}

func TestPickSuccessor(t *testing.T) {
	src := model.UpstreamSource{Key: "k-old", Name: "香港 01 | 剩余 80GB"}
	c := func(key, name, typ, server string, port int) followCand {
		return followCand{Key: key, Name: name, Type: typ, Server: server, Port: port}
	}
	cases := []struct {
		name    string
		cands   []followCand
		want    int
		verdict string
	}{
		{"指纹还在:订阅里没变", []followCand{c("k-old", "香港 01 | 剩余 80GB", "trojan", "hk1.example.net", 443)}, -1, followUnchanged},
		{"同名换了端口", []followCand{c("k1", "日本 01", "trojan", "jp1.example.net", 443), c("k2", "香港 01 | 剩余 80GB", "trojan", "hk1.example.net", 8443)}, 1, ""},
		{"名字里的剩余流量变了", []followCand{c("k2", "香港 01 | 剩余 12GB", "trojan", "hk9.example.net", 443), c("k3", "香港 02 | 剩余 12GB", "trojan", "hk2.example.net", 443)}, -1, followAmbiguous},
		{"名字里只有会变的数字不同,且唯一", []followCand{c("k2", "香港 01 | 剩余 12GB", "trojan", "hk9.example.net", 443), c("k3", "日本 01 | 剩余 12GB", "trojan", "jp1.example.net", 443)}, 0, ""},
		{"改了名但地址没变", []followCand{c("k2", "HK-Premium", "trojan", "HK1.example.net", 2053), c("k3", "JP", "trojan", "jp1.example.net", 443)}, 0, ""},
		{"同名但协议不同:不认", []followCand{c("k2", "香港 01 | 剩余 80GB", "vless", "hk1.example.net", 443)}, -1, followGone},
		{"同地址的有两个:不猜", []followCand{c("k2", "A", "trojan", "hk1.example.net", 443), c("k3", "B", "trojan", "hk1.example.net", 8443)}, -1, followAmbiguous},
		{"什么都对不上", []followCand{c("k2", "美国 01", "trojan", "us1.example.net", 443)}, -1, followGone},
		{"订阅空了", nil, -1, followGone},
	}
	for _, x := range cases {
		got, verdict := pickSuccessor(src, "trojan", "hk1.example.net", x.cands)
		if got != x.want || verdict != x.verdict {
			t.Errorf("%s: 得到 (%d, %q),应为 (%d, %q)", x.name, got, verdict, x.want, x.verdict)
		}
	}
}

func TestSameOutbound(t *testing.T) {
	a := json.RawMessage(`{"server":"a.example.net","server_port":443,"password":"x"}`)
	b := json.RawMessage(`{"password":"x","server_port":443,"server":"a.example.net"}`)
	if !sameOutbound("trojan", a, "trojan", b) {
		t.Error("键的顺序不同不该算不同")
	}
	if sameOutbound("trojan", a, "vless", b) || sameOutbound("trojan", a, "trojan", json.RawMessage(`{"server":"a.example.net","server_port":8443,"password":"x"}`)) {
		t.Error("类型或参数不同应算不同")
	}
}

// 整条链路:从外部节点加的上游全部测不通 → 订阅里那个节点换了端口 → 上游跟着改;通着的时候一动不动;手动改过参数就不再跟。
func TestUpstreamFollow(t *testing.T) {
	run, err := runner.New(filepath.Join(t.TempDir(), "m.db"))
	if err != nil {
		t.Fatal(err)
	}
	defer database.Close(run.DB())
	s := NewServer(run)
	db := run.DB()
	var local model.Node
	if db.Where("is_local = ?", true).First(&local).Error != nil {
		local = model.Node{Name: "主机", IsLocal: true, Enabled: true, Sort: 1}
		db.Create(&local)
	}

	link := func(port int) string {
		return "trojan://pw@hk1.example.net:" + strconv.Itoa(port) + "?security=tls&sni=hk1.example.net#HK-01"
	}
	e := model.ExtNode{Name: "机场", Type: "link", Value: link(443), Enabled: true}
	if err := db.Create(&e).Error; err != nil {
		t.Fatal(err)
	}
	// 走真的「添加为上游」:来源随上游一起记下
	w := httptest.NewRecorder()
	it, cands, _ := candsOf(e)
	if len(cands) != 1 {
		t.Fatalf("测试用的链接没解析成一个节点: %+v", cands)
	}
	res := s.addUpstreamsFromExt(httptest.NewRequest("POST", "http://x/app/api/exts/1/nodes/upstream", nil), e, it, []extAddItem{{Key: cands[0].Key, Name: "hk-exit"}})
	_ = w
	if len(res) != 1 || !res[0].OK {
		t.Fatalf("添加为上游失败: %+v", res)
	}
	upID := res[0].Id
	var src model.UpstreamSource
	if db.First(&src, "upstream_id = ?", upID).Error != nil || src.ExtId != e.Id || src.Key != cands[0].Key || src.Name != "HK-01" {
		t.Fatalf("来源没记下来: %+v", src)
	}
	// 一条用它的线路,部署在本机
	if err := db.Create(&model.Line{Name: "L1", Protocol: "shadowsocks", Port: 28001, UpstreamId: upID, Enabled: true,
		Options: json.RawMessage(`{"method":"aes-256-gcm","password":"p"}`)}).Error; err != nil {
		t.Fatal(err)
	}
	endpoint := func() (string, int) {
		var up model.Upstream
		db.First(&up, upID)
		return endpointOf(up.Options)
	}
	fetched := 0
	s.extRefreshFn = func(uint) error { fetched++; return nil }
	now := time.Now()

	// 1. 机场把端口改了,但上游在本机上还通着(或者还没有巡检结果):不动
	db.Model(&model.ExtNode{}).Where("id = ?", e.Id).Update("value", link(8443))
	s.followTick(now)
	if _, port := endpoint(); port != 443 {
		t.Fatalf("没有全部测不通时不该跟随: 端口变成了 %d", port)
	}
	// 2. 直接给一次跟随(相当于全部测不通的那一刻):端口跟到 8443,来源指纹换成新的
	s.followOne(src, now.Unix())
	if _, port := endpoint(); port != 8443 {
		t.Fatalf("应该跟到新端口: %d", port)
	}
	var after model.UpstreamSource
	db.First(&after, "upstream_id = ?", upID)
	if after.Note != followOK || after.FollowedAt == 0 || after.Key == src.Key || after.Detail != "hk1.example.net:443 → hk1.example.net:8443" {
		t.Fatalf("跟随后的记录不对: %+v", after)
	}
	var audits int64
	db.Model(&model.Change{}).Where("actor = ? AND key = ? AND action = ?", "FollowJob", "upstream", "follow").Count(&audits)
	if audits != 1 {
		t.Fatalf("跟随应该记一条审计: %d", audits)
	}
	// 3. 再来一次:订阅里现在这个节点就是上游的样子 → 「订阅里没变」,不改
	s.followOne(after, now.Unix()+3600)
	var again model.UpstreamSource
	db.First(&again, "upstream_id = ?", upID)
	if _, port := endpoint(); port != 8443 || again.Note != followUnchanged {
		t.Fatalf("订阅没变时不该再改: 端口 %d,结论 %q", port, again.Note)
	}
	// 4. 订阅里这个节点整个没了:不猜,记下找不到
	db.Model(&model.ExtNode{}).Where("id = ?", e.Id).Update("value", "trojan://pw@us9.example.net:443?security=tls#US-09")
	s.followOne(again, now.Unix()+7200)
	db.First(&again, "upstream_id = ?", upID)
	if _, port := endpoint(); port != 8443 || again.Note != followGone {
		t.Fatalf("找不到对应节点时不该改: 端口 %d,结论 %q", port, again.Note)
	}
	// 5. 订阅抓不下来(订阅类型才去抓):记下,不改
	db.Model(&model.ExtNode{}).Where("id = ?", e.Id).Updates(map[string]interface{}{"type": "sub", "value": "https://sub.example.net/x"})
	s.extRefreshFn = func(uint) error { fetched++; return errors.New("抓取超时") }
	s.followOne(again, now.Unix()+10800)
	db.First(&again, "upstream_id = ?", upID)
	if again.Note != followFetch || fetched != 1 {
		t.Fatalf("抓不下来应记 fetch: %q(抓了 %d 次)", again.Note, fetched)
	}

	// 6. 上游列表带着来源;手动改了参数就不再跟随,只改名不算
	rec := httptest.NewRecorder()
	s.handleUpstreams(rec, httptest.NewRequest("GET", "http://x/app/api/upstreams", nil))
	var list []struct {
		Id     uint          `json:"id"`
		Source *upSourceView `json:"source"`
	}
	if err := json.Unmarshal(rec.Body.Bytes(), &list); err != nil || len(list) != 1 || list[0].Source == nil || list[0].Source.ExtName != "机场" {
		t.Fatalf("上游列表里应带来源: %s", rec.Body.String())
	}
	var up model.Upstream
	db.First(&up, upID)
	put := func(name string, opts json.RawMessage) int {
		body, _ := json.Marshal(map[string]interface{}{"name": name, "type": up.Type, "options": opts})
		rec := httptest.NewRecorder()
		s.handleUpstreamItem(rec, httptest.NewRequest("PUT", "http://x/app/api/upstreams/"+strconv.Itoa(int(upID)), bytes.NewReader(body)))
		return rec.Code
	}
	if code := put("hk-exit-2", up.Options); code != 200 {
		t.Fatalf("改名失败: %d", code)
	}
	var n int64
	db.Model(&model.UpstreamSource{}).Where("upstream_id = ?", upID).Count(&n)
	if n != 1 {
		t.Fatal("只改名不该解除跟随")
	}
	var o map[string]interface{}
	_ = json.Unmarshal(up.Options, &o)
	o["server_port"] = 9443
	changed, _ := json.Marshal(o)
	if code := put("hk-exit-2", changed); code != 200 {
		t.Fatalf("改参数失败: %d", code)
	}
	db.Model(&model.UpstreamSource{}).Where("upstream_id = ?", upID).Count(&n)
	if n != 0 {
		t.Fatal("手动改过参数应该不再跟随")
	}
}

// 升级前从外部节点加的上游没有来源记录:参数和订阅里某个节点一模一样的,补记上
func TestLinkUpstreamSources(t *testing.T) {
	run, err := runner.New(filepath.Join(t.TempDir(), "m.db"))
	if err != nil {
		t.Fatal(err)
	}
	defer database.Close(run.DB())
	s := NewServer(run)
	db := run.DB()
	e := model.ExtNode{Name: "机场", Type: "link", Value: "trojan://pw@hk1.example.net:443?security=tls&sni=hk1.example.net#HK-01", Enabled: true}
	db.Create(&e)
	it, cands, idx := candsOf(e)
	if len(cands) != 1 {
		t.Fatalf("节点没解析出来: %+v", cands)
	}
	typ, opts, _ := ext.Outbound(it.Nodes[idx[0]])
	b, _ := json.Marshal(opts)
	same := model.Upstream{Name: "old-hk", Type: typ, Options: b}
	db.Create(&same)
	other := model.Upstream{Name: "manual", Type: "socks", Options: json.RawMessage(`{"server":"198.51.100.9","server_port":1080}`)}
	db.Create(&other)
	s.linkUpstreamSources()
	var srcs []model.UpstreamSource
	db.Find(&srcs)
	if len(srcs) != 1 || srcs[0].UpstreamId != same.Id || srcs[0].ExtId != e.Id || srcs[0].Key != cands[0].Key {
		t.Fatalf("应该只给参数一样的那条补记来源: %+v", srcs)
	}
	s.linkUpstreamSources() // 再跑一遍不重复记
	var n int64
	db.Model(&model.UpstreamSource{}).Count(&n)
	if n != 1 {
		t.Fatalf("重复记了: %d", n)
	}
}
