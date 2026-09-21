package runner

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"path/filepath"
	"sort"
	"strings"
	"sync"
	"time"

	"github.com/Maoyangui/m-ui/acme"
	"github.com/Maoyangui/m-ui/certutil"
	"github.com/Maoyangui/m-ui/database/model"
	"github.com/Maoyangui/m-ui/logger"
	"github.com/Maoyangui/m-ui/notify"
	"gorm.io/gorm"
	"gorm.io/gorm/clause"
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

func (r *Runner) setSetting(key, val string) error {
	return saveSettings(r.db, map[string]string{key: val})
}

func saveSettings(db *gorm.DB, values map[string]string) error {
	keys := make([]string, 0, len(values))
	for key := range values {
		keys = append(keys, key)
	}
	sort.Strings(keys)
	for _, key := range keys {
		if err := db.Clauses(clause.OnConflict{
			Columns:   []clause.Column{{Name: "key"}},
			DoUpdates: clause.AssignmentColumns([]string{"value"}),
		}).Create(&model.Setting{Key: key, Value: values[key]}).Error; err != nil {
			return fmt.Errorf("保存设置 %s: %w", key, err)
		}
	}
	return nil
}

// certPaths 返回数据面证书路径(未设置时给出默认固定路径)。
func (r *Runner) certPaths(domain string) (string, string) {
	c, k := r.setting("certFile"), r.setting("keyFile")
	if c == "" || k == "" {
		name := strings.NewReplacer("/", "", "\\", "", ":", "", "..", "").Replace(strings.TrimSpace(domain))
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
	r.certOpMu.Lock()
	defer r.certOpMu.Unlock()
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
		if saveErr := r.setSetting("acmeAccountKey", res.AccountKey); saveErr != nil {
			return errors.Join(err, saveErr)
		}
	}
	if err != nil {
		r.cert.logf("失败: %v", err)
		r.notifier.Event("tgOnCert", "🔴 <b>证书签发失败</b>:"+notify.Esc(domain)+"\n"+notify.Esc(err.Error()))
		return err
	}
	if err := r.afterCertChange(certFile, keyFile, domain,
		!strings.EqualFold(r.setting("acmeApplyPanel"), "false"),
		!strings.EqualFold(r.setting("acmeApplySub"), "false"),
		map[string]string{
			"certFile": certFile, "keyFile": keyFile, "acmeDomain": domain, "certSource": "acme",
			"acmeApplyPanel": r.setting("acmeApplyPanel"), "acmeApplySub": r.setting("acmeApplySub"),
		}); err != nil {
		return err
	}
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
func (r *Runner) afterCertChange(certFile, keyFile, domain string, applyPanel, applySub bool, values map[string]string) error {
	wantSubCert, wantSubKey := "", ""
	if applySub {
		wantSubCert, wantSubKey = certFile, keyFile
	}
	wantWebCert, wantWebKey := "", ""
	if applyPanel {
		wantWebCert, wantWebKey = certFile, keyFile
	}
	if values == nil {
		values = map[string]string{}
	}
	var restartSub, webChanged bool
	if err := r.db.Transaction(func(tx *gorm.DB) error {
		var rows []model.Setting
		if err := tx.Where("key IN ?", []string{"webDomain", "subCertFile", "subKeyFile", "webCertFile", "webKeyFile", "subRestartPending"}).Find(&rows).Error; err != nil {
			return err
		}
		current := map[string]string{}
		for _, row := range rows {
			current[row.Key] = row.Value
		}
		if domain != "" && current["webDomain"] == "" {
			values["webDomain"] = domain
		}
		restartSub = current["subCertFile"] != wantSubCert || current["subKeyFile"] != wantSubKey || current["subRestartPending"] == "true"
		webChanged = current["webCertFile"] != wantWebCert || current["webKeyFile"] != wantWebKey
		values["subCertFile"], values["subKeyFile"] = wantSubCert, wantSubKey
		values["webCertFile"], values["webKeyFile"] = wantWebCert, wantWebKey
		if restartSub {
			values["subRestartPending"] = "true"
		}
		return saveSettings(tx, values)
	}); err != nil {
		return fmt.Errorf("保存证书设置失败: %w", err)
	}
	var applyErr error
	if restartSub {
		if err := r.RestartSub(); err != nil {
			r.cert.logf("重启订阅服务失败: %v", err)
			applyErr = fmt.Errorf("证书设置已保存，重启订阅服务失败: %w", err)
		} else if err := r.setSetting("subRestartPending", ""); err != nil {
			applyErr = err
		} else if wantSubCert == "" {
			r.cert.logf("订阅已改为 HTTP(用户需重新获取订阅地址)")
		} else {
			r.cert.logf("订阅服务已用该证书重启(HTTPS)")
		}
	}
	if webChanged {
		if wantWebCert == "" {
			r.cert.logf("面板已取消 HTTPS,重启 m-ui 后生效(地址改回 http://)")
		} else {
			r.cert.logf("面板已启用 HTTPS,重启 m-ui 后生效")
		}
	}

	// 证书文件内容变了但路径没变:sing-box 监视着 certificate_path / key_path,写入后自动热加载,
	// 不用重启数据面(以前每次续期都强制重启,所有人掉线一次);路径变了(换来源)才整体重载。
	if r.appliedUsesCert(certFile, keyFile) {
		if err := r.ReloadAll(); err != nil { // 域名(server_name)之类变了时它自己会重启,否则无操作
			r.cert.logf("数据面重载失败: %v", err)
			applyErr = errors.Join(applyErr, fmt.Errorf("证书设置已保存，数据面重载失败: %w", err))
		} else {
			r.cert.logf("线路入站证书已由数据面自动热加载(路径不变,不重启)")
		}
		return applyErr
	}
	if err := r.ReloadAllForce(); err != nil {
		r.cert.logf("数据面重载失败: %v", err)
		applyErr = errors.Join(applyErr, fmt.Errorf("证书设置已保存，数据面重载失败: %w", err))
	} else {
		r.cert.logf("线路入站已用新证书重载")
	}
	return applyErr
}

// appliedUsesCert 当前生效的配置里,入站是否已经引用这两个证书文件路径。
func (r *Runner) appliedUsesCert(certFile, keyFile string) bool {
	r.mu.Lock()
	raw := r.appliedRaw
	r.mu.Unlock()
	if raw == nil {
		return false
	}
	var cfg struct {
		Inbounds []struct {
			TLS struct {
				Cert string `json:"certificate_path"`
				Key  string `json:"key_path"`
			} `json:"tls"`
		} `json:"inbounds"`
	}
	if json.Unmarshal(raw, &cfg) != nil {
		return false
	}
	for _, ib := range cfg.Inbounds {
		if ib.TLS.Cert != "" && (ib.TLS.Cert != certFile || ib.TLS.Key != keyFile) {
			return false
		}
	}
	return true
}

// SelfSign 生成自签证书(无域名 / 纯 IP 场景)。默认只给线路入站用:
// 自签证书不被系统信任,面板与订阅走 HTTPS 会让浏览器和客户端报错;
// 订阅链接会自动带"允许不安全",客户端打开该开关即可连上。
func (r *Runner) SelfSign(hosts []string, applyPanel, applySub bool) error {
	r.certOpMu.Lock()
	defer r.certOpMu.Unlock()
	if len(hosts) == 0 { // 无域名场景不该逼用户填东西:自动用本机探测到的公网 IP / 入口地址
		hosts = r.autoCertHosts()
	}
	if len(hosts) == 0 {
		return errors.New("没有探测到本机公网 IP,请手动填写服务器 IP")
	}
	certFile, keyFile := r.DataPlaneCert()
	if err := certutil.GenerateSelfSigned(hosts, certFile, keyFile, 3650); err != nil {
		return err
	}
	r.cert.logf("自签证书已生成: %s,包含地址 %s(用于线路入站;订阅里每个节点会自动带允许不安全标记)", certFile, strings.Join(hosts, ", "))
	return r.afterCertChange(certFile, keyFile, "", applyPanel, applySub,
		map[string]string{"certFile": certFile, "keyFile": keyFile, "certSource": "selfsign",
			"acmeApplyPanel": boolText(applyPanel), "acmeApplySub": boolText(applySub)})
}

// autoCertHosts 自签证书默认包含的地址:本机公网 IP(v4/v6)、本机节点手填的连接地址、已设置的域名。
// 客户端连哪个地址、证书里就要有哪个地址,否则即使允许不安全也可能被某些客户端拒绝。
func (r *Runner) autoCertHosts() []string {
	seen := map[string]bool{}
	var out []string
	add := func(v string) {
		for _, h := range strings.Split(v, ",") {
			h = strings.TrimSpace(h)
			if h != "" && !seen[h] {
				seen[h] = true
				out = append(out, h)
			}
		}
	}
	add(r.setting("publicIp"))
	var local model.Node
	if err := r.db.Where("is_local = ?", true).First(&local).Error; err == nil {
		add(local.PublicIP)
		add(local.Addr)
		add(local.Domain)
	}
	add(r.setting("webDomain"))
	return out
}

// UseExternalCert 使用服务器上已有的证书(如 certbot / nginx / 商业证书):只记录路径,不复制文件,
// 证书续期后覆盖原文件即可,面板与订阅会自动换用(线路入站在下次重载时生效)。
func (r *Runner) UseExternalCert(certFile, keyFile string, applyPanel, applySub bool) error {
	r.certOpMu.Lock()
	defer r.certOpMu.Unlock()
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
	domain := ""
	if len(info.DNSNames) > 0 {
		domain = info.DNSNames[0]
	}
	r.cert.logf("已使用外部证书 %s(%s,剩余 %d 天)", certFile, info.Subject, info.DaysLeft)
	return r.afterCertChange(certFile, keyFile, domain, applyPanel, applySub,
		map[string]string{"certFile": certFile, "keyFile": keyFile, "certSource": "external",
			"acmeApplyPanel": boolText(applyPanel), "acmeApplySub": boolText(applySub)})
}

// ApplyCertTargets 只改套用目标(面板 / 订阅 HTTPS),不换证书。
func (r *Runner) ApplyCertTargets(applyPanel, applySub bool) error {
	r.certOpMu.Lock()
	defer r.certOpMu.Unlock()
	certFile, keyFile := r.DataPlaneCert()
	if (applyPanel || applySub) && !acme.Info(certFile).Exists {
		return errors.New("当前没有可用证书,请先签发、自签或填写已有证书")
	}
	return r.afterCertChange(certFile, keyFile, "", applyPanel, applySub,
		map[string]string{"acmeApplyPanel": boolText(applyPanel), "acmeApplySub": boolText(applySub)})
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
	r.subSrv = r.newSubServer()
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
