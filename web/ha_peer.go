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

// 主机容灾新增接口的限流(按来源 IP):同伴接口最严;面板上的读宽松(换主机的进度每 1.5 秒问一次),
// 写(设为主机、接任、改设置…)每分钟十来次;agent 接口只有主机在调,只防洪水。
var (
	peerLimiter    = &ipLimiter{every: rate.Every(2 * time.Second), burst: 10, m: map[string]*limiterEntry{}}
	haReadLimiter  = &ipLimiter{every: rate.Every(250 * time.Millisecond), burst: 60, m: map[string]*limiterEntry{}}
	haWriteLimiter = &ipLimiter{every: rate.Every(5 * time.Second), burst: 8, m: map[string]*limiterEntry{}}
	haAgentLimiter = &ipLimiter{every: rate.Every(50 * time.Millisecond), burst: 200, m: map[string]*limiterEntry{}}
)

// haLimited 面板上的主机容灾接口:超过频率就回 429,返回 true。
func haLimited(w http.ResponseWriter, r *http.Request) bool {
	l := haReadLimiter
	if r.Method != http.MethodGet && r.Method != http.MethodHead {
		l = haWriteLimiter
	}
	if l.allow(peerIP(r)) {
		return false
	}
	writeJSON(w, http.StatusTooManyRequests, map[string]string{"error": "请求太频繁"})
	return true
}

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

// haDNSTest 主机:只读地测一下 Cloudflare 令牌和要跟着主机走的域名(不改任何记录;令牌不回显)。
func (s *Server) haDNSTest(w http.ResponseWriter, r *http.Request, h *ha.Service) {
	var body struct {
		Token string `json:"token"`
		Names string `json:"names"`
	}
	if !decodeSmall(w, r, &body) {
		return
	}
	res, err := h.DNSTest(body.Token, body.Names)
	if err != nil {
		badRequest(w, err)
		return
	}
	writeJSON(w, http.StatusOK, res)
}
