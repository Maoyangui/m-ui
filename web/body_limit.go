package web

import (
	"errors"
	"io"
	"net/http"
	"strings"
	"time"

	"github.com/Maoyangui/m-ui/logger"
	"github.com/Maoyangui/m-ui/notify"
)

// 请求体上限(审计 Q-32 #40):以前除登录等个别接口外都不限,登录后的一个几 GB 的 JSON 就能把内存撑爆
// (数据面在同一进程里,OOM 就是全员断线)。面板与代理面板的写接口、外部 API 统一封顶;
// multipart 上传(备份还原、旧面板导入)不套这个上限,它们各自按文件处理。
const (
	panelBodyLimit    = 8 << 20 // 主面板:最大的是批量建号 / 批量改用户、订阅模板、导入外部节点,几 MB 绰绰有余
	resellerBodyLimit = 1 << 20 // 代理面板:唯一的上传是落地页 logo(图片 ≤ 64 KB,接口自己再限 128 KB)
	apiBodyLimit      = 1 << 20 // 外部 API:单个用户的增改与套餐操作
)

// errBodyTooLarge 超过上限时解码器拿到的错误(原样的 "http: request body too large" 界面上看不懂)。
var errBodyTooLarge = errors.New("请求内容过大,超出上限")

// limitJSONBody 给有请求体的非 multipart 请求套上上限。
func limitJSONBody(w http.ResponseWriter, r *http.Request, n int64) {
	if r.Body == nil || r.Method == http.MethodGet || r.Method == http.MethodHead {
		return
	}
	if strings.HasPrefix(strings.ToLower(r.Header.Get("Content-Type")), "multipart/form-data") {
		return
	}
	r.Body = limitedBody{http.MaxBytesReader(w, r.Body, n)}
}

type limitedBody struct{ io.ReadCloser }

func (b limitedBody) Read(p []byte) (int, error) {
	n, err := b.ReadCloser.Read(p)
	var tooBig *http.MaxBytesError
	if errors.As(err, &tooBig) {
		return n, errBodyTooLarge
	}
	return n, err
}

// ---- 外部 API 鉴权失败限流 ----

// apiAuthBlocked 外部 API(主面板令牌与代理令牌共用一个计数):这个来源(IPv6 为所在 /64)10 分钟内失败 10 次,
// 先挡 5 分钟,期间带对的令牌也不放行 —— 不然令牌可以被无限次地试。和面板登录分开计数:调用方配错令牌不会把管理员锁在面板外。
func (s *Server) apiAuthBlocked(ip string) bool {
	s.mu.Lock()
	defer s.mu.Unlock()
	return failBlocked(s.apiFails, loginKey(ip), time.Now().Unix())
}

// noteAPIAuthFailure 记一次外部 API 鉴权失败:每次写日志;这个来源在窗口内第一次失败、以及被挡时各记一条审计
// (不是每次都写,扫描器撑不大审计表),被挡时再发一条 Telegram(和登录失败告警同一个开关)。
func (s *Server) noteAPIAuthFailure(r *http.Request, entry string) {
	ip := loginKey(peerIP(r))
	s.mu.Lock()
	if s.apiFails == nil {
		s.apiFails = map[string][]int64{}
	}
	n := failNote(s.apiFails, ip, time.Now().Unix())
	s.mu.Unlock()
	logger.Warning(entry, "外部 API 鉴权失败,来源 ", clientIP(r), " ", r.Method, " ", r.URL.Path)
	obj := map[string]interface{}{"ip": ip, "entry": entry, "method": r.Method, "path": r.URL.Path}
	switch n {
	case 1:
		s.auditAs("api", "api", "auth:fail", obj)
	case failLimit:
		obj["failures"] = n
		s.auditAs("api", "api", "auth:block", obj)
		if s.run != nil {
			s.run.Notifier().Event("tgOnLogin", "⚠️ <b>外部 API 令牌连续错误</b>\nIP:"+notify.Esc(ip)+" 10 分钟内失败 10 次,已暂停这个来源 5 分钟")
		}
	}
}

// 失败计数(面板登录与外部 API 共用的口径):failWindow 秒内失败 failLimit 次,最后一次失败后 failCooldown 秒内挡住。
const (
	failWindow   = 600
	failLimit    = 10
	failCooldown = 300
)

// failBlocked 调用方持锁。顺手把窗口外的记录丢掉;没失败过的来源不建条目,扫描器换着地址来也撑不大这张表。
func failBlocked(m map[string][]int64, key string, now int64) bool {
	fails, ok := m[key]
	if !ok {
		return false
	}
	var recent []int64
	for _, ts := range fails {
		if now-ts < failWindow {
			recent = append(recent, ts)
		}
	}
	m[key] = recent
	if len(recent) < failLimit {
		return false
	}
	return now-recent[len(recent)-1] < failCooldown
}

// failNote 调用方持锁。记一次失败,返回窗口内的失败次数。
func failNote(m map[string][]int64, key string, now int64) int {
	recent := m[key][:0]
	for _, ts := range m[key] {
		if now-ts < failWindow {
			recent = append(recent, ts)
		}
	}
	recent = append(recent, now)
	m[key] = recent
	return len(recent)
}

// reapFails 清掉窗口外的来源(调用方持锁)。
func reapFails(m map[string][]int64, now int64) {
	for ip, fails := range m {
		if len(fails) == 0 || now-fails[len(fails)-1] > failWindow {
			delete(m, ip)
		}
	}
}
