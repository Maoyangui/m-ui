package web

import (
	"path/filepath"
	"testing"

	"github.com/Maoyangui/m-ui/database"
	"github.com/Maoyangui/m-ui/database/model"
)

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

// 端口写错要等重启才发现,那时面板已经起不来:保存时就得拦住。
func TestValidatePorts(t *testing.T) {
	db, err := database.Open(filepath.Join(t.TempDir(), "x.db"))
	if err != nil {
		t.Fatal(err)
	}
	defer database.Close(db)
	s := &Server{db: db}

	bad := []map[string]string{
		{"webPort": "0"}, {"webPort": "70000"}, {"webPort": "abc"}, {"subPort": "-1"},
		{"webPort": "3000", "subPort": "3000"}, // 与订阅撞
		{"webPort": "2054"},                    // 与代理面板默认端口撞
		{"resellerPort": "2056"},               // 与订阅默认端口撞
	}
	for _, in := range bad {
		if err := s.validatePorts(in); err == nil {
			t.Errorf("应拒绝: %v", in)
		}
	}
	ok := []map[string]string{
		{}, {"webPort": "3000"}, {"webPort": "3000", "subPort": "3001", "resellerPort": "3002"},
		{"resellerPort": "2053", "resellerEnabled": "false"}, // 代理面板关着就不占端口
	}
	for _, in := range ok {
		if err := s.validatePorts(in); err != nil {
			t.Errorf("应通过 %v: %v", in, err)
		}
	}
}

// direct / block 是内置出站标签:上游重名会让整份配置冲突,重载时才炸,得在保存时拦。
func TestUpstreamReservedNames(t *testing.T) {
	db, err := database.Open(filepath.Join(t.TempDir(), "x.db"))
	if err != nil {
		t.Fatal(err)
	}
	defer database.Close(db)
	s := &Server{db: db}
	for _, name := range []string{"direct", "block", "DIRECT", " Block "} {
		up := model.Upstream{Name: name, Type: "socks"}
		if err := s.validateUpstream(&up); err == nil {
			t.Errorf("上游名 %q 应被拒绝", name)
		}
	}
	up := model.Upstream{Name: "warp", Type: "socks"}
	if err := s.validateUpstream(&up); err != nil {
		t.Errorf("正常名称应通过: %v", err)
	}
}

// 线路端口和面板/订阅/代理面板撞号:干跑不绑定端口,要等数据面启动才炸,保存时就得拦。
func TestLinePortConflictsWithPanel(t *testing.T) {
	db, err := database.Open(filepath.Join(t.TempDir(), "x.db"))
	if err != nil {
		t.Fatal(err)
	}
	defer database.Close(db)
	s := &Server{db: db}
	for _, port := range []int{2053, 2054, 2056} {
		l := model.Line{Name: "x", Protocol: "hysteria2", Port: port}
		if err := s.validateLine(&l); err == nil {
			t.Errorf("端口 %d 应被拒绝", port)
		}
	}
	// 关掉代理面板后 2054 可以给线路用
	db.Create(&model.Setting{Key: "resellerEnabled", Value: "false"})
	l := model.Line{Name: "x", Protocol: "hysteria2", Port: 2054}
	if err := s.validateLine(&l); err != nil {
		t.Errorf("代理面板关闭后 2054 应可用: %v", err)
	}
	ok := model.Line{Name: "y", Protocol: "hysteria2", Port: 30443}
	if err := s.validateLine(&ok); err != nil {
		t.Errorf("普通端口应通过: %v", err)
	}
}
