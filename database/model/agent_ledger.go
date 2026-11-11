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

// SelfLedgerKey 本机设置:主机在「设为主机」交接途中,本机流量除了照常记进用户用量,还另记一本副机那样的账本
// (agent_counters),值是这本账的纪元。整库数据里带着这本账此刻的计数(和用户用量是同一时刻的)和纪元,目标据此记下
// 本机的游标;本机变为副机后接着这本账记、上报,新主机只按增量回收 —— 导出整库之后、本机变为副机之前本机记的流量
// 不会随原主机的库丢掉。交接没成(本机照旧当主机)时清掉:用户用量一直照常在记,这本账只是没用上。
const SelfLedgerKey = "haSelfLedger"

// AddSelfLedger 主机记账时(和用户用量同一笔事务)调用:交接途中(SelfLedgerKey 有值)把这一轮各用户的原始字节另记一本账。
// 开关在事务里读:和置开关的那笔事务先后分明,导出整库时的用户用量与这本账的计数是同一时刻的。
func AddSelfLedger(tx *gorm.DB, rows []AgentCounter, batch int) error {
	if len(rows) == 0 {
		return nil
	}
	var on string
	if err := tx.Raw("SELECT value FROM settings WHERE key = ?", SelfLedgerKey).Scan(&on).Error; err != nil {
		return err
	}
	if on == "" {
		return nil
	}
	return AddAgentCounters(tx, rows, batch)
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

// PruneAgentCounters 副机应用完主机快照后清账本:主人已经不在的账本没人再回收,清掉 —— 记了主人的按主人(用户 id 与建号时间)
// 认:主机上删了这个人(包括同名用户在同一轮同步里删了又建,本机没见过"没有他"的那份快照),新用户从 0 另起一本;
// "记账时本机已经没有这个用户"的尾巴也清掉。升级前的老账本(不知道主人)只按名字清。
// 主机那边的游标留着(记着是谁的那本):还在路上的旧账本报告按它只计服务器维度,不算到新用户头上。
//
// 主人还在、只是改了名的,原名那本留着:推送在前、拉报告在后,应用改名的快照时,上次拉取之后记在原名下的流量还没报
// (和主机失联期间改名,用户照常连本机,没报的可能很多)。以前按"名字不在用户表"清掉,这段永远不计费;
// 现在照常上报,主机按主人计到改名后的用户头上。原名已经给了另一个人的照旧清掉:老版本主机(先升副机的过渡期)
// 只认名字,报上去会把前一个人的尾巴算到新的同名用户头上。
// byOwner:推来快照的主机按账本主人计费(0.7.17 起,见 hub.MasterBillsByOwner)。老版本主机只认名字,原名那本留着也计不到
// 改了名的人头上,原名之后给了新用户、推送又没送到(拉报告照常成功)时还会算到新用户头上:照旧按名字清,用户表里没有这个名字的一律清掉。
//
// 本机一个用户都没有时不清:那是停用副机推来的空用户表(或者主机上真的一个用户都没了)。停用期间主机不再拉取,
// 清了的话上次拉取之后还没报上去的那段永远不计费;重新启用后同一个人从 0 重开一本,主机还会误报"账本回退"。
// 留着,重新启用推回完整快照时再按那份清:还在的用户接着同一本记,那段照常报上去。
func PruneAgentCounters(db *gorm.DB, byOwner bool) error {
	var anyUser int64
	if err := db.Raw("SELECT EXISTS (SELECT 1 FROM users)").Scan(&anyUser).Error; err != nil {
		return err
	}
	if anyUser == 0 {
		return nil
	}
	if !byOwner {
		return db.Exec(`DELETE FROM agent_counters WHERE user_name NOT IN (SELECT name FROM users)
			OR (NOT (user_id = 0 AND user_created_at = 0) AND NOT EXISTS (SELECT 1 FROM users u
				WHERE u.name = agent_counters.user_name AND u.id = agent_counters.user_id AND u.created_at = agent_counters.user_created_at))`).Error
	}
	return db.Exec(`DELETE FROM agent_counters WHERE
		(user_id = 0 AND user_created_at = 0 AND user_name NOT IN (SELECT name FROM users))
		OR (NOT (user_id = 0 AND user_created_at = 0) AND (
			NOT EXISTS (SELECT 1 FROM users u WHERE u.id = agent_counters.user_id AND u.created_at = agent_counters.user_created_at)
			OR EXISTS (SELECT 1 FROM users u WHERE u.name = agent_counters.user_name
				AND NOT (u.id = agent_counters.user_id AND u.created_at = agent_counters.user_created_at))))`).Error
}
