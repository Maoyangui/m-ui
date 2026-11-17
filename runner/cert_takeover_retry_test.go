package runner

import (
	"errors"
	"testing"
	"time"
)

// 接任后重签订阅 / 面板证书:签不下来隔一会儿再试(刚改完解析、TXT 记录还没生效,或者正好有别的签发在跑),不是失败一次就停;
// 证书已经能用了、本机不再是主机了、试满了才停。
func TestRetryIssueForTakeover(t *testing.T) {
	delays := []time.Duration{time.Minute, 2 * time.Minute, 5 * time.Minute}
	never, always := func() bool { return false }, func() bool { return true }
	type run struct {
		fails    int // 前几次失败
		usableAt int // 第几次失败之后证书已经能用了(0 = 一直不能用)
		stopAt   int // 第几次失败之后本机不再是主机(0 = 一直是)
	}
	play := func(c run) (tries int, slept []time.Duration, err error) {
		n := 0
		usable := func() bool { return c.usableAt > 0 && n >= c.usableAt }
		stop := func() bool { return c.stopAt > 0 && n >= c.stopAt }
		tries, err = retryIssue(func() error {
			n++
			if n <= c.fails {
				return errors.New("没等到 TXT 记录生效")
			}
			return nil
		}, usable, stop, delays, func(d time.Duration) { slept = append(slept, d) }, nil)
		return
	}
	// 一次就成:不等
	if tries, slept, err := play(run{}); tries != 1 || len(slept) != 0 || err != nil {
		t.Fatalf("一次就成: %d 次 %v %v", tries, slept, err)
	}
	// 头两次不成、第三次成:按 1 分钟、2 分钟各等一次
	if tries, slept, err := play(run{fails: 2}); tries != 3 || err != nil || len(slept) != 2 || slept[0] != time.Minute || slept[1] != 2*time.Minute {
		t.Fatalf("第三次成: %d 次 %v %v", tries, slept, err)
	}
	// 一直不成:试 1 + 3 次,等满三段,返回最后的错
	if tries, slept, err := play(run{fails: 99}); tries != 4 || err == nil || len(slept) != 3 || slept[2] != 5*time.Minute {
		t.Fatalf("一直不成: %d 次 %v %v", tries, slept, err)
	}
	// 失败了,可证书已经能用(手动签好了、或者这次其实装上了):不再试,也不算失败
	if tries, slept, err := play(run{fails: 99, usableAt: 1}); tries != 1 || err != nil || len(slept) != 0 {
		t.Fatalf("证书已能用: %d 次 %v %v", tries, slept, err)
	}
	// 本机不再是主机了(原主机回来、又让位了):停,不再替别人的域名签
	if tries, slept, err := play(run{fails: 99, stopAt: 2}); tries != 2 || err == nil || len(slept) != 1 {
		t.Fatalf("不再是主机: %d 次 %v %v", tries, slept, err)
	}
	// 「正好有别的签发在跑」也是一种失败:同样等一等再来
	busy := 0
	tries, err := retryIssue(func() error {
		busy++
		if busy == 1 {
			return errIssueBusy
		}
		return nil
	}, never, never, delays, func(time.Duration) {}, nil)
	if tries != 2 || err != nil {
		t.Fatalf("撞上别的签发: %d 次 %v", tries, err)
	}
	// note 每次决定再等时叫一次,带着第几次、原因、等多久
	var notes []int
	_, _ = retryIssue(func() error { return errors.New("x") }, never, never, delays, func(time.Duration) {}, func(n int, err error, wait time.Duration) {
		if err == nil || wait != delays[n-1] {
			t.Fatalf("note 的参数不对: %d %v %v", n, err, wait)
		}
		notes = append(notes, n)
	})
	if len(notes) != 3 || notes[0] != 1 || notes[2] != 3 {
		t.Fatalf("note 应当叫三次: %v", notes)
	}
	_ = always
	// 默认的等待表:从 1 分钟起、越来越长、总共两个半小时左右
	var total time.Duration
	for i, d := range takeoverIssueDelays {
		if i > 0 && d <= takeoverIssueDelays[i-1] {
			t.Fatalf("等待应当越来越长: %v", takeoverIssueDelays)
		}
		total += d
	}
	if takeoverIssueDelays[0] != time.Minute || total < 2*time.Hour || total > 3*time.Hour {
		t.Fatalf("等待表不对: %v(合计 %v)", takeoverIssueDelays, total)
	}
}
