package main

import (
	"os"
	"path/filepath"
	"regexp"
	"strconv"
	"strings"
	"testing"
	"time"

	"github.com/Maoyangui/m-ui/database"
	"github.com/Maoyangui/m-ui/database/model"
)

// cells 一段文字在终端里占几格(汉字、全角标点两格,其余一格)。
func cells(s string) int {
	n := 0
	for _, r := range s {
		if r >= 0x1100 {
			n += 2
		} else {
			n++
		}
	}
	return n
}

// 主菜单:一屏放得下(24 行 × 80 列的终端)、每个编号恰好出现一次、每个编号在 runMenu 里都有对应的动作。
func TestMenuFitsAndIsComplete(t *testing.T) {
	// 菜单本体 + 上面最多 5 行(空行、标题、面板地址、提示、分隔线)+ 下面输入那一行,要在 24 行以内
	if len(menuRows)+6 > 24 {
		t.Fatalf("菜单 %d 行,加上头尾超过 24 行,小终端要往回翻", len(menuRows))
	}
	re := regexp.MustCompile(`(^| )(\d{1,2})\. `)
	seen := map[int]int{}
	for _, r := range menuRows {
		if r.Group != "" && cells(r.Group) != 4 {
			t.Errorf("分组名要正好两个汉字(对齐靠它): %q", r.Group)
		}
		if w := 2 + 4 + 2 + cells(r.Text); w > 78 {
			t.Errorf("这一行 %d 格,超过 78: %q", w, r.Text)
		}
		for _, m := range re.FindAllStringSubmatch(r.Text, -1) {
			n, _ := strconv.Atoi(m[2])
			seen[n]++
		}
	}
	src, err := os.ReadFile("menu.go")
	if err != nil {
		t.Fatal(err)
	}
	for n := 0; n <= 18; n++ {
		if seen[n] != 1 {
			t.Errorf("编号 %d 在菜单里出现了 %d 次", n, seen[n])
		}
		if !strings.Contains(string(src), `case "`+strconv.Itoa(n)+`"`) {
			t.Errorf("编号 %d 在 runMenu 里没有对应的 case", n)
		}
	}
	if len(seen) != 19 {
		t.Errorf("菜单里应当正好是 0–18: %v", seen)
	}
}

func TestMenuPortChange(t *testing.T) {
	free := func(int) bool { return true }
	busy := func(p int) bool { return p != 8443 }
	lines := map[int]string{443: "香港直连"}
	cur := map[string]string{} // 默认:面板 2053 / 订阅 2056 / 代理面板 2054
	for _, c := range []struct {
		name string
		cur  map[string]string
		kv   map[string]string
		bind func(int) bool
		want string // 空 = 应当通过
	}{
		{"只改路径", cur, map[string]string{"webPath": "/x/"}, busy, ""},
		{"换到空闲端口", cur, map[string]string{"webPort": "3000"}, free, ""},
		{"和订阅撞", cur, map[string]string{"webPort": "2056"}, free, "面板与订阅的端口都是 2056"},
		{"和代理面板的默认端口撞", cur, map[string]string{"subPort": "2054"}, free, "订阅与代理面板的端口都是 2054"},
		{"线路占着", cur, map[string]string{"webPort": "443"}, free, "面板端口 443 已被线路「香港直连」占用"},
		{"别的程序占着", cur, map[string]string{"subPort": "8443"}, busy, "订阅端口 8443 已被本机其它程序占用"},
		{"没改的端口不查占用(占着它的就是自己)", cur, map[string]string{"webPath": "/y/"}, func(int) bool { return false }, ""},
		{"自己的两个服务对调", cur, map[string]string{"webPort": "2056", "subPort": "2053"}, func(int) bool { return false }, ""},
		{"副机不起代理面板:可以用 2054", map[string]string{"nodeMode": "true"}, map[string]string{"webPort": "2054"}, free, ""},
		{"代理面板关着:可以用 2054", map[string]string{"resellerEnabled": "false"}, map[string]string{"subPort": "2054"}, free, ""},
	} {
		err := checkPortChange(c.cur, c.kv, lines, c.bind)
		switch {
		case c.want == "" && err != nil:
			t.Errorf("%s:应当通过,却报 %v", c.name, err)
		case c.want != "" && (err == nil || !strings.Contains(err.Error(), c.want)):
			t.Errorf("%s:应当报 %q,实际 %v", c.name, c.want, err)
		}
	}
}

func TestMenuListenAndPath(t *testing.T) {
	local := []string{"127.0.0.1", "10.0.0.5", "2001:db8::5"}
	for in, want := range map[string]string{"all": "", "ALL": "", "*": "", "0.0.0.0": "", "::": "", "127.0.0.1": "127.0.0.1", " 10.0.0.5 ": "10.0.0.5", "[2001:0db8::5]": "2001:db8::5", "::1": "::1"} {
		if got, err := parseListen(in, local); err != nil || got != want {
			t.Errorf("parseListen(%q) = %q, %v;要 %q", in, got, err, want)
		}
	}
	for _, in := range []string{"198.51.100.9", "panel.example.com", "10.0.0", "2053"} {
		if got, err := parseListen(in, local); err == nil {
			t.Errorf("parseListen(%q) 应当拒绝,却得 %q", in, got)
		}
	}
	for in, want := range map[string]string{"app": "/app/", "/app": "/app/", "app/": "/app/", "\"//a//b//\"": "/a/b/", " /x/ ": "/x/", "/": "/"} {
		if got := menuNormPath(in); got != want {
			t.Errorf("menuNormPath(%q) = %q,要 %q", in, got, want)
		}
	}
}

func TestMenuBackupPick(t *testing.T) {
	dir := t.TempDir()
	old := filepath.Join(dir, "m-ui-old.zip")
	recent := filepath.Join(dir, "pre-upgrade-new.zip")
	for i, p := range []string{old, recent, filepath.Join(dir, "notes.txt")} {
		if err := os.WriteFile(p, []byte("x"), 0o600); err != nil {
			t.Fatal(err)
		}
		at := time.Now().Add(time.Duration(i-5) * time.Hour)
		os.Chtimes(p, at, at)
	}
	os.Mkdir(filepath.Join(dir, "sub.zip"), 0o700) // 叫 .zip 的目录不算
	files := listBackups(dir)
	if len(files) != 2 || files[0].Path != recent || files[1].Path != old {
		t.Fatalf("应当只列 zip、最新的在前: %+v", files)
	}
	if listBackups(filepath.Join(dir, "nope")) != nil {
		t.Fatal("目录不存在时是空的")
	}
	if p, err := pickBackup(" 2 ", files); err != nil || p != old {
		t.Fatalf("按编号选: %q %v", p, err)
	}
	if p, err := pickBackup("\""+old+"\"", nil); err != nil || p != old {
		t.Fatalf("按路径选(带引号的也认): %q %v", p, err)
	}
	for _, in := range []string{"0", "3", filepath.Join(dir, "missing.zip"), dir} {
		if p, err := pickBackup(in, files); err == nil {
			t.Errorf("pickBackup(%q) 应当报错,却得 %q", in, p)
		}
	}
}

// 面板地址那一行、m-ui status、管理员改过名的用户名、副机接入信息:都从库里读,不联网。
func TestMenuInfoFromDB(t *testing.T) {
	p := filepath.Join(t.TempDir(), "m.db")
	db, err := database.Open(p)
	if err != nil {
		t.Fatal(err)
	}
	if got := quickPanelURL(map[string]string{}); got != "" {
		t.Fatalf("库里没有域名和公网 IP 时不显示地址(不为这一行去联网): %q", got)
	}
	db.Create(&model.Setting{Key: "webDomain", Value: "panel.example.com"})
	db.Create(&model.Setting{Key: "webPort", Value: "8443"})
	db.Create(&model.Setting{Key: "webPath", Value: "secret"})
	db.Create(&model.Setting{Key: "webCertFile", Value: "/c.pem"})
	db.Create(&model.Setting{Key: "webKeyFile", Value: "/c.key"})
	db.Create(&model.Admin{Username: "boss", Password: "x"})
	database.Close(db)

	s, err := settingsRO(p)
	if err != nil {
		t.Fatal(err)
	}
	if got := quickPanelURL(s); got != "https://panel.example.com:8443/secret/" {
		t.Fatalf("面板地址不对: %q", got)
	}
	if got := statusLine(p); !strings.Contains(got, "角色:主机") || !strings.Contains(got, "面板:https://panel.example.com:8443/secret/") || strings.Contains(got, "\033[") {
		t.Fatalf("status 不对: %q", got)
	}
	if got := statusLine(filepath.Join(t.TempDir(), "none.db")); !strings.Contains(got, "数据库读不出来") {
		t.Fatalf("库不在时: %q", got)
	}
	if got := adminName(p); got != "boss" {
		t.Fatalf("管理员改过名的要显示真名: %q", got)
	}
	if got := adminName(filepath.Join(t.TempDir(), "none.db")); got != "admin" {
		t.Fatalf("读不到时按 admin: %q", got)
	}
	// 接入信息:地址就是面板地址;令牌没有就生成一个并存下,再看一次还是同一个
	url, tok, err := nodeJoinInfo(p)
	if err != nil || url != "https://panel.example.com:8443/secret/" || len(tok) != 48 {
		t.Fatalf("接入信息不对: %q %q %v", url, tok, err)
	}
	if _, again, _ := nodeJoinInfo(p); again != tok {
		t.Fatalf("令牌应当存下来、不是每次新生成: %q → %q", tok, again)
	}
	if s, _ := settingsRO(p); s["nodeToken"] != tok {
		t.Fatalf("令牌没写进库: %q", s["nodeToken"])
	}
	if got := backupDirOf(p); got != filepath.Join(filepath.Dir(p), "backups") {
		t.Fatalf("备份目录默认在数据目录下: %q", got)
	}
}
