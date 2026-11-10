package notify

import (
	"strings"
	"testing"
	"time"
)

// OnceDurable 的去重记录落库、重启后读回;Forget 清掉 key 本身和以 "key:" 开头的记录,不误伤同前缀的别的用户(审计 MB13)。
func TestOnceDurablePersistsAndForget(t *testing.T) {
	store := ""
	open := func() *Notifier {
		n := New(func(string) string { return "" })
		n.Persist(func() string { return store }, func(v string) { store = v })
		return n
	}
	n := open()
	if !n.OnceDurable("quota:alice:100:0", time.Hour) || !n.OnceDurable("quota:alicex:100:0", time.Hour) || !n.OnceDurable("gone", -time.Second) {
		t.Fatal("首次应放行")
	}
	n = open()
	if n.OnceDurable("quota:alice:100:0", time.Hour) {
		t.Fatal("重启后同一个 key 不该再放行")
	}
	if strings.Contains(store, "gone") {
		t.Fatalf("过期记录应在写回时清掉: %s", store)
	}
	n.Forget("quota:alice")
	n = open()
	if !n.OnceDurable("quota:alice:100:0", time.Hour) {
		t.Fatal("Forget 之后应能再放行")
	}
	if n.OnceDurable("quota:alicex:100:0", time.Hour) {
		t.Fatal("Forget(quota:alice) 不该清掉 alicex 的记录")
	}
}

// 证书提醒里的剩余时间:整天数、不到一天、已过期;时间读不出来(零值)也不能算出负数天。
func TestLeftText(t *testing.T) {
	day := 24 * time.Hour
	for left, want := range map[time.Duration]string{
		23*day + time.Hour: "还剩 23 天", 3 * time.Hour: "不到 1 天就到期", -3 * time.Hour: "已过期 1 天", -2 * day: "已过期 2 天",
	} {
		if got := LeftText(left); got != want {
			t.Errorf("LeftText(%v) = %q,应是 %q", left, got, want)
		}
	}
	if got := LeftText(time.Time{}.Sub(time.Now())); !strings.HasPrefix(got, "已过期 ") || strings.Contains(got, "-") {
		t.Errorf("零值时间: %q", got)
	}
}
