package web

import (
	"net/http"

	"github.com/Maoyangui/m-ui/ha"
)

// handleHAPeer 副机之间互相确认主机是否失联、投票(HMAC 鉴权)。第五步补上。
func (s *Server) handleHAPeer(w http.ResponseWriter, r *http.Request, h *ha.Service, action string) {
	http.NotFound(w, r)
}

// haDNSTest 主机:只读地测一下 Cloudflare 令牌和要跟着主机走的域名。最后一步补上。
func (s *Server) haDNSTest(w http.ResponseWriter, r *http.Request, h *ha.Service) {
	http.NotFound(w, r)
}
