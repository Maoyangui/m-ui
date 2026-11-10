package runner

import (
	"encoding/json"
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"sort"
	"strings"
	"time"

	"github.com/Maoyangui/m-ui/acme"
	"github.com/Maoyangui/m-ui/certutil"
	"github.com/Maoyangui/m-ui/database/model"
	"github.com/Maoyangui/m-ui/logger"
	"github.com/Maoyangui/m-ui/render"
	"github.com/Maoyangui/m-ui/sub"
)

// 证书库:一台服务器可以有多张证书。每张证书一行(model.Cert),文件在数据目录 cert/ 下,或指向服务器上已有的文件。
// 线路默认证书(certFile / keyFile)、面板(webCertFile…)、订阅(subCertFile…)各选各的;线路也可以单独选一张
// (Line.Tls.cert 存证书名,各台服务器按名字找自己的那张,找不到用自己的默认证书)。
// 证书各机各签,不随快照同步;主机只从副机报告里知道每台有哪些(Node.Certs),线路编辑框与订阅的 SNI 用它。

// SelfSignedName 自签证书在库里的名字。每台机器只留一张,重新生成就覆盖。
const SelfSignedName = "self-signed"

// 证书的用途。
const (
	CertForLines = "lines" // 线路默认证书
	CertForPanel = "panel" // 面板 HTTPS
	CertForSub   = "sub"   // 订阅 HTTPS
)

// CertTargets 新证书要顶替哪些用途;没勾的不动(以前没勾的会被清掉,签第二张证书把面板的 HTTPS 关了)。
type CertTargets struct {
	Lines, Panel, Sub bool
}

// CertView 证书库的一行:库里记的(含续期状态)+ 证书文件读出来的 + 正用在哪 + 能不能自动续、为什么。
type CertView struct {
	model.Cert
	Info        acme.CertInfo `json:"info"`
	ForLines    bool          `json:"forLines"`
	ForPanel    bool          `json:"forPanel"`
	ForSub      bool          `json:"forSub"`
	ForReseller bool          `json:"forReseller,omitempty"` // 代理面板 HTTPS
	LineNames   []string      `json:"lineNames"`             // 单独选了这张证书的线路
	RenewState
}

// certNameOf 证书在库里的名字:自签的统一叫 self-signed,其余用证书里的第一个域名(泛域名也一样,*.example.com),
// 没有域名的用文件名。各台服务器上同一个域名的证书名字相同,线路才能跨服务器按名字选。
func certNameOf(info acme.CertInfo, certFile string) string {
	if info.SelfSigned {
		return SelfSignedName
	}
	if len(info.DNSNames) > 0 {
		// 先给泛域名:一张同时含 *.example.com 和 example.com 的证书,名字该是前者
		for _, d := range info.DNSNames {
			if acme.IsWildcard(d) {
				return strings.ToLower(d)
			}
		}
		return strings.ToLower(info.DNSNames[0])
	}
	if info.Subject != "" {
		return strings.ToLower(info.Subject)
	}
	base := filepath.Base(certFile)
	return strings.TrimSuffix(base, filepath.Ext(base))
}

// ensureCertStore 启动时把已有的证书收进库:设置里引用的、数据目录 cert/ 下成对的。老版本只有一张"当前证书",
// 文件名还可能和内容对不上(同一个路径被后签的域名覆盖过),所以名字一律从证书本身读。
func (r *Runner) ensureCertStore() {
	type pair struct{ cert, key, source string }
	var pairs []pair
	add := func(cert, key, source string) {
		cert, key = strings.TrimSpace(cert), strings.TrimSpace(key)
		if cert != "" && key != "" {
			pairs = append(pairs, pair{cert, key, source})
		}
	}
	add(r.setting("certFile"), r.setting("keyFile"), r.setting("certSource"))
	add(r.setting("webCertFile"), r.setting("webKeyFile"), "")
	add(r.setting("subCertFile"), r.setting("subKeyFile"), "")
	add(r.setting("resellerCertFile"), r.setting("resellerKeyFile"), "") // 代理面板的证书以前不进库,永远不续、不提醒
	if files, err := filepath.Glob(filepath.Join(r.DataDir(), "cert", "*.crt")); err == nil {
		sort.Strings(files)
		for _, f := range files {
			add(f, strings.TrimSuffix(f, ".crt")+".key", "")
		}
	}
	for _, p := range pairs {
		var n int64
		r.db.Model(&model.Cert{}).Where("cert_file = ?", p.cert).Count(&n)
		if n > 0 {
			continue
		}
		info := acme.Info(p.cert)
		if !info.Exists {
			continue
		}
		if _, err := os.Stat(p.key); err != nil {
			continue
		}
		row := model.Cert{Name: certNameOf(info, p.cert), Source: p.source, CertFile: p.cert, KeyFile: p.key, CreatedAt: time.Now().Unix()}
		if row.Source == "" {
			row.Source = guessSource(info, p.cert, r.DataDir())
		}
		if info.SelfSigned {
			row.Source = "selfsign"
		}
		if row.Source == "acme" {
			row.Method = r.guessMethod(row.Name)
			row.AutoRenew = !strings.EqualFold(r.setting("acmeAutoRenew"), "false") && row.Method != acme.MethodManual
			row.Staging = strings.EqualFold(r.setting("acmeStaging"), "true")
			if info.IsLetsEncrypt() { // 签发者看得出是不是测试环境,比表单上次的勾选准
				row.Staging = info.IsStaging()
			}
		}
		// 同名的已经有了(比如数据目录里还留着一份旧文件):留着库里那份,除非这份才是正在用的
		var same model.Cert
		if r.db.Where("name = ?", row.Name).First(&same).Error == nil {
			if p.cert == r.setting("certFile") {
				r.db.Model(&model.Cert{}).Where("id = ?", same.Id).Updates(map[string]interface{}{"cert_file": p.cert, "key_file": p.key})
			}
			continue
		}
		if err := r.db.Create(&row).Error; err != nil {
			logger.Warning("证书收进证书库失败: ", err)
		}
	}
	r.repairMisclassifiedLE()
	r.syncLocalCerts()
}

// guessSource 老文件没记来源:数据目录下、Let's Encrypt 签的算面板签发的(acme),其余算服务器上已有的。
// 认 Let's Encrypt 看签发者的组织与中间证书命名(acme.CertInfo.IsLetsEncrypt):以前只看 CN 里有没有 "encrypt",
// 现在的中间证书叫 R11、E6、YE1,全被当成了外部证书,从此不再自动续期(审计 I2)。
func guessSource(info acme.CertInfo, certFile, dataDir string) string {
	if info.IsLetsEncrypt() && underDir(certFile, dataDir) {
		return "acme"
	}
	return "external"
}

// underDir path 是不是在 dir 里面(按路径比,不是按字符串前缀:/etc/m-ui-old 不在 /etc/m-ui 里)。
func underDir(path, dir string) bool {
	rel, err := filepath.Rel(dir, path)
	return err == nil && rel != "." && rel != ".." && !strings.HasPrefix(rel, ".."+string(filepath.Separator)) && !filepath.IsAbs(rel)
}

// guessMethod 老证书没记验证方式:按签发表单上次用的。泛域名不能走 HTTP 验证:有 Cloudflare Token 就用它,否则只能手动 DNS。
func (r *Runner) guessMethod(name string) string {
	m := r.setting("acmeMethod")
	if m != acme.MethodCloudflare && m != acme.MethodManual {
		m = acme.MethodHTTP
	}
	if acme.IsWildcard(name) && m == acme.MethodHTTP {
		if r.setting("acmeCfToken") != "" {
			return acme.MethodCloudflare
		}
		return acme.MethodManual
	}
	return m
}

// certSourceRepairedKey 记下误判修复做过了(只做一次,之后管理员再改回去不再动)。
const certSourceRepairedKey = "certSourceRepaired"

// repairMisclassifiedLE 一次性修复:老版本收进库的、数据目录 cert/ 下面板自己签的 Let's Encrypt 证书,因为 guessSource
// 认不出现在的中间证书,被记成了"服务器上已有的证书"(审计 I2:签发者是 YE1 的就这样),从此不续期、没有「续期」按钮。
// 只改数据目录 cert/ 下、签发者认得出是 Let's Encrypt 的;数据目录外的(certbot 的原件)一律不动。
func (r *Runner) repairMisclassifiedLE() {
	if r.setting(certSourceRepairedKey) != "" {
		return
	}
	var rows []model.Cert
	if err := r.db.Where("source = ?", "external").Find(&rows).Error; err != nil {
		logger.Warning("检查证书来源失败: ", err)
		return
	}
	dir := filepath.Join(r.DataDir(), "cert")
	for _, c := range rows {
		info := acme.Info(c.CertFile)
		if !underDir(c.CertFile, dir) || !info.Exists || info.SelfSigned || !info.IsLetsEncrypt() {
			continue
		}
		method := r.guessMethod(c.Name)
		if err := r.db.Model(&model.Cert{}).Where("id = ?", c.Id).Updates(map[string]interface{}{
			"source": "acme", "method": method, "auto_renew": method != acme.MethodManual, "staging": info.IsStaging(),
		}).Error; err != nil {
			logger.Warning("修正证书 ", c.Name, " 的来源失败: ", err)
			return // 下次启动再试
		}
		logger.Info("证书 ", c.Name, " 是面板签发的 Let's Encrypt 证书(", c.CertFile, "),以前被误记成服务器上已有的证书,已改回并按 ", method, " 验证自动续期")
	}
	if err := r.setSetting(certSourceRepairedKey, "1"); err != nil {
		logger.Warning("记录证书来源修复失败: ", err)
	}
}

// storedPaths 这个名字的证书写到哪:库里有且是面板自己管的就写回原处(路径不变,续期不用重启数据面);
// 库里是服务器上已有的证书,原件不能动,改写面板自己的固定路径。
func (r *Runner) storedPaths(name string) (string, string) {
	var row model.Cert
	if r.db.Where("name = ?", name).First(&row).Error == nil && row.Source != "external" {
		return row.CertFile, row.KeyFile
	}
	return r.defaultCertPaths(name)
}

// saveCertRow 新建或更新库里这张证书。续期状态由 noteRenewResult 写;换成不归面板续的证书(外部 / 自签)时,
// 旧的续期失败记录一并清掉,免得证书页上一张外部证书还挂着"续期失败"。
func (r *Runner) saveCertRow(row model.Cert) error {
	var cur model.Cert
	if err := r.db.Where("name = ?", row.Name).First(&cur).Error; err == nil {
		row.Id, row.CreatedAt = cur.Id, cur.CreatedAt
		upd := map[string]interface{}{
			"source": row.Source, "cert_file": row.CertFile, "key_file": row.KeyFile,
			"method": row.Method, "auto_renew": row.AutoRenew, "staging": row.Staging,
		}
		if row.Source != "acme" {
			upd["last_renew_err"], upd["renew_fails"], upd["next_renew_at"] = "", 0, 0
		}
		return r.db.Model(&model.Cert{}).Where("id = ?", cur.Id).Updates(upd).Error
	}
	row.CreatedAt = time.Now().Unix()
	return r.db.Create(&row).Error
}

// Certs 证书库列表:正在用的排前面,其余按名字。每张带上续期情况:能不能自动续、为什么、什么时候续、上次结果。
func (r *Runner) Certs() []CertView {
	var rows []model.Cert
	r.db.Order("name asc").Find(&rows)
	byName := r.linesByCert()
	webCert, subCert, resCert := r.setting("webCertFile"), r.setting("subCertFile"), r.setting("resellerCertFile")
	defCert := r.setting("certFile")
	now := time.Now()
	port80, port80Read := "", false
	out := make([]CertView, 0, len(rows))
	for _, c := range rows {
		info := acme.Info(c.CertFile)
		v := CertView{Cert: c, Info: info, ForLines: c.CertFile == defCert, ForPanel: c.CertFile == webCert, ForSub: c.CertFile == subCert,
			ForReseller: resCert != "" && c.CertFile == resCert, LineNames: byName[c.Name], RenewState: renewStateOf(c, info, now)}
		if v.LineNames == nil {
			v.LineNames = []string{}
		}
		// HTTP 验证自动续的:80 端口被本机线路占着,到续期时必然失败 —— 现在就说
		if v.Renewable && (c.Method == acme.MethodHTTP || c.Method == "") && !acme.IsWildcard(c.Name) {
			if !port80Read {
				port80, port80Read = r.port80Line(), true
			}
			if port80 != "" {
				v.Warn = errPort80Line(port80).Error()
			}
		}
		out = append(out, v)
	}
	sort.SliceStable(out, func(i, j int) bool {
		return certRank(out[i]) < certRank(out[j])
	})
	return out
}

func certRank(v CertView) int {
	if v.ForLines || v.ForPanel || v.ForSub || v.ForReseller || len(v.LineNames) > 0 {
		return 0
	}
	return 1
}

// linesByCert 证书名 → 单独选了它的线路。
func (r *Runner) linesByCert() map[string][]string {
	var lines []model.Line
	r.db.Select("id, name, tls").Find(&lines)
	out := map[string][]string{}
	for _, l := range lines {
		if c := render.ParseTLS(l).Cert; c != "" {
			out[c] = append(out[c], l.Name)
		}
	}
	return out
}

// namedCerts 给渲染用的证书库:名字 → 文件路径。文件不在了的不算(线路退回默认证书,不让整份配置起不来)。
func (r *Runner) namedCerts() map[string]render.NodeCert {
	var rows []model.Cert
	if r.db.Find(&rows).Error != nil {
		return nil
	}
	out := make(map[string]render.NodeCert, len(rows))
	for _, c := range rows {
		if _, err := os.Stat(c.CertFile); err != nil {
			continue
		}
		if _, err := os.Stat(c.KeyFile); err != nil {
			continue
		}
		name := acme.BaseDomain(c.Name)
		if c.Name == SelfSignedName {
			name = ""
		}
		out[c.Name] = render.NodeCert{ServerName: name, CertPath: c.CertFile, KeyPath: c.KeyFile}
	}
	return out
}

// CertBriefs 这台服务器的证书清单,报给主机(线路编辑框、订阅 SNI 用;续期状态让主机代副机告警)。
func (r *Runner) CertBriefs() []model.CertBrief {
	var rows []model.Cert
	r.db.Order("name asc").Find(&rows)
	def := r.setting("certFile")
	inUse := r.certsInUse()
	now := time.Now()
	out := make([]model.CertBrief, 0, len(rows))
	for _, c := range rows {
		info := acme.Info(c.CertFile)
		if !info.Exists {
			continue
		}
		st := renewStateOf(c, info, now)
		b := model.CertBrief{
			Name: c.Name, Domains: info.DNSNames, SelfSigned: info.SelfSigned,
			NotAfter: info.NotAfter.Unix(), Default: c.CertFile == def,
			Source: c.Source, Method: c.Method, AutoRenew: st.Renewable, RenewBefore: st.RenewBefore, InUse: inUse[c.Name],
		}
		if st.Renewable && c.RenewFails > 0 {
			b.RenewErr, b.RenewFails, b.NextRenewAt = c.LastRenewErr, c.RenewFails, c.NextRenewAt
		}
		out = append(out, b)
	}
	return out
}

// syncLocalCerts 把本机的证书清单写进本机的服务器记录(主机自己的;副机的随报告上来)。
func (r *Runner) syncLocalCerts() {
	b, err := json.Marshal(r.CertBriefs())
	if err != nil {
		return
	}
	if err := r.db.Model(&model.Node{}).Where("is_local = ?", true).Update("certs", json.RawMessage(b)).Error; err != nil {
		logger.Warning("记录本机证书清单失败: ", err)
	}
}

// UseCert 把库里的一张证书用到某个用途;面板 / 订阅的 name 为空 = 不用 HTTPS。
func (r *Runner) UseCert(name, target string) error {
	r.certOpMu.Lock()
	defer r.certOpMu.Unlock()
	var row model.Cert
	if name != "" {
		if err := r.db.Where("name = ?", name).First(&row).Error; err != nil {
			return errors.New("证书库里没有这张证书")
		}
		info := acme.Info(row.CertFile)
		if !info.Exists {
			return errors.New("证书文件读不出来:" + info.Error)
		}
		if err := certutil.Verify(row.CertFile, row.KeyFile); err != nil {
			return err
		}
		// 和签发时一样:面板域名还空着就取这张证书的域名,否则面板和订阅地址还是 IP,证书对不上
		if row.Source != "selfsign" && !acme.IsWildcard(name) && r.setting("webDomain") == "" {
			if err := r.setSetting("webDomain", name); err != nil {
				return fmt.Errorf("保存证书设置失败: %w", err)
			}
		}
	}
	switch target {
	case CertForLines:
		if name == "" {
			return errors.New("线路默认证书不能为空")
		}
		values := map[string]string{"certFile": row.CertFile, "keyFile": row.KeyFile, "certSource": row.Source}
		if row.Source == "acme" {
			values["acmeDomain"] = row.Name
		}
		if err := saveSettings(r.db, values); err != nil {
			return fmt.Errorf("保存证书设置失败: %w", err)
		}
		r.syncLocalCerts()
		if err := r.ReloadAll(); err != nil {
			return fmt.Errorf("证书设置已保存，数据面重载失败: %w", err)
		}
		r.cert.logf("线路默认证书已换成 %s", certLabel(row))
		return nil
	case CertForPanel:
		if err := saveSettings(r.db, map[string]string{"webCertFile": row.CertFile, "webKeyFile": row.KeyFile}); err != nil {
			return fmt.Errorf("保存证书设置失败: %w", err)
		}
		r.cert.logf("面板 HTTPS 已设为 %s,重启 m-ui 后生效", certLabel(row))
		return nil
	case CertForSub:
		if err := saveSettings(r.db, map[string]string{"subCertFile": row.CertFile, "subKeyFile": row.KeyFile, "subRestartPending": "true"}); err != nil {
			return fmt.Errorf("保存证书设置失败: %w", err)
		}
		if err := r.RestartSub(); err != nil {
			return fmt.Errorf("证书设置已保存，重启订阅服务失败: %w", err)
		}
		if err := r.setSetting("subRestartPending", ""); err != nil {
			return err
		}
		r.cert.logf("订阅 HTTPS 已设为 %s", certLabel(row))
		return nil
	}
	return fmt.Errorf("未知的证书用途 %q", target)
}

func certLabel(c model.Cert) string {
	if c.Name == "" {
		return "不用 HTTPS"
	}
	return c.Name
}

// DeleteCert 从库里删掉一张证书。还在用的不让删;面板自己存的文件一并删,服务器上已有的证书只是不再记,原件不动。
func (r *Runner) DeleteCert(name string) error {
	r.certOpMu.Lock()
	defer r.certOpMu.Unlock()
	var row model.Cert
	if err := r.db.Where("name = ?", name).First(&row).Error; err != nil {
		return errors.New("证书库里没有这张证书")
	}
	switch row.CertFile {
	case r.setting("certFile"):
		return errors.New("这张证书是线路默认证书,先换一张再删")
	case r.setting("webCertFile"):
		return errors.New("面板 HTTPS 正在用这张证书,先换一张再删")
	case r.setting("subCertFile"):
		return errors.New("订阅 HTTPS 正在用这张证书,先换一张再删")
	case r.setting("resellerCertFile"):
		return errors.New("代理面板 HTTPS 正在用这张证书,先换一张再删")
	}
	if names := r.linesByCert()[name]; len(names) > 0 {
		return fmt.Errorf("线路「%s」单独选了这张证书,先改掉再删", strings.Join(names, "、"))
	}
	if err := r.db.Delete(&model.Cert{}, row.Id).Error; err != nil {
		return err
	}
	if row.Source != "external" && strings.HasPrefix(row.CertFile, filepath.Join(r.DataDir(), "cert")) {
		_ = os.Remove(row.CertFile)
		_ = os.Remove(row.KeyFile)
	}
	r.syncLocalCerts()
	r.notifier.Forget("cert:" + name) // 这张的续期 / 到期提醒记录不用留了
	r.cert.logf("已删除证书 %s", name)
	return nil
}

// relayLoop 线路当上游时,服务器的公网 IP、域名、证书变了要跟着改:这些变化不一定经过面板操作,所以定时对一遍。
// 只有主机算(副机的上游随快照下发),没有线路上游时只是一条查询。
func (r *Runner) relayLoop(stop <-chan struct{}) {
	t := time.NewTicker(time.Minute)
	defer t.Stop()
	for {
		select {
		case <-t.C:
			if strings.EqualFold(r.setting("nodeMode"), "true") {
				continue
			}
			changed, err := sub.SyncRelayUpstreams(r.db)
			if err != nil {
				logger.Warning("更新线路上游失败: ", err)
			}
			if changed {
				if err := r.ReloadUpstreams(); err != nil {
					logger.Warning("更新线路上游后重载失败: ", err)
				}
			}
		case <-stop:
			return
		}
	}
}
