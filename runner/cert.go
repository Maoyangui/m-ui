package runner

import (
	"context"
	"errors"
	"fmt"
	"path/filepath"
	"strings"
	"sync"
	"time"

	"github.com/fangjunsheng555/m-ui/acme"
	"github.com/fangjunsheng555/m-ui/certutil"
	"github.com/fangjunsheng555/m-ui/database/model"
	"github.com/fangjunsheng555/m-ui/logger"
	"github.com/fangjunsheng555/m-ui/notify"
	"github.com/fangjunsheng555/m-ui/sub"
)

// certState 记录一次签发的进度,供面板轮询。
type certState struct {
	mu       sync.Mutex
	running  bool
	log      []string
	lastErr  string
	lastOK   int64
	lastTime int64
}

func (c *certState) logf(format string, a ...interface{}) {
	c.mu.Lock()
	c.log = append(c.log, time.Now().Format("15:04:05")+" "+fmt.Sprintf(format, a...))
	if len(c.log) > 200 {
		c.log = c.log[len(c.log)-200:]
	}
	c.mu.Unlock()
}

// DataDir 返回数据库所在目录(证书、备份的默认位置)。
func (r *Runner) DataDir() string {
	abs, err := filepath.Abs(r.dbPath)
	if err != nil {
		return filepath.Dir(r.dbPath)
	}
	return filepath.Dir(abs)
}

func (r *Runner) setSetting(key, val string) {
	var existing model.Setting
	if err := r.db.Where("key = ?", key).First(&existing).Error; err == nil {
		r.db.Model(&model.Setting{}).Where("key = ?", key).Update("value", val)
	} else {
		r.db.Create(&model.Setting{Key: key, Value: val})
	}
}

// certPaths 返回数据面证书路径(未设置时给出默认固定路径)。
func (r *Runner) certPaths(domain string) (string, string) {
	c, k := r.setting("certFile"), r.setting("keyFile")
	if c == "" || k == "" {
		name := domain
		if name == "" {
			name = "main"
		}
		c = filepath.Join(r.DataDir(), "cert", name+".crt")
		k = filepath.Join(r.DataDir(), "cert", name+".key")
	}
	return c, k
}

// CertInfo 返回当前数据面证书信息。
func (r *Runner) CertInfo() acme.CertInfo {
	c, _ := r.certPaths(r.setting("webDomain"))
	return acme.Info(c)
}

// CertStatus 返回签发进度。
func (r *Runner) CertStatus() map[string]interface{} {
	r.cert.mu.Lock()
	defer r.cert.mu.Unlock()
	return map[string]interface{}{
		"running": r.cert.running, "log": append([]string(nil), r.cert.log...),
		"lastError": r.cert.lastErr, "lastOk": r.cert.lastOK, "lastTime": r.cert.lastTime,
	}
}

// IssueCert 按设置发起一次签发(异步),进度经 CertStatus 查询。
func (r *Runner) IssueCert() error {
	r.cert.mu.Lock()
	if r.cert.running {
		r.cert.mu.Unlock()
		return errors.New("已有签发任务在进行")
	}
	r.cert.running, r.cert.log, r.cert.lastErr = true, nil, ""
	r.cert.mu.Unlock()
	go func() {
		err := r.issueCert()
		r.cert.mu.Lock()
		r.cert.running = false
		r.cert.lastTime = time.Now().Unix()
		if err != nil {
			r.cert.lastErr = err.Error()
		} else {
			r.cert.lastOK = time.Now().Unix()
		}
		r.cert.mu.Unlock()
	}()
	return nil
}

func (r *Runner) issueCert() error {
	domain := strings.TrimSpace(r.setting("acmeDomain"))
	if domain == "" {
		domain = strings.TrimSpace(r.setting("webDomain"))
	}
	if domain == "" {
		err := errors.New("未填写域名")
		r.cert.logf("%v", err)
		return err
	}
	certFile, keyFile := r.certPaths(domain)
	cfg := acme.Config{
		Email: r.setting("acmeEmail"), Domain: domain, Method: r.setting("acmeMethod"),
		CFToken: r.setting("acmeCfToken"), Staging: strings.EqualFold(r.setting("acmeStaging"), "true"),
		CertPath: certFile, KeyPath: keyFile, AccountKey: r.setting("acmeAccountKey"), Logf: r.cert.logf,
	}
	if cfg.Method == "" {
		cfg.Method = "http"
	}
	ctx, cancel := context.WithTimeout(context.Background(), 6*time.Minute)
	defer cancel()
	res, err := acme.Issue(ctx, cfg)
	if res.AccountKey != "" && res.AccountKey != cfg.AccountKey {
		r.setSetting("acmeAccountKey", res.AccountKey)
	}
	if err != nil {
		r.cert.logf("失败: %v", err)
		r.notifier.Event("tgOnCert", "🔴 <b>证书签发失败</b>:"+notify.Esc(domain)+"\n"+notify.Esc(err.Error()))
		return err
	}
	r.setSetting("certFile", certFile)
	r.setSetting("keyFile", keyFile)
	r.setSetting("acmeDomain", domain)
	r.afterCertChange(certFile, keyFile, domain)
	r.notifier.Event("tgOnCert", fmt.Sprintf("🟢 <b>证书已签发</b>:%s\n到期 %s", notify.Esc(domain), res.NotAfter.Format("2006-01-02")))
	return nil
}

// afterCertChange 把新证书套用到面板/订阅(按设置)并重载数据面。
func (r *Runner) afterCertChange(certFile, keyFile, domain string) {
	if domain != "" && r.setting("webDomain") == "" {
		r.setSetting("webDomain", domain)
	}
	subChanged := false
	if !strings.EqualFold(r.setting("acmeApplySub"), "false") {
		if r.setting("subCertFile") != certFile || r.setting("subKeyFile") != keyFile {
			subChanged = r.setting("subCertFile") == "" // 从 http 变 https 需重启监听
			r.setSetting("subCertFile", certFile)
			r.setSetting("subKeyFile", keyFile)
			subChanged = true
		}
	}
	if !strings.EqualFold(r.setting("acmeApplyPanel"), "false") {
		if r.setting("webCertFile") == "" {
			r.cert.logf("面板当前为 http,已写入证书路径,重启 m-ui 后面板改为 https")
		}
		r.setSetting("webCertFile", certFile)
		r.setSetting("webKeyFile", keyFile)
	}
	if subChanged {
		if err := r.RestartSub(); err != nil {
			r.cert.logf("重启订阅服务失败: %v", err)
		} else {
			r.cert.logf("订阅服务已用新证书重启")
		}
	}
	if err := r.ReloadAllForce(); err != nil { // 证书文件内容变了,配置文本不变也必须重启
		r.cert.logf("数据面重载失败: %v", err)
	} else {
		r.cert.logf("数据面已用新证书重载")
	}
}

// SelfSign 生成自签证书到固定路径并套用(测试或纯 IP 场景)。
func (r *Runner) SelfSign(hosts []string) error {
	if len(hosts) == 0 {
		return errors.New("至少填一个域名或 IP")
	}
	certFile, keyFile := r.certPaths(hosts[0])
	if err := certutil.GenerateSelfSigned(hosts, certFile, keyFile, 3650); err != nil {
		return err
	}
	r.setSetting("certFile", certFile)
	r.setSetting("keyFile", keyFile)
	r.cert.logf("自签证书已生成: %s", certFile)
	r.afterCertChange(certFile, keyFile, "")
	return nil
}

// RestartSub 用当前设置重启订阅服务(端口/证书变更后)。
func (r *Runner) RestartSub() error {
	r.mu.Lock()
	defer r.mu.Unlock()
	if r.subSrv != nil {
		r.subSrv.Stop()
	}
	time.Sleep(200 * time.Millisecond)
	r.subSrv = sub.NewServer(r.db)
	return r.subSrv.Start()
}

// certLoop 每天检查一次,到期前 30 天自动续期。
func (r *Runner) certLoop(stop <-chan struct{}) {
	t := time.NewTicker(12 * time.Hour)
	defer t.Stop()
	for {
		select {
		case <-t.C:
			r.maybeRenew()
		case <-stop:
			return
		}
	}
}

func (r *Runner) maybeRenew() {
	if strings.EqualFold(r.setting("acmeAutoRenew"), "false") || r.setting("acmeDomain") == "" {
		return
	}
	info := r.CertInfo()
	if !info.Exists || info.SelfSigned || info.DaysLeft > 30 {
		return
	}
	if r.notifier.Once("cert-renew", 20*time.Hour) {
		logger.Info("证书剩余 ", info.DaysLeft, " 天,自动续期")
		r.cert.logf("自动续期:剩余 %d 天", info.DaysLeft)
		r.IssueCert()
	}
}
