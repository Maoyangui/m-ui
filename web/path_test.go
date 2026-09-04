package web

import (
	"net"
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

// 新线路的默认端口:五位、避开已有线路与面板/订阅/代理面板,且系统里确实能监听。
func TestFreePort(t *testing.T) {
	db, err := database.Open(filepath.Join(t.TempDir(), "x.db"))
	if err != nil {
		t.Fatal(err)
	}
	defer database.Close(db)
	s := &Server{db: db}

	// 占一个端口,确认不会被选中
	ln, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatal(err)
	}
	defer ln.Close()
	taken := ln.Addr().(*net.TCPAddr).Port
	db.Create(&model.Line{Name: "a", Protocol: "hysteria2", Port: 30001, Enabled: true})

	seen := map[int]bool{}
	for i := 0; i < 20; i++ {
		p, err := s.freePort()
		if err != nil {
			t.Fatal(err)
		}
		if p < 10000 || p > 65535 {
			t.Fatalf("端口应是五位: %d", p)
		}
		if p == 30001 || p == 2053 || p == 2054 || p == 2056 || p == taken {
			t.Fatalf("选到了被占用的端口: %d", p)
		}
		seen[p] = true
	}
	if len(seen) < 10 {
		t.Fatalf("端口应当随机,20 次只出现 %d 个不同值", len(seen))
	}
}

// 反方向:把面板/订阅端口改成线路正在用的端口,也要在保存时拦下。
func TestPanelPortConflictsWithLine(t *testing.T) {
	db, err := database.Open(filepath.Join(t.TempDir(), "x.db"))
	if err != nil {
		t.Fatal(err)
	}
	defer database.Close(db)
	s := &Server{db: db}
	db.Create(&model.Line{Name: "香港1", Protocol: "hysteria2", Port: 30443, Enabled: true})

	if err := s.validatePorts(map[string]string{"webPort": "30443"}); err == nil {
		t.Fatal("面板端口撞线路应被拒绝")
	}
	if err := s.validatePorts(map[string]string{"subPort": "30443"}); err == nil {
		t.Fatal("订阅端口撞线路应被拒绝")
	}
	// 代理面板关掉时它不占端口,可以与线路同号
	if err := s.validatePorts(map[string]string{"resellerPort": "30443", "resellerEnabled": "false"}); err != nil {
		t.Fatalf("代理面板关闭时不应判冲突: %v", err)
	}
	if err := s.validatePorts(map[string]string{"webPort": "3000"}); err != nil {
		t.Fatalf("正常端口应通过: %v", err)
	}
}

// 删服务器时,线路的"部署到服务器"里不能留下悬空 id;
// 只部署在这台机器上的线路要停用(留空等于跑到所有机器上,不是管理员的本意)。
func TestDetachLinesFromNode(t *testing.T) {
	db, err := database.Open(filepath.Join(t.TempDir(), "x.db"))
	if err != nil {
		t.Fatal(err)
	}
	defer database.Close(db)
	s := &Server{db: db}
	db.Create(&model.Line{Name: "只在2号机", Protocol: "hysteria2", Port: 1, Enabled: true, NodeIds: []byte(`[2]`)})
	db.Create(&model.Line{Name: "两台都有", Protocol: "anytls", Port: 2, Enabled: true, NodeIds: []byte(`[1,2]`)})
	db.Create(&model.Line{Name: "全部机器", Protocol: "tuic", Port: 3, Enabled: true})

	disabled := s.detachLinesFromNode(2)
	if len(disabled) != 1 || disabled[0] != "只在2号机" {
		t.Fatalf("应只停用一条: %v", disabled)
	}
	var lines []model.Line
	db.Order("id asc").Find(&lines)
	if lines[0].Enabled || len(lines[0].NodeIds) != 0 {
		t.Fatalf("失去全部服务器的线路应停用并清空部署列表: %+v", lines[0])
	}
	if string(lines[1].NodeIds) != "[1]" || !lines[1].Enabled {
		t.Fatalf("还有其它服务器的线路只摘掉这一台: %s", lines[1].NodeIds)
	}
	if len(lines[2].NodeIds) != 0 || !lines[2].Enabled {
		t.Fatalf("原本就是全部机器的线路不该被动: %+v", lines[2])
	}
}
