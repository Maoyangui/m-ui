package core

import (
	"testing"
	"time"
)

func TestDeviceLimit(t *testing.T) {
	l := NewLimiter()
	l.SetLimits(map[string]UserLimitSpec{"alice": {DeviceLimit: 2}})

	if !l.AllowConn("alice", "1.1.1.1") {
		t.Fatal("第 1 个 IP 应放行")
	}
	if !l.AllowConn("alice", "2.2.2.2") {
		t.Fatal("第 2 个 IP 应放行")
	}
	// 已知 IP 再连不占新名额
	if !l.AllowConn("alice", "1.1.1.1") {
		t.Fatal("已知 IP 重连应放行")
	}
	// 第 3 个新 IP 超限,拒绝
	if l.AllowConn("alice", "3.3.3.3") {
		t.Fatal("超过设备上限的新 IP 应被拒绝")
	}
	if got := len(l.ActiveIPs("alice")); got != 2 {
		t.Fatalf("活跃 IP 应为 2,实际 %d", got)
	}
}

// 设备数是"同时在线"而非锁定 IP:占位设备下线后,先前被拒的设备应立即可进入。
func TestDeviceLimitIsConcurrentNotSticky(t *testing.T) {
	l := NewLimiter()
	l.SetLimits(map[string]UserLimitSpec{"alice": {DeviceLimit: 2}})

	l.AllowConn("alice", "1.1.1.1")
	l.AllowConn("alice", "2.2.2.2")
	if l.AllowConn("alice", "3.3.3.3") {
		t.Fatal("满员时第 3 个设备应被拒")
	}

	// 模拟设备 1、2 下线(活跃时间推到空闲窗口之外)
	l.mu.Lock()
	stale := time.Now().Unix() - int64(l.idleWindow.Seconds()) - 1
	l.ips["alice"]["1.1.1.1"] = stale
	l.ips["alice"]["2.2.2.2"] = stale
	l.mu.Unlock()

	// 名额已释放,原先被拒的设备应立刻放行(不存在冷却/黑名单)
	if !l.AllowConn("alice", "3.3.3.3") {
		t.Fatal("占位设备下线后,先前被拒的设备应立即可连接")
	}
	if got := len(l.ActiveIPs("alice")); got != 1 {
		t.Fatalf("此时在线设备应为 1,实际 %d", got)
	}
}

// 有流量经过即刷新在线状态,长连接设备不会因"没建新连接"被误判下线。
func TestTrafficKeepsDeviceOnline(t *testing.T) {
	l := NewLimiter()
	l.SetLimits(map[string]UserLimitSpec{"alice": {DeviceLimit: 1}})
	l.AllowConn("alice", "1.1.1.1")

	// 把活跃时间推到即将过期
	l.mu.Lock()
	l.ips["alice"]["1.1.1.1"] = time.Now().Unix() - int64(l.idleWindow.Seconds()) - 1
	l.mu.Unlock()

	// 流量回调刷新活跃时间
	l.keepaliveFor("alice", "1.1.1.1")()

	// 名额仍被占用,新设备应被拒
	if l.AllowConn("alice", "2.2.2.2") {
		t.Fatal("有流量的设备应保持在线,新设备不应挤占其名额")
	}
}

func TestDeviceLimitUnlimited(t *testing.T) {
	l := NewLimiter()
	l.SetLimits(map[string]UserLimitSpec{"bob": {DeviceLimit: 0}})
	for i, ip := range []string{"1.1.1.1", "2.2.2.2", "3.3.3.3", "4.4.4.4"} {
		if !l.AllowConn("bob", ip) {
			t.Fatalf("不限设备时第 %d 个 IP 不应被拒", i+1)
		}
	}
}

func TestEmptyUserBypasses(t *testing.T) {
	l := NewLimiter()
	l.SetLimits(map[string]UserLimitSpec{"x": {DeviceLimit: 1}})
	// 空用户名(无鉴权直连)不受设备限制
	for i := 0; i < 5; i++ {
		if !l.AllowConn("", "9.9.9.9") {
			t.Fatal("空用户名不应受限")
		}
	}
}

func TestIdlePruneFreesSlot(t *testing.T) {
	l := NewLimiter()
	l.idleWindow = 0 // 立即判定空闲下线
	l.SetLimits(map[string]UserLimitSpec{"c": {DeviceLimit: 1}})

	if !l.AllowConn("c", "1.1.1.1") {
		t.Fatal("首个 IP 应放行")
	}
	time.Sleep(1100 * time.Millisecond) // 跨过一个 unix 秒,使其落在 cutoff 之前
	// 旧 IP 已空闲,新 IP 应能占位
	if !l.AllowConn("c", "2.2.2.2") {
		t.Fatal("空闲 IP 应被回收,新 IP 应放行")
	}
}

func TestSetLimitsConvertsMbps(t *testing.T) {
	l := NewLimiter()
	l.SetLimits(map[string]UserLimitSpec{"d": {UpMbps: 8, DownMbps: 100}})
	if got := l.limits["d"].upBps; got != 8*125000 {
		t.Fatalf("上行应为 %d 字节/秒,实际 %d", 8*125000, got)
	}
	if got := l.limits["d"].downBps; got != 100*125000 {
		t.Fatalf("下行应为 %d 字节/秒,实际 %d", 100*125000, got)
	}
	// 取用桶:限速>0 应得到非 nil 桶,=0 应为 nil
	if l.bucketFor(l.up, "d", l.limits["d"].upBps) == nil {
		t.Fatal("上行桶不应为 nil")
	}
	if l.bucketFor(l.down, "none", 0) != nil {
		t.Fatal("零限速应返回 nil 桶")
	}
}
