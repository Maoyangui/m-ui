package sub

import (
	"bytes"
	"embed"
	"encoding/base64"
	"fmt"
	"html/template"
	"math"
	"net/http"
	"net/url"
	"strings"
	"time"

	"github.com/Maoyangui/m-ui/brand"
	"github.com/Maoyangui/m-ui/database/model"
	"github.com/Maoyangui/m-ui/render"
	"github.com/Maoyangui/m-ui/tz"

	"github.com/skip2/go-qrcode"
)

//go:embed page.html
var pageFS embed.FS

var pageTmpl = template.Must(template.ParseFS(pageFS, "page.html"))

// 已知代理客户端的 UA 片段:命中即认为是客户端在拉订阅,返回原始内容。
var clientUATokens = []string{
	"clash", "mihomo", "stash", "shadowrocket", "sing-box", "singbox", "sfi/", "sfa/", "sfm/",
	"hiddify", "v2ray", "xray", "nekobox", "nekoray", "surge", "quantumult", "loon", "karing",
	"husi", "flclash", "sagernet", "matsuri", "pharos", "kitsunebi", "streisand", "v2box",
	"hysteria", "tuic", "curl/", "wget/", "go-http-client", "okhttp", "python-requests",
}

// WantsPage 判断请求来自浏览器(应返回订阅页)还是代理客户端(应返回原始订阅)。
// ?format=page 强制出页面;其它 format 强制出原始订阅。
func WantsPage(r *http.Request) bool {
	if f := r.URL.Query().Get("format"); f != "" {
		return f == "page"
	}
	ua := strings.ToLower(r.UserAgent())
	for _, tok := range clientUATokens {
		if strings.Contains(ua, tok) {
			return false
		}
	}
	return strings.Contains(ua, "mozilla") &&
		(strings.Contains(ua, "chrome") || strings.Contains(ua, "safari") || strings.Contains(ua, "firefox") || strings.Contains(ua, "edg"))
}

// publicBase 以用户实际访问到的 scheme/host 组装订阅地址,反代/换域名都不用改设置。
func publicBase(r *http.Request, subPath, name string) string {
	scheme := "http"
	if r.TLS != nil || strings.EqualFold(r.Header.Get("X-Forwarded-Proto"), "https") {
		scheme = "https"
	}
	host := r.Host
	if h := r.Header.Get("X-Forwarded-Host"); h != "" {
		host = h
	}
	return scheme + "://" + host + subPath + url.PathEscape(name)
}

type importLink struct {
	Name string
	Href template.URL
	Hint string
}

type pageLine struct {
	Name, Protocol, TLS, Transport string
}

type pageData struct {
	Lang                   string
	Icon                   template.URL // 标签图标(内联 SVG data URI)
	Title, Notice, Support string
	Name                   string
	StatusText, StatusKind string
	UsedText, TotalText    string
	Percent                int
	Unlimited              bool
	ExpiryText             string
	DaysLeft               int
	HasExpiry, Expired     bool
	ResetText              string
	UpdateHours            int
	SubLink, SubClash      string
	SubJSON                string // sing-box 远程配置(SFA / SFI)
	QRClash, QRLink        template.URL
	Imports                []importLink
	Lines                  []pageLine
	Year                   int
}

func pageLang(r *http.Request) string {
	if strings.Contains(strings.ToLower(r.Header.Get("Accept-Language")), "zh") {
		return "zh"
	}
	return "en"
}

func buildPageData(r *http.Request, subPath string, user model.User, lines []model.Line, opt Options, title, notice, support string) pageData {
	base := publicBase(r, subPath, user.Name)
	clashURL := base + "?format=clash"
	loc := tz.Location(opt.TZ) // 到期 / 重置日期按面板时区显示
	now := time.Now().Unix()
	used := user.Up + user.Down

	d := pageData{
		Lang: pageLang(r), Icon: template.URL(brand.DataURI), Title: title, Notice: notice, Support: support, Name: user.Name,
		UsedText: fmtBytesHuman(used), Unlimited: user.Volume == 0, UpdateHours: opt.UpdateHours,
		SubLink: base, SubClash: clashURL, SubJSON: base + "?format=json", QRClash: template.URL(base + "/qr?format=clash"), QRLink: template.URL(base + "/qr?format=link"),
		Year: time.Now().Year(),
	}
	if user.Volume > 0 {
		d.TotalText = fmtBytesHuman(user.Volume)
		d.Percent = int(math.Min(100, float64(used)/float64(user.Volume)*100))
	}
	if user.Expiry > 0 {
		d.HasExpiry = true
		d.ExpiryText = time.Unix(user.Expiry, 0).In(loc).Format("2006-01-02")
		d.DaysLeft = int(math.Ceil(float64(user.Expiry-now) / 86400))
		d.Expired = user.Expiry < now
	}
	if user.AutoReset && user.NextReset > 0 {
		d.ResetText = time.Unix(user.NextReset, 0).In(loc).Format("2006-01-02")
	}
	switch {
	case !user.Enabled:
		d.StatusText, d.StatusKind = "disabled", "danger"
	case d.Expired:
		d.StatusText, d.StatusKind = "expired", "danger"
	case user.Volume > 0 && used >= user.Volume:
		d.StatusText, d.StatusKind = "exhausted", "danger"
	default:
		d.StatusText, d.StatusKind = "active", "ok"
	}

	enc := url.QueryEscape
	d.Imports = []importLink{
		{Name: "Clash / Mihomo", Hint: "Clash Verge · FlClash · ClashMeta", Href: template.URL("clash://install-config?url=" + enc(clashURL) + "&name=" + enc(title))},
		{Name: "Shadowrocket", Hint: "iOS", Href: template.URL("shadowrocket://add/sub://" + base64.StdEncoding.EncodeToString([]byte(base)) + "?remark=" + enc(title))},
		{Name: "sing-box", Hint: "SFA Android · SFI iOS · Desktop", Href: template.URL("sing-box://import-remote-profile?url=" + enc(base+"?format=json") + "#" + enc(title))},
		{Name: "Hiddify", Hint: "Android · iOS · Desktop", Href: template.URL("hiddify://import/" + clashURL)},
		{Name: "Stash", Hint: "iOS · macOS", Href: template.URL("stash://install-config?url=" + enc(clashURL) + "&name=" + enc(title))},
	}
	for _, l := range lines {
		pl := pageLine{Name: l.Name, Protocol: l.Protocol}
		if mode := render.ParseTLS(l).Mode; mode != "none" {
			pl.TLS = mode
		}
		if render.HasTransport(l) {
			var tr struct {
				Type string `json:"type"`
			}
			_ = jsonUnmarshal(l.Transport, &tr)
			pl.Transport = tr.Type
		}
		d.Lines = append(d.Lines, pl)
	}
	return d
}

// servePage 输出订阅落地页。
func (s *Server) servePage(w http.ResponseWriter, r *http.Request, subPath string, user model.User, lines []model.Line, opt Options) {
	title := s.setting("subPageTitle")
	if title == "" {
		title = opt.ProfileTitle
	}
	if title == "" {
		title = "m-ui"
	}
	data := buildPageData(r, subPath, user, lines, opt, title, s.setting("subPageNotice"), s.setting("subPageSupport"))
	var buf bytes.Buffer
	if err := pageTmpl.Execute(&buf, data); err != nil {
		http.Error(w, "page error: "+err.Error(), http.StatusInternalServerError)
		return
	}
	w.Header().Set("Content-Type", "text/html; charset=utf-8")
	w.Header().Set("Cache-Control", "no-store")
	w.Header().Set("Referrer-Policy", "no-referrer")
	w.WriteHeader(http.StatusOK)
	_, _ = w.Write(buf.Bytes())
}

// serveQR 输出订阅地址二维码(PNG)。
func (s *Server) serveQR(w http.ResponseWriter, r *http.Request, subPath, name string) {
	target := publicBase(r, subPath, name)
	if r.URL.Query().Get("format") == "clash" {
		target += "?format=clash"
	}
	png, err := qrcode.Encode(target, qrcode.Medium, 320)
	if err != nil {
		http.Error(w, err.Error(), http.StatusInternalServerError)
		return
	}
	w.Header().Set("Content-Type", "image/png")
	w.Header().Set("Cache-Control", "no-store")
	_, _ = w.Write(png)
}

func fmtBytesHuman(n int64) string {
	f := float64(n)
	units := []string{"B", "KB", "MB", "GB", "TB"}
	i := 0
	for f >= 1024 && i < len(units)-1 {
		f /= 1024
		i++
	}
	if i == 0 {
		return fmt.Sprintf("%d %s", n, units[i])
	}
	return fmt.Sprintf("%.1f %s", f, units[i])
}
