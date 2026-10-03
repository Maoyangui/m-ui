package render

import (
	"encoding/base64"
	"testing"

	"github.com/Maoyangui/m-ui/database/model"
)

// 线路一个用户都没有时不能渲染成空用户表:socks / http / mixed 会开成不鉴权的开放代理,SS2022 会退化成只认服务端
// PSK 的单用户入站(老用户的链接里都有 PSK),没有线路级密码的传统 SS 直接建不起来。用户全停、代理到期、停用 / 删除副机
// 推的空用户表都会走到这里(审计 render.go:283、core/inbound_users.go:62、runner.go:777)。
func TestEmptyLineGetsPlaceholderUser(t *testing.T) {
	for _, l := range []model.Line{
		{Name: "s5", Protocol: "socks"},
		{Name: "h", Protocol: "http"},
		{Name: "mx", Protocol: "mixed"},
		{Name: "ss2022-128", Protocol: "shadowsocks", Options: raw(map[string]string{"method": "2022-blake3-aes-128-gcm", "password": "x"})},
		{Name: "ss2022-256", Protocol: "shadowsocks", Options: raw(map[string]string{"method": "2022-blake3-aes-256-gcm", "password": "x"})},
		{Name: "ss-legacy", Protocol: "shadowsocks", Options: raw(map[string]string{"method": "aes-256-gcm"})},
	} {
		users, err := renderUsers(l, nil, nil)
		if err != nil {
			t.Fatal(err)
		}
		if len(users) != 1 {
			t.Fatalf("%s:零用户应渲染一个占位用户,得到 %v", l.Name, users)
		}
		u := users[0]
		name, _ := u["name"].(string)
		if name == "" {
			name, _ = u["username"].(string)
		}
		pw, _ := u["password"].(string)
		if name != PlaceholderName || pw == "" {
			t.Fatalf("%s:占位用户不对: %v", l.Name, u)
		}
		again, _ := renderUsers(l, nil, nil)
		if again[0]["password"] != pw {
			t.Fatalf("%s:同一进程里占位凭据要稳定,不然每次热更新都重建入站", l.Name)
		}
		if l.Protocol == "shadowsocks" && l.Name != "ss-legacy" {
			key, err := base64.StdEncoding.DecodeString(pw)
			want := 32
			if l.Name == "ss2022-128" {
				want = 16
			}
			if err != nil || len(key) != want {
				t.Fatalf("%s:SS2022 占位密钥应是 %d 字节的 base64: %q", l.Name, want, pw)
			}
		}
	}
	a, _ := renderUsers(model.Line{Name: "a", Protocol: "socks"}, nil, nil)
	b, _ := renderUsers(model.Line{Name: "b", Protocol: "socks"}, nil, nil)
	if a[0]["password"] == b[0]["password"] {
		t.Fatal("不同线路的占位凭据不该相同")
	}
	// 其余协议空用户表就是谁都认证不过,不加占位
	if users, _ := renderUsers(model.Line{Name: "v", Protocol: "vless"}, nil, nil); len(users) != 0 {
		t.Fatalf("vless 零用户不该加占位: %v", users)
	}
	// 有真用户时不加占位
	alice := model.User{Name: "alice", Credentials: raw(map[string]map[string]string{"socks": {"password": "p"}})}
	if users, _ := renderUsers(model.Line{Name: "s5", Protocol: "socks"}, nil, []model.User{alice}); len(users) != 1 || users[0]["username"] != "alice" {
		t.Fatalf("有真用户时不该加占位: %v", users)
	}
}
