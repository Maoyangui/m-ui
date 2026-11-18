package web

import (
	"encoding/json"
	"path/filepath"
	"testing"

	"github.com/Maoyangui/m-ui/creds"
	"github.com/Maoyangui/m-ui/database"
	"github.com/Maoyangui/m-ui/database/model"
)

// 审计 E3:保存线路时公钥由私钥算出,不收前端传来的(以前手改私钥后订阅里还是旧公钥,客户端握手失败)。
func TestValidateLineDerivesRealityPublicKey(t *testing.T) {
	db, err := database.Open(filepath.Join(t.TempDir(), "x.db"))
	if err != nil {
		t.Fatal(err)
	}
	defer database.Close(db)
	s := &Server{db: db}
	priv, pub, _ := creds.RealityKeypair()
	_, stale, _ := creds.RealityKeypair() // 另一对的公钥:模拟编辑框里留着的旧公钥
	tls := map[string]interface{}{"mode": "reality", "reality": map[string]interface{}{
		"private_key": priv, "public_key": stale, "short_ids": []string{"ab12"}, "handshake_server": "www.example.com", "handshake_port": 443}}
	raw, _ := json.Marshal(tls)
	line := model.Line{Name: "R", Protocol: "vless", Port: 34567, Tls: raw}
	if err := s.validateLine(&line); err != nil {
		t.Fatal(err)
	}
	var got struct {
		Reality struct {
			PrivateKey string `json:"private_key"`
			PublicKey  string `json:"public_key"`
		} `json:"reality"`
	}
	json.Unmarshal(line.Tls, &got)
	if got.Reality.PublicKey != pub || got.Reality.PrivateKey != priv {
		t.Fatalf("公钥应由私钥算出: %+v", got.Reality)
	}

	// 私钥不对:拒绝保存
	bad := map[string]interface{}{"mode": "reality", "reality": map[string]interface{}{"private_key": "not-a-key", "handshake_server": "www.example.com"}}
	raw, _ = json.Marshal(bad)
	line2 := model.Line{Name: "R2", Protocol: "vless", Port: 34568, Tls: raw}
	if err := s.validateLine(&line2); err == nil {
		t.Fatal("私钥格式不对应拒绝")
	}
}
