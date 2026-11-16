package safe

import (
	"sync"
	"testing"
)

// panic 被兜住:Do 返回 false、调用方接着往下走;里面的 defer(解锁、wg.Done)照常执行。
func TestDoRecovers(t *testing.T) {
	var mu sync.Mutex
	ran := false
	ok := Do("测试", func() {
		mu.Lock()
		defer mu.Unlock()
		defer func() { ran = true }()
		var m map[string]int
		m["x"] = 1 // 往 nil map 里写
	})
	if ok || !ran {
		t.Fatalf("panic 应被兜住、defer 照常执行: ok=%v ran=%v", ok, ran)
	}
	if !mu.TryLock() {
		t.Fatal("panic 之后锁应该已经放开")
	}
	if !Do("测试", func() {}) {
		t.Fatal("正常跑完应返回 true")
	}
}

// 协程里 panic 不带走进程;带参数的写法在调用那一刻取值。
func TestGoRecoversAndBindsArgs(t *testing.T) {
	var wg sync.WaitGroup
	wg.Add(4)
	Go("测试", func() {
		defer wg.Done()
		var p *struct{ n int }
		_ = p.n // 空指针
	})
	got := make([]int, 3)
	for i := 0; i < 3; i++ {
		Go2("测试", i, i*10, func(i, v int) {
			defer wg.Done()
			got[i] = v
		})
	}
	wg.Wait()
	if got[0] != 0 || got[1] != 10 || got[2] != 20 {
		t.Fatalf("参数没按调用时的值传进去: %v", got)
	}
	a := "before"
	done := make(chan string, 1)
	Go1("测试", a, func(s string) { done <- s })
	a = "after"
	_ = a
	if s := <-done; s != "before" {
		t.Fatalf("参数应在调用那一刻取值: %q", s)
	}
	var w3 sync.WaitGroup
	w3.Add(1)
	sum := 0
	Go3("测试", 1, 2, 3, func(a, b, c int) { defer w3.Done(); sum = a + b + c })
	w3.Wait()
	if sum != 6 {
		t.Fatalf("Go3: %d", sum)
	}
}
