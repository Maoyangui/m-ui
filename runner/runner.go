// Package runner 启动 m-ui 数据面:渲染配置 → 拉起内嵌 sing-box → 应用用户限速/设备数策略。
package runner

import (
	"bytes"
	"context"
	"encoding/json"
	"fmt"
	"net"
	"os"
	"os/signal"
	"strings"
	"sync"
	"syscall"
	"time"

	"github.com/fangjunsheng555/m-ui/acme"
	"github.com/fangjunsheng555/m-ui/backup"
	"github.com/fangjunsheng555/m-ui/core"
	"github.com/fangjunsheng555/m-ui/creds"
	"github.com/fangjunsheng555/m-ui/database"
	"github.com/fangjunsheng555/m-ui/database/model"
	"github.com/fangjunsheng555/m-ui/hop"
	"github.com/fangjunsheng555/m-ui/hub"
	"github.com/fangjunsheng555/m-ui/jobs"
	"github.com/fangjunsheng555/m-ui/logger"
	"github.com/fangjunsheng555/m-ui/monitor"
	"github.com/fangjunsheng555/m-ui/notify"
	"github.com/fangjunsheng555/m-ui/upstream"
	"github.com/fangjunsheng555/m-ui/render"
	"github.com/fangjunsheng555/m-ui/sub"

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
	db       *gorm.DB
	core     *core.Core
	subSrv   *sub.Server
	jobs     *jobs.Scheduler
	notifier *notify.Notifier
	monitor  *monitor.Monitor
	hub      *hub.Hub
	dbPath   string
	cert     certState
	applied    map[string]string // 数据面当前生效的出站(tag → JSON),供上游热更新做差异
	appliedRaw []byte            // 数据面当前生效的完整配置,渲染结果相同则不重启
	mu       sync.Mutex // 串行化重载,避免并发改动互相打断
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
		if res.Error == "outbound not found" {
			res.Error = "数据面中尚无该上游(刚创建/修改请等重载完成后再测)"
		}
		return res.OK, int(res.Delay), "urltest", res.Error
	}
	switch up.Type {
	case "shadowsocks", "socks", "http":
		addr, err := upstream.ServerAddr(up.Options)
		if err != nil {
			return false, 0, "none", err.Error()
		}
		start := time.Now()
		conn, err := net.DialTimeout("tcp", addr, 5*time.Second)
		if err != nil {
			return false, 0, "tcp", "TCP 连接失败: " + err.Error()
		}
		conn.Close()
		return true, int(time.Since(start).Milliseconds()), "tcp", ""
	}
	return false, 0, "none", "数据面未运行:该协议走 QUIC/TLS,需数据面运行后才能真实测试"
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
	r := &Runner{db: db, core: core.NewCore(), subSrv: sub.NewServer(db), dbPath: dbPath}
	r.notifier = notify.New(r.setting)
	r.jobs = jobs.New(jobs.Deps{
		DB:          db,
		Box:         func() *core.Box { return r.core.GetInstance() },
		ReloadUsers: r.ReloadUsers,
		IsNode:      r.IsNode,
		Setting:     r.setting,
		Notify:      func(text string) { r.notifier.Event("tgOnUserDisabled", text) },
		LocalRatio:  r.localRatio,
	})
	r.monitor = monitor.New(monitor.Deps{
		DB: db, Setting: r.setting, CoreRunning: r.CoreRunning, Check: r.CheckUpstream, Notify: r.notifier,
	})
	r.hub = hub.New(hub.Deps{
		DB: db, Setting: r.setting, IsNode: r.IsNode, Version: Version,
		Notify:         func(toggle, text string) { r.notifier.Event(toggle, text) },
		LocalIPs:       r.OnlineIPs,
		SetExternalIPs: r.SetExternalIPs,
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
		box.Limiter().SetExternalIPs(m)
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

// ResetPassword 重置(或创建)管理员密码;pw 为空则随机生成。返回生效的明文密码。
func ResetPassword(dbPath, user, pw string) (string, error) {
	db, err := database.Open(dbPath)
	if err != nil {
		return "", err
	}
	defer database.Close(db)
	if pw == "" {
		pw = creds.Password(12)
	}
	if len(pw) < 6 {
		return "", fmt.Errorf("密码至少 6 位")
	}
	hash, err := hashPassword(pw)
	if err != nil {
		return "", err
	}
	var admin model.Admin
	if err := db.Where("username = ?", user).First(&admin).Error; err == nil {
		if err := db.Model(&model.Admin{}).Where("id = ?", admin.Id).Update("password", hash).Error; err != nil {
			return "", err
		}
	} else if err := db.Create(&model.Admin{Username: user, Password: hash}).Error; err != nil {
		return "", err
	}
	return pw, nil
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
func (r *Runner) KickUser(name string) int {
	if box := r.core.GetInstance(); box != nil {
		return box.ConnTracker().CloseConnByUser(name)
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
func (r *Runner) Start() error {
	raw, err := render.BuildConfig(r.db, r.nodeCert())
	if err != nil {
		return fmt.Errorf("渲染配置: %w", err)
	}
	if err := r.core.Start(raw); err != nil {
		return fmt.Errorf("启动 sing-box: %w", err)
	}
	r.applied, _ = outboundsOf(raw)
	r.appliedRaw = raw
	r.applyLimits()
	r.applyPortHopping()
	return nil
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
func (r *Runner) ReloadUpstreams() error {
	r.mu.Lock()
	defer r.mu.Unlock()
	if !r.core.IsRunning() {
		return nil
	}
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
func (r *Runner) ReloadUsers() error {
	r.mu.Lock()
	defer r.mu.Unlock()
	if !r.core.IsRunning() {
		return nil
	}
	raw, err := render.BuildConfig(r.db, r.nodeCert())
	if err != nil {
		return fmt.Errorf("渲染配置: %w", err)
	}
	var cfg struct {
		Inbounds []json.RawMessage `json:"inbounds"`
	}
	if err := json.Unmarshal(raw, &cfg); err != nil {
		return err
	}
	box := r.core.GetInstance()
	for _, inbound := range cfg.Inbounds {
		handled, err := r.core.UpdateInboundUsers(inbound)
		if err != nil {
			logger.Warning("热更新入站用户失败: ", err)
			continue
		}
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
		if json.Unmarshal(inbound, &meta) != nil || meta.Tag == "" {
			continue
		}
		if !handled {
			// 该协议不支持原地换用户表(socks/http/mixed):重建该入站,断开其全部连接
			if err := r.core.RemoveInbound(meta.Tag); err != nil && err != os.ErrInvalid {
				logger.Warning("重建入站 ", meta.Tag, " 失败(移除): ", err)
				continue
			}
			box.ConnTracker().CloseConnByInbound(meta.Tag)
			if err := r.core.AddInbound(inbound); err != nil {
				logger.Warning("重建入站 ", meta.Tag, " 失败(添加): ", err)
			}
			continue
		}
		// 断开已不再属于该入站的用户连接(禁用用户即时下线)
		keep := make(map[string]struct{}, len(meta.Users))
		for _, u := range meta.Users {
			if u.Name != "" {
				keep[u.Name] = struct{}{}
			}
			if u.Username != "" {
				keep[u.Username] = struct{}{}
			}
		}
		box.ConnTracker().CloseConnByInboundUsers(meta.Tag, keep)
	}
	r.applyLimits()
	return nil
}

// ReloadAll 重建整个数据面(线路增删、端口/协议变更、路由或证书变化时使用)。
func (r *Runner) ReloadAll() error {
	r.mu.Lock()
	defer r.mu.Unlock()
	raw, err := render.BuildConfig(r.db, r.nodeCert())
	if err != nil {
		return fmt.Errorf("渲染配置: %w", err)
	}
	return r.reloadAllLocked(raw)
}

// ReloadAllForce 无条件重启数据面(证书文件内容变了但配置文本没变时用)。
func (r *Runner) ReloadAllForce() error {
	r.mu.Lock()
	defer r.mu.Unlock()
	raw, err := render.BuildConfig(r.db, r.nodeCert())
	if err != nil {
		return fmt.Errorf("渲染配置: %w", err)
	}
	r.appliedRaw = nil
	return r.reloadAllLocked(raw)
}

// reloadAllLocked 用给定配置重启数据面(调用方持 mu)。渲染结果与当前生效配置完全相同则不重启。
func (r *Runner) reloadAllLocked(raw []byte) error {
	if r.core.IsRunning() && r.appliedRaw != nil && bytes.Equal(r.appliedRaw, raw) {
		r.applyLimits()
		logger.Info("配置无变化,数据面无需重启")
		return nil
	}
	// 先干跑校验新配置;不通过就让旧数据面继续服务——绝不为一条坏配置断掉所有用户。
	if r.core.IsRunning() {
		if err := core.ValidateConfig(raw); err != nil {
			return fmt.Errorf("新配置未通过校验,数据面保持原状: %w", err)
		}
	}
	r.core.Stop()
	if err := r.core.Start(raw); err != nil {
		r.applied, r.appliedRaw = nil, nil
		return fmt.Errorf("重启 sing-box: %w", err)
	}
	r.applied, _ = outboundsOf(raw)
	r.appliedRaw = raw
	r.applyLimits()
	r.applyPortHopping()
	logger.Info("数据面已重载")
	return nil
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
func (r *Runner) applyLimits() {
	var users []model.User
	if err := r.db.Find(&users).Error; err != nil {
		logger.Warning("读取用户限速策略失败: ", err)
		return
	}
	box := r.core.GetInstance()
	if box == nil {
		return
	}
	specs := make(map[string]core.UserLimitSpec, len(users))
	for _, u := range users {
		if u.SpeedUp == 0 && u.SpeedDown == 0 && u.DeviceLimit == 0 {
			continue
		}
		specs[u.Name] = core.UserLimitSpec{
			UpMbps:      u.SpeedUp,
			DownMbps:    u.SpeedDown,
			DeviceLimit: u.DeviceLimit,
		}
	}
	box.Limiter().SetLimits(specs)
	if len(specs) > 0 {
		logger.Info("已应用 ", len(specs), " 个用户的限速/设备数策略")
	}
}

func (r *Runner) Stop() {
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
			if err := database.Checkpoint(r.db); err != nil {
				logger.Warning("WAL 检查点失败: ", err)
			}
		case <-stop:
			return
		}
	}
}

// Run 启动数据面并阻塞直到收到终止信号(SIGHUP 触发重载)。
func Run(dbPath string) error {
	logger.InitLogger(logging.INFO)
	// 有待还原的备份(面板上传后重启到这里)先原子替换数据库与证书
	if applied, err := backup.ApplyPending(dbPath); err != nil {
		logger.Error("应用待还原备份失败(已改名为 .failed,继续用当前库): ", err)
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
	go r.checkpointLoop(stopCheckpoint)
	go r.certLoop(stopCheckpoint)
	go r.backupLoop(stopCheckpoint)
	go r.publicIPLoop(stopCheckpoint)
	go r.extLoop(stopCheckpoint)
	defer close(stopCheckpoint)

	sigCh := make(chan os.Signal, 1)
	signal.Notify(sigCh, syscall.SIGHUP, syscall.SIGTERM, syscall.SIGINT)
	for sig := range sigCh {
		if sig == syscall.SIGHUP {
			logger.Info("收到 SIGHUP,重载配置")
			r.Stop()
			time.Sleep(200 * time.Millisecond)
			if err := r.Start(); err != nil {
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
