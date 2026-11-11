package ha

import (
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net/http"
	"os"
	"path/filepath"
	"strconv"
	"strings"
	"sync"
	"time"

	"github.com/Maoyangui/m-ui/database/model"
	"github.com/Maoyangui/m-ui/hub"
	"github.com/Maoyangui/m-ui/logger"
	"github.com/Maoyangui/m-ui/notify"
	"github.com/Maoyangui/m-ui/selfupdate"
)

// 主机侧的副本调度。
//
// 备用 = 按服务器列表顺序(sort,再按 id)取前 N 台合格的副机:启用、5 分钟内在线、版本够、报过备用公钥且公钥没变、
// 磁盘够接任。整份加密副本只发给它们;不在名单里的,主机让它删掉手里的副本。链接是明文 http 的默认不当备用
// (链接上的主动中间人能冒充主机),设置里打开「允许明文链接的副机当备用」才算。
//
// 名单不轻易顺延(顺延 = 后排那台马上收到全部数据):已在名单里的备用暂时不合格(离线、磁盘、还没报公钥)保留名次
// 30 分钟;只因版本比主机旧(先升了主机)的一直保留名次与手里的旧副本,只是不发新的;全部副机同时离线(多半是主机
// 自己断网)时名单整个冻结。停用、删除、公钥变了、改了顺序立即顺延。名单里出现新面孔发通知;合格的备用不够时告警一次。
//
// 节奏:每隔 haReplicaMinutes(默认 5 分钟)一份;管理员一侧的审计日志有新改动时 30 秒防抖后补一份(定时任务、
// 规则引擎、容灾自己的记录不算);纪元或名单变了立即一份;两份之间至少隔 max(60 秒, 上一次耗时 × 4)。
// 内容没变就不重发,只告诉备用"你那份到现在还是最新的"。

var (
	replicaMinGap    = 60 * time.Second
	replicaDebounce  = 30 * time.Second
	replicaWarmup    = 90 * time.Second // 主机刚启动:各副机的状态还没拉齐,先用上次记下的名单
	replicaWarnSize  = int64(64 << 20)
	replicaOnlineFor = int64(300)
	standbyHold      = 30 * time.Minute // 名单里的备用暂时不合格时保留名次多久
	listEmptyWait    = 2 * time.Minute  // 名单由非空变空:等这么久(至少一轮报告刷新)再下发
	dropRetryMax     = 30 * time.Minute // 让停用 / 删除的副机删副本:重试间隔的上限
)

// TakeoverNeed 接任时(或备用磁盘门槛)除了已有的副本文件之外还要多少磁盘:解开的压缩包、解出的库、组新库的一份,
// 外加迁移与日志的余量。dbSize 不知道时按压缩包的 4 倍估(库一般能压到四分之一)。
func TakeoverNeed(zipSize, dbSize int64) int64 {
	if dbSize <= 0 {
		dbSize = 4 * zipSize
	}
	return zipSize + 2*dbSize + 64<<20
}

// standbyNeed 当备用要的磁盘:接任的峰值 + 手里两代副本(信封 ≈ 压缩包)。
func standbyNeed(zipSize, dbSize int64) int64 { return TakeoverNeed(zipSize, dbSize) + 2*zipSize }

// 不能当备用的原因(界面按代码翻译)。
const (
	reasonDisabled   = "disabled"    // 停用了
	reasonNoLink     = "noLink"      // 没填 API 地址或令牌
	reasonOffline    = "offline"     // 5 分钟内没联系上
	reasonVersion    = "version"     // 版本太旧:要先升级
	reasonNoPub      = "noPub"       // 还没报过备用公钥(没切到本机的纪元)
	reasonPubChanged = "pubChanged"  // 公钥和记住的不一样(那台重装过):到它的角色卡片里点「重新信任」
	reasonDisk       = "disk"        // 磁盘不够接任(解开的库 + 新库一份 + 手里两代副本)
	reasonPlain      = "plain"       // 链接是明文 http,没开「允许明文链接的副机当备用」
	reasonHolding    = "holding"     // 已在名单里、暂时不合格:保留名次中(不发新副本)
	reasonVerKeep    = "versionKeep" // 版本比主机旧(仍认得容灾):保留名次与手里的旧副本,不发新的;升级后恢复
)

// StandbyInfo 一台副机当备用的情况(面板显示)。
type StandbyInfo struct {
	Id       uint           `json:"id"`
	Name     string         `json:"name"`
	Rank     int            `json:"rank"` // 接任顺序(1 起),0 = 不是备用
	Eligible bool           `json:"eligible"`
	Reasons  []string       `json:"reasons,omitempty"`
	Version  string         `json:"version,omitempty"`
	PubFP    string         `json:"pubFp,omitempty"`    // 它现在报的公钥指纹
	PinnedFP string         `json:"pinnedFp,omitempty"` // 记住的公钥指纹
	Plain    bool           `json:"plain,omitempty"`    // 链接是明文 http
	Replica  *hub.HAReplica `json:"replica,omitempty"`  // 它报的手里那份副本
	Ack      *AckView       `json:"ack,omitempty"`      // 主机这边最近一次发送
	LeaseAgo int64          `json:"leaseAgo"`
	DiskFree int64          `json:"diskFree"`
	Peers    *hub.HAPeers   `json:"peers,omitempty"`
	Epoch    uint64         `json:"epoch"`
	Held     bool           `json:"held,omitempty"` // 不合格但保留名次(不发新副本、不让它删手里的)
	// 它当主机后会用的端口(就绪检查看会不会和本机订阅端口撞;老副机不报为 0)
	WebPort      int `json:"webPort,omitempty"`
	ResellerPort int `json:"resellerPort,omitempty"`
}

// AckView 最近一次给这台发副本的结果。
type AckView struct {
	Seq    uint64 `json:"seq"`
	SentAt int64  `json:"sentAt"`
	AsOf   int64  `json:"asOf"`
	Size   int64  `json:"size"`
	Err    string `json:"err,omitempty"`
	ErrAt  int64  `json:"errAt,omitempty"`
}

type ack struct {
	epoch  uint64
	seq    uint64
	sum    string
	size   int64
	sentAt int64
	asOf   int64
	err    string
	errAt  int64
}

type replicator struct {
	s       *Service
	mu      sync.Mutex
	started time.Time
	kickc   chan struct{}
	running bool
	// 上一次生成
	lastBuild time.Time
	lastCost  time.Duration
	lastMax   uint64
	changedAt time.Time
	lastEpoch uint64
	lastSum   string
	lastSize  int64
	lastDB    int64
	lastErr   string
	warnedBig time.Time
	firstDone bool // 刚成为主机的第一份(不等预热)发过了
	// 名单
	infos      []StandbyInfo
	ids        []uint
	computed   bool
	holdSince  map[uint]time.Time
	emptySince time.Time
	acks       map[uint]*ack
	gates      map[uint]chan struct{}
	alerted    map[uint]string
	dropAt     map[uint]time.Time
	// 停用 / 删除的副机删副本的重试
	dropMu      sync.Mutex
	dropRunning bool
	// 备用侧:副本自检的限频
	checkMu      sync.Mutex
	lastCheck    time.Time
	checkPending bool
}

func newReplicator(s *Service) *replicator {
	return &replicator{s: s, kickc: make(chan struct{}, 1), acks: map[uint]*ack{}, gates: map[uint]chan struct{}{},
		alerted: map[uint]string{}, dropAt: map[uint]time.Time{}, holdSince: map[uint]time.Time{}, started: s.now()}
}

// scheduleCheck 备用收到新副本:排一次自检,返回还要等多久;已经排着一次就不再排(ok=false)。
func (r *replicator) scheduleCheck(now time.Time) (time.Duration, bool) {
	r.checkMu.Lock()
	defer r.checkMu.Unlock()
	if r.checkPending {
		return 0, false
	}
	r.checkPending = true
	if r.lastCheck.IsZero() {
		return 0, true
	}
	return selfCheckGap - now.Sub(r.lastCheck), true
}

func (r *replicator) checkDone() {
	r.checkMu.Lock()
	r.checkPending = false
	r.checkMu.Unlock()
}

func (r *replicator) noteCheck(at time.Time) {
	r.checkMu.Lock()
	r.lastCheck = at
	r.checkMu.Unlock()
}

// kick 尽快生成一份(设置、名单、纪元变了)。
func (r *replicator) kick() {
	select {
	case r.kickc <- struct{}{}:
	default:
	}
}

// standbyIds 当前的备用名单(按接任顺序)。主机刚启动、各副机状态还没拉齐时,用上次记下的名单,
// 免得第一轮容灾状态里名单是空的、备用把手里的副本删了。
func (r *replicator) standbyIds() []uint {
	r.mu.Lock()
	computed, ids := r.computed, append([]uint(nil), r.ids...)
	r.mu.Unlock()
	if computed {
		if ids == nil {
			ids = []uint{}
		}
		return ids
	}
	saved := r.savedIds()
	if saved == nil {
		saved = []uint{}
	}
	return saved
}

func (r *replicator) rankOf(id uint) int {
	for i, x := range r.standbyIds() {
		if x == id {
			return i + 1
		}
	}
	return 0
}

// markFor 告诉备用:你手里序号为 Seq 的那份,到 At 为止仍是最新的。
func (r *replicator) markFor(id uint) *ReplicaMark {
	r.mu.Lock()
	defer r.mu.Unlock()
	a := r.acks[id]
	if a == nil || a.seq == 0 || a.epoch != r.lastEpoch {
		return nil
	}
	return &ReplicaMark{Seq: a.seq, At: a.asOf}
}

// Standbys 各副机当备用的情况(面板显示)。
func (r *replicator) Standbys() []StandbyInfo {
	r.mu.Lock()
	defer r.mu.Unlock()
	out := make([]StandbyInfo, len(r.infos))
	copy(out, r.infos)
	for i := range out {
		if a := r.acks[out[i].Id]; a != nil {
			out[i].Ack = &AckView{Seq: a.seq, SentAt: a.sentAt, AsOf: a.asOf, Size: a.size, Err: a.err, ErrAt: a.errAt}
		}
	}
	return out
}

// versionOK 版本够不够当备用 / 被设为主机:认得容灾接口,而且不比主机旧(副本、整库数据的表结构它得认得)。
func (s *Service) versionOK(v string) bool {
	if v == "" {
		return false
	}
	return !selfupdate.Newer(MinVersion, v) && !selfupdate.Newer(s.d.Version, v)
}

// versionLagOnly 认得容灾、只是比主机旧(先升了主机):手里的旧副本照样能接任,保留名次等它升级。
func (s *Service) versionLagOnly(v string) bool {
	return v != "" && !selfupdate.Newer(MinVersion, v) && selfupdate.Newer(s.d.Version, v)
}

// compute 按服务器列表顺序算出各副机当备用的情况与名单(名单的保留规则见文件开头)。
func (r *replicator) compute(cfg Config) ([]StandbyInfo, []uint) {
	s := r.s
	var nodes []model.Node
	s.d.DB.Where("is_local = ?", false).Order("sort asc, id asc").Find(&nodes)
	now := time.Now().Unix() // 和 hub 记的"最近联系"同一个钟
	epoch := s.Epoch()
	r.mu.Lock()
	lastSize, lastDB := r.lastSize, r.lastDB
	r.mu.Unlock()
	prev := r.standbyIds()
	var out []StandbyInfo
	linked, offline := 0, 0
	for _, n := range nodes {
		ci := StandbyInfo{Id: n.Id, Name: n.Name, DiskFree: -1, Plain: strings.HasPrefix(strings.ToLower(strings.TrimSpace(n.ApiUrl)), "http://")}
		var st hub.NodeStatus
		ok := false
		if s.d.Hub != nil {
			st, ok = s.d.Hub.StatusOf(n.Id)
		}
		ci.Version = st.Version
		if !n.Enabled {
			ci.Reasons = append(ci.Reasons, reasonDisabled)
		}
		hasLink := strings.TrimSpace(n.ApiUrl) != "" && n.Token != ""
		if !hasLink {
			ci.Reasons = append(ci.Reasons, reasonNoLink)
		}
		isOff := !ok || st.LastSeen == 0 || now-st.LastSeen > replicaOnlineFor
		if isOff {
			ci.Reasons = append(ci.Reasons, reasonOffline)
		}
		if n.Enabled && hasLink {
			linked++
			if isOff {
				offline++
			}
		}
		if ok && st.Version != "" && !s.versionOK(st.Version) {
			ci.Reasons = append(ci.Reasons, reasonVersion)
		}
		if ci.Plain && !cfg.AllowPlain {
			ci.Reasons = append(ci.Reasons, reasonPlain)
		}
		pinKey := KeyPubPrefix + strconv.FormatUint(uint64(n.Id), 10)
		pinned := s.setting(pinKey)
		ci.PinnedFP = Fingerprint(pinned)
		var replicaBytes int64
		if st.HA != nil {
			ci.Replica, ci.LeaseAgo, ci.DiskFree, ci.Peers, ci.Epoch = st.HA.Replica, st.HA.LeaseAgo, st.HA.DiskFree, st.HA.Peers, st.HA.Epoch
			ci.WebPort, ci.ResellerPort, replicaBytes = st.HA.WebPort, st.HA.ResellerPort, st.HA.ReplicaBytes
		}
		switch {
		case st.HA == nil || st.HA.Pub == "":
			// 纪元 0(还没开过容灾):副机这时本来就不报公钥,开启后一两分钟自动报上来,不算不合格
			if epoch > 0 && ok && st.LastSeen > 0 && n.Enabled {
				ci.Reasons = append(ci.Reasons, reasonNoPub)
			}
		case pinned == "":
			if _, err := ParsePub(st.HA.Pub); err == nil && n.Enabled {
				if err := s.set(map[string]string{pinKey: st.HA.Pub}); err == nil {
					logger.Info("容灾:记住了副机 ", n.Name, " 的备用公钥 ", Fingerprint(st.HA.Pub))
					pinned, ci.PinnedFP = st.HA.Pub, Fingerprint(st.HA.Pub)
				}
			}
			ci.PubFP = Fingerprint(st.HA.Pub)
		default:
			ci.PubFP = Fingerprint(st.HA.Pub)
			if pinned != st.HA.Pub {
				ci.Reasons = append(ci.Reasons, reasonPubChanged)
				r.alertOnce(n.Id, "pub:"+ci.PubFP, "🟠 <b>备用公钥变了</b>:"+notify.Esc(n.Name)+" 现在报的备用公钥("+ci.PubFP+
					")和主机记住的("+ci.PinnedFP+")不一样,不再给它发数据副本。\n如果是你重装了那台的 m-ui,在服务器列表里点它的角色标签,选「重新信任」;否则那台可能被人动过。")
			}
		}
		if lastSize > 0 && st.HA != nil && st.HA.DiskFree >= 0 {
			// 手里的副本文件本来就是给容灾用的:算回可用里,免得"有副本 → 不合格 → 删掉 → 又合格"来回转
			need, avail := standbyNeed(lastSize, lastDB), st.HA.DiskFree+replicaBytes
			if !containsId(prev, n.Id) {
				need += need / 10 // 进名单要多 10% 余量,出名单按原门槛:不在边缘反复进出
			}
			if avail < need {
				ci.Reasons = append(ci.Reasons, reasonDisk)
			}
		}
		ci.Eligible = len(ci.Reasons) == 0
		out = append(out, ci)
	}
	ids := r.pick(out, prev, cfg, linked > 0 && offline == linked)
	return out, ids
}

// pick 按顺序挑名单:合格的取前 N 台;已在名单里、暂时不合格的保留名次;全部副机同时离线时整个冻结。
func (r *replicator) pick(out []StandbyInfo, prev []uint, cfg Config, allOffline bool) []uint {
	now := r.s.now()
	r.mu.Lock()
	defer r.mu.Unlock()
	var ids []uint
	if allOffline && len(prev) > 0 {
		// 全部副机同时离线:多半是主机自己断网。名单冻结,保留名次的计时从恢复时重新算
		for i := range out {
			if containsId(prev, out[i].Id) && !hasReason(out[i].Reasons, reasonDisabled) {
				r.holdSince[out[i].Id] = now
				ids = append(ids, out[i].Id)
			}
		}
		r.markRanks(out, ids)
		return ids
	}
	for i := range out {
		c := &out[i]
		if len(ids) >= cfg.Standbys {
			break
		}
		if c.Eligible {
			delete(r.holdSince, c.Id)
			ids = append(ids, c.Id)
			continue
		}
		if !containsId(prev, c.Id) {
			continue
		}
		switch {
		case onlyReasons(c.Reasons, reasonVersion) && r.s.versionLagOnly(c.Version):
			c.Reasons, c.Held = []string{reasonVerKeep}, true
			ids = append(ids, c.Id)
		case onlyReasons(c.Reasons, reasonOffline, reasonDisk, reasonNoPub):
			since, ok := r.holdSince[c.Id]
			if !ok {
				since = now
				r.holdSince[c.Id] = now
			}
			if now.Sub(since) < standbyHold {
				c.Reasons, c.Held = append(c.Reasons, reasonHolding), true
				ids = append(ids, c.Id)
			}
		}
	}
	for id := range r.holdSince {
		if !containsId(ids, id) {
			delete(r.holdSince, id)
		}
	}
	// 名单由非空变空:等一会儿(至少一轮报告刷新)再下发,免得一次没拉到报告就让备用全删了副本
	if len(ids) == 0 && len(prev) > 0 {
		if r.emptySince.IsZero() {
			r.emptySince = now
		}
		if now.Sub(r.emptySince) < listEmptyWait {
			ids = append([]uint(nil), prev...)
		}
	} else {
		r.emptySince = time.Time{}
	}
	r.markRanks(out, ids)
	return ids
}

// markRanks 按名单写名次;名单里不合格的标成保留(调用方持有 r.mu)。
func (r *replicator) markRanks(out []StandbyInfo, ids []uint) {
	for i := range out {
		out[i].Rank = 0
		for j, id := range ids {
			if id == out[i].Id {
				out[i].Rank = j + 1
				if !out[i].Eligible && !out[i].Held {
					out[i].Held = true
					out[i].Reasons = append(out[i].Reasons, reasonHolding)
				}
			}
		}
	}
}

func hasReason(rs []string, r string) bool {
	for _, x := range rs {
		if x == r {
			return true
		}
	}
	return false
}

// onlyReasons 不合格的原因都在 allowed 里(且至少有一条)。
func onlyReasons(rs []string, allowed ...string) bool {
	if len(rs) == 0 {
		return false
	}
	for _, x := range rs {
		ok := false
		for _, a := range allowed {
			if x == a {
				ok = true
				break
			}
		}
		if !ok {
			return false
		}
	}
	return true
}

// sendable 名单里这一轮真能发副本的(合格的;保留名次的不发)。
func sendable(infos []StandbyInfo, ids []uint) []uint {
	var out []uint
	for _, id := range ids {
		for _, c := range infos {
			if c.Id == id && c.Eligible {
				out = append(out, id)
			}
		}
	}
	return out
}

func (r *replicator) alertOnce(id uint, what, text string) {
	r.mu.Lock()
	done := r.alerted[id] == what
	r.alerted[id] = what
	r.mu.Unlock()
	if !done {
		r.s.notify(text)
	}
}

// Repin 重新信任一台备用的公钥(那台重装过 m-ui 时用):它现在报着有效公钥就当场记下这把(就是管理员在卡片上核对过的那把),
// 没报就先忘掉、下一次见到时再记。
func (s *Service) Repin(id uint) error {
	if s.IsNode() {
		return errors.New("副机上不能改主机容灾:到主机面板改")
	}
	pub := ""
	if s.d.Hub != nil {
		if st, ok := s.d.Hub.StatusOf(id); ok && st.HA != nil {
			if _, err := ParsePub(st.HA.Pub); err == nil {
				pub = st.HA.Pub
			}
		}
	}
	if err := s.set(map[string]string{KeyPubPrefix + strconv.FormatUint(uint64(id), 10): pub}); err != nil {
		return err
	}
	if pub != "" {
		logger.Info("容灾:重新信任了服务器 #", id, " 的备用公钥 ", Fingerprint(pub))
	}
	s.rep.mu.Lock()
	delete(s.rep.alerted, id)
	s.rep.mu.Unlock()
	s.rep.kick()
	return nil
}

// PinnedFP 记住的这台备用的公钥指纹(没记住为空)。
func (s *Service) PinnedFP(id uint) string {
	return Fingerprint(s.setting(KeyPubPrefix + strconv.FormatUint(uint64(id), 10)))
}

// tick 每 10 秒:更新名单,该生成就生成一份(在后台,不挡这一轮)。
func (r *replicator) tick() {
	s := r.s
	if s.Epoch() == 0 || s.IsNode() {
		return
	}
	r.retryDrops()
	if s.Frozen() {
		return
	}
	cfg := s.Config()
	if cfg.Mode == ModeOff {
		r.setList(nil, nil, true)
		return
	}
	warm := s.now().Sub(r.started) >= replicaWarmup
	if !warm {
		// 刚启动:各副机的状态还没拉齐,名单先用上次记下的,只在拉齐状态后才可能变。
		// 刚成为主机(接任 / 被设为主机)的,第一份副本不等预热:新主机越早有备用越好
		if r.justBecame() {
			r.firstReplica(cfg)
		}
		return
	}
	infos, ids := r.compute(cfg)
	changed := r.setList(infos, ids, true)
	r.dropOthers(infos, ids)
	send := sendable(infos, ids)
	if len(send) == 0 {
		return
	}
	if r.due(cfg, changed) {
		r.startRun(send, cfg)
	}
}

// startRun 在后台生成并发一份(同一时间只有一份在跑)。
func (r *replicator) startRun(ids []uint, cfg Config) {
	r.mu.Lock()
	if r.running {
		r.mu.Unlock()
		return
	}
	r.running = true
	r.mu.Unlock()
	go func() {
		defer func() {
			r.mu.Lock()
			r.running = false
			r.mu.Unlock()
		}()
		r.run(ids, cfg)
	}()
}

// justBecame 本机刚成为这个纪元的主机(这次启动之前不久)。
func (r *replicator) justBecame() bool {
	var b Became
	if !r.s.getJSON(KeyBecame, &b) || b.Epoch != r.s.Epoch() {
		return false
	}
	return r.s.now().Unix()-b.At < int64(30*time.Minute/time.Second)
}

// firstReplica 刚成为主机:按组新库时带过来的名单(原主机生成副本时的备用,去掉本机)马上发第一份。
func (r *replicator) firstReplica(cfg Config) {
	r.mu.Lock()
	done := r.firstDone
	r.firstDone = true
	r.mu.Unlock()
	if done {
		return
	}
	var ids []uint
	for _, id := range r.savedIds() {
		var n model.Node
		if r.s.d.DB.Where("id = ? AND enabled = ?", id, true).Limit(1).Find(&n).Error != nil || n.Id == 0 || n.IsLocal {
			continue
		}
		if r.s.setting(KeyPubPrefix+strconv.FormatUint(uint64(id), 10)) == "" {
			continue
		}
		ids = append(ids, id)
	}
	if len(ids) > cfg.Standbys {
		ids = ids[:cfg.Standbys]
	}
	if len(ids) > 0 {
		logger.Info("容灾:刚成为主机,先给备用 [", idsKey(ids), "] 发第一份副本")
		r.startRun(ids, cfg)
	}
}

// setList 记下名单;名单变了推一轮容灾状态、存进本机设置(下次启动先用它)。返回名单变了没有。
func (r *replicator) setList(infos []StandbyInfo, ids []uint, persist bool) bool {
	hadList := r.s.setting(KeyStandbyList) != "" // 第一次开启时还没有名单:那时的"新面孔"不提示
	saved := r.savedIds()
	r.mu.Lock()
	prev := r.ids
	if !r.computed {
		prev = saved
	}
	r.infos, r.ids, r.computed = infos, ids, true
	r.mu.Unlock()
	if persist && idsKey(saved) != idsKey(ids) {
		b, _ := json.Marshal(ids)
		if ids == nil {
			b = []byte("[]")
		}
		_ = r.s.set(map[string]string{KeyStandbyList: string(b)})
	}
	if persist {
		r.checkShort(infos, ids)
	}
	if idsKey(ids) == idsKey(prev) {
		return false
	}
	logger.Info("容灾:备用名单 [", idsKey(ids), "]")
	if persist && hadList {
		r.noteNewFaces(infos, ids, prev)
	}
	r.s.invalidate()
	r.kick()
	return true
}

// noteNewFaces 名单里出现了新面孔:它马上会收到全部数据的加密副本,告诉管理员(第一次开启时不算)。
func (r *replicator) noteNewFaces(infos []StandbyInfo, ids, prev []uint) {
	for i, id := range ids {
		if containsId(prev, id) {
			continue
		}
		name := "#" + strconv.FormatUint(uint64(id), 10)
		for _, c := range infos {
			if c.Id == id {
				name = c.Name
			}
		}
		r.s.addNotice(Notice{Kind: "standbyNew", Name: name, N: int64(i + 1)})
		r.s.notify(fmt.Sprintf("🟡 <b>新的备用</b>:%s 成为备用 %d,会收到主机全部数据的加密副本。\n不希望它当备用:在服务器列表里把它排到后面,或停用它。", notify.Esc(name), i+1))
	}
}

// checkShort 合格的备用比设定的少:告警一次(恢复后再少会再告警)。
func (r *replicator) checkShort(infos []StandbyInfo, ids []uint) {
	cfg := r.s.Config()
	if cfg.Mode == ModeOff || infos == nil {
		return
	}
	ok := len(sendable(infos, ids))
	if ok >= cfg.Standbys {
		r.mu.Lock()
		delete(r.alerted, 0)
		r.mu.Unlock()
		return
	}
	var held []string
	for _, c := range infos {
		if c.Held {
			held = append(held, c.Name)
		}
	}
	text := fmt.Sprintf("🟡 <b>合格的备用不够</b>:想要 %d 台,现在能收副本的只有 %d 台。", cfg.Standbys, ok)
	if len(held) > 0 {
		text += "\n保留名次、暂时不发副本的:" + notify.Esc(strings.Join(held, ", ")) + "(版本比主机旧的先升级;离线的等它回来)。"
	}
	text += "\n到设置 →「主机容灾」看就绪检查。"
	r.alertOnce(0, "short", text)
}

func (r *replicator) savedIds() []uint {
	var saved []uint
	_ = json.Unmarshal([]byte(r.s.setting(KeyStandbyList)), &saved)
	return saved
}

func idsKey(ids []uint) string {
	parts := make([]string, len(ids))
	for i, id := range ids {
		parts[i] = strconv.FormatUint(uint64(id), 10)
	}
	return strings.Join(parts, ",")
}

// dropOthers 不在名单里、手里却还有副本的副机:让它删掉(10 分钟最多催一次)。保留名次的不在此列。
func (r *replicator) dropOthers(infos []StandbyInfo, ids []uint) {
	for _, c := range infos {
		if c.Rank > 0 || c.Held || c.Replica == nil || containsId(ids, c.Id) {
			continue
		}
		r.mu.Lock()
		last := r.dropAt[c.Id]
		if r.s.now().Sub(last) < 10*time.Minute {
			r.mu.Unlock()
			continue
		}
		r.dropAt[c.Id] = r.s.now()
		r.mu.Unlock()
		var n model.Node
		if r.s.d.DB.Where("id = ?", c.Id).Limit(1).Find(&n).Error != nil || n.Id == 0 {
			continue
		}
		go r.s.DropOn(n)
	}
}

// DropOn 让一台副机删掉手里的副本(停用、删除、移出名单时;尽力而为)。
func (s *Service) DropOn(n model.Node) {
	if s.d.Hub == nil || n.ApiUrl == "" || n.Token == "" || s.Epoch() == 0 || s.IsNode() {
		return
	}
	if err := s.d.Hub.Send(n, http.MethodPost, "ha/drop", nil, 0, nil, 15*time.Second, nil); err != nil {
		logger.Warning("容灾:让副机 ", n.Name, " 删掉副本失败: ", err)
		return
	}
	logger.Info("容灾:副机 ", n.Name, " 已删掉手里的副本")
}

// due 该不该生成一份了。
func (r *replicator) due(cfg Config, listChanged bool) bool {
	s := r.s
	now := s.now()
	var maxChange uint64
	s.d.DB.Raw("SELECT COALESCE(MAX(id), 0) FROM changes").Scan(&maxChange)
	r.mu.Lock()
	lastMax := r.lastMax
	r.mu.Unlock()
	// 只有管理员一侧的改动才值得提前补一份:定时任务(重置、耗尽、规则)、容灾自己、系统的记录不算
	var counted int64
	if maxChange != lastMax {
		s.d.DB.Raw("SELECT COUNT(*) FROM changes WHERE id > ? AND key <> 'ha' AND actor NOT IN ('ResetJob', 'DepleteJob', 'RuleJob', 'system')",
			lastMax).Scan(&counted)
	}
	kicked := false
	select {
	case <-r.kickc:
		kicked = true
	default:
	}
	r.mu.Lock()
	defer r.mu.Unlock()
	if counted > 0 && !r.lastBuild.IsZero() && r.changedAt.IsZero() {
		r.changedAt = now
	}
	if kicked && !r.lastBuild.IsZero() && now.Sub(r.lastBuild) < replicaMinGap {
		r.kickLater()
	}
	if !r.lastBuild.IsZero() && now.Sub(r.lastBuild) < replicaMinGap {
		return false
	}
	interval := time.Duration(cfg.ReplicaMinutes) * time.Minute
	if adapt := 4 * r.lastCost; adapt > interval {
		interval = adapt
	}
	// 改动触发的补发也按耗时放宽:一份要做很久的大库,不能每 30 秒就重做一份
	gap := max(replicaMinGap, 4*r.lastCost)
	switch {
	case r.lastBuild.IsZero(), kicked, listChanged, s.Epoch() != r.lastEpoch:
		return true
	case now.Sub(r.lastBuild) >= interval:
		return true
	case !r.changedAt.IsZero() && now.Sub(r.changedAt) >= replicaDebounce && now.Sub(r.lastBuild) >= gap:
		return true
	}
	return false
}

// kickLater 闸还没到:记着下一次到点时补一份(调用方持有 r.mu)。
func (r *replicator) kickLater() {
	select {
	case r.kickc <- struct{}{}:
	default:
	}
}

// ReplicateNow 立刻生成一份并发给当前名单(测试用;跳过节奏控制)。
func (s *Service) ReplicateNow() error {
	if s.Epoch() == 0 || s.IsNode() {
		return errors.New("本机不是开着主机容灾的主机")
	}
	cfg := s.Config()
	if cfg.Mode == ModeOff {
		return errors.New("主机容灾没有开启")
	}
	infos, ids := s.rep.compute(cfg)
	s.rep.setList(infos, ids, true)
	send := sendable(infos, ids)
	if len(send) == 0 {
		return errors.New("没有合格的备用")
	}
	return s.rep.run(send, cfg)
}

// run 生成一份副本,加密一次,发给名单里的每台备用。
func (r *replicator) run(ids []uint, cfg Config) error {
	s := r.s
	start := s.now()
	epoch := s.Epoch()
	if err := ensureDir(s.Dir()); err != nil {
		return err
	}
	dir, err := os.MkdirTemp(s.Dir(), "build-")
	if err != nil {
		return err
	}
	defer os.RemoveAll(dir)
	zipPath, meta, err := s.buildReplica(dir)
	if err != nil {
		r.mu.Lock()
		r.lastErr, r.lastBuild = err.Error(), start
		r.mu.Unlock()
		logger.Warning("容灾:生成副本失败: ", err)
		return err
	}
	zst, err := os.Stat(zipPath)
	if err != nil {
		return err
	}
	size := zst.Size()
	if size > replicaWarnSize {
		r.mu.Lock()
		warn := s.now().Sub(r.warnedBig) > 24*time.Hour
		if warn {
			r.warnedBig = s.now()
		}
		r.mu.Unlock()
		if warn {
			s.notify(fmt.Sprintf("🟡 <b>数据副本偏大</b>:%.1f MB。副本每隔几分钟就发一次给备用;用户或审计日志特别多时可以在「高级」里把副本间隔调长。", float64(size)/(1<<20)))
		}
	}
	// 内容没变、备用手里就是上一份的:不重发,只告诉它"到现在还是最新的"
	var send []uint
	marked := false
	r.mu.Lock()
	for _, id := range ids {
		a := r.acks[id]
		known, have := false, uint64(0)
		if s.d.Hub != nil {
			if st, ok := s.d.Hub.StatusOf(id); ok && st.HA != nil {
				known = true
				if st.HA.Replica != nil && st.HA.Replica.Epoch == epoch {
					have = st.HA.Replica.Seq
				}
			}
		}
		if a != nil && a.epoch == epoch && a.sum == meta.Sum && a.err == "" && (!known || have == a.seq) {
			a.asOf = meta.CreatedAt
			marked = true
			continue
		}
		send = append(send, id)
	}
	r.lastSum, r.lastSize, r.lastErr = meta.Sum, size, ""
	if meta.DBSize > 0 {
		r.lastDB = meta.DBSize
	}
	r.mu.Unlock()
	var sendErr error
	if len(send) > 0 {
		sendErr = r.encryptAndSend(dir, zipPath, size, meta, epoch, send)
	}
	r.mu.Lock()
	r.lastBuild, r.lastCost, r.lastMax, r.lastEpoch = start, s.now().Sub(start), meta.MaxChangeId, epoch
	r.changedAt = time.Time{}
	r.mu.Unlock()
	if marked || len(send) > 0 {
		s.invalidate() // 把"截至"推给备用
	}
	return sendErr
}

// nextSeq 副本序号:只增,不低于当前时钟(主机重启、时钟回拨都不会重复)。
func (s *Service) nextSeq() (uint64, error) {
	last, _ := strconv.ParseUint(s.setting(KeyReplicaSeq), 10, 64)
	seq := uint64(s.now().UnixNano())
	if seq <= last {
		seq = last + 1
	}
	return seq, s.set(map[string]string{KeyReplicaSeq: strconv.FormatUint(seq, 10)})
}

// encryptAndSend 明文 zip 只加密一次(同一个数据密钥),每台备用只换头部里包给它的密钥和签名。
func (r *replicator) encryptAndSend(dir, zipPath string, size int64, meta ReplicaMeta, epoch uint64, ids []uint) error {
	s := r.s
	seq, err := s.nextSeq()
	if err != nil {
		return err
	}
	sign, err := s.signer()
	if err != nil {
		return err
	}
	dataKey := NewDataKey()
	ctPath := filepath.Join(dir, "replica.ct")
	zf, err := os.Open(zipPath)
	if err != nil {
		return err
	}
	cf, err := os.OpenFile(ctPath, os.O_CREATE|os.O_TRUNC|os.O_WRONLY, 0o600)
	if err != nil {
		zf.Close()
		return err
	}
	sum, err := SealStream(cf, dataKey, zf, size)
	zf.Close()
	if cerr := cf.Close(); err == nil {
		err = cerr
	}
	if err != nil {
		return err
	}
	base := EnvHeader{Kind: KindReplica, Epoch: epoch, Seq: seq, CreatedAt: meta.CreatedAt, MasterId: meta.MasterId,
		MasterName: meta.MasterName, Version: meta.Version, Size: size, MaxChangeId: meta.MaxChangeId}
	var wg sync.WaitGroup
	errs := make([]error, len(ids))
	for i, id := range ids {
		var n model.Node
		if s.d.DB.Where("id = ?", id).Limit(1).Find(&n).Error != nil || n.Id == 0 {
			continue
		}
		pub := s.setting(KeyPubPrefix + strconv.FormatUint(uint64(id), 10))
		k, err := ParsePub(pub)
		if err != nil {
			errs[i] = err
			r.noteAck(id, epoch, 0, meta, size, err)
			continue
		}
		hdr := base
		hdr.To = Fingerprint(pub)
		if hdr.Wrap, err = SealTo(dataKey, k); err != nil {
			errs[i] = err
			continue
		}
		gate := r.gate(id)
		select {
		case gate <- struct{}{}:
		default:
			continue // 上一份还没送完:这一轮跳过它
		}
		wg.Add(1)
		go func(i int, n model.Node, hdr EnvHeader) {
			defer wg.Done()
			defer func() { <-gate }()
			// 先把最新的容灾状态推过去:名单刚变时备用还不知道自己是备用,hub 下一轮才会推
			err := s.pushState(n, 15*time.Second)
			if err == nil {
				err = s.sendReplica(n, ctPath, hdr, sum, sign)
			}
			errs[i] = err
			r.noteAck(n.Id, epoch, seq, meta, size, err)
			if err != nil {
				logger.Warning("容灾:给备用 ", n.Name, " 发副本失败: ", err)
				r.noteSendFail(n, err)
			} else {
				r.mu.Lock()
				delete(r.alerted, failKey(n.Id))
				r.mu.Unlock()
			}
		}(i, n, hdr)
	}
	wg.Wait()
	return errors.Join(errs...)
}

// failKey 告警去重表里"发副本连续失败"用的键(和公钥告警分开:id 的高位不会用到)。
func failKey(id uint) uint { return id | 1<<31 }

// noteSendFail 给某台备用发副本失败:同一台连续失败超过 30 分钟告警一次(原因进面板的 ack.err)。
func (r *replicator) noteSendFail(n model.Node, err error) {
	r.mu.Lock()
	a := r.acks[n.Id]
	since := int64(0)
	if a != nil {
		since = a.sentAt
	}
	r.mu.Unlock()
	if since == 0 || r.s.now().Unix()-since < int64(standbyHold/time.Second) {
		return
	}
	msg := err.Error()
	if rs := []rune(msg); len(rs) > 300 {
		msg = string(rs[:300]) + "…"
	}
	r.alertOnce(failKey(n.Id), "fail", "🟠 <b>备用收不到数据副本</b>:"+notify.Esc(n.Name)+" 已经 30 分钟以上没收到新的副本。\n最近一次的原因:"+notify.Esc(msg))
}

func (r *replicator) gate(id uint) chan struct{} {
	r.mu.Lock()
	defer r.mu.Unlock()
	g := r.gates[id]
	if g == nil {
		g = make(chan struct{}, 1)
		r.gates[id] = g
	}
	return g
}

func (r *replicator) noteAck(id uint, epoch, seq uint64, meta ReplicaMeta, size int64, err error) {
	r.mu.Lock()
	defer r.mu.Unlock()
	a := r.acks[id]
	if a == nil {
		a = &ack{}
		r.acks[id] = a
	}
	if err != nil {
		a.err, a.errAt = err.Error(), r.s.now().Unix()
		return
	}
	*a = ack{epoch: epoch, seq: seq, sum: meta.Sum, size: size, sentAt: r.s.now().Unix(), asOf: meta.CreatedAt}
}

// sendReplica 把一份信封推给一台备用。
func (s *Service) sendReplica(n model.Node, ctPath string, hdr EnvHeader, sum [32]byte, sign func([]byte) []byte) error {
	total, err := EnvelopeSize(hdr, 64)
	if err != nil {
		return err
	}
	ct, err := os.Open(ctPath)
	if err != nil {
		return err
	}
	defer ct.Close()
	pr, pw := io.Pipe()
	go func() {
		pw.CloseWithError(WriteEnvelopeCT(pw, hdr, ct, sum, sign))
	}()
	defer pr.Close()
	timeout := 30*time.Second + time.Duration(total>>20)*2*time.Second
	if timeout > 10*time.Minute {
		timeout = 10 * time.Minute
	}
	var out struct {
		OK  string `json:"ok"`
		Seq uint64 `json:"seq"`
	}
	h := http.Header{}
	h.Set("Content-Type", "application/octet-stream")
	if err := s.d.Hub.Send(n, http.MethodPost, "ha/replica", pr, total, h, timeout, &out); err != nil {
		return err
	}
	if out.Seq != hdr.Seq {
		return fmt.Errorf("备用确认的序号 %d 和发出去的 %d 对不上", out.Seq, hdr.Seq)
	}
	return nil
}
