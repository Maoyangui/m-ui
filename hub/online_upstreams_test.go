package hub

import (
	"sort"
	"testing"
	"time"
)

// 用户 N5:上游的在线灯按真实出站字节,副机报上来的「在用的上游」并起来;失联超过宽限期的副机不算,老副机不报就是空。
func TestRemoteOnlineUpstreams(t *testing.T) {
	now := time.Now().Unix()
	h := &Hub{
		remoteAt:        map[uint]int64{1: now - 10, 2: now - 20, 3: now - remoteReportGrace - 1, 4: now - 5},
		remoteOnlineUps: map[uint][]string{1: {"warp", "日本中转"}, 2: {"warp", ""}, 3: {"已经失联的那台上的"}, 4: nil},
	}
	got := h.RemoteOnlineUpstreams()
	sort.Strings(got)
	if len(got) != 2 || got[0] != "warp" || got[1] != "日本中转" {
		t.Fatalf("应是在线副机报上来的并集(去重、去空、不含失联的): %v", got)
	}
}
