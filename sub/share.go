package sub

import (
	"crypto/rand"
	"encoding/base64"
	"net/http"
	"strings"
	"time"

	"github.com/Maoyangui/m-ui/database/model"
	"github.com/Maoyangui/m-ui/logger"
)

// 临时共享:用户在自己的订阅页生成一条随机地址给别人临时用。
// 共享地址发的是同一份订阅(同凭据、同线路),所以流量、设备数、到期都算在本人头上;
// 每人只有一条,随时可取消;共享地址只出原始订阅,不出订阅页,也不能再生成/取消。

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
func (s *Server) handleShare(w http.ResponseWriter, r *http.Request, subPath string, user model.User) {
	if !s.shareSelfService() {
		http.NotFound(w, r)
		return
	}
	upd := map[string]interface{}{"share_token": "", "share_at": 0}
	if r.URL.Query().Get("share") == "on" {
		tok := newShareToken()
		if tok == "" {
			http.Error(w, "生成失败", http.StatusInternalServerError)
			return
		}
		upd = map[string]interface{}{"share_token": tok, "share_at": time.Now().Unix()}
	}
	if err := s.db.Model(&model.User{}).Where("id = ?", user.Id).Updates(upd).Error; err != nil {
		logger.Warning("更新共享地址失败: ", err)
		http.Error(w, "保存失败", http.StatusInternalServerError)
		return
	}
	http.Redirect(w, r, publicBase(r, subPath, user.Name), http.StatusSeeOther)
}
