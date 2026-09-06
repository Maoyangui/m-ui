// Package jobs 是数据面的"收账与执法"定时任务:
//
//   - stats(10s):把内存计数器的增量写进用户表(累计流量、最近在线)与流量时序表,
//     并维护"当前在线"的用户/线路/上游列表
//   - deplete(1m):周期重置到期的用户清零并解禁;超量或过期的用户禁用并即时踢线
//   - cleanup(24h):删除超过保留天数的流量时序
//
// 副机(nodeMode)只收账不执法:增量写入本机单调账本 AgentCounter,由主机回收后统一判定。
package jobs

import (
	"encoding/json"
	"fmt"
	"runtime/debug"
	"strconv"
	"strings"
	"sync"
	"time"

	"github.com/Maoyangui/m-ui/core"
	"github.com/Maoyangui/m-ui/database/model"
	"github.com/Maoyangui/m-ui/logger"

	"gorm.io/gorm"
	"gorm.io/gorm/clause"
)

// Deps 是任务需要的外部能力,以函数注入避免与 runner 互相依赖。
type Deps struct {
	DB          *gorm.DB
	Box         func() *core.Box // 数据面实例;未运行时返回 nil
	ReloadUsers func() error     // 用户表变更后热更新入站并踢线
	IsNode      func() bool      // 是否副机
	Setting     func(string) string
	Notify      func(text string) // 用户被禁用时的通知(可为 nil)
	LocalRatio  func() float64    // 本机流量倍率(可为 nil = 1);只在主机计费路径生效,副机账本保持原始值
	Forget      func(key string)  // 清掉通知去重键(用量清零后要允许再次告警;可为 nil)
}

// Onlines 是最近一个统计周期内有流量经过的对象。
type Onlines struct {
	Users     []string `json:"users"`
	Lines     []string `json:"lines"`
	Upstreams []string `json:"upstreams"`
}

type Scheduler struct {
	d       Deps
	mu      sync.Mutex
	onlines Onlines
	stop    chan struct{}
	wg      sync.WaitGroup
	// deadResellers 上一轮判定为"不可用"(停用 / 到期 / 额度用尽)的代理集合。到期是时间自然到的,没人保存过任何东西,
	// 数据面不会自己重载,所以每轮比对这个集合,变了就重载一次;deadInit 为假表示还没有上一轮
	deadResellers map[uint]bool
	deadInit      bool
}

// ResellerUsed 代理已用流量:名下用户的全时用量之和 + 结转 - 主面板重置基线。
// 用全时用量是关键——代理自己重置 / 续费 / 周期清零都只是把 up/down 挪进 total_*,额度不会因此回血。
func ResellerUsed(db *gorm.DB, rs model.Reseller) int64 {
	var live int64
	db.Model(&model.User{}).Where("reseller_id = ?", rs.Id).
		Select("COALESCE(SUM(up + down + total_up + total_down),0)").Scan(&live)
	used := live + rs.UsedCarried - rs.UsedBase
	if used < 0 {
		return 0
	}
	return used
}

// ResellerDepleted 代理的流量额度是否已用尽(0 = 不限)。
func ResellerDepleted(db *gorm.DB, rs model.Reseller) bool {
	return rs.Volume > 0 && ResellerUsed(db, rs) >= rs.Volume
}

// RefreshReseller 立刻重算一个代理的额度用尽标志(改额度 / 重置流量后调用,不用等下一分钟);返回是否变化。
func RefreshReseller(db *gorm.DB, id uint) bool {
	var rs model.Reseller
	if err := db.First(&rs, id).Error; err != nil {
		return false
	}
	depleted := ResellerDepleted(db, rs)
	if depleted == rs.Depleted {
		return false
	}
	db.Model(&model.Reseller{}).Where("id = ?", id).Update("depleted", depleted)
	return true
}

func New(d Deps) *Scheduler {
	return &Scheduler{d: d, stop: make(chan struct{})}
}

func (s *Scheduler) Start() {
	s.loop("统计", 10*time.Second, s.runStats)
	s.loop("配额判定", time.Minute, s.runDeplete)
	s.loop("日志清理", time.Hour, s.runCleanup) // 每小时一轮:选了"保留 1 天"时不用等到明天才生效
	logger.Info("定时任务已启动:统计 10s / 配额判定 1m / 日志清理 1h")
}

func (s *Scheduler) Stop() {
	close(s.stop)
	s.wg.Wait()
}

// runSafely 兜住定时任务里的 panic:一个周期任务崩了不该把面板和数据面一起带走。
func runSafely(name string, fn func()) {
	defer func() {
		if v := recover(); v != nil {
			logger.Warning("定时任务 ", name, " 异常: ", v, " | ", string(debug.Stack()))
		}
	}()
	fn()
}

func (s *Scheduler) loop(name string, every time.Duration, fn func()) {
	s.wg.Add(1)
	go func() {
		defer s.wg.Done()
		t := time.NewTicker(every)
		defer t.Stop()
		for {
			select {
			case <-t.C:
				runSafely(name, fn)
			case <-s.stop:
				return
			}
		}
	}()
}

// Onlines 返回最近周期的在线对象。
func (s *Scheduler) Onlines() Onlines {
	s.mu.Lock()
	defer s.mu.Unlock()
	o := s.onlines
	if o.Users == nil {
		o.Users = []string{}
	}
	if o.Lines == nil {
		o.Lines = []string{}
	}
	if o.Upstreams == nil {
		o.Upstreams = []string{}
	}
	return o
}

func (s *Scheduler) settingInt(key string, def int64) int64 {
	v, err := strconv.ParseInt(s.d.Setting(key), 10, 64)
	if err != nil {
		return def
	}
	return v
}

// ---- stats ----

func (s *Scheduler) runStats() {
	box := s.d.Box()
	if box == nil {
		return
	}
	stats := box.StatsTracker().GetStats()
	now := time.Now().Unix()

	type traffic struct{ up, down int64 }
	userTraffic := map[string]*traffic{}
	var online Onlines
	seenIn, seenOut := map[string]bool{}, map[string]bool{}
	for _, st := range *stats {
		switch st.Resource {
		case "inbound":
			if !seenIn[st.Tag] {
				seenIn[st.Tag] = true
				online.Lines = append(online.Lines, st.Tag)
			}
		case "outbound":
			if !seenOut[st.Tag] {
				seenOut[st.Tag] = true
				online.Upstreams = append(online.Upstreams, st.Tag)
			}
		case "user":
			t, ok := userTraffic[st.Tag]
			if !ok {
				t = &traffic{}
				userTraffic[st.Tag] = t
				online.Users = append(online.Users, st.Tag)
			}
			if st.Direction {
				t.up += st.Traffic
			} else {
				t.down += st.Traffic
			}
		}
	}
	s.mu.Lock()
	s.onlines = online
	s.mu.Unlock()

	if len(*stats) == 0 {
		return
	}

	ratio := 1.0
	if s.d.LocalRatio != nil {
		if r := s.d.LocalRatio(); r > 0 {
			ratio = r
		}
	}
	scale := func(v int64) int64 {
		if ratio == 1 {
			return v
		}
		return int64(float64(v) * ratio)
	}

	// 事务里只用 tx:一旦在事务体内再走 s.d.DB / 设置查询,就是向连接池要第二条连接,
	// 池子被自己占满时会死等。所有要读的东西都在开事务之前取好。
	isNode := s.d.IsNode()
	trafficAge := s.settingInt("trafficAge", 30)
	bucketSeconds := s.settingInt("statsBucketSeconds", 60)
	if bucketSeconds < 1 {
		bucketSeconds = 60
	}
	err := s.d.DB.Transaction(func(tx *gorm.DB) error {
		for name, t := range userTraffic {
			if isNode {
				// 副机:写单调账本,主机按游标回收
				if err := tx.Clauses(clause.OnConflict{
					Columns: []clause.Column{{Name: "user_name"}},
					DoUpdates: clause.Assignments(map[string]interface{}{
						"up":   gorm.Expr("agent_counters.up + ?", t.up),
						"down": gorm.Expr("agent_counters.down + ?", t.down),
					}),
				}).Create(&model.AgentCounter{UserName: name, Up: t.up, Down: t.down}).Error; err != nil {
					return err
				}
				continue
			}
			update := map[string]interface{}{"online_at": now}
			if t.up > 0 {
				update["up"] = gorm.Expr("up + ?", scale(t.up))
			}
			if t.down > 0 {
				update["down"] = gorm.Expr("down + ?", scale(t.down))
			}
			if err := tx.Model(&model.User{}).Where("name = ?", name).Updates(update).Error; err != nil {
				return err
			}
		}

		// 流量时序:按桶累加,同一桶内多个周期合并为一行
		if trafficAge <= 0 {
			return nil
		}
		bucket := now - now%bucketSeconds
		rows := *stats
		for i := range rows {
			rows[i].DateTime = bucket
			if !isNode && rows[i].Resource == "user" {
				rows[i].Traffic = scale(rows[i].Traffic) // 用户维度按倍率记(与用量一致);线路/上游维度保持真实流量
			}
		}
		return tx.Clauses(clause.OnConflict{
			Columns:   []clause.Column{{Name: "resource"}, {Name: "tag"}, {Name: "date_time"}, {Name: "direction"}},
			DoUpdates: clause.Assignments(map[string]interface{}{"traffic": gorm.Expr("stats.traffic + excluded.traffic")}),
		}).Create(&rows).Error
	})
	if err != nil {
		logger.Warning("统计落库失败: ", err)
	}
}

// ---- deplete / reset ----

func (s *Scheduler) runDeplete() {
	if s.d.IsNode() {
		return // 副机不执法
	}
	now := time.Now().Unix()
	changed := false

	err := s.d.DB.Transaction(func(tx *gorm.DB) error {
		// 1) 周期重置:到期的清零并解禁;首次开启自动重置的用户初始化下次重置时间
		var resets []model.User
		if err := tx.Where("auto_reset = ? AND reset_days > 0 AND next_reset > 0 AND next_reset < ?", true, now).Find(&resets).Error; err != nil {
			return err
		}
		for _, u := range resets {
			updates := map[string]interface{}{
				"total_up":   gorm.Expr("total_up + up"),
				"total_down": gorm.Expr("total_down + down"),
				"up":         0,
				"down":       0,
				"next_reset": now + int64(u.ResetDays)*86400,
			}
			if err := tx.Model(&model.User{}).Where("id = ?", u.Id).Updates(updates).Error; err != nil {
				return err
			}
			// 只解禁因超量被停的;手动停用的和到期的不动(以前是一律 enabled=true,手动停的人到点就复活)
			if err := tx.Model(&model.User{}).Where("id = ? AND enabled = ? AND disabled_reason = ?", u.Id, false, model.DisabledQuota).
				Updates(map[string]interface{}{"enabled": true, "disabled_reason": ""}).Error; err != nil {
				return err
			}
			if s.d.Forget != nil {
				s.d.Forget("quota:" + u.Name) // 新周期从零开始,再到阈值要能再提醒
			}
			record(tx, "ResetJob", "user", "reset", u.Name)
			changed = true
		}
		if err := tx.Model(&model.User{}).
			Where("auto_reset = ? AND reset_days > 0 AND next_reset = 0", true).
			Update("next_reset", gorm.Expr("? + reset_days * 86400", now)).Error; err != nil {
			return err
		}

		// 1.5) 代理:额度用尽只在代理行上打 depleted 标记(渲染与订阅据此把他名下用户整体撤下),用户行不动;
		// 到期 / 停用同样在渲染层拦。这里只负责察觉"不可用集合"的变化并触发一次重载,以及发通知。
		var resellers []model.Reseller
		if err := tx.Find(&resellers).Error; err != nil {
			return err
		}
		dead := map[uint]bool{}
		for _, rs := range resellers {
			depleted := ResellerDepleted(tx, rs)
			if depleted != rs.Depleted {
				if err := tx.Model(&model.Reseller{}).Where("id = ?", rs.Id).Update("depleted", depleted).Error; err != nil {
					return err
				}
				var n int64
				tx.Model(&model.User{}).Where("reseller_id = ?", rs.Id).Count(&n)
				if depleted {
					record(tx, "DepleteJob", "reseller", "depleted", rs.Name)
					logger.Info("代理 ", rs.Name, " 流量用尽,名下 ", n, " 个用户已从数据面撤下")
					if s.d.Notify != nil {
						s.d.Notify(fmt.Sprintf("⛔ <b>代理流量用尽</b>:%s(名下 %d 个用户已停止服务,补量后自动恢复)", escapeHTML(rs.Name), n))
					}
				} else {
					record(tx, "DepleteJob", "reseller", "restored", rs.Name)
					logger.Info("代理 ", rs.Name, " 额度恢复,名下 ", n, " 个用户已恢复服务")
				}
				changed = true
			}
			if !rs.Enabled || depleted || (rs.Expiry > 0 && rs.Expiry < now) {
				dead[rs.Id] = true
			}
		}
		if s.deadInit {
			for id := range dead {
				if !s.deadResellers[id] {
					changed = true // 新变得不可用(典型是到期):数据面要撤下他的用户
					for _, rs := range resellers {
						if rs.Id == id && rs.Enabled && !rs.Depleted && rs.Expiry > 0 && rs.Expiry < now {
							record(tx, "DepleteJob", "reseller", "expired", rs.Name)
							if s.d.Notify != nil {
								s.d.Notify(fmt.Sprintf("⛔ <b>代理已到期</b>:%s(名下用户已停止服务)", escapeHTML(rs.Name)))
							}
						}
					}
				}
			}
			for id := range s.deadResellers {
				if !dead[id] {
					changed = true // 续期 / 重新启用:用户要回到数据面
				}
			}
		}
		s.deadResellers, s.deadInit = dead, true

		// 2) 超量 / 过期 → 禁用,并记下原因(补量 / 延期时只有这些原因会自动恢复)
		var depleted []model.User
		cond := "enabled = ? AND ((volume > 0 AND up + down >= volume) OR (expiry > 0 AND expiry < ?))"
		if err := tx.Where(cond, true, now).Find(&depleted).Error; err != nil {
			return err
		}
		if len(depleted) == 0 {
			return nil
		}
		for _, u := range depleted {
			reason, why := model.DisabledQuota, "流量用尽"
			if u.Expiry > 0 && u.Expiry < now {
				reason, why = model.DisabledExpired, "已到期"
			}
			if err := tx.Model(&model.User{}).Where("id = ?", u.Id).
				Updates(map[string]interface{}{"enabled": false, "disabled_reason": reason}).Error; err != nil {
				return err
			}
			record(tx, "DepleteJob", "user", "disable:"+reason, u.Name)
			logger.Info("用户 ", u.Name, " 已禁用(", reason, ")")
			if s.d.Notify != nil {
				s.d.Notify(fmt.Sprintf("⛔ <b>用户已禁用</b>:%s(%s)", escapeHTML(u.Name), why))
			}
		}
		changed = true
		return nil
	})
	if err != nil {
		logger.Warning("配额判定失败: ", err)
		return
	}
	if changed && s.d.ReloadUsers != nil {
		if err := s.d.ReloadUsers(); err != nil {
			logger.Warning("配额判定后热更新失败: ", err)
		}
	}
}

// ---- cleanup ----

// runCleanup 按各自的保留天数清理:流量时序、订阅访问日志、审计日志。
// 三者分开设置 —— 订阅日志以前跟着"流量记录保留"走,想把日志收到 1 天就会把流量图的历史一起删掉。
func (s *Scheduler) runCleanup() {
	now := time.Now()
	// 流量时序
	if days := s.settingInt("trafficAge", 30); days > 0 {
		cutoff := now.AddDate(0, 0, -int(days)).Unix()
		if err := s.d.DB.Where("date_time < ?", cutoff).Delete(&model.Stats{}).Error; err != nil {
			logger.Warning("清理流量时序失败: ", err)
		}
	}
	// 48 小时之前的分钟级样本并成小时桶:图表最细的 5 分钟桶只看最近 1 小时,再往前按小时够用。
	// 不并的话几百个活跃用户一个月就是上千万行,库文件和每次拉图都跟着胖
	if err := RollupStats(s.d.DB, now.Add(-48*time.Hour).Unix()); err != nil {
		logger.Warning("合并流量时序失败: ", err)
	}
	// 订阅访问日志:没设过就沿用"流量记录保留"的天数(与老版本行为一致),0 = 不自动清理
	subDays := s.settingInt("trafficAge", 30)
	if v := s.d.Setting("subLogAge"); v != "" {
		subDays = s.settingInt("subLogAge", subDays)
	}
	if subDays > 0 {
		cutoff := now.AddDate(0, 0, -int(subDays)).Unix()
		if err := s.d.DB.Where("ts < ?", cutoff).Delete(&model.SubLog{}).Error; err != nil {
			logger.Warning("清理订阅日志失败: ", err)
		}
	}
	// 订阅端口对公网开放,每个请求(含 404)记一行:再压一道总量上限,免得被刷爆磁盘
	trim(s.d.DB, &model.SubLog{}, 200000, "订阅日志")
	// 审计日志:默认 0 = 不自动清理(保持老版本行为),设了天数才按天清
	if days := s.settingInt("auditAge", 0); days > 0 {
		cutoff := now.AddDate(0, 0, -int(days)).Unix()
		if err := s.d.DB.Where("date_time < ?", cutoff).Delete(&model.Change{}).Error; err != nil {
			logger.Warning("清理审计日志失败: ", err)
		}
	}
	trim(s.d.DB, &model.Change{}, 200000, "审计日志")
}

// RollupStats 把 before 之前、不在整点上的时序行并入所在小时的桶(同键累加),再删掉原始行。
// 唯一键是 (resource, tag, date_time, direction),小时桶就是 date_time 落在整点的那一行。
func RollupStats(db *gorm.DB, before int64) error {
	return db.Transaction(func(tx *gorm.DB) error {
		if err := tx.Exec(`INSERT INTO stats (date_time, resource, tag, direction, traffic)
			SELECT date_time - date_time % 3600, resource, tag, direction, SUM(traffic) FROM stats
			WHERE date_time < ? AND date_time % 3600 <> 0 GROUP BY 1, 2, 3, 4
			ON CONFLICT(resource, tag, date_time, direction) DO UPDATE SET traffic = stats.traffic + excluded.traffic`, before).Error; err != nil {
			return err
		}
		return tx.Exec("DELETE FROM stats WHERE date_time < ? AND date_time % 3600 <> 0", before).Error
	})
}

// trim 按总量上限裁掉最旧的行(时间设置之外的兜底,免得某一类日志被刷爆磁盘)。
func trim(db *gorm.DB, tbl interface{}, keep int64, what string) {
	var n int64
	db.Model(tbl).Count(&n)
	if n <= keep {
		return
	}
	var cut uint64
	db.Model(tbl).Order("id desc").Offset(int(keep)).Limit(1).Pluck("id", &cut)
	if cut > 0 {
		if err := db.Where("id <= ?", cut).Delete(tbl).Error; err != nil {
			logger.Warning("裁剪", what, "失败: ", err)
		}
	}
}

func escapeHTML(s string) string {
	return strings.NewReplacer("&", "&amp;", "<", "&lt;", ">", "&gt;").Replace(s)
}

// record 写一条审计记录(任务发起的变更)。
func record(tx *gorm.DB, actor, key, action string, obj interface{}) {
	b, _ := json.Marshal(obj)
	tx.Create(&model.Change{DateTime: time.Now().Unix(), Actor: actor, Key: key, Action: action, Obj: b})
}
