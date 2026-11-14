package runner

import (
	"errors"
	"io"
	"math/rand"
	"net/http"
	"strconv"
	"strings"
	"time"

	"github.com/Maoyangui/m-ui/brand"
	"github.com/Maoyangui/m-ui/logger"
)

// pingURL 测试里要换:真实的下载计数是公开的,测试打一次就多算一台。
var pingURL = brand.PingURL

const (
	pingEvery       = 24 * time.Hour
	pingRetryMin    = time.Hour // 没发出去:1–2 小时后再试(带抖动)
	pingRetrySpread = time.Hour
	pingTriesPerDay = 4 // 一天最多试这么多次(第一次 + 3 次重试),还不行就等明天
)

// pingLoop 统计有多少台面板在运行:每天从 GitHub 下载一次空文件,GitHub 的下载计数一天涨多少就是多少台
// (deploy/donors 由 GitHub Actions 定时汇总,GitHub 常把定时任务推迟几个小时;结果显示在"建议与支持"页)。
// 不带任何标识,也没有多出一家第三方:检查更新本来就连 GitHub。设置里的总开关(supportOnline)可以关。
// 只有真的记上了才写 statsPingAt、等 24 小时;没发出去(连不上、GitHub 出错)1–2 小时后再试,一天最多试 4 次。
func (r *Runner) pingLoop(stop <-chan struct{}) {
	last, _ := strconv.ParseInt(r.setting("statsPingAt"), 10, 64)
	t := time.NewTimer(firstPing(last, time.Now().Unix(), rand.Int63n(int64(6*time.Hour))))
	defer t.Stop()
	fails := 0
	for {
		select {
		case <-t.C:
			var next time.Duration
			next, fails = r.pingOnce(fails)
			t.Reset(next)
		case <-stop:
			return
		}
	}
}

// pingOnce 到点了发一次(总开关关着就不发)。fails 是这一轮已经连着失败的次数;返回下一次等多久和新的失败次数。
func (r *Runner) pingOnce(fails int) (time.Duration, int) {
	if strings.EqualFold(r.setting("supportOnline"), "false") {
		return pingEvery, 0
	}
	if err := ping(pingURL); err != nil {
		next, n := retryPing(fails+1, rand.Int63n(int64(pingRetrySpread)))
		logger.Debugf("运行统计没发出去(第 %d 次),%v 后再试: %v", fails+1, next.Round(time.Minute), err)
		return next, n
	}
	_ = r.setSetting("statsPingAt", strconv.FormatInt(time.Now().Unix(), 10))
	return pingEvery, 0
}

// retryPing 连着失败 fails 次之后等多久:不到 pingTriesPerDay 次就 1–2 小时后再试,试满了等明天(失败次数清零)。
func retryPing(fails int, jitter int64) (time.Duration, int) {
	if fails >= pingTriesPerDay {
		return pingEvery, 0
	}
	return pingRetryMin + time.Duration(jitter), fails
}

// firstPing 启动后多久发第一次。24 小时内发过就等到满 24 小时,重启不多算一台;
// 没发过或早就过了,在 6 小时内随机一个时刻,别让一次批量升级挤在同一分钟。
func firstPing(last, now, jitter int64) time.Duration {
	if left := last + 24*3600 - now; last > 0 && left > 0 && left <= 24*3600 {
		return time.Duration(left) * time.Second
	}
	return time.Duration(jitter)
}

// ping 下载一次那个空文件,返回 GitHub 有没有记上这一次(nil = 记上了)。
// GitHub 在 github.com 发出跳转的那一刻就给下载计数加一,跳转后的下载地址(*.githubusercontent.com)
// 在有的网络里连不上也不影响计数;所以 github.com 给了跳转、或者跟随跳转后拿到 2xx,都算记上了。
// 把这种情况当失败去重试,同一台会被多算几次。连不上 github.com、它回 4xx / 5xx,才算没记上。
func ping(url string) error {
	redirected := false
	c := &http.Client{Timeout: 30 * time.Second, CheckRedirect: func(_ *http.Request, via []*http.Request) error {
		redirected = true
		if len(via) >= 10 {
			return errors.New("stopped after 10 redirects")
		}
		return nil
	}}
	resp, err := c.Get(url)
	if err != nil {
		if redirected {
			return nil
		}
		return err
	}
	defer resp.Body.Close()
	_, _ = io.Copy(io.Discard, io.LimitReader(resp.Body, 1<<10))
	if redirected || resp.StatusCode/100 == 2 {
		return nil
	}
	return errors.New(resp.Status)
}
