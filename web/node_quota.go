package web

import (
	"encoding/json"
	"errors"
	"fmt"
	"html"
	"net/http"
	"strings"
	"time"

	"github.com/Maoyangui/m-ui/database/model"
	"github.com/Maoyangui/m-ui/hub"
	"github.com/Maoyangui/m-ui/logger"

	"gorm.io/gorm"
)

// ---- 服务器的流量额度与到期(模型与计量口径见 model.NodeQuota)----
//
// 这里是主机上的那一半:接口(看 / 改 / 重置用量)和判定。判定每分钟一轮、并账之后再补一轮(jobs 经 runner 的钩子调进来):
//
//	周期重置到点 → 本期清零;
//	按现在的用量和时间得出状态(正常 / 流量用完 / 已到期);
//	用完或到期:默认把这台服务器停用(和手动停用同一条路:推空用户表、之后不再同步),记下是自动停的;
//	  关了「自动停用」的、主机自己那台(停用不了)改成只停代理服务(nodes.suspended):服务器照常同步;
//	恢复正常(重置了、加了额度、延了期、把额度去掉了):自动停的重新启用,停止服务的标记去掉。
//
// 手动停用的服务器不归这里管:不会被自动启用。

// nodeQuotaView 一台服务器的额度给界面看的样子(接口 nodes 的每一行带一份;没有记录的不带)。
type nodeQuotaView struct {
	Volume      int64  `json:"volume"`      // 本期额度(字节);0 = 不限
	Used        int64  `json:"used"`        // 本期已用(上行 + 下行)
	Up          int64  `json:"up"`          // 本期上行
	Down        int64  `json:"down"`        // 本期下行
	Total       int64  `json:"total"`       // 开始统计以来的合计(含本期)
	PeriodStart int64  `json:"periodStart"` // 本期从什么时候算起
	Expiry      int64  `json:"expiry"`      // 到期时间;0 = 不设
	ResetMode   string `json:"resetMode"`   // "" 不重置 / days / month
	ResetDays   int    `json:"resetDays"`
	ResetDay    int    `json:"resetDay"`
	NextReset   int64  `json:"nextReset"`
	AutoDisable bool   `json:"autoDisable"` // 用完 / 到期后自动停用这台(主机自己那台恒为假:停用不了)
	State       string `json:"state"`       // "" 正常 / traffic 流量用完 / expired 已到期
	StateAt     int64  `json:"stateAt"`
	AutoOff     bool   `json:"autoOff"` // 这台现在的「停用」是额度自动停的
	Tracked     bool   `json:"tracked"` // 这台报过数据面的累计(版本够新);没报过的用量一直是 0,额度不生效
}

func quotaView(q model.NodeQuota, n model.Node) *nodeQuotaView {
	return &nodeQuotaView{
		Volume: q.Volume, Used: q.Used(), Up: q.Up, Down: q.Down, Total: q.TotalUp + q.TotalDown + q.Used(), PeriodStart: q.PeriodStart,
		Expiry: q.Expiry, ResetMode: q.ResetMode, ResetDays: q.ResetDays, ResetDay: q.ResetDay, NextReset: q.NextReset,
		AutoDisable: !q.KeepOn && !n.IsLocal, State: q.State, StateAt: q.StateAt, AutoOff: q.AutoOff, Tracked: q.Tracked,
	}
}

// nodeQuotas 全部服务器的额度记录,按服务器 id。
func (s *Server) nodeQuotas() map[uint]model.NodeQuota {
	var list []model.NodeQuota
	s.db.Find(&list)
	out := make(map[uint]model.NodeQuota, len(list))
	for _, q := range list {
		out[q.NodeId] = q
	}
	return out
}

// 额度的上限:1 EB(够用,也不会在加减时溢出)。
const maxNodeQuota = int64(1) << 60

// nodeQuotaReq 改额度设置的请求体(没给的字段不动)。单独改:PUT nodes/{id}/quota;也可以随新增 / 编辑服务器一起给(nodePayload.Quota)。
type nodeQuotaReq struct {
	Volume      *int64  `json:"volume"`      // 本期额度(字节);0 = 不限
	Expiry      *int64  `json:"expiry"`      // 到期时间(unix 秒);0 = 不设
	ResetMode   *string `json:"resetMode"`   // "" 不重置 / days / month
	ResetDays   *int    `json:"resetDays"`   // 按天数:每几天
	ResetDay    *int    `json:"resetDay"`    // 按月:每月几日
	NextReset   *int64  `json:"nextReset"`   // 按天数时可以指定下一次重置的时间(对上服务商的账单日)
	AutoDisable *bool   `json:"autoDisable"` // 用完 / 到期后自动停用这台服务器
}

// quotaAfter 把一份请求套到现有的额度记录上(q;还没有记录时给零值),校验、算好下次重置的时间,返回改完的样子。不写库。
func (s *Server) quotaAfter(q model.NodeQuota, nodeId uint, p nodeQuotaReq, now int64) (model.NodeQuota, error) {
	if q.NodeId == 0 {
		q = model.NodeQuota{NodeId: nodeId, PeriodStart: now}
	}
	was := q
	if p.Volume != nil {
		q.Volume = *p.Volume
	}
	if p.Expiry != nil {
		q.Expiry = *p.Expiry
	}
	if p.ResetMode != nil {
		q.ResetMode = strings.TrimSpace(*p.ResetMode)
	}
	if p.ResetDays != nil {
		q.ResetDays = *p.ResetDays
	}
	if p.ResetDay != nil {
		q.ResetDay = *p.ResetDay
	}
	if p.AutoDisable != nil {
		q.KeepOn = !*p.AutoDisable
	}
	if err := checkNodeQuota(&q, was, p.Expiry != nil, now); err != nil {
		return q, err
	}
	// 下次重置的时间:周期变了重新算;按天数的可以自己指定从哪天开始重置(对上服务商的账单日)
	cycleChanged := q.ResetMode != was.ResetMode || q.ResetDays != was.ResetDays || q.ResetDay != was.ResetDay
	switch {
	case q.ResetMode == model.QuotaResetNone:
		q.NextReset = 0
	case q.ResetMode == model.QuotaResetDays && p.NextReset != nil && *p.NextReset > 0 && *p.NextReset != was.NextReset:
		if *p.NextReset <= now {
			return q, errors.New("下次重置的时间要在现在之后")
		}
		if *p.NextReset > model.AddDays(now, q.ResetDays) {
			return q, errors.New("下次重置的时间不能比一个周期还远")
		}
		q.NextReset = *p.NextReset
	case cycleChanged || q.NextReset <= 0:
		q.NextReset = model.NextQuotaReset(q.ResetMode, q.ResetDays, q.ResetDay, now, s.panelLocation())
	}
	return q, nil
}

// saveQuotaSettings 把 quotaAfter 算好的设置写库(只写设置那几列:用量、状态、游标不动)。fresh = 原来没有记录。
func saveQuotaSettings(tx *gorm.DB, q model.NodeQuota, fresh bool) error {
	if fresh {
		return tx.Create(&q).Error
	}
	return tx.Model(&model.NodeQuota{}).Where("node_id = ?", q.NodeId).Updates(map[string]interface{}{
		"volume": q.Volume, "expiry": q.Expiry, "reset_mode": q.ResetMode, "reset_days": q.ResetDays, "reset_day": q.ResetDay,
		"next_reset": q.NextReset, "keep_on": q.KeepOn,
	}).Error
}

// handleNodeQuota nodes/{id}/quota:
//
//	GET  看这台的额度;
//	PUT  {volume, expiry, resetMode, resetDays, resetDay, nextReset, autoDisable} 保存设置(没给的字段不动),保存后马上判一次;
//	POST nodes/{id}/quota/reset 把本期用量清零(新的一期从现在起算),马上判一次。
//
// 只在主机上改;副机的服务器页是只读的。
func (s *Server) handleNodeQuota(w http.ResponseWriter, r *http.Request, node model.Node, rest []string) {
	if r.Method != http.MethodGet && s.role() == "node" {
		writeJSON(w, http.StatusForbidden, map[string]string{"error": nodeReadOnly})
		return
	}
	reply := func() {
		var q model.NodeQuota
		s.db.Where("node_id = ?", node.Id).Limit(1).Find(&q)
		var n model.Node
		s.db.First(&n, node.Id)
		if q.NodeId == 0 {
			writeJSON(w, http.StatusOK, map[string]interface{}{"quota": nil, "enabled": n.Enabled, "suspended": n.Suspended})
			return
		}
		writeJSON(w, http.StatusOK, map[string]interface{}{"quota": quotaView(q, n), "enabled": n.Enabled, "suspended": n.Suspended})
	}
	if len(rest) == 1 && rest[0] == "reset" {
		if r.Method != http.MethodPost {
			writeJSON(w, http.StatusMethodNotAllowed, map[string]string{"error": "方法不允许"})
			return
		}
		now := time.Now().Unix()
		s.quotaMu.Lock()
		err := s.db.Transaction(func(tx *gorm.DB) error {
			var q model.NodeQuota
			if err := tx.Where("node_id = ?", node.Id).Limit(1).Find(&q).Error; err != nil {
				return err
			}
			if q.NodeId == 0 {
				return nil // 还没有记录:没有用量可清
			}
			upd := map[string]interface{}{
				"total_up": gorm.Expr("total_up + up"), "total_down": gorm.Expr("total_down + down"), "up": 0, "down": 0, "period_start": now,
			}
			// 按天数重置的:手动清零就是新的一期,下一次重置从现在重新数(和用户的重置流量一样);按月的还是那个日子
			if q.ResetMode == model.QuotaResetDays {
				upd["next_reset"] = model.NextQuotaReset(q.ResetMode, q.ResetDays, q.ResetDay, now, s.panelLocation())
			}
			return tx.Model(&model.NodeQuota{}).Where("node_id = ?", node.Id).Updates(upd).Error
		})
		s.quotaMu.Unlock()
		if err != nil {
			badRequest(w, err)
			return
		}
		s.audit(r, "node", "quota-reset", node.Name)
		s.NodeQuotaTick()
		reply()
		return
	}
	if len(rest) != 0 {
		http.NotFound(w, r)
		return
	}
	switch r.Method {
	case http.MethodGet:
		reply()
	case http.MethodPut:
		var p nodeQuotaReq
		if err := json.NewDecoder(r.Body).Decode(&p); err != nil {
			badRequest(w, err)
			return
		}
		now := time.Now().Unix()
		s.quotaMu.Lock()
		err := s.db.Transaction(func(tx *gorm.DB) error {
			var q model.NodeQuota
			if err := tx.Where("node_id = ?", node.Id).Limit(1).Find(&q).Error; err != nil {
				return err
			}
			fresh := q.NodeId == 0
			next, err := s.quotaAfter(q, node.Id, p, now)
			if err != nil {
				return err
			}
			return saveQuotaSettings(tx, next, fresh)
		})
		s.quotaMu.Unlock()
		if err != nil {
			badRequest(w, err)
			return
		}
		s.audit(r, "node", "quota-set", node.Name)
		s.NodeQuotaTick() // 改完马上生效:调高了额度 / 延了期的立刻恢复,调低到已用以下的立刻停
		reply()
	default:
		writeJSON(w, http.StatusMethodNotAllowed, map[string]string{"error": "方法不允许"})
	}
}

// checkNodeQuota 校验并规整一份额度设置(q 是改完的样子,was 是原来的)。expirySet = 这一次请求里动了到期时间。
func checkNodeQuota(q *model.NodeQuota, was model.NodeQuota, expirySet bool, now int64) error {
	if q.Volume < 0 || q.Volume > maxNodeQuota {
		return errors.New("流量额度填 0(不限)或一个正数")
	}
	if q.Expiry < 0 {
		return errors.New("到期时间不对")
	}
	// 新填的到期时间已经过了:多半是填错了,而一保存这台就会被停用。原来就过期的(没动它)不拦
	if expirySet && q.Expiry > 0 && q.Expiry <= now && q.Expiry != was.Expiry {
		return errors.New("到期时间已经过了:填一个将来的时间,或者留空不设")
	}
	switch q.ResetMode {
	case model.QuotaResetNone:
		q.ResetDays, q.ResetDay = 0, 0
	case model.QuotaResetDays:
		if q.ResetDays < 1 || q.ResetDays > 3650 {
			return errors.New("重置周期填 1–3650 天")
		}
		q.ResetDay = 0
	case model.QuotaResetMonth:
		if q.ResetDay < 1 || q.ResetDay > 31 {
			return errors.New("每月重置的日子填 1–31")
		}
		q.ResetDays = 0
	default:
		return errors.New("重置周期只能是不重置、按天数或每月")
	}
	return nil
}

// NodeQuotaTick 判一轮全部服务器的额度(见文件开头)。主机上才判;副机、测试里没有数据面时什么都不做。
// 串行:接口保存后的那一次和定时的不会同时跑。
func (s *Server) NodeQuotaTick() {
	if s.role() == "node" {
		return
	}
	s.quotaMu.Lock()
	defer s.quotaMu.Unlock()
	var quotas []model.NodeQuota
	if err := s.db.Order("node_id asc").Find(&quotas).Error; err != nil || len(quotas) == 0 {
		return
	}
	var nodes []model.Node
	if err := s.db.Find(&nodes).Error; err != nil {
		return
	}
	byId := make(map[uint]model.Node, len(nodes))
	for _, n := range nodes {
		byId[n.Id] = n
	}
	now := time.Now().Unix()
	loc := s.panelLocation()
	localChanged := false
	for _, q := range quotas {
		n, ok := byId[q.NodeId]
		if !ok { // 服务器删了,记录没跟着删干净(并账和删除撞上):清掉
			s.db.Where("node_id = ?", q.NodeId).Delete(&model.NodeQuota{})
			continue
		}
		// 1) 周期重置
		if q.ResetMode != model.QuotaResetNone && q.NextReset <= 0 {
			q.NextReset = model.NextQuotaReset(q.ResetMode, q.ResetDays, q.ResetDay, now, loc)
			s.db.Model(&model.NodeQuota{}).Where("node_id = ?", q.NodeId).Update("next_reset", q.NextReset)
		}
		if roll := model.RollQuota(q, now, loc); roll != nil {
			if err := s.db.Model(&model.NodeQuota{}).Where("node_id = ?", q.NodeId).Updates(roll).Error; err != nil {
				logger.Warning("服务器 ", n.Name, " 的流量额度到期重置失败: ", err)
				continue
			}
			if err := s.db.Where("node_id = ?", q.NodeId).First(&q).Error; err != nil {
				continue
			}
			s.auditAs(model.ActorQuotaJob, "node", "quota-reset", n.Name)
			logger.Info("服务器 ", n.Name, " 的流量额度按周期重置,新的一期开始")
		}
		// 2) 状态 → 3) 处置
		if s.applyQuotaState(n, q, q.StateNow(now), now) && n.IsLocal {
			localChanged = true
		}
	}
	if localChanged {
		s.reloadAll("本机的流量额度状态变了") // 本机被停止服务 / 恢复:用户全撤(或回来)、中转入口关(或开),要全量重载
	}
}

// applyQuotaState 按判出来的状态处置一台服务器;返回这台的「停止服务」标记(nodes.suspended)有没有变。
func (s *Server) applyQuotaState(n model.Node, q model.NodeQuota, state string, now int64) (heldChanged bool) {
	block := state != model.QuotaOK
	wantOff := block && !q.KeepOn && !n.IsLocal // 整台停用(主机自己那台停用不了)
	wantHold := block && !wantOff               // 只停代理服务
	if state != q.State {
		if err := s.db.Model(&model.NodeQuota{}).Where("node_id = ?", n.Id).Updates(map[string]interface{}{"state": state, "state_at": now}).Error; err != nil {
			logger.Warning("服务器 ", n.Name, " 的额度状态没记下: ", err)
			return false
		}
		s.noteQuotaState(n, q, state, wantOff)
	}
	switch {
	case wantOff && n.Enabled:
		s.quotaDisable(n, state)
	case !wantOff && q.AutoOff:
		// 自动停的:现在不该停了(恢复正常,或者改成了「只停代理服务」)。它要是已经被人手动启用了,只把记号去掉
		s.quotaEnable(n)
	}
	if n.Suspended != wantHold {
		if err := s.db.Model(&model.Node{}).Where("id = ?", n.Id).Update("suspended", wantHold).Error; err != nil {
			logger.Warning("服务器 ", n.Name, " 的停止服务标记没写下: ", err)
			return false
		}
		return true
	}
	return false
}

// quotaDisable 额度用完 / 到期:把这台服务器停用。和手动停用同一条路 —— 改库之前记下它此刻的配置,改完给它推一份空用户表
// (没推到的主机每轮重试),容灾里把它从同伴里去掉 —— 另记一笔「这是自动停的」。
func (s *Server) quotaDisable(n model.Node, state string) {
	before, err := hub.BuildSnapshot(s.db, s.setting)
	if err != nil {
		logger.Warning("服务器 ", n.Name, " 额度用完,自动停用前构造快照失败(下一轮再试): ", err)
		return
	}
	done := false
	if err := s.db.Transaction(func(tx *gorm.DB) error {
		res := tx.Model(&model.Node{}).Where("id = ? AND enabled = ?", n.Id, true).Update("enabled", false)
		if res.Error != nil || res.RowsAffected == 0 {
			return res.Error // 没改到:刚被人手动停了,不算自动停的
		}
		done = true
		return tx.Model(&model.NodeQuota{}).Where("node_id = ?", n.Id).Update("auto_off", true).Error
	}); err != nil || !done {
		if err != nil {
			logger.Warning("服务器 ", n.Name, " 自动停用失败(下一轮再试): ", err)
		}
		return
	}
	s.auditAs(model.ActorQuotaJob, "node", "quota-off:"+state, n.Name)
	logger.Info("服务器 ", n.Name, " ", quotaStateText(state), ",已自动停用")
	s.syncRelays()
	s.decommission(n, before) // 没推到的记在 node_decommissions 里,主机每轮重试,服务器页有提示
	s.haForget(n, false)
}

// quotaEnable 自动停用的服务器恢复:重新启用(还没送到的下线通知作废,定时同步推回完整快照),去掉「自动停的」记号。
func (s *Server) quotaEnable(n model.Node) {
	enabled := false
	if err := s.db.Transaction(func(tx *gorm.DB) error {
		res := tx.Model(&model.Node{}).Where("id = ? AND enabled = ?", n.Id, false).Update("enabled", true)
		if res.Error != nil {
			return res.Error
		}
		enabled = res.RowsAffected > 0
		if enabled {
			if err := tx.Where("node_id = ?", n.Id).Delete(&model.NodeDecommission{}).Error; err != nil {
				return err
			}
		}
		return tx.Model(&model.NodeQuota{}).Where("node_id = ?", n.Id).Update("auto_off", false).Error
	}); err != nil {
		logger.Warning("服务器 ", n.Name, " 自动启用失败(下一轮再试): ", err)
		return
	}
	if !enabled {
		return
	}
	s.auditAs(model.ActorQuotaJob, "node", "quota-on", n.Name)
	logger.Info("服务器 ", n.Name, " 的流量额度恢复,已自动启用")
	s.syncRelays()
}

func quotaStateText(state string) string {
	if state == model.QuotaExpired {
		return "已到期"
	}
	return "流量用完"
}

// noteQuotaState 状态变了:记审计、发通知(和服务器离线、磁盘快满同一个开关)。
func (s *Server) noteQuotaState(n model.Node, q model.NodeQuota, state string, off bool) {
	name := html.EscapeString(n.Name)
	if state == model.QuotaOK {
		if !q.AutoOff { // 自动停用的那种在重新启用时记(quota-on),不重复记一笔
			s.auditAs(model.ActorQuotaJob, "node", "quota-ok", n.Name)
		}
		if s.run != nil {
			s.run.Notifier().Event("tgOnCore", fmt.Sprintf("✅ <b>服务器恢复服务</b>:%s(流量额度已恢复)", name))
		}
		return
	}
	how := "已停止这台上的代理服务(服务器仍在同步,同步本身还会消耗少量流量)"
	if off {
		how = "已自动停用这台服务器" // 审计在真正停用时记(quota-off,见 quotaDisable):它要是本来就手动停着,这里什么都没做
	} else {
		s.auditAs(model.ActorQuotaJob, "node", "quota-hold:"+state, n.Name)
	}
	if s.run == nil {
		return
	}
	head := "服务器流量用完"
	detail := fmt.Sprintf("本期已用 %s / %s", quotaBytes(q.Used()), quotaBytes(q.Volume))
	then := "加额度或重置用量后自动恢复"
	if q.ResetMode != model.QuotaResetNone && q.NextReset > 0 {
		then = time.Unix(q.NextReset, 0).In(s.panelLocation()).Format("1 月 2 日") + " 重置后自动恢复"
	}
	if state == model.QuotaExpired {
		head, detail, then = "服务器已到期", "到期时间 "+time.Unix(q.Expiry, 0).In(s.panelLocation()).Format("2006-01-02 15:04"), "延长到期时间后自动恢复"
	}
	s.run.Notifier().Event("tgOnCore", fmt.Sprintf("⛔ <b>%s</b>:%s(%s),%s;%s", head, name, detail, how, then))
}

// quotaBytes 通知里写用量:按 GB / TB(1024 进制,和面板里一致)。
func quotaBytes(n int64) string {
	const gb = float64(1 << 30)
	f := float64(n) / gb
	if f >= 1024 {
		return fmt.Sprintf("%.2f TB", f/1024)
	}
	return fmt.Sprintf("%.2f GB", f)
}

// quotaBlocksEnable 手动启用一台服务器之前问一句:它是不是正被额度拦着(用完 / 到期,而且设的是自动停用)。是的话返回原因 ——
// 启用了下一分钟又会被停掉,不如直接说清楚该去改什么。with 是这一次保存里一起改的额度(没有为 nil,按库里的判):
// 同一次保存里调高了额度、延了期再启用,是放行的。
func (s *Server) quotaBlocksEnable(n model.Node, with *model.NodeQuota) error {
	var q model.NodeQuota
	if with != nil {
		q = *with
	} else if err := s.db.Where("node_id = ?", n.Id).Limit(1).Find(&q).Error; err != nil {
		return nil
	}
	if q.NodeId == 0 || q.KeepOn {
		return nil
	}
	switch q.StateNow(time.Now().Unix()) {
	case model.QuotaTraffic:
		return errors.New("这台服务器本期流量已用完,按设置自动停用着:调高额度或重置用量后它会自动启用(关掉「用完或到期后自动停用」只是不停用整台,代理服务仍然停着)")
	case model.QuotaExpired:
		return errors.New("这台服务器已到期,按设置自动停用着:延长到期时间后它会自动启用(关掉「用完或到期后自动停用」只是不停用整台,代理服务仍然停着)")
	}
	return nil
}
