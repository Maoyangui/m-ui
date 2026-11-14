package runner

import "testing"

// 只供测试的 Cloudflare 接口地址:只收完整的 http(s) 地址,不带用户名密码、查询参数;尾部斜杠去掉。
func TestCheckTestCFAPI(t *testing.T) {
	for _, c := range []struct {
		in, want string
		ok       bool
	}{
		{"https://mt-t.example:8443/client/v4/", "https://mt-t.example:8443/client/v4", true},
		{"http://127.0.0.1:9", "http://127.0.0.1:9", true},
		{"ftp://x.example/", "", false},
		{"https://user:pw@x.example/", "", false},
		{"https:///client/v4", "", false},
		{"https://x.example/?token=1", "", false},
		{"x.example", "", false},
	} {
		got, err := checkTestCFAPI(c.in)
		if (err == nil) != c.ok || got != c.want {
			t.Fatalf("%q → %q %v", c.in, got, err)
		}
	}
}
