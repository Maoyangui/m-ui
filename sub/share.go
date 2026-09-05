package sub

import (
	"crypto/rand"
	"encoding/base64"
	"encoding/json"
	"net/http"
	"strings"
	"time"

	"github.com/Maoyangui/m-ui/creds"
	"github.com/Maoyangui/m-ui/database/model"
	"github.com/Maoyangui/m-ui/logger"
)

// 临时共享:用户在自己的订阅页生成一条随机地址给别人临时用。
//
// 共享地址发的是本人那份订阅(同线路、同外部节点),但**用一套单独的凭据**,
// 在数据面里挂在本人名下——流量、设备数、限速、到期都算本人,取消后凭据即被撤下,
// 已经拉走的节点也连不上。每人只有一条;共享地址只出原始订阅,不出订阅页,也不能生成/取消。

// shareEnabled 是否允许用户自助生成共享地址(设置 subShareEnabled,默认开)。
func (s *Server) shareEnabled() bool {
	return !strings.EqualFold(s.setting("subShareEnabled"), "false")
}

// shareSelfService 副机不提供生成/取消:用户表由主机下发,在副机改会被下一次同步覆盖。
func (s *Server) shareSelfService() bool {
	return s.shareEnabled() && !strings.EqualFold(s.setting("nodeMode"), "true")
}

// newShareToken 22 位随机地址,不可猜。
func newShareToken() string {
	b := make([]byte, 16)
	if _, err := rand.Read(b); err != nil {
		return ""
	}
	return base64.RawURLEncoding.EncodeToString(b)
}

// handleShare 处理订阅页上的生成/取消(POST ?share=on|off),完成后跳回订阅页。
func (s *Server) handleShare(w http.ResponseWriter, r *http.Request, subPath, key string, user model.User, rs *model.Reseller) {
	if !s.shareSelfService() || (rs != nil && !rs.ShareOn) {
		http.NotFound(w, r)
		return
	}
	upd := map[string]interface{}{"share_token": "", "share_creds": nil, "share_at": 0}
	on := r.URL.Query().Get("share") == "on"
	if on {
		tok := newShareToken()
		if tok == "" {
			http.Error(w, "生成失败", http.StatusInternalServerError)
			return
		}
		c, _ := json.Marshal(creds.Generate(user.Name))
		upd = map[string]interface{}{"share_token": tok, "share_creds": json.RawMessage(c), "share_at": time.Now().Unix()}
	}
	if err := s.db.Model(&model.User{}).Where("id = ?", user.Id).Updates(upd).Error; err != nil {
		logger.Warning("更新共享地址失败: ", err)
		http.Error(w, "保存失败", http.StatusInternalServerError)
		return
	}
	s.applyShare(user.Name, user.ShareToken != "") // 旧凭据被作废才需要断线
	// 直接把更新后的落地页返回(200),省掉一次跳转往返 —— 用户离服务器远时每趟要两三秒;页面脚本只替换共享那张卡。
	// 非浏览器(客户端 / curl)POST 完拿到的就是它平时拉的订阅内容。
	get := r.Clone(r.Context())
	get.Method = http.MethodGet
	get.URL.RawQuery = ""
	s.handle()(w, get)
}

// applyShare 让共享凭据即时生效/失效。
func (s *Server) applyShare(name string, kick bool) {
	if s.OnShareChange != nil {
		s.OnShareChange(name, kick)
	}
}
