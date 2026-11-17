package ha

import (
	"archive/zip"
	"os"
	"strconv"
	"strings"

	"github.com/Maoyangui/m-ui/database/model"
	"github.com/Maoyangui/m-ui/logger"
	"github.com/Maoyangui/m-ui/render"

	"gorm.io/gorm"
)

// 面板身份跟着主机走(用户 11-15:「副机接任了外部 API 也要复制过去,不然要在很多地方重新填」)。
//
// 外部 API、代理面板(和代理们自己的外部 API)都挂在面板的地址上。API 密钥、权限、白名单本来就在数据副本里,
// 接任后照样能用;会变的只是地址 —— 面板、代理面板的端口、路径、证书是每台机器自己的。所以全量接管(以及设为主机)
// 而且面板用的是域名时,新主机把面板和代理面板的域名、端口、路径、证书、受信任的反向代理一起换成原主机的,面板域名也经
// DNS 接口改指过来:面板、外部 API、代理面板的地址都不变,对接的商城、机器人、代理一处都不用改。
//   - 证书(含私钥)和订阅证书一样:只有选了「全量接管」(或设为主机的整库数据)才进副本,副本本身用备用的公钥加密、主机签名;
//   - 新主机上端口撞了(它的订阅 / WARP 端口、它上面的线路),面板就留它自己的(地址会变),写进通知;
//   - 启动时迁来的端口绑不上(本机别的程序占着),自动退回它原来的面板 / 代理面板,不让新主机起不来;
//   - 面板用的是 IP 时跟不过去(IP 换了机器就变),就绪检查提醒给面板配域名。

// PanelIdentity 原主机的面板身份(Identity.Panel)。
type PanelIdentity struct {
	Host           string       `json:"host"` // 面板域名(webDomain);空或 IP = 跟不过去
	Port           int          `json:"port"`
	Path           string       `json:"path"`
	HTTPS          bool         `json:"https,omitempty"`
	Cert           *CarriedCert `json:"cert,omitempty"`     // 面板证书(和订阅证书是同一份文件时不重复带,见 SameCert)
	SameCert       bool         `json:"sameCert,omitempty"` // 面板和订阅用的同一张证书
	ResellerPort   int          `json:"resellerPort,omitempty"`
	ResellerPath   string       `json:"resellerPath,omitempty"`
	ResellerCert   *CarriedCert `json:"resellerCert,omitempty"` // 代理面板单独的证书(没有 = 跟面板用同一张)
	ResellerOwn    bool         `json:"resellerOwn,omitempty"`  // 代理面板有单独的证书
	TrustedProxies string       `json:"trustedProxies,omitempty"`
}

// follows 面板能不能跟过去:有域名、有端口。
func (p *PanelIdentity) follows() bool {
	return p != nil && p.Host != "" && !isIPHost(p.Host) && p.Port > 0
}

// panelPrev 新主机自己原来的面板设置(迁来的端口启动时绑不上,改回用它)。
type panelPrev struct {
	Domain, Path, CertFile, KeyFile                     string
	Port, ResellerPort                                  int
	ResellerPath, ResellerCertFile, ResellerKeyFile, TP string
}

// capturePanel 记下本机(主机)的面板身份;carry 时把证书文件也写进 zip(和订阅证书同一个口径)。
func (s *Service) capturePanel(zw *zip.Writer, id *Identity, carry bool) error {
	p := &PanelIdentity{Host: strings.TrimSpace(s.setting("webDomain")), Port: s.settingInt("webPort", 2053, 1, 65535),
		Path: s.setting("webPath"), ResellerPort: s.settingInt("resellerPort", 2054, 1, 65535), ResellerPath: s.setting("resellerPath"),
		TrustedProxies: s.setting("trustedProxies")}
	id.Panel = p
	cf, kf := strings.TrimSpace(s.setting("webCertFile")), strings.TrimSpace(s.setting("webKeyFile"))
	p.HTTPS = cf != "" && kf != ""
	rcf, rkf := strings.TrimSpace(s.setting("resellerCertFile")), strings.TrimSpace(s.setting("resellerKeyFile"))
	p.ResellerOwn = rcf != "" && rkf != ""
	if !carry || !p.follows() {
		return nil
	}
	if p.HTTPS {
		if cf == strings.TrimSpace(s.setting("subCertFile")) && id.SubCert != nil {
			p.SameCert = true
		} else if cc, err := s.carryCert(zw, cf, kf, "identity/panel"); err != nil {
			return err
		} else {
			p.Cert = cc
		}
	}
	if p.ResellerOwn {
		cc, err := s.carryCert(zw, rcf, rkf, "identity/reseller")
		if err != nil {
			return err
		}
		p.ResellerCert = cc
	}
	return nil
}

// carryCert 把一张证书(含私钥)写进 zip,带上证书库里那一行的续期方式;文件读不出来不带(不挡副本)。
func (s *Service) carryCert(zw *zip.Writer, certFile, keyFile, prefix string) (*CarriedCert, error) {
	cb, err1 := os.ReadFile(certFile)
	kb, err2 := os.ReadFile(keyFile)
	if err1 != nil || err2 != nil {
		return nil, nil
	}
	cc := &CarriedCert{Source: "external", CertZip: prefix + ".crt", KeyZip: prefix + ".key"}
	var row model.Cert
	if s.d.DB.Where("cert_file = ?", certFile).Limit(1).Find(&row).Error == nil && row.Id != 0 {
		cc.Name, cc.Source, cc.Method, cc.AutoRenew, cc.Staging = row.Name, row.Source, row.Method, row.AutoRenew, row.Staging
	}
	if cc.Name == "" {
		cc.Name = strings.TrimSuffix(strings.TrimSuffix(fileBase(certFile), ".crt"), ".pem")
	}
	if err := zipAddBytes(zw, cc.CertZip, cb); err != nil {
		return nil, err
	}
	if err := zipAddBytes(zw, cc.KeyZip, kb); err != nil {
		return nil, err
	}
	return cc, nil
}

func fileBase(p string) string {
	if i := strings.LastIndexAny(p, `/\`); i >= 0 {
		return p[i+1:]
	}
	return p
}

// 面板身份的告警代码(界面 ha.idw.*)。
const (
	idwPanelPortClash    = "panelPortClash"    // 迁来的面板端口在本机被占着,面板留本机自己的:{port, what, own}
	idwResellerPortClash = "resellerPortClash" // 迁来的代理面板端口在本机被占着,留本机自己的:{port, what, own}
	idwPanelPortBind     = "panelPortBind"     // 启动时迁来的面板端口绑不上,改回本机原来的:{port, own, err}
	idwResellerPortBind  = "resellerPortBind"  // 启动时迁来的代理面板端口绑不上,改回本机原来的:{port, own, err}
	idwPanelCertNew      = "panelCertNew"      // 面板证书不能用,正经 DNS 接口给面板域名重签:{host}
	idwPanelCertTodo     = "panelCertTodo"     // 面板证书不能用、也没法自动重签:{host}
	idwPanelCertFail     = "panelCertFail"     // 面板证书没能装上:{err}
)

// applyPanelIdentity 在要接任的库里换上原主机的面板身份(在 applyIdentity 之后调:订阅端口、订阅证书已经换好)。
func applyPanelIdentity(tx *gorm.DB, id Identity, selfId uint, extracted, dataDir string, res *identityResult) error {
	p := id.Panel
	if !p.follows() {
		return nil
	}
	set := func(k, v string) error { return upsert(tx, map[string]string{k: v}) }
	get := func(k string) string {
		var v string
		tx.Raw("SELECT value FROM settings WHERE key = ?", k).Scan(&v)
		return v
	}
	prev := panelPrev{Domain: get("webDomain"), Path: get("webPath"), CertFile: get("webCertFile"), KeyFile: get("webKeyFile"),
		Port: atoiDef(get("webPort"), 2053), ResellerPort: atoiDef(get("resellerPort"), 2054), ResellerPath: get("resellerPath"),
		ResellerCertFile: get("resellerCertFile"), ResellerKeyFile: get("resellerKeyFile"), TP: get("trustedProxies")}
	resellerOn := !strings.EqualFold(get("resellerEnabled"), "false")
	subPort := atoiDef(get("subPort"), 2056)
	warp := 0
	if strings.TrimSpace(get("warpPort")) != "" {
		warp = atoiDef(get("warpPort"), 40000)
	}
	var lines []model.Line
	tx.Find(&lines)
	clash := func(port int, others map[string]int) string {
		for what, q := range others {
			if q > 0 && q == port {
				return what
			}
		}
		for _, l := range lines {
			if l.Port == port && !render.UDPOnly(l.Protocol) && render.LineOnNode(l, selfId) {
				return l.Name
			}
		}
		return ""
	}
	// 面板端口:和本机(迁来的)订阅端口、WARP、线路撞了就留本机自己的面板(地址会变)
	panelPort := p.Port
	if what := clash(panelPort, map[string]int{"sub": subPort, "warp": warp}); what != "" && panelPort != prev.Port {
		res.Warnings = append(res.Warnings, item(idwPanelPortClash, "port", strconv.Itoa(panelPort), "what", what, "own", strconv.Itoa(prev.Port)))
		return nil
	}
	res.PrevPanel = &prev
	if err := set("webDomain", p.Host); err != nil {
		return err
	}
	if err := set("webPort", strconv.Itoa(panelPort)); err != nil {
		return err
	}
	if err := set("webPath", p.Path); err != nil {
		return err
	}
	switch {
	case !p.HTTPS: // 原主机的面板走 http:照它的(地址的开头一样)
		if err := set("webCertFile", ""); err != nil {
			return err
		}
		if err := set("webKeyFile", ""); err != nil {
			return err
		}
	case p.SameCert && get("subCertFile") != "":
		if err := set("webCertFile", get("subCertFile")); err != nil {
			return err
		}
		if err := set("webKeyFile", get("subKeyFile")); err != nil {
			return err
		}
	case p.Cert != nil:
		cp, kp, err := placeCert(tx, p.Cert, extracted, dataDir)
		if err != nil {
			res.Warnings = append(res.Warnings, item(idwPanelCertFail, "err", err.Error()))
			break
		}
		if err := set("webCertFile", cp); err != nil {
			return err
		}
		if err := set("webKeyFile", kp); err != nil {
			return err
		}
	} // 走 https 却没带证书:先用本机自己的,接任后检查不含面板域名就经 DNS 接口重签(ensurePanelCert)
	if err := set("trustedProxies", p.TrustedProxies); err != nil {
		return err
	}
	res.PanelKept, res.PanelHost, res.PanelHTTPS = true, p.Host, p.HTTPS
	// 代理面板:跟着面板走;端口撞了留本机自己的
	if resellerOn && p.ResellerPort > 0 {
		if what := clash(p.ResellerPort, map[string]int{"sub": subPort, "warp": warp, "panel": panelPort}); what != "" && p.ResellerPort != prev.ResellerPort {
			res.Warnings = append(res.Warnings, item(idwResellerPortClash, "port", strconv.Itoa(p.ResellerPort), "what", what, "own", strconv.Itoa(prev.ResellerPort)))
		} else {
			if err := set("resellerPort", strconv.Itoa(p.ResellerPort)); err != nil {
				return err
			}
			if err := set("resellerPath", p.ResellerPath); err != nil {
				return err
			}
			cf, kf := "", "" // 原主机的代理面板跟面板用同一张证书:清掉本机单独的
			if p.ResellerOwn && p.ResellerCert != nil {
				if cp, kp, err := placeCert(tx, p.ResellerCert, extracted, dataDir); err == nil {
					cf, kf = cp, kp
				}
			}
			if err := set("resellerCertFile", cf); err != nil {
				return err
			}
			if err := set("resellerKeyFile", kf); err != nil {
				return err
			}
			rp := prev
			res.ResellerKept, res.PrevReseller = true, &rp
		}
	}
	return nil
}

// PanelFallback 刚换上新库(成为主机后第一次启动、还没发通知)时迁来的面板端口绑不上(本机别的程序占着):
// 面板改回本机原来的(端口、路径、域名、证书、反向代理),记进成为主机的告警,返回要不要再试一次。
func (s *Service) PanelFallback(cause error) bool {
	if s.IsNode() {
		return false
	}
	var b Became
	if !s.getJSON(KeyBecame, &b) || b.Notified || b.Identity.PrevPanel == nil {
		return false
	}
	pv := b.Identity.PrevPanel
	from := s.settingInt("webPort", 2053, 1, 65535)
	if from == pv.Port && s.setting("webPath") == pv.Path {
		return false
	}
	logger.Warning("容灾:迁来的面板端口 ", from, " 绑不上(", cause, "),面板改回本机原来的 ", pv.Port)
	b.Identity.Warnings = append(b.Identity.Warnings, item(idwPanelPortBind, "port", strconv.Itoa(from), "own", strconv.Itoa(pv.Port), "err", errText(cause)))
	b.Identity.PanelKept, b.Identity.PrevPanel = false, nil
	return s.set(map[string]string{"webPort": strconv.Itoa(pv.Port), "webPath": pv.Path, "webDomain": pv.Domain,
		"webCertFile": pv.CertFile, "webKeyFile": pv.KeyFile, "trustedProxies": pv.TP, KeyBecame: mustJSON(b)}) == nil
}

// ResellerFallback 同上,代理面板。
func (s *Service) ResellerFallback(cause error) bool {
	if s.IsNode() {
		return false
	}
	var b Became
	if !s.getJSON(KeyBecame, &b) || b.Notified || b.Identity.PrevReseller == nil {
		return false
	}
	pv := b.Identity.PrevReseller
	from := s.settingInt("resellerPort", 2054, 1, 65535)
	if from == pv.ResellerPort {
		return false
	}
	logger.Warning("容灾:迁来的代理面板端口 ", from, " 绑不上(", cause, "),代理面板改回本机原来的 ", pv.ResellerPort)
	b.Identity.Warnings = append(b.Identity.Warnings, item(idwResellerPortBind, "port", strconv.Itoa(from), "own", strconv.Itoa(pv.ResellerPort), "err", errText(cause)))
	b.Identity.ResellerKept, b.Identity.PrevReseller = false, nil
	return s.set(map[string]string{"resellerPort": strconv.Itoa(pv.ResellerPort), "resellerPath": pv.ResellerPath,
		"resellerCertFile": pv.ResellerCertFile, "resellerKeyFile": pv.ResellerKeyFile, KeyBecame: mustJSON(b)}) == nil
}

func errText(err error) string {
	if err == nil {
		return ""
	}
	return shortErr(err.Error(), 200)
}

// PrevPanelAddr 本机换成原主机的面板以前,自己的面板地址(端口、路径、证书)。
type PrevPanelAddr struct {
	Port              int
	Path              string
	CertFile, KeyFile string
}

// PrevPanelListen 面板跟着主机走了:本机原来的面板地址(端口没变、只换了路径的,web 在现在的面板上给原来的路径留 agent 的别名)(继续在上面接 agent 请求 —— 原主机回来、
// 别的服务器库里记的还是这个地址,接不到就核实不了新主机还活着,原主机就不会变为副机)。没换过为 nil。
func (s *Service) PrevPanelListen() *PrevPanelAddr {
	if s.IsNode() {
		return nil
	}
	var b Became
	if !s.getJSON(KeyBecame, &b) || !b.Identity.PanelKept || b.Identity.PrevPanel == nil {
		return nil
	}
	pv := b.Identity.PrevPanel
	if pv.Port <= 0 {
		return nil
	}
	return &PrevPanelAddr{Port: pv.Port, Path: pv.Path, CertFile: pv.CertFile, KeyFile: pv.KeyFile}
}
