package core

import "testing"

// anytlsRaceSkip -race 下跳过 anytls 的真实端口用例:上游 github.com/anytls/sing-anytls(v0.0.11,最新 v0.0.13 仍是这样)
// session/stream.go 里 Stream.dieErr 没有同步 —— closeLocally / closeWithError 在 sync.Once 里写它,Read / Write
// 在别的 goroutine 里不加锁读,会话关闭(踢线、对端断开)和正在转发的数据同时发生时竞态检测必报(30 次约 3~4 次)。
// 这不是 m-ui 的用法问题:客户端、服务端两头的 Stream 都是上游自己的代码,m-ui 只是调 Session.Close。
// 影响:关闭后极短窗口内的一次 Write 可能没看到「已关闭」,照样去写会话(随后失败);理论上 error 接口两个字没写完就被读到
// 才可能出更怪的结果,实际没观察到。普通构建与发布不受竞态检测影响。修法(dieErr 改成 atomic.Pointer[error])
// 已整理成补丁给上游;上游合入后升级依赖、删掉这个跳过。别的协议照常在 -race 下跑。
func anytlsRaceSkip(t *testing.T) {
	t.Helper()
	if raceOn {
		t.Skip("-race 下跳过:上游 sing-anytls Stream.dieErr 未同步(见 anytlsRaceSkip 的说明)")
	}
}
