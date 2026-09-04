package web

import "testing"

// 面板路径是用户自己填的:填成 app、/app、app/、带空格、带引号、多斜杠、甚至 /,
// 都要规整成能用的形式,否则改完路径面板就打不开了。
func TestNormalizePath(t *testing.T) {
	cases := map[string]string{
		"":           "/app/",
		"  ":         "/app/",
		"app":        "/app/",
		"/app":       "/app/",
		"app/":       "/app/",
		"/app/":      "/app/",
		" /ad/ ":     "/ad/",
		"\"/ad/\"":   "/ad/",
		"//ad//":     "/ad/",
		"/a/b":       "/a/b/",
		"a/b/":       "/a/b/",
		"/":          "/",
		"panel-2053": "/panel-2053/",
		"/很长的中文路径":   "/很长的中文路径/",
	}
	for in, want := range cases {
		if got := normalizePath(in, "/app/"); got != want {
			t.Errorf("normalizePath(%q) = %q,应为 %q", in, got, want)
		}
	}
	if got := normalizePath("", "/dl/"); got != "/dl/" {
		t.Errorf("默认值应生效: %q", got)
	}
}
