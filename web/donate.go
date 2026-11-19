package web

import (
	"context"
	"encoding/json"
	"fmt"
	"io"
	"math"
	"math/rand"
	"net/http"
	"regexp"
	"strconv"
	"strings"
	"sync"
	"time"
	"unicode"

	"github.com/Maoyangui/m-ui/brand"
	"github.com/Maoyangui/m-ui/database/model"
	"github.com/Maoyangui/m-ui/safe"
)

// 功德箱:"建议与支持"页上留名 + 转 USDT,到账后所有面板都能看到累计排行。
//
// 没有自己的服务端,数据全在公开的地方:
//   - 留名经 ntfy.sh(免费的公开消息接口)发出去,只有点"留名"时才发
//   - GitHub Actions 定时把留名和 TronGrid 查到的到账对上,结果写进本仓库 donors 分支的 donors.json(deploy/donors);
//     GitHub 的定时任务常常晚几个小时,所以到账后要过几个小时才上榜
//   - 各面板打开这页时读 donors.json,最多一小时取一次;raw 和 jsDelivr 同时取,先到的合法名单先用,
//     两份都到了留新的那份(jsDelivr 上的可能旧几个小时)。取不到就不显示,页面其它部分照常
//
// 名单里只有名字和累计金额。谁留的名、从哪台机器留的,都不记。
// 设置里的总开关(supportOnline,默认开)关掉后,这里不再发任何外部请求,页面上也不显示。

// 这几个地址测试里要换。
var (
	donorsURLs = []string{
		"https://raw.githubusercontent.com/" + brand.RepoPath + "/donors/donors.json",
		"https://cdn.jsdelivr.net/gh/" + brand.RepoPath + "@donors/donors.json", // raw 被墙或卡住的机器走它(分支文件在它那里最多缓存 12 小时)
	}
	pledgeURL = "https://ntfy.sh/" + brand.DonateTopic
	tronURL   = "https://api.trongrid.io"
)

const (
	usdtContract = "TR7NHqjeKQxGTCi8q8ZY4pL8otSzgjLj6t"
	pledgeWindow = int64(3600) // 留名后多久内到账才算(deploy/donors/settle.mjs 的 WINDOW)
	// 页面上问「到了没有」时,到账的链上时间最多可以比留名时间早这么多:留名时间按这台面板的钟记,钟快了几分钟的机器上,
	// 马上转出的那一笔会显得「在留名之前」,页面就一直说没到账(上榜不受影响:对账那边按 ntfy 收到留名的时间算)。
	// 金额带着随机零头,十分钟里正好有另一笔一模一样的,可以不计。
	pledgeSkew = int64(600)
	tronPage   = 200 // 向 TronGrid 一次要多少条(它一页最多这么多)
	// 有人在等到账时,多久可以问一次 TronGrid。页面 5 秒问一次面板,这里要比它短一点:两边一样长的话,页面来问时离上一次
	// 查完常常还差几毫秒,轮空一次,实际成了 5 秒、10 秒交替着查(实机测出来的)。TronGrid 不带密钥的限额是每秒 1 次
	// (超了停 5 秒),留足了余量;不管多少人在等、问的是哪一笔,大家共用这一次查询。
	tronEvery = 4 * time.Second
	// 这一次没查成(限速、超时)或者正在查:上一次查成的结果在这么久之内照样拿来答。免费接口偶尔限速一下是常事,
	// 不该让页面一会儿「知道」一会儿「不知道」;超过这么久还没有新的结果,才老实说不知道。
	tronGoodFor = 30 * time.Second
	// 连着没查成:从 tronBackoff 起每次把间隔加倍(10、20、40 秒),最长 tronBackoffMax —— 被限速时越敲越久。
	tronBackoff     = 5 * time.Second
	tronBackoffMax  = 60 * time.Second
	donorsTTL       = time.Hour // 名单最多一小时取一次
	donorsWaitAge   = 6 * time.Hour
	donorsWait      = 3 * time.Second  // 手上没有名单或比 donorsWaitAge 还旧时,打开页面最多等这么久拿新的
	donorsBudget    = 12 * time.Second // 一轮最多取这么久
	donorsRetry     = 5 * time.Minute  // 都取不到:过这么久再试,别每次打开都等
	devicesFresh    = int64(48 * 3600) // 面板数这么久没更新(deploy/donors 停了或读不到下载计数)就不显示
	boardClockSkew  = int64(10 * 60)   // 名单里的时间最多比本机晚这么多还算可信(两边时钟的误差);再晚的是写错了,不拿来比新旧
	donorsMaxBytes  = 512 << 10
	donorsMaxRows   = 500
	donorNameMax    = 24
	pledgeMinUSDT   = 1.0
	pledgeMaxUSDT   = 100000.0
	pledgeMaxMicros = int64(100_000_100_000) // 要转的金额上限:100000 再加零头;deploy/donors/settle.mjs 的 MAX_MICROS 与它一致
)

type donorRow struct {
	Name  string  `json:"name"`
	Total float64 `json:"total"`
}

type donorBoard struct {
	UpdatedAt int64      `json:"updatedAt"`
	Devices   int        `json:"devices"`   // 过去 24 小时有多少台面板在运行(见 deploy/donors)
	DevicesAt int64      `json:"devicesAt"` // 面板数是什么时候算出来的;没有或太旧就不显示
	Donors    []donorRow `json:"donors"`
}

// stamp 名单是什么时候的:UpdatedAt 和 DevicesAt 里较大的那个。比本机现在晚出时钟误差(boardClockSkew)的时间
// 是写错了,不可信,不算;两个都不可信(或都没有)就是 0,不知道。
func (b donorBoard) stamp(now int64) int64 {
	s := int64(0)
	for _, t := range []int64{b.UpdatedAt, b.DevicesAt} {
		if t <= now+boardClockSkew {
			s = max(s, t)
		}
	}
	return s
}

// olderThan 名单 b 比手上的 cur 旧。jsDelivr 上的分支文件最多旧 12 小时,raw 偶尔没取到时别拿它把手上新的换掉。
// 只比可信的时间:写在未来的不参与比较,一份时间写成几小时后的名单不会让之后这几个小时的正常名单都被拒收;
// 哪一份没有可信的时间就不比(照样换上,也挡不住之后的)。
func (b donorBoard) olderThan(cur donorBoard, now int64) bool {
	bs, cs := b.stamp(now), cur.stamp(now)
	return bs > 0 && cs > 0 && bs < cs
}

// donorState 这台面板手上的一份名单。
type donorState struct {
	mu        sync.Mutex
	board     donorBoard
	got       time.Time          // 最近一次取到合法名单的时间(零值 = 还没取到过)
	next      time.Time          // 什么时候可以再取
	fetching  chan struct{}      // 正在取:拿到第一份合法的名单或这一轮结束时关闭
	rate      map[string][]int64 // 留名限流:来源 → 最近几次的时间
	total     []int64            // 这台面板最近一小时的留名
	tron      []tronTx           // 最近一次查成的:收款地址最近两小时的到账
	tronFull  bool               // 那一页是满的:更早的到账可能没列出来,没找到不能当成"没到账"
	tronGood  time.Time          // 那一次是什么时候查成的(零值 = 还没查成过)
	tronNext  time.Time          // 什么时候可以再问 TronGrid
	tronBusy  bool               // 正在问
	tronFails int                // 连着几次没查成
}

type tronTx struct{ micros, at int64 }

var donorHTTP = &http.Client{Timeout: 10 * time.Second}

// cleanBoard 外面取来的名单再过一遍:名字不合规的不要,行数有上限。页面上用 textContent 显示,这里是第二道。
func cleanBoard(b donorBoard) donorBoard {
	out := donorBoard{UpdatedAt: b.UpdatedAt, Devices: b.Devices, DevicesAt: b.DevicesAt}
	if out.Devices < 0 {
		out.Devices = 0
	}
	for _, d := range b.Donors {
		name, err := cleanDonorName(d.Name)
		if err != "" || d.Total <= 0 || math.IsNaN(d.Total) || math.IsInf(d.Total, 0) {
			continue
		}
		out.Donors = append(out.Donors, donorRow{Name: name, Total: d.Total})
		if len(out.Donors) >= donorsMaxRows {
			break
		}
	}
	return out
}

// visibleDevices 页面上显示的面板数:统计时间没有(老版本写的名单)或超过 48 小时就是 0,页面随之不显示,
// 免得 deploy/donors 停了以后一直挂着几天前的数。按显示时的时间算:取不到新名单时,手上那份也会过期。
func visibleDevices(b donorBoard, now int64) int {
	if b.DevicesAt <= 0 || now-b.DevicesAt > devicesFresh {
		return 0
	}
	return b.Devices
}

func fetchDonorURL(ctx context.Context, u string) (donorBoard, bool) {
	req, err := http.NewRequestWithContext(ctx, http.MethodGet, u, nil)
	if err != nil {
		return donorBoard{}, false
	}
	req.Header.Set("User-Agent", "m-ui")
	resp, err := donorHTTP.Do(req)
	if err != nil {
		return donorBoard{}, false
	}
	body, err := io.ReadAll(io.LimitReader(resp.Body, donorsMaxBytes))
	resp.Body.Close()
	if err != nil || resp.StatusCode != http.StatusOK {
		return donorBoard{}, false
	}
	var b donorBoard
	if json.Unmarshal(body, &b) != nil {
		return donorBoard{}, false
	}
	return cleanBoard(b), true
}

// fetchDonorBoards 几个地址同时取:raw 在有的地方会卡住直到超时,一个接一个取的话兜底的 jsDelivr 就没时间了。
// 每拿到一份合法的名单就交给 got(按到达先后),全部结束(或 ctx 到期)后返回有没有拿到过。
func fetchDonorBoards(ctx context.Context, urls []string, got func(donorBoard)) bool {
	type result struct {
		b  donorBoard
		ok bool
	}
	ch := make(chan result, len(urls))
	for _, u := range urls {
		safe.Go("web.fetchDonorBoards", func() {
			b, ok := fetchDonorURL(ctx, u)
			ch <- result{b, ok}
		})
	}
	gotAny := false
	for range urls {
		if r := <-ch; r.ok {
			gotAny = true
			got(r.b)
		}
	}
	return gotAny
}

// refreshDonors 取一轮名单。先到的合法名单马上换上(等着的人立刻拿到),后到的不比手上旧也换上:
// jsDelivr 的可能比 raw 旧,谁先到都最后留下新的那份。都取不到就过 5 分钟再试,手上那份不动。
func (st *donorState) refreshDonors(urls []string, done chan struct{}) {
	ctx, cancel := context.WithTimeout(context.Background(), donorsBudget)
	defer cancel()
	first := true
	ok := fetchDonorBoards(ctx, urls, func(b donorBoard) {
		now := time.Now()
		st.mu.Lock()
		if st.got.IsZero() || !b.olderThan(st.board, now.Unix()) {
			st.board = b
		}
		st.got, st.next = now, now.Add(donorsTTL)
		st.mu.Unlock()
		if first {
			first = false
			close(done)
		}
	})
	st.mu.Lock()
	if !ok {
		st.next = time.Now().Add(donorsRetry)
	}
	st.fetching = nil
	st.mu.Unlock()
	if first {
		close(done)
	}
}

// donorSnapshot 现有的名单。过了一小时在后台取新的,先给手上这份;
// 手上还没有、或者已经比 6 小时还旧(这页很少有人开),就等新的最多 3 秒,不把很久以前的名单给第一个打开的人看。
func (s *Server) donorSnapshot() donorBoard {
	st := &s.donors
	now := time.Now()
	st.mu.Lock()
	if st.fetching == nil && !now.Before(st.next) {
		st.fetching = make(chan struct{})
		safe.Go2("web.refreshDonors", donorsURLs, st.fetching, st.refreshDonors)
	}
	wait := st.fetching
	if !st.got.IsZero() && now.Sub(st.got) <= donorsWaitAge {
		wait = nil
	}
	board := st.board
	st.mu.Unlock()
	if wait == nil {
		return board
	}
	t := time.NewTimer(donorsWait)
	defer t.Stop()
	select {
	case <-wait:
	case <-t.C:
	}
	st.mu.Lock()
	defer st.mu.Unlock()
	return st.board
}

var domainLike = regexp.MustCompile(`(?i)[a-z0-9-]+\.[a-z]{2,}`)

// cleanDonorName 名字要显示在所有面板上:只收文字、数字、空格和少数标点(. _ - · ' @),不能带链接。返回规整后的名字和错误代码。
// @ 是 v0.8.1 放开的(留 Telegram 用户名用);带域名样子的照旧不收,所以邮箱地址填不了。
// 更早版本的面板不认带 @ 的名字:它们的榜单上不显示那一行,别的行照常。
func cleanDonorName(s string) (string, string) {
	s = strings.Join(strings.Fields(s), " ")
	if s == "" {
		return "", "name_empty"
	}
	if len([]rune(s)) > donorNameMax {
		return "", "name_long"
	}
	for _, r := range s {
		if unicode.IsLetter(r) || unicode.IsNumber(r) || unicode.IsMark(r) || strings.ContainsRune(" ._-·'@", r) {
			continue
		}
		return "", "name_bad"
	}
	if domainLike.MatchString(s) {
		return "", "name_bad"
	}
	return s, ""
}

// payableAmount 用户要转的金额:在填的数上加一点零头(0.001–0.099),同一时间有两个人填了一样的数也分得开。
func payableAmount(amount float64) string {
	cents := math.Round(amount*100) / 100
	return strconv.FormatFloat(cents+float64(1+rand.Intn(99))/1000, 'f', 3, 64)
}

// allowPledge 留名限流:每个来源 10 分钟 5 次,这台面板一小时 30 次。公开接口,不能被拿来刷外部服务。
func (s *Server) allowPledge(peer string, now int64) bool {
	st := &s.donors
	st.mu.Lock()
	defer st.mu.Unlock()
	if st.rate == nil {
		st.rate = map[string][]int64{}
	}
	key := model.IPKey(peer)
	keep := st.rate[key][:0]
	for _, t := range st.rate[key] {
		if t > now-600 {
			keep = append(keep, t)
		}
	}
	all := st.total[:0]
	for _, t := range st.total {
		if t > now-3600 {
			all = append(all, t)
		}
	}
	st.total = all
	if len(keep) >= 5 || len(all) >= 30 {
		st.rate[key] = keep
		return false
	}
	st.rate[key] = append(keep, now)
	st.total = append(st.total, now)
	if len(st.rate) > 2048 {
		for k, v := range st.rate {
			if len(v) == 0 || v[len(v)-1] <= now-600 {
				delete(st.rate, k)
			}
		}
	}
	return true
}

// supportOnline 总开关:功德箱和在线设备数要不要联网。
func (s *Server) supportOnline() bool { return !strings.EqualFold(s.setting("supportOnline"), "false") }

// handleSupportDonors GET support/donors:功德箱名单与运行中的面板数。和这页一样不用登录,里面只有公开信息。
func (s *Server) handleSupportDonors(w http.ResponseWriter, r *http.Request) {
	if !s.supportOnline() {
		writeJSON(w, http.StatusNotFound, map[string]string{"code": "off"})
		return
	}
	b := s.donorSnapshot()
	now := time.Now().Unix()
	b.Devices = visibleDevices(b, now)
	if b.Donors == nil {
		b.Donors = []donorRow{}
	}
	w.Header().Set("Cache-Control", "no-store")
	writeJSON(w, http.StatusOK, struct {
		donorBoard
		Now int64 `json:"now"` // 服务器时间:页面拿它算"统计于几小时前",不看浏览器的钟
	}{b, now})
}

// handleSupportPledge POST support/pledge {name, amount}:留名,返回要转的金额。错误只给代码,文案在页面上(有中英文)。
func (s *Server) handleSupportPledge(w http.ResponseWriter, r *http.Request) {
	if !s.supportOnline() {
		writeJSON(w, http.StatusNotFound, map[string]string{"code": "off"})
		return
	}
	if r.Method != http.MethodPost {
		writeJSON(w, http.StatusMethodNotAllowed, map[string]string{"code": "method"})
		return
	}
	if !sameOrigin(r) {
		writeJSON(w, http.StatusForbidden, map[string]string{"code": "origin"})
		return
	}
	var req struct {
		Name   string  `json:"name"`
		Amount float64 `json:"amount"`
	}
	r.Body = http.MaxBytesReader(w, r.Body, 4<<10)
	if json.NewDecoder(r.Body).Decode(&req) != nil {
		writeJSON(w, http.StatusBadRequest, map[string]string{"code": "bad"})
		return
	}
	name, bad := cleanDonorName(req.Name)
	if bad != "" {
		writeJSON(w, http.StatusBadRequest, map[string]string{"code": bad})
		return
	}
	if math.IsNaN(req.Amount) || req.Amount < pledgeMinUSDT || req.Amount > pledgeMaxUSDT {
		writeJSON(w, http.StatusBadRequest, map[string]string{"code": "amount"})
		return
	}
	at := time.Now().Unix() // 留名时间:在发出之前取,页面的截止时间按它算,不会比对账那边按 ntfy 收到的时间算的晚
	if !s.allowPledge(peerIP(r), at) {
		writeJSON(w, http.StatusTooManyRequests, map[string]string{"code": "rate"})
		return
	}
	pay := payableAmount(req.Amount)
	if m, ok := usdtMicros(pay); !ok || m > pledgeMaxMicros { // 对账那边认不了的金额不发出去
		writeJSON(w, http.StatusBadRequest, map[string]string{"code": "amount"})
		return
	}
	body, _ := json.Marshal(map[string]string{"n": name, "a": pay})
	ctx, cancel := context.WithTimeout(r.Context(), 10*time.Second)
	defer cancel()
	post, err := http.NewRequestWithContext(ctx, http.MethodPost, pledgeURL, strings.NewReader(string(body)))
	if err != nil {
		writeJSON(w, http.StatusBadGateway, map[string]string{"code": "send"})
		return
	}
	resp, err := donorHTTP.Do(post)
	if err != nil {
		writeJSON(w, http.StatusBadGateway, map[string]string{"code": "send"})
		return
	}
	io.Copy(io.Discard, io.LimitReader(resp.Body, 4<<10))
	resp.Body.Close()
	if resp.StatusCode >= 300 {
		writeJSON(w, http.StatusBadGateway, map[string]string{"code": "send"})
		return
	}
	// now 是服务器时间:页面按它和浏览器时钟的差来倒计时,电脑时区或时钟不对也不会一留名就"超时"。
	// 写响应前才取:发 ntfy 可能要好几秒,先取的话钟差会少算这段,倒计时会前后跳
	writeJSON(w, http.StatusOK, map[string]interface{}{"amount": pay, "name": name, "at": at, "expiresAt": at + pledgeWindow, "address": brand.Tron, "now": time.Now().Unix()})
}

// usdtMicros "10.037" → 10037000(USDT 六位小数,按字符串算,不经浮点)。
func usdtMicros(s string) (int64, bool) {
	whole, frac, _ := strings.Cut(s, ".")
	if whole == "" || len(frac) > 6 {
		return 0, false
	}
	frac += strings.Repeat("0", 6-len(frac))
	w, err1 := strconv.ParseInt(whole, 10, 64)
	f, err2 := strconv.ParseInt(frac, 10, 64)
	if err1 != nil || err2 != nil || w < 0 || f < 0 {
		return 0, false
	}
	return w*1_000_000 + f, true
}

// recentTransfers 收款地址最近两小时收到的 USDT。不管多少人在等、问的是哪一笔,tronEvery 最多查一次:免费接口不能被这个公开页面打爆。
// 最后一个返回值是"知不知道":手上没有够新的结果(tronGoodFor 之内查成过的)就是不知道,不能当成"没到账"。
// 这一次没查成、或者别人正在查:够新的上一次结果照样拿来答 —— 偶尔一次限速不让页面变成"不知道"。
// full = 查到的那一页是满的(有人拿零碎转账刷屏):列出来的照样算数,但没找到的那一笔可能只是被挤到了下一页。
func (s *Server) recentTransfers(ctx context.Context) (txs []tronTx, full, ok bool) {
	st := &s.donors
	st.mu.Lock()
	fresh := func() ([]tronTx, bool, bool) { // 手上那份还够不够新(要拿着锁)
		if !st.tronGood.IsZero() && time.Since(st.tronGood) <= tronGoodFor {
			return st.tron, st.tronFull, true
		}
		return nil, false, false
	}
	if st.tronBusy || time.Now().Before(st.tronNext) {
		txs, full, ok = fresh()
		st.mu.Unlock()
		return txs, full, ok
	}
	st.tronBusy = true
	st.mu.Unlock()
	func() { // 查的过程中万一出了意外也要把「正在问」放掉,不然以后谁问都是不知道
		defer func() {
			if recover() != nil {
				txs, full, ok = nil, false, false
			}
		}()
		txs, full, ok = fetchTransfers(ctx)
	}()
	st.mu.Lock()
	defer st.mu.Unlock()
	st.tronBusy = false
	if ok {
		st.tron, st.tronFull, st.tronGood, st.tronFails = txs, full, time.Now(), 0
		st.tronNext = time.Now().Add(tronEvery)
		return txs, full, true
	}
	if st.tronFails < 8 {
		st.tronFails++
	}
	wait := tronBackoff << uint(st.tronFails) // 10、20、40…
	if wait > tronBackoffMax {
		wait = tronBackoffMax
	}
	st.tronNext = time.Now().Add(wait)
	return fresh()
}

func fetchTransfers(ctx context.Context) (txs []tronTx, full, ok bool) {
	u := fmt.Sprintf("%s/v1/accounts/%s/transactions/trc20?only_to=true&limit=%d&contract_address=%s&min_timestamp=%d", tronURL, brand.Tron, tronPage, usdtContract, (time.Now().Unix()-2*pledgeWindow-pledgeSkew)*1000)
	req, err := http.NewRequestWithContext(ctx, http.MethodGet, u, nil)
	if err != nil {
		return nil, false, false
	}
	resp, err := donorHTTP.Do(req)
	if err != nil {
		return nil, false, false
	}
	defer resp.Body.Close()
	var out struct {
		Data []struct {
			To    string `json:"to"`
			Type  string `json:"type"`
			Value string `json:"value"`
			Time  int64  `json:"block_timestamp"`
		} `json:"data"`
	}
	if resp.StatusCode != http.StatusOK || json.NewDecoder(io.LimitReader(resp.Body, 2<<20)).Decode(&out) != nil {
		return nil, false, false
	}
	txs = make([]tronTx, 0, len(out.Data))
	for _, d := range out.Data {
		if d.To != brand.Tron || d.Type != "Transfer" {
			continue
		}
		if v, err := strconv.ParseInt(d.Value, 10, 64); err == nil {
			txs = append(txs, tronTx{micros: v, at: d.Time / 1000})
		}
	}
	return txs, len(out.Data) >= tronPage, true
}

// handleSupportCheck GET support/pledge-check?amount=10.037&since=<unix>:这笔到了没有。只查留名之后 60 分钟内的。
func (s *Server) handleSupportCheck(w http.ResponseWriter, r *http.Request) {
	if !s.supportOnline() {
		writeJSON(w, http.StatusNotFound, map[string]string{"code": "off"})
		return
	}
	want, ok := usdtMicros(r.URL.Query().Get("amount"))
	since, err := strconv.ParseInt(r.URL.Query().Get("since"), 10, 64)
	now := time.Now().Unix()
	if !ok || err != nil || since < now-2*pledgeWindow || since > now+60 {
		writeJSON(w, http.StatusBadRequest, map[string]string{"code": "bad"})
		return
	}
	// 这次查询是大家共用的:发起的那个人关了页面也别把它取消掉,不然别人拿到的都是"不知道"
	ctx, cancel := context.WithTimeout(context.WithoutCancel(r.Context()), 10*time.Second)
	defer cancel()
	txs, full, got := s.recentTransfers(ctx)
	now = time.Now().Unix() // 给页面的服务器时间在写响应前取:查 TronGrid 可能要好几秒,先取的话钟差会少算这段,倒计时会前后跳
	if !got {
		writeJSON(w, http.StatusOK, map[string]interface{}{"known": false, "arrived": false, "now": now})
		return
	}
	arrived := false
	for _, tx := range txs {
		if tx.micros == want && tx.at >= since-pledgeSkew && tx.at <= since+pledgeWindow {
			arrived = true
			break
		}
	}
	// 那一页是满的又没找到:可能只是被挤到了下一页,算"不知道",页面不会据此说"没到账"
	writeJSON(w, http.StatusOK, map[string]interface{}{"known": arrived || !full, "arrived": arrived, "now": now})
}
