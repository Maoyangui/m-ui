package sub

import (
	"bytes"
	"embed"
	"html/template"
	"net/http"
	"time"

	"github.com/Maoyangui/m-ui/brand"
	"github.com/Maoyangui/m-ui/database/model"
)

// 订阅地址对不上任何人时,浏览器里给一页人话而不是空白的 404。
//
// 到期 / 用尽 / 停用的用户不走这里:地址能对上就出正常落地页,顶部标明原因(见 page.go)。
// 这里只剩"地址无效":按地址反查归属拿不到人,就用主面板的公告、联系方式与选购链接;
// 反查得到代理(理论上不会,留作兜底)就用代理的。客户端(非浏览器)仍是干净的 404。
//
//go:embed gone.html
var goneFS embed.FS

var goneTmpl = template.Must(template.ParseFS(goneFS, "gone.html"))

type goneData struct {
	Lang    string
	Icon    template.URL
	Title   string
	Notice  string
	Support string
	BuyURL  string // 「选购订阅」按钮(空=不显示)
	Year    int
}

// ownerOf 按订阅地址反查用户与其代理,忽略启停状态(停用的人才最需要看到这页)。
func (s *Server) ownerOf(key string) (*model.User, *model.Reseller) {
	var u model.User
	if s.db.Where("name = ? AND COALESCE(reseller_id, 0) = 0 AND COALESCE(sub_token, '') = ''", key).First(&u).Error != nil &&
		s.db.Where("sub_token = ?", key).First(&u).Error != nil &&
		s.db.Where("share_token = ?", key).First(&u).Error != nil {
		return nil, nil
	}
	return &u, s.resellerOf(u)
}

// serveNotFound 浏览器来的 404 给一页说明,其它一律原样 404。
func (s *Server) serveNotFound(w http.ResponseWriter, r *http.Request, key string) {
	if !WantsPage(r) {
		http.NotFound(w, r)
		return
	}
	_, rs := s.ownerOf(key)
	if !s.pageEnabled(rs) { // 订阅页整体关掉了就别单独冒出来一页
		http.NotFound(w, r)
		return
	}
	notice, support := s.setting("subPageNotice"), s.setting("subPageSupport")
	if rs != nil {
		notice, support = pick(rs.PageNotice, notice), pick(rs.PageSupport, support)
	}
	d := goneData{
		Lang: pageLang(r), Icon: template.URL(brand.DataURI),
		Title: s.pageTitle(rs, s.options()), Notice: notice, Support: support, BuyURL: s.buyURL(rs), Year: time.Now().Year(),
	}
	var buf bytes.Buffer
	if err := goneTmpl.Execute(&buf, d); err != nil {
		http.NotFound(w, r)
		return
	}
	w.Header().Set("Content-Type", "text/html; charset=utf-8")
	w.Header().Set("Cache-Control", "no-store")
	w.Header().Set("Referrer-Policy", "no-referrer")
	w.Header().Set("X-Frame-Options", "DENY")
	w.Header().Set("X-Content-Type-Options", "nosniff")
	w.WriteHeader(http.StatusNotFound)
	_, _ = w.Write(buf.Bytes())
}
