package hub

import (
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"path/filepath"
	"strings"
	"sync"
	"testing"

	"github.com/Maoyangui/m-ui/database"
	"github.com/Maoyangui/m-ui/database/model"
)

// 推一份新配置失败(副机库里已经是那份修订、数据面回滚了),管理员把改动撤回:撤回后的配置内容和上一次推成功的那份
// 一模一样,修订号(内容哈希)也一样。主机不能只拿"上次推成功的修订"比 —— 副机实际在那份失败的修订上,得立刻再推一次,
// 不然副机库里一直留着撤掉的东西、待重载标记不清、面板一直显示未同步,要等 10 分钟的定期重推。
func TestRevertAfterFailedPushRepushesImmediately(t *testing.T) {
	db, err := database.Open(filepath.Join(t.TempDir(), "x.db"))
	if err != nil {
		t.Fatal(err)
	}
	defer database.Close(db)
	db.Create(&model.Node{Name: "本机", IsLocal: true, Enabled: true})
	db.Create(&model.User{Name: "alice", Enabled: true, Credentials: []byte(`{}`)})

	var mu sync.Mutex
	var applied []string // 副机收到的每次 apply 的修订号
	nodeRev, pending := "", false
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		switch {
		case strings.HasSuffix(r.URL.Path, "/agent/apply"):
			var snap Snapshot
			_ = json.NewDecoder(r.Body).Decode(&snap)
			bad := false
			for _, u := range snap.Users {
				if u.Name == "bob" { // 这份配置在副机上起不来
					bad = true
				}
			}
			mu.Lock()
			applied = append(applied, snap.Revision)
			nodeRev, pending = snap.Revision, bad // 真实副机落库就写修订号,数据面没起来就记待重载
			mu.Unlock()
			if bad {
				http.Error(w, `{"error":"本机数据面应用失败: 端口被占"}`, http.StatusServiceUnavailable)
				return
			}
			json.NewEncoder(w).Encode(map[string]string{"ok": "1", "revision": snap.Revision})
		case strings.HasSuffix(r.URL.Path, "/agent/report"):
			mu.Lock()
			rep := Report{Version: "t", CoreRunning: true, Revision: nodeRev, ReloadPending: pending}
			mu.Unlock()
			json.NewEncoder(w).Encode(rep)
		default:
			w.Write([]byte(`{"ok":"1"}`))
		}
	}))
	defer srv.Close()
	db.Create(&model.Node{Name: "n1", ApiUrl: srv.URL + "/app/", Token: "t", Enabled: true})
	h := New(Deps{DB: db, Setting: func(string) string { return "" }, IsNode: func() bool { return false }})
	round := func() { h.tick(); h.settle() }
	count := func(rev string) int {
		mu.Lock()
		defer mu.Unlock()
		n := 0
		for _, r := range applied {
			if r == rev {
				n++
			}
		}
		return n
	}

	round()
	mu.Lock()
	revA := nodeRev
	mu.Unlock()
	if revA == "" || count(revA) != 1 {
		t.Fatalf("首轮应推一次并成功: %v", applied)
	}
	bob := model.User{Name: "bob", Enabled: true, Credentials: []byte(`{}`)}
	db.Create(&bob)
	round()
	if mu.Lock(); nodeRev == revA || !pending {
		mu.Unlock()
		t.Fatalf("加了 bob 的修订应推过去并失败(副机库里是新修订、待重载): %v", applied)
	}
	mu.Unlock()

	db.Delete(&model.User{}, bob.Id) // 撤回:内容回到推成功的那份
	round()
	if n := count(revA); n != 2 {
		t.Fatalf("撤回后副机还停在失败的修订上,应立刻再推一次 %s,实际推了 %d 次(全部: %v)", revA, n, applied)
	}
	round()
	if n := count(revA); n != 2 {
		t.Fatalf("副机已经确认后不该再重复推: %d 次", n)
	}
	if st := h.Statuses()[2]; !st.Synced {
		t.Fatalf("重推成功后应显示已同步: %+v", st)
	}
}
