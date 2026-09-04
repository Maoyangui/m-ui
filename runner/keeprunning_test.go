package runner

import (
	"sync/atomic"
	"testing"
	"time"
)

// 常驻循环 panic 后必须被重新拉起,而不是到进程重启前一直缺席。
func TestKeepRunningRestartsAfterPanic(t *testing.T) {
	loopRestartDelay = 20 * time.Millisecond
	defer func() { loopRestartDelay = time.Minute }()

	var runs int32
	stop := make(chan struct{})
	done := make(chan struct{})
	go func() {
		keepRunning("测试", func(s <-chan struct{}) {
			if atomic.AddInt32(&runs, 1) == 1 {
				panic("第一次故意崩")
			}
			<-s // 第二次正常跑到 stop
		}, stop)
		close(done)
	}()

	deadline := time.After(2 * time.Second)
	for atomic.LoadInt32(&runs) < 2 {
		select {
		case <-deadline:
			t.Fatalf("panic 后没有被拉起,运行次数 %d", atomic.LoadInt32(&runs))
		case <-time.After(5 * time.Millisecond):
		}
	}
	close(stop)
	select {
	case <-done:
	case <-time.After(2 * time.Second):
		t.Fatal("stop 后循环没有退出")
	}
}
