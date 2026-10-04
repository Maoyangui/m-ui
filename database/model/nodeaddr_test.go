package model

import "testing"

func TestAutoIP(t *testing.T) {
	cases := []struct {
		pub, pub6, fam string
		want, wantFam  string
	}{
		{"203.0.113.5", "2001:db8::5", "", "203.0.113.5", "v4"},    // 双栈默认 IPv4
		{"203.0.113.5", "2001:db8::5", "v6", "2001:db8::5", "v6"},  // 选了 IPv6
		{"203.0.113.5", "", "v6", "203.0.113.5", "v4"},             // 选了 IPv6 但没有,用 IPv4
		{"2001:db8::5", "2001:db8::5", "", "2001:db8::5", "v6"},    // 纯 IPv6:默认也只能用 IPv6
		{"2001:db8::5", "", "", "2001:db8::5", "v6"},               // 老副机在纯 v6 机器上只报了 PublicIP
		{"", "", "v6", "", ""},                                     // 都没有
		{"not-an-ip", "", "", "", ""},                              // 不是 IP 的不认
		{"203.0.113.5", "198.51.100.1", "v6", "203.0.113.5", "v4"}, // PublicIP6 里混进 IPv4 不认
		{" 2001:DB8::5 ", "", "", "2001:db8::5", "v6"},             // 规整写法
	}
	for _, c := range cases {
		got, fam := AutoIP(c.pub, c.pub6, c.fam)
		if got != c.want || fam != c.wantFam {
			t.Errorf("AutoIP(%q, %q, %q) = %q %q,应为 %q %q", c.pub, c.pub6, c.fam, got, fam, c.want, c.wantFam)
		}
	}
	if NormAddrFamily(" V6 ") != "v6" || NormAddrFamily("v4") != "" || NormAddrFamily("x") != "" {
		t.Fatal("NormAddrFamily 规整不对")
	}
}

func TestBareHost(t *testing.T) {
	for in, want := range map[string]string{
		"[2001:db8::1]": "2001:db8::1", " [2001:db8::1] ": "2001:db8::1", "2001:db8::1": "2001:db8::1",
		"203.0.113.1": "203.0.113.1", "[not-ip]": "[not-ip]", "a.example.com": "a.example.com", "": "",
	} {
		if got := BareHost(in); got != want {
			t.Errorf("BareHost(%q) = %q,应为 %q", in, got, want)
		}
	}
}
