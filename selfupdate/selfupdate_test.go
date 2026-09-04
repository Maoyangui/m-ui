package selfupdate

import "testing"

// 版本比较要保守:解析不出来就当没有更新,免得面板天天挂一个假的更新提示。
func TestNewer(t *testing.T) {
	yes := [][2]string{{"0.3.7", "0.3.6"}, {"0.4.0", "0.3.9"}, {"1.0.0", "0.9.9"}, {"0.10.0", "0.9.0"}}
	no := [][2]string{
		{"0.3.6", "0.3.6"}, {"0.3.5", "0.3.6"}, {"0.3.6", "0.4.0"},
		{"", "0.3.6"}, {"dev", "0.3.6"}, {"0.3.6", "dev"}, {"v0.3.7", "0.3.6"}, // 带 v 的没去前缀 → 解析失败 → 不提示
	}
	for _, c := range yes {
		if !newer(c[0], c[1]) {
			t.Errorf("%s 应比 %s 新", c[0], c[1])
		}
	}
	for _, c := range no {
		if newer(c[0], c[1]) {
			t.Errorf("%s 不应判为比 %s 新", c[0], c[1])
		}
	}
}

// 压缩包里没有 m-ui 时要报错而不是 panic。
func TestExtractBinaryRejectsGarbage(t *testing.T) {
	if _, err := extractBinary([]byte("not a tarball")); err == nil {
		t.Fatal("应报错")
	}
}
