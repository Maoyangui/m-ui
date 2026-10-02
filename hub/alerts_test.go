package hub

import (
	"strings"
	"testing"
	"unicode/utf8"

	"github.com/Maoyangui/m-ui/database/model"
)

type sentAlerts struct{ msgs []string }

func (s *sentAlerts) hub() *Hub {
	h := New(Deps{Setting: func(string) string { return "" }, IsNode: func() bool { return false },
		Notify: func(_, text string) { s.msgs = append(s.msgs, text) }})
	return h
}

// 失联告警里的服务器名、副机返回的错误原文要转义并截断:Telegram 按 HTML 解析,一个裸的 < 或几 KB 的报错页
// 就整条被拒,告警被永久吞掉(审计 M060)。
func TestOfflineAlertEscapesAndTruncates(t *testing.T) {
	var sent sentAlerts
	h := sent.hub()
	n := model.Node{Id: 2, Name: "台湾<b>&"}
	h.setStatus(n, false, "x", nil)
	h.mu.Lock()
	h.status[2].failSince -= 120
	h.mu.Unlock()
	h.setStatus(n, false, "HTTP 502: "+strings.Repeat("<div>坏网关</div>", 200), nil)
	if len(sent.msgs) != 1 {
		t.Fatalf("应发一条失联告警,实际 %d 条", len(sent.msgs))
	}
	m := sent.msgs[0]
	if strings.Contains(m, "<b>&") || strings.Contains(m, "<div>") || !strings.Contains(m, "台湾&lt;b&gt;&amp;") {
		t.Fatalf("外部文本没转义: %s", m)
	}
	// 原文 3000 多字;截到 300 字再转义,最坏膨胀 5 倍(&amp;)也远低于 Telegram 的 4096 字上限
	if utf8.RuneCountInString(m) > 1600 {
		t.Fatalf("告警正文没截断(%d 字)", utf8.RuneCountInString(m))
	}
}

// 副机回 HTML 报错页时只留状态码,其余错误体只留前 200 字节。
func TestErrBody(t *testing.T) {
	if got := errBody([]byte("  <html><body>502 Bad Gateway</body></html>"), 502); got != "Bad Gateway" {
		t.Fatalf("HTML 报错页应只留状态码,得到 %q", got)
	}
	if got := errBody([]byte(strings.Repeat("错", 500)), 500); utf8.RuneCountInString(got) != 201 || !strings.HasSuffix(got, "…") {
		t.Fatalf("长错误体应截到 200 字,得到 %d 字", utf8.RuneCountInString(got))
	}
	if got := errBody([]byte("token invalid"), 401); got != "token invalid" {
		t.Fatalf("短文本应原样保留,得到 %q", got)
	}
}

// 在线但配置一直没同步上(推送被拒、版本过低、数据面应用失败):持续 unsyncedAlertAfter 告警一次,
// 同步上了再通知一次;短暂不同步(刚改完配置)不告警(审计 M064)。
func TestUnsyncedAlert(t *testing.T) {
	var sent sentAlerts
	h := sent.hub()
	h.revision = "new"
	n := model.Node{Id: 2, Name: "tw"}
	stale := &Report{Version: "0.6.11", Revision: "old"}
	h.setStatus(n, true, "推送失败: HTTP 409: 副机版本过低", stale)
	h.setStatus(n, true, "推送失败: HTTP 409: 副机版本过低", stale)
	if len(sent.msgs) != 0 {
		t.Fatalf("刚开始不同步不该告警: %v", sent.msgs)
	}
	h.mu.Lock()
	h.status[2].unsyncedSince -= unsyncedAlertAfter + 1
	h.mu.Unlock()
	h.setStatus(n, true, "推送失败: HTTP 409: 副机版本过低", stale)
	h.setStatus(n, true, "推送失败: HTTP 409: 副机版本过低", stale)
	if len(sent.msgs) != 1 || !strings.Contains(sent.msgs[0], "没同步上") || !strings.Contains(sent.msgs[0], "版本过低") {
		t.Fatalf("持续不同步应告警一次并说明原因: %v", sent.msgs)
	}
	h.setStatus(n, true, "", &Report{Version: "0.6.12", Revision: "new"})
	if len(sent.msgs) != 2 || !strings.Contains(sent.msgs[1], "已同步") {
		t.Fatalf("同步上了应通知一次: %v", sent.msgs)
	}
}
