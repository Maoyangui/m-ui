package web

import (
	"fmt"
	"net/http/httptest"
	"os"
	"path/filepath"
	"regexp"
	"strings"
	"testing"

	"github.com/Maoyangui/m-ui/creds"
	"github.com/Maoyangui/m-ui/database"
	"github.com/Maoyangui/m-ui/database/model"
	"github.com/Maoyangui/m-ui/render"
	"github.com/Maoyangui/m-ui/runner"
)

// 线路的协议和加密方式配不上(实机逐个跑过,原因见 render/combo.go):
//   - 新建、修改保存时拦下,报错点名协议、说该选什么;库里不留;配得上的照常存;
//   - 开关、批量启用时拦下(批量的点名是哪一条);停用不拦 —— 停用是补救手段;
//   - shadowsocks 在多用户入站上起不来的两种算法,保存时说清楚(以前是内核的 "invalid argument");
//   - 库里已有的「走 QUIC 配 Reality」不算进健康检查「该有几个监听」(渲染时摘掉了,见 render.Unstartable)。
//
// 线路都按停用建:保存照样走完全部校验,但保存后的重载不会真把数据面起起来占端口。
func TestLineComboRejectedOnSaveAndEnable(t *testing.T) {
	run, err := runner.New(filepath.Join(t.TempDir(), "m.db"))
	if err != nil {
		t.Fatal(err)
	}
	defer database.Close(run.DB())
	s, db := NewServer(run), run.DB()
	priv, _, err := creds.RealityKeypair()
	if err != nil {
		t.Fatal(err)
	}
	reality := fmt.Sprintf(`{"mode":"reality","reality":{"private_key":%q,"handshake_server":"www.microsoft.com","handshake_port":443,"short_ids":["0123456789abcdef"]}}`, priv)
	post := func(body string) (int, string) {
		w := httptest.NewRecorder()
		s.handleLines(w, httptest.NewRequest("POST", "http://x/app/api/lines", strings.NewReader(body)))
		return w.Code, w.Body.String()
	}
	count := func(name string) (n int64) {
		db.Model(&model.Line{}).Where("name = ?", name).Count(&n)
		return
	}

	// 保存时拦
	for _, tc := range []struct{ proto, tls, opts, want string }{
		{"hysteria2", reality, `{}`, "QUIC"},
		{"tuic", reality, `{}`, "QUIC"},
		{"anytls", reality, `{}`, "AnyTLS 配不了 Reality"},
		{"vmess", reality, `{}`, "VMess 配不了 Reality"},
		{"http", reality, `{}`, "HTTP 配不了 Reality"},
		{"mixed", `{"mode":"cert"}`, `{}`, "Mixed 不带 TLS"},
		{"socks", reality, `{}`, "SOCKS5 不带 TLS"},
		{"shadowsocks", `{"mode":"cert"}`, `{"method":"aes-256-gcm","password":"pw12345678"}`, "Shadowsocks 不带 TLS"},
		{"anytls", `{"mode":"none"}`, `{}`, "必须启用 TLS"},
		{"shadowsocks", `{"mode":"none"}`, `{"method":"2022-blake3-chacha20-poly1305"}`, "不支持多个用户共用一条线路"},
		{"shadowsocks", `{"mode":"none"}`, `{"method":"none","password":"pw12345678"}`, "不能选不加密"},
	} {
		name := "bad-" + tc.proto
		code, body := post(fmt.Sprintf(`{"name":%q,"protocol":%q,"port":%d,"enabled":false,"tls":%s,"options":%s}`, name, tc.proto, idleTCPPort(t), tc.tls, tc.opts))
		if code != 400 || !strings.Contains(body, tc.want) {
			t.Errorf("%s + %s 应被拦下、报错里有 %q: %d %s", tc.proto, tc.tls[:18], tc.want, code, body)
		}
		if count(name) != 0 {
			t.Errorf("%s 被拦下后库里不该留线路", name)
		}
	}
	// 配得上的照常存
	for i, tc := range []struct{ proto, tls, opts string }{
		{"vless", reality, `{"vision":true}`},
		{"trojan", reality, `{}`},
		{"trojan", `{"mode":"none"}`, `{}`}, // 只用于前面有 CDN / 反代做 TLS 的情况:表单里有提示,不拦
		{"vmess", `{"mode":"none"}`, `{}`},
		{"http", `{"mode":"none"}`, `{}`},
		{"shadowsocks", `{"mode":"none"}`, `{"method":"2022-blake3-aes-128-gcm"}`},
	} {
		name := fmt.Sprintf("ok-%d-%s", i, tc.proto)
		if code, body := post(fmt.Sprintf(`{"name":%q,"protocol":%q,"port":%d,"enabled":false,"tls":%s,"options":%s}`, name, tc.proto, idleTCPPort(t), tc.tls, tc.opts)); code != 200 {
			t.Errorf("%s + %s 配得上,应存得下: %d %s", tc.proto, tc.tls[:15], code, body)
		}
	}

	// 修改时拦:好的 VLESS + Reality 改成 Hysteria2,加密方式没跟着换
	var good model.Line
	db.Where("name = ?", "ok-0-vless").First(&good)
	w := httptest.NewRecorder()
	s.handleLineItem(w, httptest.NewRequest("PUT", fmt.Sprintf("http://x/app/api/lines/%d", good.Id),
		strings.NewReader(fmt.Sprintf(`{"name":"ok-0-vless","protocol":"hysteria2","port":%d,"enabled":false,"tls":%s}`, good.Port, reality))))
	if w.Code != 400 || !strings.Contains(w.Body.String(), "Hysteria2 配不了 Reality") {
		t.Fatalf("改成配不上的组合应被拦下: %d %s", w.Code, w.Body.String())
	}
	var after model.Line
	db.First(&after, good.Id)
	if after.Protocol != "vless" {
		t.Fatalf("被拦下后线路不该变: %+v", after)
	}

	// 库里已有的坏线路(以前存得下):启用不了,停用可以
	old := model.Line{Name: "老的hy2", Protocol: "hysteria2", Port: idleTCPPort(t), Tls: []byte(reality), Sort: 90}
	db.Create(&old)
	db.Model(&model.Line{}).Where("id = ?", old.Id).Update("enabled", false) // Create 会把 default:true 的 false 写成 true
	toggle := func(id uint) (int, string) {
		w := httptest.NewRecorder()
		s.handleLineItem(w, httptest.NewRequest("POST", fmt.Sprintf("http://x/app/api/lines/%d/toggle", id), nil))
		return w.Code, w.Body.String()
	}
	if code, body := toggle(old.Id); code != 400 || !strings.Contains(body, "Hysteria2 配不了 Reality") {
		t.Fatalf("开关启用配不上的线路应被拦下: %d %s", code, body)
	}
	batch := func(body string) (int, string) {
		w := httptest.NewRecorder()
		s.handleLineItem(w, httptest.NewRequest("POST", "http://x/app/api/lines/batch", strings.NewReader(body)))
		return w.Code, w.Body.String()
	}
	if code, body := batch(fmt.Sprintf(`{"ids":[%d,%d],"action":"enable"}`, good.Id, old.Id)); code != 400 || !strings.Contains(body, "线路「老的hy2」") || !strings.Contains(body, "配不了 Reality") {
		t.Fatalf("批量启用里有配不上的线路应整批拦下并点名: %d %s", code, body)
	}
	db.First(&after, good.Id)
	if after.Enabled {
		t.Fatal("整批被拦下,同批里好的那条也不该被启用")
	}
	// 已经启用着的坏线路(老库):停得掉
	db.Model(&model.Line{}).Where("id = ?", old.Id).Update("enabled", true)
	if n := s.expectedLines(); n != 0 {
		t.Fatalf("起不来的线路渲染时摘掉了,健康检查不该等它的监听:expectedLines = %d", n)
	}
	if code, body := toggle(old.Id); code != 200 {
		t.Fatalf("停用应通过: %d %s", code, body)
	}
	if code, body := batch(fmt.Sprintf(`{"ids":[%d],"action":"disable"}`, old.Id)); code != 200 {
		t.Fatalf("批量停用应通过: %d %s", code, body)
	}
	// 入站起得来、只是订阅给不出节点的(AnyTLS 配 Reality)照旧算在该有的监听里:渲染时没摘它
	any := model.Line{Name: "老的anytls", Protocol: "anytls", Port: idleTCPPort(t), Tls: []byte(reality), Enabled: true, Sort: 91}
	db.Create(&any)
	if n := s.expectedLines(); n != 1 {
		t.Fatalf("AnyTLS 配 Reality 的入站起得来,该算进健康检查:expectedLines = %d", n)
	}
}

// 前端选的时候只给配得上的:lines.js 的 PROTOCOLS(每个协议的 tls 列表、默认方式)和后端 render.TLSModes / Protocols 一致;
// 下拉里的 shadowsocks 算法没有后端会拦的。两边各写一张表,靠这个测试钉住不走样。
func TestLineComboTableMatchesFrontend(t *testing.T) {
	js, err := os.ReadFile("assets/js/pages/lines.js")
	if err != nil {
		t.Fatal(err)
	}
	src := strings.ReplaceAll(string(js), "\r\n", "\n")
	row := regexp.MustCompile(`(?m)^  (\w+): \{ tls: \[([^\]]*)\], tlsDefault: '(\w+)'`)
	seen := map[string]bool{}
	for _, m := range row.FindAllStringSubmatch(src, -1) {
		proto, def := m[1], m[3]
		var modes []string
		for _, x := range strings.Split(m[2], ",") {
			modes = append(modes, strings.Trim(strings.TrimSpace(x), "'"))
		}
		seen[proto] = true
		spec, ok := render.Protocols[proto]
		if !ok {
			t.Errorf("前端有协议 %s,后端没有", proto)
			continue
		}
		if got, want := strings.Join(modes, ","), strings.Join(render.TLSModes(proto), ","); got != want {
			t.Errorf("%s 能选的加密方式:前端 [%s],后端 [%s]", proto, got, want)
		}
		if def != spec.TLSDefault {
			t.Errorf("%s 的默认加密方式:前端 %s,后端 %s", proto, def, spec.TLSDefault)
		}
	}
	for proto := range render.Protocols {
		if !seen[proto] {
			t.Errorf("后端的协议 %s 在 lines.js 的 PROTOCOLS 里没找到(表的写法变了?)", proto)
		}
	}
	m := regexp.MustCompile(`const SS_METHODS = \[([^\]]*)\];`).FindStringSubmatch(src)
	if m == nil {
		t.Fatal("lines.js 里找不到 SS_METHODS")
	}
	for _, x := range strings.Split(m[1], ",") {
		method := strings.Trim(strings.TrimSpace(x), "'")
		if err := render.SSMethodProblem(method); err != nil {
			t.Errorf("下拉里的算法 %s 后端会拦: %v", method, err)
		}
	}
}
