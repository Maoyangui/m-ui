package sub

import (
	"strings"
	"testing"

	"github.com/Maoyangui/m-ui/database/model"
)

// 用户在某条线路上只拿了 B 服务器:订阅里只出 B 的入口;没收窄的线路照常两台都出。
func TestSubscriptionRespectsLineNodes(t *testing.T) {
	user := model.User{Name: "u", Enabled: true, Credentials: []byte(`{"hysteria2":{"password":"pw"},"anytls":{"password":"pw"}}`)}
	lines := []model.Line{
		{Id: 1, Name: "hk", Protocol: "hysteria2", Port: 30443, Enabled: true},
		{Id: 2, Name: "jp", Protocol: "anytls", Port: 30444, Enabled: true},
	}
	entries := []Entry{
		{Name: "A", Host: "a.example.com", SNI: "a.example.com", Suffix: "-A", NodeId: 1},
		{Name: "B", Host: "b.example.com", SNI: "b.example.com", Suffix: "-B", NodeId: 2},
	}
	opt := Options{Entries: entries, LineNodes: map[uint]map[uint]bool{1: {2: true}}}

	links := GenerateLinksFor(user, lines, entries, opt.LineNodes)
	joined := strings.Join(links, "\n")
	if strings.Contains(joined, "hk-A") || !strings.Contains(joined, "hk-B") {
		t.Fatalf("hk 应只出 B 入口: %s", joined)
	}
	if !strings.Contains(joined, "jp-A") || !strings.Contains(joined, "jp-B") {
		t.Fatalf("jp 没收窄,两台都要出: %s", joined)
	}
	// 三种格式都走同一过滤
	if out := BuildLinkSub(user, lines, opt); strings.Contains(out.Body, "hk-A") {
		t.Fatal("link 订阅漏过滤")
	}
	if out, err := BuildClashSub(user, lines, opt); err != nil || strings.Contains(out.Body, "hk-A") || !strings.Contains(out.Body, "hk-B") {
		t.Fatalf("clash 订阅漏过滤: %v", err)
	}
	if out, err := BuildSingBoxSub(user, lines, opt); err != nil || strings.Contains(out.Body, "hk-A") || !strings.Contains(out.Body, "hk-B") {
		t.Fatalf("sing-box 订阅漏过滤: %v", err)
	}
	// 老调用(不带范围)行为不变
	if all := strings.Join(GenerateLinks(user, lines, entries), "\n"); !strings.Contains(all, "hk-A") || !strings.Contains(all, "hk-B") {
		t.Fatal("不带范围时应两台都出")
	}
}
