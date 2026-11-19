package runner

import (
	"context"
	"encoding/json"
	"errors"
	"net"
	"net/http"
	"strconv"
	"strings"
	"sync"
	"time"

	"os"

	"github.com/Maoyangui/m-ui/brand"
	"github.com/Maoyangui/m-ui/database/model"
	"github.com/Maoyangui/m-ui/logger"
	"github.com/Maoyangui/m-ui/reward"
	"github.com/Maoyangui/m-ui/safe"
)

// 流量奖励里面板这一侧有状态的部分:登记的地址、面板身份、提醒、最近一次上报的答复、公开账本的缓存、留名捐赠。
// 真实流量的累计值在 model.RewardCounter(记账时顺手加的);协议和校验在 reward 包;接口在 web/reward_api.go。
//
// 什么时候联网(设置里关掉流量奖励后都不联):
//   - 活动页打开时读公开账本:没登记地址的只读 GitHub 上的那份;登记了的先问登记服务(新),问不到再读 GitHub 的。
//   - 登记了地址之后每半小时上报一次累计值(登记、换地址时马上报一次)。
//   - 管理员提交留名捐赠时登记这一笔,之后等到账期间查它的状态。
//
// 只在主机上有:副机的流量随用量汇到主机,在主机上累计。

// 这几个地址测试里要换。
var (
	RewardAPI        = brand.RewardAPI
	RewardLedgerURLs = []string{
		"https://raw.githubusercontent.com/" + brand.RepoPath + "/rewards/ledger.json",
		"https://cdn.jsdelivr.net/gh/" + brand.RepoPath + "@rewards/ledger.json", // raw 取不到的机器走它(分支文件在它那里会旧几个小时)
	}
)

// 联调时把登记服务和账本指到别处(本机起的登记服务、测试用的账本文件):只认环境变量,面板里没有这个设置。
func init() {
	if v := strings.TrimSpace(os.Getenv("M_UI_REWARD_API")); v != "" {
		RewardAPI = v
	}
	if v := strings.TrimSpace(os.Getenv("M_UI_REWARD_LEDGER")); v != "" {
		RewardLedgerURLs = strings.Split(v, ",")
	}
}

const (
	rewardReportEvery = 30 * time.Minute
	rewardLedgerTTL   = 10 * time.Minute // 账本最多这么久取一次
	rewardLedgerRetry = 2 * time.Minute  // 取不到:过这么久再试,别每次打开都等
	rewardLedgerWait  = 4 * time.Second  // 手上没有账本时,打开活动页最多等这么久
	rewardStaleAfter  = 3 * time.Hour    // 账本比这还旧:页面上标出来「没取到最新的」
	rewardNetBudget   = 12 * time.Second
	rewardIntentPoll  = 4 * time.Second // 等到账时最多这么勤地问一次(登记服务被问到时会当场看链,付了款一二十秒页面就跳)
)

type rewardSvc struct {
	mu       sync.Mutex
	ledger   *reward.Ledger
	got      time.Time     // 最近一次取到合法账本的时间
	next     time.Time     // 什么时候可以再取
	fetching chan struct{} // 正在取:这一轮结束时关闭
	dropped  bool          // 手上那份明知是旧的(刚登记、积分变了):下一次要等新的取回来再答
	kick     chan struct{} // 让上报马上跑一轮(登记、换地址之后)
	intentAt time.Time     // 上次问留名捐赠的状态是什么时候
	// 登记服务最近一次答复里的 scanAt / now,是哪一笔留名的(只放内存:等到账时几秒变一次,不值得写库)
	intentId              string
	intentScan, intentNow int64
	reportMu              sync.Mutex // 上报串行:手动触发和定时的别撞在一起
}

// 登记服务按「来源」认面板:一处来源(一个公网 IPv4 地址;没有 IPv4 的按 IPv6 /48)只认一个地址。
// IPv4 地址比 IPv6 的稳定,所以能走 IPv4 就走 IPv4,实在连不上才走 IPv6 —— 免得同一台面板一会儿从这边报、一会儿从那边报。
var rewardHTTP = &http.Client{Timeout: 10 * time.Second, Transport: &http.Transport{
	Proxy: http.ProxyFromEnvironment,
	DialContext: func(ctx context.Context, network, addr string) (net.Conn, error) {
		d := &net.Dialer{Timeout: 4 * time.Second, KeepAlive: 30 * time.Second}
		if c, err := d.DialContext(ctx, "tcp4", addr); err == nil {
			return c, nil
		}
		return d.DialContext(ctx, "tcp6", addr)
	},
	ForceAttemptHTTP2:   true,
	MaxIdleConns:        2,
	IdleConnTimeout:     60 * time.Second,
	TLSHandshakeTimeout: 5 * time.Second,
}}

func (r *Runner) rewardClient() *reward.Client {
	return &reward.Client{API: RewardAPI, LedgerURLs: RewardLedgerURLs, HTTP: rewardHTTP, UserAgent: "m-ui/" + Version}
}

// RewardEnabled 流量奖励开着没有(默认开;副机上一律算关:只在主机上显示和累计)。
func (r *Runner) RewardEnabled() bool {
	return !r.IsNode() && !strings.EqualFold(r.setting(reward.KeyEnabled), "false")
}

// RewardState 活动页要的本机状态。
type RewardState struct {
	Bytes    int64
	Address  string
	BoundAt  int64
	Remind   string // fresh(刚装好,还没提醒过)| tb(收起之后又满了 1 TB)| ""
	Last     reward.ReportReply
	Donation *reward.Intent
}

// RewardState 读一遍本机状态。
func (r *Runner) RewardState() RewardState {
	st := RewardState{Bytes: model.RewardBytes(r.db), Address: r.setting(reward.KeyAddress)}
	st.BoundAt, _ = strconv.ParseInt(r.setting(reward.KeyBoundAt), 10, 64)
	_ = json.Unmarshal([]byte(r.setting(reward.KeyLast)), &st.Last)
	if raw := r.setting(reward.KeyDonation); raw != "" {
		var it reward.Intent
		if json.Unmarshal([]byte(raw), &it) == nil && it.Id != "" {
			r.reward.mu.Lock()
			if r.reward.intentId == it.Id && r.reward.intentNow >= it.Now {
				it.ScanAt, it.Now = r.reward.intentScan, r.reward.intentNow
			}
			r.reward.mu.Unlock()
			it.Status = it.Shown(time.Now().Unix()) // 过了期限:核对完了是 expired,还没核对完是 settling
			st.Donation = &it
		}
	}
	if st.Address == "" {
		points := st.Bytes / reward.TB
		switch seen := r.setting(reward.KeyRemind); {
		case seen == "":
			st.Remind = "fresh"
			if points > 0 {
				st.Remind = "tb"
			}
		default:
			if n, err := strconv.ParseInt(seen, 10, 64); err != nil || points > n {
				st.Remind = "tb"
			}
		}
	}
	return st
}

// RewardRemindDismiss 提醒气泡点了「以后再说」:到下一次满 1 TB 之前不再冒。
func (r *Runner) RewardRemindDismiss() error {
	return r.setSetting(reward.KeyRemind, strconv.FormatInt(model.RewardBytes(r.db)/reward.TB, 10))
}

// rewardKey 面板身份:第一次用到时生成,之后一直是这一把(随备份、副本走)。
func (r *Runner) rewardKey() (string, error) {
	if k := r.setting(reward.KeyPriv); k != "" {
		return k, nil
	}
	k, err := reward.NewKey()
	if err != nil {
		return "", err
	}
	if err := r.setSetting(reward.KeyPriv, k); err != nil {
		return "", err
	}
	return k, nil
}

// RewardBind 登记(或更换)领取地址。先在本机记下,再马上向登记服务报一次;登记服务这会儿连不上不算失败,之后定时再报。
// 登记服务明确拒绝的(比如换地址还在冷却期)原样把原因交回去,本机保持原来的地址。
func (r *Runner) RewardBind(address string) error {
	address = strings.TrimSpace(address)
	if !reward.ValidSolana(address) {
		return errors.New("这不是 Solana 地址")
	}
	if !r.RewardEnabled() {
		return errors.New("流量奖励没有开着")
	}
	old, oldAt := r.setting(reward.KeyAddress), r.setting(reward.KeyBoundAt)
	if old == address {
		return nil
	}
	if _, err := r.rewardKey(); err != nil {
		return err
	}
	if err := r.setSetting(reward.KeyAddress, address); err != nil {
		return err
	}
	if err := r.setSetting(reward.KeyBoundAt, strconv.FormatInt(time.Now().Unix(), 10)); err != nil {
		return err
	}
	ctx, cancel := context.WithTimeout(context.Background(), rewardNetBudget)
	defer cancel()
	if err := r.rewardReport(ctx); err != nil {
		var rej rewardRejected
		if errors.As(err, &rej) { // 登记服务不接受这次登记:退回原来的
			_ = r.setSetting(reward.KeyAddress, old)
			_ = r.setSetting(reward.KeyBoundAt, oldAt)
			return errors.New(rej.msg)
		}
		logger.Warning("流量奖励:地址已在本机记下,向登记服务报到没成功(之后定时再报): ", err)
	}
	r.rewardDropLedger() // 积分榜里要有「我」了
	return nil
}

// RewardUnbind 取消登记:之后不再上报(登记处已经记下的积分留在原地址名下)。
func (r *Runner) RewardUnbind() error {
	if err := r.setSetting(reward.KeyAddress, ""); err != nil {
		return err
	}
	_ = r.setSetting(reward.KeyBoundAt, "")
	_ = r.setSetting(reward.KeyLast, "")
	return nil
}

// rewardRejected 登记服务明确拒绝(4xx):原因要给管理员看。
type rewardRejected struct{ msg string }

func (e rewardRejected) Error() string { return e.msg }

// rewardReport 上报一次累计值;登记处记着的比本机大(本机是接任 / 还原回来的)就按它往上接。
func (r *Runner) rewardReport(ctx context.Context) error {
	r.reward.reportMu.Lock()
	defer r.reward.reportMu.Unlock()
	address := r.setting(reward.KeyAddress)
	if address == "" || !r.RewardEnabled() {
		return nil
	}
	raw, err := r.rewardKey()
	if err != nil {
		return err
	}
	priv, err := reward.ParseKey(raw)
	if err != nil {
		return err
	}
	now := time.Now().Unix()
	var prev reward.ReportReply
	_ = json.Unmarshal([]byte(r.setting(reward.KeyLast)), &prev)
	rep, err := r.rewardClient().Report(ctx, reward.NewReport(priv, address, model.RewardBytes(r.db), now))
	if err != nil {
		if st := reward.StatusOf(err); st >= 400 && st < 500 && st != http.StatusTooManyRequests {
			return rewardRejected{err.Error()}
		}
		return err
	}
	rep.At = now
	if b, err := json.Marshal(rep); err == nil {
		_ = r.setSetting(reward.KeyLast, string(b))
	}
	// 积分变了、被剔除了或者恢复了:手上那份账本(十分钟一取)已经是旧的,丢掉,下次打开活动页取新的 ——
	// 不然「我的积分」已经是新的,积分榜和合计却还停在旧的那份上。
	if rep.Credited != prev.Credited || (rep.Excluded == nil) != (prev.Excluded == nil) {
		r.rewardDropLedger()
	}
	if up := rep.GB << 30; up > model.RewardBytes(r.db) {
		if err := model.RaiseRewardBytes(r.db, up, now); err != nil {
			logger.Warning("流量奖励:按登记处的累计值往上接没成功: ", err)
		}
	}
	return nil
}

// RewardReportNow 现在就上报一次(平时由定时任务和登记动作触发;排查和测试时用)。
func (r *Runner) RewardReportNow(ctx context.Context) error { return r.rewardReport(ctx) }

// rewardLoop 登记了地址的面板定时上报。
func (r *Runner) rewardLoop(stop <-chan struct{}) {
	t := time.NewTicker(rewardReportEvery)
	defer t.Stop()
	first := time.NewTimer(90 * time.Second) // 开机后先等数据面起来、记过一轮账
	defer first.Stop()
	run := func() {
		if r.setting(reward.KeyAddress) == "" || !r.RewardEnabled() {
			return
		}
		ctx, cancel := context.WithTimeout(context.Background(), rewardNetBudget)
		defer cancel()
		if err := r.rewardReport(ctx); err != nil {
			logger.Debug("流量奖励:这一轮上报没成功(下一轮再报): ", err)
		}
	}
	for {
		select {
		case <-stop:
			return
		case <-first.C:
			run()
		case <-t.C:
			run()
		case <-r.rewardKickCh():
			run()
		}
	}
}

// ---- 公开账本 ----

// rewardDropLedger 手上那份账本明知是旧的了:下次要用时重新取,而且等它取回来(最多等 rewardLedgerWait)再答,不拿旧的顶。
func (r *Runner) rewardDropLedger() {
	r.reward.mu.Lock()
	r.reward.next = time.Time{}
	r.reward.dropped = true
	r.reward.mu.Unlock()
}

// RewardLedger 手上的账本(可能是 nil:还没取到过)和它是不是旧了。到点了就在后台取新的;手上没有时最多等一会儿。
func (r *Runner) RewardLedger() (*reward.Ledger, bool) {
	if !r.RewardEnabled() {
		return nil, false
	}
	s := &r.reward
	s.mu.Lock()
	now := time.Now()
	if now.After(s.next) && s.fetching == nil {
		done := make(chan struct{})
		s.fetching = done
		s.next = now.Add(rewardLedgerRetry)
		bound := r.setting(reward.KeyAddress) != ""
		safe.Go("runner.rewardLedger", func() {
			defer close(done)
			ctx, cancel := context.WithTimeout(context.Background(), rewardNetBudget)
			defer cancel()
			c := r.rewardClient()
			var l *reward.Ledger
			var err error
			if bound { // 登记了地址的本来就在和登记服务说话:它那份最新
				l, err = c.Export(ctx)
			}
			if l == nil {
				l, err = c.FetchLedger(ctx)
			}
			s.mu.Lock()
			defer s.mu.Unlock()
			s.fetching = nil
			s.dropped = false
			if err != nil || l == nil {
				logger.Debug("流量奖励:账本这一轮没取到: ", err)
				return
			}
			if s.ledger != nil && l.UpdatedAt < s.ledger.UpdatedAt {
				return // 取到的比手上的还旧(jsDelivr 上的那份):不换
			}
			s.ledger, s.got, s.next = l, time.Now(), time.Now().Add(rewardLedgerTTL)
		})
	}
	wait := s.fetching
	have := s.ledger
	mustWait := have == nil || s.dropped
	s.mu.Unlock()
	if mustWait && wait != nil {
		select {
		case <-wait:
		case <-time.After(rewardLedgerWait):
		}
		s.mu.Lock()
		have = s.ledger
		s.mu.Unlock()
	}
	if have == nil {
		return nil, false
	}
	return have, time.Since(time.Unix(have.UpdatedAt, 0)) > rewardStaleAfter
}

// ---- 留名捐赠 ----

func (r *Runner) rewardSaveIntent(it *reward.Intent) error {
	if it == nil {
		return r.setSetting(reward.KeyDonation, "")
	}
	b, err := json.Marshal(it)
	if err != nil {
		return err
	}
	return r.setSetting(reward.KeyDonation, string(b))
}

// RewardDonate 登记一笔留名捐赠。
func (r *Runner) RewardDonate(name string, amount float64) error {
	if !r.RewardEnabled() {
		return errors.New("流量奖励没有开着")
	}
	name, err := reward.CleanName(name)
	if err != nil {
		return err
	}
	if !(amount >= 1) || amount > 100000 || float64(int64(amount*100+0.5))/100 != amount {
		return errors.New("金额要在 1 到 100000 之间,最多两位小数")
	}
	ctx, cancel := context.WithTimeout(context.Background(), rewardNetBudget)
	defer cancel()
	it, err := r.rewardClient().Donate(ctx, name, amount)
	if err != nil {
		return err
	}
	if it.Id == "" || it.Exact <= 0 || it.ExpiresAt <= time.Now().Unix() || !reward.ValidSolana(it.Address) {
		return errors.New("登记服务给的答复不对,请稍后再试")
	}
	it.Status = "pending"
	return r.rewardSaveIntent(&it)
}

// RewardDonateRefresh 等到账的那一笔问一下登记服务(问得不会比 rewardIntentPoll 更勤);到账了账本要重取。
func (r *Runner) RewardDonateRefresh() {
	st := r.RewardState()
	if st.Donation == nil || (st.Donation.Status != "pending" && st.Donation.Status != "settling") {
		if st.Donation != nil && st.Donation.Status == "expired" {
			_ = r.rewardSaveIntent(st.Donation) // 把「过期」落下来
		}
		return
	}
	s := &r.reward
	s.mu.Lock()
	if time.Since(s.intentAt) < rewardIntentPoll {
		s.mu.Unlock()
		return
	}
	s.intentAt = time.Now()
	s.mu.Unlock()
	ctx, cancel := context.WithTimeout(context.Background(), 6*time.Second)
	defer cancel()
	got, err := r.rewardClient().DonateStatus(ctx, st.Donation.Id)
	if err != nil {
		if reward.StatusOf(err) == http.StatusNotFound && st.Donation.Status == "settling" {
			it := *st.Donation // 登记服务已经不记得这一笔了(没到账的留名过期 7 天后清掉):没到账
			it.Status, it.ScanAt = "expired", 0
			_ = r.rewardSaveIntent(&it)
		}
		return
	}
	s.mu.Lock()
	s.intentId, s.intentScan, s.intentNow = st.Donation.Id, got.ScanAt, got.Now
	s.mu.Unlock()
	// 登记服务说「没到账」,要等它把期限之后的链上情况看全了才算数(Resolve);在那之前接着问
	if end := st.Donation.Resolve(got); end != "" {
		it := *st.Donation
		it.Status, it.ScanAt, it.Now = end, got.ScanAt, got.Now
		if got.Exact > 0 {
			it.Exact = got.Exact
		}
		_ = r.rewardSaveIntent(&it)
		if end == "paid" {
			r.rewardDropLedger()
		}
	}
}

// RewardDonateCancel 取消还在等的那一笔(把占着的金额让出来)。
func (r *Runner) RewardDonateCancel() error {
	st := r.RewardState()
	if st.Donation != nil && st.Donation.Status == "pending" {
		ctx, cancel := context.WithTimeout(context.Background(), 6*time.Second)
		defer cancel()
		_ = r.rewardClient().DonateCancel(ctx, st.Donation.Id) // 让不出来也无妨,到期自己就没了
	}
	return r.rewardSaveIntent(nil)
}

// RewardKick 让上报马上跑一轮。
func (r *Runner) RewardKick() {
	select {
	case r.rewardKickCh() <- struct{}{}:
	default:
	}
}

// rewardKickCh 触发上报的通道(容量 1);第一次用到时建。
func (r *Runner) rewardKickCh() chan struct{} {
	r.reward.mu.Lock()
	defer r.reward.mu.Unlock()
	if r.reward.kick == nil {
		r.reward.kick = make(chan struct{}, 1)
	}
	return r.reward.kick
}
