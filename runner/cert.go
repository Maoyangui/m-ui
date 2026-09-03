package runner

import (
	"context"
	"errors"
	"fmt"
	"path/filepath"
	"strings"
	"sync"
	"time"

	"github.com/Maoyangui/m-ui/acme"
	"github.com/Maoyangui/m-ui/certutil"
	"github.com/Maoyangui/m-ui/database/model"
	"github.com/Maoyangui/m-ui/logger"
	"github.com/Maoyangui/m-ui/notify"
	"github.com/Maoyangui/m-ui/sub"
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
	r.setSetting("certSource", "acme")
	r.afterCertChange(certFile, keyFile, domain,
		!strings.EqualFold(r.setting("acmeApplyPanel"), "false"),
		!strings.EqualFold(r.setting("acmeApplySub"), "false"))
	r.notifier.Event("tgOnCert", fmt.Sprintf("🟢 <b>证书已签发</b>:%s\n到期 %s", notify.Esc(domain), res.NotAfter.Format("2006-01-02")))
	return nil
}

// DataPlaneCert 当前线路入站(数据面)用的证书与私钥路径。
func (r *Runner) DataPlaneCert() (string, string) { return r.certPaths(r.setting("webDomain")) }

// CertSource 证书来源:acme(Let's Encrypt 签发)/ selfsign(自签)/ external(服务器上已有)。
func (r *Runner) CertSource() string {
	src := r.setting("certSource")
	if src != "" {
		return src
	}
	if info := r.CertInfo(); info.Exists && info.SelfSigned { // 老库没有该设置,按证书本身推断
		return "selfsign"
	}
	if r.setting("acmeDomain") != "" {
		return "acme"
	}
	return ""
}

// afterCertChange 证书变更后:线路入站(数据面)始终换用新证书;面板与订阅按 applyPanel / applySub 开关,
// 取消勾选会清掉对应设置(订阅立即重启生效,面板监听器需重启 m-ui)。
func (r *Runner) afterCertChange(certFile, keyFile, domain string, applyPanel, applySub bool) {
	if domain != "" && r.setting("webDomain") == "" {
		r.setSetting("webDomain", domain)
	}

	wantSubCert, wantSubKey := "", ""
	if applySub {
		wantSubCert, wantSubKey = certFile, keyFile
	}
	if r.setting("subCertFile") != wantSubCert || r.setting("subKeyFile") != wantSubKey {
		r.setSetting("subCertFile", wantSubCert)
		r.setSetting("subKeyFile", wantSubKey)
		if err := r.RestartSub(); err != nil {
			r.cert.logf("重启订阅服务失败: %v", err)
		} else if wantSubCert == "" {
			r.cert.logf("订阅已改为 HTTP(用户需重新获取订阅地址)")
		} else {
			r.cert.logf("订阅服务已用该证书重启(HTTPS)")
		}
	}

	wantWebCert, wantWebKey := "", ""
	if applyPanel {
		wantWebCert, wantWebKey = certFile, keyFile
	}
	if r.setting("webCertFile") != wantWebCert || r.setting("webKeyFile") != wantWebKey {
		r.setSetting("webCertFile", wantWebCert)
		r.setSetting("webKeyFile", wantWebKey)
		if wantWebCert == "" {
			r.cert.logf("面板已取消 HTTPS,重启 m-ui 后生效(地址改回 http://)")
		} else {
			r.cert.logf("面板已启用 HTTPS,重启 m-ui 后生效")
		}
	}

	if err := r.ReloadAllForce(); err != nil { // 证书文件内容变了,配置文本不变也必须重启
		r.cert.logf("数据面重载失败: %v", err)
	} else {
		r.cert.logf("线路入站已用新证书重载")
	}
}

// SelfSign 生成自签证书(无域名 / 纯 IP 场景)。默认只给线路入站用:
// 自签证书不被系统信任,面板与订阅走 HTTPS 会让浏览器和客户端报错;
// 订阅链接会自动带"允许不安全",客户端打开该开关即可连上。
func (r *Runner) SelfSign(hosts []string, applyPanel, applySub bool) error {
	if len(hosts) == 0 {
		return errors.New("至少填一个域名或 IP")
	}
	certFile, keyFile := r.certPaths(hosts[0])
	if err := certutil.GenerateSelfSigned(hosts, certFile, keyFile, 3650); err != nil {
		return err
	}
	r.setSetting("certFile", certFile)
	r.setSetting("keyFile", keyFile)
	r.setSetting("certSource", "selfsign")
	r.cert.logf("自签证书已生成: %s(用于线路入站;订阅链接会自动带允许不安全标记)", certFile)
	r.afterCertChange(certFile, keyFile, "", applyPanel, applySub)
	return nil
}

// UseExternalCert 使用服务器上已有的证书(如 certbot / nginx / 商业证书):只记录路径,不复制文件,
// 证书续期后覆盖原文件即可,面板与订阅会自动换用(线路入站在下次重载时生效)。
func (r *Runner) UseExternalCert(certFile, keyFile string, applyPanel, applySub bool) error {
	certFile, keyFile = strings.TrimSpace(certFile), strings.TrimSpace(keyFile)
	if certFile == "" || keyFile == "" {
		return errors.New("请填写证书与私钥的完整路径")
	}
	if err := certutil.Verify(certFile, keyFile); err != nil {
		return err
	}
	info := acme.Info(certFile)
	if info.Exists && info.DaysLeft < 0 {
		return fmt.Errorf("该证书已于 %s 过期", info.NotAfter.Format("2006-01-02"))
	}
	r.setSetting("certFile", certFile)
	r.setSetting("keyFile", keyFile)
	r.setSetting("certSource", "external")
	domain := ""
	if len(info.DNSNames) > 0 {
		domain = info.DNSNames[0]
	}
	r.cert.logf("已使用外部证书 %s(%s,剩余 %d 天)", certFile, info.Subject, info.DaysLeft)
	r.afterCertChange(certFile, keyFile, domain, applyPanel, applySub)
	return nil
}

// ApplyCertTargets 只改套用目标(面板 / 订阅 HTTPS),不换证书。
func (r *Runner) ApplyCertTargets(applyPanel, applySub bool) error {
	certFile, keyFile := r.DataPlaneCert()
	if (applyPanel || applySub) && !acme.Info(certFile).Exists {
		return errors.New("当前没有可用证书,请先签发、自签或填写已有证书")
	}
	r.setSetting("acmeApplyPanel", boolText(applyPanel))
	r.setSetting("acmeApplySub", boolText(applySub))
	r.afterCertChange(certFile, keyFile, "", applyPanel, applySub)
	return nil
}

func boolText(b bool) string {
	if b {
		return "true"
	}
	return "false"
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
