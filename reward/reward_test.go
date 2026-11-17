package reward

import (
	"context"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
	"time"
)

func TestValidSolana(t *testing.T) {
	for _, a := range []string{
		"11111111111111111111111111111111",             // 系统程序:32 个 0 字节
		"Es9vMFrzaCERmJfrF4H2FYD4KCoNkY11McCe8BenwNYB", // USDT 的铸币地址
		"7xKXtg2CW87d97TXJSDpbD5jBkheTqA83TZRuJosgAsU",
		"So11111111111111111111111111111111111111112",
	} {
		if !ValidSolana(a) {
			t.Errorf("%s 是合法的 Solana 地址", a)
		}
	}
	for _, a := range []string{
		"", "abc", "0x52908400098527886E0F7030069857D2E4169EE7",
		"Es9vMFrzaCERmJfrF4H2FYD4KCoNkY11McCe8Benw0YB",  // 有 0
		"Es9vMFrzaCERmJfrF4H2FYD4KCoNkY11McCe8BenwNYBl", // 有 l,而且太长
		"TDoUMF4nF244o5GZvBBwX5t9axvnSoP1Cm",            // TRON 地址:解出来不是 32 字节
		"1111111111111111111111111111111",               // 31 个 0 字节
		"DEMopoo1So1anaAddr0NotRea1xxxxxxxxxxxxxxxxxx",  // 演示站用的占位地址:故意无效
	} {
		if ValidSolana(a) {
			t.Errorf("%q 不该算合法", a)
		}
	}
}

func TestReportSignature(t *testing.T) {
	k, err := NewKey()
	if err != nil {
		t.Fatal(err)
	}
	priv, err := ParseKey(k)
	if err != nil {
		t.Fatal(err)
	}
	r := NewReport(priv, "7xKXtg2CW87d97TXJSDpbD5jBkheTqA83TZRuJosgAsU", 5*TB+123, 1794880000)
	if r.GB != 5*1024 || r.V != 1 || r.Pub != PubID(priv) || !r.Verify() {
		t.Fatalf("上报不对: %+v", r)
	}
	for _, bad := range []func(*Report){
		func(x *Report) { x.GB++ }, func(x *Report) { x.TS++ }, func(x *Report) { x.Address = "11111111111111111111111111111111" },
		func(x *Report) { other, _ := NewKey(); p, _ := ParseKey(other); x.Pub = PubID(p) },
	} {
		x := r
		bad(&x)
		if x.Verify() {
			t.Fatalf("改过内容的上报不该验得过: %+v", x)
		}
	}
	if got := Canonical("P", "A", 7, 9); got != "m-ui-reward-v1\nP\nA\n7\n9" {
		t.Fatalf("被签名的文字变了,登记服务那边要跟着改: %q", got)
	}
	if _, err := ParseKey("not-a-key"); err == nil {
		t.Fatal("读不出来的密钥要报错")
	}
}

func TestCleanName(t *testing.T) {
	ok := map[string]string{"  老王 ": "老王", "night_owl": "night_owl", "a   b\tc": "a b c", "二十个字二十个字二十个字二十个字二十个字": "二十个字二十个字二十个字二十个字二十个字"}
	for in, want := range ok {
		if got, err := CleanName(in); err != nil || got != want {
			t.Errorf("CleanName(%q) = %q, %v,应为 %q", in, got, err, want)
		}
	}
	for _, in := range []string{"", "   ", "二十一个字二十一个字二十一个字二十一个字啊", "see https://x.example", "www.spam.example", "a‮b", "x​y"} {
		if _, err := CleanName(in); err == nil {
			t.Errorf("CleanName(%q) 应当被拒", in)
		}
	}
}

func goodLedger(now int64) Ledger {
	return Ledger{V: 1, UpdatedAt: now - 60, PayoutAt: now + 86400, OrderRate: 0.4,
		Pool:   PoolSum{Total: 23.96, OrderCount: 2, OrderAmount: 39.9, OrderShare: 15.96, DonateCount: 1, DonateAmount: 8, Points: 3, Addresses: 2},
		Chains: []Chain{{Chain: "solana", Address: "7xKXtg2CW87d97TXJSDpbD5jBkheTqA83TZRuJosgAsU", Received: 37.9}, {Chain: "tron", Address: "TDoUMF4nF244o5GZvBBwX5t9axvnSoP1Cm", Received: 10}},
		Donate: DonateInfo{Chain: "solana", Address: "7xKXtg2CW87d97TXJSDpbD5jBkheTqA83TZRuJosgAsU", Min: 1, Max: 100000},
		Deposits: []Deposit{
			{TS: now - 100, Kind: "order", Chain: "solana", Amount: 29.9, From: "Es9vMFrzaCERmJfrF4H2FYD4KCoNkY11McCe8BenwNYB", Tx: "sig1"},
			{TS: now - 200, Kind: "order", Chain: "tron", Amount: 10, From: "TDoUMF4nF244o5GZvBBwX5t9axvnSoP1Cm", Tx: "tx2"},
			{TS: now - 300, Kind: "donation", Chain: "solana", Amount: 8, From: "Es9vMFrzaCERmJfrF4H2FYD4KCoNkY11McCe8BenwNYB", Tx: "sig3", Name: "老王"},
		},
		Board: []BoardRow{{Address: "7xKXtg2CW87d97TXJSDpbD5jBkheTqA83TZRuJosgAsU", Points: 2, Panels: 1}, {Address: "Es9vMFrzaCERmJfrF4H2FYD4KCoNkY11McCe8BenwNYB", Points: 1, Panels: 2}},
	}
}

func TestLedgerCheckAndShare(t *testing.T) {
	now := time.Now().Unix()
	l := goodLedger(now)
	if err := l.check(now); err != nil {
		t.Fatal(err)
	}
	if got := l.Share(l.Deposits[0]); got != 11.96 {
		t.Fatalf("订单按比例计入: %v", got)
	}
	if got := l.Share(l.Deposits[2]); got != 8 {
		t.Fatalf("捐赠全额计入: %v", got)
	}
	// 人工确认发放的订单(可以没有交易号)、订单取消后作废的入账:认;作废的不计入
	marked := goodLedger(now)
	marked.Deposits = append(marked.Deposits,
		Deposit{TS: now - 400, Kind: "order", Chain: "tron", Amount: 18.003708, Note: "manual"},
		Deposit{TS: now - 500, Kind: "order", Chain: "solana", Amount: 87.305142, From: "Es9vMFrzaCERmJfrF4H2FYD4KCoNkY11McCe8BenwNYB", Tx: "sig9", Note: "manual"},
		Deposit{TS: now - 600, Kind: "order", Chain: "tron", Amount: 18.007208, From: "TDoUMF4nF244o5GZvBBwX5t9axvnSoP1Cm", Tx: "tx7", Void: true})
	if err := marked.check(now); err != nil {
		t.Fatalf("带标记的账本应通过: %v", err)
	}
	if got := marked.Share(marked.Deposits[4]); got != 34.92 {
		t.Fatalf("人工确认的订单照样按比例计入: %v", got)
	}
	if got := marked.Share(marked.Deposits[5]); got != 0 {
		t.Fatalf("作废的不计入: %v", got)
	}
	for name, bad := range map[string]func(*Ledger){
		"版本":    func(x *Ledger) { x.V = 2 },
		"未来":    func(x *Ledger) { x.UpdatedAt = now + 86400 },
		"比例":    func(x *Ledger) { x.OrderRate = 1.5 },
		"链":     func(x *Ledger) { x.Chains[0].Chain = "eth" },
		"收款地址":  func(x *Ledger) { x.Chains[1].Address = "not-a-tron-address" },
		"没留名":   func(x *Ledger) { x.Deposits[2].Name = "" },
		"类型":    func(x *Ledger) { x.Deposits[0].Kind = "gift" },
		"留名太长":  func(x *Ledger) { x.Deposits[2].Name = strings.Repeat("字", 30) },
		"没交易号":  func(x *Ledger) { x.Deposits[0].Tx = "" },
		"标记":    func(x *Ledger) { x.Deposits[0].Note = "whatever" },
		"捐赠标人工": func(x *Ledger) { x.Deposits[2].Note = "manual" },
		"捐赠作废":  func(x *Ledger) { x.Deposits[2].Void = true },
		"榜单地址":  func(x *Ledger) { x.Board[0].Address = "bad" },
	} {
		x := goodLedger(now)
		bad(&x)
		if x.check(now) == nil {
			t.Errorf("写坏的账本(%s)不该通过", name)
		}
	}
}

func TestClient(t *testing.T) {
	now := time.Now().Unix()
	var gotReport Report
	mux := http.NewServeMux()
	mux.HandleFunc("/v1/report", func(w http.ResponseWriter, r *http.Request) {
		_ = json.NewDecoder(r.Body).Decode(&gotReport)
		if !gotReport.Verify() {
			w.WriteHeader(400)
			_, _ = w.Write([]byte(`{"error":"签名不对"}`))
			return
		}
		_ = json.NewEncoder(w).Encode(ReportReply{GB: gotReport.GB + 5, Credited: 2})
	})
	mux.HandleFunc("/v1/donate", func(w http.ResponseWriter, r *http.Request) {
		_ = json.NewEncoder(w).Encode(Intent{Id: "abc", Name: "老王", Amount: 20, Exact: 20.03, Chain: "solana", Address: "7xKXtg2CW87d97TXJSDpbD5jBkheTqA83TZRuJosgAsU", CreatedAt: now, ExpiresAt: now + 3600, Status: "pending"})
	})
	mux.HandleFunc("/v1/donate/abc", func(w http.ResponseWriter, r *http.Request) {
		_ = json.NewEncoder(w).Encode(Intent{Id: "abc", Exact: 20.03, Status: "paid"})
	})
	mux.HandleFunc("/v1/donate/gone", func(w http.ResponseWriter, r *http.Request) {
		w.WriteHeader(404)
		_, _ = w.Write([]byte(`{"error":"没有这一笔"}`))
	})
	mux.HandleFunc("/v1/export", func(w http.ResponseWriter, r *http.Request) { _ = json.NewEncoder(w).Encode(goodLedger(now)) })
	mux.HandleFunc("/bad.json", func(w http.ResponseWriter, r *http.Request) { _, _ = w.Write([]byte(`{"v":1}`)) })
	mux.HandleFunc("/ledger.json", func(w http.ResponseWriter, r *http.Request) { _ = json.NewEncoder(w).Encode(goodLedger(now)) })
	srv := httptest.NewServer(mux)
	defer srv.Close()
	c := &Client{API: srv.URL + "/v1/", LedgerURLs: []string{srv.URL + "/missing.json", srv.URL + "/bad.json", srv.URL + "/ledger.json"}}
	ctx := context.Background()

	k, _ := NewKey()
	priv, _ := ParseKey(k)
	rep, err := c.Report(ctx, NewReport(priv, "7xKXtg2CW87d97TXJSDpbD5jBkheTqA83TZRuJosgAsU", 3*TB, now))
	if err != nil || rep.GB != 3*1024+5 || rep.Credited != 2 {
		t.Fatalf("上报: %+v %v", rep, err)
	}
	bad := NewReport(priv, "7xKXtg2CW87d97TXJSDpbD5jBkheTqA83TZRuJosgAsU", 3*TB, now)
	bad.GB++
	if _, err := c.Report(ctx, bad); err == nil || StatusOf(err) != 400 || err.Error() != "签名不对" {
		t.Fatalf("登记服务拒绝的要把原因带回来: %v", err)
	}
	it, err := c.Donate(ctx, "老王", 20)
	if err != nil || it.Exact != 20.03 || it.Id != "abc" {
		t.Fatalf("留名捐赠: %+v %v", it, err)
	}
	if st, err := c.DonateStatus(ctx, "abc"); err != nil || st.Status != "paid" {
		t.Fatalf("查到账: %+v %v", st, err)
	}
	if _, err := c.DonateStatus(ctx, "gone"); StatusOf(err) != 404 {
		t.Fatalf("没有这一笔: %v", err)
	}
	l, err := c.Export(ctx)
	if err != nil || len(l.Deposits) != 3 {
		t.Fatalf("向登记服务要账本: %v", err)
	}
	l, err = c.FetchLedger(ctx) // 前两个地址一个 404、一个写坏了,用第三个
	if err != nil || l.Pool.Total != 23.96 {
		t.Fatalf("读公开账本: %+v %v", l, err)
	}
	c.LedgerURLs = c.LedgerURLs[:2]
	if _, err := c.FetchLedger(ctx); err == nil {
		t.Fatal("都取不到要报错")
	}
	if StatusOf(context.DeadlineExceeded) != 0 {
		t.Fatal("不是登记服务答复的错,状态码是 0")
	}
}

// 登记服务的错带代号:认识的代号换成面板自己的说法(英文界面靠它有译文),不认识的原样显示登记服务写的那句话。
func TestRegistryErrorCodes(t *testing.T) {
	reply := ""
	status := 400
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.WriteHeader(status)
		_, _ = w.Write([]byte(reply))
	}))
	defer srv.Close()
	c := &Client{API: srv.URL + "/v1"}
	for _, tc := range []struct {
		status int
		reply  string
		want   string
	}{
		{400, `{"error":"换地址还在冷却期,6 天后再试","code":"cooldown","days":6}`, "换领取地址有 7 天的冷却期,还要等 6 天"},
		{400, `{"error":"x","code":"cooldown"}`, "换领取地址有 7 天的冷却期,还要等 1 天"},
		{429, `{"error":"这处来源今天登记的面板太多了,明天再试","code":"busy_src"}`, "这台服务器所在的来源今天登记的面板太多了,明天再试"},
		{400, `{"error":"签名不对","code":"sig"}`, "登记服务不认这次请求,请把面板升到最新版再试"},
		{503, `{"error":"这个金额现在排不开,换个数再试","code":"no_slot"}`, "这个金额现在排不开,换个数再试"},
		{400, `{"error":"以后才有的新规矩","code":"from_the_future"}`, "以后才有的新规矩"},
		{400, `{"error":"没带代号的老写法"}`, "没带代号的老写法"},
		{502, `<html>bad gateway</html>`, "登记服务答复 502"},
	} {
		status, reply = tc.status, tc.reply
		_, err := c.Donate(context.Background(), "老王", 20)
		if err == nil || err.Error() != tc.want || StatusOf(err) != tc.status {
			t.Fatalf("%s → %v (状态 %d),想要 %q", tc.reply, err, StatusOf(err), tc.want)
		}
	}
	// 被剔除的答复带规则代号
	status, reply = 200, `{"gb":5,"credited":1,"excluded":{"rule":"same_src","reason":"这处来源上已经有别的地址在登记","at":123}}`
	k, _ := NewKey()
	priv, _ := ParseKey(k)
	rep, err := c.Report(context.Background(), NewReport(priv, "7xKXtg2CW87d97TXJSDpbD5jBkheTqA83TZRuJosgAsU", TB, time.Now().Unix()))
	if err != nil || rep.Excluded == nil || rep.Excluded.Rule != "same_src" || rep.Excluded.At != 123 {
		t.Fatalf("被剔除的答复: %+v %v", rep, err)
	}
}
