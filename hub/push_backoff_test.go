package hub

import (
	"testing"
	"time"
)

// 同一修订对同一台副机连续推送失败后要退避。副机上一份配置起不来时,每 5 秒硬推一次没有任何意义,
// 只会让它每 5 秒重来一遍"停数据面 → 起失败 → 回滚",这台机器上的用户每 5 秒断一次线。
func TestPushBackoff(t *testing.T) {
	for _, c := range []struct {
		n    int
		want time.Duration
	}{
		{0, 0},
		{1, 5 * time.Second},
		{2, 10 * time.Second},
		{3, 20 * time.Second},
		{6, 160 * time.Second},
		{7, 5 * time.Minute}, // 320s 封顶到 5 分钟
		{50, 5 * time.Minute},
	} {
		if got := pushBackoff(c.n); got != c.want {
			t.Fatalf("pushBackoff(%d) = %v, want %v", c.n, got, c.want)
		}
	}
}
