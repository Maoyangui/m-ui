package model

import (
	"encoding/json"
	"testing"
)

func TestFamilyIPs(t *testing.T) {
	for _, c := range []struct{ addr, pub, pub6, v4, v6 string }{
		{"", "1.2.3.4", "2001:db8::1", "1.2.3.4", "2001:db8::1"},
		{"", "1.2.3.4", "", "1.2.3.4", ""},
		{"", "2001:db8::1", "", "", "2001:db8::1"}, // 老版本副机在纯 IPv6 机器上报的是 v6
		{"5.6.7.8", "1.2.3.4", "2001:db8::1", "5.6.7.8", "2001:db8::1"},
		{"[2001:db8::9]", "1.2.3.4", "2001:db8::1", "1.2.3.4", "2001:db8::9"},
		{"cdn.example.com", "1.2.3.4", "2001:db8::1", "1.2.3.4", "2001:db8::1"},
	} {
		v4, v6 := FamilyIPs(c.addr, c.pub, c.pub6)
		if v4 != c.v4 || v6 != c.v6 {
			t.Errorf("FamilyIPs(%q,%q,%q) = %q %q", c.addr, c.pub, c.pub6, v4, v6)
		}
	}
}

func TestNormNodeFamily(t *testing.T) {
	got, err := NormNodeFamily(json.RawMessage(`{"1":"v6","2":"","3":"v4"}`))
	if err != nil {
		t.Fatal(err)
	}
	m := ParseNodeFamily(got)
	if len(m) != 2 || m[1] != "v6" || m[3] != "v4" {
		t.Fatalf("实际 %s", got)
	}
	for _, s := range []string{`{}`, `null`, `{"1":""}`} {
		if got, err := NormNodeFamily(json.RawMessage(s)); err != nil || got != nil {
			t.Errorf("%s 应规整成空: %s %v", s, got, err)
		}
	}
	for _, s := range []string{`{"1":"v5"}`, `{"a":"v4"}`, `[1]`} {
		if _, err := NormNodeFamily(json.RawMessage(s)); err == nil {
			t.Errorf("%s 应报错", s)
		}
	}
}

// 默认端口不写,但只有和协议对得上才能省:http 跑在 443 上省掉的话浏览器会去连 80。
func TestURLHost(t *testing.T) {
	for _, c := range []struct {
		scheme, host string
		port         int
		want         string
	}{
		{"https", "aa.com", 443, "aa.com"},
		{"http", "aa.com", 80, "aa.com"},
		{"http", "aa.com", 443, "aa.com:443"},
		{"https", "aa.com", 80, "aa.com:80"},
		{"https", "aa.com", 2056, "aa.com:2056"},
		{"https", "2001:db8::1", 443, "[2001:db8::1]"},
		{"http", "2001:db8::1", 2053, "[2001:db8::1]:2053"},
	} {
		if got := URLHost(c.scheme, c.host, c.port); got != c.want {
			t.Errorf("URLHost(%s,%s,%d) = %s,应为 %s", c.scheme, c.host, c.port, got, c.want)
		}
	}
}

func TestIPKey(t *testing.T) {
	if got := IPKey("1.2.3.4"); got != "1.2.3.4" {
		t.Fatal(got)
	}
	if got := IPKey("::ffff:1.2.3.4"); got != "1.2.3.4" {
		t.Fatalf("映射地址算 IPv4: %s", got)
	}
	if IPKey("2001:db8:1:2:aaaa::1") != IPKey("2001:db8:1:2:bbbb::2") {
		t.Fatal("同一个 /64 应该是同一个键")
	}
	if IPKey("2001:db8:1:2::1") == IPKey("2001:db8:1:3::1") {
		t.Fatal("不同 /64 不该相同")
	}
	if got := DeviceCount([]string{"1.2.3.4", "2001:db8:1:2::1", "2001:db8:1:2::2", "2001:db8:1:3::1"}); got != 3 {
		t.Fatalf("实际 %d", got)
	}
}
