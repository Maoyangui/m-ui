package hub

// 老副机(0.7.16 及以前,确认推送时不回 ledgerKept)应用完快照会把快照里没有的名字的账本清掉:主机给这些游标打清账标记,
// 之后拉到的报告就知道是清账后从 0 重开的那本(见 markLedgerCleared、MergeReport)。这里是主机这边的流程:
//   - 标记和拉报告谁先谁后看序号(ledgerSeq),不看时钟;
//   - 打之前拉一份报告核对老副机真清了(它清账那条 DELETE 不看错误,没删成也照样确认);
//   - 推送确认、下线推送确认、等确认超时后看报告确认,三条路都打;
//   - 快照里的用户名单没变、这台的游标也没新添或去掉标记,就不再把游标名单整个读一遍。

import (
	"hash/fnv"

	"github.com/Maoyangui/m-ui/database/model"
	"github.com/Maoyangui/m-ui/logger"
)

// checkedReport 一份报告连同拉它时取的序号(见 ledgerSeq)。
type checkedReport struct {
	rep Report
	seq int64
}

// clearSeen 上一次看过要不要打清账标记时,快照的用户名单指纹和这台游标的代数(见 Hub.cursorGen)。
type clearSeen struct {
	names uint64
	gen   uint64
}

// ledgerSeq 发一个序号给清账标记或拉报告:两样谁先谁后只看序号。以前比的是打标记时和拉报告时的 time.Now() ——
// 主机的钟往回拨(NTP)、换主机后新主机的钟比原主机慢,清账之后拉到的报告都会被当成清账之前就在路上的,按增量计、标记留着,
// 等钟走过标记那一刻,下一份又被当成清账后重开的一本整本从 0 计,重开那本被计了不止一次。
// 序号只在本进程里发、严格递增;启动后第一次用时接在库里已有的最大标记后面:上次运行打的、换主机随整库数据或副本带过来的标记,
// 都排在本进程拉的任何一份报告之前 —— 本来也是。同一台副机的推送(打标记)和拉报告都在它的闸里做,先后就是发号的先后。
// 读库失败返回 0(不知道):报告按"不知道是标记之前还是之后拉的"处理,标记按老格式只认计数变小。
func (h *Hub) ledgerSeq() int64 {
	h.seqMu.Lock()
	defer h.seqMu.Unlock()
	if !h.seqReady {
		var top int64
		if err := h.d.DB.Model(&model.TrafficCursor{}).Select("COALESCE(MAX(cleared_at), 0)").Scan(&top).Error; err != nil {
			logger.Warning("读取清账标记的序号失败(这一次按不知道先后处理): ", err)
			return 0
		}
		h.seq, h.seqReady = max(top, 1), true
	}
	h.seq++
	return h.seq
}

// namesPrint 快照里用户名单的指纹:同一修订号的名单一样,按修订号记住上一次算的。
func (h *Hub) namesPrint(snap Snapshot) uint64 {
	h.mu.Lock()
	if snap.Revision != "" && h.printRev == snap.Revision {
		v := h.printVal
		h.mu.Unlock()
		return v
	}
	h.mu.Unlock()
	f := fnv.New64a()
	for _, u := range snap.Users {
		f.Write([]byte(u.Name))
		f.Write([]byte{0})
	}
	v := f.Sum64()
	h.mu.Lock()
	h.printRev, h.printVal = snap.Revision, v
	h.mu.Unlock()
	return v
}

// cursorsChanged 并账给这台副机新建了游标、或者去掉了游标上的清账标记:下一次推给它的快照名单没变,也要重新看要不要打标记。
func (h *Hub) cursorsChanged(nodeId uint) {
	h.mu.Lock()
	if h.cursorGen == nil {
		h.cursorGen = map[uint]uint64{}
	}
	h.cursorGen[nodeId]++
	h.mu.Unlock()
}

// oldAgentApplied 老副机确认应用了快照 snap(推送确认,或者等确认超时后看报告确认):给它清掉的那些账本打清账标记。
// 推给老副机的每份快照都会走到这里,绝大多数时候没有要标的:快照的用户名单和上次看过的一样、这台的游标也没新添或去掉标记,
// 就不再把游标名单整个读一遍(稳定时差不多是全部用户)。got 见 oldAgentCleared;返回在那里拉到的报告。调用方持有这台的闸。
func (h *Hub) oldAgentApplied(n model.Node, snap Snapshot, got *checkedReport) *checkedReport {
	names := h.namesPrint(snap)
	h.mu.Lock()
	gen := h.cursorGen[n.Id] // 先取代数再读游标:读的时候正在并的账,并完会把代数加上去,下一次照样重看
	seen, ok := h.clearSeen[n.Id]
	h.mu.Unlock()
	if ok && seen.names == names && seen.gen == gen {
		return nil
	}
	fetched, done := h.oldAgentCleared(n, userNames(snap), got)
	if done {
		h.mu.Lock()
		if h.clearSeen == nil {
			h.clearSeen = map[uint]clearSeen{}
		}
		h.clearSeen[n.Id] = clearSeen{names: names, gen: gen}
		h.mu.Unlock()
	}
	return fetched
}

// oldAgentCleared 老副机应用了一份快照(kept 是快照里的用户名;停用推的空用户表为 nil),打清账标记(见 clearCandidates、
// markLedgerCleared)。打之前核对它真清了:got 是已经拉到的、应用完之后的报告(看报告确认时就是那一份),nil 就在这里拉一份
// —— 只在真有要标的名字时才拉;拉不到就照旧全打(没清成、又拉不到报告的极少;不打的话清账后重开的那本会被当成账本回退少计)。
// 标记的序号排在核对用的报告之前:那份报告(定时同步拿它当这一轮的报告)和之后拉的都按清账之后的算。
// 返回在这里拉到的报告,以及这一遍是不是做完了(读写都成功、也没有核对出没清成的):没做完的下一次推送再看。调用方持有这台的闸。
func (h *Hub) oldAgentCleared(n model.Node, kept map[string]bool, got *checkedReport) (*checkedReport, bool) {
	cands, err := clearCandidates(h.d.DB, n.Id, kept)
	if err != nil {
		logger.Warning("读取副机 ", n.Name, " 要打清账标记的游标失败(同名用户再出现时第一份变小的计数会按账本回退处理): ", err)
		return nil, false
	}
	if len(cands) == 0 {
		return nil, true
	}
	var fetched *checkedReport
	var mark int64
	var held map[string]model.AgentCounter
	if got != nil {
		mark, held = max(got.seq-1, 0), countersOf(got.rep) // 序号 0 是不知道:标记也按不知道
	} else {
		mark = h.ledgerSeq()
		seq := h.ledgerSeq()
		if rep, err := h.fetchReport(n); err != nil {
			logger.Warning("确认之后拉副机 ", n.Name, " 的报告核对清账失败,照旧给 ", len(cands), " 个用户打清账标记: ", err)
		} else {
			fetched, held = &checkedReport{rep: rep, seq: seq}, countersOf(rep)
		}
	}
	skipped, err := markLedgerCleared(h.d.DB, n.Id, cands, held, mark)
	if err != nil {
		logger.Warning("记下副机 ", n.Name, " 清掉的账本失败(同名用户再出现时第一份变小的计数会按账本回退处理): ", err)
		return fetched, false
	}
	if skipped > 0 {
		logger.Warning("副机 ", n.Name, " 确认应用了快照,但有 ", skipped, " 个用户的账本没清掉(老版本清账出错也照样确认):",
			"这些不打清账标记、照常按增量计,下一次推送再看")
	}
	return fetched, skipped == 0
}

// countersOf 报告里的账本,按用户名。
func countersOf(rep Report) map[string]model.AgentCounter {
	out := make(map[string]model.AgentCounter, len(rep.Counters))
	for _, c := range rep.Counters {
		out[c.UserName] = c
	}
	return out
}
