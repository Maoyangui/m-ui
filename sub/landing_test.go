package sub

import (
	"encoding/json"
	"net/http/httptest"
	"strings"
	"testing"
	"time"

	"github.com/Maoyangui/m-ui/database/model"
)

// 落地页的状态化:地址能对上面板用户就出正常落地页并标明原因,客户端仍按启停拿 404。

func TestDisabledUserSeesLandingPage(t *testing.T) {
	s, db := shareServer(t)
	db.Create(&model.Setting{Key: "subPageSupport", Value: "TG: @support"})
	db.Create(&model.Setting{Key: "subPageBuyURL", Value: "https://shop.example/buy"})
	db.Model(&model.User{}).Where("name = ?", "alice").Update("enabled", false)

	w := doReqZh(s, "/sub/alice")
	if w.Code != 200 {
		t.Fatalf("停用用户在浏览器里应看到落地页(200),实际 %d", w.Code)
	}
	body := w.Body.String()
	for _, want := range []string{"订阅已停用", "已停用", "https://shop.example/buy", "选购 / 续费订阅", "TG: @support", "订阅地址", "id=\"usage\"", "/sub/alice?format=clash"} {
		if !strings.Contains(body, want) {
			t.Fatalf("落地页缺少 %q", want)
		}
	}
	if strings.Contains(body, "订阅地址无效") {
		t.Fatal("能对上用户就不该是 404 页")
	}
	// 客户端拉取仍是干净的 404
	c := doReq(s, "GET", "/sub/alice", "clash-verge/2.0")
	if c.Code != 404 || strings.Contains(c.Body.String(), "<html") {
		t.Fatalf("客户端应拿到纯 404,实际 %d %q", c.Code, c.Body.String())
	}
	// 停用状态下不能生成共享
	p := doReq(s, "POST", "/sub/alice?share=on", browserUA)
	if p.Code != 404 {
		t.Fatalf("停用用户生成共享应 404,实际 %d", p.Code)
	}
}

func TestExpiredAndExhaustedStates(t *testing.T) {
	s, db := shareServer(t)
	db.Model(&model.User{}).Where("name = ?", "alice").Updates(map[string]interface{}{"expiry": time.Now().Unix() - 3600})
	body := doReqZh(s, "/sub/alice").Body.String()
	if !strings.Contains(body, "订阅已到期") || !strings.Contains(body, "节点已停止服务") {
		t.Fatal("到期用户的落地页应有到期状态卡")
	}
	if strings.Contains(body, `class="buylink"`) {
		t.Fatal("不可用时不该再出现右上角的小选购按钮(状态卡里已是主按钮)")
	}
	// 到期但没被停用:客户端行为和以前一样,面板说了算
	if c := doReq(s, "GET", "/sub/alice?format=clash", "clash-verge/2.0"); c.Code != 200 {
		t.Fatalf("到期未停用的用户,客户端应仍拿到订阅,实际 %d", c.Code)
	}

	db.Model(&model.User{}).Where("name = ?", "alice").Updates(map[string]interface{}{"expiry": 0, "volume": 1000, "up": 600, "down": 500, "auto_reset": true, "next_reset": time.Now().Unix() + 86400})
	body = doReqZh(s, "/sub/alice").Body.String()
	if !strings.Contains(body, "流量已用完") || !strings.Contains(body, "重置") {
		t.Fatal("用尽用户的落地页应有用尽状态卡并提到重置")
	}
}

func TestActivePageShowsSmallBuyLink(t *testing.T) {
	s, db := shareServer(t)
	body := doReqZh(s, "/sub/alice").Body.String()
	if strings.Contains(body, `class="buylink"`) || strings.Contains(body, "class=\"buy\"") {
		t.Fatal("没填选购链接时不该有选购按钮")
	}
	db.Create(&model.Setting{Key: "subPageBuyURL", Value: "https://shop.example/buy"})
	body = doReqZh(s, "/sub/alice").Body.String()
	if !strings.Contains(body, `class="buylink"`) || !strings.Contains(body, "https://shop.example/buy") || strings.Contains(body, "class=\"buy\"") {
		t.Fatal("正常状态应只有右上角的小选购按钮")
	}
}

func TestBuyURLResellerOverrideAndValidation(t *testing.T) {
	s, db := shareServer(t)
	db.Create(&model.Setting{Key: "subPageBuyURL", Value: "https://main.example/buy"})
	db.Create(&model.Reseller{Name: "dl1", Enabled: true, PageEnabled: true, ShareOn: true, PageBuyURL: "https://dl1.example/buy"})
	db.Create(&model.User{Name: "bob", Enabled: true, ResellerId: 1, SubToken: "tok123", Credentials: []byte(`{"hysteria2":{"password":"p"}}`)})
	body := doReqZh(s, "/sub/tok123").Body.String()
	if !strings.Contains(body, "https://dl1.example/buy") || strings.Contains(body, "https://main.example/buy") {
		t.Fatal("代理填了选购链接就该用代理的")
	}
	// 非 http(s) 的地址当作没填
	db.Create(&model.Setting{Key: "subPageBuyURL2", Value: "x"})
	db.Model(&model.Setting{}).Where("key = ?", "subPageBuyURL").Update("value", "javascript:alert(1)")
	db.Model(&model.Reseller{}).Where("id = ?", 1).Update("page_buy_url", "")
	body = doReqZh(s, "/sub/tok123").Body.String()
	if strings.Contains(body, "javascript:") || strings.Contains(body, `class="buylink"`) {
		t.Fatal("非 http(s) 的选购链接不该出现在页面上")
	}
}

func TestUnknownKeyGetsInvalidPageWithBuyButton(t *testing.T) {
	s, db := shareServer(t)
	db.Create(&model.Setting{Key: "subPageBuyURL", Value: "https://shop.example/buy"})
	w := doReqZh(s, "/sub/nobody-here")
	if w.Code != 404 {
		t.Fatalf("未知地址应 404,实际 %d", w.Code)
	}
	body := w.Body.String()
	if !strings.Contains(body, "订阅地址无效") || !strings.Contains(body, "https://shop.example/buy") || !strings.Contains(body, "选购订阅") {
		t.Fatal("404 页应说明地址无效并给选购按钮")
	}
}

func TestUsageStatsEndpoint(t *testing.T) {
	s, db := shareServer(t)
	now := time.Now().Unix()
	db.Create(&model.Stats{Resource: "user", Tag: "alice", Direction: true, Traffic: 1000, DateTime: now - 600})
	db.Create(&model.Stats{Resource: "user", Tag: "alice", Direction: false, Traffic: 5000, DateTime: now - 600})
	db.Create(&model.Stats{Resource: "user", Tag: "someone-else", Direction: false, Traffic: 99999, DateTime: now - 600})

	w := doReq(s, "GET", "/sub/alice?stats=24", browserUA)
	if w.Code != 200 || !strings.HasPrefix(w.Header().Get("Content-Type"), "application/json") {
		t.Fatalf("用量接口应返回 JSON,实际 %d %s", w.Code, w.Header().Get("Content-Type"))
	}
	var res struct {
		Points    []map[string]int64 `json:"points"`
		Span      int64              `json:"span"`
		TotalUp   int64              `json:"totalUp"`
		TotalDown int64              `json:"totalDown"`
	}
	if err := json.Unmarshal(w.Body.Bytes(), &res); err != nil {
		t.Fatal(err)
	}
	if res.TotalUp != 1000 || res.TotalDown != 5000 || res.Span != 3600 || len(res.Points) < 24 {
		t.Fatalf("聚合不对:%+v", res)
	}
	// 客户端 UA 也能拿(页面脚本用的是浏览器 UA,但接口不看 UA)
	if c := doReq(s, "GET", "/sub/alice?stats=168", "curl/8"); c.Code != 200 {
		t.Fatalf("stats 不该看 UA,实际 %d", c.Code)
	}
	// 停用的人也能看自己的用量(落地页照常显示)
	db.Model(&model.User{}).Where("name = ?", "alice").Update("enabled", false)
	if d := doReq(s, "GET", "/sub/alice?stats=24", browserUA); d.Code != 200 {
		t.Fatalf("停用用户看用量应 200,实际 %d", d.Code)
	}
	// 共享地址不给用量
	db.Model(&model.User{}).Where("name = ?", "alice").Updates(map[string]interface{}{"enabled": true, "share_token": "shr-token-xyz", "share_creds": []byte(`{"hysteria2":{"password":"q"}}`)})
	if x := doReq(s, "GET", "/sub/shr-token-xyz?stats=24", browserUA); x.Code != 404 {
		t.Fatalf("共享地址查用量应 404,实际 %d", x.Code)
	}
}

// 共享地址的落地页:精简版 —— 临时共享标识、选购、公告、一键导入、订阅地址(共享令牌)、节点;
// 本人的用量 / 到期 / 用量图 / 共享管理都不出;取消后地址对不上就是 404 页。
func TestSharedLandingPage(t *testing.T) {
	s, db := shareServer(t)
	db.Create(&model.Setting{Key: "subPageNotice", Value: "每月 1 号重置流量"})
	db.Create(&model.Setting{Key: "subPageBuyURL", Value: "https://shop.example/buy"})
	db.Model(&model.User{}).Where("name = ?", "alice").Updates(map[string]interface{}{"volume": 1000, "up": 100, "down": 100, "expiry": time.Now().Unix() + 86400})
	doReq(s, "POST", "/sub/alice?share=on", browserUA)
	tok := token(t, db)

	w := doReqZh(s, "/sub/"+tok)
	if w.Code != 200 {
		t.Fatalf("共享落地页应 200,实际 %d", w.Code)
	}
	body := w.Body.String()
	for _, want := range []string{"临时共享", "选购自己的订阅", "https://shop.example/buy", "每月 1 号重置流量", "一键导入", "订阅地址", "/sub/" + tok + "?format=clash", "/sub/" + tok + "/qr?format=clash"} {
		if !strings.Contains(body, want) {
			t.Fatalf("共享落地页缺少 %q", want)
		}
	}
	for _, unwanted := range []string{"我的订阅", `id="usage"`, `id="share"`, "?share=off", "/sub/alice", "100.0 B", "剩 1 天"} {
		if strings.Contains(body, unwanted) {
			t.Fatalf("共享落地页不该出现 %q(本人用量 / 到期 / 共享管理 / 本人地址)", unwanted)
		}
	}
	// 借用者不能看本人用量、不能改共享
	if x := doReq(s, "GET", "/sub/"+tok+"?stats=24", browserUA); x.Code != 404 {
		t.Fatalf("共享地址查用量应 404,实际 %d", x.Code)
	}
	// 客户端拉共享订阅照常
	if c := doReq(s, "GET", "/sub/"+tok+"?format=clash", "clash-verge/2.0"); c.Code != 200 {
		t.Fatalf("客户端拉共享订阅应 200,实际 %d", c.Code)
	}
	// 本人到期:借用者看到状态卡但没有日期和数字;客户端仍按启停拿订阅
	db.Model(&model.User{}).Where("name = ?", "alice").Update("expiry", time.Now().Unix()-3600)
	body = doReqZh(s, "/sub/"+tok).Body.String()
	if !strings.Contains(body, "分享给你的这条订阅已经到期") || strings.Contains(body, `class="buylink"`) || strings.Count(body, "选购自己的订阅</a>") != 1 { // 状态卡里那一个,共享卡里的不再重复
		t.Fatal("共享落地页到期时应有不带数字的状态卡,且不再出现「选购自己的订阅」按钮")
	}
	// 本人被停用:借用者浏览器看到落地页(已停用),客户端 404
	db.Model(&model.User{}).Where("name = ?", "alice").Updates(map[string]interface{}{"expiry": 0, "enabled": false})
	if w := doReqZh(s, "/sub/"+tok); w.Code != 200 || !strings.Contains(w.Body.String(), "分享给你的这条订阅目前无法使用") {
		t.Fatalf("本人停用时共享落地页应 200 并标明不可用,实际 %d", w.Code)
	}
	if c := doReq(s, "GET", "/sub/"+tok+"?format=clash", "clash-verge/2.0"); c.Code != 404 {
		t.Fatalf("本人停用时客户端拉共享订阅应 404,实际 %d", c.Code)
	}
	// 取消共享后地址对不上任何人:404 页
	db.Model(&model.User{}).Where("name = ?", "alice").Update("enabled", true)
	doReq(s, "POST", "/sub/alice?share=off", browserUA)
	if w := doReqZh(s, "/sub/"+tok); w.Code != 404 || !strings.Contains(w.Body.String(), "订阅地址无效") {
		t.Fatalf("取消后的共享地址应是 404 页,实际 %d", w.Code)
	}
}

func TestSharePostReturnsPageDirectly(t *testing.T) {
	s, _ := shareServer(t)
	r := httptest.NewRequest("POST", "http://hk.example:2056/sub/alice?share=on", nil)
	r.Header.Set("User-Agent", browserUA)
	r.Header.Set("Accept-Language", "zh-CN")
	w := httptest.NewRecorder()
	s.handle()(w, r)
	if w.Code != 200 {
		t.Fatalf("生成共享应直接返回落地页(200),实际 %d", w.Code)
	}
	body := w.Body.String()
	if !strings.Contains(body, `id="share"`) || !strings.Contains(body, "已开启") || !strings.Contains(body, "取消共享") {
		t.Fatal("返回的页面里应是已开启状态的共享卡")
	}
	// 标题、徽章、按钮同一行(cardhead 里),地址在下一行
	i := strings.Index(body, `id="share"`)
	card := body[i:]
	if !strings.Contains(card[:strings.Index(card, "</form>")], "cardhead") {
		t.Fatal("共享卡的按钮应和标题在同一行")
	}
}
