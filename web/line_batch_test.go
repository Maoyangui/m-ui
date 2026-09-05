package web

import (
	"net/http"
	"net/http/httptest"
	"path/filepath"
	"strings"
	"testing"

	"github.com/Maoyangui/m-ui/database"
	"github.com/Maoyangui/m-ui/database/model"
)

// 线路批量设置:启停、换上游、改部署服务器;任何一条不通过整批不保存;端口按服务器范围判冲突。
func TestLineBatch(t *testing.T) {
	db, err := database.Open(filepath.Join(t.TempDir(), "x.db"))
	if err != nil {
		t.Fatal(err)
	}
	defer database.Close(db)
	s := &Server{db: db}
	db.Create(&model.Node{Name: "主机", IsLocal: true, Enabled: true, Sort: 1})
	db.Create(&model.Node{Name: "副机", ApiUrl: "http://b", Enabled: true, Sort: 2})
	db.Create(&model.Upstream{Name: "warp", Type: "socks", Options: []byte(`{"server":"127.0.0.1","server_port":40000}`)})
	// 明文 shadowsocks 不需要证书,整体干跑能过
	ss := `{"method":"aes-128-gcm","password":"pw12345678"}`
	db.Create(&model.Line{Name: "a", Protocol: "shadowsocks", Port: 31001, Enabled: true, Options: []byte(ss), NodeIds: []byte(`[1]`)})
	db.Create(&model.Line{Name: "b", Protocol: "shadowsocks", Port: 31001, Enabled: true, Options: []byte(ss), NodeIds: []byte(`[2]`)})
	db.Create(&model.Line{Name: "c", Protocol: "shadowsocks", Port: 31002, Enabled: true, Options: []byte(ss)})

	call := func(body string) (int, string) {
		req := httptest.NewRequest("POST", "http://x/app/api/lines/batch", strings.NewReader(body))
		w := httptest.NewRecorder()
		s.handleLineItem(w, req)
		return w.Code, w.Body.String()
	}
	lines := func() map[string]model.Line {
		var all []model.Line
		db.Find(&all)
		out := map[string]model.Line{}
		for _, l := range all {
			out[l.Name] = l
		}
		return out
	}

	if code, body := call(`{"ids":[1,2],"action":"disable"}`); code != 200 || !strings.Contains(body, `"affected":2`) {
		t.Fatalf("批量停用应通过: %d %s", code, body)
	}
	if m := lines(); m["a"].Enabled || m["b"].Enabled || !m["c"].Enabled {
		t.Fatalf("a、b 应停用,c 不动: %+v", m)
	}
	if code, _ := call(`{"ids":[1,2],"action":"enable"}`); code != 200 {
		t.Fatal("批量启用应通过")
	}
	if code, body := call(`{"ids":[1,3],"action":"upstream","upstreamId":99}`); code != 400 || !strings.Contains(body, "上游不存在") {
		t.Fatalf("不存在的上游应被拒: %d %s", code, body)
	}
	if code, _ := call(`{"ids":[1,3],"action":"upstream","upstreamId":1}`); code != 200 {
		t.Fatal("换上游应通过")
	}
	if m := lines(); m["a"].UpstreamId != 1 || m["c"].UpstreamId != 1 || m["b"].UpstreamId != 0 {
		t.Fatalf("a、c 应换到 warp,b 不动: %+v", m)
	}
	// 把 a 改到全部服务器:和只在副机的 b 同端口 → 在副机上撞 → 整批拒绝,a 保持原样
	if code, body := call(`{"ids":[1],"action":"nodes","nodeIds":[]}`); code != 400 || !strings.Contains(body, "副机") {
		t.Fatalf("改到有交集的范围应被拒并点名服务器: %d %s", code, body)
	}
	if m := lines(); string(m["a"].NodeIds) != "[1]" {
		t.Fatalf("被拒后 a 的部署范围不该变: %s", m["a"].NodeIds)
	}
	// 批内彼此冲突:a、b 同端口一起改到主机
	if code, body := call(`{"ids":[1,2],"action":"nodes","nodeIds":[1]}`); code != 400 || !strings.Contains(body, "不能同时部署") {
		t.Fatalf("批内同端口线路改到同一台应被拒: %d %s", code, body)
	}
	// c 改到只在副机:没有冲突
	if code, _ := call(`{"ids":[3],"action":"nodes","nodeIds":[2]}`); code != 200 {
		t.Fatal("无冲突的范围改动应通过")
	}
	if m := lines(); string(m["c"].NodeIds) != "[2]" {
		t.Fatalf("c 的部署范围应改为副机: %s", m["c"].NodeIds)
	}
	if code, _ := call(`{"ids":[3],"action":"nodes","nodeIds":[9]}`); code != 400 {
		t.Fatal("不存在的服务器应被拒")
	}
	if code, _ := call(`{"ids":[42],"action":"enable"}`); code != http.StatusBadRequest {
		t.Fatal("不存在的线路应被拒")
	}
}
