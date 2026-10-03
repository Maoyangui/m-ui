package render

import (
	"encoding/json"
	"testing"

	"github.com/Maoyangui/m-ui/database/model"
)

// 凭据里缺这条线路协议的口令时不下发这个用户:空口令谁都能冒用(socks / http 只要知道用户名),
// 缺 SS2022 密钥还会让整份配置起不来。缺口令的人不在表里,别人照常。
func TestUsersWithoutSecretNotRendered(t *testing.T) {
	ok := model.User{Id: 1, Name: "ok", Credentials: []byte(`{"hysteria2":{"password":"p"},"socks":{"password":"s"},"vless":{"uuid":"00000000-0000-0000-0000-000000000001"},"tuic":{"uuid":"00000000-0000-0000-0000-000000000001","password":"t"}}`)}
	bare := model.User{Id: 2, Name: "bare", Credentials: []byte(`{"anytls":{"password":"a"}}`)}
	for _, proto := range []string{"hysteria2", "socks", "vless", "tuic"} {
		line := model.Line{Name: "l", Protocol: proto, Port: 1}
		if proto == "vless" {
			line.Tls = []byte(`{"mode":"none"}`)
		}
		raw, err := renderInbound(line, NodeCert{ServerName: "x", CertPath: "c", KeyPath: "k"}, []model.User{ok, bare})
		if err != nil {
			t.Fatal(err)
		}
		var ib struct {
			Users []map[string]interface{} `json:"users"`
		}
		json.Unmarshal(raw, &ib)
		if len(ib.Users) != 1 {
			t.Fatalf("%s:缺口令的用户不该下发,应只剩 1 个: %v", proto, ib.Users)
		}
		name, _ := ib.Users[0]["name"].(string)
		if name == "" {
			name, _ = ib.Users[0]["username"].(string)
		}
		if name != "ok" {
			t.Fatalf("%s:留下的应是凭据齐全的用户: %v", proto, ib.Users)
		}
	}
}
