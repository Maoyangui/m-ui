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

// CertView 证书库的一行:库里记的 + 证书文件读出来的 + 正用在哪。
type CertView struct {
	model.Cert
	Info      acme.CertInfo `json:"info"`
	ForLines  bool          `json:"forLines"`
	ForPanel  bool          `json:"forPanel"`
	ForSub    bool          `json:"forSub"`
	LineNames []string      `json:"lineNames"` // 单独选了这张证书的线路
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
			row.Method = r.setting("acmeMethod")
			if row.Method == "" {
				row.Method = acme.MethodHTTP
			}
			row.AutoRenew = !strings.EqualFold(r.setting("acmeAutoRenew"), "false")
			row.Staging = strings.EqualFold(r.setting("acmeStaging"), "true")
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
	r.syncLocalCerts()
}

// guessSource 老文件没记来源:Let's Encrypt 签的算 acme,数据目录外的算服务器上已有的。
func guessSource(info acme.CertInfo, certFile, dataDir string) string {
	if strings.Contains(strings.ToLower(info.Issuer), "encrypt") && strings.HasPrefix(certFile, dataDir) {
		return "acme"
	}
	return "external"
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

// saveCertRow 新建或更新库里这张证书。
func (r *Runner) saveCertRow(row model.Cert) error {
	var cur model.Cert
	if err := r.db.Where("name = ?", row.Name).First(&cur).Error; err == nil {
		row.Id, row.CreatedAt = cur.Id, cur.CreatedAt
		return r.db.Model(&model.Cert{}).Where("id = ?", cur.Id).Updates(map[string]interface{}{
			"source": row.Source, "cert_file": row.CertFile, "key_file": row.KeyFile,
			"method": row.Method, "auto_renew": row.AutoRenew, "staging": row.Staging,
		}).Error
	}
	row.CreatedAt = time.Now().Unix()
	return r.db.Create(&row).Error
}

// Certs 证书库列表:正在用的排前面,其余按名字。
func (r *Runner) Certs() []CertView {
	var rows []model.Cert
	r.db.Order("name asc").Find(&rows)
	byName := r.linesByCert()
	webCert, subCert := r.setting("webCertFile"), r.setting("subCertFile")
	defCert := r.setting("certFile")
	out := make([]CertView, 0, len(rows))
	for _, c := range rows {
		v := CertView{Cert: c, Info: acme.Info(c.CertFile), ForLines: c.CertFile == defCert, ForPanel: c.CertFile == webCert, ForSub: c.CertFile == subCert, LineNames: byName[c.Name]}
		if v.LineNames == nil {
			v.LineNames = []string{}
		}
		out = append(out, v)
	}
	sort.SliceStable(out, func(i, j int) bool {
		return certRank(out[i]) < certRank(out[j])
	})
	return out
}

func certRank(v CertView) int {
	if v.ForLines || v.ForPanel || v.ForSub || len(v.LineNames) > 0 {
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

// CertBriefs 这台服务器的证书清单,报给主机(线路编辑框、订阅 SNI 用)。
func (r *Runner) CertBriefs() []model.CertBrief {
	var rows []model.Cert
	r.db.Order("name asc").Find(&rows)
	def := r.setting("certFile")
	out := make([]model.CertBrief, 0, len(rows))
	for _, c := range rows {
		info := acme.Info(c.CertFile)
		if !info.Exists {
			continue
		}
		out = append(out, model.CertBrief{
			Name: c.Name, Domains: info.DNSNames, SelfSigned: info.SelfSigned,
			NotAfter: info.NotAfter.Unix(), Default: c.CertFile == def,
		})
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
	r.cert.logf("已删除证书 %s", name)
	return nil
}

// renewDue 自动续期:库里到期前 30 天内的 ACME 证书一张张来。手动 DNS 的续不了,提前 14 天提醒。
func (r *Runner) renewDue() {
	var rows []model.Cert
	if r.db.Where("source = ?", "acme").Find(&rows).Error != nil {
		return
	}
	for _, c := range rows {
		info := acme.Info(c.CertFile)
		if !info.Exists || info.SelfSigned {
			continue
		}
		if c.Method == acme.MethodManual {
			if info.DaysLeft <= 14 && r.notifier.Once("cert-manual:"+c.Name, 20*time.Hour) {
				r.cert.logf("证书 %s 剩 %d 天:手动 DNS 验证的证书不能自动续期,请重新申请", c.Name, info.DaysLeft)
				r.notifier.Event("tgOnCert", fmt.Sprintf("🟡 <b>证书快到期</b>:%s 还剩 %d 天。它用手动 DNS 验证,不能自动续期,请到证书页重新申请", c.Name, info.DaysLeft))
			}
			continue
		}
		if !c.AutoRenew || info.DaysLeft > 30 || !r.notifier.Once("cert-renew:"+c.Name, 20*time.Hour) {
			continue
		}
		logger.Info("证书 ", c.Name, " 剩余 ", info.DaysLeft, " 天,自动续期")
		r.cert.logf("自动续期 %s:剩余 %d 天", c.Name, info.DaysLeft)
		if err := r.runIssue(IssueRequest{Domain: c.Name, Method: c.Method, Staging: c.Staging, AutoRenew: c.AutoRenew}); err != nil {
			logger.Warning("证书 ", c.Name, " 自动续期失败: ", err)
		}
	}
}
