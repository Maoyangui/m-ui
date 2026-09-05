package sub

import (
	"bytes"
	"embed"
	"encoding/base64"
	"encoding/json"
	"fmt"
	"html/template"
	"math"
	"net/http"
	"net/url"
	"strconv"
	"strings"
	"time"

	"github.com/Maoyangui/m-ui/brand"
	"github.com/Maoyangui/m-ui/database/model"
	"github.com/Maoyangui/m-ui/render"
	"github.com/Maoyangui/m-ui/stats"
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
	StatusText, StatusKind string // active / expired / exhausted / disabled;ok / danger
	Unavailable            bool   // 不是 active:页面顶部出状态卡
	BuyURL                 string // 「选购 / 续费」按钮地址(空=不显示)
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
	ShareOn                bool   // 是否显示"临时共享"卡片
	ShareURL               string // 已生成的共享地址(空=未生成)
	QRClash, QRLink        template.URL
	Imports                []importLink
	ClientsURL             template.URL // 客户端下载页(同一地址加 ?clients=1)
	Lines                  []pageLine
	Year                   int
	Brand                  bool // 页脚 "Powered by m-ui":设置可关;代理的落地页不显示
	TZOffset               int  // 面板时区相对 UTC 的分钟数,用量图的时间标签按它显示
	// Shared:这是共享地址的落地页。只给借用者看得着的:临时共享标识、选购、公告、一键导入、订阅地址、节点;
	// 本人的用量 / 到期 / 用量图 / 共享管理一概不出,状态卡也只说原因不带数字。
	Shared bool
}

func pageLang(r *http.Request) string {
	if strings.Contains(strings.ToLower(r.Header.Get("Accept-Language")), "zh") {
		return "zh"
	}
	return "en"
}

// stateOf 落地页上的状态:到期 > 流量用尽 > 停用(含所属代理停用 / 到期)> 正常。
// 只决定页面怎么显示;客户端能不能拉到订阅只看"启用"(server.go 里的 blocked),和以前一致。
func stateOf(u model.User, rs *model.Reseller, now int64) string {
	switch {
	case u.Expiry > 0 && u.Expiry < now:
		return "expired"
	case u.Volume > 0 && u.Up+u.Down >= u.Volume:
		return "exhausted"
	case !u.Enabled:
		return "disabled"
	case rs != nil && (!rs.Enabled || (rs.Expiry > 0 && rs.Expiry < now)):
		return "disabled"
	}
	return "active"
}

// safeURL 只放行 http(s) 地址,别的(javascript: 之类)当作没填。
func safeURL(u string) string {
	u = strings.TrimSpace(u)
	l := strings.ToLower(u)
	if strings.HasPrefix(l, "http://") || strings.HasPrefix(l, "https://") {
		return u
	}
	return ""
}

// buyURL 「选购 / 续费」地址:代理填了用代理的,否则用主面板的。
func (s *Server) buyURL(rs *model.Reseller) string {
	u := s.setting("subPageBuyURL")
	if rs != nil {
		u = pick(rs.PageBuyURL, u)
	}
	return safeURL(u)
}

// key 是订阅地址里的那一段:主面板用户是用户名,代理建的用户是随机令牌。
func buildPageData(r *http.Request, subPath, key string, user model.User, lines []model.Line, opt Options, title, notice, support string) pageData {
	base := publicBase(r, subPath, key)
	clashURL := base + "?format=clash"
	loc := tz.Location(opt.TZ) // 到期 / 重置日期按面板时区显示
	now := time.Now().Unix()
	used := user.Up + user.Down
	_, off := time.Now().In(loc).Zone()

	d := pageData{
		Lang: pageLang(r), Icon: template.URL(brand.DataURI), Title: title, Notice: notice, Support: support, Name: user.Name,
		UsedText: fmtBytesHuman(used), Unlimited: user.Volume == 0, UpdateHours: opt.UpdateHours,
		SubLink: base, SubClash: clashURL, SubJSON: base + "?format=json", QRClash: template.URL(base + "/qr?format=clash"), QRLink: template.URL(base + "/qr?format=link"),
		ClientsURL: template.URL(base + "?clients=1"),
		Year:       time.Now().Year(), TZOffset: off / 60,
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
	if opt.Share {
		d.ShareOn = true
		if user.ShareToken != "" {
			d.ShareURL = publicBase(r, subPath, user.ShareToken)
		}
	}
	if user.AutoReset && user.NextReset > 0 {
		d.ResetText = time.Unix(user.NextReset, 0).In(loc).Format("2006-01-02")
	}
	d.StatusText = stateOf(user, nil, now)
	d.StatusKind = "ok"
	if d.StatusText != "active" {
		d.StatusKind, d.Unavailable = "danger", true
	}

	enc := url.QueryEscape
	imp := SubTitle(user, opt) // 一键导入按订阅标题命名,与响应头一致
	d.Imports = []importLink{
		{Name: "Clash / Mihomo", Hint: "Clash Verge · FlClash · ClashMeta", Href: template.URL("clash://install-config?url=" + enc(clashURL) + "&name=" + enc(imp))},
		{Name: "Shadowrocket", Hint: "iOS", Href: template.URL("shadowrocket://add/sub://" + base64.StdEncoding.EncodeToString([]byte(base)) + "?remark=" + enc(imp))},
		{Name: "sing-box", Hint: "SFA Android · SFI iOS · Desktop", Href: template.URL("sing-box://import-remote-profile?url=" + enc(base+"?format=json") + "#" + enc(imp))},
		{Name: "Hiddify", Hint: "Android · iOS · Desktop", Href: template.URL("hiddify://import/" + clashURL)},
		{Name: "Stash", Hint: "iOS · macOS", Href: template.URL("stash://install-config?url=" + enc(clashURL) + "&name=" + enc(imp))},
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

// pageTitle 是落地页(以及客户端下载页)顶上的名字:代理有自己的就用代理的。
func (s *Server) pageTitle(rs *model.Reseller, opt Options) string {
	title := s.setting("subPageTitle")
	if rs != nil {
		title = pick(rs.PageTitle, title)
	}
	if title == "" {
		title = opt.ProfileTitle
	}
	if title == "" {
		title = brand.Name
	}
	return title
}

// servePage 输出订阅落地页。不可用(到期 / 用尽 / 停用)的用户也出这一页,顶部标明原因。
// shared 为真是共享地址(key 是共享令牌)的精简版:见 pageData.Shared。
func (s *Server) servePage(w http.ResponseWriter, r *http.Request, subPath, key string, user model.User, lines []model.Line, opt Options, rs *model.Reseller, shared bool) {
	notice, support := s.setting("subPageNotice"), s.setting("subPageSupport")
	if rs != nil { // 代理可以给自己的用户配一套落地页文案
		notice, support = pick(rs.PageNotice, notice), pick(rs.PageSupport, support)
	}
	title := s.pageTitle(rs, opt)
	if shared {
		opt.Share = false // 借用者不能管理共享
	}
	data := buildPageData(r, subPath, key, user, lines, opt, title, notice, support)
	data.Shared = shared
	data.Brand = rs == nil && !strings.EqualFold(s.setting("subPageBrand"), "false") // 代理的落地页尊重代理品牌
	data.BuyURL = s.buyURL(rs)
	if st := stateOf(user, rs, time.Now().Unix()); st != data.StatusText { // 代理被停用 / 到期也算停用
		data.StatusText, data.StatusKind, data.Unavailable = st, "danger", true
	}
	var buf bytes.Buffer
	if err := pageTmpl.Execute(&buf, data); err != nil {
		http.Error(w, "page error: "+err.Error(), http.StatusInternalServerError)
		return
	}
	w.Header().Set("Content-Type", "text/html; charset=utf-8")
	w.Header().Set("Cache-Control", "no-store")
	w.Header().Set("Referrer-Policy", "no-referrer")
	w.Header().Set("X-Frame-Options", "DENY")
	w.Header().Set("X-Content-Type-Options", "nosniff")
	w.WriteHeader(http.StatusOK)
	_, _ = w.Write(buf.Bytes())
}

// serveStats 落地页"用量情况"的数据:GET <地址>?stats=24|168|720[&tz=分钟偏移],返回该用户的时段柱。
// 只对本人地址开放(共享地址在 server.go 里已拦下);知道地址就能看,和落地页本身同一信任级别。
func (s *Server) serveStats(w http.ResponseWriter, r *http.Request, user model.User) {
	hours, _ := strconv.Atoi(r.URL.Query().Get("stats"))
	switch hours {
	case 24, 168, 720:
	default:
		hours = 24
	}
	loc := tz.Location(s.setting("timezone"))
	if v, err := strconv.Atoi(r.URL.Query().Get("tz")); err == nil && v >= -14*60 && v <= 14*60 {
		loc = time.FixedZone("viewer", v*60)
	}
	res := stats.Series(s.db, "user", user.Name, hours, stats.BucketFor(hours), s.settingInt("statsBucketSeconds", 60), loc)
	w.Header().Set("Content-Type", "application/json; charset=utf-8")
	w.Header().Set("Cache-Control", "no-store")
	w.Header().Set("X-Content-Type-Options", "nosniff")
	_ = json.NewEncoder(w).Encode(res)
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

// pick 返回第一个非空串。
func pick(v, def string) string {
	if strings.TrimSpace(v) != "" {
		return v
	}
	return def
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
