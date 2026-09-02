package totp

import (
	"strings"
	"testing"
	"time"
)

// RFC 6238 附录 B 的 SHA-1 测试向量(密钥 "12345678901234567890"),取 8 位码的后 6 位。
const rfcSecret = "GEZDGNBVGY3TQOJQGEZDGNBVGY3TQOJQ"

func TestRFCVectors(t *testing.T) {
	cases := map[int64]string{59: "287082", 1111111109: "081804", 1234567890: "005924", 2000000000: "279037"}
	for ts, want := range cases {
		got, err := Code(rfcSecret, time.Unix(ts, 0))
		if err != nil || got != want {
			t.Fatalf("T=%d: got %q err=%v, want %q", ts, got, err, want)
		}
	}
}

func TestVerifyWindowAndReplay(t *testing.T) {
	now := time.Unix(1234567890, 0)
	prev, _ := Code(rfcSecret, now.Add(-30*time.Second))
	next, _ := Code(rfcSecret, now.Add(30*time.Second))
	far, _ := Code(rfcSecret, now.Add(90*time.Second))
	if ok, step := Verify(rfcSecret, "005924", now); !ok || step != now.Unix()/Period {
		t.Fatal("current code should verify")
	}
	if ok, _ := Verify(rfcSecret, prev, now); !ok {
		t.Fatal("previous step should be accepted")
	}
	if ok, _ := Verify(rfcSecret, next, now); !ok {
		t.Fatal("next step should be accepted")
	}
	if ok, _ := Verify(rfcSecret, far, now); ok {
		t.Fatal("code two steps ahead must be rejected")
	}
	if ok, _ := Verify(rfcSecret, "00592", now); ok {
		t.Fatal("5-digit code must be rejected")
	}
	if ok, _ := Verify(rfcSecret, "005 924", now); !ok {
		t.Fatal("spaces inside the code should be tolerated")
	}
}

func TestNormalizeAndGenerate(t *testing.T) {
	if s, err := Normalize(" gezd gnbv-gy3t qojq gezd gnbv gy3t qojq= "); err != nil || s != rfcSecret {
		t.Fatalf("normalize: %q %v", s, err)
	}
	if _, err := Normalize("ABC"); err == nil {
		t.Fatal("short secret must fail")
	}
	if _, err := Normalize("ABCDEFGHIJKLMNOP1890"); err == nil {
		t.Fatal("invalid base32 chars must fail")
	}
	s := GenerateSecret()
	if len(s) != 32 || strings.ContainsAny(s, "=01 ") {
		t.Fatalf("unexpected secret %q", s)
	}
	if _, err := Code(s, time.Now()); err != nil {
		t.Fatal(err)
	}
	u := URL("m-ui", "admin@example.com", s)
	if !strings.HasPrefix(u, "otpauth://totp/m-ui:admin@example.com?") || !strings.Contains(u, "secret="+s) || !strings.Contains(u, "issuer=m-ui") {
		t.Fatalf("bad url %s", u)
	}
}
