package web

// 页面伪装(用户 N2)的设置页接口:预览这台的伪装站(或跳转)、「换一个样子」、开启后哪些入口会回应它、80 / 443 的状况。
// 只给主机的管理员用(设置随主机同步给副机;种子每台各自生成,副机上的伪装站和主机的不一样)。

import (
	"bytes"
	"encoding/json"
	"errors"
	"fmt"
	"net/http"
	"strings"

	"github.com/Maoyangui/m-ui/camo"
	"github.com/Maoyangui/m-ui/database/model"
	"github.com/Maoyangui/m-ui/render"
	"github.com/Maoyangui/m-ui/runner"
)

// normCamoTemplate 设置 camoTemplate 的写法:空(或 auto)= 自动,按本机种子挑;redirect = 跳转到管理员自己的网站;
// 别的只认内置模板的名字。
func normCamoTemplate(v string) (string, error) {
	t := strings.ToLower(strings.TrimSpace(v))
	if t == "" || t == "auto" {
		return "", nil
	}
	if t == runner.CamoRedirectMode {
		return t, nil
	}
	for _, x := range camo.Templates {
		if x == t {
			return t, nil
		}
	}
	return "", errors.New("伪装成什么只能是自动、下载镜像、软件下载、视频站、个人博客或跳转到我的网站")
}

// camoView GET camo 的返回。
type camoView struct {
	Mode     string                  `json:"mode"`               // site(伪装站)/ redirect(跳转)
	Template string                  `json:"template,omitempty"` // 伪装站实际用的模板(自动时是按种子挑出来的那个)
	Auto     bool                    `json:"auto,omitempty"`     // 是自动挑的
	Name     string                  `json:"name,omitempty"`     // 站名
	HTML     string                  `json:"html,omitempty"`     // 首页(样式表、图片内联,链接拿掉,只给预览看)
	Target   string                  `json:"target,omitempty"`   // 跳转模式:跳到哪(规整后的写法);网址填错时为空、Error 写原因
	Error    string                  `json:"error,omitempty"`
	Host     string                  `json:"host"`             // 预览地址栏里写的域名:订阅域名、面板域名、公网 IP 里先有的那个
	Live     string                  `json:"live,omitempty"`   // 这台上正在回应的伪装站(订阅端口的根目录),设置页「在新标签页打开」用;没开时为空
	Photos   *runner.CamoPhotoStatus `json:"photos,omitempty"` // 封面照片的状况(视频站、博客、部分软件下载页;别的为空)
	Ports    []runner.CamoPortState  `json:"ports"`            // 80 / 443 的状况(没开 camoPorts 时为空)
	Entries  camoEntries             `json:"entries"`
}

// camoEntries 开启后会回应伪装站的入口:这台的三个端口(根目录和对不上的路径),全部服务器上的几类线路(只算启用的)。
type camoEntries struct {
	Panel    int `json:"panel"`
	Sub      int `json:"sub"`
	Reseller int `json:"reseller"` // 0 = 代理面板没开
	AnyTLS   int `json:"anytls"`
	Trojan   int `json:"trojan"` // 没配传输层的(配了 ws / grpc 的由传输层回应,不经过这里)
	Hy2      int `json:"hy2"`    // 没填伪装站点的
	Hy2Own   int `json:"hy2Own"` // 填了自己的伪装站点的:照旧用它
}

func (s *Server) handleCamo(w http.ResponseWriter, r *http.Request) {
	switch r.Method {
	case http.MethodGet:
		q := r.URL.Query()
		tmpl, target := s.setting("camoTemplate"), s.setting("camoRedirect")
		if q.Has("template") { // 设置页上改了还没保存:按页面上的值预览
			tmpl = q.Get("template")
		}
		if q.Has("redirect") {
			target = q.Get("redirect")
		}
		t, err := normCamoTemplate(tmpl)
		if err != nil {
			badRequest(w, err)
			return
		}
		v := camoView{Mode: "site", Host: s.camoHost(), Ports: s.run.CamoPorts(), Entries: s.camoEntries()}
		if t == runner.CamoRedirectMode {
			v.Mode = runner.CamoRedirectMode
			if rd, _, err := camo.ParseRedirect(target); err == nil {
				v.Target = rd.Target()
			} else if strings.TrimSpace(target) != "" {
				v.Error = err.Error()
			}
		} else {
			site, photos := s.run.CamoPreview(t)
			v.Template, v.Auto, v.Name, v.HTML, v.Photos = site.Template(), t == "", site.Name(), string(site.Preview()), photos
		}
		if strings.EqualFold(s.setting("camoEnabled"), "true") {
			v.Live = s.camoLive()
		}
		w.Header().Set("Cache-Control", "no-store")
		writeJSON(w, http.StatusOK, v)
	case http.MethodPost: // 换一个样子:只换这台的种子
		if err := s.run.CamoReseed(); err != nil {
			writeJSON(w, http.StatusInternalServerError, map[string]string{"error": "没换成: " + err.Error()})
			return
		}
		s.audit(r, "settings", "camoReseed", nil)
		writeJSON(w, http.StatusOK, map[string]bool{"ok": true})
	default:
		w.Header().Set("Allow", "GET, POST")
		w.WriteHeader(http.StatusMethodNotAllowed)
	}
}

// camoLive 这台上正在回应的伪装站:订阅端口的根目录(订阅路径就是根目录时没有)
func (s *Server) camoLive() string {
	if s.subPath() == "/" {
		return ""
	}
	scheme := "http"
	if s.setting("subCertFile") != "" && s.setting("subKeyFile") != "" {
		scheme = "https"
	}
	host := s.publicHost()
	if host == "" {
		return ""
	}
	return fmt.Sprintf("%s://%s/", scheme, model.URLHost(scheme, host, s.settingInt("subPort", 2056)))
}

func (s *Server) camoHost() string {
	for _, k := range []string{"subDomain", "webDomain", "publicIp"} {
		if v := strings.TrimSpace(s.setting(k)); v != "" {
			return v
		}
	}
	return ""
}

func (s *Server) camoEntries() camoEntries {
	e := camoEntries{Panel: s.settingInt("webPort", 2053), Sub: s.settingInt("subPort", 2056)}
	if resellerListens(s.setting("resellerEnabled"), s.setting("nodeMode")) {
		e.Reseller = s.settingInt("resellerPort", 2054)
	}
	var lines []model.Line
	s.db.Select("protocol, options, transport").Where("enabled = ?", true).Find(&lines)
	for _, l := range lines {
		switch l.Protocol {
		case "anytls":
			e.AnyTLS++
		case "trojan":
			if !render.HasTransport(l) {
				e.Trojan++
			}
		case "hysteria2":
			if hy2OwnMasquerade(l.Options) {
				e.Hy2Own++
			} else {
				e.Hy2++
			}
		}
	}
	return e
}

// hy2OwnMasquerade Hysteria2 线路自己填了伪装站点(和入站一样的判法:masquerade 写成网址,或者是带 type 的对象)。
func hy2OwnMasquerade(opts json.RawMessage) bool {
	var o struct {
		Masquerade json.RawMessage `json:"masquerade"`
	}
	if json.Unmarshal(opts, &o) != nil {
		return false
	}
	m := bytes.TrimSpace(o.Masquerade)
	if len(m) == 0 || string(m) == "null" {
		return false
	}
	var str string
	if json.Unmarshal(m, &str) == nil {
		return strings.TrimSpace(str) != ""
	}
	var obj struct {
		Type string `json:"type"`
	}
	return json.Unmarshal(m, &obj) == nil && obj.Type != ""
}
