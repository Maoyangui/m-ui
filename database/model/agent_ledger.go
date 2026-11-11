package model

import (
	"math/rand/v2"

	"gorm.io/gorm"
	"gorm.io/gorm/clause"
)

// agentLedgerNameChunk 按用户名 IN (...) 查本机用户时每批的个数(SQLite 一条语句最多 32766 个变量)。
const agentLedgerNameChunk = 500

// OwnerAbsent 账本主人的建号时间记成它(id 为 0):记账那一刻本机已经没有这个用户(刚删,数据面还没重载完),
// 和升级前的老账本(id 与建号时间都是 0,不知道是谁的)区分开。主机游标里同样的记法表示"并账那一刻主机上没有这个用户"。
const OwnerAbsent int64 = -1

// sameAgentLedger 这一行增量和已有的账本是不是同一本:
//   - 已有的是升级前的老账本(不知道主人):接着记、不认主人 —— 它可能是同一轮同步里删掉的前一个同名用户的,
//     认了主机就会当成新账本从 0 计,把前人的用量记到新用户头上;不认主人,主机照老办法按增量并;
//   - 这一行的用户本机已经没有了(OwnerAbsent):是已有那本账主人的尾巴,接着记;
//   - 否则主人(id 与建号时间)一样才是同一本,不一样就是删了又建的新用户,换一本从 0 记。
const sameAgentLedger = "((agent_counters.user_id = 0 AND agent_counters.user_created_at = 0) OR " +
	"(excluded.user_id = 0 AND excluded.user_created_at = -1) OR " +
	"(agent_counters.user_id = excluded.user_id AND agent_counters.user_created_at = excluded.user_created_at))"

// AddAgentCounters 副机记账:把这一轮各用户的增量加到本机单调账本上(批量 upsert)。
// 每本账记下主人(本机用户表里这个名字现在的 id 与建号时间,和主机同步下来的一致):同名用户删了又建是另一本账,
// 主人换了就换一本从 0 记,主机按主人认出新账本、从 0 计 —— 不靠"计数比游标小"去猜。
// 新开的一本(插入新行、换了主人换一本)还记一个随机代号 LedgerId,同一本累加时不变:账本被整本清掉后同一个主人
// 从 0 重开(改名后再改回)主人对得上,主机靠代号认出是另一本、从 0 计,不当成账本回退。
// rows 里的 UserId / UserCreatedAt / LedgerId 由这里填。
func AddAgentCounters(tx *gorm.DB, rows []AgentCounter, batch int) error {
	if len(rows) == 0 {
		return nil
	}
	names := make([]string, len(rows))
	for i, r := range rows {
		names[i] = r.UserName
	}
	owners := make(map[string]User, len(rows))
	for rest := names; len(rest) > 0; {
		k := min(len(rest), agentLedgerNameChunk)
		var us []User
		if err := tx.Select("id, name, created_at").Where("name IN ?", rest[:k]).Find(&us).Error; err != nil {
			return err
		}
		for _, u := range us {
			owners[u.Name] = u
		}
		rest = rest[k:]
	}
	ledger := newLedgerId()
	for i := range rows {
		rows[i].LedgerId = ledger // 只有新开的一本用得上,接着记的保留原来的代号
		if u, ok := owners[rows[i].UserName]; ok {
			rows[i].UserId, rows[i].UserCreatedAt = u.Id, u.CreatedAt
		} else {
			rows[i].UserId, rows[i].UserCreatedAt = 0, OwnerAbsent // 本机已经没有这个用户(刚删):下次应用快照时清掉
		}
	}
	return tx.Clauses(clause.OnConflict{
		Columns: []clause.Column{{Name: "user_name"}},
		DoUpdates: clause.Assignments(map[string]interface{}{
			"up":              gorm.Expr("CASE WHEN " + sameAgentLedger + " THEN agent_counters.up + excluded.up ELSE excluded.up END"),
			"down":            gorm.Expr("CASE WHEN " + sameAgentLedger + " THEN agent_counters.down + excluded.down ELSE excluded.down END"),
			"user_id":         gorm.Expr("CASE WHEN " + sameAgentLedger + " THEN agent_counters.user_id ELSE excluded.user_id END"),
			"user_created_at": gorm.Expr("CASE WHEN " + sameAgentLedger + " THEN agent_counters.user_created_at ELSE excluded.user_created_at END"),
			"ledger_id":       gorm.Expr("CASE WHEN " + sameAgentLedger + " THEN agent_counters.ledger_id ELSE excluded.ledger_id END"),
		}),
	}).CreateInBatches(&rows, batch).Error
}

// newLedgerId 新账本的代号:随机、非 0(0 表示升级前没有代号的账本)。不用时间:时钟回拨、粗粒度时钟下
// 先后两本可能撞上同一个值。
func newLedgerId() int64 {
	for {
		if v := rand.Int64(); v != 0 {
			return v
		}
	}
}

// PruneAgentCounters 副机应用完主机快照后清账本:主机已经不认识的用户,它的计数器没人再回收,清掉;
// 记了主人的账本,主人已经不在(同名用户在同一轮同步里删了又建,本机没见过"没有他"的那份快照)也清掉,
// 新用户从 0 另起一本。升级前的老账本(不知道主人)只按名字清。
// 主机那边的游标留着(记着是谁的那本):还在路上的旧账本报告按它只计服务器维度,不算到新用户头上。
//
// 本机一个用户都没有时不清:那是停用副机推来的空用户表(或者主机上真的一个用户都没了)。停用期间主机不再拉取,
// 清了的话上次拉取之后还没报上去的那段永远不计费;重新启用后同一个人从 0 重开一本,主机还会误报"账本回退"。
// 留着,重新启用推回完整快照时再按那份清:还在的用户接着同一本记,那段照常报上去。
func PruneAgentCounters(db *gorm.DB) error {
	var anyUser int64
	if err := db.Raw("SELECT EXISTS (SELECT 1 FROM users)").Scan(&anyUser).Error; err != nil {
		return err
	}
	if anyUser == 0 {
		return nil
	}
	return db.Exec(`DELETE FROM agent_counters WHERE user_name NOT IN (SELECT name FROM users)
		OR (NOT (user_id = 0 AND user_created_at = 0) AND NOT EXISTS (SELECT 1 FROM users u
			WHERE u.name = agent_counters.user_name AND u.id = agent_counters.user_id AND u.created_at = agent_counters.user_created_at))`).Error
}
