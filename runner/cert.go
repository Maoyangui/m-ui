package runner

import (
	"context"
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
	txt      []acme.TXTRecord // 手动 DNS:正在等用户加的记录
}

func (c *certState) setTXT(recs []acme.TXTRecord) {
	c.mu.Lock()
	c.txt = recs
	c.mu.Unlock()
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

// certFileName 域名变成文件名:* 换成 _(*.example.com → _.example.com)。
func certFileName(domain string) string {
	return strings.NewReplacer("/", "", "\\", "", ":", "", "..", "", "*", "_").Replace(strings.TrimSpace(domain))
}

// certPaths 返回数据面证书路径(未设置时给出默认固定路径)。
func (r *Runner) certPaths(domain string) (string, string) {
	c, k := r.setting("certFile"), r.setting("keyFile")
	if c == "" || k == "" {
		return r.defaultCertPaths(domain)
	}
	return c, k
}

// defaultCertPaths 面板自己存证书的固定路径(数据目录下 cert/<域名>.crt / .key)。
func (r *Runner) defaultCertPaths(domain string) (string, string) {
	name := certFileName(domain)
	if name == "" {
		name = "main"
	}
	return filepath.Join(r.DataDir(), "cert", name+".crt"), filepath.Join(r.DataDir(), "cert", name+".key")
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
		"lastError": r.cert.lastErr, "lastOk": r.cert.lastOK, "lastTime": r.cert.lastTime, "txt": r.cert.txt,
	}
}

// IssueRequest 一次签发:域名与验证方式,签好之后顶替哪些用途。
type IssueRequest struct {
	Domain, Method      string
	Staging, AutoRenew  bool
	Targets             CertTargets
	directoryURL        string // 测试用
}

// IssueCert 发起一次签发(异步),进度经 CertStatus 查询。
func (r *Runner) IssueCert(req IssueRequest) error {
	if err := r.startIssue(); err != nil {
		return err
	}
	go r.finishIssue(req)
	return nil
}

func (r *Runner) startIssue() error {
	r.cert.mu.Lock()
	defer r.cert.mu.Unlock()
	if r.cert.running {
		return errors.New("已有签发任务在进行")
	}
	r.cert.running, r.cert.log, r.cert.lastErr, r.cert.txt = true, nil, "", nil
	return nil
}

func (r *Runner) finishIssue(req IssueRequest) error {
	err := r.issueCert(req)
	r.cert.mu.Lock()
	r.cert.running, r.cert.txt = false, nil
	r.cert.lastTime = time.Now().Unix()
	if err != nil {
		r.cert.lastErr = err.Error()
	} else {
		r.cert.lastOK = time.Now().Unix()
	}
	r.cert.mu.Unlock()
	return err
}

// runIssue 同步签发一张(自动续期用,一张张来)。
func (r *Runner) runIssue(req IssueRequest) error {
	if err := r.startIssue(); err != nil {
		return err
	}
	return r.finishIssue(req)
}

func (r *Runner) issueCert(req IssueRequest) error {
	r.certOpMu.Lock()
	defer r.certOpMu.Unlock()
	domain := strings.ToLower(strings.TrimSpace(req.Domain))
	if domain == "" {
		err := errors.New("未填写域名")
		r.cert.logf("%v", err)
		return err
	}
	if req.Method == "" {
		req.Method = acme.MethodHTTP
	}
	certFile, keyFile := r.storedPaths(domain)
	cfg := acme.Config{
		Email: r.setting("acmeEmail"), Domain: domain, Method: req.Method,
		CFToken: r.setting("acmeCfToken"), Staging: req.Staging,
		CertPath: certFile, KeyPath: keyFile, AccountKey: r.setting("acmeAccountKey"), Logf: r.cert.logf,
		OnTXT: r.cert.setTXT, DirectoryURL: req.directoryURL,
	}
	wait := 8 * time.Minute
	if req.Method == acme.MethodManual {
		wait = 30 * time.Minute // 要等人去 DNS 服务商加记录
	}
	ctx, cancel := context.WithTimeout(context.Background(), wait)
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
	if err := r.saveCertRow(model.Cert{Name: domain, Source: "acme", CertFile: certFile, KeyFile: keyFile,
		Method: req.Method, AutoRenew: req.AutoRenew && req.Method != acme.MethodManual, Staging: req.Staging}); err != nil {
		return fmt.Errorf("证书已写入 %s,记进证书库失败: %w", certFile, err)
	}
	values := map[string]string{"acmeDomain": domain}
	if req.Targets.Lines {
		values["certSource"] = "acme"
	}
	if err := r.afterCertChange(certFile, keyFile, domain, req.Targets, values); err != nil {
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

// afterCertChange 证书写好之后:targets 勾了的用途换成它(订阅立即重启生效,面板监听器需重启 m-ui),没勾的不动。
// 这张证书本来就在某个用途上的(续期),路径没变,不用换。
func (r *Runner) afterCertChange(certFile, keyFile, domain string, targets CertTargets, values map[string]string) error {
	wantSubCert, wantSubKey := r.setting("subCertFile"), r.setting("subKeyFile")
	if targets.Sub {
		wantSubCert, wantSubKey = certFile, keyFile
	}
	wantWebCert, wantWebKey := r.setting("webCertFile"), r.setting("webKeyFile")
	if targets.Panel {
		wantWebCert, wantWebKey = certFile, keyFile
	}
	if values == nil {
		values = map[string]string{}
	}
	if targets.Lines || r.setting("certFile") == "" { // 还没有默认证书时,第一张就是默认的
		values["certFile"], values["keyFile"] = certFile, keyFile
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
		// 泛域名不能当访问地址(也进不了 SNI):面板域名只从具体域名上取
		if domain != "" && !acme.IsWildcard(domain) && current["webDomain"] == "" {
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

	r.syncLocalCerts()
	// 证书文件内容变了但路径没变:sing-box 监视着 certificate_path / key_path,写入后自动热加载,不用重启数据面
	// (以前每次续期都强制重启,所有人掉线一次)。ReloadAll 按渲染结果比:路径或域名真变了才重启,否则无操作。
	if err := r.ReloadAll(); err != nil {
		r.cert.logf("数据面重载失败: %v", err)
		applyErr = errors.Join(applyErr, fmt.Errorf("证书设置已保存，数据面重载失败: %w", err))
	} else {
		r.cert.logf("证书已生效(路径没变的由数据面自动热加载,不重启)")
	}
	return applyErr
}

// SelfSign 生成自签证书(无域名 / 纯 IP 场景)。默认只给线路入站用:
// 自签证书不被系统信任,面板与订阅走 HTTPS 会让浏览器和客户端报错;
// 订阅链接会自动带"允许不安全",客户端打开该开关即可连上。
func (r *Runner) SelfSign(hosts []string, targets CertTargets) error {
	r.certOpMu.Lock()
	defer r.certOpMu.Unlock()
	if len(hosts) == 0 { // 无域名场景不该逼用户填东西:自动用本机探测到的公网 IP / 入口地址
		hosts = r.autoCertHosts()
	}
	if len(hosts) == 0 {
		return errors.New("没有探测到本机公网 IP,请手动填写服务器 IP")
	}
	certFile, keyFile := r.storedPaths(SelfSignedName)
	if err := certutil.GenerateSelfSigned(hosts, certFile, keyFile, 3650); err != nil {
		return err
	}
	if err := r.saveCertRow(model.Cert{Name: SelfSignedName, Source: "selfsign", CertFile: certFile, KeyFile: keyFile}); err != nil {
		return err
	}
	r.cert.logf("自签证书已生成: %s,包含地址 %s(用于线路入站;订阅里每个节点会自动带允许不安全标记)", certFile, strings.Join(hosts, ", "))
	values := map[string]string{}
	if targets.Lines {
		values["certSource"] = "selfsign"
	}
	return r.afterCertChange(certFile, keyFile, "", targets, values)
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
	add(r.setting("publicIp6")) // 双栈机器的 IPv6(服务器选了"自动地址用 IPv6"时客户端连的就是它)
	var local model.Node
	if err := r.db.Where("is_local = ?", true).First(&local).Error; err == nil {
		add(local.PublicIP)
		add(local.PublicIP6)
		add(local.Addr)
		add(local.Domain)
	}
	add(r.setting("webDomain"))
	return out
}

// UseExternalCert 使用服务器上已有的证书(如 certbot / nginx / 商业证书):只记录路径,不复制文件,
// 证书续期后覆盖原文件即可,面板与订阅会自动换用(线路入站在下次重载时生效)。
func (r *Runner) UseExternalCert(certFile, keyFile string, targets CertTargets) error {
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
	name := certNameOf(info, certFile)
	if info.SelfSigned { // 用户自己的自签文件:不能和面板生成的那张抢 self-signed 这个名字
		name = strings.ToLower(strings.TrimSuffix(filepath.Base(certFile), filepath.Ext(certFile)))
	}
	if err := r.saveCertRow(model.Cert{Name: name, Source: "external", CertFile: certFile, KeyFile: keyFile}); err != nil {
		return err
	}
	r.cert.logf("已添加服务器上的证书 %s(%s,剩余 %d 天)", certFile, info.Subject, info.DaysLeft)
	values := map[string]string{}
	if targets.Lines {
		values["certSource"] = "external"
	}
	return r.afterCertChange(certFile, keyFile, domain, targets, values)
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

// certLoop 每 12 小时查一次证书库,到期前 30 天的自动续期(只续自己签发的,服务器上已有的证书和自签不管)。
func (r *Runner) certLoop(stop <-chan struct{}) {
	t := time.NewTicker(12 * time.Hour)
	defer t.Stop()
	for {
		select {
		case <-t.C:
			r.renewDue()
		case <-stop:
			return
		}
	}
}
