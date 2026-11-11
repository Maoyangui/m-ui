package ha

import (
	"archive/zip"
	"fmt"
	"net"
	"os"
	"path/filepath"
	"strconv"
	"strings"

	"github.com/Maoyangui/m-ui/database/model"
	"github.com/Maoyangui/m-ui/render"

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

// identityResult 新主机套用主机身份的结果(写进通知、面板)。
type identityResult struct {
	SubURLKept  bool       `json:"subUrlKept"` // 用户的订阅地址不变(DNS 指过来以后)
	SubHost     string     `json:"subHost"`
	Warnings    []PlanItem `json:"warnings,omitempty"`
	PrevSubPort int        `json:"prevSubPort,omitempty"`
}

// identityWarnText 身份告警的中文(Telegram 用;界面按 Code 翻译)。
func identityWarnText(it PlanItem) string { return it.Args["text"] }

func textItem(t string) PlanItem { return PlanItem{Code: "text", Args: map[string]string{"text": t}} }

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
	// 订阅端口:新主机上要是被线路占了,留新主机自己的(订阅地址会变)
	port := id.SubPort
	if port > 0 && port != atoiDef(get("subPort"), 2056) {
		var lines []model.Line
		tx.Where("enabled = ?", true).Find(&lines)
		for _, l := range lines {
			if l.Port == port && !render.UDPOnly(l.Protocol) && render.LineOnNode(l, selfId) {
				res.Warnings = append(res.Warnings, textItem(fmt.Sprintf("订阅端口 %d 在本机被线路「%s」占用,订阅仍用本机的端口 %s:用户的订阅地址会变", port, l.Name, nonEmpty(get("subPort"), "2056"))))
				port = 0
				break
			}
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
		res.Warnings = append(res.Warnings, textItem(fmt.Sprintf("原主机的订阅地址用的是 IP(%s):换主机后订阅地址会变,用户要重新导入订阅。以后建议给订阅配一个域名", id.SubHost)))
	default:
		if err := set("subDomain", id.SubHost); err != nil {
			return res, err
		}
	}
	res.SubURLKept = port > 0 && id.SubHost != "" && !isIPHost(id.SubHost)
	if id.SubCert == nil {
		if get("subCertFile") != "" && res.SubURLKept {
			res.Warnings = append(res.Warnings, textItem("原主机的订阅是 http,本机的订阅是 https:订阅地址的开头会从 http 变成 https"))
			res.SubURLKept = false
		}
		return res, nil
	}
	cc := id.SubCert
	certPath, keyPath, err := placeCert(tx, cc, extracted, dataDir)
	if err != nil {
		res.Warnings = append(res.Warnings, textItem("订阅证书没能装上("+err.Error()+"),订阅仍用本机原来的证书"))
		return res, nil
	}
	if err := set("subCertFile", certPath); err != nil {
		return res, err
	}
	if err := set("subKeyFile", keyPath); err != nil {
		return res, err
	}
	if id.AcmeCfToken != "" && strings.TrimSpace(get("acmeCfToken")) == "" {
		if err := set("acmeCfToken", id.AcmeCfToken); err != nil { // 证书靠 Cloudflare 续期:续期要用
			return res, err
		}
	}
	return res, nil
}

// placeCert 把带过来的证书放进新主机的证书库:同名的证书新主机已经有了就用它自己的(各机各签,同一个域名);
// 否则写到数据目录 cert/ 下,记一行,续期方式照原主机的。
func placeCert(tx *gorm.DB, cc *CarriedCert, extracted, dataDir string) (string, string, error) {
	var row model.Cert
	if tx.Where("name = ?", cc.Name).Limit(1).Find(&row).Error == nil && row.Id != 0 {
		if _, err := os.Stat(row.CertFile); err == nil {
			if _, err := os.Stat(row.KeyFile); err == nil {
				return row.CertFile, row.KeyFile, nil
			}
		}
	}
	cb, err := os.ReadFile(filepath.Join(extracted, filepath.FromSlash(cc.CertZip)))
	if err != nil {
		return "", "", err
	}
	kb, err := os.ReadFile(filepath.Join(extracted, filepath.FromSlash(cc.KeyZip)))
	if err != nil {
		return "", "", err
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
	if _, err := os.Stat(certPath); err == nil { // 文件名撞了(内容是别的证书):换个名字,不覆盖
		certPath, keyPath = filepath.Join(dir, base+"-master.crt"), filepath.Join(dir, base+"-master.key")
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
			"source": src, "cert_file": certPath, "key_file": keyPath, "method": cc.Method, "auto_renew": cc.AutoRenew, "staging": cc.Staging}).Error
	}
	if err := tx.Create(&nr).Error; err != nil {
		return "", "", err
	}
	if !cc.AutoRenew { // gorm 的 default:true 会吞掉 false
		tx.Model(&model.Cert{}).Where("id = ?", nr.Id).Update("auto_renew", false)
	}
	return certPath, keyPath, nil
}

// applyFormerRow 新库里原主机那一行:填上它作为副机时怎么连;接任时先停用(失联期间不进订阅),记下等它回来。
func (s *Service) applyFormerRow(tx *gorm.DB, formerId, selfId uint, id Identity, how string) error {
	if formerId == 0 || formerId == selfId {
		return nil
	}
	up := map[string]interface{}{"is_local": false}
	if l := id.Self; l.ApiUrl != "" && l.Token != "" {
		up["api_url"], up["token"], up["cert_fp"] = l.ApiUrl, l.Token, l.CertFP
		up["insecure"] = strings.HasPrefix(strings.ToLower(l.ApiUrl), "https://")
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
