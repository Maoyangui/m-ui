package web

import (
	"context"
	"encoding/json"
	"errors"
	"net/http"
	"strings"
	"time"

	"github.com/Maoyangui/m-ui/appdomains"
)

// routeApps 线路分流规则里"应用(查域名)"的数据来源;测试里换成假的。
var routeApps = appdomains.Default

const maxRouteApps = 20 // 一次最多查这么多个应用名

// handleRouteApps POST {names}:按应用名查域名(由面板所在的服务器联网去取),
// 结果只用来填进编辑框,保存的仍是普通的域名规则。
func (s *Server) handleRouteApps(w http.ResponseWriter, r *http.Request) {
	if r.Method != http.MethodPost {
		writeJSON(w, http.StatusMethodNotAllowed, map[string]string{"error": "方法不允许"})
		return
	}
	var in struct {
		Names []string `json:"names"`
	}
	if err := json.NewDecoder(http.MaxBytesReader(w, r.Body, 64<<10)).Decode(&in); err != nil {
		badRequest(w, err)
		return
	}
	var names []string
	for _, n := range in.Names {
		if n = strings.TrimSpace(n); n != "" {
			if len([]rune(n)) > 64 {
				badRequest(w, errors.New("应用名太长"))
				return
			}
			names = append(names, n)
		}
	}
	if len(names) == 0 {
		badRequest(w, errors.New("先填应用名"))
		return
	}
	if len(names) > maxRouteApps {
		badRequest(w, errors.New("一次最多查 20 个应用"))
		return
	}
	ctx, cancel := context.WithTimeout(r.Context(), 45*time.Second)
	defer cancel()
	writeJSON(w, http.StatusOK, map[string]interface{}{"results": routeApps.Lookup(ctx, names)})
}
