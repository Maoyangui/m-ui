package runner

import (
	"testing"
	"time"
)

func TestFirstPing(t *testing.T) {
	const now = int64(1_800_000_000)
	jitter := int64(time.Hour)
	for _, c := range []struct {
		name string
		last int64
		want time.Duration
	}{
		{"从没发过", 0, time.Hour},
		{"一小时前发过,等满 24 小时", now - 3600, 23 * time.Hour},
		{"刚好 24 小时前", now - 24*3600, time.Hour},
		{"早就过了", now - 72*3600, time.Hour},
		{"时间在未来(改过时钟)", now + 3600, time.Hour},
	} {
		if got := firstPing(c.last, now, jitter); got != c.want {
			t.Errorf("%s: 实际 %v,应为 %v", c.name, got, c.want)
		}
	}
}
