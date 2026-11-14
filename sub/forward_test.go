package sub

import (
	"encoding/json"
	"net/url"
	"strings"
	"testing"

	"github.com/Maoyangui/m-ui/database/model"
)

func fwLine(t *testing.T, fs ...model.LineForward) model.Line {
	t.Helper()
	b, _ := json.Marshal(fs)
	return model.Line{Id: 7, Name: "hy2", Protocol: "hysteria2", Port: 443, Enabled: true, NodeIds: []byte(`[2,3]`),
		Options: []byte(`{"port_hopping":"20000-30000"}`), Forwards: b}
}

var fwEntries = []Entry{
	{Name: "广州中转", Host: "gz.example.com", SNI: "gz.example.com", NodeId: 1, Suffix: "-广州中转"},
	{Name: "东京", Host: "jp.example.com", SNI: "jp.example.com", NodeId: 2, Suffix: "-东京"},
	{Name: "大阪", Host: "203.0.113.3", SNI: "osaka.example.com", NodeId: 3, Suffix: "-大阪 x2", Ratio: 2},
}

// 中转入口进订阅:集群中转用那台的连接地址、外部中转用填的地址,端口是中转入口的;SNI 跟出口那台;
// 节点名 = 线路名 + 出口后缀 + "-" + 中转名(倍率放最后);不带端口跳跃;暂停的、中转 / 出口不在的、出口不在部署范围的不给;
// 用户被收窄到别的服务器时,转给那台之外的中转也不给。
func TestForwardsInLinks(t *testing.T) {
	user := model.User{Name: "u", Credentials: []byte(`{"hysteria2":{"password":"p"}}`)}
	line := fwLine(t,
		model.LineForward{Via: 1, Port: 30001, To: 2},
		model.LineForward{Host: "hk.relay.example", Port: 31000, To: 3, Name: "IPLC"},
		model.LineForward{Via: 1, Port: 30002, To: 2, Off: true}, // 暂停
		model.LineForward{Via: 9, Port: 30003, To: 2},            // 中转服务器不在(停用 / 删了)
		model.LineForward{Via: 1, Port: 30004, To: 4},            // 出口不在部署范围
		model.LineForward{Via: 2, Port: 30005, To: 2},            // 中转和出口同一台
	)
	links := GenerateLinksFor(user, []model.Line{line}, fwEntries, nil)
	if len(links) != 4 {
		t.Fatalf("应是 2 台直连 + 2 个中转:%v", links)
	}
	type got struct{ host, sni, name, mport string }
	parse := func(l string) got {
		u, err := url.Parse(l)
		if err != nil {
			t.Fatal(err)
		}
		return got{u.Host, u.Query().Get("sni"), u.Fragment, u.Query().Get("mport")}
	}
	want := []got{
		{"jp.example.com:443", "jp.example.com", "hy2-东京", "20000-30000"},
		{"203.0.113.3:443", "osaka.example.com", "hy2-大阪 x2", "20000-30000"},
		{"gz.example.com:30001", "jp.example.com", "hy2-东京-广州中转", ""},
		{"hk.relay.example:31000", "osaka.example.com", "hy2-大阪-IPLC x2", ""},
	}
	for i, w := range want {
		if g := parse(links[i]); g != w {
			t.Fatalf("第 %d 条:\n got %+v\nwant %+v", i+1, g, w)
		}
	}

	// 收窄到东京:转给大阪的外部中转不给
	links = GenerateLinksFor(user, []model.Line{line}, fwEntries, map[uint]map[uint]bool{7: {2: true}})
	if len(links) != 2 || parse(links[1]).host != "gz.example.com:30001" {
		t.Fatalf("收窄到东京只剩东京直连和转给东京的中转:%v", links)
	}
	// 「线路作上游」只要出口本身:resolveAddrs 不含中转
	if a := resolveAddrs(line, fwEntries, nil); len(a) != 2 {
		t.Fatalf("resolveAddrs 不该带中转:%v", a)
	}
}

// Clash 和 sing-box 订阅同样带上中转节点,线路测速组把它们算进去;中转节点不带端口跳跃。
func TestForwardsInClashAndSingbox(t *testing.T) {
	user := model.User{Name: "u", Credentials: []byte(`{"hysteria2":{"password":"p"}}`)}
	line := fwLine(t, model.LineForward{Via: 1, Port: 30001, To: 2})
	out, err := BuildClash(user, []model.Line{line}, fwEntries, "", "")
	if err != nil {
		t.Fatal(err)
	}
	if !strings.Contains(out, "name: hy2-东京-广州中转") || !strings.Contains(out, "server: gz.example.com") || !strings.Contains(out, "port: 30001") {
		t.Fatalf("clash 缺中转节点:\n%s", out)
	}
	if strings.Count(out, "ports: 20000-30000") != 2 {
		t.Fatalf("clash 里只有两台直连带端口跳跃:\n%s", out)
	}
	res, err := BuildSingBoxSub(user, []model.Line{line}, Options{Entries: fwEntries})
	if err != nil {
		t.Fatal(err)
	}
	var cfg struct {
		Outbounds []map[string]interface{} `json:"outbounds"`
	}
	if err := json.Unmarshal([]byte(res.Body), &cfg); err != nil {
		t.Fatal(err)
	}
	var relay map[string]interface{}
	var group []interface{}
	for _, o := range cfg.Outbounds {
		if o["tag"] == "hy2-东京-广州中转" {
			relay = o
		}
		if o["tag"] == "hy2" && o["type"] == "urltest" {
			group, _ = o["outbounds"].([]interface{})
		}
	}
	if relay == nil || relay["server"] != "gz.example.com" || relay["server_port"] != float64(30001) || relay["server_ports"] != nil {
		t.Fatalf("sing-box 中转节点不对: %v", relay)
	}
	if tls, _ := relay["tls"].(map[string]interface{}); tls == nil || tls["server_name"] != "jp.example.com" {
		t.Fatalf("中转节点的 SNI 应是出口的: %v", relay["tls"])
	}
	if len(group) != 3 {
		t.Fatalf("线路测速组应含 2 台直连 + 1 个中转: %v", group)
	}
}
