package runner

import "testing"

func TestSameRuleOutbounds(t *testing.T) {
	a := []byte(`{"route":{"rules":[{"inbound":["l1"],"action":"route","outbound":"warp"}],"final":"direct"}}`)
	same := []byte(`{"route":{"rules":[{"inbound":["l1"],"action":"route","outbound":"warp"},{"action":"sniff"}],"final":"direct"}}`)
	renamed := []byte(`{"route":{"rules":[{"inbound":["l1"],"action":"route","outbound":"warp2"}],"final":"direct"}}`)
	if !sameRuleOutbounds(a, same) {
		t.Fatal("同一组出站标签应判定相同")
	}
	if sameRuleOutbounds(a, renamed) {
		t.Fatal("上游改名后路由引用变化,不能只热换出站")
	}
	if got := ruleOutboundsOf(a); !got["warp"] || !got["direct"] || len(got) != 2 {
		t.Fatalf("引用集合不对: %v", got)
	}
}
