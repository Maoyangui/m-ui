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
)

// 功德箱:"建议与支持"页上留名 + 转 USDT,到账后所有面板都能看到累计排行。
//
// 没有自己的服务端,数据全在公开的地方:
//   - 留名经 ntfy.sh(免费的公开消息接口)发出去,只有点"留名"时才发
//   - GitHub Actions 定时把留名和 TronGrid 查到的到账对上,结果写进本仓库 donors 分支的 donors.json(deploy/donors)
//   - 各面板打开这页时读 donors.json,最多一小时取一次;取不到就不显示,页面其它部分照常
//
// 名单里只有名字和累计金额。谁留的名、从哪台机器留的,都不记。

// 这几个地址测试里要换。
var (
	donorsURLs = []string{
		"https://raw.githubusercontent.com/" + brand.RepoPath + "/donors/donors.json",
		"https://cdn.jsdelivr.net/gh/" + brand.RepoPath + "@donors/donors.json", // raw 被墙的机器走它
	}
	pledgeURL = "https://ntfy.sh/" + brand.DonateTopic
	tronURL   = "https://api.trongrid.io"
)

const (
	usdtContract   = "TR7NHqjeKQxGTCi8q8ZY4pL8otSzgjLj6t"
	pledgeWindow   = int64(3600) // 留名后多久内到账才算
	donorsTTL      = time.Hour
	donorsMaxBytes = 512 << 10
	donorsMaxRows  = 500
	donorNameMax   = 24
	pledgeMinUSDT  = 1.0
	pledgeMaxUSDT  = 100000.0
)

type donorRow struct {
	Name  string  `json:"name"`
	Total float64 `json:"total"`
}

type donorBoard struct {
	UpdatedAt int64      `json:"updatedAt"`
	Devices   int        `json:"devices"` // 过去 24 小时有多少台面板在运行(见 deploy/donors)
	Donors    []donorRow `json:"donors"`
}

// donorState 这台面板手上的一份名单。
type donorState struct {
	mu       sync.Mutex
	board    donorBoard
	at       time.Time
	fetching bool
	rate     map[string][]int64 // 留名限流:来源 → 最近几次的时间
	total    []int64            // 这台面板最近一小时的留名
	tron     []tronTx           // 最近两小时的到账
	tronAt   time.Time
}

type tronTx struct{ micros, at int64 }

var donorHTTP = &http.Client{Timeout: 10 * time.Second}

// cleanBoard 外面取来的名单再过一遍:名字不合规的不要,行数有上限。页面上用 textContent 显示,这里是第二道。
func cleanBoard(b donorBoard) donorBoard {
	out := donorBoard{UpdatedAt: b.UpdatedAt, Devices: b.Devices}
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

func fetchDonorBoard(ctx context.Context) (donorBoard, bool) {
	for _, u := range donorsURLs {
		req, err := http.NewRequestWithContext(ctx, http.MethodGet, u, nil)
		if err != nil {
			continue
		}
		req.Header.Set("User-Agent", "m-ui")
		resp, err := donorHTTP.Do(req)
		if err != nil {
			continue
		}
		body, err := io.ReadAll(io.LimitReader(resp.Body, donorsMaxBytes))
		resp.Body.Close()
		if err != nil || resp.StatusCode != http.StatusOK {
			continue
		}
		var b donorBoard
		if json.Unmarshal(body, &b) != nil {
			continue
		}
		return cleanBoard(b), true
	}
	return donorBoard{}, false
}

// donorSnapshot 现有的名单;过期了在后台取一次,第一次等一小会儿。
func (s *Server) donorSnapshot() donorBoard {
	st := &s.donors
	st.mu.Lock()
	stale := time.Since(st.at) > donorsTTL
	first := st.at.IsZero()
	start := stale && !st.fetching
	if start {
		st.fetching = true
	}
	board := st.board
	st.mu.Unlock()
	if !start {
		return board
	}
	done := make(chan donorBoard, 1)
	go func() {
		ctx, cancel := context.WithTimeout(context.Background(), 12*time.Second)
		defer cancel()
		b, ok := fetchDonorBoard(ctx)
		st.mu.Lock()
		st.fetching = false
		if ok {
			st.board, st.at = b, time.Now()
		} else {
			st.at = time.Now().Add(-donorsTTL + 5*time.Minute) // 取不到:过 5 分钟再试,别每次打开都等
		}
		cur := st.board
		st.mu.Unlock()
		done <- cur
	}()
	if !first {
		return board
	}
	select {
	case b := <-done:
		return b
	case <-time.After(3 * time.Second):
		return board
	}
}

var domainLike = regexp.MustCompile(`(?i)[a-z0-9-]+\.[a-z]{2,}`)

// cleanDonorName 名字要显示在所有面板上:只收文字、数字、空格和少数标点,不能带链接。返回规整后的名字和错误代码。
func cleanDonorName(s string) (string, string) {
	s = strings.Join(strings.Fields(s), " ")
	if s == "" {
		return "", "name_empty"
	}
	if len([]rune(s)) > donorNameMax {
		return "", "name_long"
	}
	for _, r := range s {
		if unicode.IsLetter(r) || unicode.IsNumber(r) || unicode.IsMark(r) || strings.ContainsRune(" ._-·'", r) {
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

// handleSupportDonors GET support/donors:功德箱名单与运行中的面板数。和这页一样不用登录,里面只有公开信息。
func (s *Server) handleSupportDonors(w http.ResponseWriter, r *http.Request) {
	b := s.donorSnapshot()
	if b.Donors == nil {
		b.Donors = []donorRow{}
	}
	w.Header().Set("Cache-Control", "no-store")
	writeJSON(w, http.StatusOK, b)
}

// handleSupportPledge POST support/pledge {name, amount}:留名,返回要转的金额。错误只给代码,文案在页面上(有中英文)。
func (s *Server) handleSupportPledge(w http.ResponseWriter, r *http.Request) {
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
	now := time.Now().Unix()
	if !s.allowPledge(peerIP(r), now) {
		writeJSON(w, http.StatusTooManyRequests, map[string]string{"code": "rate"})
		return
	}
	pay := payableAmount(req.Amount)
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
	writeJSON(w, http.StatusOK, map[string]interface{}{"amount": pay, "name": name, "at": now, "expiresAt": now + pledgeWindow, "address": brand.Tron})
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

// recentTransfers 收款地址最近两小时收到的 USDT。不管多少人在等、问的是哪一笔,20 秒最多查一次:免费接口不能被这个公开页面打爆。
func (s *Server) recentTransfers(ctx context.Context) ([]tronTx, bool) {
	st := &s.donors
	st.mu.Lock()
	if time.Since(st.tronAt) < 20*time.Second {
		txs := st.tron
		st.mu.Unlock()
		return txs, true
	}
	st.tronAt = time.Now() // 查失败也算一次,等下一轮再试
	st.tron = nil
	st.mu.Unlock()
	u := fmt.Sprintf("%s/v1/accounts/%s/transactions/trc20?only_to=true&limit=200&contract_address=%s&min_timestamp=%d", tronURL, brand.Tron, usdtContract, (time.Now().Unix()-2*pledgeWindow)*1000)
	req, err := http.NewRequestWithContext(ctx, http.MethodGet, u, nil)
	if err != nil {
		return nil, false
	}
	resp, err := donorHTTP.Do(req)
	if err != nil {
		return nil, false
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
		return nil, false
	}
	txs := make([]tronTx, 0, len(out.Data))
	for _, d := range out.Data {
		if d.To != brand.Tron || d.Type != "Transfer" {
			continue
		}
		if v, err := strconv.ParseInt(d.Value, 10, 64); err == nil {
			txs = append(txs, tronTx{micros: v, at: d.Time / 1000})
		}
	}
	st.mu.Lock()
	st.tron = txs
	st.mu.Unlock()
	return txs, true
}

// handleSupportCheck GET support/pledge-check?amount=10.037&since=<unix>:这笔到了没有。只查留名之后 60 分钟内的。
func (s *Server) handleSupportCheck(w http.ResponseWriter, r *http.Request) {
	want, ok := usdtMicros(r.URL.Query().Get("amount"))
	since, err := strconv.ParseInt(r.URL.Query().Get("since"), 10, 64)
	now := time.Now().Unix()
	if !ok || err != nil || since < now-2*pledgeWindow || since > now+60 {
		writeJSON(w, http.StatusBadRequest, map[string]string{"code": "bad"})
		return
	}
	ctx, cancel := context.WithTimeout(r.Context(), 10*time.Second)
	defer cancel()
	txs, got := s.recentTransfers(ctx)
	if !got {
		writeJSON(w, http.StatusOK, map[string]interface{}{"known": false, "arrived": false})
		return
	}
	arrived := false
	for _, tx := range txs {
		if tx.micros == want && tx.at >= since && tx.at <= since+pledgeWindow {
			arrived = true
			break
		}
	}
	writeJSON(w, http.StatusOK, map[string]interface{}{"known": true, "arrived": arrived})
}
