package sub

import (
	"net/http"
	"testing"
)

// 设为主机冻结写操作时,订阅页生成 / 取消临时共享也挡住(503,不然改了带不到新主机);没冻结时登记在途、做完放开;
// 拉订阅、打开落地页照常。
func TestShareFrozenDuringSetMaster(t *testing.T) {
	s, db := shareServer(t)
	frozen, inflight, gated := true, 0, 0
	s.WriteGate = func() (func(), bool, string) {
		gated++
		if frozen {
			return func() {}, false, "正在把主机换到 TW,写操作暂停"
		}
		inflight++
		return func() { inflight-- }, true, ""
	}
	if w := doReq(s, "POST", "/sub/alice?share=on", browserUA); w.Code != http.StatusServiceUnavailable {
		t.Fatalf("冻结时生成共享要 503: %d", w.Code)
	}
	if token(t, db) != "" {
		t.Fatal("冻结时不能生成共享")
	}
	if w := doReq(s, "GET", "/sub/alice", "curl/8.4.0"); w.Code != http.StatusOK || gated != 1 {
		t.Fatalf("拉订阅照常、不经写闸: %d gated=%d", w.Code, gated)
	}
	frozen = false
	if w := doReq(s, "POST", "/sub/alice?share=on", browserUA); w.Code != http.StatusOK {
		t.Fatalf("没冻结时照常生成: %d", w.Code)
	}
	if token(t, db) == "" || inflight != 0 {
		t.Fatalf("生成了,在途登记已放开: inflight=%d", inflight)
	}
}
