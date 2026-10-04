package web

import (
	"encoding/json"
	"net/http/httptest"
	"path/filepath"
	"strings"
	"testing"

	"github.com/Maoyangui/m-ui/database"
	"github.com/Maoyangui/m-ui/database/model"
	"github.com/Maoyangui/m-ui/runner"
)

const rrSS = `{"method":"aes-128-gcm","password":"pw12345678"}`

// 保存线路时校验分流规则:内容整理后再存、指向不存在的上游要拒、空列表存成空。
func TestValidateLineRouteRules(t *testing.T) {
	db, err := database.Open(filepath.Join(t.TempDir(), "x.db"))
	if err != nil {
		t.Fatal(err)
	}
	defer database.Close(db)
	s := &Server{db: db}
	db.Create(&model.Upstream{Name: "warp", Type: "socks", Options: []byte(`{"server":"127.0.0.1","server_port":40000}`)})
	l := model.Line{Name: "a", Protocol: "shadowsocks", Port: 31001, Options: []byte(rrSS),
		RouteRules: json.RawMessage(`[{"type":"domain_suffix","values":["Netflix.com","netflix.com"],"to":1},{"type":"domain","values":["x.com"],"to":99}]`)}
	if err := s.validateLine(&l); err == nil || !strings.Contains(err.Error(), "第 2 条分流规则指向的上游不存在") {
		t.Fatalf("指向不存在的上游要拒: %v", err)
	}
	l.RouteRules = json.RawMessage(`[{"type":"domain_suffix","values":["Netflix.com","netflix.com"],"to":1},{"type":"port","values":["8000-9000"],"to":-1}]`)
	if err := s.validateLine(&l); err != nil {
		t.Fatal(err)
	}
	if want := `[{"type":"domain_suffix","values":["netflix.com"],"to":1},{"type":"port","values":["8000:9000"],"to":-1}]`; string(l.RouteRules) != want {
		t.Fatalf("要存整理过的:\n%s", l.RouteRules)
	}
	l.RouteRules = json.RawMessage(`[]`)
	if err := s.validateLine(&l); err != nil || l.RouteRules != nil {
		t.Fatalf("空列表存成空: %v %s", err, l.RouteRules)
	}
	l.RouteRules = json.RawMessage(`[{"type":"domain","values":["https://x.com"],"to":0}]`)
	if err := s.validateLine(&l); err == nil || !strings.Contains(err.Error(), "不是域名") {
		t.Fatalf("不合法的值要拒并说清楚: %v", err)
	}
}

// 删上游:分流规则指向它也算在用;巡检 / "未使用"按规则指向的也算。
func TestUpstreamRefsIncludeRouteRules(t *testing.T) {
	db, err := database.Open(filepath.Join(t.TempDir(), "x.db"))
	if err != nil {
		t.Fatal(err)
	}
	defer database.Close(db)
	s := &Server{db: db}
	db.Create(&model.Node{Name: "主机", IsLocal: true, Enabled: true, Sort: 1})
	db.Create(&model.Upstream{Name: "warp", Type: "socks", Options: []byte(`{"server":"127.0.0.1","server_port":40000}`)})
	db.Create(&model.Upstream{Name: "jp", Type: "socks", Options: []byte(`{"server":"127.0.0.1","server_port":40001}`)})
	db.Create(&model.Line{Name: "a", Protocol: "shadowsocks", Port: 31001, Enabled: true, Options: []byte(rrSS),
		RouteRules: json.RawMessage(`[{"type":"domain_suffix","values":["jp.example"],"to":2}]`)})
	if n := linesUsingUpstream(db, 2); n != 1 {
		t.Fatalf("只被分流规则引用的上游也算在用: %d", n)
	}
	r := httptest.NewRequest("DELETE", "http://x/app/api/upstreams/2", nil)
	w := httptest.NewRecorder()
	s.handleUpstreamItem(w, r)
	if w.Code != 400 || !strings.Contains(w.Body.String(), "仍被 1 条线路使用") {
		t.Fatalf("被分流规则引用的上游不能删: %d %s", w.Code, w.Body.String())
	}
	var n int64
	db.Model(&model.Upstream{}).Where("id = ?", 2).Count(&n)
	if n != 1 {
		t.Fatal("被拒后上游要还在")
	}
	users, _ := s.upstreamUsers()
	if !users[2][1] || users[1] != nil {
		t.Fatalf("jp 由规则用在主机上,warp 没人用: %v", users)
	}
}

// 改线路时请求里没带 routeRules(外部程序按老格式改)不能把规则清掉;带了空列表才清。
func TestLineUpdateKeepsRouteRulesWhenOmitted(t *testing.T) {
	run, err := runner.New(filepath.Join(t.TempDir(), "m.db"))
	if err != nil {
		t.Fatal(err)
	}
	defer database.Close(run.DB())
	db := run.DB()
	s := NewServer(run)
	db.Create(&model.Upstream{Name: "warp", Type: "socks", Options: []byte(`{"server":"127.0.0.1","server_port":40000}`)})
	db.Create(&model.Line{Name: "a", Protocol: "shadowsocks", Port: 31001, Enabled: true, Options: []byte(rrSS),
		RouteRules: json.RawMessage(`[{"type":"domain_suffix","values":["openai.com"],"to":1}]`)})
	put := func(body string) {
		t.Helper()
		r := httptest.NewRequest("PUT", "http://x/app/api/lines/1", strings.NewReader(body))
		w := httptest.NewRecorder()
		s.handleLineItem(w, r)
		if w.Code != 200 {
			t.Fatalf("改线路失败: %d %s", w.Code, w.Body.String())
		}
	}
	rules := func() string {
		var l model.Line
		db.First(&l, 1)
		return string(l.RouteRules)
	}
	put(`{"name":"a","protocol":"shadowsocks","port":31002,"enabled":true,"options":` + rrSS + `}`)
	if !strings.Contains(rules(), "openai.com") {
		t.Fatalf("没带 routeRules 时规则要留着: %q", rules())
	}
	put(`{"name":"a","protocol":"shadowsocks","port":31002,"enabled":true,"options":` + rrSS + `,"routeRules":[{"type":"domain","values":["x.com"],"to":-1}]}`)
	if r := rules(); !strings.Contains(r, "x.com") || strings.Contains(r, "openai") {
		t.Fatalf("带了就按新的存: %q", r)
	}
	put(`{"name":"a","protocol":"shadowsocks","port":31002,"enabled":true,"options":` + rrSS + `,"routeRules":[]}`)
	if r := rules(); r != "" && r != "null" {
		t.Fatalf("带空列表就清掉: %q", r)
	}
}
