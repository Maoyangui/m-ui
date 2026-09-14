package web

import (
	"encoding/json"
	"path/filepath"
	"regexp"
	"testing"

	"github.com/Maoyangui/m-ui/database"
	"github.com/Maoyangui/m-ui/database/model"
)

// REALITY 线路没填 short_id 时保存会自动补一个:入站默认接受空 short_id,等于只靠公钥认人。
func TestRealityLineGetsShortID(t *testing.T) {
	db, err := database.Open(filepath.Join(t.TempDir(), "x.db"))
	if err != nil {
		t.Fatal(err)
	}
	defer database.Close(db)
	s := &Server{db: db}

	l := model.Line{Name: "r", Protocol: "vless", Port: 30443,
		Tls: []byte(`{"mode":"reality","reality":{"private_key":"k","public_key":"p","handshake_server":"www.apple.com","handshake_port":443}}`)}
	if err := s.validateLine(&l); err != nil {
		t.Fatal(err)
	}
	var tls struct {
		Mode    string `json:"mode"`
		Reality struct {
			PrivateKey string   `json:"private_key"`
			ShortIDs   []string `json:"short_ids"`
		} `json:"reality"`
	}
	if err := json.Unmarshal(l.Tls, &tls); err != nil {
		t.Fatal(err)
	}
	if tls.Mode != "reality" || tls.Reality.PrivateKey != "k" {
		t.Fatalf("其它字段不该被动:%s", l.Tls)
	}
	if len(tls.Reality.ShortIDs) != 1 || !regexp.MustCompile(`^[0-9a-f]{2,16}$`).MatchString(tls.Reality.ShortIDs[0]) {
		t.Fatalf("应补一个十六进制 short_id,得 %v", tls.Reality.ShortIDs)
	}

	// 已经填了的原样保留
	kept := model.Line{Name: "r2", Protocol: "vless", Port: 30444,
		Tls: []byte(`{"mode":"reality","reality":{"private_key":"k","short_ids":["abcd"]}}`)}
	if err := s.validateLine(&kept); err != nil {
		t.Fatal(err)
	}
	if string(kept.Tls) != `{"mode":"reality","reality":{"private_key":"k","short_ids":["abcd"]}}` {
		t.Fatalf("填了 short_id 的不该被改写:%s", kept.Tls)
	}

	// 非 REALITY 的不碰
	plain := model.Line{Name: "c", Protocol: "trojan", Port: 30445, Tls: []byte(`{"mode":"cert"}`)}
	if err := s.validateLine(&plain); err != nil {
		t.Fatal(err)
	}
	if string(plain.Tls) != `{"mode":"cert"}` {
		t.Fatalf("非 REALITY 不该被改写:%s", plain.Tls)
	}
}
