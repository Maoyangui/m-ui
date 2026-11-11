package ha

import (
	"archive/zip"
	"crypto/x509"
	"encoding/pem"
	"fmt"
	"net"
	"os"
	"path/filepath"
	"strconv"
	"strings"
	"time"

	"github.com/Maoyangui/m-ui/database/model"

	"gorm.io/gorm"
)

// 主机身份:随主机一起迁移的那几项本机设置 —— 订阅地址(域名、端口、路径)和订阅用的证书。
// 用户手里的订阅地址是"主机的订阅域名(没有就面板域名、再没有就公网 IP):订阅端口/订阅路径/令牌",
// 接任或设为主机后新主机照用同一个域名、端口、路径,DNS 指过来以后订阅地址不变。面板、代理面板的地址用新主机自己的,
// 完成后在页面上和 Telegram 里给出新地址。
//
// 另外带上原主机"作为副机时别人该怎么连"(API 地址、副机令牌、证书指纹):新主机凭它把原主机当副机管起来。

// Identity 主机身份(副本、设为主机的数据里都有)。
type Identity struct {
	MasterId    uint         `json:"masterId"`
	MasterName  string       `json:"masterName"`
	SubHost     string       `json:"subHost"` // 订阅地址里的主机名
	SubPort     int          `json:"subPort"`
	SubPath     string       `json:"subPath"`
	SubCert     *CarriedCert `json:"subCert,omitempty"` // 订阅走 https 时用的证书(文件在 zip 的 identity/ 下)
	AcmeCfToken string       `json:"acmeCfToken,omitempty"`
	Self        Link         `json:"self"` // 这台主机作为副机时别人该怎么连
	PublicIP    string       `json:"publicIp,omitempty"`
	PublicIP6   string       `json:"publicIp6,omitempty"`
	PanelURL    string       `json:"panelUrl,omitempty"`
}

// CarriedCert 随主机身份带过去的证书:证书库里的那一行 + zip 里的文件名。
type CarriedCert struct {
	Name      string `json:"name"`
	Source    string `json:"source"`
	Method    string `json:"method,omitempty"`
	AutoRenew bool   `json:"autoRenew,omitempty"`
	Staging   bool   `json:"staging,omitempty"`
	CertZip   string `json:"certZip"`
	KeyZip    string `json:"keyZip"`
}

// subHost 订阅地址里的主机名:订阅域名 → 面板域名 → 公网 IP(和 runner.PublicHost 一样的顺序)。
func (s *Service) subHost() string {
	for _, k := range []string{"subDomain", "webDomain", "publicIp"} {
		if v := strings.TrimSpace(s.setting(k)); v != "" {
			return v
		}
	}
	return ""
}

// isIPHost 主机名是不是 IP(订阅地址用的是 IP 时,换主机后订阅地址一定会变)。
func isIPHost(h string) bool { return net.ParseIP(strings.Trim(h, "[]")) != nil }

// captureIdentity 记下本机(主机)的身份,订阅证书文件写进 zip。
func (s *Service) captureIdentity(zw *zip.Writer) (Identity, error) {
	id := Identity{MasterId: s.SelfId(), MasterName: s.selfName(), SubHost: s.subHost(),
		SubPort: s.settingInt("subPort", 2056, 1, 65535), SubPath: s.setting("subPath"),
		Self: s.selfLink(), PublicIP: s.setting("publicIp"), PublicIP6: s.setting("publicIp6"), PanelURL: s.panelURL()}
	if strings.Contains(id.PublicIP, ":") && id.PublicIP6 == "" {
		id.PublicIP6, id.PublicIP = id.PublicIP, ""
	}
	certFile, keyFile := strings.TrimSpace(s.setting("subCertFile")), strings.TrimSpace(s.setting("subKeyFile"))
	if certFile == "" || keyFile == "" {
		return id, nil
	}
	cb, err1 := os.ReadFile(certFile)
	kb, err2 := os.ReadFile(keyFile)
	if err1 != nil || err2 != nil {
		return id, nil // 证书文件读不到不挡副本:新主机订阅照常用自己的证书
	}
	cc := &CarriedCert{Source: "external", CertZip: "identity/sub.crt", KeyZip: "identity/sub.key"}
	var row model.Cert
	if s.d.DB.Where("cert_file = ?", certFile).Limit(1).Find(&row).Error == nil && row.Id != 0 {
		cc.Name, cc.Source, cc.Method, cc.AutoRenew, cc.Staging = row.Name, row.Source, row.Method, row.AutoRenew, row.Staging
	}
	if cc.Name == "" {
		cc.Name = strings.TrimSuffix(filepath.Base(certFile), filepath.Ext(certFile))
	}
	if cc.Source == "acme" && cc.Method == "cloudflare" {
		id.AcmeCfToken = s.setting("acmeCfToken")
	}
	if err := zipAddBytes(zw, cc.CertZip, cb); err != nil {
		return id, err
	}
	if err := zipAddBytes(zw, cc.KeyZip, kb); err != nil {
		return id, err
	}
	id.SubCert = cc
	return id, nil
}

// identityResult 新主机套用主机身份的结果(写进通知、面板)。告警是代码加参数(界面按 Code 翻译,Telegram 用 identityWarnText)。
type identityResult struct {
	SubURLKept  bool       `json:"subUrlKept"` // 用户的订阅地址不变(DNS 指过来以后)
	SubHost     string     `json:"subHost"`
	Warnings    []PlanItem `json:"warnings,omitempty"`
	PrevSubPort int        `json:"prevSubPort,omitempty"` // 新主机自己原来的订阅端口(迁来的端口启动时绑不上,改回用它)
}

// 身份告警的代码(界面 ha.idw.*)。
const (
	idwSubPortLine = "subPortLine" // 迁来的订阅端口被本机的线路占着:{port, line, own}
	idwSubPortSvc  = "subPortSvc"  // 迁来的订阅端口和本机的面板 / 代理面板 / WARP 端口相同:{port, svc, own}
	idwSubIp       = "subIp"       // 原主机的订阅地址用的是 IP:{host}
	idwSubHttps    = "subHttps"    // 原主机的订阅是 http、本机是 https
	idwSubCertFail = "subCertFail" // 订阅证书没能装上:{err}
	idwAcmeZone    = "acmeZone"    // 本机已有别的签证书令牌,订阅证书续期要它能管那个域名:{name}
	idwSubPortBind = "subPortBind" // 启动时迁来的订阅端口绑不上,改回本机原来的:{port, own, err}
)

// svcText 服务端口的称呼。
func svcText(svc string) string {
	switch svc {
	case "panel":
		return "面板"
	case "reseller":
		return "代理面板"
	case "warp":
		return "WARP"
	}
	return svc
}

// identityWarnText 身份告警的中文(Telegram、命令行用;界面按 Code 翻译)。
func identityWarnText(it PlanItem) string {
	a := it.Args
	switch it.Code {
	case idwSubPortLine:
		return "订阅端口 " + a["port"] + " 在本机被线路「" + a["line"] + "」占用,订阅仍用本机的端口 " + a["own"] + ":用户的订阅地址会变"
	case idwSubPortSvc:
		return "订阅端口 " + a["port"] + " 和本机的" + svcText(a["svc"]) + "端口相同,订阅仍用本机的端口 " + a["own"] + ":用户的订阅地址会变"
	case idwSubIp:
		return "原主机的订阅地址用的是 IP(" + a["host"] + "):换主机后订阅地址会变,用户要重新导入订阅。以后建议给订阅配一个域名"
	case idwSubHttps:
		return "原主机的订阅是 http,本机的订阅是 https:订阅地址的开头会从 http 变成 https"
	case idwSubCertFail:
		return "订阅证书没能装上(" + a["err"] + "),订阅仍用本机原来的证书"
	case idwAcmeZone:
		return "订阅证书 " + a["name"] + " 靠 Cloudflare 续期,而本机已经有自己的签证书令牌(没换成原主机的):确认这个令牌能管 " + a["name"] + " 所在的区域,否则续期会失败"
	case idwSubPortBind:
		return "订阅端口 " + a["port"] + " 在本机绑不上(" + a["err"] + "),订阅改回本机原来的端口 " + a["own"] + ":用户的订阅地址会变"
	}
	return a["text"]
}

// applyIdentity 在要接任的库里套用原主机的身份(订阅端口、路径、域名、证书)。selfId 是新主机自己的服务器 id,
// extracted 是解出来的 zip 目录。证书写到新主机数据目录 cert/ 下(同名的已有就用已有的)。
func applyIdentity(tx *gorm.DB, id Identity, selfId uint, extracted, dataDir string) (identityResult, error) {
	res := identityResult{SubHost: id.SubHost}
	set := func(k, v string) error { return upsert(tx, map[string]string{k: v}) }
	get := func(k string) string {
		var v string
		tx.Raw("SELECT value FROM settings WHERE key = ?", k).Scan(&v)
		return v
	}
	// 订阅端口:迁来的端口在新主机上被它自己的面板、代理面板、WARP 或线路占着,留新主机自己的(订阅地址会变)。
	// 设为主机的确认框按同一个规则提前说(subPortClash),说的和做的一致;绝不能因此让新主机起不来
	own := atoiDef(get("subPort"), 2056)
	res.PrevSubPort = own
	port := id.SubPort
	if port > 0 && port != own {
		tp := targetPorts{web: atoiDef(get("webPort"), 2053)}
		if !strings.EqualFold(get("resellerEnabled"), "false") {
			tp.reseller = atoiDef(get("resellerPort"), 2054)
		}
		if strings.TrimSpace(get("warpPort")) != "" {
			tp.warp = atoiDef(get("warpPort"), 40000)
		}
		var lines []model.Line
		tx.Where("enabled = ?", true).Find(&lines)
		if svc, line := subPortClash(port, tp, lines, selfId); svc != "" {
			res.Warnings = append(res.Warnings, item(idwSubPortSvc, "port", strconv.Itoa(port), "svc", svc, "own", strconv.Itoa(own)))
			port = 0
		} else if line != "" {
			res.Warnings = append(res.Warnings, item(idwSubPortLine, "port", strconv.Itoa(port), "line", line, "own", strconv.Itoa(own)))
			port = 0
		}
	}
	if port > 0 {
		if err := set("subPort", strconv.Itoa(port)); err != nil {
			return res, err
		}
	}
	if err := set("subPath", id.SubPath); err != nil {
		return res, err
	}
	switch {
	case id.SubHost == "":
	case isIPHost(id.SubHost):
		res.Warnings = append(res.Warnings, item(idwSubIp, "host", id.SubHost))
	default:
		if err := set("subDomain", id.SubHost); err != nil {
			return res, err
		}
	}
	res.SubURLKept = port > 0 && id.SubHost != "" && !isIPHost(id.SubHost)
	if id.SubCert == nil {
		if get("subCertFile") != "" && res.SubURLKept {
			res.Warnings = append(res.Warnings, item(idwSubHttps))
			res.SubURLKept = false
		}
		return res, nil
	}
	cc := id.SubCert
	certPath, keyPath, err := placeCert(tx, cc, extracted, dataDir)
	if err != nil {
		res.Warnings = append(res.Warnings, item(idwSubCertFail, "err", err.Error()))
		return res, nil
	}
	if err := set("subCertFile", certPath); err != nil {
		return res, err
	}
	if err := set("subKeyFile", keyPath); err != nil {
		return res, err
	}
	if id.AcmeCfToken != "" {
		switch mine := strings.TrimSpace(get("acmeCfToken")); {
		case mine == "":
			if err := set("acmeCfToken", id.AcmeCfToken); err != nil { // 证书靠 Cloudflare 续期:续期要用
				return res, err
			}
		case mine != strings.TrimSpace(id.AcmeCfToken):
			// 本机的令牌签着本机自己的证书,不能换掉;续期用的是它,看不到订阅域名所在的区域就续不上:提前说
			res.Warnings = append(res.Warnings, item(idwAcmeZone, "name", nonEmpty(cc.Name, id.SubHost)))
		}
	}
	return res, nil
}

// placeCert 把带过来的证书放进新主机的证书库:同名的证书新主机已经有了、没过期、也不比带来的早到期,就用它自己的
// (各机各签,同一个域名);否则写到数据目录 cert/ 下(不覆盖已有文件),那一行改指新文件、续期退避清零,续期方式照原主机的。
func placeCert(tx *gorm.DB, cc *CarriedCert, extracted, dataDir string) (string, string, error) {
	cb, err := os.ReadFile(filepath.Join(extracted, filepath.FromSlash(cc.CertZip)))
	if err != nil {
		return "", "", err
	}
	kb, err := os.ReadFile(filepath.Join(extracted, filepath.FromSlash(cc.KeyZip)))
	if err != nil {
		return "", "", err
	}
	var row model.Cert
	if tx.Where("name = ?", cc.Name).Limit(1).Find(&row).Error == nil && row.Id != 0 {
		if _, err := os.Stat(row.CertFile); err == nil {
			if _, err := os.Stat(row.KeyFile); err == nil {
				mine, _ := certFileNotAfter(row.CertFile)
				theirs, terr := pemNotAfter(cb)
				if terr != nil || !mine.Before(theirs) { // 本机那张不比带来的早到期(过期的到期时间更早,自然被换掉)
					return row.CertFile, row.KeyFile, nil
				}
			}
		}
	}
	dir := filepath.Join(dataDir, "cert")
	if err := os.MkdirAll(dir, 0o700); err != nil {
		return "", "", err
	}
	base := strings.NewReplacer("/", "", "\\", "", ":", "", "..", "", "*", "_").Replace(cc.Name)
	if base == "" {
		base = "sub"
	}
	certPath, keyPath := filepath.Join(dir, base+".crt"), filepath.Join(dir, base+".key")
	for i := 0; ; i++ { // 文件名撞了(内容是别的证书,或本机那张过期的):换个名字,不覆盖
		if _, err := os.Stat(certPath); err != nil {
			break
		}
		suf := "-master"
		if i > 0 {
			suf += "-" + strconv.Itoa(i)
		}
		certPath, keyPath = filepath.Join(dir, base+suf+".crt"), filepath.Join(dir, base+suf+".key")
	}
	if err := writeFileAtomic(certPath, cb); err != nil {
		return "", "", err
	}
	if err := writeFileAtomic(keyPath, kb); err != nil {
		return "", "", err
	}
	src := cc.Source
	if src == "" {
		src = "external"
	}
	nr := model.Cert{Name: cc.Name, Source: src, CertFile: certPath, KeyFile: keyPath, Method: cc.Method, AutoRenew: cc.AutoRenew,
		Staging: cc.Staging, CreatedAt: nowUnix()}
	if row.Id != 0 {
		return certPath, keyPath, tx.Model(&model.Cert{}).Where("id = ?", row.Id).Updates(map[string]interface{}{
			"source": src, "cert_file": certPath, "key_file": keyPath, "method": cc.Method, "auto_renew": cc.AutoRenew, "staging": cc.Staging,
			"renew_fails": 0, "next_renew_at": 0, "last_renew_err": ""}).Error // 换了一张新证书:旧的续期退避不再算数
	}
	if err := tx.Create(&nr).Error; err != nil {
		return "", "", err
	}
	if !cc.AutoRenew { // gorm 的 default:true 会吞掉 false
		tx.Model(&model.Cert{}).Where("id = ?", nr.Id).Update("auto_renew", false)
	}
	return certPath, keyPath, nil
}

// applyFormerRow 新库里原主机那一行:填上它作为副机时怎么连(地址用它的公网 IP 时按证书指纹钉);那一行的域名要是随主机身份
// 迁移的名字(订阅主机名、要跟着主机走的域名),清空 —— 那个名字以后指向新主机,原主机线路的用户不能被指过去,
// 订阅里按它的 IP 出。接任时先停用(失联期间不进订阅),记下等它回来。
func (s *Service) applyFormerRow(tx *gorm.DB, formerId, selfId uint, id Identity, how string) error {
	if formerId == 0 || formerId == selfId {
		return nil
	}
	up := map[string]interface{}{"is_local": false}
	if l := id.Self; l.ApiUrl != "" && l.Token != "" {
		up["api_url"], up["token"], up["cert_fp"] = l.ApiUrl, l.Token, l.CertFP
		up["insecure"] = strings.HasPrefix(strings.ToLower(l.ApiUrl), "https://")
	}
	var row model.Node
	if tx.Select("id, domain").Where("id = ?", formerId).Limit(1).Find(&row).Error == nil && row.Id != 0 {
		var dnsNames string
		tx.Raw("SELECT value FROM settings WHERE key = ?", KeyDnsNames).Scan(&dnsNames)
		if movingName(row.Domain, id.SubHost, splitNames(dnsNames)) {
			up["domain"] = ""
		}
	}
	if how == HowTakeover {
		up["enabled"] = false // 失联期间不进订阅;它回来自动变为副机时再启用
	}
	if err := tx.Model(&model.Node{}).Where("id = ?", formerId).Updates(up).Error; err != nil {
		return err
	}
	if how == HowTakeover {
		if err := upsert(tx, map[string]string{KeyFormer: mustJSON(Former{Id: formerId, Name: id.MasterName, Since: s.now().Unix()})}); err != nil {
			return err
		}
	}
	return nil
}

// certFileNotAfter / pemNotAfter 证书(第一张)的到期时间。
func certFileNotAfter(path string) (time.Time, error) {
	b, err := os.ReadFile(path)
	if err != nil {
		return time.Time{}, err
	}
	return pemNotAfter(b)
}

func pemNotAfter(b []byte) (time.Time, error) {
	for {
		var blk *pem.Block
		blk, b = pem.Decode(b)
		if blk == nil {
			return time.Time{}, fmt.Errorf("证书文件里没有证书")
		}
		if blk.Type == "CERTIFICATE" {
			c, err := x509.ParseCertificate(blk.Bytes)
			if err != nil {
				return time.Time{}, err
			}
			return c.NotAfter, nil
		}
	}
}

func atoiDef(s string, def int) int {
	n, err := strconv.Atoi(strings.TrimSpace(s))
	if err != nil {
		return def
	}
	return n
}

func nonEmpty(v, def string) string {
	if strings.TrimSpace(v) == "" {
		return def
	}
	return v
}

// SubPortFallback 刚换上新库(成为主机后第一次启动、还没发通知)时迁来的订阅端口绑不上(本机别的程序占着):
// 返回本机原来的订阅端口,runner 改用它再起一次。别的时候不兜底(订阅端口是管理员自己设的,照常报错)。
func (s *Service) SubPortFallback() (int, bool) {
	if s.IsNode() {
		return 0, false
	}
	var b Became
	if !s.getJSON(KeyBecame, &b) || b.Notified {
		return 0, false
	}
	p := b.Identity.PrevSubPort
	if p <= 0 || p > 65535 || p == s.settingInt("subPort", 2056, 1, 65535) {
		return 0, false
	}
	return p, true
}

// NoteSubPortFallback 订阅端口改回本机原来的:写设置,记进成为主机的告警(随后的通知、顶部提示里会说订阅地址变了)。
func (s *Service) NoteSubPortFallback(from, to int, cause error) error {
	var b Became
	s.getJSON(KeyBecame, &b)
	b.Identity.SubURLKept = false
	msg := ""
	if cause != nil {
		msg = cause.Error()
	}
	b.Identity.Warnings = append(b.Identity.Warnings, item(idwSubPortBind, "port", strconv.Itoa(from), "own", strconv.Itoa(to), "err", msg))
	return s.set(map[string]string{"subPort": strconv.Itoa(to), KeyBecame: mustJSON(b)})
}
