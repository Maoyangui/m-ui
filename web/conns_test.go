package web

import "testing"

func TestInboundConnRegexMatchesRingBufferFormat(t *testing.T) {
	cases := map[string][3]string{
		"2026/09/02 12:51:53 INFO - inbound/anytls[对照-anytls-443]inbound connection from 20.78.1.208:27872": {"anytls", "对照-anytls-443", "20.78.1.208"},
		"2026/09/02 12:51:51 INFO - inbound/hysteria2[测试-hy2] inbound connection from [2001:db8::1]:46502":     {"hysteria2", "测试-hy2", "[2001:db8::1]"},
	}
	for line, want := range cases {
		m := reInboundConn.FindStringSubmatch(line)
		if m == nil || m[1] != want[0] || m[2] != want[1] || m[3] != want[2] {
			t.Fatalf("行 %q 解析结果 %v,期望 %v", line, m, want)
		}
	}
	if reInboundConn.MatchString("inbound/anytls[x][tester] inbound connection to www.gstatic.com:80") {
		t.Fatal("出向连接行不应匹配")
	}
	if ts := reLogTime.FindStringSubmatch("2026/09/02 12:51:53 INFO - x"); ts == nil || ts[1] != "2026/09/02 12:51:53" {
		t.Fatal("时间戳解析失败")
	}
}
