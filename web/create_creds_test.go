package web

import (
	"bytes"
	"net/http/httptest"
	"path/filepath"
	"strings"
	"testing"

	"github.com/Maoyangui/m-ui/certutil"
	"github.com/Maoyangui/m-ui/database"
	"github.com/Maoyangui/m-ui/database/model"
	"github.com/Maoyangui/m-ui/render"
)

// 建号时请求体里自带的凭据 / 共享凭据一律不认:代理(或手滑的调用方)塞一份坏凭据,
// 部署这条线路的每台机器渲染配置都会失败,停用、到期、删号从此都下发不到数据面。
func TestCreateUserIgnoresSubmittedCredentials(t *testing.T) {
	dir := t.TempDir()
	db, err := database.Open(filepath.Join(dir, "x.db"))
	if err != nil {
		t.Fatal(err)
	}
	defer database.Close(db)
	crt, key := filepath.Join(dir, "a.crt"), filepath.Join(dir, "a.key")
	if err := certutil.GenerateSelfSigned([]string{"t.example.com"}, crt, key, 10); err != nil {
		t.Fatal(err)
	}
	cert := render.NodeCert{ServerName: "t.example.com", CertPath: crt, KeyPath: key}
	s := &Server{db: db}
	line := model.Line{Name: "hk", Protocol: "hysteria2", Port: 30443, Enabled: true}
	db.Create(&line)
	rs := model.Reseller{Name: "r", Enabled: true}
	db.Create(&rs)
	s.setResellerLines(rs.Id, []uint{line.Id})

	post := func(rid uint, body string) {
		t.Helper()
		r := httptest.NewRequest("POST", "http://x/app/api/users", strings.NewReader(body))
		if rid > 0 {
			r = r.WithContext(withScope(r, rid))
		}
		w := httptest.NewRecorder()
		s.handleUsers(w, r)
		if w.Code != 200 {
			t.Fatalf("建号失败: %d %s", w.Code, w.Body.String())
		}
	}
	post(rs.Id, `{"name":"r1","enabled":true,"lineIds":[1],"credentials":[]}`)
	post(rs.Id, `{"name":"r2","enabled":true,"lineIds":[1],"credentials":{"hysteria2":{"password":""},"vless":{"uuid":"x"}},"disabledReason":"quota"}`)
	post(0, `{"name":"m1","enabled":true,"lineIds":[1],"credentials":"abc","shareToken":"t","shareCreds":"zzz"}`)

	for _, name := range []string{"r1", "r2", "m1"} {
		var u model.User
		db.Where("name = ?", name).First(&u)
		if bytes.Contains(u.Credentials, []byte(`"x"`)) || !bytes.Contains(u.Credentials, []byte("hysteria2")) {
			t.Fatalf("%s 的凭据应由服务端生成: %s", name, u.Credentials)
		}
		if u.ShareToken != "" || u.ShareCreds != nil || u.DisabledReason != "" {
			t.Fatalf("%s 的共享与停用原因不该取自请求体: %+v", name, u)
		}
	}
	if err := validateFullConfig(db, cert); err != nil {
		t.Fatalf("建号后整配置应能渲染并通过干跑: %v", err)
	}
}
