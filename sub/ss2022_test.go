package sub

import (
	"encoding/base64"
	"strings"
	"testing"

	"github.com/Maoyangui/m-ui/database/model"
)

// ss2022 多用户:客户端密码必须是 "服务端PSK:用户PSK"(与 旧面板 一致),否则客户端握手被服务端静默丢弃表现为超时。
func TestShadowsocks2022ClientPasswordIncludesServerPSK(t *testing.T) {
	user := model.User{Name: "u", Credentials: []byte(`{"shadowsocks":{"password":"userpw32"},"shadowsocks16":{"password":"userpw16"}}`)}
	entries := []Entry{{Host: "1.2.3.4"}}
	l2022 := model.Line{Name: "ss22", Protocol: "shadowsocks", Port: 1, Enabled: true, Options: []byte(`{"method":"2022-blake3-aes-128-gcm","password":"SERVERPSK"}`)}
	links := GenerateLinks(user, []model.Line{l2022}, entries)
	if len(links) != 1 {
		t.Fatal(links)
	}
	dec, _ := base64.StdEncoding.DecodeString(strings.TrimPrefix(strings.Split(links[0], "@")[0], "ss://"))
	if string(dec) != "2022-blake3-aes-128-gcm:SERVERPSK:userpw16" {
		t.Fatalf("ss2022 链接密码应为 服务端PSK:用户PSK,实际 %q", dec)
	}
	out, _ := BuildClash(user, []model.Line{l2022}, entries, "", "")
	if !strings.Contains(out, "password: SERVERPSK:userpw16") {
		t.Fatalf("clash ss2022 密码应为 服务端PSK:用户PSK:\n%s", out)
	}
	// 普通算法只用用户密码
	lAES := model.Line{Name: "ss", Protocol: "shadowsocks", Port: 2, Enabled: true, Options: []byte(`{"method":"aes-128-gcm"}`)}
	links = GenerateLinks(user, []model.Line{lAES}, entries)
	dec, _ = base64.StdEncoding.DecodeString(strings.TrimPrefix(strings.Split(links[0], "@")[0], "ss://"))
	if string(dec) != "aes-128-gcm:userpw32" {
		t.Fatalf("普通 ss 应只用用户密码,实际 %q", dec)
	}
}
