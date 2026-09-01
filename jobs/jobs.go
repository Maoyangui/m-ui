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
	"strconv"
	"sync"
	"time"

	"github.com/fangjunsheng555/m-ui/core"
	"github.com/fangjunsheng555/m-ui/database/model"
	"github.com/fangjunsheng555/m-ui/logger"

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
}

func New(d Deps) *Scheduler {
	return &Scheduler{d: d, stop: make(chan struct{})}
}

func (s *Scheduler) Start() {
	s.loop(10*time.Second, s.runStats)
	s.loop(time.Minute, s.runDeplete)
	s.loop(24*time.Hour, s.runCleanup)
	logger.Info("定时任务已启动:统计 10s / 配额判定 1m / 时序清理 24h")
}

func (s *Scheduler) Stop() {
	close(s.stop)
	s.wg.Wait()
}

func (s *Scheduler) loop(every time.Duration, fn func()) {
	s.wg.Add(1)
	go func() {
		defer s.wg.Done()
		t := time.NewTicker(every)
		defer t.Stop()
		for {
			select {
			case <-t.C:
				fn()
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

	err := s.d.DB.Transaction(func(tx *gorm.DB) error {
		isNode := s.d.IsNode()
		for name, t := range userTraffic {
			if isNode {
				// 副机:写单调账本,主机按游标回收
				if err := tx.Clauses(clause.OnConflict{
					Columns:   []clause.Column{{Name: "user_name"}},
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
				update["up"] = gorm.Expr("up + ?", t.up)
			}
			if t.down > 0 {
				update["down"] = gorm.Expr("down + ?", t.down)
			}
			if err := tx.Model(&model.User{}).Where("name = ?", name).Updates(update).Error; err != nil {
				return err
			}
		}

		// 流量时序:按桶累加,同一桶内多个周期合并为一行
		if s.settingInt("trafficAge", 30) <= 0 {
			return nil
		}
		bucketSeconds := s.settingInt("statsBucketSeconds", 60)
		if bucketSeconds < 1 {
			bucketSeconds = 60
		}
		bucket := now - now%bucketSeconds
		rows := *stats
		for i := range rows {
			rows[i].DateTime = bucket
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
				"enabled":    true,
				"next_reset": now + int64(u.ResetDays)*86400,
			}
			if err := tx.Model(&model.User{}).Where("id = ?", u.Id).Updates(updates).Error; err != nil {
				return err
			}
			record(tx, "ResetJob", "user", "reset", u.Name)
			changed = true
		}
		if err := tx.Model(&model.User{}).
			Where("auto_reset = ? AND reset_days > 0 AND next_reset = 0", true).
			Update("next_reset", gorm.Expr("? + reset_days * 86400", now)).Error; err != nil {
			return err
		}

		// 2) 超量 / 过期 → 禁用
		var depleted []model.User
		cond := "enabled = ? AND ((volume > 0 AND up + down > volume) OR (expiry > 0 AND expiry < ?))"
		if err := tx.Where(cond, true, now).Find(&depleted).Error; err != nil {
			return err
		}
		if len(depleted) == 0 {
			return nil
		}
		if err := tx.Model(&model.User{}).Where(cond, true, now).Update("enabled", false).Error; err != nil {
			return err
		}
		for _, u := range depleted {
			reason := "quota"
			if u.Expiry > 0 && u.Expiry < now {
				reason = "expired"
			}
			record(tx, "DepleteJob", "user", "disable:"+reason, u.Name)
			logger.Info("用户 ", u.Name, " 已禁用(", reason, ")")
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

func (s *Scheduler) runCleanup() {
	days := s.settingInt("trafficAge", 30)
	if days <= 0 {
		return
	}
	cutoff := time.Now().AddDate(0, 0, -int(days)).Unix()
	if err := s.d.DB.Where("date_time < ?", cutoff).Delete(&model.Stats{}).Error; err != nil {
		logger.Warning("清理流量时序失败: ", err)
		return
	}
	if err := s.d.DB.Where("ts < ?", cutoff).Delete(&model.SubLog{}).Error; err != nil {
		logger.Warning("清理订阅日志失败: ", err)
	}
}

// record 写一条审计记录(任务发起的变更)。
func record(tx *gorm.DB, actor, key, action string, obj interface{}) {
	b, _ := json.Marshal(obj)
	tx.Create(&model.Change{DateTime: time.Now().Unix(), Actor: actor, Key: key, Action: action, Obj: b})
}
