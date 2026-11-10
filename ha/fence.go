package ha

import (
	"errors"
	"fmt"
	"strconv"
	"sync"
	"time"

	"github.com/Maoyangui/m-ui/database/model"
	"github.com/Maoyangui/m-ui/hub"
	"github.com/Maoyangui/m-ui/logger"
	"github.com/Maoyangui/m-ui/notify"
)

// 旧主机降级(fencing)。
//
// 被接管的旧主机回来后,第一轮推送里可达的(认得纪元的)副机过半回 stale_epoch —— 或者启动自检时就发现了更高的纪元 ——
// 它就降为只读副机:先留一份分叉备份,记下被谁接管、副本之后自己还收了哪些改动,写 nodeMode=true、haRole=fenced,
// 换一本新的流量账本,告警,然后重启(代理面板、Telegram 机器人只在启动时按角色拉起)。
//
// 只有一台副机这么说、别的副机还认本机时不降级:那台标成"已脱离"并告警 —— 被攻破的一台副机不能把主机踢下线。

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
	at     time.Time
	kind   replyKind
	epoch  uint64
	master uint
	since  uint64
}

// NoteReply hub 每次请求副机后调用:记下这台的纪元判定,够多数就降级。
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
	s.replies[n.Id] = reply{at: s.now(), kind: k, epoch: r.Epoch, master: r.MasterId, since: r.SinceChange}
	s.mu.Unlock()
	if k == replyStale { // 只有回 stale 才可能要降级:每轮推送都会再回一次,够多数时总有一次判得出来
		s.evaluateFence(false)
	}
}

// fenceTally 一次降级判定的计数。
type fenceTally struct {
	total, aware, stale, unknown int
	best                         reply // 回 stale 的里面纪元最高的那条
	staleIds                     []uint
}

// tally 统计启用的副机最近的回复(只算 freshReplyLimit 以内的;老副机不计)。
func (s *Service) tally() fenceTally {
	var nodes []model.Node
	s.d.DB.Select("id, name").Where("enabled = ? AND is_local = ? AND COALESCE(api_url, '') <> '' AND COALESCE(token, '') <> ''", true, false).Find(&nodes)
	now := s.now()
	var t fenceTally
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
		if r.kind == replyStale {
			t.stale++
			t.staleIds = append(t.staleIds, n.Id)
			if r.epoch > t.best.epoch {
				t.best = r
			}
		}
	}
	return t
}

// shouldFence 够不够降级:回 stale 的占全部(认得纪元的)副机过半,直接降;否则要等每台都回过话,
// 再看是不是占可达的过半。只有一台副机时它一台说了就算。
func (t fenceTally) shouldFence() bool {
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

// evaluateFence 按最近的回复判定要不要降级;不够多数时把回 stale 的副机标成"已脱离"并告警一次。
// startup 为真表示启动自检:每台都已经问过一遍,没回的就当不可达。
func (s *Service) evaluateFence(startup bool) bool {
	t := s.tally()
	if startup {
		t.unknown = 0
	}
	if t.shouldFence() {
		fb := FencedBy{Epoch: t.best.epoch, MasterId: t.best.master, SinceChange: t.best.since, OldEpoch: s.Epoch(), At: s.now().Unix()}
		if err := s.Fence(fb); err != nil {
			logger.Warning("容灾:降级失败: ", err)
		}
		return true
	}
	for _, id := range t.staleIds {
		s.mu.Lock()
		done := s.detached[id]
		s.detached[id] = true
		s.mu.Unlock()
		if !done {
			name := s.nodeName(id)
			logger.Warning("容灾:副机 ", name, " 已跟随别的主机,但多数副机仍认本机,不降级")
			s.notify("🟠 <b>副机已脱离</b>:" + notify.Esc(name) + " 跟随了纪元更高的另一台主机,而多数副机仍认本机。\n" +
				"本机继续当主机;如果确实有人在那台上接管过,到那台服务器的面板上核对(必要时回滚接管)。")
		}
	}
	return false
}

// FencedBy 降级记录:被谁接管、本机原来的纪元、副本之后本机还收了哪些改动。
type FencedBy struct {
	Epoch       uint64 `json:"epoch"`    // 接管方的纪元
	MasterId    uint   `json:"masterId"` // 接管方的服务器 id
	MasterName  string `json:"masterName,omitempty"`
	OldEpoch    uint64 `json:"oldEpoch"`
	SinceChange uint64 `json:"sinceChange"` // 接管所用副本里最大的审计 id:之后的改动没进临时主机
	Divergent   int64  `json:"divergent"`   // 分叉改动条数
	Backup      string `json:"backup,omitempty"`
	At          int64  `json:"at"`
}

// Fence 本机降为只读副机。只做一次;做完安排重启。
func (s *Service) Fence(fb FencedBy) error {
	s.mu.Lock()
	if s.fenced {
		s.mu.Unlock()
		return nil
	}
	s.fenced = true
	s.mu.Unlock()
	fb.MasterName = s.nodeName(fb.MasterId)
	if s.d.Backup != nil {
		name, err := s.d.Backup(fmt.Sprintf("fenced-E%d-", fb.OldEpoch))
		if err != nil {
			logger.Warning("容灾:降级前的分叉备份失败(继续降级): ", err)
		} else {
			fb.Backup = name
		}
	}
	s.d.DB.Model(&model.Change{}).Where("id > ?", fb.SinceChange).Count(&fb.Divergent)
	values := map[string]string{
		"nodeMode": "true", KeyRole: RoleFenced, KeyFencedBy: mustJSON(fb),
		hub.LedgerEpochKey: hub.NewLedgerEpoch(), // 本机的流量账本换一本:以后作为副机加入,主机只建基线
		KeySignKey:         "", KeyFreezeUntil: "", KeyHandover: "", KeyExportCode: "",
	}
	if err := s.set(values); err != nil {
		s.mu.Lock()
		s.fenced = false
		s.mu.Unlock()
		return err
	}
	logger.Warning("容灾:本机已被纪元 ", fb.Epoch, " 的主机 ", fb.MasterName, " 接管,降为只读副机;分叉备份 ", fb.Backup,
		",副本之后本机还有 ", fb.Divergent, " 条改动没进新主机")
	msg := fmt.Sprintf("🟠 <b>本机已被接管,降为只读副机</b>\n「%s」在纪元 %d 接管了主机。本机的数据面照常按最后的配置服务,不再推送配置、不再跑主机任务。",
		notify.Esc(fb.MasterName), fb.Epoch)
	if fb.Divergent > 0 {
		msg += fmt.Sprintf("\n副本之后本机还收了 %d 条改动,没进新主机:在本机面板「查看分叉改动」核对后补录。", fb.Divergent)
	}
	if fb.Backup != "" {
		msg += "\n分叉备份:" + notify.Esc(fb.Backup)
	}
	msg += "\n要让本机重新当主机:在本机面板选「从临时主机恢复为主机」。"
	s.notify(msg)
	s.restart(3 * time.Second)
	return nil
}

// StartupCheck 启动自检:在跑主机任务(推送、并账、判超量……)之前,带着本机纪元把启用的副机都问一遍(最多 10 秒),
// 过半回来更高的纪元就直接降级,不先跑一轮主机任务。返回是否降级了。
func (s *Service) StartupCheck() bool {
	if s.Epoch() == 0 || s.IsNode() || s.d.Hub == nil {
		return false
	}
	var nodes []model.Node
	if err := s.d.DB.Where("enabled = ? AND is_local = ? AND COALESCE(api_url, '') <> '' AND COALESCE(token, '') <> ''", true, false).Find(&nodes).Error; err != nil || len(nodes) == 0 {
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
	return s.evaluateFence(true)
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

// Divergence 降级后:接管所用副本之后,本机还收了哪些改动(新的在前,最多 limit 条),供人工核对补录。
func (s *Service) Divergence(limit int) (FencedBy, []model.Change, error) {
	var fb FencedBy
	if !s.getJSON(KeyFencedBy, &fb) {
		return fb, nil, errNotFenced
	}
	if limit <= 0 || limit > 2000 {
		limit = 500
	}
	var out []model.Change
	err := s.d.DB.Where("id > ?", fb.SinceChange).Order("id desc").Limit(limit).Find(&out).Error
	return fb, out, err
}

// errNotFenced 本机没有降级记录。
var errNotFenced = errors.New("本机没有被接管过,没有分叉改动")
