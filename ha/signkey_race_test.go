package ha

import (
	"sync"
	"testing"
)

// 还没有签名密钥的主机,第一次被几处同时要密钥(向几台副机并发推容灾状态、发副本):只能生成一把,
// 大家拿到的公钥一样,签名用的也是存下来的那把。以前各生成一把、后写的覆盖先写的,先收到的副机钉住的公钥作废,
// 同一纪元之后的推送全被拒(CI 上 TestPeerAuthAndVote、TestTakeoverVerdictFollowsEvidence 等因此失败)。
func TestSignKeyConcurrentFirstUse(t *testing.T) {
	for round := 0; round < 5; round++ {
		m := testService(t, false, map[string]string{KeyEpoch: "1"})
		if m.setting(KeySignKey) != "" {
			t.Fatal("前提:新主机还没有签名密钥")
		}
		const n = 16
		pubs := make([]string, n)
		errs := make([]error, n)
		start := make(chan struct{})
		var wg sync.WaitGroup
		for i := 0; i < n; i++ {
			wg.Add(1)
			go func(i int) {
				defer wg.Done()
				<-start
				if i%2 == 0 {
					pubs[i], errs[i] = m.SignPub()
					return
				}
				k, err := m.signPriv()
				if err == nil {
					pubs[i] = PubOf(k)
				}
				errs[i] = err
			}(i)
		}
		close(start)
		wg.Wait()
		stored, err := SignKey(m.setting(KeySignKey))
		if err != nil {
			t.Fatalf("存下的签名密钥读不出来: %v", err)
		}
		want := PubOf(stored)
		for i := range pubs {
			if errs[i] != nil {
				t.Fatalf("第 %d 轮第 %d 个取密钥出错: %v", round, i, errs[i])
			}
			if pubs[i] != want {
				t.Fatalf("第 %d 轮第 %d 个拿到的公钥和存下的不是同一把:并发第一次用时生成了不止一把", round, i)
			}
		}
	}
}
