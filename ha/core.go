package ha

import (
	"crypto/ed25519"
	"sync"
	"time"
)

// coreState 纪元与接任核心在内存里的运行时状态。Service 的结构体定义在 service.go,这些字段另挂在这里,
// 按 *Service 一份(进程里通常只有一个;测试里一台一个)。
type coreState struct {
	mu     sync.Mutex // 管下面的字段(baseKey、notListed 除外)
	evalMu sync.Mutex // 主机侧的判定同一时刻只做一次(TryLock:判定里推状态,回复又会进来要求判定)

	inst    string             // 本机实例号(<数据目录>/ha/instance,读一次就缓存)
	baseKey ed25519.PrivateKey // 与 Service.base 一起缓存的签名私钥(归 Service.mu 管)

	// 副机侧
	notListed time.Time // 从什么时候起不在备用名单里了(连续 standbyDropGrace 才删副本;归 Service.mu 管)

	// 主机侧
	revoke       uint64             // 随状态推出去的撤销:副机清掉纪元不超过它的交接标记
	fence        *BootResult        // Boot 定下的围栏,StartupCheck 执行
	checking     bool               // 启动自检进行中:回复只记下,不判定、不告警
	reclaimed    map[string]bool    // 已经收回过的 (主机, 纪元):同一个只收回一次
	downSince    map[uint]time.Time // 各台从什么时候起连不上
	staleSince   map[uint]time.Time // 各台从什么时候起一直回 stale("已脱离"告警要持续一阵)
	bogus        map[uint]bool      // 已经告过"报了异常纪元"的副机
	unknownAlert map[string]bool    // 已经告过"跟着本机不认识的主机"的 (副机, 主机, 纪元)
	formerPlain  uint               // 已经提示过"原主机回来了但地址是明文 http"的那一台

	// 备用侧
	takeoverErrAt int64 // 最近一次接任失败的时间(同伴据此不再给本机让行)
}

var cores sync.Map // *Service → *coreState

func (s *Service) core() *coreState {
	if c, ok := cores.Load(s); ok {
		return c.(*coreState)
	}
	c, _ := cores.LoadOrStore(s, &coreState{reclaimed: map[string]bool{}, downSince: map[uint]time.Time{}, staleSince: map[uint]time.Time{},
		bogus: map[uint]bool{}, unknownAlert: map[string]bool{}})
	return c.(*coreState)
}

// revoked 随状态推出去的撤销纪元。
func (s *Service) revoked() uint64 {
	c := s.core()
	c.mu.Lock()
	defer c.mu.Unlock()
	return c.revoke
}

// RevokeGrants 撤销纪元不超过 epoch 的交接标记(设为主机确认没做成时):随下一份状态推给全部副机,并马上推一轮。
func (s *Service) RevokeGrants(epoch uint64) {
	c := s.core()
	c.mu.Lock()
	if epoch > c.revoke {
		c.revoke = epoch
	}
	c.mu.Unlock()
	s.invalidate()
	go s.pushAll(s.remoteNodes(), 15*time.Second)
}

// lastTakeoverErr 本机最近一次接任失败的时间(unix 秒;同伴状态里带上,排在后面的备用据此不再等本机)。
func (s *Service) lastTakeoverErr() int64 {
	c := s.core()
	c.mu.Lock()
	defer c.mu.Unlock()
	return c.takeoverErrAt
}

func (s *Service) noteTakeoverErr() {
	c := s.core()
	c.mu.Lock()
	c.takeoverErrAt = s.now().Unix()
	c.mu.Unlock()
}
