package core

import (
	"context"
	"encoding/json"
	"fmt"
	"net"
	"sync"
	"testing"
	"time"
)

// 审计 I5:以前隔离测速从建实例一直到网络测完都持有 validateMu,单个最长 10 秒:外部节点测速只能一个接一个,
// 期间面板保存的干跑、副机应用快照的干跑都要排队(事务里还占着数据库写锁)。现在锁只包住建实例,网络测试在锁外。
func TestIsolatedCheckDoesNotBlockValidate(t *testing.T) {
	ln, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatal(err)
	}
	defer ln.Close()
	var mu sync.Mutex
	var held []net.Conn
	go func() { // 接受连接但一个字节都不回:SOCKS 握手一直等着
		for {
			c, err := ln.Accept()
			if err != nil {
				return
			}
			mu.Lock()
			held = append(held, c)
			mu.Unlock()
		}
	}()
	defer func() {
		mu.Lock()
		for _, c := range held {
			c.Close()
		}
		mu.Unlock()
	}()
	port := ln.Addr().(*net.TCPAddr).Port
	ob := json.RawMessage(fmt.Sprintf(`{"type":"socks","tag":"hang","server":"127.0.0.1","server_port":%d,"version":"5"}`, port))
	ctx, cancel := context.WithTimeout(context.Background(), 8*time.Second)
	defer cancel()
	done := make(chan OutboundTest, 2)
	for i := 0; i < 2; i++ { // 两个测速同时在等网络
		go func() { done <- TestOutboundIsolated(ctx, ob, "http://example.com/", 1) }()
	}
	deadline := time.Now().Add(5 * time.Second)
	for {
		mu.Lock()
		n := len(held)
		mu.Unlock()
		if n >= 2 {
			break // 两个都建好实例、连上了,正卡在网络上
		}
		if time.Now().After(deadline) {
			t.Fatalf("测速没能并发连出去(只连上 %d 个)", n)
		}
		time.Sleep(10 * time.Millisecond)
	}
	start := time.Now()
	if err := ValidateOutbound(json.RawMessage(`{"type":"direct","tag":"d2"}`)); err != nil {
		t.Fatal(err)
	}
	if d := time.Since(start); d > 2*time.Second {
		t.Fatalf("干跑被网络测速挡了 %v", d)
	}
	cancel()
	for i := 0; i < 2; i++ {
		select {
		case res := <-done:
			if res.OK || res.Config {
				t.Fatalf("连不通应是 fail 而不是 OK / 配置错误: %+v", res)
			}
		case <-time.After(5 * time.Second):
			t.Fatal("取消后测速应立即返回")
		}
	}
}

// 出站参数 sing-box 不认:标成配置错误(测不了),不是"不通"。
func TestIsolatedCheckConfigError(t *testing.T) {
	res := TestOutboundIsolated(context.Background(), json.RawMessage(`{"type":"shadowsocks","tag":"bad","server":"127.0.0.1","server_port":1,"method":"no-such-cipher","password":"x"}`), "http://example.com/", 1)
	if res.OK || !res.Config || res.Error == "" {
		t.Fatalf("应报配置错误: %+v", res)
	}
}
