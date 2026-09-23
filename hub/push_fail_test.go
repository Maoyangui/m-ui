package hub

import (
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"path/filepath"
	"strings"
	"sync"
	"sync/atomic"
	"testing"

	"github.com/Maoyangui/m-ui/database"
	"github.com/Maoyangui/m-ui/database/model"
)

// 副机 apply 返回 503(新配置在它那边起不来)时主机该怎么做,几件事一起钉住:
//  1. 同一修订连续被拒要退避,不能每 5 秒硬推(那会让副机每 5 秒拆一次数据面);
//  2. 推送失败也照样拉报告 —— 流量并入、在线 IP、设备并集不能跟着推送失败一起停掉
//     (0.6.10 推送一失败就 return,这台副机的流量不再并入主机、300 秒后设备也不再计入);
//  3. 状态上它是"在线但配置未同步",不是"离线",也不能因为副机库里的修订号已经是新的就显示"已同步";
//  4. 修订号一变立刻重推,不受退避影响;推送成功后失败记录清掉。
func TestPushFailureBacksOffButStillFetchesReport(t *testing.T) {
	db, err := database.Open(filepath.Join(t.TempDir(), "x.db"))
	if err != nil {
		t.Fatal(err)
	}
	defer database.Close(db)
	db.Create(&model.Node{Name: "本机", IsLocal: true, Enabled: true})
	db.Create(&model.User{Name: "alice", Enabled: true, DeviceLimit: 3, Credentials: []byte(`{}`)})

	var failApply int32 = 1
	var applies int32
	var mu sync.Mutex
	lastRev := "" // 副机"库里"的修订号:真实副机在 ApplySnapshot 落库时就写了,不等数据面
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		switch {
		case strings.HasSuffix(r.URL.Path, "/agent/apply"):
			atomic.AddInt32(&applies, 1)
			var snap Snapshot
			_ = json.NewDecoder(r.Body).Decode(&snap)
			mu.Lock()
			lastRev = snap.Revision
			mu.Unlock()
			if atomic.LoadInt32(&failApply) == 1 {
				http.Error(w, `{"error":"本机数据面应用失败: 端口被占"}`, http.StatusServiceUnavailable)
				return
			}
			json.NewEncoder(w).Encode(map[string]string{"ok": "1", "revision": snap.Revision})
		case strings.HasSuffix(r.URL.Path, "/agent/report"):
			mu.Lock()
			rev := lastRev
			mu.Unlock()
			json.NewEncoder(w).Encode(Report{Version: "t", CoreRunning: true, Revision: rev,
				ReloadPending: atomic.LoadInt32(&failApply) == 1,
				Counters:      []model.AgentCounter{{UserName: "alice", Up: 100, Down: 200}},
				Onlines:       map[string][]string{"alice": {"9.9.9.9"}}})
		case strings.HasSuffix(r.URL.Path, "/agent/external-ips"):
			w.Write([]byte(`{"ok":"1"}`))
		default:
			http.NotFound(w, r)
		}
	}))
	defer srv.Close()
	db.Create(&model.Node{Name: "n1", ApiUrl: srv.URL + "/app/", Token: "t", Enabled: true})
	h := New(Deps{DB: db, Setting: func(string) string { return "" }, IsNode: func() bool { return false }})

	// 三轮 tick 连着来(中间不睡):第一次被拒后进 5 秒退避,后两轮不该再推
	h.tick()
	h.tick()
	h.tick()
	if got := atomic.LoadInt32(&applies); got != 1 {
		t.Fatalf("同一修订被拒后应退避,三轮只该推 1 次,实际 %d 次", got)
	}
	st := h.Statuses()[2]
	if !st.OK {
		t.Fatalf("报告拉到了就是在线,不该判成离线: %+v", st)
	}
	if !strings.Contains(st.Error, "推送失败") {
		t.Fatalf("推送失败应写进 Error 让人看见: %+v", st)
	}
	if st.Synced {
		t.Fatal("副机库里的修订号已经是新的,但数据面没应用成功(reloadPending):不能显示已同步")
	}
	var u model.User
	db.Where("name = ?", "alice").First(&u)
	if u.Up != 100 || u.Down != 200 {
		t.Fatalf("推送失败也要拉报告并入流量(且按游标不重复),实际 %d/%d", u.Up, u.Down)
	}
	if ips := h.RemoteIPs("alice"); len(ips) != 1 {
		t.Fatalf("推送失败也要记住副机上的在线设备,实际 %v", ips)
	}

	// 管理员改了配置:修订号变了,立刻重推,不受退避影响
	db.Model(&model.User{}).Where("name = ?", "alice").Update("device_limit", 5)
	h.tick()
	if got := atomic.LoadInt32(&applies); got != 2 {
		t.Fatalf("修订号变了应立刻重推,实际累计 %d 次", got)
	}

	// 副机修好了:推送成功,失败记录清掉,状态干净、已同步
	atomic.StoreInt32(&failApply, 0)
	db.Model(&model.User{}).Where("name = ?", "alice").Update("device_limit", 6)
	h.tick()
	h.mu.Lock()
	left := h.pushFail[2]
	h.mu.Unlock()
	if left != nil {
		t.Fatalf("推送成功后应清掉失败记录: %+v", left)
	}
	if st := h.Statuses()[2]; !st.OK || st.Error != "" || !st.Synced {
		t.Fatalf("推送成功后应是在线、无错误、已同步: %+v", st)
	}
}

// 失联(连不上)不算"被拒":副机一回来,下一轮 tick 就得把最新配置推过去 ——
// 失联期间做的停用、换凭据都等着它。把失联也算进退避,副机回来还要再等最长 5 分钟,
// 旧凭据在那台机上就多活 5 分钟(0.6.9 / 0.6.10 都是 ≤5 秒)。
func TestTransportFailureDoesNotBackOff(t *testing.T) {
	db, err := database.Open(filepath.Join(t.TempDir(), "x.db"))
	if err != nil {
		t.Fatal(err)
	}
	defer database.Close(db)
	db.Create(&model.Node{Name: "本机", IsLocal: true, Enabled: true})
	db.Create(&model.User{Name: "bob", Enabled: true, Credentials: []byte(`{}`)})

	var applies int32
	var down int32 = 1 // 1 = 副机失联(连接被立刻拒绝)
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		switch {
		case strings.HasSuffix(r.URL.Path, "/agent/apply"):
			atomic.AddInt32(&applies, 1)
			var snap Snapshot
			_ = json.NewDecoder(r.Body).Decode(&snap)
			json.NewEncoder(w).Encode(map[string]string{"ok": "1", "revision": snap.Revision})
		case strings.HasSuffix(r.URL.Path, "/agent/report"):
			json.NewEncoder(w).Encode(Report{Version: "t", CoreRunning: true})
		default:
			w.Write([]byte(`{"ok":"1"}`))
		}
	}))
	defer srv.Close()
	// 先指向一个没人监听的地址模拟失联,恢复时再改回真地址
	deadURL := "http://127.0.0.1:1/app/"
	db.Create(&model.Node{Name: "n1", ApiUrl: deadURL, Token: "t", Enabled: true})
	h := New(Deps{DB: db, Setting: func(string) string { return "" }, IsNode: func() bool { return false }})

	for i := 0; i < 3; i++ {
		h.tick()
	}
	h.mu.Lock()
	pf := h.pushFail[2]
	h.mu.Unlock()
	if pf != nil {
		t.Fatalf("连不上不该计入退避: %+v", pf)
	}
	atomic.StoreInt32(&down, 0)
	db.Model(&model.Node{}).Where("id = ?", 2).Update("api_url", srv.URL+"/app/")
	h.tick()
	if got := atomic.LoadInt32(&applies); got != 1 {
		t.Fatalf("副机一回来就该在这一轮拿到配置,实际推了 %d 次", got)
	}
	if st := h.Statuses()[2]; !st.OK {
		t.Fatalf("恢复后应在线: %+v", st)
	}
}
