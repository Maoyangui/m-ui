package web

import (
	"crypto/rand"
	"crypto/subtle"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"net"
	"net/http"
	"os"
	"sort"
	"strconv"
	"strings"
	"time"

	"github.com/Maoyangui/m-ui/database/model"
	"github.com/Maoyangui/m-ui/ha"
	"github.com/Maoyangui/m-ui/hub"
	"github.com/Maoyangui/m-ui/logger"
	"github.com/Maoyangui/m-ui/monitor"
	"github.com/Maoyangui/m-ui/selfupdate"

	"gorm.io/gorm"
)

// ---- 副机端:供主机调用的接口(令牌鉴权,不走会话) ----

func (s *Server) nodeToken() (string, error) {
	tok := s.setting("nodeToken")
	if tok == "" {
		b := make([]byte, 24)
		if _, err := rand.Read(b); err != nil {
			return "", err
		}
		tok = hex.EncodeToString(b)
		if err := s.run.SetSetting("nodeToken", tok); err != nil {
			return "", fmt.Errorf("保存副机令牌: %w", err)
		}
	}
	return tok, nil
}

// agentAuth 主机调用副机接口的鉴权,顺序是 令牌 → 纪元 → 副机模式。
// 临时主机也保留自己的副机令牌:被接管的旧主机带着旧纪元来请求时,收到的是 409 stale_epoch(它据此降级)而不是 403。
// 没开过容灾(本机纪元为 0、请求也不带纪元)时和以前一样:不是副机就 403,令牌错 401。
func (s *Server) agentAuth(next http.HandlerFunc) http.HandlerFunc {
	return func(w http.ResponseWriter, r *http.Request) {
		isNode := strings.EqualFold(s.setting("nodeMode"), "true")
		h := s.haSvc()
		haActive := h != nil && (r.Header.Get(ha.HeaderEpoch) != "" || h.Epoch() > 0)
		if !isNode && (!haActive || s.setting("nodeToken") == "") {
			writeJSON(w, http.StatusForbidden, map[string]string{"error": "本机不是副机(设置里未开启副服务器模式)"})
			return
		}
		tok := r.Header.Get("X-Agent-Token")
		want, err := s.nodeToken()
		if err != nil {
			writeJSON(w, http.StatusServiceUnavailable, map[string]string{"error": err.Error()})
			return
		}
		if tok == "" || subtle.ConstantTimeCompare([]byte(tok), []byte(want)) != 1 {
			time.Sleep(200 * time.Millisecond)
			writeJSON(w, http.StatusUnauthorized, map[string]string{"error": "令牌错误"})
			return
		}
		if haActive {
			action := strings.TrimPrefix(r.URL.Path, innerBase+"api/agent/")
			if rej := h.CheckAgent(r.Header, action); rej != nil {
				w.Header().Set(ha.HeaderNode, strconv.FormatUint(h.Epoch(), 10))
				writeJSON(w, http.StatusConflict, rej)
				return
			}
			w.Header().Set(ha.HeaderNode, strconv.FormatUint(h.Epoch(), 10))
		}
		if !isNode {
			writeJSON(w, http.StatusForbidden, map[string]string{"error": "本机不是副机(设置里未开启副服务器模式)"})
			return
		}
		next(w, r)
	}
}

// haSvc 本机的容灾服务(测试里拼出来的 Server 可能没有)。
func (s *Server) haSvc() *ha.Service {
	if s.run == nil {
		return nil
	}
	return s.run.HA()
}

func (s *Server) handleAgent(w http.ResponseWriter, r *http.Request) {
	action := strings.TrimPrefix(r.URL.Path, innerBase+"api/agent/")
	switch action {
	case "info": // 会话鉴权:副机面板展示配对信息
		s.auth(s.handleAgentInfo)(w, r)
	case "rotate":
		s.auth(s.handleAgentRotate)(w, r)
	case "ping":
		s.agentAuth(s.handleAgentPing)(w, r)
	case "apply":
		s.agentAuth(s.handleAgentApply)(w, r)
	case "report":
		s.agentAuth(s.handleAgentReport)(w, r)
	case "external-ips":
		s.agentAuth(s.handleAgentExternalIPs)(w, r)
	case "kick":
		s.agentAuth(s.handleAgentKick)(w, r)
	case "upstream-test": // 主机让本机立刻测一条上游(面板的"测试"按钮派发过来)
		s.agentAuth(s.handleAgentUpstreamTest)(w, r)
	case "upstream-check": // 主机让本机立刻跑一轮巡检(概览的「立即巡检」派发过来),返回本机全部结果
		s.agentAuth(s.handleAgentUpstreamCheck)(w, r)
	case "outbound-test": // 主机让本机实测一个临时出站(外部订阅展开后的逐台测速),不落库
		s.agentAuth(s.handleAgentOutboundTest)(w, r)
	case "ha/state": // 主机推来的容灾状态(纪元、候选名单、暂停、交接标记……)
		s.agentAuth(s.handleAgentHAState)(w, r)
	default:
		http.NotFound(w, r)
	}
}

func (s *Server) handleAgentInfo(w http.ResponseWriter, r *http.Request) {
	tok, err := s.nodeToken()
	if err != nil {
		writeJSON(w, http.StatusServiceUnavailable, map[string]string{"error": err.Error()})
		return
	}
	writeJSON(w, http.StatusOK, map[string]interface{}{
		"isNode": strings.EqualFold(s.setting("nodeMode"), "true"),
		"token":  tok, "revision": s.setting("hubRevision"), "appliedAt": s.setting("hubAppliedAt"),
		"apiUrl": s.selfApiURL(),
	})
}

func (s *Server) handleAgentRotate(w http.ResponseWriter, r *http.Request) {
	if r.Method != http.MethodPost {
		writeJSON(w, http.StatusMethodNotAllowed, map[string]string{"error": "方法不允许"})
		return
	}
	if err := s.run.SetSetting("nodeToken", ""); err != nil {
		writeJSON(w, http.StatusServiceUnavailable, map[string]string{"error": "保存令牌重置状态失败: " + err.Error()})
		return
	}
	s.audit(r, "agent", "rotate-token", nil)
	tok, err := s.nodeToken()
	if err != nil {
		writeJSON(w, http.StatusServiceUnavailable, map[string]string{"error": err.Error()})
		return
	}
	writeJSON(w, http.StatusOK, map[string]string{"token": tok})
}

// selfApiURL 猜测本机面板对外地址,便于复制到主机。
func (s *Server) selfApiURL() string {
	scheme := "http"
	if s.setting("webCertFile") != "" {
		scheme = "https"
	}
	host := s.setting("webDomain")
	if host == "" {
		host = s.run.PublicHost()
	}
	if host == "" {
		host = "<本机IP>"
	}
	return fmt.Sprintf("%s://%s%s", scheme, model.URLHost(scheme, host, s.settingInt("webPort", 2053)), s.basePath())
}

func (s *Server) handleAgentPing(w http.ResponseWriter, r *http.Request) {
	host, _ := os.Hostname()
	writeJSON(w, http.StatusOK, map[string]interface{}{
		"version": Version, "role": s.role(), "hostname": host,
		"coreRunning": s.run.CoreRunning(), "uptime": s.run.Uptime(),
		"revision": s.setting("hubRevision"), "certDays": s.run.CertDaysInUse(),
	})
}

// agentApplyNeedsFullReload keeps the data plane converging after a failed
// snapshot application. The pending marker is deliberately independent of
// the incoming revision: a newer user-only snapshot can arrive while the
// previous line/upstream change is still unapplied in the running core.
func agentApplyNeedsFullReload(previousPending string, linesChanged, coreRunning bool) bool {
	return !coreRunning || linesChanged || strings.TrimSpace(previousPending) != ""
}

func (s *Server) handleAgentApply(w http.ResponseWriter, r *http.Request) {
	if r.Method != http.MethodPost {
		writeJSON(w, http.StatusMethodNotAllowed, map[string]string{"error": "方法不允许"})
		return
	}
	snap, err := hub.DecodeSnapshot(w, r) // 新主机发 gzip 压缩的,老主机发不压缩的;解压后最多 512 MiB
	if err != nil {
		badRequest(w, err)
		return
	}
	if snap.Revision == "" {
		badRequest(w, errors.New("快照缺少修订号"))
		return
	}
	if snap.MinNode != "" && selfupdate.Newer(snap.MinNode, Version) {
		// 主机的快照里有本机不认识的字段(比如停用原因、代理额度标志):应用了也是错的,直接拒绝并说清楚
		writeJSON(w, http.StatusConflict, map[string]string{"error": fmt.Sprintf("副机版本 v%s 低于主机要求的 v%s,请先升级这台副机", Version, snap.MinNode)})
		return
	}
	if s.run == nil {
		writeJSON(w, http.StatusServiceUnavailable, map[string]string{"error": "本机数据面未初始化,暂不能确认配置已应用"})
		return
	}
	s.agentApplyMu.Lock()
	defer s.agentApplyMu.Unlock()
	// 排队等上一份应用完的时候主机可能已经放弃了这次请求(超时、停机):不再整表重写,下一轮它会带着最新的那份再来
	if r.Context().Err() != nil {
		logger.Info("主机已经放弃了这次推送(", snap.Revision, "),不再应用")
		return
	}
	var previousPending string
	if err := s.db.Model(&model.Setting{}).Select("value").Where("key = ?", "hubReloadPending").Scan(&previousPending).Error; err != nil {
		writeJSON(w, http.StatusServiceUnavailable, map[string]string{"error": "读取副机待重载状态失败: " + err.Error()})
		return
	}
	// 同一修订、同一序号已经完整应用过(库里是它、数据面也应用成功了、没有待踢线):直接确认,不再整表删了重插、重载一遍。
	// 主机等确认超时后的重推、每 10 分钟一次的定期重推都会这样;以前每次都整表重写,大库上几十秒,WAL 跟着涨
	if s.agentAlreadyApplied(snap, previousPending) {
		writeJSON(w, http.StatusOK, map[string]string{"ok": "1", "revision": snap.Revision})
		return
	}
	revoked, err := hub.RevokedSharesChecked(s.db, snap)
	if err != nil {
		writeJSON(w, http.StatusServiceUnavailable, map[string]string{"error": "读取旧共享凭据失败: " + err.Error()})
		return
	}
	rotated, err := hub.RotatedUsersChecked(s.db, snap)
	if err != nil {
		writeJSON(w, http.StatusServiceUnavailable, map[string]string{"error": "读取旧订阅凭据失败: " + err.Error()})
		return
	}
	pendingKick, err := s.pendingAgentKicks()
	if err != nil {
		writeJSON(w, http.StatusServiceUnavailable, map[string]string{"error": "读取副机待踢线状态失败: " + err.Error()})
		return
	}
	kickSet := make(map[string]bool, len(pendingKick)+len(rotated))
	for _, name := range pendingKick {
		kickSet[name] = true
	}
	for _, name := range rotated {
		kickSet[name] = true
	}
	kickNames := sortedNames(kickSet)
	// 先把目标持久化,再进行可能失败的网络/内核操作。这样即使本次
	// 请求在踢线中途断开,下一次同一修订仍会继续处理旧凭据连接。
	if err := s.saveAgentKicks(kickNames); err != nil {
		writeJSON(w, http.StatusServiceUnavailable, map[string]string{"error": "记录副机待踢线状态失败: " + err.Error()})
		return
	}
	linesChanged, upsChanged, err := hub.ApplySnapshot(s.db, snap)
	if err != nil {
		badRequest(w, err)
		return
	}
	// ================= D. 本机账本只增不删:主机已经不认识的用户,它的计数器没人再回收,清掉 =================
	// 主机那边的游标还留着;同名用户以后再建,计数从 0 起小于游标,会被当成回绕重认,不会多算
	s.db.Exec("DELETE FROM agent_counters WHERE user_name NOT IN (SELECT name FROM users)")
	// 配置接口只有在本机数据面真正应用完成后才确认 revision。这样主机
	// 看到 HTTP 错误会保留重试机会，而不是把“请求已收到”误当成“已生效”。
	// 数据库快照先落地、数据面后重载。若重载失败，保留一个只在本机使用的
	// 待重载标记；同一 revision 重试时必须再次走全量重载，不能因表已经
	// 写入而误降级成 ReloadUsers。
	// 事务会把本次修订记为待重载，保证落库后崩溃也能恢复。重载级别只看
	// ApplySnapshot 前的标记；只有同一修订的待重载才强制重建，旧修订仍
	// 按当前快照的线路/上游差异选择热更新。
	// Any pending marker means the previous attempt did not prove that the
	// data-plane accepted its snapshot.  A newer snapshot may have a different
	// revision (for example, only a user changed), but the database already
	// contains the newer lines/upstreams while the running core still has the
	// older failed configuration.  Always force the full reconciliation until
	// the marker is cleared by a successful reload.
	pendingReload := agentApplyNeedsFullReload(previousPending, linesChanged, s.run.CoreRunning())
	var reloadErr error
	switch {
	case pendingReload:
		reloadErr = s.run.ReloadAll()
	case upsChanged:
		reloadErr = s.run.ReloadUpstreams()
	default:
		reloadErr = s.run.ReloadUsers()
	}
	if reloadErr == nil && !s.run.CoreRunning() {
		reloadErr = errors.New("本机数据面未运行，不能确认快照已应用")
	}
	if reloadErr != nil {
		if err := s.run.SetSetting("hubReloadPending", snap.Revision); err != nil {
			logger.Error("记录副机待重载状态失败: ", err)
		}
		logger.Warning("应用主机配置 ", snap.Revision, " 失败: ", reloadErr)
		// 配置没应用成功,不等于执法可以停:运行中的数据面(回滚后的旧配置)上,用户表已经尽量热更新过了
		// (见 runner 的冷却期逻辑),这里把该踢的人先在本机踢掉。待踢名单留着,下次成功应用时会再踢一遍(幂等)。
		// 0.6.10 在这里直接 return,于是待重载期间停用、换凭据的用户在这台副机上一直连得上。
		s.kickBestEffort(revoked, kickNames)
		writeJSON(w, http.StatusServiceUnavailable, map[string]string{"error": "本机数据面应用失败: " + reloadErr.Error()})
		return
	}
	if err := s.run.SetSetting("hubReloadPending", ""); err != nil {
		// 快照已经应用,但不清标记比错误地报告已清理更安全;主机重试会
		// 再次确认同一 revision 的数据面状态。
		logger.Error("清除副机待重载状态失败: ", err)
		writeJSON(w, http.StatusServiceUnavailable, map[string]string{"error": "数据面已应用,但待重载状态未能持久化: " + err.Error()})
		return
	}
	if len(revoked) > 0 || len(kickNames) > 0 {
		for _, name := range revoked {
			if n := s.run.KickShare(name); n > 0 {
				logger.Info("临时共享已取消,断开 ", name, " 的 ", n, " 条连接")
			}
		}
		for i, name := range kickNames {
			res := s.run.KickUserAll(name)
			if res.Failed > 0 {
				// 保留未完成的目标;已完成的从队列中去掉,避免每次
				// 重试都重复踢大量已经处理完的用户。
				remaining := make(map[string]bool)
				for _, n := range kickNames[i:] {
					remaining[n] = true
				}
				_ = s.saveAgentKicks(sortedNames(remaining))
				writeJSON(w, http.StatusServiceUnavailable, map[string]string{"error": fmt.Sprintf("用户 %s 踢线未完成，%d 台副机失败", name, res.Failed)})
				return
			}
			if res.Closed > 0 {
				logger.Info("订阅链接已重置,断开 ", name, " 旧凭据上的 ", res.Closed, " 条连接")
			}
		}
	}
	if err := s.saveAgentKicks(nil); err != nil {
		writeJSON(w, http.StatusServiceUnavailable, map[string]string{"error": "清除副机待踢线状态失败: " + err.Error()})
		return
	}
	if err := s.run.SetSetting(agentAppliedKey, agentAppliedStamp(snap)); err != nil {
		// 不影响这次确认:没记下来,同一份快照再来时整份重做一遍而已
		logger.Warning("记录本版程序已应用快照 ", snap.Revision, " 失败: ", err)
	}
	logger.Info("已应用主机配置 ", snap.Revision, "(线路变化: ", linesChanged, ",上游变化: ", upsChanged, ")")
	writeJSON(w, http.StatusOK, map[string]string{"ok": "1", "revision": snap.Revision})
}

// agentAlreadyApplied 这份快照是不是已经完整应用过:没有待重载、数据面在跑,库里也正是它(见 agentSnapshotInDB)。
// 老主机不发序号(0),不走这条捷径。
func (s *Server) agentAlreadyApplied(snap hub.Snapshot, pendingReload string) bool {
	if snap.Sequence == 0 || strings.TrimSpace(pendingReload) != "" || !s.run.CoreRunning() {
		return false
	}
	return agentSnapshotInDB(s.db, snap)
}

// agentAppliedKey 记下"哪一版程序完整应用了哪一份快照"(见 agentAppliedStamp)。
//
// 旧版程序收快照时会丢掉自己不认识的字段(比如落地页 logo),修订号和序号却照写:只比这两项的话,副机升级以后
// 主机重推的同一份快照永远被当成"已经应用过",新字段一直落不了库,主机那边还显示已同步。
// 旧版不写这个键,所以升级后、降级期间收过新快照再升回来、换了任何版本,第一份快照都会整份应用一次。
const agentAppliedKey = "hubAppliedBy"

func agentAppliedStamp(snap hub.Snapshot) string {
	return Version + "#" + snap.Revision + "#" + strconv.FormatUint(snap.Sequence, 10)
}

// agentSnapshotInDB 库里是不是已经是这份快照:修订号、序号一样且是本版程序完整应用的,没有待踢线,本机 id 一样,
// 落地页 logo 也和快照里的一张不差(旧版主机不发 logo = 一张都没有)。
func agentSnapshotInDB(db *gorm.DB, snap hub.Snapshot) bool {
	var rows []model.Setting
	if err := db.Where("key IN ?", []string{"hubRevision", "hubSnapshotSequence", agentKickPendingKey, agentAppliedKey}).Find(&rows).Error; err != nil {
		return false
	}
	have := map[string]string{}
	for _, row := range rows {
		have[row.Key] = row.Value
	}
	if have["hubRevision"] != snap.Revision || have["hubSnapshotSequence"] != strconv.FormatUint(snap.Sequence, 10) ||
		have[agentAppliedKey] != agentAppliedStamp(snap) || strings.TrimSpace(have[agentKickPendingKey]) != "" {
		return false
	}
	var self []uint
	if err := db.Model(&model.Node{}).Where("is_local = ?", true).Pluck("id", &self).Error; err != nil {
		return false
	}
	if len(self) != 1 || self[0] != snap.SelfNodeId {
		return false
	}
	var logos []model.PageLogo
	if err := db.Select("reseller_id", "sum").Find(&logos).Error; err != nil {
		return false
	}
	return hub.LogosMatch(logos, snap.Logos)
}

// kickBestEffort 本机能踢多少踢多少,不因为副机没确认就一个都不踢。只在"配置应用失败"那条路上用。
func (s *Server) kickBestEffort(revoked, kickNames []string) {
	for _, name := range revoked {
		if n := s.run.KickShare(name); n > 0 {
			logger.Info("临时共享已取消,断开 ", name, " 的 ", n, " 条连接")
		}
	}
	for _, name := range kickNames {
		if closed, sessions := s.run.KickUserLocal(name); closed > 0 || sessions > 0 {
			logger.Info("配置应用失败期间先在本机断开 ", name, " 旧凭据上的 ", closed, " 条连接、", sessions, " 条会话")
		}
	}
}

const agentKickPendingKey = "hubKickPending"

func (s *Server) pendingAgentKicks() ([]string, error) {
	var raw string
	if err := s.db.Raw("SELECT value FROM settings WHERE key = ?", agentKickPendingKey).Scan(&raw).Error; err != nil {
		return nil, err
	}
	if strings.TrimSpace(raw) == "" {
		return nil, nil
	}
	var names []string
	if err := json.Unmarshal([]byte(raw), &names); err != nil {
		return nil, fmt.Errorf("解析待踢线状态: %w", err)
	}
	set := make(map[string]bool, len(names))
	for _, name := range names {
		if name = strings.TrimSpace(name); name != "" {
			set[name] = true
		}
	}
	return sortedNames(set), nil
}

func sortedNames(set map[string]bool) []string {
	out := make([]string, 0, len(set))
	for name, keep := range set {
		if keep && strings.TrimSpace(name) != "" {
			out = append(out, name)
		}
	}
	sort.Strings(out)
	return out
}

func (s *Server) saveAgentKicks(names []string) error {
	value := ""
	if len(names) > 0 {
		b, err := json.Marshal(names)
		if err != nil {
			return err
		}
		value = string(b)
	}
	return s.run.SetSetting(agentKickPendingKey, value)
}

func (s *Server) handleAgentReport(w http.ResponseWriter, r *http.Request) {
	host, _ := os.Hostname()
	rep := hub.Report{
		Version: Version, Hostname: host, CoreRunning: s.run.CoreRunning(), Uptime: s.run.Uptime(),
		Revision: s.setting("hubRevision"), ReloadPending: strings.TrimSpace(s.setting("hubReloadPending")) != "",
		Onlines: map[string][]string{}, CertDays: s.run.CertDaysInUse(), Certs: s.run.CertBriefs(),
		PublicIP:     s.setting("publicIp"),
		PublicIP6:    s.setting("publicIp6"),
		Conns:        s.recentConns(50),
		ConnFails:    s.recentFails(30),
		ConnFailsOK:  true,
		LedgerEpoch:  s.setting(hub.LedgerEpochKey),
		ServicePorts: s.reportServicePorts(),
		KickPending:  strings.TrimSpace(s.setting(agentKickPendingKey)) != "",
		ApplyGzip:    true, // 本机收得了 gzip 压缩的快照
		LedgerFails:  s.run.LedgerFailures(),
	}
	if seq, err := strconv.ParseUint(s.setting("hubSnapshotSequence"), 10, 64); err == nil {
		rep.Sequence = seq
	}
	rep.OnlineLinesByIP = s.run.OnlineIPLines()
	rep.Groups = s.run.GroupState()
	if rl := s.run.ReloadStatus(); rl.At > 0 {
		rep.Reload = &hub.ReloadState{At: rl.At, Op: rl.Op, OK: rl.OK, Error: rl.Error}
	}
	for _, h := range s.run.UpstreamHealthLocal() { // 本机线路用到的那些上游,量出来的结果交给主机汇总
		rep.Upstreams = append(rep.Upstreams, hub.UpstreamHealth{Id: h.Id, Name: h.Name, OK: h.OK,
			DelayMs: h.DelayMs, Method: h.Method, Error: h.Error, CheckedAt: h.CheckedAt, Fails: h.Fails})
	}
	s.db.Find(&rep.Counters)
	o := s.run.Onlines()
	allIPs := s.run.OnlineIPsAll() // 一次锁拿全量,不按用户逐个抢数据面的锁
	for _, u := range o.Users {
		rep.Onlines[u] = allIPs[u]
	}
	// 本周期没流量但仍在线(空闲窗口内)的 IP 也要上报:用户自己的设备数限制、代理设备池都要靠它跨机并集判定
	for u, ips := range allIPs {
		if _, ok := rep.Onlines[u]; !ok && len(ips) > 0 {
			rep.Onlines[u] = ips
		}
	}
	rep.OnlineLines = o.Lines
	writeJSON(w, http.StatusOK, rep)
}

// externalIPsLimit 外部设备租约请求体的上限:有设备数限制的用户 × 几个 IP,几万用户也就几 MB。
var externalIPsLimit int64 = 32 << 20

func (s *Server) handleAgentExternalIPs(w http.ResponseWriter, r *http.Request) {
	var m map[string][]string
	if err := json.NewDecoder(http.MaxBytesReader(w, r.Body, externalIPsLimit)).Decode(&m); err != nil {
		badRequest(w, err)
		return
	}
	s.run.SetExternalIPs(m)
	writeJSON(w, http.StatusOK, map[string]string{"ok": "1"})
}

// handleAgentKick 主机要求副机断开某用户的现有连接。
// agentAuth 已确认请求来自已配对的主机;副机上的 Runner.KickUser 只会操作本机。
func (s *Server) handleAgentKick(w http.ResponseWriter, r *http.Request) {
	if r.Method != http.MethodPost {
		writeJSON(w, http.StatusMethodNotAllowed, map[string]string{"error": "方法不允许"})
		return
	}
	var body struct {
		Name string `json:"name"`
	}
	if err := json.NewDecoder(http.MaxBytesReader(w, r.Body, 1<<20)).Decode(&body); err != nil {
		badRequest(w, err)
		return
	}
	body.Name = strings.TrimSpace(body.Name)
	if body.Name == "" {
		badRequest(w, errors.New("用户名不能为空"))
		return
	}
	closed, sessions := s.run.KickUserLocal(body.Name)
	writeJSON(w, http.StatusOK, map[string]interface{}{"closed": closed, "sessions": sessions})
}

// handleAgentUpstreamTest 主机派发过来的单条上游测试:在本机实测并返回结果。
// 上游通不通要在真正跑这条线路的机器上量,面板的"测试"按钮因此要派发到各机而不是只测主机。
func (s *Server) handleAgentUpstreamTest(w http.ResponseWriter, r *http.Request) {
	if r.Method != http.MethodPost {
		writeJSON(w, http.StatusMethodNotAllowed, map[string]string{"error": "方法不允许"})
		return
	}
	var body struct {
		Id uint `json:"id"`
	}
	if err := json.NewDecoder(r.Body).Decode(&body); err != nil {
		badRequest(w, err)
		return
	}
	var up model.Upstream
	if err := s.db.First(&up, body.Id).Error; err != nil {
		writeJSON(w, http.StatusNotFound, map[string]string{"error": "上游不存在"})
		return
	}
	ok, ms, meth, errStr := s.run.CheckUpstream(up)
	now := time.Now().Unix()
	// 本机的巡检结果也更新:下一轮随报告上报给主机的就是这份,不会又退回旧状态
	s.run.Monitor().SetResult(monitor.UpstreamHealth{Id: up.Id, Name: up.Name, OK: ok, DelayMs: ms, Method: meth, Error: errStr, CheckedAt: now})
	writeJSON(w, http.StatusOK, hub.UpstreamHealth{Id: up.Id, Name: up.Name, OK: ok, DelayMs: ms,
		Method: meth, Error: errStr, CheckedAt: now})
}

// handleAgentOutboundTest 用主机发来的出站参数在本机真连一次(临时实例),结果原样返回。
func (s *Server) handleAgentOutboundTest(w http.ResponseWriter, r *http.Request) {
	if r.Method != http.MethodPost {
		writeJSON(w, http.StatusMethodNotAllowed, map[string]string{"error": "方法不允许"})
		return
	}
	var up model.Upstream
	if err := json.NewDecoder(r.Body).Decode(&up); err != nil {
		badRequest(w, err)
		return
	}
	if up.Type == "" || len(up.Options) == 0 {
		badRequest(w, errors.New("出站参数不完整"))
		return
	}
	if up.Name == "" {
		up.Name = "ext-test"
	}
	// 只有外部节点测速用这个接口:直接起临时实例测(不在数据面里找同名出站),method "config" = 出站建不起来、测不了
	ok, ms, meth, errStr := s.run.TestOutbound(r.Context(), up)
	writeJSON(w, http.StatusOK, hub.UpstreamHealth{Name: up.Name, OK: ok, DelayMs: ms, Method: meth, Error: errStr, CheckedAt: time.Now().Unix()})
}

// handleAgentUpstreamCheck 本机立刻跑一轮巡检,把全部结果交给主机(主机据此刷新汇总)。
func (s *Server) handleAgentUpstreamCheck(w http.ResponseWriter, r *http.Request) {
	if r.Method != http.MethodPost {
		writeJSON(w, http.StatusMethodNotAllowed, map[string]string{"error": "方法不允许"})
		return
	}
	s.run.Monitor().RunUpstreamCheck()
	local := s.run.UpstreamHealthLocal()
	out := make([]hub.UpstreamHealth, 0, len(local))
	for _, h := range local {
		out = append(out, hub.UpstreamHealth{Id: h.Id, Name: h.Name, OK: h.OK, DelayMs: h.DelayMs,
			Method: h.Method, Error: h.Error, CheckedAt: h.CheckedAt, Fails: h.Fails})
	}
	writeJSON(w, http.StatusOK, out)
}

// reportServicePorts 副机随报告上报自己监听的服务端口:配置值,外加正在监听、但配置已改(等重启)的旧端口。
// 副机不起代理面板(StartReseller),不报它。
func (s *Server) reportServicePorts() []model.ServicePort {
	out := []model.ServicePort{{Label: "面板", Port: s.settingInt("webPort", 2053)}, {Label: "订阅", Port: s.settingInt("subPort", 2056)}}
	if s.listener != nil {
		if a, ok := s.listener.Addr().(*net.TCPAddr); ok && a.Port != out[0].Port {
			out = append(out, model.ServicePort{Label: "面板", Port: a.Port})
		}
	}
	return out
}

// handleAgentHAState 收下主机推来的容灾状态(纪元已在 agentAuth 里对齐)。
func (s *Server) handleAgentHAState(w http.ResponseWriter, r *http.Request) {
	if r.Method != http.MethodPost {
		writeJSON(w, http.StatusMethodNotAllowed, map[string]string{"error": "方法不允许"})
		return
	}
	h := s.haSvc()
	if h == nil {
		writeJSON(w, http.StatusServiceUnavailable, map[string]string{"error": "本机容灾服务未初始化"})
		return
	}
	var st ha.State
	if err := json.NewDecoder(http.MaxBytesReader(w, r.Body, 1<<20)).Decode(&st); err != nil {
		badRequest(w, err)
		return
	}
	if err := h.ApplyState(st); err != nil {
		badRequest(w, err)
		return
	}
	writeJSON(w, http.StatusOK, map[string]string{"ok": "1"})
}
