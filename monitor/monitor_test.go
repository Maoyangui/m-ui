package monitor

import (
	"path/filepath"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/fangjunsheng555/m-ui/database"
	"github.com/fangjunsheng555/m-ui/database/model"
	"github.com/fangjunsheng555/m-ui/notify"
)

type fakeSink struct {
	mu   sync.Mutex
	msgs []string
}

func (f *fakeSink) send(_, _, text string) error {
	f.mu.Lock()
	f.msgs = append(f.msgs, text)
	f.mu.Unlock()
	return nil
}
func (f *fakeSink) wait() []string {
	time.Sleep(50 * time.Millisecond)
	f.mu.Lock()
	defer f.mu.Unlock()
	return append([]string(nil), f.msgs...)
}

func setup(t *testing.T, settings map[string]string) (*Monitor, *fakeSink, func(string) string) {
	t.Helper()
	db, err := database.Open(filepath.Join(t.TempDir(), "m.db"))
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { database.Close(db) })
	base := map[string]string{"tgEnabled": "true", "tgToken": "x", "tgChatId": "1"}
	for k, v := range settings {
		base[k] = v
	}
	setting := func(k string) string { return base[k] }
	sink := &fakeSink{}
	n := notify.New(setting)
	n.SetSender(sink.send)
	m := New(Deps{DB: db, Setting: setting, CoreRunning: func() bool { return true }, Notify: n})
	return m, sink, setting
}

func TestUpstreamDownThenRecoverAlerts(t *testing.T) {
	m, sink, _ := setup(t, map[string]string{"upstreamCheckFailThreshold": "2"})
	m.d.DB.Create(&model.Upstream{Name: "hk", Type: "socks", Options: []byte(`{"server":"127.0.0.1","server_port":1}`)})
	state := false
	m.d.Check = func(model.Upstream) (bool, int, string, string) {
		if state {
			return true, 42, "urltest", ""
		}
		return false, 0, "urltest", "timeout"
	}
	// 第一次失败:未达阈值,不告警
	if ch := m.RunUpstreamCheck(); len(ch) != 0 || len(sink.wait()) != 0 {
		t.Fatal("首次失败不应告警")
	}
	// 第二次失败:达到阈值,告警一次
	if ch := m.RunUpstreamCheck(); len(ch) != 1 {
		t.Fatal("达到阈值应记为变化")
	}
	if msgs := sink.wait(); len(msgs) != 1 || !strings.Contains(msgs[0], "上游故障") || !strings.Contains(msgs[0], "hk") {
		t.Fatalf("应发出故障告警: %v", msgs)
	}
	// 持续失败:不重复告警
	m.RunUpstreamCheck()
	if len(sink.wait()) != 1 {
		t.Fatal("持续故障不应重复告警")
	}
	// 恢复:恢复告警
	state = true
	if ch := m.RunUpstreamCheck(); len(ch) != 1 {
		t.Fatal("恢复应记为变化")
	}
	msgs := sink.wait()
	if len(msgs) != 2 || !strings.Contains(msgs[1], "上游恢复") {
		t.Fatalf("应发出恢复告警: %v", msgs)
	}
	r := m.Results()
	if len(r) != 1 || !r[0].OK || r[0].DelayMs != 42 || r[0].Fails != 0 {
		t.Fatalf("结果不符: %+v", r)
	}
}

func TestUserWarningsOncePerDay(t *testing.T) {
	m, sink, _ := setup(t, map[string]string{"tgExpiringDays": "3", "tgQuotaPercent": "80"})
	now := time.Now().Unix()
	m.d.DB.Create(&model.User{Name: "soon", Enabled: true, Expiry: now + 2*86400})
	m.d.DB.Create(&model.User{Name: "later", Enabled: true, Expiry: now + 30*86400})
	m.d.DB.Create(&model.User{Name: "heavy", Enabled: true, Volume: 100, Up: 50, Down: 35})
	m.d.DB.Create(&model.User{Name: "light", Enabled: true, Volume: 100, Up: 10})
	m.CheckUsers()
	m.CheckUsers() // 同日再跑不应重复
	msgs := sink.wait()
	if len(msgs) != 2 {
		t.Fatalf("应恰好 2 条(到期 + 用量): %v", msgs)
	}
	joined := strings.Join(msgs, "|")
	if !strings.Contains(joined, "soon") || !strings.Contains(joined, "heavy") || strings.Contains(joined, "later") || strings.Contains(joined, "light") {
		t.Fatalf("预警对象不符: %v", msgs)
	}
}

func TestCoreWatchdogTransitions(t *testing.T) {
	m, sink, _ := setup(t, nil)
	running := true
	m.d.CoreRunning = func() bool { return running }
	m.tickCore()
	running = false
	m.tickCore()
	m.tickCore()
	running = true
	m.tickCore()
	msgs := sink.wait()
	joined := strings.Join(msgs, "|") // 两条异步发送,顺序不保证
	if len(msgs) != 2 || !strings.Contains(joined, "停止") || !strings.Contains(joined, "恢复") {
		t.Fatalf("看门狗应各告警一次: %v", msgs)
	}
}

func TestDailyReport(t *testing.T) {
	m, _, _ := setup(t, nil)
	now := time.Now().Unix()
	m.d.DB.Create(&model.User{Name: "a", Enabled: true, Expiry: now + 86400})
	m.d.DB.Create(&model.Stats{DateTime: now - 100, Resource: "user", Tag: "a", Direction: true, Traffic: 1 << 30})
	m.d.DB.Create(&model.Stats{DateTime: now - 100, Resource: "user", Tag: "a", Direction: false, Traffic: 3 << 30})
	txt := m.DailyReport()
	for _, want := range []string{"m-ui 日报", "1 启用 / 1 总计", "7 天内到期 1", "↑ 1.0 GB", "↓ 3.0 GB", "1. a"} {
		if !strings.Contains(txt, want) {
			t.Errorf("日报缺少 %q:\n%s", want, txt)
		}
	}
}

func TestNotifierDisabledSendsNothing(t *testing.T) {
	m, sink, _ := setup(t, map[string]string{"tgEnabled": "false"})
	m.d.Notify.Event("", "x")
	if len(sink.wait()) != 0 {
		t.Fatal("未开启时不应发送")
	}
}
