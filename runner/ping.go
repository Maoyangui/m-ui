package runner

import (
	"io"
	"math/rand"
	"net/http"
	"strconv"
	"strings"
	"time"

	"github.com/Maoyangui/m-ui/brand"
)

// pingLoop 统计有多少台面板在运行:每天从 GitHub 下载一次空文件,GitHub 的下载计数一天涨多少就是多少台
// (deploy/donors 每小时算一次,结果显示在"建议与支持"页)。不带任何标识,也没有多出一家第三方:
// 检查更新本来就连 GitHub。设置里的总开关(supportOnline)可以关。
func (r *Runner) pingLoop(stop <-chan struct{}) {
	last, _ := strconv.ParseInt(r.setting("statsPingAt"), 10, 64)
	t := time.NewTimer(firstPing(last, time.Now().Unix(), rand.Int63n(int64(6*time.Hour))))
	defer t.Stop()
	for {
		select {
		case <-t.C:
			if !strings.EqualFold(r.setting("supportOnline"), "false") {
				ping(brand.PingURL)
				_ = r.setSetting("statsPingAt", strconv.FormatInt(time.Now().Unix(), 10))
			}
			t.Reset(24 * time.Hour)
		case <-stop:
			return
		}
	}
}

// firstPing 启动后多久发第一次。24 小时内发过就等到满 24 小时,重启不多算一台;
// 没发过或早就过了,在 6 小时内随机一个时刻,别让一次批量升级挤在同一分钟。
func firstPing(last, now, jitter int64) time.Duration {
	if left := last + 24*3600 - now; last > 0 && left > 0 && left <= 24*3600 {
		return time.Duration(left) * time.Second
	}
	return time.Duration(jitter)
}

func ping(url string) {
	c := &http.Client{Timeout: 30 * time.Second}
	resp, err := c.Get(url)
	if err != nil {
		return
	}
	defer resp.Body.Close()
	_, _ = io.Copy(io.Discard, io.LimitReader(resp.Body, 1<<10))
}
