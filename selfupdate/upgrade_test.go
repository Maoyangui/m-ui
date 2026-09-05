package selfupdate

import (
	"bytes"
	"errors"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"
)

// 换程序:旧的挪到 .prev,新的落到正式位置;新程序缺失时旧程序要回到原位。
func TestSwap(t *testing.T) {
	dir := t.TempDir()
	bin, next, prev := filepath.Join(dir, "m-ui"), filepath.Join(dir, "m-ui.new"), filepath.Join(dir, "m-ui.prev")
	os.WriteFile(bin, []byte("old"), 0o755)
	os.WriteFile(next, []byte("new"), 0o755)
	if err := Swap(bin, next, prev); err != nil {
		t.Fatal(err)
	}
	if b, _ := os.ReadFile(bin); string(b) != "new" {
		t.Fatalf("正式程序应是新的,实际 %q", b)
	}
	if b, _ := os.ReadFile(prev); string(b) != "old" {
		t.Fatalf(".prev 应是旧的,实际 %q", b)
	}
	// 第二次:没有 .new,替换失败,正式程序必须回到原位
	if err := Swap(bin, next, prev); err == nil {
		t.Fatal("缺少新程序应报错")
	}
	if b, _ := os.ReadFile(bin); string(b) != "new" {
		t.Fatalf("失败后正式程序应原样,实际 %q", b)
	}
	// 首次安装:没有旧程序也能落新的
	os.Remove(bin)
	os.Remove(prev)
	os.WriteFile(next, []byte("fresh"), 0o755)
	if err := Swap(bin, next, prev); err != nil {
		t.Fatal(err)
	}
	if b, _ := os.ReadFile(bin); string(b) != "fresh" {
		t.Fatalf("首次安装应落新程序,实际 %q", b)
	}
}

func TestPrune(t *testing.T) {
	dir := t.TempDir()
	base := time.Now().Add(-time.Hour)
	for i, n := range []string{"pre-upgrade-a.zip", "pre-upgrade-b.zip", "pre-upgrade-c.zip", "pre-upgrade-d.zip", "other.zip"} {
		p := filepath.Join(dir, n)
		os.WriteFile(p, []byte("x"), 0o600)
		os.Chtimes(p, base.Add(time.Duration(i)*time.Minute), base.Add(time.Duration(i)*time.Minute))
	}
	Prune(dir, BackupPrefix, 2)
	ents, _ := os.ReadDir(dir)
	var left []string
	for _, e := range ents {
		left = append(left, e.Name())
	}
	got := strings.Join(left, ",")
	if got != "other.zip,pre-upgrade-c.zip,pre-upgrade-d.zip" {
		t.Fatalf("应只留最新两份和无关文件,实际 %s", got)
	}
}

func TestLocalURL(t *testing.T) {
	if u := LocalURL(false, 2053, "/app/"); u != "http://127.0.0.1:2053/app/" {
		t.Fatal(u)
	}
	if u := LocalURL(true, 3053, "ad"); u != "https://127.0.0.1:3053/ad/" {
		t.Fatal(u)
	}
}

func TestPlanArgsRoundTrip(t *testing.T) {
	p := Plan{Bin: "/b", Prev: "/b.prev", Failed: "/b.failed", URL: "http://127.0.0.1:1/app/", Service: "m-ui", OldPID: 42, DBPath: "/d", Backup: "/k.zip", StatusPath: "/s.json", From: "v1", To: "v2", Timeout: 90 * time.Second}
	got := ParseArgs(p.Args())
	if got != p {
		t.Fatalf("参数来回后不一致:\n%+v\n%+v", p, got)
	}
}

// 一个假的系统:sysctl 记录调用;健康与否由测试决定;时钟只随 sleep 前进,不用真等 90 秒。
type fakeSys struct {
	t       *testing.T
	calls   []string
	clock   time.Time
	healthy func() bool
	alive   int // oldAlive 返回 true 的次数
	restore func(bin, db, backup string) error
}

func (f *fakeSys) install(w *watcher) {
	w.sysctl = func(args ...string) error { f.calls = append(f.calls, strings.Join(args, " ")); return nil }
	w.oldAlive = func() bool {
		if f.alive > 0 {
			f.alive--
			return true
		}
		return false
	}
	w.healthy = func() bool { return f.healthy() }
	w.sleep = func(d time.Duration) { f.clock = f.clock.Add(d) }
	w.now = func() time.Time { return f.clock }
	w.restore = func(bin, db, backup string) error {
		f.calls = append(f.calls, "restore "+backup)
		if f.restore != nil {
			return f.restore(bin, db, backup)
		}
		return nil
	}
}

func setup(t *testing.T) (Plan, string) {
	dir := t.TempDir()
	bin := filepath.Join(dir, "m-ui")
	os.WriteFile(bin, []byte("new"), 0o755)
	os.WriteFile(PrevPath(bin), []byte("old"), 0o755)
	os.WriteFile(filepath.Join(dir, "pre.zip"), []byte("zip"), 0o600)
	return Plan{Bin: bin, From: "v1", To: "v2", URL: "http://127.0.0.1:1/app/", Service: "m-ui", OldPID: 7,
		DBPath: filepath.Join(dir, "m-ui.db"), Backup: filepath.Join(dir, "pre.zip"), StatusPath: filepath.Join(dir, "upgrade-status.json"), Timeout: 90 * time.Second}, dir
}

func read(p string) string { b, _ := os.ReadFile(p); return string(b) }

// 新版本健康:删掉 .prev,记录成功,不碰 systemctl。
func TestWatchHealthy(t *testing.T) {
	p, _ := setup(t)
	var out bytes.Buffer
	w := newWatcher(p, &out)
	f := &fakeSys{t: t, alive: 2, healthy: func() bool { return true }}
	f.install(w)
	st, err := w.run()
	if err != nil || !st.OK || !st.Healthy || st.RolledBack {
		t.Fatalf("应成功:%+v %v", st, err)
	}
	if _, err := os.Stat(PrevPath(p.Bin)); !os.IsNotExist(err) {
		t.Fatal("成功后 .prev 应删除")
	}
	if len(f.calls) != 0 {
		t.Fatalf("成功不该动 systemctl:%v", f.calls)
	}
	if got := ReadStatus(p.StatusPath); got == nil || !got.OK || got.To != "v2" {
		t.Fatalf("状态文件不对:%+v", got)
	}
	if read(p.Bin) != "new" {
		t.Fatal("正式程序应保持新版本")
	}
}

// 新版本起不来:停服务、坏程序改名 .failed、旧程序换回、起服务;旧程序健康就到此为止。
func TestWatchRollbackBinary(t *testing.T) {
	p, _ := setup(t)
	w := newWatcher(p, nil)
	f := &fakeSys{t: t, healthy: func() bool { return read(p.Bin) == "old" }}
	f.install(w)
	st, err := w.run()
	if !errors.Is(err, ErrRolledBack) {
		t.Fatalf("应返回 ErrRolledBack,实际 %v", err)
	}
	if !st.RolledBack || st.DBRestored || !st.Healthy || st.OK {
		t.Fatalf("状态不对:%+v", st)
	}
	if read(p.Bin) != "old" || read(FailedPath(p.Bin)) != "new" {
		t.Fatalf("程序文件不对:bin=%q failed=%q", read(p.Bin), read(FailedPath(p.Bin)))
	}
	if strings.Join(f.calls, ";") != "stop m-ui;start m-ui" {
		t.Fatalf("systemctl 调用不对:%v", f.calls)
	}
	if f.clock.Sub(time.Time{}) < 90*time.Second {
		t.Fatal("应等满超时才回滚")
	}
	if got := ReadStatus(p.StatusPath); got == nil || !got.RolledBack || got.Failed != FailedPath(p.Bin) {
		t.Fatalf("状态文件不对:%+v", got)
	}
	if !strings.Contains(read(filepath.Join(filepath.Dir(p.StatusPath), "upgrade.log")), "rolledBack=true") {
		t.Fatal("upgrade.log 应记录回滚")
	}
}

// 旧程序对新库也起不来:还原升级前备份后再起。
func TestWatchRollbackWithDB(t *testing.T) {
	p, _ := setup(t)
	w := newWatcher(p, nil)
	restored := false
	f := &fakeSys{t: t, healthy: func() bool { return restored }, restore: func(bin, db, backup string) error { restored = true; return nil }}
	f.install(w)
	st, err := w.run()
	if !errors.Is(err, ErrRolledBack) || !st.RolledBack || !st.DBRestored || !st.Healthy {
		t.Fatalf("应回滚并还原库:%+v %v", st, err)
	}
	if strings.Join(f.calls, ";") != "stop m-ui;start m-ui;stop m-ui;restore "+p.Backup+";start m-ui" {
		t.Fatalf("调用顺序不对:%v", f.calls)
	}
}

// 没有备份、旧程序也救不回来:如实报告,不假装成功。
func TestWatchRollbackFails(t *testing.T) {
	p, _ := setup(t)
	p.Backup = ""
	w := newWatcher(p, nil)
	f := &fakeSys{t: t, healthy: func() bool { return false }}
	f.install(w)
	st, err := w.run()
	if err == nil || errors.Is(err, ErrRolledBack) || st.Healthy || !st.RolledBack {
		t.Fatalf("应报告回滚后仍不健康:%+v %v", st, err)
	}
}

// 旧进程还没退出时,即使首页 200 也不能当成新版本健康。
func TestWatchWaitsForOldProcess(t *testing.T) {
	p, _ := setup(t)
	w := newWatcher(p, nil)
	checks := 0
	f := &fakeSys{t: t, alive: 3}
	f.install(w)
	w.healthy = func() bool { checks++; return true }
	if _, err := w.run(); err != nil {
		t.Fatal(err)
	}
	if f.clock.Sub(time.Time{}) < 3*time.Second {
		t.Fatal("应先等旧进程退出")
	}
	if checks != 1 {
		t.Fatalf("旧进程退出前不该判健康,判了 %d 次", checks)
	}
	// 旧进程 60 秒都不退:不删 .prev,不报成功
	p2, _ := setup(t)
	w2 := newWatcher(p2, nil)
	f2 := &fakeSys{t: t, alive: 1000, healthy: func() bool { return true }}
	f2.install(w2)
	st, err := w2.run()
	if err == nil || st.OK {
		t.Fatalf("服务没重启不该算成功:%+v %v", st, err)
	}
	if _, err := os.Stat(PrevPath(p2.Bin)); err != nil {
		t.Fatal("服务没重启时 .prev 必须保留")
	}
}

func TestStatusFile(t *testing.T) {
	dir := t.TempDir()
	p := StatusPath(dir)
	if ReadStatus(p) != nil {
		t.Fatal("没有文件应返回 nil")
	}
	writeStatus(p, Status{At: 1, From: "v1", To: "v2", RolledBack: true, Message: "x"})
	if st := ReadStatus(p); st == nil || !st.RolledBack || st.From != "v1" {
		t.Fatalf("读回不对:%+v", st)
	}
	ClearStatus(p)
	if ReadStatus(p) != nil {
		t.Fatal("清除后应为 nil")
	}
}
