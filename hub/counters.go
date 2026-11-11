package hub

// 并账:把副机报上来的单调账本按游标并入主机(只计增量)。
//
// 以前整份报告在一笔 BEGIN IMMEDIATE 里逐个计数器查游标、写用户、写时序、写游标:成本和这台机器上用过的全部用户数成正比,
// 有没有流量都一样,两万个计数器一笔事务就占写锁好几秒,别的写者等满 busy_timeout 就 database is locked(审计 I4)。现在:
//   - 游标在事务外一次读进内存,算出哪些计数器变了;没变的(绝大多数)直接跳过,整份都没变就根本不碰写锁;
//   - 变了的每 counterChunk 个一笔短事务,事务里复核这一块的游标(重复报告、并发都幂等),块与块之间让出写锁;
//   - 每笔事务开头确认副机还在:删副机的事务和在途的并账总有先后,已删的副机不会再被写回游标和纪元。

import (
	"errors"
	"fmt"
	"time"

	"github.com/Maoyangui/m-ui/database/model"
	"github.com/Maoyangui/m-ui/logger"

	"gorm.io/gorm"
	"gorm.io/gorm/clause"
)

// counterChunk 并账每笔事务处理的计数器个数。中途失败只回滚这一块:块内的增量和游标同进同退,
// 已提交的块下一份报告按游标跳过,不重复记、不漏记。
const counterChunk = 1000

// nameChunk 按用户名 IN (...) 查询时每批的个数:SQLite 一条语句最多 32766 个变量。
const nameChunk = 500

// 每块提交后让出写锁的时长:和这块占锁的一样久,至少 mergeYield、最多 mergeYieldMax。SQLite 的忙等不排队,
// 背靠背的短事务照样能把别的写者饿到超时;等锁的写者每次最多睡 100 毫秒,空档太短它就醒在两块中间、又撞上下一块。
var (
	mergeYield    = 20 * time.Millisecond
	mergeYieldMax = 100 * time.Millisecond
)

func yieldAfter(held time.Duration) time.Duration {
	return min(max(held, mergeYield), mergeYieldMax)
}

// userAbsent 游标里的建号时间记成它:并账那一刻主机上没有这个用户(已经删了,副机还在报在途流量)。
// 以后同名的用户再建出来就是"删了又建",和老版本留下的、不知道是谁的游标(id 与建号时间都是 0)区分开。
const userAbsent = model.OwnerAbsent

// errNodeGone 副机已经删除:它在途的报告整份丢掉。
var errNodeGone = errors.New("副机已删除")

// MergeResult 一份报告的并账结果。
type MergeResult struct {
	Merged    int  // 有增量、记了账的用户数
	Regressed int  // 同一个用户的计数比游标小:按账本回退处理(游标对齐、这一次不计)
	Recreated int  // 主机上删了又建的同名用户,副机那本也从头记了:从 0 计
	Reopened  int  // 同一个用户的账本被整本清掉后从 0 重开了一本(改名后再改回、老副机停用时清了账):从 0 计
	Restarted int  // 游标不知道是哪一本(主机 / 副机升级或回滚的过渡期),计数比游标小:当成新开的一本从 0 计
	NodeGone  bool // 副机已经删除,整份丢弃
}

// ApplyCounters 把副机的单调账本按游标并入主机,返回并入的用户数。见 MergeCounters。
func ApplyCounters(db *gorm.DB, nodeId uint, nodeName, epoch string, counters []model.AgentCounter, now int64, bucketSeconds int64, ratio float64) (int, error) {
	r, err := MergeCounters(db, nodeId, nodeName, epoch, counters, now, bucketSeconds, ratio)
	return r.Merged, err
}

// MergeCounters 把副机的单调账本按游标并入主机:只计增量。ratio 为该服务器的流量倍率(≤0 视为 1),
// 增量按倍率计入用户用量与时序,游标记原始计数。
//
// 计数器比游标小分两种:主机上同名用户删了又建(游标记下的用户 id / 建号时间对不上),副机那本也从头记了,从 0 计;
// 同一个用户的计数变小是副机账本回退(synchronous=NORMAL 下断电丢了最后几笔提交、虚机快照回滚),
// 游标对齐到现在的值、这一次不计 —— 以前一律当回绕,把整段历史再记一遍(回退 15 MB,多记 400 GB)。
//
// 新版副机的账本记着主人(用户 id 与建号时间,见 model.AddAgentCounters),游标也记账本主人:主人和游标对不上就是
// 同名用户删了又建后新开的一本,不管计数比游标大还是小都从 0 计。只靠"比游标小"会漏:新用户在这台机器上用得和前人
// 一样多或更多时,首份报告追上旧游标,以前只计差值、相等时整份当"没变化"跳过(Azure 真机 run3,13 个用户的副机流量全漏)。
// 主人一样、账本代号(LedgerId)不一样也是新开的一本:副机账本被整本清掉后同一个人从 0 重开(改名后再改回),
// 以前同一主人计数比游标小当成账本回退(对齐不计,还误报)、追上游标只计差值,每个用户漏掉 min(首份, 旧游标)。
// 账本主人和主机上现在这个同名用户对不上(主机已经删了又建、副机还没收到快照时报来的旧账本),只计服务器维度。
// 老副机 / 升级前的老账本不报主人,照下面的老办法。
//
// 游标不一定知道这是哪一本:升级是一台一台做的,老版本主机写的游标没有主人和代号(主机升级之前、回滚期间),
// 老副机的报告也不带代号。同一本账照样会碰上这样的游标,不能当成另一本从 0 计 —— 那是把整本账再计一遍
// (先升副机、新副机挂老主机、主机或副机回滚再升回来都会这样)。这时只能按计数判断:没变小就是同一本,按增量;
// 变小了是新开的一本,从 0 计。代价是升级前删掉的同名用户留下的游标遇到重建用户的首份报告不小于它时少计一段,
// 只在过渡期发生一次,比整本重复计费安全。
func MergeCounters(db *gorm.DB, nodeId uint, nodeName, epoch string, counters []model.AgentCounter, now int64, bucketSeconds int64, ratio float64) (MergeResult, error) {
	var res MergeResult
	if bucketSeconds < 1 {
		bucketSeconds = 60
	}
	if ratio <= 0 {
		ratio = 1
	}
	bucket := now - now%bucketSeconds
	if gone, err := nodeGone(db, nodeId); err != nil {
		return res, fmt.Errorf("确认副机是否还在: %w", err)
	} else if gone {
		res.NodeGone = true
		return res, nil
	}
	var curs []model.TrafficCursor
	if err := db.Where("node_id = ?", nodeId).Find(&curs).Error; err != nil {
		return res, fmt.Errorf("读取并账游标: %w", err) // 读失败绝不能当成"没有游标",那会把整段累计值再记一遍
	}
	known := make(map[string]model.TrafficCursor, len(curs))
	for _, c := range curs {
		known[c.UserName] = c
	}
	// 只留有变化的;同名的只留最后一份(副机账本按用户名唯一,这里只是防御)。
	// 计数和游标一样、但账本换了主人(删了又建,新用户在这台机器上恰好用到和前人一样多)不是"没变化"
	changed := make([]model.AgentCounter, 0, 64)
	at := map[string]int{}
	for _, c := range counters {
		if cur, ok := known[c.UserName]; ok && unchanged(cur, c) {
			continue
		}
		if i, ok := at[c.UserName]; ok {
			changed[i] = c
			continue
		}
		at[c.UserName] = len(changed)
		changed = append(changed, c)
	}
	baseline, writeEpoch, err := ledgerEpochState(db, nodeId, epoch, len(curs) > 0)
	if err != nil {
		return res, err
	}
	if baseline {
		err := alignCursors(db, nodeId, epoch, changed)
		if errors.Is(err, errNodeGone) {
			res.NodeGone = true
			return res, nil
		}
		return res, err
	}
	if writeEpoch {
		err := db.Transaction(func(tx *gorm.DB) error {
			if gone, err := nodeGone(tx, nodeId); err != nil {
				return err
			} else if gone {
				return errNodeGone
			}
			return upsertSetting(tx, LedgerEpochSetting(nodeId), epoch)
		})
		if errors.Is(err, errNodeGone) {
			res.NodeGone = true
			return res, nil
		}
		if err != nil {
			return res, err
		}
	}
	defer func() {
		if res.Regressed > 0 {
			logger.Warning("副机 ", nodeName, " 的流量账本有 ", res.Regressed, " 个用户的计数比上次小(断电或快照回滚丢了最近几笔记录),",
				"已按账本回退处理:游标对齐,这一小段不计,之后照常按增量计")
		}
		if res.Recreated > 0 {
			logger.Info("副机 ", nodeName, " 上有 ", res.Recreated, " 个同名用户是删了重建的,从 0 开始计")
		}
		if res.Reopened > 0 {
			logger.Info("副机 ", nodeName, " 上有 ", res.Reopened, " 个用户的账本是清掉后重开的(比如改名后又改回),从 0 开始计")
		}
		if res.Restarted > 0 {
			logger.Info("副机 ", nodeName, " 上有 ", res.Restarted, " 个用户的计数比上次小,游标是升级或回滚过渡期留下的、",
				"不知道是哪一本,按新开的一本从 0 开始计")
		}
	}()
	for len(changed) > 0 {
		k := min(len(changed), counterChunk)
		t0 := time.Now()
		r, err := mergeChunk(db, nodeId, changed[:k], now, bucket, ratio, nodeName)
		held := time.Since(t0)
		changed = changed[k:]
		res.Merged += r.Merged
		res.Regressed += r.Regressed
		res.Recreated += r.Recreated
		res.Reopened += r.Reopened
		res.Restarted += r.Restarted
		if errors.Is(err, errNodeGone) {
			res.NodeGone = true
			return res, nil
		}
		if err != nil {
			return res, err
		}
		// 让出写锁:面板写入、统计任务在两块之间(以及并完这台、并下一台之前)插进来
		time.Sleep(yieldAfter(held))
	}
	return res, nil
}

// mergeChunk 并一块计数器:一笔短事务,事务里复核游标。
func mergeChunk(db *gorm.DB, nodeId uint, part []model.AgentCounter, now, bucket int64, ratio float64, nodeName string) (MergeResult, error) {
	var res MergeResult
	err := db.Transaction(func(tx *gorm.DB) error {
		res = MergeResult{}
		if gone, err := nodeGone(tx, nodeId); err != nil {
			return err
		} else if gone {
			return errNodeGone
		}
		names := make([]string, len(part))
		for i, c := range part {
			names[i] = c.UserName
		}
		cursors, err := cursorsOf(tx, nodeId, names)
		if err != nil {
			return err
		}
		users, err := userIdentities(tx, names)
		if err != nil {
			return err
		}
		var nodeUp, nodeDown int64
		rows := make([]model.Stats, 0, 2*len(part)+2)
		next := make([]model.TrafficCursor, 0, len(part))
		for _, c := range part {
			old, hasOld := cursors[c.UserName]
			if hasOld && unchanged(old, c) {
				continue // 读游标之后已经有人并过(重复的报告)
			}
			u, hasUser := users[c.UserName]
			owned := ownedLedger(c)
			if owned && hasUser && (u.Id != c.UserId || u.CreatedAt != c.UserCreatedAt) {
				// 副机报的是前一个同名用户那本账(主机上已经删了又建,副机还没收到快照),或者副机那边已经没有这个用户:
				// 在途流量不算到现在这个同名用户头上,服务器维度照记
				hasUser = false
			}
			dUp, dDown := c.Up, c.Down // 没有游标:从 0 计
			dropped := hasOld && (c.Up < old.Up || c.Down < old.Down)
			switch {
			case !hasOld:
			case owned && sameLedger(old, c):
				// 游标记的就是这一本。代号是随机取的,对得上就是副机上同一行账 —— 老主机回滚期间只改过计数也是这一行的
				if dropped {
					res.Regressed++ // 同一本账计数变小:账本回退,对齐不计
					dUp, dDown = 0, 0
				} else {
					dUp, dDown = c.Up-old.Up, c.Down-old.Down
				}
			case owned && cursorTrusted(old) && old.PrevLedgerId != 0 && old.PrevLedgerId == c.LedgerId:
				// 报上来的是游标换下去的上一本:副机的库回到了重开之前(虚机快照回滚)。按账本回退对齐、告警,
				// 不当成又重开了一本、把这一本从 0 再计一遍
				res.Regressed++
				dUp, dDown = 0, 0
			case owned && cursorKnowsOwner(old) && !sameOwner(old, c):
				// 账本记着主人,和游标记的不是同一个人:同名用户删了又建,副机那本是新开的、从 0 记的。
				// 不管计数比游标大还是小都从 0 计 —— 以前只认"比游标小",新用户首份报告追上旧游标时只计差值、相等时整份跳过
				res.Recreated++
			case owned && cursorKnowsOwner(old) && old.LedgerId != 0:
				// 主人一样、不是游标记的那一本:这个用户的账本被整本清掉后从 0 重开了(改名后再改回)。
				// 不能当账本回退对齐不计,也不能只计比游标多出来的那段
				res.Reopened++
			case owned && cursorTrusted(old) && old.Cleared:
				res.Reopened++ // 老副机清掉过这个人的账本(停用推的空用户表、改名或删除,见 markLedgerCleared),升级后新开的这一本从 0 记
			case owned:
				// 游标不知道是哪一本:老版本主机写的(主机升级之前、回滚期间),或者是按老副机的报告写的(没有代号)。
				// 同一本账也会碰上这样的游标,只能按计数判断:没变小就是同一本,按增量;变小了是新开的一本,从 0 计
				if dropped {
					res.Restarted++
				} else {
					dUp, dDown = c.Up-old.Up, c.Down-old.Down
				}
			default:
				// 老副机 / 升级前的老账本不报主人,只能按计数猜
				known := cursorKnowsOwner(old) // 游标记得当时是谁(老游标、老主机动过的不知道)
				recreated := hasUser && known && (old.UserId != u.Id || old.UserCreatedAt != u.CreatedAt)
				switch {
				case dropped && recreated:
					res.Recreated++ // 主机上删了又建,副机那本也从头记了:从 0 计
				case dropped && cursorTrusted(old) && old.Cleared:
					// 老副机清掉过这个人的账本(停用推的空用户表、改名或删除,见 markLedgerCleared),这是之后从 0 记的一本:
					// 从 0 计,不告警(停用再启用、改名后又改回)
					res.Reopened++
				case dropped:
					res.Regressed++ // 同一个用户(或老游标、已删的用户)计数变小:账本回退,对齐不计
					dUp, dDown = 0, 0
				default:
					// 计数没变小就按游标算增量。删了又建但副机那本没清(同一轮同步里删了又建、或者那台当时失联)也走这里:
					// 从 0 计会把前一个同名用户在这台机器上的全部用量记到新用户头上
					dUp, dDown = c.Up-old.Up, c.Down-old.Down
				}
			}
			next = append(next, cursorFor(nodeId, c, u, hasUser, old, hasOld))
			if dUp <= 0 && dDown <= 0 {
				continue
			}
			if ratio != 1 {
				dUp, dDown = int64(float64(dUp)*ratio), int64(float64(dDown)*ratio)
			}
			res.Merged++
			nodeUp += maxInt64(dUp, 0)
			nodeDown += maxInt64(dDown, 0)
			if !hasUser {
				// 用户已经删了(副机还在报在途流量):不记用户时序 —— 孤儿行会让以后同名的新用户看到前人的历史(审计 MB28);
				// 服务器维度照记
				continue
			}
			update := map[string]interface{}{"online_at": now}
			if dUp > 0 {
				update["up"] = gorm.Expr("up + ?", dUp)
			}
			if dDown > 0 {
				update["down"] = gorm.Expr("down + ?", dDown)
			}
			model.StartOnFirstUse(update, now)
			if err := tx.Model(&model.User{}).Where("id = ?", u.Id).Updates(update).Error; err != nil {
				return err
			}
			if dUp > 0 {
				rows = append(rows, model.Stats{DateTime: bucket, Resource: "user", Tag: c.UserName, Direction: true, Traffic: dUp})
			}
			if dDown > 0 {
				rows = append(rows, model.Stats{DateTime: bucket, Resource: "user", Tag: c.UserName, Direction: false, Traffic: dDown})
			}
		}
		if nodeUp > 0 {
			rows = append(rows, model.Stats{DateTime: bucket, Resource: "node", Tag: nodeName, Direction: true, Traffic: nodeUp})
		}
		if nodeDown > 0 {
			rows = append(rows, model.Stats{DateTime: bucket, Resource: "node", Tag: nodeName, Direction: false, Traffic: nodeDown})
		}
		if len(rows) > 0 {
			if err := tx.Clauses(clause.OnConflict{
				Columns:   []clause.Column{{Name: "resource"}, {Name: "tag"}, {Name: "date_time"}, {Name: "direction"}},
				DoUpdates: clause.Assignments(map[string]interface{}{"traffic": gorm.Expr("stats.traffic + excluded.traffic")}),
			}).CreateInBatches(&rows, counterChunk).Error; err != nil {
				return err
			}
		}
		return upsertCursors(tx, next)
	})
	if err != nil {
		return MergeResult{}, err // 整块回滚了,这一块什么都没记
	}
	return res, nil
}

// alignCursors 新的一本账只建基线:游标对齐到报上来的计数,这一份不计增量。和记下纪元在同一笔事务里 ——
// 拆开的话中途失败,下一份会把还没对齐的用户当成"有游标的老副机",整段历史再记一遍。
// 只在副机新加、删了再加、从备份还原时发生一次,所以不分块。
func alignCursors(db *gorm.DB, nodeId uint, epoch string, counters []model.AgentCounter) error {
	return db.Transaction(func(tx *gorm.DB) error {
		if gone, err := nodeGone(tx, nodeId); err != nil {
			return err
		} else if gone {
			return errNodeGone
		}
		names := make([]string, len(counters))
		for i, c := range counters {
			names[i] = c.UserName
		}
		users, err := userIdentities(tx, names)
		if err != nil {
			return err
		}
		rows := make([]model.TrafficCursor, 0, len(counters))
		for _, c := range counters {
			u, ok := users[c.UserName]
			rows = append(rows, cursorFor(nodeId, c, u, ok, model.TrafficCursor{}, false)) // 新的一本账:旧游标记的都不算数
		}
		if err := upsertCursors(tx, rows); err != nil {
			return err
		}
		return upsertSetting(tx, LedgerEpochSetting(nodeId), epoch)
	})
}

// ledgerEpochState 按账本纪元判断这一份报告是不是只建基线、要不要记下纪元。以前副机账本没有身份:删了再加(主机游标随副机删掉)、
// 副机从备份还原(计数倒退,被当成回绕从 0 计)、同一台机器加两次,都会把整段历史流量重算一遍。
//   - 纪元变了:换了一本账,只建基线;
//   - 这台第一次报纪元:有游标说明是升级上来的老副机,照常计(不丢流量);没有游标(新加的、删了再加的)只建基线;
//   - 同一纪元记在另一台还在的副机上:不计入(DuplicateLedgerError)。已删副机留下的纪元不算 ——
//     老版本删副机时和在途并账竞态,会把纪元写回已删的 id,同一台机器重新加回后永远被当成重复账本;
//   - 老副机不报纪元(epoch 为空):照旧。
func ledgerEpochState(db *gorm.DB, nodeId uint, epoch string, hasCursors bool) (baseline, write bool, err error) {
	if epoch == "" {
		return false, false, nil
	}
	key := LedgerEpochSetting(nodeId)
	var prev string
	if err := db.Model(&model.Setting{}).Select("value").Where("key = ?", key).Scan(&prev).Error; err != nil {
		return false, false, err
	}
	if prev == epoch {
		return false, false, nil
	}
	var other []uint
	if err := db.Raw("SELECT n.id FROM settings s JOIN nodes n ON s.key = 'ledgerEpoch:' || n.id WHERE s.value = ? AND n.id <> ? LIMIT 1",
		epoch, nodeId).Scan(&other).Error; err != nil {
		return false, false, err
	}
	if len(other) > 0 {
		return false, false, &DuplicateLedgerError{Other: other[0]}
	}
	baseline = prev != "" || !hasCursors
	if baseline {
		logger.Info("副机 #", nodeId, " 的流量账本是新的一本(纪元 ", epoch, "),这一份只建基线、不计增量")
	}
	return baseline, true, nil
}

// SweepOrphanLedgers 清掉指向已删副机的游标与账本纪元:老版本删副机时和在途的并账竞态,会把它们写回已删的 id。
// 删副机时与启动后第一轮同步各做一次。
func SweepOrphanLedgers(db *gorm.DB) error {
	return db.Transaction(func(tx *gorm.DB) error {
		if err := tx.Exec("DELETE FROM traffic_cursors WHERE node_id NOT IN (SELECT id FROM nodes)").Error; err != nil {
			return err
		}
		return tx.Exec("DELETE FROM settings WHERE key LIKE 'ledgerEpoch:%' AND substr(key, 13) NOT IN (SELECT CAST(id AS TEXT) FROM nodes)").Error
	})
}

// cursorFor 记到这个计数为止的游标,连同这本账是谁的,计数抄进 StampUp / StampDown(见 cursorTrusted);清账标记随之清掉。
//   - 副机报了账本主人:记账本主人与代号(同名用户删了又建、整本清掉后重开靠它认出新账本);换了一本的话,
//     换下去的那本代号记进 PrevLedgerId,副机的库回滚到重开之前时认得出来;
//   - 老副机 / 升级前的老账本不报:记此刻主机上这个用户是谁。游标原来记的就是这个人的那本账时代号留着 ——
//     副机回滚到老版本又升回来、报出原来的代号,是同一本,不能当成重开再计一遍;
//   - 主机上没有这个用户:记"当时没有这个人"。
func cursorFor(nodeId uint, c model.AgentCounter, u model.User, exists bool, old model.TrafficCursor, hasOld bool) model.TrafficCursor {
	cur := model.TrafficCursor{NodeId: nodeId, UserName: c.UserName, Up: c.Up, Down: c.Down, StampUp: c.Up, StampDown: c.Down}
	switch {
	case ownedLedger(c):
		cur.UserId, cur.UserCreatedAt, cur.LedgerId = c.UserId, c.UserCreatedAt, c.LedgerId
		switch {
		case hasOld && sameLedger(old, c):
			cur.PrevLedgerId = old.PrevLedgerId
		case hasOld && cursorTrusted(old) && old.LedgerId != 0:
			cur.PrevLedgerId = old.LedgerId
		}
	case exists:
		cur.UserId, cur.UserCreatedAt = u.Id, u.CreatedAt
		if hasOld && cursorTrusted(old) && old.UserId == u.Id && old.UserCreatedAt == u.CreatedAt {
			cur.LedgerId, cur.PrevLedgerId = old.LedgerId, old.PrevLedgerId
		}
	default:
		cur.UserId, cur.UserCreatedAt = 0, userAbsent
	}
	return cur
}

// cursorTrusted 游标上记的这本账是谁的(主人、代号、上一本的代号、清账标记)还能不能信。这些都是本版主机连同计数一起写的,
// 写时把计数抄进 StampUp / StampDown;老版本主机(0.7.16:升级之前、回滚期间)只认计数两列,插入的新游标这些列都是 0,
// 已有的只改计数、别的原样留着 —— 回滚期间这个人的账本可能已经换了一本(改名后又改回、删了又建)。
// 计数和抄本对不上就是老主机动过。
func cursorTrusted(cur model.TrafficCursor) bool {
	return cur.StampUp == cur.Up && cur.StampDown == cur.Down
}

// cursorKnowsOwner 游标记得这本账是谁的:能信,而且记了主人(或者"当时没有这个人")。升级前的老游标不知道。
func cursorKnowsOwner(cur model.TrafficCursor) bool {
	return cursorTrusted(cur) && (cur.UserId != 0 || cur.UserCreatedAt == userAbsent)
}

// ownedLedger 副机报了这本账的主人(新版副机、升级后新开的账本):主人的 id 与建号时间,
// 或者"记账时副机上已经没有这个用户"(OwnerAbsent)。都是 0 的是老副机 / 升级前的老账本,不知道主人。
func ownedLedger(c model.AgentCounter) bool {
	return c.UserId != 0 || c.UserCreatedAt == userAbsent
}

// sameOwner 游标记的主人和这本账的主人是不是同一个人。游标记得是谁时(cursorKnowsOwner)才拿来下结论。
func sameOwner(cur model.TrafficCursor, c model.AgentCounter) bool {
	return cur.UserId == c.UserId && cur.UserCreatedAt == c.UserCreatedAt
}

// sameLedger 游标记的就是这一本:主人和账本代号都一样。没有代号的账本(只在升级途中的测试版本上出现过)只能看主人。
func sameLedger(cur model.TrafficCursor, c model.AgentCounter) bool {
	return sameOwner(cur, c) && cur.LedgerId == c.LedgerId
}

// unchanged 这份报告和游标一模一样(同一本账、计数没动),不用并。老副机不报主人,只看计数。
func unchanged(cur model.TrafficCursor, c model.AgentCounter) bool {
	return cur.Up == c.Up && cur.Down == c.Down && (!ownedLedger(c) || sameLedger(cur, c))
}

func nodeGone(db *gorm.DB, nodeId uint) (bool, error) {
	var n int64
	if err := db.Model(&model.Node{}).Where("id = ?", nodeId).Count(&n).Error; err != nil {
		return false, err
	}
	return n == 0, nil
}

// cursorsOf 这台副机上这些用户的游标(按批查)。
func cursorsOf(tx *gorm.DB, nodeId uint, names []string) (map[string]model.TrafficCursor, error) {
	out := make(map[string]model.TrafficCursor, len(names))
	for rest := names; len(rest) > 0; {
		k := min(len(rest), nameChunk)
		var curs []model.TrafficCursor
		if err := tx.Where("node_id = ? AND user_name IN ?", nodeId, rest[:k]).Find(&curs).Error; err != nil {
			return nil, err
		}
		for _, c := range curs {
			out[c.UserName] = c
		}
		rest = rest[k:]
	}
	return out, nil
}

// userIdentities 这些用户名现在对应的用户(只取 id 与建号时间;删了的不在里面)。
func userIdentities(tx *gorm.DB, names []string) (map[string]model.User, error) {
	out := make(map[string]model.User, len(names))
	for rest := names; len(rest) > 0; {
		k := min(len(rest), nameChunk)
		var us []model.User
		if err := tx.Select("id, name, created_at").Where("name IN ?", rest[:k]).Find(&us).Error; err != nil {
			return nil, err
		}
		for _, u := range us {
			out[u.Name] = u
		}
		rest = rest[k:]
	}
	return out, nil
}

func upsertCursors(tx *gorm.DB, rows []model.TrafficCursor) error {
	if len(rows) == 0 {
		return nil
	}
	return tx.Clauses(clause.OnConflict{
		Columns: []clause.Column{{Name: "node_id"}, {Name: "user_name"}},
		DoUpdates: clause.AssignmentColumns([]string{"up", "down", "user_id", "user_created_at", "ledger_id",
			"prev_ledger_id", "stamp_up", "stamp_down", "cleared"}),
	}).CreateInBatches(&rows, counterChunk).Error
}

// markLedgerCleared 老副机(确认推送时不回 ledgerKept 的)确认应用了一份快照:老版本应用完把快照里没有的用户的账本清掉,
// 同名的以后再出现是从 0 重开的一本 —— 停用推的空用户表就是整本清掉(all),平时是主机上已经没有这个名字的
// (改名、删除)。老副机不报账本代号,同一个人计数变小本来只能当账本回退(对齐不计、告警);给这些游标打上清账标记,
// 之后第一次变小按重开从 0 计、不告警。没变小(清账没成、在途的旧报告)照常按增量,标记随这次写入清掉。
// 平时拿主机现在的用户表代替推过去的那份:两者只差推送前后这一刻改过的几个名字,对不上的那几个最多白打一次标记。
// 标记要靠计数抄本才认得出之后有没有被老版本主机动过:老主机动过、主人和代号已经不可信的,这里一并清成"不知道",再按现在的计数抄一份。
func markLedgerCleared(db *gorm.DB, nodeId uint, all bool) error {
	const trusted = "stamp_up = up AND stamp_down = down"
	where := "node_id = ? AND (NOT cleared OR NOT (" + trusted + "))"
	if !all {
		where += " AND user_name NOT IN (SELECT name FROM users)"
	}
	return db.Exec(`UPDATE traffic_cursors SET cleared = 1,
		user_id = CASE WHEN `+trusted+` THEN user_id ELSE 0 END,
		user_created_at = CASE WHEN `+trusted+` THEN user_created_at ELSE 0 END,
		ledger_id = CASE WHEN `+trusted+` THEN ledger_id ELSE 0 END,
		prev_ledger_id = CASE WHEN `+trusted+` THEN prev_ledger_id ELSE 0 END,
		stamp_up = up, stamp_down = down
		WHERE `+where, nodeId).Error
}
