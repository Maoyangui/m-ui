package creds

import (
	"encoding/base64"
	"strings"
	"testing"
)

// 口令必须是定长、字母数字,且不带取模偏置(直接对 62 取模会偏向字母表开头 8 个字符)。
func TestPasswordUniform(t *testing.T) {
	if got := len(Password(16)); got != 16 {
		t.Fatalf("长度应为 16,得 %d", got)
	}
	counts := map[byte]int{}
	const n = 20000
	for i := 0; i < n/8; i++ {
		p := Password(8)
		if len(p) != 8 {
			t.Fatalf("长度不稳定: %q", p)
		}
		for j := 0; j < len(p); j++ {
			if !strings.ContainsRune(alphabet, rune(p[j])) {
				t.Fatalf("出现字母表外的字符: %q", p[j])
			}
			counts[p[j]]++
		}
	}
	// 均匀分布下每个字符期望 n/62 次;偏置版本里前 8 个字符会高出约 25%
	exp := float64(n) / float64(len(alphabet))
	var head, tail float64
	for i := 0; i < 8; i++ {
		head += float64(counts[alphabet[i]])
	}
	for i := len(alphabet) - 8; i < len(alphabet); i++ {
		tail += float64(counts[alphabet[i]])
	}
	if head/tail > 1.15 || head/tail < 0.85 {
		t.Fatalf("字符分布不均:前八 %.0f 后八 %.0f(期望各约 %.0f)", head, tail, exp*8)
	}
}

// 审计 E3:公钥由私钥算出,和生成的那一对一致;几种 base64 写法都认,统一成 RawURL;长度不对、不是 base64 都拒。
func TestRealityPublicKey(t *testing.T) {
	priv, pub, err := RealityKeypair()
	if err != nil {
		t.Fatal(err)
	}
	n, p, err := RealityPublicKey(priv)
	if err != nil || n != priv || p != pub {
		t.Fatalf("应算出同一把公钥: %v %q %q", err, n, p)
	}
	raw, _ := base64.RawURLEncoding.DecodeString(priv)
	for _, alt := range []string{base64.URLEncoding.EncodeToString(raw), base64.StdEncoding.EncodeToString(raw), "  " + priv + "\n"} {
		n, p, err := RealityPublicKey(alt)
		if err != nil || n != priv || p != pub {
			t.Fatalf("%q 应认得出并统一成 RawURL: %v %q %q", alt, err, n, p)
		}
	}
	for _, bad := range []string{"", "not base64 !!", base64.RawURLEncoding.EncodeToString([]byte("short"))} {
		if _, _, err := RealityPublicKey(bad); err == nil {
			t.Fatalf("%q 应拒绝", bad)
		}
	}
}
