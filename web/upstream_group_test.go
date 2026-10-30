package web

import (
	"encoding/json"
	"fmt"
	"net/http/httptest"
	"path/filepath"
	"strings"
	"testing"

	"github.com/Maoyangui/m-ui/database"
	"github.com/Maoyangui/m-ui/database/model"
	"github.com/Maoyangui/m-ui/render"
	"github.com/Maoyangui/m-ui/runner"
)

func groupServer(t *testing.T) (*Server, func(method, path, body string) (int, string)) {
	t.Helper()
	run, err := runner.New(filepath.Join(t.TempDir(), "m.db"))
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { database.Close(run.DB()) })
	s := NewServer(run)
	call := func(method, path, body string) (int, string) {
		t.Helper()
		r := httptest.NewRequest(method, "http://x/app/api/"+path, strings.NewReader(body))
		w := httptest.NewRecorder()
		if path == "upstreams" {
			s.handleUpstreams(w, r)
		} else {
			s.handleUpstreamItem(w, r)
		}
		return w.Code, w.Body.String()
	}
	return s, call
}

func socksUp(name string, port int) string {
	return fmt.Sprintf(`{"name":%q,"type":"socks","options":{"server":"127.0.0.1","server_port":%d}}`, name, port)
}

func groupUp(name string, members ...string) string {
	b, _ := json.Marshal(map[string]interface{}{"name": name, "type": render.GroupType, "options": map[string]interface{}{"outbounds": members}})
	return string(b)
}

func TestGroupRules(t *testing.T) {
	s, call := groupServer(t)
	mustOK := func(code int, body string) {
		t.Helper()
		if code != 200 {
			t.Fatalf("应该成功: %d %s", code, body)
		}
	}
	mustFail := func(code int, body, want string) {
		t.Helper()
		if code == 200 || !strings.Contains(body, want) {
			t.Fatalf("应该因「%s」被拒: %d %s", want, code, body)
		}
	}
	code, body := call("POST", "upstreams", socksUp("a", 1))
	mustOK(code, body)
	code, body = call("POST", "upstreams", socksUp("b", 2))
	mustOK(code, body)
	code, body = call("POST", "upstreams", socksUp("c", 3))
	mustOK(code, body)

	code, body = call("POST", "upstreams", groupUp("g", "a"))
	mustFail(code, body, "至少要选两条")
	code, body = call("POST", "upstreams", groupUp("g", "a", "a"))
	mustFail(code, body, "至少要选两条")
	code, body = call("POST", "upstreams", groupUp("g", "a", "gone"))
	mustFail(code, body, "不存在")
	code, body = call("POST", "upstreams", groupUp("g", "a", "g"))
	mustFail(code, body, "不能包含它自己")

	code, body = call("POST", "upstreams", groupUp("g", "a", " b "))
	mustOK(code, body)
	var g model.Upstream
	s.db.Where("name = ?", "g").First(&g)
	var o map[string]interface{}
	json.Unmarshal(g.Options, &o)
	if o["interval"] != "30s" || o["url"] != "http://www.gstatic.com/generate_204" || o["interrupt_exist_connections"] != true {
		t.Fatalf("测试参数由服务端定: %v", o)
	}
	if m := render.GroupMembers(g); len(m) != 2 || m[0] != "a" || m[1] != "b" {
		t.Fatalf("成员去空格: %v", m)
	}

	code, body = call("POST", "upstreams", groupUp("g2", "g", "a"))
	mustFail(code, body, "不能再套上游组")

	// 上游组的成员不能删,也不能改成组
	var a model.Upstream
	s.db.Where("name = ?", "a").First(&a)
	code, body = call("DELETE", fmt.Sprintf("upstreams/%d", a.Id), "")
	mustFail(code, body, "在上游组「g」里")
	code, body = call("PUT", fmt.Sprintf("upstreams/%d", a.Id), groupUp("a", "b", "c"))
	mustFail(code, body, "不能改成组")

	// 成员改名:组里的名字一起改
	code, body = call("PUT", fmt.Sprintf("upstreams/%d", a.Id), socksUp("a-new", 1))
	mustOK(code, body)
	s.db.First(&g, g.Id)
	if m := render.GroupMembers(g); m[0] != "a-new" || m[1] != "b" {
		t.Fatalf("改名要跟着改: %v", m)
	}
	if o := string(g.Options); !strings.Contains(o, "\"interval\"") {
		t.Fatalf("改名不该丢别的参数: %s", o)
	}

	// 线路用组:整份配置要过内核校验,组能被线路用
	line := model.Line{Name: "l", Protocol: "shadowsocks", Port: 31999, Enabled: true, UpstreamId: g.Id, Options: []byte(rrSS)}
	s.db.Create(&line)
	if err := validateFullConfig(s.db, s.run.NodeCert()); err != nil {
		t.Fatalf("线路走上游组的配置内核要认: %v", err)
	}
	// 组被线路用着,不能删
	code, body = call("DELETE", fmt.Sprintf("upstreams/%d", g.Id), "")
	mustFail(code, body, "仍被 1 条线路使用")

	// 巡检按「谁在用」:用组就算用它的成员
	users, _ := s.upstreamUsers()
	s.db.Where("name = ?", "b").First(&a)
	if len(users[a.Id]) == 0 {
		t.Fatalf("线路用组,成员也算在用: %v", users)
	}
	s.db.Where("name = ?", "c").First(&a)
	if len(users[a.Id]) != 0 {
		t.Fatalf("组外的不算: %v", users)
	}

	// 测试地址改了,组跟着改
	s.db.Create(&model.Setting{Key: "upstreamTestUrl", Value: "http://t.example/204"})
	if !s.syncGroupTestURL() {
		t.Fatal("地址变了要报有改动")
	}
	s.db.First(&g, g.Id)
	json.Unmarshal(g.Options, &o)
	if o["url"] != "http://t.example/204" {
		t.Fatalf("组的测试地址要跟着改: %v", o)
	}
	if s.syncGroupTestURL() {
		t.Fatal("没变就不该报有改动")
	}
}

// 组里放了指回用这个组的线路的线路上游:绕成环,保存要拒;保存线路时也要查出来。
func TestGroupRelayLoop(t *testing.T) {
	s, call := groupServer(t)
	s.db.Create(&model.Node{Name: "本机", IsLocal: true, Enabled: true, Addr: "1.1.1.1"})
	line := model.Line{Name: "L", Protocol: "shadowsocks", Port: 31998, Enabled: true, Options: []byte(rrSS)}
	s.db.Create(&line)
	code, body := call("POST", "upstreams", socksUp("x", 1))
	if code != 200 {
		t.Fatal(body)
	}
	code, body = call("POST", "upstreams", socksUp("y", 2))
	if code != 200 {
		t.Fatal(body)
	}
	code, body = call("POST", "upstreams", fmt.Sprintf(`{"name":"toL","fromLine":%d,"fromNode":1}`, line.Id))
	if code != 200 {
		t.Fatal(body)
	}
	code, body = call("POST", "upstreams", groupUp("g", "x", "y"))
	if code != 200 {
		t.Fatal(body)
	}
	var g model.Upstream
	s.db.Where("name = ?", "g").First(&g)
	s.db.Model(&model.Line{}).Where("id = ?", line.Id).Update("upstream_id", g.Id)

	code, body = call("PUT", fmt.Sprintf("upstreams/%d", g.Id), groupUp("g", "x", "toL"))
	if code == 200 || !strings.Contains(body, "绕成环") {
		t.Fatalf("组里放指回自己的线路上游要拒: %d %s", code, body)
	}
	s.db.First(&g, g.Id)
	if m := render.GroupMembers(g); m[1] != "y" {
		t.Fatalf("被拒后组要保持原样: %v", m)
	}
}
