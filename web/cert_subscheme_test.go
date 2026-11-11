package web

import (
	"encoding/json"
	"testing"

	"github.com/Maoyangui/m-ui/database/model"
)

// 证书页要知道有没有用户:订阅从 http 改成 https(或反过来)时,所有用户手里的订阅地址都会变、旧地址失效。
// 有用户时「用于订阅 HTTPS」默认不勾,勾了要明确提示(见 assets/js/subtls.js);代理名下的用户同样算在内。
func TestCertInfoCountsSubUsers(t *testing.T) {
	s, run := certServer(t)
	read := func() float64 {
		w := certCall(s, "GET", "", "")
		var out map[string]interface{}
		if err := json.Unmarshal(w.Body.Bytes(), &out); err != nil {
			t.Fatal(err)
		}
		n, ok := out["subUsers"].(float64)
		if !ok {
			t.Fatalf("GET /cert 应带 subUsers,得 %v", out["subUsers"])
		}
		return n
	}
	if n := read(); n != 0 {
		t.Fatalf("没有用户时 subUsers 应为 0,得 %v", n)
	}
	run.DB().Create(&model.User{Name: "alice", Enabled: true})
	run.DB().Create(&model.User{Name: "bob", Enabled: false, ResellerId: 3, SubToken: "tok"})
	if n := read(); n != 2 {
		t.Fatalf("subUsers 应把停用的、代理名下的都算上(2),得 %v", n)
	}
}
