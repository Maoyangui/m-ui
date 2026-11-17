package web

import (
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"path/filepath"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/Maoyangui/m-ui/database"
	"github.com/Maoyangui/m-ui/database/model"
	"github.com/Maoyangui/m-ui/reward"
	"github.com/Maoyangui/m-ui/runner"
)

const (
	rwAddrMe    = "7xKXtg2CW87d97TXJSDpbD5jBkheTqA83TZRuJosgAsU"
	rwAddrOther = "Es9vMFrzaCERmJfrF4H2FYD4KCoNkY11McCe8BenwNYB"
)

// fakeRegistry 假的登记服务 + 公开账本。
type fakeRegistry struct {
	mu       sync.Mutex
	reports  []reward.Report
	reject   string // 非空:上报一律按这个原因拒绝(400)
	gbFloor  int64  // 登记处记着的累计值(GB);答复里取它和上报值里较大的
	credited int64
	intent   reward.Intent
	exports  int // 向登记服务要账本的次数
	raws     int // 读公开账本(GitHub 那份)的次数
	ledger   func() reward.Ledger
}

func rwLedger(now int64) reward.Ledger {
	l := reward.Ledger{V: 1, UpdatedAt: now - 30, PayoutAt: now + 86400*200, OrderRate: 0.4,
		Chains: []reward.Chain{{Chain: "solana", Address: rwAddrOther, Received: 120, Balance: 120}, {Chain: "tron", Address: "TDoUMF4nF244o5GZvBBwX5t9axvnSoP1Cm", Received: 30, Balance: 30}},
		Donate: reward.DonateInfo{Chain: "solana", Address: rwAddrOther, Min: 1, Max: 100000},
	}
	for i := 0; i < 12; i++ { // 12 笔订单:9 笔 Solana、3 笔 TRON
		chain := "solana"
		if i%4 == 3 {
			chain = "tron"
		}
		l.Deposits = append(l.Deposits, reward.Deposit{TS: now - int64(i+1)*3600, Kind: "order", Chain: chain, Amount: 9.9, From: rwAddrOther, Tx: "o" + string(rune('a'+i))})
	}
	for i, d := range []struct {
		name string
		amt  float64
	}{{"老王", 50}, {"night_owl", 20}, {"老王", 30}, {"", 6}} {
		l.Deposits = append(l.Deposits, reward.Deposit{TS: now - int64(i+1)*5000, Kind: "donation", Chain: "solana", Amount: d.amt, From: rwAddrOther, Tx: "d" + string(rune('a'+i)), Name: d.name})
	}
	l.Pool = reward.PoolSum{Total: 153.52, OrderCount: 12, OrderAmount: 118.8, OrderShare: 47.52, DonateCount: 4, DonateAmount: 106, Points: 30, Addresses: 14}
	// 14 个地址:13 个别人的(其中 1 个被剔除)+ 我(2 分,排在最后)
	alpha := "123456789ABCDEFGHJKLMNPQRSTUVWXYZabcdefghijkmnopqrstuvwxyz"
	for i := 0; i < 13; i++ {
		a := strings.Repeat("1", 31) + string(alpha[i+1]) // 31 个 0 字节 + 1 个小字节:合法的 32 字节地址
		row := reward.BoardRow{Address: a, Points: int64(20 - i), Panels: 1 + i%2}
		if i == 4 {
			row.Excluded, row.Reason = true, "同一网段下成批登记"
		}
		l.Board = append(l.Board, row)
	}
	l.Board = append(l.Board, reward.BoardRow{Address: rwAddrMe, Points: 2, Panels: 1})
	return l
}

func rewardTestServer(t *testing.T) (*Server, *runner.Runner, *fakeRegistry, func(method, path, body string) (int, map[string]json.RawMessage)) {
	t.Helper()
	reg := &fakeRegistry{credited: 2}
	now := time.Now().Unix()
	reg.ledger = func() reward.Ledger { return rwLedger(now) }
	mux := http.NewServeMux()
	mux.HandleFunc("/v1/report", func(w http.ResponseWriter, r *http.Request) {
		var rep reward.Report
		_ = json.NewDecoder(r.Body).Decode(&rep)
		reg.mu.Lock()
		defer reg.mu.Unlock()
		if reg.reject != "" || !rep.Verify() {
			msg := reg.reject
			if msg == "" {
				msg = "签名不对"
			}
			w.WriteHeader(http.StatusBadRequest)
			_ = json.NewEncoder(w).Encode(map[string]string{"error": msg})
			return
		}
		reg.reports = append(reg.reports, rep)
		gb := rep.GB
		if reg.gbFloor > gb {
			gb = reg.gbFloor
		}
		_ = json.NewEncoder(w).Encode(reward.ReportReply{GB: gb, Credited: reg.credited})
	})
	mux.HandleFunc("/v1/export", func(w http.ResponseWriter, r *http.Request) {
		reg.mu.Lock()
		reg.exports++
		reg.mu.Unlock()
		_ = json.NewEncoder(w).Encode(reg.ledger())
	})
	mux.HandleFunc("/raw/ledger.json", func(w http.ResponseWriter, r *http.Request) {
		reg.mu.Lock()
		reg.raws++
		reg.mu.Unlock()
		_ = json.NewEncoder(w).Encode(reg.ledger())
	})
	mux.HandleFunc("/v1/donate", func(w http.ResponseWriter, r *http.Request) {
		var p struct {
			Name   string  `json:"name"`
			Amount float64 `json:"amount"`
		}
		_ = json.NewDecoder(r.Body).Decode(&p)
		reg.mu.Lock()
		defer reg.mu.Unlock()
		reg.intent = reward.Intent{Id: "i1", Name: p.Name, Amount: p.Amount, Exact: p.Amount + 0.03, Chain: "solana", Address: rwAddrOther, CreatedAt: now, ExpiresAt: now + 3600, Status: "pending"}
		_ = json.NewEncoder(w).Encode(reg.intent)
	})
	mux.HandleFunc("/v1/donate/i1", func(w http.ResponseWriter, r *http.Request) {
		reg.mu.Lock()
		defer reg.mu.Unlock()
		_ = json.NewEncoder(w).Encode(reg.intent)
	})
	mux.HandleFunc("/v1/donate/i1/cancel", func(w http.ResponseWriter, r *http.Request) {
		reg.mu.Lock()
		reg.intent.Status = "cancelled"
		reg.mu.Unlock()
		_, _ = w.Write([]byte(`{}`))
	})
	srv := httptest.NewServer(mux)
	t.Cleanup(srv.Close)
	oldAPI, oldURLs := runner.RewardAPI, runner.RewardLedgerURLs
	runner.RewardAPI, runner.RewardLedgerURLs = srv.URL+"/v1", []string{srv.URL + "/raw/ledger.json"}
	t.Cleanup(func() { runner.RewardAPI, runner.RewardLedgerURLs = oldAPI, oldURLs })

	run, err := runner.New(filepath.Join(t.TempDir(), "reward.db"))
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { database.Close(run.DB()) })
	s := NewServer(run)
	do := func(method, path, body string) (int, map[string]json.RawMessage) {
		t.Helper()
		w := httptest.NewRecorder()
		var rd *strings.Reader
		if body != "" {
			rd = strings.NewReader(body)
		} else {
			rd = strings.NewReader("")
		}
		s.handleReward(w, httptest.NewRequest(method, "http://x/app/api/reward"+path, rd))
		out := map[string]json.RawMessage{}
		_ = json.Unmarshal(w.Body.Bytes(), &out)
		return w.Code, out
	}
	return s, run, reg, do
}

func rwStr(t *testing.T, m map[string]json.RawMessage, k string) string {
	t.Helper()
	var s string
	_ = json.Unmarshal(m[k], &s)
	return s
}

func rwNum(t *testing.T, m map[string]json.RawMessage, k string) int64 {
	t.Helper()
	var n float64
	_ = json.Unmarshal(m[k], &n)
	return int64(n)
}

func TestRewardSummaryAndBind(t *testing.T) {
	_, run, reg, do := rewardTestServer(t)
	db := run.DB()

	// 刚装好:0 流量、没登记 → 提醒 fresh;只读公开账本,不碰登记服务
	code, m := do("GET", "", "")
	if code != 200 || string(m["enabled"]) != "true" || rwNum(t, m, "bytes") != 0 || rwStr(t, m, "remind") != "fresh" || rwStr(t, m, "address") != "" {
		t.Fatalf("刚装好的概况: %d %v", code, mapStr(m))
	}
	var pool struct {
		Total     float64
		OrderRate float64
		Chains    []reward.Chain
		Points    int64
		LedgerURL string `json:"ledgerUrl"`
	}
	_ = json.Unmarshal(m["pool"], &pool)
	if pool.Total != 153.52 || pool.OrderRate != 0.4 || len(pool.Chains) != 2 || pool.Points != 30 || !strings.HasSuffix(pool.LedgerURL, "/tree/rewards") {
		t.Fatalf("奖池: %+v", pool)
	}
	reg.mu.Lock()
	if reg.exports != 0 || reg.raws != 1 || len(reg.reports) != 0 {
		t.Fatalf("没登记地址时只读公开账本: exports=%d raws=%d reports=%d", reg.exports, reg.raws, len(reg.reports))
	}
	reg.mu.Unlock()

	// 收起提醒 → 不再提醒;流量满 1 TB → 再提醒(tb)
	if code, _ := do("POST", "/remind", ""); code != 200 {
		t.Fatal("收起提醒")
	}
	if _, m = do("GET", "", ""); rwStr(t, m, "remind") != "" {
		t.Fatalf("收起之后不该再提醒: %s", m["remind"])
	}
	_ = model.AddRewardBytes(db, 3*reward.TB+5<<30, time.Now().Unix())
	if _, m = do("GET", "", ""); rwStr(t, m, "remind") != "tb" || rwNum(t, m, "points") != 3 {
		t.Fatalf("又满了 1 TB 要再提醒: %v", mapStr(m))
	}

	// 登记:地址写错的拦下;写对的向登记服务报到(带签名,累计值取整到 GB)
	if code, m = do("POST", "/bind", `{"address":"not-an-address"}`); code != 400 || !strings.Contains(rwStr(t, m, "error"), "Solana") {
		t.Fatalf("地址写错: %d %v", code, mapStr(m))
	}
	if code, m = do("POST", "/bind", `{"address":"`+rwAddrMe+`"}`); code != 200 || rwStr(t, m, "address") != rwAddrMe || rwStr(t, m, "remind") != "" || rwNum(t, m, "credited") != 2 {
		t.Fatalf("登记: %d %v", code, mapStr(m))
	}
	reg.mu.Lock()
	if len(reg.reports) != 1 || reg.reports[0].Address != rwAddrMe || reg.reports[0].GB != 3*1024+5 || !reg.reports[0].Verify() {
		t.Fatalf("登记时的上报: %+v", reg.reports)
	}
	pub := reg.reports[0].Pub
	reg.mu.Unlock()
	if strings.Contains(string(mustJSON(m)), "rewardKey") || run.DB().Where("key = ?", "rewardKey").First(&model.Setting{}).Error != nil {
		t.Fatal("面板密钥要存下来,但不出现在接口里")
	}

	// 登记处记着的累计值比本机大(本机是接任 / 还原回来的):按它往上接
	reg.mu.Lock()
	reg.gbFloor = 9 * 1024
	reg.mu.Unlock()
	run.RewardKick()
	if err := run.RewardBind(rwAddrOther); err != nil { // 换地址也会马上报一次
		t.Fatal(err)
	}
	if got := model.RewardBytes(db); got != 9*reward.TB {
		t.Fatalf("按登记处的往上接: %d", got)
	}
	reg.mu.Lock()
	last := reg.reports[len(reg.reports)-1]
	if last.Pub != pub || last.Address != rwAddrOther {
		t.Fatalf("换地址之后还是同一个面板: %+v", last)
	}
	// 登记服务拒绝(比如还在冷却期):原因给管理员,本机保持原来的地址
	reg.reject = "换地址还在冷却期,7 天后再试"
	reg.mu.Unlock()
	if code, m = do("POST", "/bind", `{"address":"`+rwAddrMe+`"}`); code != 400 || !strings.Contains(rwStr(t, m, "error"), "冷却期") {
		t.Fatalf("登记服务拒绝的: %d %v", code, mapStr(m))
	}
	if _, m = do("GET", "", ""); rwStr(t, m, "address") != rwAddrOther {
		t.Fatalf("被拒之后保持原来的地址: %s", m["address"])
	}

	// 设置里关掉:只回 enabled=false,别的不给
	_ = run.SetSetting("rewardEnabled", "false")
	if code, m = do("GET", "", ""); code != 200 || string(m["enabled"]) != "false" || len(m) != 1 {
		t.Fatalf("关掉之后: %d %v", code, mapStr(m))
	}
	if code, _ = do("GET", "/ledger", ""); code != 400 {
		t.Fatalf("关掉之后别的接口不给: %d", code)
	}
	_ = run.SetSetting("rewardEnabled", "true")
	_ = run.SetSetting("nodeMode", "true") // 副机上也一样
	if _, m = do("GET", "", ""); string(m["enabled"]) != "false" {
		t.Fatalf("副机上不显示: %v", mapStr(m))
	}
}

func TestRewardListsAndDonation(t *testing.T) {
	_, run, reg, do := rewardTestServer(t)
	_ = model.AddRewardBytes(run.DB(), 2*reward.TB, time.Now().Unix())
	if err := run.RewardBind(rwAddrMe); err != nil {
		t.Fatal(err)
	}
	type list struct {
		Items []map[string]interface{} `json:"items"`
		Total int                      `json:"total"`
		Top   float64                  `json:"top"`
		Named struct {
			Donors, Count int
			Amount        float64
		} `json:"named"`
		Anon struct {
			Count  int
			Amount float64
		} `json:"anon"`
	}
	get := func(path string) list {
		t.Helper()
		code, m := do("GET", path, "")
		if code != 200 {
			t.Fatalf("GET %s: %d", path, code)
		}
		var l list
		_ = json.Unmarshal(mustJSON(m), &l)
		return l
	}
	// 逐笔入账:16 笔,新的在前,每页十条;订单按 40% 计入、捐赠全额;能按类型和链筛
	l := get("/ledger")
	if l.Total != 16 || len(l.Items) != 10 || l.Items[0]["kind"] != "order" || l.Items[0]["share"].(float64) != 3.96 {
		t.Fatalf("逐笔入账第一页: total=%d n=%d first=%v", l.Total, len(l.Items), l.Items[0])
	}
	if l = get("/ledger?page=2"); len(l.Items) != 6 {
		t.Fatalf("第二页: %d", len(l.Items))
	}
	if l = get("/ledger?kind=donation"); l.Total != 4 || l.Items[0]["share"].(float64) != l.Items[0]["amount"].(float64) || l.Items[0]["name"] != "老王" {
		t.Fatalf("按捐赠筛: %+v", l.Items)
	}
	if l = get("/ledger?kind=order&chain=tron"); l.Total != 3 {
		t.Fatalf("按链筛: %d", l.Total)
	}
	// 积分榜:没被剔除的按积分排,我(2 分,第 13 名)钉在第一页最上面;被剔除的排在最后、带原因;能按地址查
	b := get("/board")
	if b.Total != 14 || len(b.Items) != 10 || b.Items[0]["me"] != true || b.Items[0]["rank"].(float64) != 13 || b.Items[1]["rank"].(float64) != 1 || b.Top != 20 {
		t.Fatalf("积分榜第一页: total=%d first=%v second=%v top=%v", b.Total, b.Items[0], b.Items[1], b.Top)
	}
	b2 := get("/board?page=2")
	lastRow := b2.Items[len(b2.Items)-1]
	if lastRow["excluded"] != true || lastRow["reason"] != "同一网段下成批登记" {
		t.Fatalf("被剔除的排在最后并写明原因: %v", lastRow)
	}
	if b = get("/board?q=7xkxt"); b.Total != 1 || b.Items[0]["address"] != rwAddrMe {
		t.Fatalf("按地址查(不分大小写): %+v", b.Items)
	}
	// 捐赠榜:留名的按合计排(老王两笔 80),匿名的只给笔数和合计
	d := get("/donors")
	if d.Total != 2 || d.Items[0]["name"] != "老王" || d.Items[0]["amount"].(float64) != 80 || d.Items[0]["count"].(float64) != 2 || d.Named.Donors != 2 || d.Named.Count != 3 || d.Named.Amount != 100 || d.Anon.Count != 1 || d.Anon.Amount != 6 {
		t.Fatalf("捐赠榜: %+v", d)
	}
	// 登记了地址的面板向登记服务要账本(比分支上的新)
	reg.mu.Lock()
	if reg.exports == 0 {
		t.Fatal("登记了地址之后账本应当从登记服务取")
	}
	reg.mu.Unlock()

	// 留名捐赠:名字、金额不对的拦下;提交后拿到要转的确切金额;到账后状态变成 paid;取消 / 收起后清掉
	if code, m := do("POST", "/donate", `{"name":"","amount":20}`); code != 400 || !strings.Contains(rwStr(t, m, "error"), "留名") {
		t.Fatalf("没填留名: %d %v", code, mapStr(m))
	}
	if code, _ := do("POST", "/donate", `{"name":"x","amount":0.5}`); code != 400 {
		t.Fatalf("金额太小: %d", code)
	}
	if code, _ := do("POST", "/donate", `{"name":"x","amount":1.234}`); code != 400 {
		t.Fatalf("三位小数: %d", code)
	}
	code, m := do("POST", "/donate", `{"name":"  半夜 修机器  ","amount":20}`)
	var it reward.Intent
	_ = json.Unmarshal(m["donation"], &it)
	if code != 200 || it.Name != "半夜 修机器" || it.Exact != 20.03 || it.Status != "pending" || it.Address != rwAddrOther {
		t.Fatalf("提交留名捐赠: %d %+v", code, it)
	}
	reg.mu.Lock()
	reg.intent.Status = "paid"
	reg.mu.Unlock()
	run.RewardDonateRefresh() // 有间隔限制:刚问过就不再问
	if st := run.RewardState(); st.Donation == nil || st.Donation.Status != "pending" {
		t.Fatalf("刚提交完不该马上又去问: %+v", st.Donation)
	}
	if code, m = do("POST", "/donate/cancel", ""); code != 200 || string(m["donation"]) != "null" {
		t.Fatalf("取消: %d %s", code, m["donation"])
	}
	reg.mu.Lock()
	if reg.intent.Status != "cancelled" {
		t.Fatalf("取消要告诉登记服务把金额让出来: %s", reg.intent.Status)
	}
	reg.mu.Unlock()

	// 二维码:PNG
	w := httptest.NewRecorder()
	(&Server{}).handleRewardQRForTest(t, run, w)
	if w.Code != 200 || w.Header().Get("Content-Type") != "image/png" || w.Body.Len() < 200 {
		t.Fatalf("二维码: %d %s %d 字节", w.Code, w.Header().Get("Content-Type"), w.Body.Len())
	}
}

func (s *Server) handleRewardQRForTest(t *testing.T, run *runner.Runner, w *httptest.ResponseRecorder) {
	t.Helper()
	NewServer(run).handleReward(w, httptest.NewRequest("GET", "http://x/app/api/reward/qr?chain=solana", nil))
}

func mapStr(m map[string]json.RawMessage) string { return string(mustJSON(m)) }

func mustJSON(v interface{}) []byte {
	b, _ := json.Marshal(v)
	return b
}
