// Package runner 启动 m-ui 数据面:渲染配置 → 拉起内嵌 sing-box → 应用用户限速/设备数策略。
package runner

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"os"
	"os/signal"
	"runtime/debug"
	"strings"
	"sync"
	"sync/atomic"
	"syscall"
	"time"

	"github.com/Maoyangui/m-ui/acme"
	"github.com/Maoyangui/m-ui/backup"
	"github.com/Maoyangui/m-ui/core"
	"github.com/Maoyangui/m-ui/creds"
	"github.com/Maoyangui/m-ui/database"
	"github.com/Maoyangui/m-ui/database/model"
	"github.com/Maoyangui/m-ui/hop"
	"github.com/Maoyangui/m-ui/hub"
	"github.com/Maoyangui/m-ui/jobs"
	"github.com/Maoyangui/m-ui/logger"
	"github.com/Maoyangui/m-ui/monitor"
	"github.com/Maoyangui/m-ui/notify"
	"github.com/Maoyangui/m-ui/render"
	"github.com/Maoyangui/m-ui/rules"
	"github.com/Maoyangui/m-ui/sub"
	"github.com/Maoyangui/m-ui/tz"

	"github.com/op/go-logging"
	"golang.org/x/crypto/bcrypt"
	"gorm.io/gorm"
)

func hashPassword(plain string) (string, error) {
	b, err := bcrypt.GenerateFromPassword([]byte(plain), bcrypt.DefaultCost)
	return string(b), err
}

// startPanel 由 main 注入(web 包依赖 runner,反向直接引用会成环)。
var startPanel func(*Runner) error

// SetPanelStarter 注册面板启动函数,在 Run 时随数据面一起拉起。
func SetPanelStarter(f func(*Runner) error) { startPanel = f }

// Runner 持有数据面运行所需的一切。
type Runner struct {
	db           *gorm.DB
	core         *core.Core
	subSrv       *sub.Server
	jobs         *jobs.Scheduler
	notifier     *notify.Notifier
	monitor      *monitor.Monitor
	hub          *hub.Hub
	dbPath       string
	cert         certState
	certOpMu     sync.Mutex        // 串行化签发、切换来源与套用目标
	applied      map[string]string // 数据面当前生效的出站(tag → JSON),供上游热更新做差异
	appliedRaw   []byte            // 数据面当前生效的完整配置,渲染结果相同则不重启
	pendingStats []model.Stats     // 旧数据面关闭后暂时无法落库的流量,下次启动继续记账
	// 上一份"校验能过、真正启动却失败"的配置。同一份(或只有用户表不同的一份)在冷却期内不再拆掉
	// 正在服务的数据面去重试 —— 见 reloadAllLockedWithForce 里的说明。
	lastFailedRaw []byte
	lastFailedAt  time.Time
	lastFailedErr error
	// 上一次成功读到并应用的限速 / 设备数策略。数据库抖动时沿用它,后台重试,绝不为此停机。
	lastSpecs     map[string]core.UserLimitSpec
	lastGroups    map[string]core.GroupLimitSpec
	limitsPending atomic.Bool // applyLimits 读库失败,等 secureReloadLoop 重试
	applyLimitsMu sync.Mutex  // applyLimits 串行:ApplyLimits() / RulesNow() 不持 r.mu 就会调它,lastSpecs 不能裸着被并发读写
	mu            sync.Mutex  // 串行化重载,避免并发改动互相打断

	rules *rules.Engine // 限速规则判定器(只在主机跑)

	reloadMu sync.Mutex
	reload   ReloadStatus // 最近一次重载的结果:面板据此在页面上明说"已保存但没生效"
}

// ReloadStatus 最近一次数据面重载(启动、全量、热换出站、热更新用户)的结果。
// 重载都是保存之后异步做的,接口早已返回"已保存";失败只写日志的话操作者永远不知道线上还是旧配置。
type ReloadStatus struct {
	At    int64  `json:"at"`
	Op    string `json:"op"`
	OK    bool   `json:"ok"`
	Error string `json:"error,omitempty"`
}

// ReloadStatus 返回最近一次重载的结果(尚未重载过时 At 为 0)。
func (r *Runner) ReloadStatus() ReloadStatus {
	r.reloadMu.Lock()
	defer r.reloadMu.Unlock()
	return r.reload
}

// noteReload 记下一次重载的结果。
func (r *Runner) noteReload(op string, err error) {
	st := ReloadStatus{At: time.Now().Unix(), Op: op, OK: err == nil}
	if err != nil {
		st.Error = err.Error()
	}
	r.reloadMu.Lock()
	r.reload = st
	r.reloadMu.Unlock()
}

// Notifier 暴露通知器(面板发测试消息、登录告警)。
func (r *Runner) Notifier() *notify.Notifier { return r.notifier }

// Monitor 暴露巡检器(面板展示上游健康)。
func (r *Runner) Monitor() *monitor.Monitor { return r.monitor }

// CheckUpstream 对单个上游做健康检查(面板手动测试与定时巡检共用)。
// 数据面运行时经该上游真实请求测试 URL;未运行时 TCP 类做端口探测,QUIC 类无法离线测试。
func (r *Runner) CheckUpstream(up model.Upstream) (ok bool, delayMs int, method, errStr string) {
	testURL := r.setting("upstreamTestUrl")
	if testURL == "" {
		testURL = "http://www.gstatic.com/generate_204"
	}
	if r.CoreRunning() {
		res := r.TestUpstream(up.Name, testURL)
		if res.Error != "outbound not found" {
			return res.OK, int(res.Delay), "urltest", res.Error
		}
	}
	// 本机没有线路用这条上游(它就不在数据面里),或数据面没起来:起一个只含它的临时实例真实测一次,
	// 结果和经数据面测的同样可信,不再退化成"只探 TCP 端口"
	ob, err := render.OutboundJSON(up)
	if err != nil {
		return false, 0, "none", err.Error()
	}
	res := core.CheckOutboundIsolated(ob, testURL)
	return res.OK, int(res.Delay), "urltest", res.Error
}

// DB 暴露数据库句柄给面板层。
func (r *Runner) DB() *gorm.DB { return r.db }

func New(dbPath string) (*Runner, error) {
	db, err := database.Open(dbPath)
	if err != nil {
		return nil, err
	}
	// 升级后新增的协议需要新的凭据键:启动时为所有用户补全(复用既有口令与 UUID)
	if n, err := creds.EnsureAll(db); err != nil {
		logger.Warning("补全用户凭据失败: ", err)
	} else if n > 0 {
		logger.Info("已为 ", n, " 个用户补全新协议凭据")
	}
	r := &Runner{db: db, core: core.NewCore(), dbPath: dbPath}
	r.subSrv = r.newSubServer()
	r.notifier = notify.New(r.setting)
	r.rules = &rules.Engine{DB: db, Location: func() *time.Location { return tz.Location(r.setting("timezone")) }, Notify: r.notifyRule}
	r.jobs = jobs.New(jobs.Deps{
		DB:          db,
		Box:         func() *core.Box { return r.core.GetInstance() },
		ReloadUsers: r.ReloadUsers,
		IsNode:      r.IsNode,
		Setting:     r.setting,
		Notify:      func(text string) { r.notifier.Event("tgOnUserDisabled", text) },
		LocalRatio:  r.localRatio,
		Forget:      r.notifier.Forget,
		Rules:       r.RulesNow,
		ApplyLimits: func() { _ = r.applyLimits() },
	})
	r.monitor = monitor.New(monitor.Deps{
		UsedUpstreams: r.usedUpstreams,
		RemoteHealth:  r.remoteUpstreamHealth,
		SelfName:      r.LocalNodeName,
		SelfNodeId:    r.LocalNodeId,
		Alerting:      func() bool { return !r.IsNode() },
		DB:            db, Setting: r.setting, CoreRunning: r.CoreRunning, Check: r.CheckUpstream, Notify: r.notifier,
	})
	r.hub = hub.New(hub.Deps{
		DB: db, Setting: r.setting, IsNode: r.IsNode, Version: Version,
		Notify:         func(toggle, text string) { r.notifier.Event(toggle, text) },
		LocalIPs:       r.OnlineIPs,
		SetExternalIPs: r.SetExternalIPs,
		LocalGroups:    r.GroupState,
	})
	r.ensureAdmin()
	r.ensureLocalNode()
	return r, nil
}

// ensureLocalNode 保证 nodes 表里有一条"本机"记录(全新安装时创建),订阅入口与副机接入都以它为基准。
func (r *Runner) ensureLocalNode() {
	if r.IsNode() {
		return // 副机的 nodes 表由主机下发
	}
	var n int64
	r.db.Model(&model.Node{}).Where("is_local = ?", true).Count(&n)
	if n > 0 {
		return
	}
	r.db.Create(&model.Node{Name: "主机", Domain: r.setting("webDomain"), IsLocal: true, Enabled: true, Sort: 1})
	logger.Info("已创建本机服务器记录(服务器页可改名与域名)")
}

// Hub 暴露主副机同步器。
func (r *Runner) Hub() *hub.Hub { return r.hub }

// SetExternalIPs 下发其他机器上在线的 IP 给本机限制器(跨机设备数)。
func (r *Runner) SetExternalIPs(m map[string][]string) {
	if box := r.core.GetInstance(); box != nil {
		victims := box.Limiter().SetExternalIPs(m)
		if n := box.ConnTracker().CloseConnByDevices(victims); n > 0 {
			logger.Info("跨机设备记录恢复后,断开实际超额设备的 ", n, " 条连接")
		}
	}
}

// SetSettings 离线写入设置(首次启动前改端口/角色等)。
func SetSettings(dbPath string, kv map[string]string) error {
	db, err := database.Open(dbPath)
	if err != nil {
		return err
	}
	defer database.Close(db)
	for k, v := range kv {
		var existing model.Setting
		if err := db.Where("key = ?", k).First(&existing).Error; err == nil {
			if err := db.Model(&model.Setting{}).Where("key = ?", k).Update("value", v).Error; err != nil {
				return err
			}
		} else if err := db.Create(&model.Setting{Key: k, Value: v}).Error; err != nil {
			return err
		}
	}
	return nil
}

// ResetPassword 重置管理员密码;user 为空 = 库里现有的管理员(改过用户名也认得),库里一个管理员都没有时才创建 admin;
// pw 为空则随机生成。返回实际重置的用户名与明文密码。
// 以前硬编码 "admin":管理员把用户名改掉之后,菜单里重置密码会凭空多出一个 admin 账号。
func ResetPassword(dbPath, user, pw string) (string, string, error) {
	db, err := database.Open(dbPath)
	if err != nil {
		return "", "", err
	}
	defer database.Close(db)
	if pw == "" {
		pw = creds.Password(12)
	}
	if len(pw) < 6 {
		return "", "", fmt.Errorf("密码至少 6 位")
	}
	hash, err := hashPassword(pw)
	if err != nil {
		return "", "", err
	}
	var admin model.Admin
	q := db.Order("id asc")
	if user != "" {
		q = q.Where("username = ?", user)
	}
	if err := q.First(&admin).Error; err == nil {
		if err := db.Model(&model.Admin{}).Where("id = ?", admin.Id).Update("password", hash).Error; err != nil {
			return "", "", err
		}
		return admin.Username, pw, nil
	}
	if user == "" {
		user = "admin"
	}
	if err := db.Create(&model.Admin{Username: user, Password: hash}).Error; err != nil {
		return "", "", err
	}
	return user, pw, nil
}

// ensureAdmin 全新数据库没有管理员时创建 admin 并把随机密码打到日志(首次登录后请修改)。
func (r *Runner) ensureAdmin() {
	var n int64
	r.db.Model(&model.Admin{}).Count(&n)
	if n > 0 {
		return
	}
	// 与主流面板一致:首次安装默认 admin / admin,面板内醒目提示修改;安装脚本会打印出来
	hash, err := hashPassword("admin")
	if err != nil {
		logger.Error("生成初始密码失败: ", err)
		return
	}
	r.db.Create(&model.Admin{Username: "admin", Password: hash})
	r.setSetting("adminDefault", "true")
	logger.Info("==================================================")
	logger.Info("首次启动:已创建管理员 admin / admin(默认密码,请登录后立即修改)")
	logger.Info("==================================================")
}

// IsNode 报告本机是否以副机角色运行(设置 nodeMode)。
func (r *Runner) IsNode() bool { return strings.EqualFold(r.setting("nodeMode"), "true") }

// Onlines 返回最近统计周期内在线的用户/线路/上游。
func (r *Runner) Onlines() jobs.Onlines { return r.jobs.Onlines() }

// KickUser 断开某用户的全部连接,返回断开数。
// KickUser 只断本机:连接 + 整条会话,再清掉 limiter 里他的在线 IP。返回断开的连接数。
// 停用 / 删除 / 重置链接这些路径用它就够了:快照 5 秒内推到副机,副机热换用户表时自己关掉被移除 / 换了凭据的会话。
func (r *Runner) KickUser(name string) int {
	closed, _ := r.KickUserLocal(name)
	return closed
}

// KickUserLocal 本机踢线,返回 (断开的连接数, 关掉的会话数)。
func (r *Runner) KickUserLocal(name string) (closed, sessions int) {
	box := r.core.GetInstance()
	if box == nil {
		return 0, 0
	}
	closed = box.ConnTracker().CloseConnByUser(name)
	// 再把整条会话关掉(hysteria2 / tuic / anytls 只鉴权一次,只断流的话客户端马上再开一条):本人的和借用者的一起
	sessions = box.CloseUserSessions([]string{name, name + model.ShareSuffix})
	if sessions > 0 {
		logger.Info("踢线:关闭 ", name, " 的 ", sessions, " 条会话")
	}
	// 连接都断了,在线 IP 不用再等 60 秒空闲窗口才从"在线设备"里消失;设备重连会重新登记
	box.Limiter().Forget(name)
	return closed, sessions
}

// KickUserAll 面板 / 代理面板 / 外部 API 的「踢下线」:本机 + 所有副机。
// 只有主机派发;副机上调用等于本机踢线(hub.KickUser 在副机上直接返回空)。
func (r *Runner) KickUserAll(name string) hub.KickResult {
	closed, sessions := r.KickUserLocal(name)
	res := hub.KickResult{Closed: closed, Sessions: sessions, Servers: []hub.KickServer{{Local: true, Closed: closed, Sessions: sessions}}}
	if r.hub != nil && !r.IsNode() {
		remote := r.hub.KickUser(name)
		res.Closed += remote.Closed
		res.Sessions += remote.Sessions
		res.Failed = remote.Failed
		res.Servers = append(res.Servers, remote.Servers...)
	}
	return res
}

// KickShare 只断某用户临时共享凭据("名字#share")上的连接,本人的连接不动。
func (r *Runner) KickShare(name string) int {
	if box := r.core.GetInstance(); box != nil {
		n := box.ConnTracker().CloseConnByDataPlaneName(name + model.ShareSuffix)
		box.CloseUserSessions([]string{name + model.ShareSuffix}) // 精确匹配共享凭据的名字,本人的会话不动
		return n
	}
	return 0
}

// ConnCounts 返回每用户当前连接数。
func (r *Runner) ConnCounts() map[string]int {
	if box := r.core.GetInstance(); box != nil {
		return box.ConnTracker().ConnCountByUser()
	}
	return map[string]int{}
}

func (r *Runner) setting(key string) string {
	var v string
	r.db.Raw("SELECT value FROM settings WHERE key = ?", key).Scan(&v)
	return v
}

// nodeCert 读取本机证书材料(各机各签,路径固定)。
func (r *Runner) nodeCert() render.NodeCert {
	// 数据面证书优先用 certFile/keyFile(证书页签发的固定路径),
	// 未设置时回落到面板证书,兼容从旧库导入的配置。
	certPath, keyPath := r.setting("certFile"), r.setting("keyFile")
	if certPath == "" || keyPath == "" {
		certPath, keyPath = r.setting("webCertFile"), r.setting("webKeyFile")
	}
	return render.NodeCert{
		ServerName: r.setting("webDomain"),
		CertPath:   certPath,
		KeyPath:    keyPath,
	}
}

// Start 渲染配置并启动 sing-box,随后应用限速/设备数策略。
func (r *Runner) Start() (err error) {
	r.mu.Lock()
	defer r.mu.Unlock()
	defer func() { r.noteReload("启动", err) }()
	raw, err := render.BuildConfig(r.db, r.nodeCert())
	if err != nil {
		return fmt.Errorf("渲染配置: %w", err)
	}
	if r.lastSpecs == nil && r.lastGroups == nil {
		// 进程刚起、手上还没有任何一份策略:这时读库失败又先起数据面,Limiter 就是空表,
		// 设备数 / 限速 / 代理池全部放开,直到几秒后重试成功。数据面还没起、什么都不会断,
		// 先把策略读到手再起;读不到就让服务管理器按常规重启我们。
		specs, groups, err := r.readLimits()
		if err != nil {
			return fmt.Errorf("启动前读取限速/设备数策略: %w", err)
		}
		r.lastSpecs, r.lastGroups = specs, groups
	}
	if err := r.core.Start(raw); err != nil {
		return fmt.Errorf("启动 sing-box: %w", err)
	}
	r.restorePendingStats()
	r.applied, _ = outboundsOf(raw)
	r.appliedRaw = raw
	r.clearFailedConfig()
	if err := r.applyLimits(); err != nil {
		// 数据面已经起来了。策略读不到就先沿用上一份(applyLimits 里已经装回去了),后台重试;
		// 拿停机去换"策略确定",只会让所有用户为一次数据库抖动断网(0.6.10 就是这么改的)。
		r.markLimitsRetry(err)
	}
	r.applyPortHopping()
	r.applyLogLevel()
	return nil
}

// applyLogLevel 按设置调数据面日志级别:logEnabled=false 只留 panic(等于不记);开着时按 coreLogLevel,
// 默认 warn —— 逐条记连接的 info 一小时几万行,副机磁盘扛不住会拖慢所有连接,排障时再临时调到 info / debug。
// 级别是运行时可改的,不重启数据面、不断线。
func (r *Runner) applyLogLevel() {
	r.core.SetLogLevelName(r.coreLogLevel())
}

// coreLogLevel 数据面该记到哪一级。
func (r *Runner) coreLogLevel() string {
	if r.setting("logEnabled") == "false" {
		return "panic"
	}
	switch lv := strings.ToLower(strings.TrimSpace(r.setting("coreLogLevel"))); lv {
	case "debug", "info", "error":
		return lv
	default:
		return "warn"
	}
}

// SetLogEnabled 面板"日志"页开关:立即生效,不重启数据面。
func (r *Runner) SetLogEnabled(bool) { r.applyLogLevel() }

// InboundCount 数据面当前的入站数(健康检查用:线路都在库里,监听器却没起来就是不健康)。
func (r *Runner) InboundCount() int {
	if box := r.core.GetInstance(); box != nil {
		return len(box.Inbound().Inbounds())
	}
	return 0
}

// applyPortHopping 为本机部署的、开启端口跳跃的 hysteria2 线路应用 UDP 端口范围转发(Linux root 生效)。
func (r *Runner) applyPortHopping() {
	var lines []model.Line
	r.db.Where("enabled = ? AND protocol = ?", true, "hysteria2").Find(&lines)
	self := render.LocalNodeID(r.db)
	var rules []hop.Rule
	for _, l := range lines {
		if !render.LineOnNode(l, self) {
			continue
		}
		var o struct {
			PortHopping string `json:"port_hopping"`
		}
		if json.Unmarshal(l.Options, &o) != nil || o.PortHopping == "" {
			continue
		}
		a, b, err := hop.ParseRange(o.PortHopping)
		if err != nil {
			logger.Warning("线路 ", l.Name, " 端口跳跃范围无效: ", err)
			continue
		}
		rules = append(rules, hop.Rule{From: a, To: b, Port: l.Port})
	}
	if err := hop.Apply(rules); err != nil {
		logger.Warning("端口跳跃规则应用失败: ", err)
	} else if len(rules) > 0 {
		logger.Info("端口跳跃已应用 ", len(rules), " 条规则")
	}
}

// outboundsOf 从渲染好的配置里取出出站列表(tag → 规范化 JSON 文本)。
func outboundsOf(raw []byte) (map[string]string, error) {
	var cfg struct {
		Outbounds []json.RawMessage `json:"outbounds"`
	}
	if err := json.Unmarshal(raw, &cfg); err != nil {
		return nil, err
	}
	out := make(map[string]string, len(cfg.Outbounds))
	for _, ob := range cfg.Outbounds {
		var meta struct {
			Tag string `json:"tag"`
		}
		if json.Unmarshal(ob, &meta) == nil && meta.Tag != "" {
			out[meta.Tag] = string(ob)
		}
	}
	return out, nil
}

// ReloadUpstreams 只增删改有变化的出站,不重启数据面,现有用户连接不受影响。
// 适用于上游增删改(线路与路由未变)。任一步失败则回退到全量重载。
func (r *Runner) ReloadUpstreams() (err error) {
	r.mu.Lock()
	defer r.mu.Unlock()
	if !r.core.IsRunning() {
		return nil
	}
	defer func() { r.noteReload("热换出站", err) }()
	raw, err := render.BuildConfig(r.db, r.nodeCert())
	if err != nil {
		return fmt.Errorf("渲染配置: %w", err)
	}
	want, err := outboundsOf(raw)
	if err != nil {
		return err
	}
	if r.applied == nil {
		return r.reloadAllLocked(raw)
	}
	// 路由规则引用的出站集合变了(典型是上游改名):运行中的规则还指着旧标签,只换出站会让这些线路
	// 找不到出口而断流。这种情况必须整体重启,让规则和出站一起换。
	if r.appliedRaw != nil && !sameRuleOutbounds(r.appliedRaw, raw) {
		logger.Info("路由引用的出站变化(如上游改名),改为全量重载")
		return r.reloadAllLocked(raw)
	}
	changed := 0
	for tag, ob := range want {
		prev, existed := r.applied[tag]
		if existed && prev == ob {
			continue
		}
		if existed {
			if err := r.core.RemoveOutbound(tag); err != nil {
				logger.Warning("热移除出站 ", tag, " 失败,改为全量重载: ", err)
				return r.reloadAllLocked(raw)
			}
		}
		if err := r.core.AddOutbound([]byte(ob)); err != nil {
			logger.Warning("热添加出站 ", tag, " 失败,改为全量重载: ", err)
			return r.reloadAllLocked(raw)
		}
		changed++
	}
	for tag := range r.applied {
		if _, keep := want[tag]; keep {
			continue
		}
		if err := r.core.RemoveOutbound(tag); err != nil {
			logger.Warning("热移除出站 ", tag, " 失败,改为全量重载: ", err)
			return r.reloadAllLocked(raw)
		}
		changed++
	}
	r.applied = want
	logger.Info("上游已热更新(", changed, " 个出站变化,数据面未重启)")
	return nil
}

// ReloadUsers 只刷新用户相关状态:入站用户表就地热更新(不断开现有连接),
// 并重新下发限速/设备数策略。用于新增/禁用用户、改配额与限速等高频操作。
func (r *Runner) ReloadUsers() (err error) {
	r.mu.Lock()
	defer r.mu.Unlock()
	if !r.core.IsRunning() {
		return nil
	}
	defer func() {
		if err != nil {
			// 部分入站可能已经更新，不能把失败吞掉；持久待重试标记让
			// 后台按数据库最新用户表继续收敛，直到所有入站成功。
			if markerErr := r.setSetting("userReloadPending", "true"); markerErr != nil {
				logger.Error("记录用户热更新待重试状态失败: ", markerErr)
			}
		} else {
			if markerErr := r.setSetting("userReloadPending", ""); markerErr != nil {
				// 数据面已经处理,但清理标记失败;返回错误让调用方保留
				// 重试语义,不会把持久状态误报成已收敛。
				err = fmt.Errorf("清理用户热更新状态失败: %w", markerErr)
			}
		}
		r.noteReload("热更新用户", err)
	}()
	raw, err := render.BuildConfig(r.db, r.nodeCert())
	if err != nil {
		return fmt.Errorf("渲染配置: %w", err)
	}
	if err := r.reloadUsersLocked(raw); err != nil {
		return err
	}
	// 用户表已经是新的了:如果配置里除了用户没别的变化,记下这份配置,之后的全量重载比对不会再为它重启一次
	if r.appliedRaw != nil && onlyUsersDiffer(r.appliedRaw, raw) {
		r.appliedRaw = raw
	}
	return nil
}

func (r *Runner) userReloadLoop(stop <-chan struct{}) {
	ticker := time.NewTicker(5 * time.Second)
	defer ticker.Stop()
	for {
		select {
		case <-stop:
			return
		case <-ticker.C:
			if r.setting("userReloadPending") == "true" {
				if err := r.ReloadUsers(); err != nil {
					logger.Warning("重试用户热更新: ", err)
				}
			}
		}
	}
}

// ReloadUsersSecure 用于撤销凭据等不能继续接受旧用户表的变更。
// 重载失败时停止数据面并保留重试标记，绝不回滚成已撤销的凭据。
func (r *Runner) ReloadUsersSecure() (err error) {
	r.mu.Lock()
	defer r.mu.Unlock()
	defer func() { r.noteReload("撤销凭据", err) }()
	if markerErr := r.setSetting("secureReloadPending", "true"); markerErr != nil {
		return fmt.Errorf("记录凭据撤销待重试状态: %w", markerErr)
	}
	raw, err := render.BuildConfig(r.db, r.nodeCert())
	if err == nil {
		if r.core.IsRunning() {
			err = r.reloadUsersLocked(raw)
		} else {
			err = r.reloadAllLocked(raw)
		}
	}
	if err != nil {
		// 撤销凭据只需要保证"这个用户的旧凭据不再可用"。热更新有一处入站失败(往往是另一条根本
		// 起不来的线路),不是把整台机器停掉的理由 —— 0.6.10 在这里 Stop(),之后每 5 秒
		// "起旧配置 → 判失败 → 再停",主机在管理员修好之前一直不可用。
		// 标记留着,secureReloadLoop 会退避重试;运行中的入站上用户表已经更新到哪算哪。
		return fmt.Errorf("凭据撤销的热更新没有全部完成,数据面保持服务并在后台重试: %w", err)
	}
	if r.appliedRaw != nil && onlyUsersDiffer(r.appliedRaw, raw) {
		r.appliedRaw = raw
	}
	if markerErr := r.setSetting("secureReloadPending", ""); markerErr != nil {
		return fmt.Errorf("清理凭据撤销状态失败: %w", markerErr)
	}
	return nil
}

func (r *Runner) secureReloadLoop(stop <-chan struct{}) {
	ticker := time.NewTicker(5 * time.Second)
	defer ticker.Stop()
	var failures int
	var next time.Time // 连续失败时退避:5s、10s、20s … 最长 5 分钟,别每 5 秒刷一条一样的告警
	for {
		select {
		case <-stop:
			return
		case <-ticker.C:
			if r.limitsPending.Load() {
				r.mu.Lock()
				err := r.applyLimits()
				r.mu.Unlock()
				if err == nil {
					r.limitsPending.Store(false)
					logger.Info("限速/设备数策略已在重试后应用")
				}
			}
			if r.setting("secureReloadPending") != "true" {
				failures = 0
				continue
			}
			if time.Now().Before(next) {
				continue
			}
			if err := r.ReloadUsersSecure(); err != nil {
				failures++
				delay := 5 * time.Second << uint(min(failures, 6)) // 10s … 5m20s
				if delay > 5*time.Minute {
					delay = 5 * time.Minute
				}
				next = time.Now().Add(delay)
				logger.Warning("重试凭据撤销(", delay, " 后再试): ", err)
			} else {
				failures = 0
			}
		}
	}
}

// reloadUsersLocked 按给定配置热替换各入站的用户表(调用方持 mu,数据面在运行)。
func (r *Runner) reloadUsersLocked(raw []byte) error {
	var cfg struct {
		Inbounds []json.RawMessage `json:"inbounds"`
	}
	if err := json.Unmarshal(raw, &cfg); err != nil {
		return err
	}
	// 当前生效配置里的各入站(按 tag),用来判断不支持热换用户的入站到底变没变
	prevInbounds := map[string]json.RawMessage{}
	if r.appliedRaw != nil {
		var prev struct {
			Inbounds []json.RawMessage `json:"inbounds"`
		}
		if json.Unmarshal(r.appliedRaw, &prev) == nil {
			for _, ib := range prev.Inbounds {
				var m struct {
					Tag string `json:"tag"`
				}
				if json.Unmarshal(ib, &m) == nil && m.Tag != "" {
					prevInbounds[m.Tag] = ib
				}
			}
		}
	}
	box := r.core.GetInstance()
	keepAll := map[string]map[string]struct{}{} // 入站 → 仍然有效的用户名
	sessionsClosed := 0                         // 随换表关掉的整条会话数(被停用 / 换了凭据的用户)
	var firstErr error
	for _, inbound := range cfg.Inbounds {
		handled, closed, err := r.core.UpdateInboundUsers(inbound)
		if err != nil {
			logger.Warning("热更新入站用户失败: ", err)
			if firstErr == nil {
				firstErr = fmt.Errorf("热更新入站用户: %w", err)
			}
			continue
		}
		sessionsClosed += closed
		if box == nil {
			continue
		}
		var meta struct {
			Tag   string `json:"tag"`
			Users []struct {
				Name     string `json:"name"`
				Username string `json:"username"`
			} `json:"users"`
		}
		if err := json.Unmarshal(inbound, &meta); err != nil {
			if firstErr == nil {
				firstErr = fmt.Errorf("解析入站用户配置: %w", err)
			}
			continue
		}
		if meta.Tag == "" {
			if firstErr == nil {
				firstErr = errors.New("入站用户配置缺少 tag")
			}
			continue
		}
		if !handled {
			// 该协议不支持原地换用户表(socks/http/mixed、单用户 shadowsocks):只有这个入站本身变了才重建
			// (重建会断开它上面的全部连接);别人改了用户表和它无关,不能每次都把它重启一遍
			if prev, ok := prevInbounds[meta.Tag]; ok && bytes.Equal(prev, inbound) {
				continue
			}
			prevDef, wasRunning := prevInbounds[meta.Tag]
			if err := r.core.RemoveInbound(meta.Tag); err != nil && err != os.ErrInvalid {
				logger.Warning("重建入站 ", meta.Tag, " 失败(移除): ", err)
				if firstErr == nil && wasRunning {
					firstErr = fmt.Errorf("重建入站 %s(移除): %w", meta.Tag, err)
				}
				continue
			}
			box.ConnTracker().CloseConnByInbound(meta.Tag)
			if err := r.core.AddInbound(inbound); err != nil {
				// 这个入站本来就不在运行中的配置里(上一次整机重载时它没起来、已回滚),这里补建
				// 又失败,是意料之中:它上面没有任何用户在服务,不能让它把别的入站的用户热更新
				// 一起判成失败 —— 那正是 0.6.10 里"撤销凭据失败 → 停掉整台机器"的起点。
				if !wasRunning {
					logger.Warning("入站 ", meta.Tag, " 不在运行中的配置里且补建失败(它本来就没在服务),跳过: ", err)
					continue
				}
				logger.Warning("重建入站 ", meta.Tag, " 失败(添加): ", err)
				if firstErr == nil {
					firstErr = fmt.Errorf("重建入站 %s(添加): %w", meta.Tag, err)
				}
				// 正在服务的旧入站已经被拆掉了,新的又加不上:把旧定义补回去,至少让这条线路照旧服务,
				// 不要让它在整个冷却期里停摆。补不回去才是真的没了。
				if err2 := r.core.AddInbound(prevDef); err2 != nil {
					logger.Warning("重建入站 ", meta.Tag, " 失败后恢复旧定义也失败,这条线路暂停服务: ", err2)
				} else {
					logger.Warning("重建入站 ", meta.Tag, " 失败,已恢复旧定义继续服务")
				}
			}
			continue
		}
		// 记下这个入站还有哪些用户,最后一次性断开不在其中的连接(禁用用户即时下线)
		keep := make(map[string]struct{}, len(meta.Users))
		for _, u := range meta.Users {
			if u.Name != "" {
				keep[u.Name] = struct{}{}
			}
			if u.Username != "" {
				keep[u.Username] = struct{}{}
			}
		}
		keepAll[meta.Tag] = keep
	}
	if box != nil && len(keepAll) > 0 {
		box.ConnTracker().CloseConnsNotIn(keepAll) // 一次扫描,锁只拿一次
	}
	if sessionsClosed > 0 {
		// 数据面日志默认只到 warn,这一句必须从面板日志出去:线上出事(比如每次推送都把所有人踢掉)只能靠它看出来
		logger.Info("热更新用户:断开 ", sessionsClosed, " 条会话(被停用 / 撤销凭据的用户)")
	}
	return errors.Join(firstErr, r.applyLimits())
}

// ReloadAll 重建整个数据面(线路增删、端口/协议变更、路由或证书变化时使用)。
func (r *Runner) ReloadAll() (err error) {
	r.mu.Lock()
	defer r.mu.Unlock()
	defer func() { r.noteReload("全量重载", err) }()
	raw, err := render.BuildConfig(r.db, r.nodeCert())
	if err != nil {
		return fmt.Errorf("渲染配置: %w", err)
	}
	return r.reloadAllLocked(raw)
}

// ReloadAllForce 无条件重启数据面(证书文件内容变了但配置文本没变时用)。
func (r *Runner) ReloadAllForce() (err error) {
	r.mu.Lock()
	defer r.mu.Unlock()
	defer func() { r.noteReload("全量重载", err) }()
	raw, err := render.BuildConfig(r.db, r.nodeCert())
	if err != nil {
		return fmt.Errorf("渲染配置: %w", err)
	}
	return r.reloadAllLockedForce(raw)
}

// reloadAllLocked 用给定配置重启数据面(调用方持 mu)。渲染结果与当前生效配置完全相同则不重启。
func (r *Runner) reloadAllLocked(raw []byte) error {
	return r.reloadAllLockedWithForce(raw, false)
}

func (r *Runner) reloadAllLockedForce(raw []byte) error {
	return r.reloadAllLockedWithForce(raw, true)
}

// failedConfigRetryEvery 同一份起不来的配置,多久才允许再拆一次正在服务的数据面去重试。
//
// 副机上一份新配置"校验能过、真正启动失败"(典型:新线路的端口在这台机上被别的进程占着)时,
// 主机每 5 秒会重推同一份修订。没有这道闸,每一轮都是"停掉正在服务的旧数据面 → 新配置起不来 →
// 再起旧配置":这台机器上的所有用户每 5 秒断一次线,直到管理员发现并修好 —— 0.4.16 修过的
// "每次推送都重启数据面"换了个形式回来。冷却期内同一份配置只热更新用户表,不动数据面。
const failedConfigRetryEvery = 10 * time.Minute

// sameFailedConfig 这份配置是不是上次刚起不来的那份(或者只有用户表不一样的那份)且还在冷却期内。
// "只有用户表不一样"也算:管理员没改线路、只是停用了个用户,推下来的新修订照样起不来。
func (r *Runner) sameFailedConfig(raw []byte) bool {
	if r.lastFailedRaw == nil || time.Since(r.lastFailedAt) >= failedConfigRetryEvery {
		return false
	}
	return bytes.Equal(r.lastFailedRaw, raw) || onlyUsersDiffer(r.lastFailedRaw, raw)
}

func (r *Runner) noteFailedConfig(raw []byte, err error) {
	r.lastFailedRaw, r.lastFailedAt, r.lastFailedErr = raw, time.Now(), err
}

func (r *Runner) clearFailedConfig() {
	r.lastFailedRaw, r.lastFailedAt, r.lastFailedErr = nil, time.Time{}, nil
}

// markLimitsRetry 限速 / 设备数策略这次没读到:记下来让 secureReloadLoop 隔几秒再读,数据面照常服务。
func (r *Runner) markLimitsRetry(err error) {
	r.limitsPending.Store(true)
	logger.Warning("限速/设备数策略暂时没读到,沿用上一份并在后台重试: ", err)
}

func (r *Runner) reloadAllLockedWithForce(raw []byte, force bool) error {
	if !force && r.core.IsRunning() && r.appliedRaw != nil && bytes.Equal(r.appliedRaw, raw) {
		if err := r.applyLimits(); err != nil {
			r.markLimitsRetry(err)
			return err
		}
		r.applyPortHopping() // 端口跳跃等面板侧参数不进 sing-box 配置,即使无需重启也要同步
		logger.Info("配置无变化,数据面无需重启")
		return nil
	}
	// 只有用户表变了(线路、上游、证书、路由都没动):热替换用户即可,绝不为此重启数据面断掉所有人。
	// 副机整表替换后判断"线路是否变化"曾经误报过,这里再兜一层,不依赖调用方判断得对不对。
	if !force && r.core.IsRunning() && r.appliedRaw != nil && onlyUsersDiffer(r.appliedRaw, raw) {
		if err := r.reloadUsersLocked(raw); err == nil {
			r.appliedRaw = raw
			r.applyPortHopping()
			logger.Info("只有用户变化,已热更新,数据面未重启")
			return nil
		} else {
			logger.Warning("用户热更新失败,改为重启数据面: ", err)
		}
	}
	// 同一份刚刚起不来、已经回滚过的配置,冷却期内不再拆正在服务的数据面。
	// 用户表照样热更新到运行中的旧配置上 —— 停用、到期、换凭据在这台机上不能被冻结;
	// 线路等管理员改过(渲染结果就不一样了)、或者冷却期过了,再试一次真正的重启。
	if !force && r.core.IsRunning() && r.sameFailedConfig(raw) {
		if err := r.reloadUsersLocked(raw); err != nil {
			logger.Warning("同一份起不来的配置冷却期内只热更新用户,部分入站没更新到: ", err)
		}
		return fmt.Errorf("这份配置 %s 前启动失败并已回滚,冷却期内不再重启数据面(用户表已热更新到运行中的旧配置);改好线路或 %s 后会再试。原因: %w",
			time.Since(r.lastFailedAt).Round(time.Second), failedConfigRetryEvery, r.lastFailedErr)
	}
	// 先干跑校验新配置;不通过就让旧数据面继续服务——绝不为一条坏配置断掉所有用户。
	if r.core.IsRunning() {
		if err := core.ValidateConfig(raw); err != nil {
			return fmt.Errorf("新配置未通过校验,数据面保持原状: %w", err)
		}
	}
	prev := r.appliedRaw // 新配置起不来时用它把服务拉回来(端口被别的进程抢走之类)
	// StatsTracker belongs to the Box. Flush before Stop so bytes collected
	// since the last scheduler tick survive a full data-plane replacement.
	if r.jobs != nil && !r.jobs.FlushStats() {
		// 一次 SQLite 抖动不能让改线路、续证书全都失败:计数器没被消费,下面停掉旧数据面后还会再试,再不行就带到新数据面补记
		logger.Warning("重载前统计落库失败,这段流量会在新数据面起来后补记")
	}
	oldBox := r.core.GetInstance()
	r.core.Stop()
	// Closing the old box stops new accepts. Drain it once more afterwards so
	// bytes that arrived during the preflight transaction are retained. A
	// second pass covers any final read/write callbacks fired by Close.
	if oldBox != nil && r.jobs != nil {
		ok1 := r.jobs.FlushStatsBox(oldBox)
		ok2 := r.jobs.FlushStatsBox(oldBox)
		if !ok1 || !ok2 {
			r.pendingStats = append(r.pendingStats, *oldBox.StatsTracker().SnapshotStats()...)
		}
	}
	if err := r.core.Start(raw); err != nil {
		r.applied, r.appliedRaw = nil, nil
		r.noteFailedConfig(raw, err)
		if prev != nil {
			if err2 := r.core.Start(prev); err2 == nil {
				r.restorePendingStats()
				r.applied, r.appliedRaw = outboundsOfSafe(prev), prev
				if limErr := r.applyLimits(); limErr != nil {
					// 旧配置起来了就让它服务:策略没读到只是"沿用上一份",不是再停一次机的理由
					r.markLimitsRetry(limErr)
				}
				// 旧配置带的是它当初生效时的用户表。此后被停用的用户、被换掉的旧凭据不能跟着回来 ——
				// 把当前用户表热更新到回滚后的入站上(起不来的那条入站会被跳过)。
				if uerr := r.reloadUsersLocked(raw); uerr != nil {
					logger.Warning("回滚后热更新用户表有入站没更新到: ", uerr)
				}
				r.applyPortHopping()
				r.applyLogLevel()
				logger.Warning("新配置启动失败,已回滚到上一份可用配置: ", err)
				return fmt.Errorf("新配置启动失败,已回滚到上一份配置: %w", err)
			}
			logger.Error("新配置启动失败且回滚也失败: ", err)
		}
		return fmt.Errorf("重启 sing-box: %w", err)
	}
	r.clearFailedConfig()
	r.restorePendingStats()
	r.applied, _ = outboundsOf(raw)
	r.appliedRaw = raw
	if err := r.applyLimits(); err != nil {
		// 新配置已经在服务了,策略没读到就沿用上一份、后台重试。0.6.10 在这里停掉新实例再回滚,
		// 回滚后再失败就保持停机 —— 一次数据库抖动就能让整台机器断网。
		r.markLimitsRetry(err)
	}
	r.applyPortHopping()
	r.applyLogLevel()
	logger.Info("数据面已重载")
	return nil
}

// ruleOutboundsOf 取出配置里路由规则(含 final)引用的出站标签集合。
func ruleOutboundsOf(raw []byte) map[string]bool {
	var cfg struct {
		Route struct {
			Rules []struct {
				Outbound string `json:"outbound"`
			} `json:"rules"`
			Final string `json:"final"`
		} `json:"route"`
	}
	out := map[string]bool{}
	if json.Unmarshal(raw, &cfg) != nil {
		return out
	}
	for _, rule := range cfg.Route.Rules {
		if rule.Outbound != "" {
			out[rule.Outbound] = true
		}
	}
	if cfg.Route.Final != "" {
		out[cfg.Route.Final] = true
	}
	return out
}

// sameRuleOutbounds 两份配置的路由是否引用同一组出站标签;不同就不能只热换出站。
func sameRuleOutbounds(prev, next []byte) bool {
	a, b := ruleOutboundsOf(prev), ruleOutboundsOf(next)
	if len(a) != len(b) {
		return false
	}
	for k := range a {
		if !b[k] {
			return false
		}
	}
	return true
}

// onlyUsersDiffer 两份 sing-box 配置是否只有入站用户表不同(入站的 users 字段抹掉后逐字节相同)。
// 解析不了就按"不止用户变了"处理,让调用方走稳妥的重启路径。
func onlyUsersDiffer(prev, next []byte) bool {
	strip := func(raw []byte) []byte {
		var cfg map[string]json.RawMessage
		if json.Unmarshal(raw, &cfg) != nil {
			return nil
		}
		var inbounds []map[string]json.RawMessage
		if in, ok := cfg["inbounds"]; ok {
			if json.Unmarshal(in, &inbounds) != nil {
				return nil
			}
			for _, ib := range inbounds {
				delete(ib, "users")
			}
			b, err := json.Marshal(inbounds)
			if err != nil {
				return nil
			}
			cfg["inbounds"] = b
		}
		out, err := json.Marshal(cfg) // map 序列化按键排序,两边一致
		if err != nil {
			return nil
		}
		return out
	}
	a, b := strip(prev), strip(next)
	return a != nil && b != nil && bytes.Equal(a, b) && !bytes.Equal(prev, next)
}

// outboundsOfSafe 与 outboundsOf 相同,解析失败时返回 nil(回滚路径上不该再报错)。
func outboundsOfSafe(raw []byte) map[string]string {
	m, err := outboundsOf(raw)
	if err != nil {
		return nil
	}
	return m
}

// CoreRunning 报告内嵌 sing-box 是否在运行。
func (r *Runner) CoreRunning() bool { return r.core.IsRunning() }

// Uptime 返回数据面已运行秒数。
func (r *Runner) Uptime() uint32 {
	if box := r.core.GetInstance(); box != nil {
		return box.Uptime()
	}
	return 0
}

// OnlineIPs 返回某用户当前在线的源 IP(同时在线设备)。
func (r *Runner) OnlineIPs(user string) []string {
	if box := r.core.GetInstance(); box != nil {
		return box.Limiter().ActiveIPs(user)
	}
	return nil
}

// OnlineIPsAll 本机上 用户 → 活跃源 IP(一次锁拿全量,面板列表用)。
func (r *Runner) OnlineIPsAll() map[string][]string {
	if box := r.core.GetInstance(); box != nil {
		return box.Limiter().ActiveIPsAll()
	}
	return map[string][]string{}
}

// RecentConns 本机最近的入站连接(源 IP × 线路聚合),概览诊断卡与副机报告用。
func (r *Runner) RecentConns(limit int) []hub.RecentConn {
	box := r.core.GetInstance()
	if box == nil {
		return nil
	}
	list := box.ConnTracker().Recent(limit)
	out := make([]hub.RecentConn, 0, len(list))
	for _, c := range list {
		out = append(out, hub.RecentConn{IP: c.IP, User: c.User, Line: c.Inbound, Protocol: c.Protocol, Count: c.Count, Ts: c.Last})
	}
	return out
}

// OnlineIPLines 本机上 用户 → 源 IP → 线路名。
func (r *Runner) OnlineIPLines() map[string]map[string][]string {
	if box := r.core.GetInstance(); box != nil {
		return box.ConnTracker().IPLinesByUser()
	}
	return nil
}

// TestUpstream 通过运行中的数据面,经指定上游真实发一次 HTTP 请求,返回延迟或错误。
// 这是最可信的健康检查:连 WARP 本地代理是否真的通都能测出来。
func (r *Runner) TestUpstream(name, testURL string) core.CheckOutboundResult {
	// 先等进行中的重载结束,保证"刚保存就测试"测的是新配置而不是撞上重启窗口。
	r.mu.Lock()
	running := r.core.IsRunning()
	ctx := r.core.GetCtx()
	r.mu.Unlock()
	if !running {
		return core.CheckOutboundResult{Error: "数据面未运行"}
	}
	return core.CheckOutbound(ctx, name, testURL)
}

// NodeCert 暴露本机数据面证书材料(面板保存前的干跑校验需要)。
func (r *Runner) NodeCert() render.NodeCert { return r.nodeCert() }

// applyLimits 把用户表里的限速与设备数策略下发给数据面。
func (r *Runner) applyLimits() error {
	r.applyLimitsMu.Lock()
	defer r.applyLimitsMu.Unlock()
	box := r.core.GetInstance()
	if box == nil {
		return nil
	}
	specs, groups, err := r.readLimits()
	if err != nil {
		// 数据库抖动。不能把 Limiter 留成空的(那等于所有限额都放开),也不能为此停机:
		// 把上一次成功读到的那份装回去,让调用方记下待重试。数据面刚重启时 Limiter 是全新的,
		// 没有这一步,重试成功之前设备数 / 限速就是空的。
		if r.lastSpecs != nil || r.lastGroups != nil {
			box.Limiter().SetGroups(r.lastGroups)
			box.Limiter().SetLimits(r.lastSpecs)
			box.Limiter().ReconcileDevices()
		}
		return err
	}
	r.lastSpecs, r.lastGroups = specs, groups
	box.Limiter().SetGroups(groups)
	box.Limiter().SetLimits(specs)
	// 重新算一遍"并集超限时哪些设备不再接受新登记"。它**不会**断开任何已连接的设备:
	// 你定过的语义是"池满只拒新设备,已连接的不动",0.6.10 曾按 IP 字典序踢掉在线设备,一个连了
	// 几小时、完全合规的付费用户会因为 IP 排得靠后被断网。
	box.Limiter().ReconcileDevices()
	if len(specs) > 0 || len(groups) > 0 {
		logger.Info("已应用 ", len(specs), " 个用户的限速/设备数策略,", len(groups), " 个代理池")
	}
	return nil
}

// readLimits 从库里读出限速 / 设备数策略。三张表任一读失败都整体报错,不拿半份策略去覆盖。
func (r *Runner) readLimits() (map[string]core.UserLimitSpec, map[string]core.GroupLimitSpec, error) {
	var users []model.User
	if err := r.db.Find(&users).Error; err != nil {
		return nil, nil, fmt.Errorf("读取用户限速策略: %w", err)
	}
	var resellers []model.Reseller
	if err := r.db.Find(&resellers).Error; err != nil {
		// 空切片会被 SetGroups 清空代理池,反而把一次短暂的数据库抖动变成限速绕过 —— 所以整体报错
		return nil, nil, fmt.Errorf("读取代理限速策略: %w", err)
	}
	// 规则限速:生效中的状态叠加到用户自己的限速上(只升不降 / 覆盖,多条取最严);
	// 到期的不算,主机失联时副机也能按到期时间自行放开
	var states []model.LimitState
	if err := r.db.Find(&states).Error; err != nil {
		return nil, nil, fmt.Errorf("读取限速规则状态: %w", err)
	}
	specs, groups := limitSpecs(users, resellers, rules.Active(states, time.Now().Unix()))
	return specs, groups, nil
}

// restorePendingStats moves traffic that could not be committed while an old
// data plane was closing into the newly started tracker. It is deliberately
// retried on the next successful start if a reload failed before a box was
// available.
func (r *Runner) restorePendingStats() {
	if len(r.pendingStats) == 0 {
		return
	}
	box := r.core.GetInstance()
	if box == nil {
		return
	}
	box.StatsTracker().RestoreStats(r.pendingStats)
	r.pendingStats = nil
}

// limitSpecs 把用户表、代理池与生效中的规则限速算成数据面要的策略。
// 代理池:设备池跨机并集判定,带宽池每台服务器各一份(带宽是单机物理量,和用户限速同一逻辑)。
func limitSpecs(users []model.User, resellers []model.Reseller, states []model.LimitState) (map[string]core.UserLimitSpec, map[string]core.GroupLimitSpec) {
	groups := map[string]core.GroupLimitSpec{}
	for _, rs := range resellers {
		if rs.DeviceLimit > 0 || rs.SpeedUp > 0 || rs.SpeedDown > 0 {
			groups[model.ResellerGroup(rs.Id)] = core.GroupLimitSpec{UpMbps: rs.SpeedUp, DownMbps: rs.SpeedDown, DeviceLimit: rs.DeviceLimit}
		}
	}
	statesBy := map[string][]model.LimitState{}
	for _, st := range states {
		statesBy[st.UserName] = append(statesBy[st.UserName], st)
	}
	specs := make(map[string]core.UserLimitSpec, len(users))
	for _, u := range users {
		up, down := rules.Effective(u.SpeedUp, u.SpeedDown, statesBy[u.Name])
		spec := core.UserLimitSpec{UpMbps: up, DownMbps: down, DeviceLimit: u.DeviceLimit}
		if u.ResellerId > 0 {
			if g := model.ResellerGroup(u.ResellerId); groups[g].DeviceLimit > 0 || groups[g].UpMbps > 0 || groups[g].DownMbps > 0 {
				spec.Group = g
			}
		}
		if spec.UpMbps == 0 && spec.DownMbps == 0 && spec.DeviceLimit == 0 && spec.Group == "" {
			continue
		}
		specs[u.Name] = spec
	}
	return specs, groups
}

// RulesNow 立刻判一轮限速规则(规则增删改后不用等下一轮统计),状态变了就重新下发限速;
// 快照里带着状态表,下一轮同步自然推给副机。
func (r *Runner) RulesNow() {
	if r.rules == nil || r.IsNode() {
		return
	}
	changed, err := r.rules.Tick(time.Now())
	if err != nil {
		logger.Warning("限速规则判定失败: ", err)
		return
	}
	if changed {
		r.applyLimits()
	}
}

// ApplyLimits 重新下发限速(副机按到期时间解除规则限速时用)。
func (r *Runner) ApplyLimits() { r.applyLimits() }

// notifyRule 突发限速触发的通知:设置里明确打开才发(默认关,免得高峰期刷屏)。
func (r *Runner) notifyRule(text string) {
	if strings.EqualFold(r.setting("tgOnRuleLimit"), "true") {
		r.notifier.Event("", text)
	}
}

// usedUpstreams 本机线路真正用到的上游 id:启用的线路 ∩ 部署在本机 ∩ 指定了上游。
// 巡检据此只测本机用得上的那些,不部署线路的主机一条都不测。
func (r *Runner) usedUpstreams() map[uint]bool {
	self := render.LocalNodeID(r.db)
	var lines []model.Line
	r.db.Select("id, upstream_id, node_ids").Where("enabled = ? AND upstream_id > 0", true).Find(&lines)
	out := map[uint]bool{}
	for _, l := range lines {
		if render.LineOnNode(l, self) {
			out[l.UpstreamId] = true
		}
	}
	return out
}

// remoteUpstreamHealth 各副机上报的上游巡检结果,转成巡检器的形式(带服务器名)。
func (r *Runner) remoteUpstreamHealth() []monitor.NodeHealth {
	if r.hub == nil { // 巡检器比 Hub 先建,理论上跑不到这里,留个兜底
		return nil
	}
	all := r.hub.UpstreamHealthAll()
	if len(all) == 0 {
		return nil
	}
	names := map[uint]string{}
	var nodes []model.Node
	r.db.Select("id, name").Find(&nodes)
	for _, n := range nodes {
		names[n.Id] = n.Name
	}
	var out []monitor.NodeHealth
	for id, list := range all {
		for _, h := range list {
			out = append(out, monitor.NodeHealth{NodeId: id, NodeName: names[id], Id: h.Id, Name: h.Name,
				OK: h.OK, DelayMs: h.DelayMs, Method: h.Method, Error: h.Error, CheckedAt: h.CheckedAt, Fails: h.Fails})
		}
	}
	return out
}

// LocalNodeName 本机在服务器列表里的名字(巡检告警里点名用)。
func (r *Runner) LocalNodeName() string {
	var n model.Node
	if r.db.Where("is_local = ?", true).First(&n).Error != nil {
		return ""
	}
	return n.Name
}

// LocalNodeId 本机在 nodes 表里的 id。
func (r *Runner) LocalNodeId() uint { return render.LocalNodeID(r.db) }

// UpstreamHealthLocal 本机巡检结果(副机随报告上报给主机)。
func (r *Runner) UpstreamHealthLocal() []monitor.UpstreamHealth { return r.monitor.Results() }

// GroupState 各代理池在本机的状态(在线设备数、设备池满被拒次数),供 Hub 汇总给面板。
func (r *Runner) GroupState() map[string]hub.GroupState {
	out := map[string]hub.GroupState{}
	if box := r.core.GetInstance(); box != nil {
		for g, s := range box.Limiter().GroupState() {
			out[g] = hub.GroupState{Devices: s.Devices, Rejects: s.Rejects}
		}
	}
	return out
}

func (r *Runner) Stop() {
	r.mu.Lock()
	defer r.mu.Unlock()
	if err := r.core.Stop(); err != nil {
		logger.Warning("停止 sing-box: ", err)
	}
}

// publicIPLoop 探测并记录本机公网 IP(设置 publicIp):没配域名时订阅地址与节点地址用它兜底。
func (r *Runner) publicIPLoop(stop <-chan struct{}) {
	probe := func() {
		ctx, cancel := context.WithTimeout(context.Background(), 15*time.Second)
		defer cancel()
		ip := acme.PublicIP(ctx)
		if ip == "" {
			return
		}
		if ip != r.setting("publicIp") {
			r.setSetting("publicIp", ip)
			logger.Info("本机公网 IP: ", ip)
		}
		// 本机服务器记录也同步(订阅入口用)
		r.db.Model(&model.Node{}).Where("is_local = ? AND public_ip != ?", true, ip).Update("public_ip", ip)
	}
	probe()
	t := time.NewTicker(time.Hour)
	defer t.Stop()
	for {
		select {
		case <-t.C:
			probe()
		case <-stop:
			return
		}
	}
}

// localRatio 本机服务器记录的流量倍率(无记录或 ≤0 视为 1)。
func (r *Runner) localRatio() float64 {
	var n model.Node
	if err := r.db.Where("is_local = ?", true).First(&n).Error; err != nil || n.Ratio <= 0 {
		return 1
	}
	return n.Ratio
}

// PublicHost 返回对外地址:订阅域名 → 面板域名 → 本机公网 IP。
func (r *Runner) PublicHost() string {
	for _, k := range []string{"subDomain", "webDomain", "publicIp"} {
		if v := strings.TrimSpace(r.setting(k)); v != "" {
			return v
		}
	}
	return ""
}

// checkpointLoop 定期把 WAL 并回主库,使运行中的 .db 文件随时可安全复制备份。
func (r *Runner) checkpointLoop(stop <-chan struct{}) {
	ticker := time.NewTicker(10 * time.Minute)
	defer ticker.Stop()
	for {
		select {
		case <-ticker.C:
			if err := database.CheckpointPassive(r.db); err != nil { // 定时的不许阻塞;备份和停机才用 TRUNCATE
				logger.Warning("WAL 检查点失败: ", err)
			}
		case <-stop:
			return
		}
	}
}

// keepRunning 跑一个常驻循环:它 panic 了就记日志,歇一分钟再拉起来,
// 直到 stop 关闭。只兜一次的话,证书续期这种循环崩一回就到重启前都不再工作。
// loopRestartDelay 循环崩溃后的重启间隔,测试里会调短。
var loopRestartDelay = time.Minute

func keepRunning(name string, fn func(<-chan struct{}), stop <-chan struct{}) {
	for {
		func() {
			defer func() {
				if v := recover(); v != nil {
					logger.Warning("后台任务 ", name, " 异常,一分钟后重启: ", v, " | ", string(debug.Stack()))
				}
			}()
			fn(stop)
		}()
		select {
		case <-stop:
			return
		case <-time.After(loopRestartDelay):
		}
	}
}

// Run 启动数据面并阻塞直到收到终止信号(SIGHUP 触发重载)。
func Run(dbPath string) error {
	logger.InitLogger(logging.INFO)
	// 有待还原的备份(面板上传后重启到这里)先原子替换数据库与证书
	if applied, err := backup.ApplyPending(dbPath); err != nil {
		// 备份包已改名 .failed。注意错误文本里会说明数据库有没有换:证书阶段出错时库**已经**是备份里的了。
		logger.Error("应用待还原备份时出错(备份包已改名为 .failed): ", err)
	} else if applied {
		logger.Info("已从备份还原数据库与证书")
	}
	r, err := New(dbPath)
	if err != nil {
		return err
	}
	// core 启动失败(如证书未就绪)不阻塞面板与订阅:
	// 面板必须先可用,操作者才能在面板里解决问题。
	if err := r.Start(); err != nil {
		logger.Error("数据面启动失败(面板与订阅继续运行): ", err)
	}
	if err := r.subSrv.Start(); err != nil {
		return fmt.Errorf("启动订阅服务: %w", err)
	}
	if startPanel != nil {
		if err := startPanel(r); err != nil {
			return fmt.Errorf("启动面板: %w", err)
		}
	}
	logger.Info("m-ui 数据面已启动")

	r.jobs.Start()
	defer r.jobs.Stop()
	r.monitor.Start()
	defer r.monitor.Stop()
	r.hub.Start()
	defer r.hub.Stop()

	stopCheckpoint := make(chan struct{})
	// 常驻循环各自兜住 panic:一个后台任务崩了不该把面板和数据面一起带走
	for _, l := range []struct {
		name string
		fn   func(<-chan struct{})
	}{
		{"库检查点", r.checkpointLoop}, {"证书续期", r.certLoop}, {"定时备份", r.backupLoop},
		{"公网 IP 探测", r.publicIPLoop}, {"外部订阅刷新", r.extLoop}, {"凭据撤销重试", r.secureReloadLoop}, {"用户热更新重试", r.userReloadLoop},
	} {
		go keepRunning(l.name, l.fn, stopCheckpoint)
	}
	defer close(stopCheckpoint)

	sigCh := make(chan os.Signal, 1)
	signal.Notify(sigCh, syscall.SIGHUP, syscall.SIGTERM, syscall.SIGINT)
	for sig := range sigCh {
		if sig == syscall.SIGHUP {
			logger.Info("收到 SIGHUP,重载配置")
			if err := r.ReloadAllForce(); err != nil { // 和面板走同一把锁,不和进行中的热更新互相打断
				logger.Error("重载失败: ", err)
			}
			continue
		}
		logger.Info("收到终止信号,正在停止")
		r.Stop()
		if err := database.Checkpoint(r.db); err != nil {
			logger.Warning("WAL 检查点失败: ", err)
		}
		return nil
	}
	return nil
}

// newSubServer 建订阅服务,并接上临时共享回调:用户在订阅页生成/取消后立即生效。
// 取消时先热更新撤下共享凭据,再断开该用户的连接——否则借用者已经建立的连接还能接着用。
func (r *Runner) newSubServer() *sub.Server {
	s := sub.NewServer(r.db)
	s.OnShareChange = func(name string, kick bool) {
		go func() {
			if err := r.ReloadUsers(); err != nil {
				logger.Warning("临时共享热更新失败: ", err)
				return
			}
			if !kick {
				logger.Info("用户 ", name, " 的临时共享凭据已生效")
				return
			}
			// 只断借用者(名字#share)的连接:取消共享时 ReloadUsers 已把这份凭据撤下并按用户表断掉不在表里的连接,
			// 重新生成时旧凭据的连接还挂着,这里补一刀;本人自己的连接从头到尾不动
			logger.Info("用户 ", name, " 的旧共享凭据已作废,断开 ", r.KickShare(name), " 条共享连接")
		}()
	}
	return s
}
