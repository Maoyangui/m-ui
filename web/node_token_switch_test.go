package web

import (
	"net/http/httptest"
	"path/filepath"
	"strings"
	"testing"

	"github.com/Maoyangui/m-ui/database"
	"github.com/Maoyangui/m-ui/database/model"
)

// 换了一台服务器接着用这一行(域名、API 地址不变):面板上只改令牌。记住的旧机器证书指纹要随之作废,
// 新机器第一次被认可时重新记住 —— 以前得再去点「重置指纹」,不然推送一直报"指纹变了"。
// 不改令牌(留空 / 同一个)的编辑不动指纹。
func TestNodeTokenChangeForgetsFingerprint(t *testing.T) {
	db, err := database.Open(filepath.Join(t.TempDir(), "x.db"))
	if err != nil {
		t.Fatal(err)
	}
	defer database.Close(db)
	s := &Server{db: db}
	db.Create(&model.Node{Name: "主机", IsLocal: true, Enabled: true, Sort: 1})
	db.Create(&model.Node{Name: "直连", ApiUrl: "https://zl.example.com:2053/app/", Token: "old", Insecure: true, CertFP: "aaaa", Enabled: true, Sort: 2})
	put := func(body string) {
		t.Helper()
		w := httptest.NewRecorder()
		s.handleNodeItem(w, httptest.NewRequest("PUT", "http://x/app/api/nodes/2", strings.NewReader(body)))
		if w.Code != 200 {
			t.Fatalf("改服务器失败: %d %s", w.Code, w.Body.String())
		}
	}
	fp := func() (string, string) {
		var n model.Node
		db.First(&n, 2)
		return n.CertFP, n.Token
	}
	base := `"name":"直连","apiUrl":"https://zl.example.com:2053/app/","insecure":true,"enabled":true,"sort":2`
	put(`{` + base + `}`) // 令牌留空:沿用,指纹不动
	if f, tok := fp(); f != "aaaa" || tok != "old" {
		t.Fatalf("没改令牌的编辑不该动指纹 / 令牌: %q %q", f, tok)
	}
	put(`{` + base + `,"token":"old"}`) // 同一个令牌
	if f, _ := fp(); f != "aaaa" {
		t.Fatalf("令牌没变不该动指纹: %q", f)
	}
	put(`{` + base + `,"token":"new"}`) // 换成新机器的令牌
	if f, tok := fp(); f != "" || tok != "new" {
		t.Fatalf("换了令牌,旧指纹应作废: %q %q", f, tok)
	}
}
