package web

import (
	"encoding/json"
	"errors"
	"net/http"
	"sort"
	"strconv"
	"strings"

	"github.com/skip2/go-qrcode"

	"github.com/Maoyangui/m-ui/brand"
	"github.com/Maoyangui/m-ui/reward"
)

// 流量奖励活动页的接口(前端 js/reward.js):
//
//	GET  reward                 概况:累计真实流量、积分、登记的地址、提醒、奖池合计、正在等的留名捐赠
//	GET  reward/ledger          逐笔入账(page、size、kind=order|donation、chain=solana|tron)
//	GET  reward/board           积分榜(page、size、q=按地址查)
//	GET  reward/donors          捐赠榜(page、size):留名的按合计排;匿名的只给笔数和合计
//	GET  reward/qr              奖池捐赠地址的二维码(PNG)
//	POST reward/bind            {address} 登记 / 更换领取地址
//	POST reward/unbind          取消登记
//	POST reward/remind          提醒气泡点了「以后再说」
//	POST reward/donate          {name, amount} 登记一笔留名捐赠
//	POST reward/donate/cancel   取消还在等的那一笔
//	POST reward/donate/clear    看过结果了,收起
//
// 数真实流量、上报、读账本都在 runner/reward.go;这里只管把手上的东西按页面要的样子给出去。
// 副机上、以及设置里关掉之后:GET reward 只回 {enabled:false},别的一律不给。

const rewardPageMax = 50

// rewardLedgerPage 活动页「公开账本」那个链接:仓库里 rewards 分支。
const rewardLedgerPage = brand.Repo + "/tree/rewards"

type rewardPool struct {
	Total        float64        `json:"total"`
	OrderRate    float64        `json:"orderRate"`
	OrderCount   int            `json:"orderCount"`
	OrderAmount  float64        `json:"orderAmount"`
	OrderShare   float64        `json:"orderShare"`
	DonateCount  int            `json:"donateCount"`
	DonateAmount float64        `json:"donateAmount"`
	DonateMin    float64        `json:"donateMin"`
	DonateMax    float64        `json:"donateMax"`
	Chains       []reward.Chain `json:"chains"`
	Points       int64          `json:"points"`
	Addresses    int            `json:"addresses"`
	PayoutAt     int64          `json:"payoutAt"`
	UpdatedAt    int64          `json:"updatedAt"`
	Stale        bool           `json:"stale"`
	LedgerURL    string         `json:"ledgerUrl"`
}

func (s *Server) handleReward(w http.ResponseWriter, r *http.Request) {
	path := strings.TrimSuffix(r.URL.Path, "/")
	rest := ""
	if i := strings.LastIndex(path, "/reward"); i >= 0 {
		rest = strings.Trim(path[i+len("/reward"):], "/")
	}
	on := s.run.RewardEnabled()
	if r.Method == http.MethodGet && rest == "" {
		if !on {
			writeJSON(w, http.StatusOK, map[string]interface{}{"enabled": false})
			return
		}
		writeJSON(w, http.StatusOK, s.rewardSummary())
		return
	}
	if !on {
		badRequest(w, errors.New("流量奖励没有开着"))
		return
	}
	q := r.URL.Query()
	page, _ := strconv.Atoi(q.Get("page"))
	size, _ := strconv.Atoi(q.Get("size"))
	if page < 1 {
		page = 1
	}
	if size < 1 {
		size = 10
	}
	if size > rewardPageMax {
		size = rewardPageMax
	}
	cut := func(n int) (int, int) {
		from := (page - 1) * size
		if from > n {
			from = n
		}
		to := from + size
		if to > n {
			to = n
		}
		return from, to
	}
	decode := func(v interface{}) bool {
		if err := json.NewDecoder(http.MaxBytesReader(w, r.Body, 1<<14)).Decode(v); err != nil {
			badRequest(w, errors.New("请求体不是合法 JSON"))
			return false
		}
		return true
	}
	switch {
	case r.Method == http.MethodGet && rest == "ledger":
		type row struct {
			reward.Deposit
			Share float64 `json:"share"`
		}
		l, _ := s.run.RewardLedger()
		out := []row{}
		total := 0
		if l != nil {
			kind, chain := q.Get("kind"), q.Get("chain")
			all := make([]reward.Deposit, 0, len(l.Deposits))
			for _, d := range l.Deposits {
				if (kind == "" || d.Kind == kind) && (chain == "" || d.Chain == chain) {
					all = append(all, d)
				}
			}
			sort.SliceStable(all, func(i, j int) bool { return all[i].TS > all[j].TS })
			total = len(all)
			from, to := cut(total)
			for _, d := range all[from:to] {
				out = append(out, row{Deposit: d, Share: l.Share(d)})
			}
		}
		writeJSON(w, http.StatusOK, map[string]interface{}{"items": out, "total": total})

	case r.Method == http.MethodGet && rest == "board":
		type row struct {
			Rank     int    `json:"rank"`
			Address  string `json:"address"`
			Points   int64  `json:"points"`
			Panels   int    `json:"panels"`
			Excluded bool   `json:"excluded,omitempty"`
			Reason   string `json:"reason,omitempty"`
			Me       bool   `json:"me,omitempty"`
		}
		l, _ := s.run.RewardLedger()
		out := []row{}
		total, top := 0, int64(1)
		if l != nil {
			me := s.run.RewardState().Address
			var ok, gone []row
			for _, b := range l.Board {
				x := row{Address: b.Address, Points: b.Points, Panels: b.Panels, Excluded: b.Excluded, Reason: b.Reason, Me: me != "" && b.Address == me}
				if b.Excluded {
					gone = append(gone, x)
				} else if b.Points > 0 || x.Me {
					ok = append(ok, x)
				}
			}
			sort.SliceStable(ok, func(i, j int) bool { return ok[i].Points > ok[j].Points })
			for i := range ok {
				ok[i].Rank = i + 1
			}
			if len(ok) > 0 && ok[0].Points > 0 {
				top = ok[0].Points
			}
			list := append(ok, gone...)
			if k := strings.TrimSpace(q.Get("q")); k != "" {
				kept := list[:0:0]
				for _, x := range list {
					if strings.Contains(strings.ToLower(x.Address), strings.ToLower(k)) {
						kept = append(kept, x)
					}
				}
				list = kept
			} else {
				for i, x := range list { // 我不在第一页时,钉在最上面
					if x.Me && i >= size {
						list = append([]row{x}, append(list[:i:i], list[i+1:]...)...)
						break
					}
				}
			}
			total = len(list)
			from, to := cut(total)
			out = append(out, list[from:to]...)
		}
		writeJSON(w, http.StatusOK, map[string]interface{}{"items": out, "total": total, "top": top})

	case r.Method == http.MethodGet && rest == "donors":
		type row struct {
			Rank   int     `json:"rank"`
			Name   string  `json:"name"`
			Amount float64 `json:"amount"`
			Count  int     `json:"count"`
			LastAt int64   `json:"lastAt"`
			Me     bool    `json:"me,omitempty"`
		}
		type sum struct {
			Donors int     `json:"donors,omitempty"`
			Count  int     `json:"count"`
			Amount float64 `json:"amount"`
		}
		l, _ := s.run.RewardLedger()
		out := []row{}
		var named, anon sum
		total, top := 0, 1.0
		if l != nil {
			mine := ""
			if it := s.run.RewardState().Donation; it != nil && it.Status == "paid" {
				mine = it.Name
			}
			by := map[string]*row{}
			var list []*row
			for _, d := range l.Deposits {
				if d.Kind != "donation" {
					continue
				}
				if d.Name == "" {
					anon.Count++
					anon.Amount += d.Amount
					continue
				}
				x := by[d.Name]
				if x == nil {
					x = &row{Name: d.Name, Me: d.Name == mine}
					by[d.Name] = x
					list = append(list, x)
				}
				x.Amount += d.Amount
				x.Count++
				if d.TS > x.LastAt {
					x.LastAt = d.TS
				}
				named.Count++
				named.Amount += d.Amount
			}
			sort.SliceStable(list, func(i, j int) bool {
				if list[i].Amount != list[j].Amount {
					return list[i].Amount > list[j].Amount
				}
				return list[i].LastAt > list[j].LastAt
			})
			for i, x := range list {
				x.Rank = i + 1
				x.Amount = cents(x.Amount)
			}
			named.Donors, named.Amount, anon.Amount = len(list), cents(named.Amount), cents(anon.Amount)
			if len(list) > 0 && list[0].Amount > 0 {
				top = list[0].Amount
			}
			total = len(list)
			from, to := cut(total)
			for _, x := range list[from:to] {
				out = append(out, *x)
			}
		}
		writeJSON(w, http.StatusOK, map[string]interface{}{"items": out, "total": total, "top": top, "named": named, "anon": anon})

	case r.Method == http.MethodGet && rest == "qr":
		l, _ := s.run.RewardLedger()
		if l == nil || l.Donate.Address == "" {
			http.NotFound(w, r)
			return
		}
		png, err := qrcode.Encode(l.Donate.Address, qrcode.Medium, 264)
		if err != nil {
			badRequest(w, err)
			return
		}
		w.Header().Set("Content-Type", "image/png")
		w.Header().Set("Cache-Control", "private, max-age=600")
		_, _ = w.Write(png)

	case r.Method == http.MethodPost && rest == "bind":
		var p struct {
			Address string `json:"address"`
		}
		if !decode(&p) {
			return
		}
		if err := s.run.RewardBind(p.Address); err != nil {
			badRequest(w, err)
			return
		}
		s.audit(r, "reward", "bind", shortAddr(p.Address))
		writeJSON(w, http.StatusOK, s.rewardSummary())

	case r.Method == http.MethodPost && rest == "unbind":
		if err := s.run.RewardUnbind(); err != nil {
			badRequest(w, err)
			return
		}
		s.audit(r, "reward", "unbind", nil)
		writeJSON(w, http.StatusOK, s.rewardSummary())

	case r.Method == http.MethodPost && rest == "remind":
		if err := s.run.RewardRemindDismiss(); err != nil {
			badRequest(w, err)
			return
		}
		writeJSON(w, http.StatusOK, map[string]string{"ok": "1"})

	case r.Method == http.MethodPost && rest == "donate":
		var p struct {
			Name   string  `json:"name"`
			Amount float64 `json:"amount"`
		}
		if !decode(&p) {
			return
		}
		if err := s.run.RewardDonate(p.Name, p.Amount); err != nil {
			badRequest(w, err)
			return
		}
		s.audit(r, "reward", "donate", map[string]interface{}{"name": p.Name, "amount": p.Amount})
		writeJSON(w, http.StatusOK, s.rewardSummary())

	case r.Method == http.MethodPost && (rest == "donate/cancel" || rest == "donate/clear"):
		if err := s.run.RewardDonateCancel(); err != nil {
			badRequest(w, err)
			return
		}
		writeJSON(w, http.StatusOK, s.rewardSummary())

	default:
		http.NotFound(w, r)
	}
}

// rewardSummary GET reward 的回复。
func (s *Server) rewardSummary() map[string]interface{} {
	s.run.RewardDonateRefresh() // 正在等到账的那一笔顺手问一下(有间隔,不会每次都问)
	st := s.run.RewardState()
	out := map[string]interface{}{
		"enabled": true, "tb": reward.TB, "bytes": st.Bytes, "points": st.Bytes / reward.TB,
		"address": st.Address, "boundAt": st.BoundAt, "remind": st.Remind,
		"credited": int64(0), "excluded": nil, "donation": st.Donation, "pool": nil,
	}
	l, stale := s.run.RewardLedger()
	credited, excluded := int64(0), st.Last.Excluded
	if st.Address != "" {
		credited = st.Last.Credited
		if l != nil { // 账本里有这个地址:以较新的为准(上报的答复只在上报时更新,账本十分钟一取)
			for _, b := range l.Board {
				if b.Address != st.Address {
					continue
				}
				if l.UpdatedAt >= st.Last.At {
					credited = b.Points
					excluded = nil
					if b.Excluded {
						excluded = &reward.Excluded{Reason: b.Reason}
					}
				}
				break
			}
		}
		if p := st.Bytes / reward.TB; credited > p {
			credited = p
		}
	} else {
		excluded = nil
	}
	out["credited"] = credited
	if excluded != nil {
		out["excluded"] = excluded
	}
	if l != nil {
		chains := l.Chains
		if chains == nil {
			chains = []reward.Chain{}
		}
		out["pool"] = rewardPool{
			Total: l.Pool.Total, OrderRate: l.OrderRate, OrderCount: l.Pool.OrderCount, OrderAmount: l.Pool.OrderAmount, OrderShare: l.Pool.OrderShare,
			DonateCount: l.Pool.DonateCount, DonateAmount: l.Pool.DonateAmount, DonateMin: l.Donate.Min, DonateMax: l.Donate.Max,
			Chains: chains, Points: l.Pool.Points, Addresses: l.Pool.Addresses, PayoutAt: l.PayoutAt, UpdatedAt: l.UpdatedAt, Stale: stale,
			LedgerURL: rewardLedgerPage,
		}
	}
	return out
}

func cents(v float64) float64 { return float64(int64(v*100+0.5)) / 100 }

// shortAddr 审计日志里的地址只留头尾。
func shortAddr(a string) string {
	a = strings.TrimSpace(a)
	if len(a) <= 12 {
		return a
	}
	return a[:5] + "…" + a[len(a)-5:]
}
