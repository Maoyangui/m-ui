package ha

import (
	"fmt"
	"strconv"
	"sync"
	"time"

	"github.com/Maoyangui/m-ui/database/model"
	"github.com/Maoyangui/m-ui/hub"
	"github.com/Maoyangui/m-ui/logger"
	"github.com/Maoyangui/m-ui/notify"
)

// 原主机回来后自动变为副机。
//
// 主机失联期间备用以更高的纪元接任了。原主机回来后,第一轮推送里可达的(认得纪元的)副机过半回 stale_epoch ——
// 或者启动自检时就发现了更高的纪元 —— 它就变为副机:先留一份备份(失联期间本机可能还收了改动),记下是谁接任的,
// 写 nodeMode=true,换一本新的流量账本,发通知,然后重启(代理面板、Telegram 机器人只在启动时按角色拉起)。
// 重启后它是 demoted 状态:接受任何更高纪元的主机;新主机发现它回来了,把它那一行重新启用、照常管起来。
//
// 只有一台副机这么说、别的副机还认本机时不变:那台标成"已脱离"并告警 —— 被攻破的一台副机不能把主机踢下线。

type replyKind int

const (
	replyNone replyKind = iota
	replyOK
	replyLegacy // 老副机:不认纪元,不计入
	replyStale
	replyFollowing
	replyNotAdopted
	replyDown
)

type reply struct {
	at       time.Time
	kind     replyKind
	epoch    uint64
	master   uint
	since    uint64
	leaseAgo int64 // 回 stale 的副机距上次收到它那台主机的请求多少秒(主机已经不在了的话会一直涨)
}

// NoteReply hub 每次请求副机后调用:记下这台的纪元判定,够多数就变为副机。
func (s *Service) NoteReply(n model.Node, r hub.EpochReply) {
	if s.Epoch() == 0 || s.IsNode() || n.Id == 0 {
		return
	}
	k := replyLegacy
	switch {
	case r.Down:
		k = replyDown
	case r.Code == CodeStale:
		k = replyStale
	case r.Code == CodeFollowing:
		k = replyFollowing
	case r.Code == CodeNotAdopted:
		k = replyNotAdopted
	case r.Aware:
		k = replyOK
	}
	s.mu.Lock()
	s.replies[n.Id] = reply{at: s.now(), kind: k, epoch: r.Epoch, master: r.MasterId, since: r.SinceChange, leaseAgo: r.LeaseAgo}
	s.mu.Unlock()
	switch k {
	case replyStale: // 回 stale 才可能要变为副机:每轮推送都会再回一次,够多数时总有一次判得出来
		s.evaluateDemote(false)
	case replyFollowing:
		s.evaluateYield()
	}
}

// demoteTally 一次判定的计数。
type demoteTally struct {
	total, aware, stale, unknown int
	ok, following, orphans       int   // 认本机的;还跟着另一台(纪元更低、还活着的)主机的;跟着的主机已经不在了的(stale 且租约过期)
	best                         reply // 回 stale 的里面纪元最高的那条
	followBest                   reply // 回 still_following 的里面纪元最高的那条
	staleIds                     []uint
}

// tally 统计启用的副机最近的回复(只算 freshReplyLimit 以内的;老副机不计)。
func (s *Service) tally() demoteTally {
	nodes := s.remoteNodes()
	now := s.now()
	var t demoteTally
	s.mu.Lock()
	defer s.mu.Unlock()
	for _, n := range nodes {
		r, ok := s.replies[n.Id]
		if !ok || now.Sub(r.at) > freshReplyLimit {
			t.total++
			t.unknown++
			continue
		}
		switch r.kind {
		case replyLegacy:
			continue // 不认纪元的老副机既不算赞成也不算反对
		case replyDown:
			t.total++
			continue
		}
		t.total++
		t.aware++
		switch r.kind {
		case replyOK:
			t.ok++
		case replyFollowing:
			t.following++
			if r.epoch > t.followBest.epoch {
				t.followBest = r
			}
		case replyStale:
			t.stale++
			t.staleIds = append(t.staleIds, n.Id)
			if r.epoch > t.best.epoch {
				t.best = r
			}
			if r.leaseAgo >= int64(LeaseExpire/time.Second) {
				t.orphans++
			}
		}
	}
	return t
}

// majority 本机加上认本机的副机过不过半(按启用的服务器总数算,含本机)。
func (t demoteTally) majority(clusterSize int) bool { return 2*(t.ok+1) > clusterSize }

// shouldDemote 够不够变为副机:回 stale 的占全部(认得纪元的)副机过半,直接变;否则要等每台都回过话,
// 再看是不是占可达的过半。只有一台副机时它一台说了就算。
func (t demoteTally) shouldDemote() bool {
	if t.stale == 0 {
		return false
	}
	if 2*t.stale > t.total {
		return true
	}
	if t.unknown > 0 {
		return false
	}
	return 2*t.stale > t.aware
}

// evaluateDemote 按最近的回复判定要不要变为副机;不够多数时把回 stale 的副机标成"已脱离"并告警一次。
// startup 为真表示启动自检:每台都已经问过一遍,没回的就当不可达。
func (s *Service) evaluateDemote(startup bool) bool {
	t := s.tally()
	if startup {
		t.unknown = 0
	}
	if t.shouldDemote() {
		d := Demoted{Epoch: t.best.epoch, MasterId: t.best.master, SinceChange: t.best.since, OldEpoch: s.Epoch(), At: s.now().Unix()}
		if err := s.Demote(d); err != nil {
			logger.Warning("容灾:变为副机失败: ", err)
		}
		return true
	}
	if !startup && t.orphans > 0 && t.majority(s.clusterSize()) {
		// 有副机跟过一台纪元更高的主机,而那台已经不在了(它们的租约早过期),本机又有多数副机跟着:
		// 以更高的纪元把它们收回来(先给跟着本机的副机发交接标记,它们才肯跟着本机升纪元)
		s.reclaim(t.best.epoch, false)
		return false
	}
	for _, id := range t.staleIds {
		s.mu.Lock()
		done := s.detached[id]
		s.detached[id] = true
		s.mu.Unlock()
		if !done {
			name := s.nodeName(id)
			logger.Warning("容灾:副机 ", name, " 已跟随别的主机,但多数副机仍认本机,本机继续当主机")
			s.notify("🟠 <b>副机已脱离</b>:" + notify.Esc(name) + " 跟随了纪元更高的另一台主机,而多数副机仍认本机。\n" +
				"本机继续当主机。那台(和跟着它的副机)在它那台主机不在之后会自动回到本机;两边一直僵持时,在不要的那台主机的设置里打开「副服务器模式」。")
		}
	}
	return false
}

// Demoted 原主机变为副机的记录:是谁接任的、本机原来的纪元、失联期间本机还收了哪些改动。
type Demoted struct {
	Epoch       uint64 `json:"epoch"`    // 接任方的纪元
	MasterId    uint   `json:"masterId"` // 接任方的服务器 id
	MasterName  string `json:"masterName,omitempty"`
	OldEpoch    uint64 `json:"oldEpoch"`
	SinceChange uint64 `json:"sinceChange"` // 接任所用副本里最大的审计 id:之后的改动没进新主机
	Divergent   int64  `json:"divergent"`   // 失联期间本机收的改动条数
	Backup      string `json:"backup,omitempty"`
	At          int64  `json:"at"`
	Yield       bool   `json:"yield,omitempty"` // 不是原主机回来,而是本机在原主机还在时被误接任、又让了出来
}

// Demote 本机变为副机。只做一次;做完安排重启。
func (s *Service) Demote(d Demoted) error {
	s.mu.Lock()
	if s.demoting {
		s.mu.Unlock()
		return nil
	}
	s.demoting = true
	s.mu.Unlock()
	d.MasterName = s.nodeName(d.MasterId)
	if s.d.Backup != nil {
		name, err := s.d.Backup(fmt.Sprintf("former-master-E%d-", d.OldEpoch))
		if err != nil {
			logger.Warning("容灾:变为副机前的备份失败(继续): ", err)
		} else {
			d.Backup = name
		}
	}
	s.d.DB.Model(&model.Change{}).Where("id > ?", d.SinceChange).Count(&d.Divergent)
	values := map[string]string{
		"nodeMode": "true", KeyRole: RoleDemoted, KeyDemoted: mustJSON(d),
		hub.LedgerEpochKey: hub.NewLedgerEpoch(), // 本机的流量账本换一本:作为副机加入后,新主机只建基线
		KeySignKey:         "", KeyFreezeUntil: "", KeyMasterId: strconv.FormatUint(uint64(d.MasterId), 10),
	}
	if err := s.set(values); err != nil {
		s.mu.Lock()
		s.demoting = false
		s.mu.Unlock()
		return err
	}
	logger.Warning("容灾:主机是「", d.MasterName, "」(纪元 ", d.Epoch, "),本机变为副机;备份 ", d.Backup,
		",本机还有 ", d.Divergent, " 条改动没进那台主机")
	msg := fmt.Sprintf("🟠 <b>本机已变为副机</b>\n本机失联期间「%s」已接任主机。本机继续用自己的线路服务,不再推送配置、不再跑主机任务,新主机会把本机当副机管起来。",
		notify.Esc(d.MasterName))
	if d.Yield {
		msg = fmt.Sprintf("🟠 <b>本机已让出主机</b>\n多数副机一直还跟着原来的主机「%s」:它其实还在。本机变回副机,继续用自己的线路服务,那台主机会把本机收回去。",
			notify.Esc(d.MasterName))
	}
	if d.Divergent > 0 {
		msg += fmt.Sprintf("\n失联期间本机还收了 %d 条改动,没进新主机,已留备份:%s", d.Divergent, notify.Esc(d.Backup))
	}
	msg += "\n想让本机重新当主机:在新主机的服务器列表里对本机「设为主机」。"
	s.notify(msg)
	action := "demoted"
	if d.Yield {
		action = "yielded"
	}
	s.audit("ha", action, map[string]interface{}{"master": d.MasterName, "epoch": d.Epoch, "divergent": d.Divergent, "backup": d.Backup})
	s.addNotice(Notice{Kind: "demoted", Name: d.MasterName, N: d.Divergent, File: d.Backup, At: d.At})
	s.restart(3 * time.Second)
	return nil
}

// StartupCheck 启动自检:在跑主机任务(推送、并账、判超量……)之前,带着本机纪元把启用的副机都问一遍(最多 10 秒),
// 过半回来更高的纪元就直接变为副机,不先跑一轮主机任务。返回是否变了。
func (s *Service) StartupCheck() bool {
	if s.Epoch() == 0 || s.IsNode() || s.d.Hub == nil {
		return false
	}
	nodes := s.remoteNodes()
	if len(nodes) == 0 {
		return false
	}
	var wg sync.WaitGroup
	for _, n := range nodes {
		wg.Add(1)
		go func(n model.Node) {
			defer wg.Done()
			_ = s.d.Hub.Send(n, "GET", "ping", nil, 0, nil, 10*time.Second, nil) // 结果经 NoteReply 记下
		}(n)
	}
	wg.Wait()
	t := s.tally()
	logger.Info("容灾启动自检:副机 ", t.total, " 台,认得纪元的 ", t.aware, " 台,其中 ", t.stale, " 台已跟随更高纪元")
	return s.evaluateDemote(true)
}

// replyView 给面板看的各副机回复。
type replyView struct {
	Kind   string `json:"kind"`
	At     int64  `json:"at"`
	Epoch  uint64 `json:"epoch,omitempty"`
	Master uint   `json:"master,omitempty"`
}

func (s *Service) replyViews() map[string]replyView {
	names := map[replyKind]string{replyOK: "ok", replyLegacy: "legacy", replyStale: "stale", replyFollowing: "following", replyNotAdopted: "notAdopted", replyDown: "down"}
	s.mu.Lock()
	defer s.mu.Unlock()
	out := map[string]replyView{}
	for id, r := range s.replies {
		out[strconv.FormatUint(uint64(id), 10)] = replyView{Kind: names[r.kind], At: r.at.Unix(), Epoch: r.epoch, Master: r.master}
	}
	return out
}

// evaluateYield 本机是主机,但多数副机一直回 still_following(它们还跟着另一台纪元更低、仍然活着的主机),
// 本机自己又凑不够多数:说明是在主机其实还在的时候被误接任了。持续超过租约期再加一分钟,本机让出主机、变为副机;
// 那台主机随后以更高的纪元把本机和跟着本机的副机收回去(见 reclaim)。
func (s *Service) evaluateYield() {
	t := s.tally()
	now := s.now()
	s.mu.Lock()
	if 2*t.following <= t.aware || t.majority(s.clusterSize()) {
		s.yieldSince = time.Time{}
		s.mu.Unlock()
		return
	}
	if s.yieldSince.IsZero() {
		s.yieldSince = now
	}
	since := s.yieldSince
	s.mu.Unlock()
	if now.Sub(since) < LeaseExpire+time.Minute {
		return
	}
	d := Demoted{Epoch: t.followBest.epoch, MasterId: t.followBest.master, OldEpoch: s.Epoch(), At: now.Unix(), Yield: true}
	if err := s.Demote(d); err != nil {
		logger.Warning("容灾:让出主机失败: ", err)
	}
}

// reclaim 以更高的纪元把跟过别的主机的副机收回来:先给认本机的副机发交接标记(纪元 E' 的主机还是本机),
// 再把本机纪元升到 E'。force 为真时(命令行 m-ui ha takeover --force)不管多数,也不限频。
func (s *Service) reclaim(seen uint64, force bool) (uint64, error) {
	if s.IsNode() || s.Epoch() == 0 {
		return 0, fmt.Errorf("本机不是主机")
	}
	s.mu.Lock()
	if !force && s.now().Sub(s.reclaimAt) < 5*time.Minute {
		s.mu.Unlock()
		return 0, nil
	}
	s.reclaimAt = s.now()
	s.mu.Unlock()
	L := s.Epoch()
	next := max(L, seen) + 1
	self := s.SelfId()
	s.setGrantOut(&Grant{Epoch: next, To: self, At: s.now().Unix()})
	defer s.setGrantOut(nil)
	s.invalidate()
	var followers []model.Node
	s.mu.Lock()
	now := s.now()
	for _, n := range s.remoteNodes() {
		if r, ok := s.replies[n.Id]; force || (ok && r.kind == replyOK && now.Sub(r.at) <= freshReplyLimit) {
			followers = append(followers, n)
		}
	}
	s.mu.Unlock()
	granted := s.pushAll(followers, 15*time.Second)
	if err := s.set(map[string]string{KeyEpoch: strconv.FormatUint(next, 10), KeyMasterId: strconv.FormatUint(uint64(self), 10)}); err != nil {
		return 0, err
	}
	s.invalidate()
	s.rep.kick()
	s.audit("ha", "reclaim", map[string]interface{}{"epoch": next, "from": L, "granted": granted, "force": force})
	logger.Warning("容灾:以纪元 ", next, " 收回跟过别的主机的副机(原纪元 ", L, ",已给 ", granted, " 台发交接标记)")
	s.notify(fmt.Sprintf("🟠 <b>已收回脱离的副机</b>:有副机跟过一台纪元更高、现在已经不在的主机。本机以纪元 %d 继续当主机,它们会自动回来。", next))
	return next, nil
}

// setGrantOut 这一轮随容灾状态推给副机的交接标记(收回副机时用;设为主机的交接标记在 switch.go)。
func (s *Service) setGrantOut(g *Grant) {
	s.mu.Lock()
	s.grantOut = g
	s.mu.Unlock()
}

func (s *Service) pendingGrant() *Grant {
	s.mu.Lock()
	defer s.mu.Unlock()
	return s.grantOut
}
