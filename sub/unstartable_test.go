package sub

import (
	"encoding/base64"
	"strings"
	"testing"

	"github.com/Maoyangui/m-ui/database/model"
)

// 入站注定起不来的线路(走 QUIC 的协议配了 Reality,以前存得下,见 render.Unstartable)数据面渲染时跳过了,
// 订阅里也不给这个节点 —— 给了也是永远连不上的一个。三种格式都没有它,别的线路照常;线路改好之后回来。
// AnyTLS 配 Reality 这种入站起得来的不动(有人手写客户端配置在用,不替他从订阅里拿掉)。
func TestUnstartableLineNotInSubscription(t *testing.T) {
	s, db := shareServer(t) // alice 已有「香港1」(hysteria2,443,默认证书)
	reality := `{"mode":"reality","reality":{"private_key":"k","public_key":"p","short_ids":["ab"],"handshake_server":"www.microsoft.com"}}`
	db.Create(&model.Line{Name: "bad-hy2", Protocol: "hysteria2", Port: 8443, Enabled: true, Tls: []byte(reality), Sort: 2})
	db.Create(&model.Line{Name: "old-anytls", Protocol: "anytls", Port: 9443, Enabled: true, Tls: []byte(reality), Sort: 3})
	db.Exec("INSERT INTO user_lines (user_id, line_id) VALUES (1, 2), (1, 3)")
	db.Model(&model.User{}).Where("id = 1").Update("credentials", []byte(`{"hysteria2":{"password":"p"},"anytls":{"password":"q"}}`))

	fetch := func(query, ua string) string {
		t.Helper()
		w := doReq(s, "GET", "/sub/alice"+query, ua)
		if w.Code != 200 {
			t.Fatalf("取订阅 %q 得 %d", query, w.Code)
		}
		body := w.Body.String()
		if query == "" { // 链接订阅:设置里开着 base64 就先解开(默认不编码)
			if b, err := base64.StdEncoding.DecodeString(strings.TrimSpace(body)); err == nil {
				body = string(b)
			}
		}
		return body
	}
	formats := []struct{ query, ua, good, bad, kept string }{
		{"", "v2rayN/7.0", ":443?", ":8443?", ":9443?"},
		{"?format=clash", "curl/8.5.0", "port: 443", "port: 8443", "port: 9443"},
		{"?format=json", "curl/8.5.0", `"server_port": 443`, `"server_port": 8443`, `"server_port": 9443`},
	}
	for _, f := range formats {
		body := fetch(f.query, f.ua)
		if !strings.Contains(body, f.good) {
			t.Errorf("%q:好线路应照常在订阅里(找 %s)", f.query, f.good)
		}
		if strings.Contains(body, f.bad) {
			t.Errorf("%q:起不来的线路不该进订阅(找到了 %s)", f.query, f.bad)
		}
		if !strings.Contains(body, f.kept) {
			t.Errorf("%q:入站起得来的(AnyTLS 配 Reality)不该被拿掉(找 %s)", f.query, f.kept)
		}
	}
	// 改成证书:回到订阅里
	db.Model(&model.Line{}).Where("id = 2").Update("tls", []byte(`{"mode":"cert"}`))
	for _, f := range formats {
		if body := fetch(f.query, f.ua); !strings.Contains(body, f.bad) {
			t.Errorf("%q:线路改好之后应回到订阅里(找 %s)", f.query, f.bad)
		}
	}
}
