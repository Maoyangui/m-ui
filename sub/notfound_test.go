package sub

import (
	"net/http/httptest"
	"strings"
	"testing"

	"github.com/Maoyangui/m-ui/database/model"
)

// 中文页面:落地页按 Accept-Language 选语言,测试里显式带上
func doReqZh(s *Server, target string) *httptest.ResponseRecorder {
	r := httptest.NewRequest("GET", "http://hk.example:2056"+target, nil)
	r.Header.Set("User-Agent", browserUA)
	r.Header.Set("Accept-Language", "zh-CN,zh;q=0.9")
	w := httptest.NewRecorder()
	s.handle()(w, r)
	return w
}

// 停用的用户在浏览器里看到的是正常落地页(200,顶部标明"已停用",带主面板的公告与联系方式),
// 客户端拿到的仍然是干净的 404;地址对不上任何人才是"订阅地址无效"的 404 页。
func TestNotFoundPageForBrowser(t *testing.T) {
	s, db := shareServer(t)
	db.Create(&model.Setting{Key: "subPageNotice", Value: "每月 1 号重置流量"})
	db.Create(&model.Setting{Key: "subPageSupport", Value: "TG: @support"})
	db.Model(&model.User{}).Where("name = ?", "alice").Update("enabled", false)

	w := doReqZh(s, "/sub/alice")
	if w.Code != 200 {
		t.Fatalf("状态码应为 200(落地页),实际 %d", w.Code)
	}
	body := w.Body.String()
	for _, want := range []string{"订阅已停用", "每月 1 号重置流量", "TG: @support"} {
		if !strings.Contains(body, want) {
			t.Fatalf("页面缺少 %q", want)
		}
	}

	c := doReq(s, "GET", "/sub/alice", "clash-verge/2.0")
	if c.Code != 404 || strings.Contains(c.Body.String(), "<html") {
		t.Fatalf("客户端应拿到纯 404,实际 %d %q", c.Code, c.Body.String())
	}

	// 地址根本不存在:404 页,同样带公告与联系方式
	n := doReqZh(s, "/sub/nobody-here")
	if n.Code != 404 || !strings.Contains(n.Body.String(), "订阅地址无效") || !strings.Contains(n.Body.String(), "TG: @support") {
		t.Fatalf("未知地址应给 404 页,实际 %d", n.Code)
	}
	if nc := doReq(s, "GET", "/sub/nobody-here", "clash-verge/2.0"); nc.Code != 404 || strings.Contains(nc.Body.String(), "<html") {
		t.Fatalf("未知地址客户端应拿到纯 404,实际 %d", nc.Code)
	}
}

// 代理的用户看到的是代理自己配的公告与联系方式,不是主面板的。
func TestNotFoundPageUsesResellerCopy(t *testing.T) {
	s, db := shareServer(t)
	db.Create(&model.Setting{Key: "subPageNotice", Value: "主面板公告"})
	db.Create(&model.Setting{Key: "subPageSupport", Value: "TG: @master"})
	db.Create(&model.Reseller{Name: "dl1", Enabled: false, PageEnabled: true,
		PageTitle: "dl1 机场", PageNotice: "本店公告", PageSupport: "TG: @dl1"})
	db.Model(&model.Reseller{}).Where("id = ?", 1).Update("enabled", false) // Create 会把零值 false 写成列默认值 true,要显式停用
	db.Create(&model.User{Name: "bob", Enabled: true, ResellerId: 1, SubToken: "tok123",
		Credentials: []byte(`{"hysteria2":{"password":"p"}}`)})

	// 代理被停用:名下用户在浏览器里看到的是落地页(已停用),文案是代理自己的;客户端 404
	w := doReqZh(s, "/sub/tok123")
	if w.Code != 200 {
		t.Fatalf("代理停用时用户应看到落地页(200),实际 %d", w.Code)
	}
	body := w.Body.String()
	for _, want := range []string{"dl1 机场", "本店公告", "TG: @dl1", "订阅已停用"} {
		if !strings.Contains(body, want) {
			t.Fatalf("页面缺少代理自己的文案 %q", want)
		}
	}
	if c := doReq(s, "GET", "/sub/tok123?format=clash", "clash-verge/2.0"); c.Code != 404 {
		t.Fatalf("代理停用时客户端应 404,实际 %d", c.Code)
	}
	for _, unwanted := range []string{"主面板公告", "TG: @master"} {
		if strings.Contains(body, unwanted) {
			t.Fatalf("不该出现主面板的文案 %q", unwanted)
		}
	}
}
