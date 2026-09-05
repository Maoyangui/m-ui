package web

import (
	"net"
	"path/filepath"
	"strconv"
	"strings"
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

// 机器上可能还跑着别的项目:手填的端口被别的程序占着就不该进库;
// 但端口没改的编辑不能误报(那时占着它的正是本机运行中的数据面)。
func TestPortBusyOnSave(t *testing.T) {
	db, err := database.Open(filepath.Join(t.TempDir(), "x.db"))
	if err != nil {
		t.Fatal(err)
	}
	defer database.Close(db)
	s := &Server{db: db}

	// 监听所有网卡:只绑 127.0.0.1 时 Windows 仍允许再绑 0.0.0.0,测不出冲突
	ln, err := net.Listen("tcp", ":0")
	if err != nil {
		t.Fatal(err)
	}
	defer ln.Close()
	busy := ln.Addr().(*net.TCPAddr).Port

	// 新建:占用的端口要被拒
	l := model.Line{Name: "x", Protocol: "hysteria2", Port: busy}
	if err := s.validateLine(&l); err == nil {
		t.Fatal("被别的程序占用的端口应被拒绝")
	}
	// 已有线路(端口不变)重新保存:不测占用,不能误报
	saved := model.Line{Name: "y", Protocol: "hysteria2", Port: busy}
	db.Create(&saved)
	edit := saved
	edit.Name = "y2"
	if err := s.validateLine(&edit); err != nil {
		t.Fatalf("端口没改的编辑不该报占用: %v", err)
	}
	// 设置里改端口撞上别的程序也要拒
	if err := s.validatePorts(map[string]string{"webPort": strconv.Itoa(busy)}); err == nil {
		t.Fatal("面板端口撞别的程序应被拒绝")
	}
	// 没改动的提交不测(库里没值时按默认值走,不会误报)
	if err := s.validatePorts(map[string]string{"webDomain": "x.com"}); err != nil {
		t.Fatalf("与端口无关的设置不该被拦: %v", err)
	}
}

// 订阅地址形式:设置开着(默认)用用户名,关掉后新建用户发随机令牌;
// 改设置不影响已有用户,代理建的用户一律随机令牌。
func TestSubTokenPolicy(t *testing.T) {
	db, err := database.Open(filepath.Join(t.TempDir(), "x.db"))
	if err != nil {
		t.Fatal(err)
	}
	defer database.Close(db)
	s := &Server{db: db}

	u1 := model.User{Name: "a"}
	s.applySubTokenPolicy(&u1)
	if u1.SubToken != "" {
		t.Fatalf("默认应沿用用户名: %q", u1.SubToken)
	}
	db.Create(&model.Setting{Key: "subUseUserName", Value: "false"})
	u2 := model.User{Name: "b"}
	s.applySubTokenPolicy(&u2)
	if len(u2.SubToken) < 20 {
		t.Fatalf("关掉后应发随机令牌: %q", u2.SubToken)
	}
	// 已经有地址的用户不会被重新发一个(改设置也不会动到已有用户:改用户走的是字段白名单,不含 sub_token)
	kept := model.User{Name: "old", SubToken: "KEEPTHISONE1234567890ab"}
	s.applySubTokenPolicy(&kept)
	if kept.SubToken != "KEEPTHISONE1234567890ab" {
		t.Fatalf("已有地址不该被改写: %q", kept.SubToken)
	}
	// 代理的用户由代理逻辑发令牌,这里不插手
	u3 := model.User{Name: "c", ResellerId: 3}
	s.applySubTokenPolicy(&u3)
	if u3.SubToken != "" {
		t.Fatal("代理用户不走这条策略")
	}
}

// 两条线路同端口:部署范围有交集才拦(端口只在同一台服务器上会撞),而且要说清是被哪条线路占了;
// 范围不相交(只在 2 号机 vs 只在 1 号机)放行。
func TestLinePortConflictsWithLine(t *testing.T) {
	db, err := database.Open(filepath.Join(t.TempDir(), "x.db"))
	if err != nil {
		t.Fatal(err)
	}
	defer database.Close(db)
	s := &Server{db: db}
	db.Create(&model.Line{Name: "香港1", Protocol: "hysteria2", Port: 30443, Enabled: true, NodeIds: []byte("[1]")})

	for _, l := range []model.Line{
		{Name: "新线路", Protocol: "anytls", Port: 30443, NodeIds: []byte("[1,2]")},
		{Name: "全部机器", Protocol: "anytls", Port: 30443},
	} {
		err := s.validateLine(&l)
		if err == nil || !strings.Contains(err.Error(), "香港1") {
			t.Fatalf("%s:同端口应拦下并点名线路,实际: %v", l.Name, err)
		}
	}
	only2 := model.Line{Name: "只在 2 号机", Protocol: "anytls", Port: 30443, NodeIds: []byte("[2]")}
	if err := s.validateLine(&only2); err != nil {
		t.Fatalf("只在 2 号机的同端口线路不该被 1 号机的线路拦下: %v", err)
	}
	// 编辑自己不算和自己冲突
	self := model.Line{Id: 1, Name: "香港1", Protocol: "hysteria2", Port: 30443, NodeIds: []byte("[1]")}
	if err := s.validateLine(&self); err != nil && strings.Contains(err.Error(), "线路") {
		t.Fatalf("编辑自己不该被自己占用的端口拦下: %v", err)
	}
}

// 设置里把默认端口 2053 明文填进去(库里原本是空 = 默认),不能被"自己占着的端口"拦下。
func TestPanelPortUnchangedSkipsBindCheck(t *testing.T) {
	db, err := database.Open(filepath.Join(t.TempDir(), "x.db"))
	if err != nil {
		t.Fatal(err)
	}
	defer database.Close(db)
	s := &Server{db: db}
	ln, err := net.Listen("tcp", ":2053") // 模拟面板自己正监听默认端口
	if err != nil {
		t.Skip("2053 在本机不可用,跳过")
	}
	defer ln.Close()
	if err := s.validatePorts(map[string]string{"webPort": "2053"}); err != nil {
		t.Fatalf("端口没变(默认值明文提交)不该被拦: %v", err)
	}
}
