package hub

import (
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"path/filepath"
	"strings"
	"sync/atomic"
	"testing"
	"time"

	"github.com/Maoyangui/m-ui/database"
	"github.com/Maoyangui/m-ui/database/model"
)

// settle 等定时同步这一轮发出去的请求都回来、结果都落库(测试里 tick 之后用)。
func (h *Hub) settle() { h.rounds.Wait() }

// 一台挂起的副机(请求进来就不回)不能拖住别的副机:tick 立刻返回;健康副机每一轮都按时同步;
// 挂起那台上一轮还在途时不再叠加请求;Stop 时在途请求随 ctx 取消立刻收尾。
// 以前整轮等所有副机,一台黑洞把所有副机的同步周期从 5 秒拖到 75 秒左右。
func TestHungNodeDoesNotStallOthers(t *testing.T) {
	db, err := database.Open(filepath.Join(t.TempDir(), "x.db"))
	if err != nil {
		t.Fatal(err)
	}
	defer database.Close(db)
	db.Create(&model.Node{Name: "本机", IsLocal: true, Enabled: true})
	db.Create(&model.User{Name: "alice", Enabled: true, DeviceLimit: 3, Credentials: []byte(`{}`)})

	release := make(chan struct{})
	var hungSync, hungIPs int32 // 同步(推送 / 拉报告)与设备租约各自的请求数
	hung := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if strings.HasSuffix(r.URL.Path, "/agent/external-ips") {
			atomic.AddInt32(&hungIPs, 1)
		} else {
			atomic.AddInt32(&hungSync, 1)
		}
		select {
		case <-release:
		case <-r.Context().Done():
		}
	}))
	defer hung.Close()
	defer close(release)

	var reports int32
	healthy := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		switch {
		case strings.HasSuffix(r.URL.Path, "/agent/apply"):
			var snap Snapshot
			_ = json.NewDecoder(r.Body).Decode(&snap)
			json.NewEncoder(w).Encode(map[string]string{"ok": "1", "revision": snap.Revision})
		case strings.HasSuffix(r.URL.Path, "/agent/report"):
			atomic.AddInt32(&reports, 1)
			json.NewEncoder(w).Encode(Report{Version: "t", CoreRunning: true, Onlines: map[string][]string{"alice": {"9.9.9.9"}}})
		default:
			w.Write([]byte(`{"ok":"1"}`))
		}
	}))
	defer healthy.Close()

	db.Create(&model.Node{Name: "hung", ApiUrl: hung.URL + "/app/", Token: "t", Enabled: true})
	db.Create(&model.Node{Name: "ok", ApiUrl: healthy.URL + "/app/", Token: "t", Enabled: true})
	h := New(Deps{DB: db, Setting: func(string) string { return "" }, IsNode: func() bool { return false }})

	for round := 1; round <= 3; round++ {
		start := time.Now()
		h.tick()
		if d := time.Since(start); d > time.Second {
			t.Fatalf("第 %d 轮 tick 被挂起的副机拖住了 %v", round, d)
		}
		deadline := time.Now().Add(3 * time.Second)
		for atomic.LoadInt32(&reports) < int32(round) && time.Now().Before(deadline) {
			time.Sleep(10 * time.Millisecond)
		}
		if got := atomic.LoadInt32(&reports); got < int32(round) {
			t.Fatalf("第 %d 轮健康副机没按时同步(报告 %d 次)", round, got)
		}
	}
	if st := h.Statuses()[3]; !st.OK || !st.CoreRunning {
		t.Fatalf("健康副机状态不对: %+v", st)
	}
	if s, i := atomic.LoadInt32(&hungSync), atomic.LoadInt32(&hungIPs); s != 1 || i != 1 {
		t.Fatalf("挂起的副机上一轮还在途,不该叠加请求:同步 %d 个、设备租约 %d 个(各应 1 个)", s, i)
	}
	start := time.Now()
	h.Stop()
	if d := time.Since(start); d > 2*time.Second {
		t.Fatalf("Stop 等在途请求等了 %v,应随取消立刻返回", d)
	}
}

// 手动推送等不到这台副机的闸(上一轮还挂着)要如实报错,不能无限等。
func TestPushNowGivesUpOnBusyNode(t *testing.T) {
	db, err := database.Open(filepath.Join(t.TempDir(), "x.db"))
	if err != nil {
		t.Fatal(err)
	}
	defer database.Close(db)
	db.Create(&model.Node{Name: "本机", IsLocal: true, Enabled: true})
	db.Create(&model.Node{Name: "n1", ApiUrl: "http://127.0.0.1:1/app/", Token: "t", Enabled: true})
	h := New(Deps{DB: db, Setting: func(string) string { return "" }, IsNode: func() bool { return false }})
	if !h.tryNode(2) {
		t.Fatal("拿不到空闲副机的闸")
	}
	if err := h.waitNode(2, 50*time.Millisecond); err == nil || !strings.Contains(err.Error(), "还没结束") {
		t.Fatalf("闸被占着时应报错,得到 %v", err)
	}
	h.releaseNode(2)
	if err := h.waitNode(2, 50*time.Millisecond); err != nil {
		t.Fatalf("闸放开后应能拿到: %v", err)
	}
	h.releaseNode(2)
}
