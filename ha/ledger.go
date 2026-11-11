package ha

import (
	"strings"
	"time"

	"github.com/Maoyangui/m-ui/database"
	"github.com/Maoyangui/m-ui/database/model"
	"github.com/Maoyangui/m-ui/hub"
	"github.com/Maoyangui/m-ui/logger"

	"gorm.io/gorm"
)

// 换主机前后本机的流量账(设为主机、接任、原主机变为副机):每一段都要进新主机的库,而且只进一次。
//
//   - 原主机变为副机(失联后被接任、让位):本机当主机时流量直接记进用户用量,库里没有自己的游标。换一本新纪元的账,
//     同一笔事务里把账本清空(当主机时不该有,有也是早年当副机时的残行)、声明这本账从 0 开始(hub.LedgerFreshKey);
//     新主机组库时给原主机记上 hub.LedgerAwaitFresh,第一份报告就从 0 计,不只建基线。
//   - 设为主机:导出整库之后原主机照常当主机(冻结只挡管理类写操作),本机流量照常记进它自己的用户用量,却进不了新主机的库。
//     冻结开始时另起一本账(model.SelfLedgerKey),本机流量同时记两边;整库数据里带着这本账的纪元和同一时刻的计数,
//     目标组库时记成原主机的游标;原主机变为副机后接着这本账记、上报,新主机只按增量回收。交接没成就清掉这本账。
//   - 备用 / 目标组好新库之后到重启换库之前,自己还在记账(写进旧库的账本):换库时按新库里本机的游标再并一次。

// startSelfLedger 设为主机冻结开始:清空账本、另起一本账(本机流量从此同时记进用户用量和这本账)。
// 同一笔事务里清空和置开关:jobs 在记账事务里读开关,两边先后分明。
func (s *Service) startSelfLedger() error {
	return s.d.DB.Transaction(func(tx *gorm.DB) error {
		if err := tx.Exec("DELETE FROM agent_counters").Error; err != nil {
			return err
		}
		return upsert(tx, map[string]string{KeySelfLedger: hub.NewLedgerEpoch()})
	})
}

// stopSelfLedger 设为主机没做成(本机照旧当主机):用户用量一直照常在记,另记的那本账不用了,连同开关一起清掉。
func (s *Service) stopSelfLedger() {
	err := s.d.DB.Transaction(func(tx *gorm.DB) error {
		if err := upsert(tx, map[string]string{KeySelfLedger: ""}); err != nil {
			return err
		}
		return tx.Exec("DELETE FROM agent_counters").Error
	})
	if err != nil {
		logger.Warning("容灾:清掉设为主机途中另记的账本失败(下次设为主机或变为副机时会再清): ", err)
	}
}

// setNode 本机变为副机:values 连同本机流量账的交接在同一笔事务里写(jobs 在记账事务里看 nodeMode,从这一刻起记进账本)。
// selfLedger 为真时接着设为主机途中另记的那本账(纪元已经随整库数据交给新主机,连同游标);否则清空账本、
// 换一本新纪元的账并声明从 0 开始。
func (s *Service) setNode(values map[string]string, selfLedger bool) error {
	return s.d.DB.Transaction(func(tx *gorm.DB) error {
		var cur string
		if err := tx.Raw("SELECT value FROM settings WHERE key = ?", KeySelfLedger).Scan(&cur).Error; err != nil {
			return err
		}
		if selfLedger && cur != "" {
			values[hub.LedgerEpochKey], values[hub.LedgerFreshKey] = cur, ""
		} else {
			if err := tx.Exec("DELETE FROM agent_counters").Error; err != nil {
				return err
			}
			e := hub.NewLedgerEpoch()
			values[hub.LedgerEpochKey], values[hub.LedgerFreshKey] = e, e
		}
		values[KeySelfLedger] = ""
		return upsert(tx, values)
	})
}

// installFormerLedger 组新库时(在新库的事务里)给原主机 formerId 准备好账:它变为副机后报上来的第一份怎么并。
//   - 设为主机、整库数据里带着原主机交接途中另记的那本账(ledger 为它的纪元,账本行在新库的 agent_counters 里,
//     和用户用量是同一时刻的):记成原主机的游标与纪元,它变为副机后接着这本账报,只计增量;
//   - 否则(接任,或原主机是老版本):记 hub.LedgerAwaitFresh,等它清空重开的那本,第一份从 0 计。
//
// 库里早年它当副机时留下的游标一律作废。
func installFormerLedger(tx *gorm.DB, formerId uint, ledger string) error {
	if err := tx.Where("node_id = ?", formerId).Delete(&model.TrafficCursor{}).Error; err != nil {
		return err
	}
	if ledger == "" {
		return upsert(tx, map[string]string{hub.LedgerEpochSetting(formerId): hub.LedgerAwaitFresh})
	}
	if err := tx.Exec(`INSERT INTO traffic_cursors (node_id, user_name, up, down, user_id, user_created_at, ledger_id,
		prev_ledger_id, prev_ledger_id2, prev_ledger_id3, prev_ledger_id4, stamp_up, stamp_down, cleared, cleared_at)
		SELECT ?, user_name, up, down, user_id, user_created_at, ledger_id, 0, 0, 0, 0, up, down, 0, 0 FROM agent_counters`, formerId).Error; err != nil {
		return err
	}
	return upsert(tx, map[string]string{hub.LedgerEpochSetting(formerId): ledger})
}

// mergeOwnLedger 把本机(副机)账本 from 里还没进 to 的流量并进 to:按 to 里本机的游标只计增量,不重计。
func mergeOwnLedger(to *gorm.DB, selfId uint, self model.Node, from *gorm.DB, now time.Time) (hub.MergeResult, error) {
	l, err := hub.LocalLedger(from)
	if err != nil || len(l.Counters) == 0 {
		return hub.MergeResult{}, err
	}
	b := int64(atoiDef(settingIn(to, "statsBucketSeconds"), 60))
	if b <= 0 {
		b = 60
	}
	return hub.MergeReport(to, selfId, self.Name, l, now.Unix(), b, self.Ratio)
}

// mergeOldLedger 换上新库之后:旧库(本机当副机时的库,刚改名为 old)的账本里,组新库之后到重启之前本机又记了一些
// (接任:组库后到重启;设为主机:暂存后到提交、重启),按新库里本机的游标并进去。失败只记日志:少的只是这一小段。
func mergeOldLedger(dbPath, old string, now time.Time) {
	odb, err := database.OpenNoMigrate(old)
	if err != nil {
		logger.Warning("容灾:打开换下来的旧库失败,组库之后本机记的那点流量没并进新库: ", err)
		return
	}
	defer closeDB(odb)
	var n int64
	if err := odb.Model(&model.AgentCounter{}).Count(&n).Error; err != nil || n == 0 {
		return
	}
	var oldSelf model.Node
	if err := odb.Where("is_local = ?", true).Limit(1).Find(&oldSelf).Error; err != nil || oldSelf.Id == 0 {
		return
	}
	ndb, err := database.Open(dbPath)
	if err != nil {
		logger.Warning("容灾:打开新库失败,组库之后本机记的那点流量没并进去: ", err)
		return
	}
	defer database.Close(ndb)
	var self model.Node
	if err := ndb.Where("is_local = ?", true).Limit(1).Find(&self).Error; err != nil || self.Id != oldSelf.Id {
		return // 新库里本机不是同一台(不该发生):不并
	}
	res, err := mergeOwnLedger(ndb, self.Id, self, odb, now)
	if err != nil {
		logger.Warning("容灾:并入组库之后本机记的流量失败: ", err)
		return
	}
	if res.Merged > 0 {
		logger.Info("容灾:组库之后到重启之前本机记的流量已并进新库(", res.Merged, " 个用户)")
	}
}

// settingIn 某个库里的一项设置(新库、旧库,不是本机正在用的库)。
func settingIn(db *gorm.DB, key string) string {
	var v string
	db.Raw("SELECT value FROM settings WHERE key = ?", key).Scan(&v)
	return v
}

// selfLedgerLeftover 本机是主机、没有在设为主机,却还开着另记账本的开关(交接收尾时清它出了错,或者进程退出了):返回开关里
// 那本账的纪元,没有为空。读到之后要清的话用 dropSelfLedger,只清这一本。
func (s *Service) selfLedgerLeftover() string {
	if s.IsNode() {
		return ""
	}
	v := s.setting(KeySelfLedger)
	if strings.TrimSpace(v) == "" {
		return ""
	}
	return v
}

// dropSelfLedger 清掉交接收尾时没清成、留下来的那本另记的账(纪元 epoch,selfLedgerLeftover 读到的):开关还是它、本机也还不是副机,
// 才在同一笔事务里关开关、清账本。看过之后管理员又发起了一次设为主机、另起了新的一本,开关就不是它了,不动 ——
// 以前看过就不管三七二十一清掉,新开的这本连同开关一起没了:导出的整库数据里没有它,原主机从导出到变为副机之间的流量就丢了。
// 已经变为副机的,账本是副机自己的账,也不动。
func (s *Service) dropSelfLedger(epoch string) {
	err := s.d.DB.Transaction(func(tx *gorm.DB) error {
		var node string
		if err := tx.Raw("SELECT value FROM settings WHERE key = ?", "nodeMode").Scan(&node).Error; err != nil {
			return err
		}
		if strings.EqualFold(strings.TrimSpace(node), "true") {
			return nil
		}
		r := tx.Exec("UPDATE settings SET value = '' WHERE key = ? AND value = ?", KeySelfLedger, epoch)
		if r.Error != nil || r.RowsAffected != 1 {
			return r.Error
		}
		return tx.Exec("DELETE FROM agent_counters").Error
	})
	if err != nil {
		logger.Warning("容灾:清掉设为主机途中另记的账本失败(下一轮再清): ", err)
	}
}
