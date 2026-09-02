package hop

import (
	"strings"
	"testing"
)

func TestParseRangeAndNFT(t *testing.T) {
	if a, b, err := ParseRange(" 20000 - 30000 "); err != nil || a != 20000 || b != 30000 {
		t.Fatalf("解析失败: %d %d %v", a, b, err)
	}
	for _, bad := range []string{"abc", "30000-20000", "80-100", "20000-20005", "60000-70000"} {
		if _, _, err := ParseRange(bad); err == nil {
			t.Fatalf("%q 应报错", bad)
		}
	}
	if n, _ := Normalize("20000:30000"); n != "20000-30000" {
		t.Fatalf("规范化错误: %s", n)
	}
	rules := []Rule{{20000, 30000, 30443}, {40000, 50000, 8443}}
	if err := Overlaps(rules, []int{30443, 8443, 443}); err != nil {
		t.Fatal(err)
	}
	if err := Overlaps([]Rule{{20000, 30000, 1}, {25000, 26000, 2}}, nil); err == nil {
		t.Fatal("重叠应报错")
	}
	if err := Overlaps([]Rule{{20000, 30000, 30443}}, []int{25000}); err == nil {
		t.Fatal("覆盖其它线路端口应报错")
	}
	s := NFTScript(rules)
	for _, want := range []string{"delete table inet m_ui_hop", "type nat hook prerouting priority dstnat", "udp dport 20000-30000 redirect to :30443", "udp dport 40000-50000 redirect to :8443"} {
		if !strings.Contains(s, want) {
			t.Fatalf("nft 脚本缺少 %q:\n%s", want, s)
		}
	}
	if s := NFTScript(nil); strings.Contains(s, "chain") {
		t.Fatal("无规则时应只删表")
	}
}
