package model

import (
	"time"

	"gorm.io/gorm"
	"gorm.io/gorm/clause"
)

// ---- 服务器的流量额度与到期 ----
//
// 计量口径:这台服务器的数据面收发的全部字节 —— 各条线路的入站,加上经它转发的中转入口 —— 真实上下行,不乘倍率。
// 服务器之间同步配置、SSH、系统更新这些不经过数据面,不在这个数里(所以「只停代理服务、不停用服务器」时
// 那台还会有少量流量,界面上要说清楚)。
//
// 每台机器(主机、副机都一样)在本机记一个只增不减的总数(LocalTotal);副机随报告带给主机(hub.Report.Total),
// 主机按每台的游标只计增量(MergeNodeTotal),记进 NodeQuota 的本期用量。主机自己那台走的是同一条路:
// 接任、设为主机前后都只是「上次并到哪、现在是多少」,不用另外交接。

// LocalTotal 本机数据面累计收发的字节(只有 Id = 1 这一行)。是这台机器自己的东西:不进快照,
// 容灾的副本、设为主机的整库数据都不带(见 ha 的 replicaSkip / bundleSkip)。
type LocalTotal struct {
	Id        uint  `json:"-" gorm:"primaryKey;autoIncrement:false"`
	Up        int64 `json:"up"`
	Down      int64 `json:"down"`
	LedgerId  int64 `json:"ledgerId"` // 这本账的代号(随机、非 0):库被换掉、重装之后是另一本,主机据此只建基线、不把整本当增量
	UpdatedAt int64 `json:"-"`
}

// NodeTotal 一台机器报给主机的数据面累计(LocalTotal 的那三个数)。LedgerId 为 0 = 没有(老版本不报)。
type NodeTotal struct {
	Up       int64 `json:"up"`
	Down     int64 `json:"down"`
	LedgerId int64 `json:"ledgerId"`
}

// AddLocalTotal 把这一轮数据面的字节加到本机的累计上,返回加完之后的数。up / down 都不大于 0 时只读不写(还没有记录返回零值)。
func AddLocalTotal(tx *gorm.DB, up, down, now int64) (NodeTotal, error) {
	if up < 0 {
		up = 0
	}
	if down < 0 {
		down = 0
	}
	if up > 0 || down > 0 {
		if err := tx.Clauses(clause.OnConflict{
			Columns: []clause.Column{{Name: "id"}},
			DoUpdates: clause.Assignments(map[string]interface{}{
				"up":         gorm.Expr("local_totals.up + excluded.up"),
				"down":       gorm.Expr("local_totals.down + excluded.down"),
				"updated_at": now,
			}),
		}).Create(&LocalTotal{Id: 1, Up: up, Down: down, LedgerId: newLedgerId(), UpdatedAt: now}).Error; err != nil {
			return NodeTotal{}, err
		}
	}
	return ReadLocalTotal(tx)
}

// EnsureLocalTotal 本机的累计还没有记录时建一条全零的(带上这本账的代号);已经有了就不动。打开数据库时调。
//
// 为什么不等有了流量再建:主机头一次见到一本账只拿它当基线、不计(见 MergeNodeTotal)。等第一笔流量才建的话,
// 主机头一次见到它时那一整笔已经在里面 —— 实机上升级后跑的第一段 25 MB 一个字节都没记上。启动时就建好,
// 主机头一次见到的是 0(或者只有这台比主机早升级的那一小段),之后的流量都是增量。
func EnsureLocalTotal(db *gorm.DB, now int64) error {
	return db.Clauses(clause.OnConflict{DoNothing: true}).Create(&LocalTotal{Id: 1, LedgerId: newLedgerId(), UpdatedAt: now}).Error
}

// ReadLocalTotal 本机数据面的累计(还没有记录返回零值,LedgerId 为 0)。
func ReadLocalTotal(db *gorm.DB) (NodeTotal, error) {
	var row LocalTotal
	if err := db.Where("id = ?", 1).Limit(1).Find(&row).Error; err != nil {
		return NodeTotal{}, err
	}
	return NodeTotal{Up: row.Up, Down: row.Down, LedgerId: row.LedgerId}, nil
}

// 服务器额度的状态(NodeQuota.State)。
const (
	QuotaOK      = ""        // 正常
	QuotaTraffic = "traffic" // 本期流量用完
	QuotaExpired = "expired" // 已到期
)

// 重置周期(NodeQuota.ResetMode)。
const (
	QuotaResetNone  = ""      // 不重置
	QuotaResetDays  = "days"  // 每 ResetDays 天
	QuotaResetMonth = "month" // 每月 ResetDay 日(那个月没有这一天就用月底)
)

// NodeQuota 一台服务器的流量额度、到期时间和本期用量。只在主机上判、在主机上改;容灾的副本带着这张表。
//
// 用完 / 到期之后有两种处理(KeepOn):
//   - 默认(KeepOn 假):把这台服务器停用 —— 和手动停用一样,主机给它推一份空用户表、之后不再同步,那台不再有流量。
//     记下 AutoOff;重置、加额度、延期之后自动重新启用。手动停用的(AutoOff 假)不会被自动启用。
//   - KeepOn 真,或者是主机自己那台(停用不了):服务器照常同步,只是不再放行用户 —— nodes.suspended 置真,
//     那台渲染数据面时不下发任何用户、不开中转入口,订阅里也不再发它的节点。
type NodeQuota struct {
	NodeId uint `json:"nodeId" gorm:"primaryKey;autoIncrement:false"`

	Volume    int64  `json:"volume"`    // 本期额度(字节);0 = 不限
	Expiry    int64  `json:"expiry"`    // 到期时间(unix 秒);0 = 不设
	ResetMode string `json:"resetMode"` // 见 QuotaReset*
	ResetDays int    `json:"resetDays"`
	ResetDay  int    `json:"resetDay"`
	NextReset int64  `json:"nextReset"` // 下次重置的时间;0 = 不重置 / 还没算
	KeepOn    bool   `json:"keepOn"`    // 用完 / 到期后不停用服务器,只停它的代理服务

	Up          int64 `json:"up"` // 本期用量(真实字节)
	Down        int64 `json:"down"`
	TotalUp     int64 `json:"totalUp"` // 以前各期的合计(不含本期)
	TotalDown   int64 `json:"totalDown"`
	PeriodStart int64 `json:"periodStart"` // 本期从什么时候算起

	State   string `json:"state"`   // 见 Quota*
	StateAt int64  `json:"stateAt"` // 进入这个状态的时间
	AutoOff bool   `json:"autoOff"` // 这台现在的「停用」是额度自动停的

	// 计量游标:上次并到的那台的累计(见 MergeNodeTotal)。Tracked = 那台报过累计(版本够新);没报过的用量一直是 0,额度不生效。
	CurUp     int64 `json:"-"`
	CurDown   int64 `json:"-"`
	CurLedger int64 `json:"-"`
	CurAt     int64 `json:"curAt"`
	Tracked   bool  `json:"tracked"`
}

// Used 本期已用(上行 + 下行)。
func (q NodeQuota) Used() int64 { return q.Up + q.Down }

// Configured 设了额度或到期时间(没设的只是在记用量)。
func (q NodeQuota) Configured() bool { return q.Volume > 0 || q.Expiry > 0 }

// StateNow 按现在的用量和时间该是什么状态:到期优先(到期了,重置也不会恢复)。
// 额度只对报得上累计的服务器生效(Tracked):没报过的用量是 0,不会、也不该判成用完。
func (q NodeQuota) StateNow(now int64) string {
	if q.Expiry > 0 && now >= q.Expiry {
		return QuotaExpired
	}
	if q.Volume > 0 && q.Used() >= q.Volume {
		return QuotaTraffic
	}
	return QuotaOK
}

// MergeNodeTotal 把一台机器报上来的数据面累计并进它的本期用量:只计增量。返回这一次计进去的字节。
//
//   - 第一次见到这台的累计、或者换了一本(LedgerId 不同:重装、库被换掉、接任后新开的):只建基线,这一份不计 ——
//     宁可少算从开机到第一次并账的那一小段,也不能把一台跑了几个月的机器的整本累计当成本期用量、当场把它判成用完;
//   - 同一本账却变小了(那台的库回到了以前):对齐,不计;
//   - 否则按差值计。
//
// 服务器已经不在(删了,报告还在路上)什么都不做。t.LedgerId 为 0(老版本不报)也什么都不做。
func MergeNodeTotal(tx *gorm.DB, nodeId uint, t NodeTotal, now int64) (int64, error) {
	if nodeId == 0 || t.LedgerId == 0 || t.Up < 0 || t.Down < 0 {
		return 0, nil
	}
	var n int64
	if err := tx.Model(&Node{}).Where("id = ?", nodeId).Count(&n).Error; err != nil || n == 0 {
		return 0, err
	}
	var q NodeQuota
	if err := tx.Where("node_id = ?", nodeId).Limit(1).Find(&q).Error; err != nil {
		return 0, err
	}
	if q.NodeId == 0 {
		return 0, tx.Create(&NodeQuota{NodeId: nodeId, CurUp: t.Up, CurDown: t.Down, CurLedger: t.LedgerId, CurAt: now, PeriodStart: now, Tracked: true}).Error
	}
	var dUp, dDown int64
	if q.CurLedger == t.LedgerId && t.Up >= q.CurUp && t.Down >= q.CurDown {
		dUp, dDown = t.Up-q.CurUp, t.Down-q.CurDown
	}
	if dUp == 0 && dDown == 0 && q.CurLedger == t.LedgerId && q.CurUp == t.Up && q.CurDown == t.Down && q.Tracked {
		return 0, nil // 没变:不写库(每 5 秒一份报告)
	}
	upd := map[string]interface{}{"cur_up": t.Up, "cur_down": t.Down, "cur_ledger": t.LedgerId, "cur_at": now, "tracked": true}
	if dUp > 0 {
		upd["up"] = gorm.Expr("up + ?", dUp)
	}
	if dDown > 0 {
		upd["down"] = gorm.Expr("down + ?", dDown)
	}
	if q.PeriodStart == 0 {
		upd["period_start"] = now
	}
	return dUp + dDown, tx.Model(&NodeQuota{}).Where("node_id = ?", nodeId).Updates(upd).Error
}

// NextQuotaReset 从 now 起下一次重置的时间:按天数 = now + ResetDays 天;按月 = 下一个「每月 ResetDay 日 0 点」
// (loc 时区;那个月没有这一天就用那个月的最后一天)。不重置、参数不对返回 0。
func NextQuotaReset(mode string, days, day int, now int64, loc *time.Location) int64 {
	switch mode {
	case QuotaResetDays:
		if days <= 0 {
			return 0
		}
		return AddDays(now, days)
	case QuotaResetMonth:
		if day < 1 || day > 31 {
			return 0
		}
		if loc == nil {
			loc = time.Local
		}
		t := time.Unix(now, 0).In(loc)
		at := func(y int, m time.Month) time.Time {
			last := time.Date(y, m+1, 0, 0, 0, 0, 0, loc).Day() // 这个月的最后一天
			return time.Date(y, m, min(day, last), 0, 0, 0, 0, loc)
		}
		next := at(t.Year(), t.Month())
		if !next.After(t) {
			next = at(t.Year(), t.Month()+1) // time.Date 会把 13 月折到下一年
		}
		return next.Unix()
	}
	return 0
}

// RollQuota 到了重置时间:本期用量并进历史合计、清零,新的一期从 now 起算,算出下一次重置的时间。
// 返回要写的列;没到时间(或不重置)返回 nil。
func RollQuota(q NodeQuota, now int64, loc *time.Location) map[string]interface{} {
	if q.ResetMode == QuotaResetNone || q.NextReset <= 0 || now < q.NextReset {
		return nil
	}
	return map[string]interface{}{
		"total_up":     gorm.Expr("total_up + up"),
		"total_down":   gorm.Expr("total_down + down"),
		"up":           0,
		"down":         0,
		"period_start": now,
		"next_reset":   NextQuotaReset(q.ResetMode, q.ResetDays, q.ResetDay, now, loc),
	}
}
