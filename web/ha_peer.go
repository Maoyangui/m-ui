package web

import (
	"io"
	"net/http"
	"sync"
	"time"

	"github.com/Maoyangui/m-ui/ha"
	"github.com/Maoyangui/m-ui/logger"

	"golang.org/x/time/rate"
)

// handleHAPeer 副机之间互相确认主机是否失联、投票(POST /api/ha/peer/status|vote,集群密钥 HMAC 鉴权,不要会话)。
// 只在副机上有;按来源 IP 限流(每分钟 30 次),鉴权失败延时 200 毫秒、不说哪一项不对。
func (s *Server) handleHAPeer(w http.ResponseWriter, r *http.Request, h *ha.Service, action string) {
	if r.Method != http.MethodPost || (action != "status" && action != "vote") {
		http.NotFound(w, r)
		return
	}
	if !peerLimiter.allow(peerIP(r)) {
		writeJSON(w, http.StatusTooManyRequests, map[string]string{"error": "请求太频繁"})
		return
	}
	body, err := io.ReadAll(http.MaxBytesReader(w, r.Body, 16<<10))
	if err != nil {
		badRequest(w, errBadRequest)
		return
	}
	rep, err := h.ServePeer(action, r.Header, body)
	if err != nil {
		time.Sleep(200 * time.Millisecond)
		logger.Warning("容灾:同伴接口鉴权失败,来源 ", clientIP(r))
		writeJSON(w, http.StatusUnauthorized, map[string]string{"error": "鉴权失败"})
		return
	}
	for k, vs := range rep.Header {
		for _, v := range vs {
			w.Header().Add(k, v)
		}
	}
	w.WriteHeader(rep.Status)
	_, _ = w.Write(rep.Body)
}

// ipLimiter 按来源 IP(IPv6 按 /64)限流。
type ipLimiter struct {
	mu    sync.Mutex
	every rate.Limit
	burst int
	m     map[string]*limiterEntry
}

type limiterEntry struct {
	l    *rate.Limiter
	seen time.Time
}

var peerLimiter = &ipLimiter{every: rate.Every(2 * time.Second), burst: 10, m: map[string]*limiterEntry{}}

func (l *ipLimiter) allow(ip string) bool {
	key := loginKey(ip)
	l.mu.Lock()
	defer l.mu.Unlock()
	now := time.Now()
	if len(l.m) > 4096 { // 扫描器换着地址来:过期的清掉
		for k, e := range l.m {
			if now.Sub(e.seen) > 10*time.Minute {
				delete(l.m, k)
			}
		}
	}
	e := l.m[key]
	if e == nil {
		e = &limiterEntry{l: rate.NewLimiter(l.every, l.burst)}
		l.m[key] = e
	}
	e.seen = now
	return e.l.Allow()
}

// haDNSTest 主机:只读地测一下 Cloudflare 令牌和要跟着主机走的域名。最后一步补上。
func (s *Server) haDNSTest(w http.ResponseWriter, r *http.Request, h *ha.Service) {
	http.NotFound(w, r)
}
