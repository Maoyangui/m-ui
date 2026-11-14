package sub

import (
	"crypto/sha256"
	"encoding/base64"
	"encoding/hex"
	"html/template"
	"net/http"
	"net/url"
	"strconv"
	"strings"

	"github.com/Maoyangui/m-ui/brand"
	"github.com/Maoyangui/m-ui/database/model"
)

// 落地页 logo:主面板和代理在各自面板里上传(web/page_logo.go),存在 page_logos 表,随快照到副机。
// 页面里用相对地址 ./<地址>/logo?v=<摘要> 引用:同源,反代改了前缀也不坏;共享页里的地址段是共享令牌,不暴露本人用户名。
// 代理没传用主面板的,都没传就内联内置 logo(不走这个路由)。

const builtinIconType = "image/svg+xml"

// builtinLogoETag 内置 logo 的 ETag(页面打开期间自定义 logo 刚被删掉时,/logo 回落到它)。
var builtinLogoETag = func() string {
	sum := sha256.Sum256(brand.SVG)
	return `"m-ui-` + hex.EncodeToString(sum[:6]) + `"`
}()

// logoMimeOK 只认面板上传时校验过的三种格式;库里是别的(手改过库)就当没有。
func logoMimeOK(m string) bool {
	return m == "image/png" || m == "image/jpeg" || m == "image/gif"
}

// logoOwners 这个代理(nil = 主面板)的用户该看谁的 logo,按先后:代理自己的,再是主面板的。
func logoOwners(rs *model.Reseller) []uint {
	if rs != nil && rs.Id > 0 {
		return []uint{rs.Id, 0}
	}
	return []uint{0}
}

// pageLogoMeta 落地页要引用的 logo 的摘要与类型(都没有返回空)。只查这两列,不读图片。
func (s *Server) pageLogoMeta(rs *model.Reseller) (sum, mime string) {
	for _, id := range logoOwners(rs) {
		var row model.PageLogo
		if s.db.Select("sum", "mime").Where("reseller_id = ?", id).Limit(1).Find(&row).Error == nil && row.Sum != "" && logoMimeOK(row.Mime) {
			return row.Sum, row.Mime
		}
	}
	return "", ""
}

// pageLogo 同上,连图片一起读(/logo 路由用)。
func (s *Server) pageLogo(rs *model.Reseller) (model.PageLogo, bool) {
	for _, id := range logoOwners(rs) {
		var row model.PageLogo
		if s.db.Where("reseller_id = ?", id).Limit(1).Find(&row).Error == nil && row.Sum != "" && len(row.Data) > 0 && logoMimeOK(row.Mime) {
			return row, true
		}
	}
	return model.PageLogo{}, false
}

// pageIcon 落地页与客户端下载页的标签图标 / 页头 logo:有自定义的给 ./<key>/logo?v=<摘要> 和它的类型,否则内联内置 SVG。
// key 是这次访问用的那一段(用户名、订阅令牌或共享令牌)。
func (s *Server) pageIcon(rs *model.Reseller, key string) (template.URL, string) {
	if sum, mime := s.pageLogoMeta(rs); sum != "" {
		return template.URL("./" + url.PathEscape(key) + "/logo?v=" + url.QueryEscape(sum)), mime
	}
	return template.URL(brand.DataURI), builtinIconType
}

// inlineIcon 同 pageIcon 的取法(代理的用户先用代理的,再用主面板的,都没有用内置的),但把图内联成 data URI:
// 「订阅地址无效」页的地址对不上任何人,./<地址>/logo 在那里是 404,引用不到。上传上限 64 KiB,内联得起。
func (s *Server) inlineIcon(rs *model.Reseller) (template.URL, string) {
	if row, ok := s.pageLogo(rs); ok {
		return template.URL("data:" + row.Mime + ";base64," + base64.StdEncoding.EncodeToString(row.Data)), row.Mime
	}
	return template.URL(brand.DataURI), builtinIconType
}

// serveLogo GET <地址>/logo。图片是用户上传的(上传时解码校验过,只有 PNG / JPEG / GIF),照样加几道头兜底:
// 禁止类型嗅探、整个响应进沙箱(直接在地址栏打开也执行不了任何东西)、只许同源页面引用。
// 地址里的 v 等于当前摘要时长缓存(换图后摘要变、页面里的地址跟着变);对不上就每次来问,ETag 一致回 304。
// 没有自定义 logo(页面打开期间刚被删掉)给内置 SVG。和二维码一样不记订阅日志。
func (s *Server) serveLogo(w http.ResponseWriter, r *http.Request, rs *model.Reseller) {
	if r.Method != http.MethodGet && r.Method != http.MethodHead {
		http.NotFound(w, r)
		return
	}
	body, mime, etag, cache := brand.SVG, builtinIconType, builtinLogoETag, "no-cache"
	if row, ok := s.pageLogo(rs); ok {
		body, mime, etag = row.Data, row.Mime, `"`+row.Sum+`"`
		if r.URL.Query().Get("v") == row.Sum {
			cache = "private, max-age=2592000, immutable"
		}
	}
	h := w.Header()
	h.Set("Content-Type", mime)
	h.Set("X-Content-Type-Options", "nosniff")
	h.Set("Content-Security-Policy", "default-src 'none'; sandbox")
	h.Set("Cross-Origin-Resource-Policy", "same-origin")
	h.Set("Referrer-Policy", "no-referrer")
	h.Set("Cache-Control", cache)
	h.Set("ETag", etag)
	if etagMatches(r.Header.Get("If-None-Match"), etag) {
		w.WriteHeader(http.StatusNotModified)
		return
	}
	h.Set("Content-Length", strconv.Itoa(len(body)))
	w.WriteHeader(http.StatusOK)
	if r.Method != http.MethodHead {
		_, _ = w.Write(body)
	}
}

// etagMatches If-None-Match 里有没有这个 ETag(可能是逗号分隔的一串,也可能带弱校验前缀 W/,* 表示任意)。
func etagMatches(header, etag string) bool {
	for _, t := range strings.Split(header, ",") {
		t = strings.TrimSpace(t)
		if t == "*" || (t != "" && strings.TrimPrefix(t, "W/") == etag) {
			return true
		}
	}
	return false
}
