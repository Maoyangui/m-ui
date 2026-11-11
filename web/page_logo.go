package web

import (
	"bytes"
	"crypto/sha256"
	"encoding/base64"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"image"
	"image/gif"
	"image/jpeg"
	"image/png"
	"io"
	"net/http"
	"strings"
	"time"
	"unicode/utf8"

	"github.com/Maoyangui/m-ui/database/model"

	"gorm.io/gorm"
	"gorm.io/gorm/clause"
)

// 落地页 logo:主面板 /api/sub-logo(reseller_id = 0),代理面板 /api/self/logo(只能是当前代理自己的)。
//
// 浏览器先把用户选的图(PNG / JPEG / GIF / WebP / SVG)用 canvas 缩到 480×160 以内、转成 PNG 再传(assets/js/logo.js),
// 顺带去掉 EXIF、SVG 里的脚本与外链。这里不信任前端,自己再校验一遍:只认 PNG / JPEG / GIF 的文件头,≤ 64 KB,
// 宽高 1~1024,并完整解码一次(截断、损坏的拦下)。SVG 一概不收 —— 订阅端口不用登录、和落地页同源,
// SVG 被直接打开就是能跑脚本的页面;落地页那边的 /logo 也只输出这三种格式(sub/logo.go)。

const (
	maxLogoBytes = 64 << 10  // 图片本身
	maxLogoBody  = 128 << 10 // 请求体:base64 比原图大三分之一,再加 JSON 外壳
	maxLogoSide  = 1024
)

type logoImage struct {
	mime          string
	data          []byte
	width, height int
	sum           string // sha256 前 8 字节的十六进制
}

// decodeLogo 解析 data:image/…;base64,… 或纯 base64 并校验。
func decodeLogo(s string) (logoImage, error) {
	s = strings.TrimSpace(s)
	if strings.HasPrefix(s, "data:") {
		i := strings.IndexByte(s, ',')
		if i < 0 || !strings.HasSuffix(strings.ToLower(s[:i]), ";base64") {
			return logoImage{}, errors.New("图片数据格式不对")
		}
		s = s[i+1:]
	}
	raw, err := base64.StdEncoding.DecodeString(s)
	if err != nil || len(raw) == 0 {
		return logoImage{}, errors.New("图片数据格式不对")
	}
	if len(raw) > maxLogoBytes {
		return logoImage{}, errors.New("logo 不能超过 64 KB")
	}
	// 按文件头选解码器,不走 image 包的全局格式表:别的依赖注册了 WebP、BMP 之类,这里也不认
	var (
		mime   string
		cfg    image.Config
		decode func(io.Reader) (image.Image, error)
	)
	switch {
	case bytes.HasPrefix(raw, []byte("\x89PNG\r\n\x1a\n")):
		mime, decode = "image/png", png.Decode
		cfg, err = png.DecodeConfig(bytes.NewReader(raw))
	case bytes.HasPrefix(raw, []byte("\xff\xd8\xff")):
		mime, decode = "image/jpeg", jpeg.Decode
		cfg, err = jpeg.DecodeConfig(bytes.NewReader(raw))
	case bytes.HasPrefix(raw, []byte("GIF87a")), bytes.HasPrefix(raw, []byte("GIF89a")):
		mime, decode = "image/gif", gif.Decode // 动图只解第一帧
		cfg, err = gif.DecodeConfig(bytes.NewReader(raw))
	default:
		return logoImage{}, errors.New("只支持 PNG / JPEG / GIF 图片")
	}
	if err != nil {
		return logoImage{}, errors.New("图片数据不完整或已损坏")
	}
	if cfg.Width < 1 || cfg.Height < 1 || cfg.Width > maxLogoSide || cfg.Height > maxLogoSide {
		return logoImage{}, errors.New("图片尺寸要在 1024×1024 以内")
	}
	if _, err := decode(bytes.NewReader(raw)); err != nil { // 文件头对、数据截断或坏了的
		return logoImage{}, errors.New("图片数据不完整或已损坏")
	}
	sum := sha256.Sum256(raw)
	return logoImage{mime: mime, data: raw, width: cfg.Width, height: cfg.Height, sum: hex.EncodeToString(sum[:8])}, nil
}

// logoView 接口返回的样子:没设是 {set:false};设了带上图片本身(data URI),面板直接拿去预览。
func logoView(row *model.PageLogo) map[string]interface{} {
	if row == nil || row.Sum == "" {
		return map[string]interface{}{"set": false}
	}
	return map[string]interface{}{
		"set": true, "sum": row.Sum, "mime": row.Mime, "width": row.Width, "height": row.Height, "size": len(row.Data),
		"data": "data:" + row.Mime + ";base64," + base64.StdEncoding.EncodeToString(row.Data),
	}
}

// handlePageLogo 某个归属(0 = 主面板,> 0 = 代理)的 logo:GET 读,POST / PUT 上传(立即生效),DELETE 恢复默认。
// audit 记操作:上传是 logo,恢复默认是 logo-reset。
func (s *Server) handlePageLogo(w http.ResponseWriter, r *http.Request, resellerID uint, audit func(action string)) {
	w.Header().Set("Cache-Control", "no-store")
	switch r.Method {
	case http.MethodGet:
		var row model.PageLogo
		if err := s.db.Where("reseller_id = ?", resellerID).Limit(1).Find(&row).Error; err != nil {
			badRequest(w, err)
			return
		}
		writeJSON(w, http.StatusOK, logoView(&row))
	case http.MethodPost, http.MethodPut:
		r.Body = http.MaxBytesReader(w, r.Body, maxLogoBody)
		var body struct {
			Data string `json:"data"`
		}
		if err := json.NewDecoder(r.Body).Decode(&body); err != nil {
			var tooBig *http.MaxBytesError
			if errors.As(err, &tooBig) {
				badRequest(w, errors.New("logo 不能超过 64 KB"))
				return
			}
			badRequest(w, errors.New("图片数据格式不对"))
			return
		}
		img, err := decodeLogo(body.Data)
		if err != nil {
			badRequest(w, err)
			return
		}
		row := model.PageLogo{ResellerId: resellerID, Mime: img.mime, Data: img.data, Sum: img.sum,
			Width: img.width, Height: img.height, UpdatedAt: time.Now().Unix()}
		// 代理的 logo:请求体读完(慢网络上要几秒)时这个代理可能已经被删了。和删代理的事务一样一开始就取写锁
		// (库连接是 _txlock=immediate),在同一个事务里确认代理还在再写:要么先写、删代理时一起删掉,要么代理已删、这里报错,
		// 不会留下没主的一行(没有界面删得掉,却每次都随快照推给副机)
		err = s.db.Transaction(func(tx *gorm.DB) error {
			if resellerID > 0 {
				var n int64
				if err := tx.Model(&model.Reseller{}).Where("id = ?", resellerID).Count(&n).Error; err != nil {
					return err
				}
				if n == 0 {
					return errors.New("代理不存在")
				}
			}
			return tx.Clauses(clause.OnConflict{
				Columns:   []clause.Column{{Name: "reseller_id"}},
				DoUpdates: clause.AssignmentColumns([]string{"mime", "data", "sum", "width", "height", "updated_at"}),
			}).Create(&row).Error
		})
		if err != nil {
			badRequest(w, err)
			return
		}
		audit("logo")
		writeJSON(w, http.StatusOK, logoView(&row))
	case http.MethodDelete:
		if err := s.db.Where("reseller_id = ?", resellerID).Delete(&model.PageLogo{}).Error; err != nil {
			badRequest(w, err)
			return
		}
		audit("logo-reset")
		writeJSON(w, http.StatusOK, logoView(nil))
	default:
		writeJSON(w, http.StatusMethodNotAllowed, map[string]string{"error": "方法不允许"})
	}
}

// handleSubLogo 主面板的落地页 logo(设置 → 订阅页)。副机只读:写请求被 masterOnly 拦下,logo 随主机的快照来。
func (s *Server) handleSubLogo(w http.ResponseWriter, r *http.Request) {
	if scope(r) > 0 { // 只注册在主面板上,代理会话本来进不来;再挡一道
		writeJSON(w, http.StatusForbidden, map[string]string{"error": "无权限"})
		return
	}
	s.handlePageLogo(w, r, 0, func(action string) { s.audit(r, "settings", action, "subPageLogo") })
}

// 落地页公告与联系方式的长度上限(按字数):公告随快照下发、每次打开落地页都要过一遍链接识别;联系方式还会出现在页脚。
const (
	maxPageNotice  = 4000
	maxPageSupport = 300
)

// validatePageText 主面板设置与代理面板保存落地页文案时共用。按去掉首尾空白后的字数算,存的也是去掉首尾空白的
// (主面板保存设置一律去首尾空白,代理面板也一样),首尾垫空白绕不过上限。
func validatePageText(notice, support string) error {
	if utf8.RuneCountInString(strings.TrimSpace(notice)) > maxPageNotice {
		return fmt.Errorf("公告太长了,上限 %d 字", maxPageNotice)
	}
	if utf8.RuneCountInString(strings.TrimSpace(support)) > maxPageSupport {
		return fmt.Errorf("联系方式太长了,上限 %d 字", maxPageSupport)
	}
	return nil
}

// validatePageTextChange 保存时只拦这次改了的:和库里现有的一样(升级前、或从旧备份还原的超长文字原样提交回来)就放行。
// 设置页与代理面板都是整组提交,不放行的话改同一组里的任何一项(比如关掉临时共享)都会被这段旧文字挡住。
func validatePageTextChange(notice, oldNotice, support, oldSupport string) error {
	if strings.TrimSpace(notice) == strings.TrimSpace(oldNotice) {
		notice = ""
	}
	if strings.TrimSpace(support) == strings.TrimSpace(oldSupport) {
		support = ""
	}
	return validatePageText(notice, support)
}
