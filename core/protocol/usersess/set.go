package usersess

import (
	"context"
	"sync"

	"github.com/sagernet/sing/service"
)

// Set 一个数据面实例里全部入站的登记表,踢线时按用户名一次扫全部入站。
//
// 放在 sing-box 的服务注册表里(NewBox 时注册,入站构造时取)。注意这份注册表是进程级共享的:
// 每次 NewBox 都会覆盖同一个槽位,热加入站也从同一处取 —— 先停旧数据面再起新的,所以拿到的永远是当前这份;
// 入站只在构造时取一次,运行期不再回头查。
type Set struct {
	mu sync.Mutex
	m  map[*Registry]struct{}
}

func NewSet() *Set { return &Set{m: map[*Registry]struct{}{}} }

// SetFromContext 取当前数据面的 Set;没有(干跑、测试)返回 nil。
func SetFromContext(ctx context.Context) *Set { return service.FromContext[*Set](ctx) }

// Add 入站**启动成功之后**再加:构造成功但没起来的入站不会被关闭,加早了就永远留在这里。
func (s *Set) Add(r *Registry) {
	if s == nil || r == nil {
		return
	}
	s.mu.Lock()
	s.m[r] = struct{}{}
	s.mu.Unlock()
}

// Remove 幂等。
func (s *Set) Remove(r *Registry) {
	if s == nil || r == nil {
		return
	}
	s.mu.Lock()
	delete(s.m, r)
	s.mu.Unlock()
}

// CloseUsers 在全部入站上关掉这些名字的会话。先复制登记表列表再解锁,不在持 Set.mu 时进 Registry.mu。
func (s *Set) CloseUsers(names []string) int {
	if s == nil {
		return 0
	}
	s.mu.Lock()
	regs := make([]*Registry, 0, len(s.m))
	for r := range s.m {
		regs = append(regs, r)
	}
	s.mu.Unlock()
	n := 0
	for _, r := range regs {
		n += r.CloseUsers(names)
	}
	return n
}

// Len 登记在册的入站数。
func (s *Set) Len() int {
	if s == nil {
		return 0
	}
	s.mu.Lock()
	defer s.mu.Unlock()
	return len(s.m)
}
