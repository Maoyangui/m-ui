package sub

import (
	"encoding/json"
	"net/http"
	"regexp"
	"strings"
	"testing"
	"time"

	"github.com/Maoyangui/m-ui/database/model"

	"gorm.io/gorm"
)

// 落地页「节点」一栏:一条线路点开,就地列出它在各台服务器上的节点;每个节点可以复制分享链接、看二维码。
// 页面本身不带链接(展开时才取:?nodes=线路 id),二维码按链接的摘要取(/qr?node=键),链接不进地址栏。

// nodesServer 三台服务器(HK 本机、TW、JP 被额度停了服务)、一条部署在全部服务器上的 hysteria2 线路、
// 一条 mixed 线路(一个地址出 socks + http 两条链接)、一条 alice 没有的线路。
func nodesServer(t *testing.T) (*Server, *gorm.DB) {
	t.Helper()
	s, db := shareServer(t) // HK(本机)+ 线路「香港1」(id 1)+ alice(id 1,挂着线路 1)
	db.Create(&model.Node{Name: "TW", Domain: "tw.example", Enabled: true})
	jp := model.Node{Name: "JP", Domain: "jp.example", Enabled: true}
	db.Create(&jp)
	db.Model(&model.Node{}).Where("id = ?", jp.Id).Update("suspended", true)
	db.Create(&model.Line{Name: "本地代理", Protocol: "mixed", Port: 1080, Enabled: true, Sort: 2})
	db.Create(&model.Line{Name: "别人的线路", Protocol: "hysteria2", Port: 8443, Enabled: true, Sort: 3})
	db.Exec("INSERT INTO user_lines (user_id, line_id) VALUES (1, 2)")
	db.Model(&model.User{}).Where("id = ?", 1).Update("credentials",
		[]byte(`{"hysteria2":{"password":"p"},"socks":{"password":"sp"},"http":{"password":"hp"}}`))
	return s, db
}

func getNodes(t *testing.T, s *Server, target string) (int, []lineNode) {
	t.Helper()
	w := doReq(s, "GET", target, browserUA)
	var list []lineNode
	if w.Code == 200 {
		if ct := w.Header().Get("Content-Type"); !strings.HasPrefix(ct, "application/json") {
			t.Fatalf("%s 的 Content-Type 应是 JSON,实际 %q", target, ct)
		}
		if cc := w.Header().Get("Cache-Control"); cc != "no-store" {
			t.Fatalf("%s 带着凭据,必须 no-store,实际 %q", target, cc)
		}
		if err := json.Unmarshal(w.Body.Bytes(), &list); err != nil {
			t.Fatalf("%s 不是 JSON: %v %s", target, err, w.Body.String())
		}
	}
	return w.Code, list
}

func TestLandingNodesExpand(t *testing.T) {
	s, _ := nodesServer(t)
	body := doReqZh(s, "/sub/alice").Body.String()
	// 页面:两条线路都能点开,带着节点数;页面里没有任何分享链接(展开时才取)
	for _, want := range []string{`id="nodes"`, `<div class="ln" data-id="1">`, `<div class="ln" data-id="2">`, `<span class="cnt">2</span>`, `<span class="cnt">4</span>`, `id="qrp"`, "点一条线路"} {
		if !strings.Contains(body, want) {
			t.Fatalf("落地页缺少 %q", want)
		}
	}
	for _, bad := range []string{"hysteria2://", "socks5://", "别人的线路", `data-id="3"`} {
		if strings.Contains(body, bad) {
			t.Fatalf("落地页里不该有 %q", bad)
		}
	}
	// 展开线路 1:本机和 TW 各一个节点;被额度停了服务的 JP 没有(订阅里也没有它)
	code, list := getNodes(t, s, "/sub/alice?nodes=1")
	if code != 200 || len(list) != 2 {
		t.Fatalf("线路 1 应有 2 个节点,实际 %d %+v", code, list)
	}
	keyRe := regexp.MustCompile(`^[0-9a-f]{16}$`)
	for _, n := range list {
		if !strings.HasPrefix(n.Link, "hysteria2://p@") || !strings.HasPrefix(n.Name, "香港1") || !keyRe.MatchString(n.Key) || n.Key != nodeKey(n.Link) || n.Kind != "" {
			t.Fatalf("节点不对: %+v", n)
		}
	}
	if !strings.Contains(list[0].Link, "hk.example") || !strings.Contains(list[1].Link, "tw.example") || list[0].Name == list[1].Name {
		t.Fatalf("两个节点应分别是本机和 TW 的: %+v", list)
	}
	// 和「通用」订阅里给的一模一样(同一批链接、同一个顺序)
	sub := doReq(s, "GET", "/sub/alice?format=link", "v2rayN/7.0").Body.String()
	var fromSub []string
	for _, l := range strings.Split(strings.TrimSpace(sub), "\n") {
		if strings.HasPrefix(l, "hysteria2://") {
			fromSub = append(fromSub, l)
		}
	}
	if len(fromSub) != 2 || fromSub[0] != list[0].Link || fromSub[1] != list[1].Link {
		t.Fatalf("展开的节点应和订阅里的一样:\n订阅 %v\n展开 %+v", fromSub, list)
	}
	// mixed 线路:一个地址出 socks、http 两条,各自标出是哪种
	code, mixed := getNodes(t, s, "/sub/alice?nodes=2")
	if code != 200 || len(mixed) != 4 {
		t.Fatalf("mixed 线路应有 4 条(两台 × socks / http),实际 %d %+v", code, mixed)
	}
	if mixed[0].Kind != "socks5" || mixed[1].Kind != "http" || mixed[0].Name != mixed[1].Name || mixed[0].Key == mixed[1].Key {
		t.Fatalf("同一个地址的两条链接应标出种类、键不同: %+v %+v", mixed[0], mixed[1])
	}
	// 不是这个人的线路、不存在的线路、乱填:404
	for _, q := range []string{"3", "999", "abc", "", "-1", "1.5", "0"} {
		if code, _ := getNodes(t, s, "/sub/alice?nodes="+q); code != 404 {
			t.Fatalf("?nodes=%q 应当 404,实际 %d", q, code)
		}
	}
}

func TestLandingNodeQR(t *testing.T) {
	s, _ := nodesServer(t)
	_, list := getNodes(t, s, "/sub/alice?nodes=1")
	if len(list) != 2 {
		t.Fatalf("取不到节点: %+v", list)
	}
	var imgs [][]byte
	for _, n := range list {
		w := doReq(s, "GET", "/sub/alice/qr?node="+n.Key, browserUA)
		if w.Code != 200 || w.Header().Get("Content-Type") != "image/png" || w.Header().Get("Cache-Control") != "no-store" || !strings.HasPrefix(w.Body.String(), "\x89PNG\r\n") {
			t.Fatalf("节点二维码应是不缓存的 PNG:%d %q", w.Code, w.Header().Get("Content-Type"))
		}
		imgs = append(imgs, w.Body.Bytes())
	}
	if string(imgs[0]) == string(imgs[1]) {
		t.Fatal("两个节点的二维码不该一样")
	}
	for _, bad := range []string{"0000000000000000", "abc", "", strings.Repeat("f", 64), list[0].Key + "0"} {
		if w := doReq(s, "GET", "/sub/alice/qr?node="+bad, browserUA); w.Code != 404 {
			t.Fatalf("对不上任何节点的键 %q 应当 404,实际 %d", bad, w.Code)
		}
	}
	// 订阅地址自己的二维码照旧
	if w := doReq(s, "GET", "/sub/alice/qr?format=clash", browserUA); w.Code != 200 || w.Header().Get("Content-Type") != "image/png" {
		t.Fatalf("订阅地址的二维码应照旧,实际 %d", w.Code)
	}
	// 很长的链接(中等纠错放不下)也出得来:降一档
	long := lineNode{Link: "vless://" + strings.Repeat("a", 2400) + "@example.com:443?security=reality#x"}
	if _, err := encodeNodeQR(long.Link); err != nil {
		t.Fatalf("长链接的二维码应当降档后出得来: %v", err)
	}
}

// 客户端拉不到订阅的人(停用 / 到期 / 用尽),节点同样点不开、取不到链接和二维码;落地页照常,只是那一栏是普通列表。
func TestLandingNodesBlockedUser(t *testing.T) {
	s, db := nodesServer(t)
	_, list := getNodes(t, s, "/sub/alice?nodes=1")
	if len(list) == 0 {
		t.Fatal("先得取得到")
	}
	for name, upd := range map[string]map[string]interface{}{
		"停用": {"enabled": false},
		"到期": {"enabled": true, "expiry": time.Now().Unix() - 60},
		"用尽": {"enabled": true, "expiry": 0, "volume": 100, "up": 60, "down": 60},
	} {
		db.Model(&model.User{}).Where("id = ?", 1).Updates(upd)
		if code, _ := getNodes(t, s, "/sub/alice?nodes=1"); code != 404 {
			t.Fatalf("%s的用户取节点应当 404,实际 %d", name, code)
		}
		if w := doReq(s, "GET", "/sub/alice/qr?node="+list[0].Key, browserUA); w.Code != 404 {
			t.Fatalf("%s的用户取节点二维码应当 404,实际 %d", name, w.Code)
		}
		w := doReqZh(s, "/sub/alice")
		body := w.Body.String()
		if w.Code != 200 || strings.Contains(body, `id="nodes"`) || strings.Contains(body, `class="ln"`) || !strings.Contains(body, "香港1") {
			t.Fatalf("%s的用户:落地页照常出,节点一栏是普通列表(不能点开)", name)
		}
		db.Model(&model.User{}).Where("id = ?", 1).Updates(map[string]interface{}{"enabled": true, "expiry": 0, "volume": 0, "up": 0, "down": 0})
	}
	// 落地页关着:节点也不给
	db.Create(&model.Setting{Key: "subPageEnabled", Value: "false"})
	if code, _ := getNodes(t, s, "/sub/alice?nodes=1"); code != 404 {
		t.Fatalf("落地页关着时取节点应当 404,实际 %d", code)
	}
}

// 共享地址的落地页:借用者拿到的是共享凭据的链接;写着用户名的线路(socks / http / mixed)不给。
func TestLandingNodesSharedLink(t *testing.T) {
	s, db := nodesServer(t)
	if w := doReq(s, "POST", "/sub/alice?share=on", browserUA); w.Code != http.StatusOK {
		t.Fatalf("生成共享地址应当 200,实际 %d", w.Code)
	}
	tok := token(t, db)
	if tok == "" {
		t.Fatal("没有生成共享令牌")
	}
	code, list := getNodes(t, s, "/sub/"+tok+"?nodes=1")
	if code != 200 || len(list) != 2 {
		t.Fatalf("共享地址也能点开线路 1:%d %+v", code, list)
	}
	for _, n := range list {
		if strings.HasPrefix(n.Link, "hysteria2://p@") {
			t.Fatalf("共享地址给的应是共享凭据,不是本人的: %s", n.Link)
		}
	}
	if code, _ := getNodes(t, s, "/sub/"+tok+"?nodes=2"); code != 404 {
		t.Fatalf("写着用户名的线路(mixed)共享地址不给,应当 404,实际 %d", code)
	}
	body := doReqZh(s, "/sub/"+tok).Body.String()
	if !strings.Contains(body, `data-id="1"`) || strings.Contains(body, `data-id="2"`) || strings.Contains(body, "本地代理") {
		t.Fatal("共享地址的落地页:线路 1 能点开,mixed 线路不出现")
	}
	// 本人的键在共享地址上对不上(链接不同)
	_, own := getNodes(t, s, "/sub/alice?nodes=1")
	if w := doReq(s, "GET", "/sub/"+tok+"/qr?node="+own[0].Key, browserUA); w.Code != 404 {
		t.Fatalf("本人链接的键在共享地址上应当 404,实际 %d", w.Code)
	}
	if w := doReq(s, "GET", "/sub/"+tok+"/qr?node="+list[0].Key, browserUA); w.Code != 200 {
		t.Fatalf("共享地址自己的节点二维码应当出得来,实际 %d", w.Code)
	}
}

// 线路上关了输出的节点、这个人被收窄到的服务器:展开后和订阅一样不出现,行上的节点数也跟着变。
func TestLandingNodesFollowFilters(t *testing.T) {
	s, db := nodesServer(t)
	var tw model.Node
	db.Where("name = ?", "TW").First(&tw)
	// 线路 1 上把 TW 这个节点关掉输出
	opts, _ := json.Marshal(map[string]model.NodeOpt{model.DirectKey(tw.Id): {Hidden: true}})
	db.Model(&model.Line{}).Where("id = ?", 1).Update("node_opts", json.RawMessage(opts))
	code, list := getNodes(t, s, "/sub/alice?nodes=1")
	if code != 200 || len(list) != 1 || !strings.Contains(list[0].Link, "hk.example") {
		t.Fatalf("关了输出的节点不该出现:%d %+v", code, list)
	}
	if body := doReqZh(s, "/sub/alice").Body.String(); !strings.Contains(body, `<div class="ln" data-id="1"><button type="button" class="line" aria-expanded="false"><span class="nm">香港1</span>`) || !regexp.MustCompile(`data-id="1">.*?<span class="cnt">1</span>`).MatchString(body) {
		t.Fatal("行上的节点数应当变成 1")
	}
	// 一个节点都不剩的线路:不能点开(普通一行)
	db.Model(&model.Node{}).Where("is_local = ?", true).Update("suspended", true)
	body := doReqZh(s, "/sub/alice").Body.String()
	if strings.Contains(body, `data-id="1"`) || !strings.Contains(body, "香港1") {
		t.Fatal("没有节点的线路应当是普通一行,不能点开")
	}
	if code, list := getNodes(t, s, "/sub/alice?nodes=1"); code != 200 || len(list) != 0 {
		t.Fatalf("没有节点的线路取出来是空列表(不是 null):%d %+v", code, list)
	}
	if raw := doReq(s, "GET", "/sub/alice?nodes=1", browserUA).Body.String(); strings.TrimSpace(raw) != "[]" {
		t.Fatalf("空列表应当是 [],实际 %q", raw)
	}
}
