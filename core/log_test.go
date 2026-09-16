package core

import (
	"testing"
	"time"

	"github.com/sagernet/sing-box/log"
)

// 数据面日志入队即返:一次塞几万条也不能把调用方卡住(内核是在处理连接的 goroutine 里调日志的),
// 队列满了丢,丢了要计数。
func TestEnqueueLogNeverBlocks(t *testing.T) {
	before := logDropped.Load()
	start := time.Now()
	for i := 0; i < 50000; i++ {
		enqueueLog(log.LevelInfo, "t", "burst")
	}
	if d := time.Since(start); d > 2*time.Second {
		t.Fatalf("5 万条入队用了 %v,说明在阻塞", d)
	}
	// 队列只有 8192 格,写盘那头再快也来不及全吞下,必然有丢弃
	deadline := time.Now().Add(3 * time.Second)
	for logDropped.Load() == before && time.Now().Before(deadline) {
		time.Sleep(20 * time.Millisecond)
	}
	if logDropped.Load() == before {
		t.Fatal("队列满了应记丢弃数")
	}
	// 排空后 PlatformWriter 也走同一条队列
	time.Sleep(200 * time.Millisecond)
	PlatformWriter{}.WriteMessage(log.LevelWarn, "via platform writer")
}
