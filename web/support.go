package web

import (
	"html/template"
	"net/http"
	"strings"

	"github.com/fangjunsheng555/m-ui/brand"

	"github.com/skip2/go-qrcode"
)

// "建议 / 支持"页:作者联系方式与打赏地址(取自 brand 包),按语言渲染;不含任何面板数据,无需登录。
var supportTmpl = template.Must(template.ParseFS(assets, "assets/support.html"))

func (s *Server) handleSupport(w http.ResponseWriter, r *http.Request) {
	lang := r.URL.Query().Get("lang")
	if lang != "zh" && lang != "en" {
		lang = "en"
		if strings.Contains(strings.ToLower(r.Header.Get("Accept-Language")), "zh") {
			lang = "zh"
		}
	}
	w.Header().Set("Content-Type", "text/html; charset=utf-8")
	w.Header().Set("Cache-Control", "no-cache")
	_ = supportTmpl.Execute(w, map[string]interface{}{
		"Lang": lang, "Telegram": brand.Telegram, "TelegramURL": brand.TelegramURL,
		"Tron": brand.Tron, "TronHead": brand.Tron[:4], "TronTail": brand.Tron[len(brand.Tron)-4:],
		"Repo": brand.Repo,
	})
}

// handleSupportQR 打赏地址二维码(PNG)。
func (s *Server) handleSupportQR(w http.ResponseWriter, r *http.Request) {
	png, err := qrcode.Encode(brand.Tron, qrcode.Medium, 300)
	if err != nil {
		badRequest(w, err)
		return
	}
	w.Header().Set("Content-Type", "image/png")
	w.Header().Set("Cache-Control", "public, max-age=86400")
	w.Write(png)
}
