// Package runner 启动 m-ui 数据面:渲染配置 → 拉起内嵌 sing-box → 应用用户限速/设备数策略。
package runner

import (
	"encoding/json"
	"fmt"
	"os"
	"os/signal"
	"sync"
	"syscall"
	"time"

	"github.com/fangjunsheng555/m-ui/core"
	"github.com/fangjunsheng555/m-ui/database"
	"github.com/fangjunsheng555/m-ui/database/model"
	"github.com/fangjunsheng555/m-ui/logger"
	"github.com/fangjunsheng555/m-ui/render"
	"github.com/fangjunsheng555/m-ui/sub"

	"github.com/op/go-logging"
	"gorm.io/gorm"
)

// startPanel 由 main 注入(web 包依赖 runner,反向直接引用会成环)。
var startPanel func(*Runner) error

// SetPanelStarter 注册面板启动函数,在 Run 时随数据面一起拉起。
func SetPanelStarter(f func(*Runner) error) { startPanel = f }

// Runner 持有数据面运行所需的一切。
type Runner struct {
	db     *gorm.DB
	core   *core.Core
	subSrv *sub.Server
	mu     sync.Mutex // 串行化重载,避免并发改动互相打断
}

// DB 暴露数据库句柄给面板层。
func (r *Runner) DB() *gorm.DB { return r.db }

func New(dbPath string) (*Runner, error) {
	db, err := database.Open(dbPath)
	if err != nil {
		return nil, err
	}
	return &Runner{db: db, core: core.NewCore(), subSrv: sub.NewServer(db)}, nil
}

func (r *Runner) setting(key string) string {
	var v string
	r.db.Raw("SELECT value FROM settings WHERE key = ?", key).Scan(&v)
	return v
}

// nodeCert 读取本机证书材料(各机各签,路径固定)。
func (r *Runner) nodeCert() render.NodeCert {
	return render.NodeCert{
		ServerName: r.setting("webDomain"),
		CertPath:   r.setting("webCertFile"),
		KeyPath:    r.setting("webKeyFile"),
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
	r.applyLimits()
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
		if !handled || box == nil {
			continue
		}
		// 断开已不再属于该入站的用户连接(禁用用户即时下线)
		var meta struct {
			Tag   string `json:"tag"`
			Users []struct {
				Name string `json:"name"`
			} `json:"users"`
		}
		if json.Unmarshal(inbound, &meta) != nil || meta.Tag == "" {
			continue
		}
		keep := make(map[string]struct{}, len(meta.Users))
		for _, u := range meta.Users {
			keep[u.Name] = struct{}{}
		}
		box.ConnTracker().CloseConnByInboundUsers(meta.Tag, keep)
	}
	r.applyLimits()
	return nil
}

// ReloadAll 重建整个数据面(线路增删、端口/协议变更、上游或路由变化时使用)。
func (r *Runner) ReloadAll() error {
	r.mu.Lock()
	defer r.mu.Unlock()
	r.core.Stop()
	raw, err := render.BuildConfig(r.db, r.nodeCert())
	if err != nil {
		return fmt.Errorf("渲染配置: %w", err)
	}
	if err := r.core.Start(raw); err != nil {
		return fmt.Errorf("重启 sing-box: %w", err)
	}
	r.applyLimits()
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

	stopCheckpoint := make(chan struct{})
	go r.checkpointLoop(stopCheckpoint)
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
