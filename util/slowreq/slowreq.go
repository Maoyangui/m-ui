// Package slowreq 慢请求日志:一个 HTTP 请求处理超过阈值(默认 3 秒)就记一条 WARNING,
// 写明方法、路由形状、状态码、耗时、请求体大小,以后从生产日志就能直接看出慢在哪个接口。
//
// 路由形状不带查询串,也不带路径里的秘密:面板 / 代理面板的对外路径、订阅路径、订阅键、令牌一律换成占位
// (例如 /api/lines/:id、{sub}/:key)。同一路由(方法 + 形状)在 Every(默认 1 分钟)内最多记 1 条,
// 某个接口一直慢也不会刷屏;被压掉的条数在下一条里带出来。
package slowreq

import (
	"fmt"
	"io"
	"net/http"
	"strings"
	"sync"
	"time"

	"github.com/Maoyangui/m-ui/logger"
)

const (
	// DefaultThreshold 处理超过多久算慢。
	DefaultThreshold = 3 * time.Second
	// DefaultEvery 同一路由多久最多记一条。
	DefaultEvery = time.Minute
	maxRoutes    = 512 // 限频表最多记多少条路由;超了先清掉过期的,还超就整表清空(扫描器乱撞路径也撑不大)
)

// Logger 一个 HTTP 服务一份(面板、代理面板、订阅各自一份,限频互不影响)。零值字段取默认值。
type Logger struct {
	Name      string                       // 日志里的服务名,如"面板"
	Shape     func(r *http.Request) string // 路由形状(必须去掉一切秘密);在处理完以后调用,能看到改写过的路径
	Threshold time.Duration
	Every     time.Duration
	Log       func(msg string) // 测试里换掉;默认 logger.Warning
	Now       func() time.Time // 测试里换掉;默认 time.Now

	mu         sync.Mutex
	last       map[string]time.Time
	suppressed map[string]int
}

// Wrap 给 next 套上慢请求日志。
func (l *Logger) Wrap(next http.Handler) http.Handler {
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		start := l.now()
		rw := &recorder{ResponseWriter: w}
		var body *countingBody
		if r.Body != nil && r.Body != http.NoBody {
			body = &countingBody{ReadCloser: r.Body}
			r.Body = body
		}
		declared := r.ContentLength
		method := r.Method
		defer func() {
			d := l.now().Sub(start)
			if d < l.threshold() {
				return
			}
			size := declared
			if size < 0 { // 分块上传没有声明长度:按实际读到的算
				size = 0
				if body != nil {
					size = body.n
				}
			}
			shape := "/"
			if l.Shape != nil {
				shape = l.Shape(r)
			}
			l.note(method, shape, rw.status(), d, size)
		}()
		next.ServeHTTP(rw, r)
	})
}

func (l *Logger) note(method, shape string, status int, d time.Duration, size int64) {
	key := method + " " + shape
	now := l.now()
	l.mu.Lock()
	if l.last == nil {
		l.last, l.suppressed = map[string]time.Time{}, map[string]int{}
	}
	if t, ok := l.last[key]; ok && now.Sub(t) < l.every() {
		l.suppressed[key]++
		l.mu.Unlock()
		return
	}
	if len(l.last) >= maxRoutes {
		for k, t := range l.last {
			if now.Sub(t) >= l.every() {
				delete(l.last, k)
				delete(l.suppressed, k)
			}
		}
		if len(l.last) >= maxRoutes {
			l.last, l.suppressed = map[string]time.Time{}, map[string]int{}
		}
	}
	l.last[key] = now
	skipped := l.suppressed[key]
	delete(l.suppressed, key)
	l.mu.Unlock()

	name := l.Name
	if name == "" {
		name = "HTTP"
	}
	msg := fmt.Sprintf("%s慢请求: %s %s → %d,耗时 %s,请求体 %d 字节", name, method, shape, status, d.Round(time.Millisecond), size)
	if skipped > 0 {
		msg += fmt.Sprintf("(此前限频期间同一接口另有 %d 次慢请求未记)", skipped)
	}
	if l.Log != nil {
		l.Log(msg)
	} else {
		logger.Warning(msg)
	}
}

func (l *Logger) now() time.Time {
	if l.Now != nil {
		return l.Now()
	}
	return time.Now()
}

func (l *Logger) threshold() time.Duration {
	if l.Threshold > 0 {
		return l.Threshold
	}
	return DefaultThreshold
}

func (l *Logger) every() time.Duration {
	if l.Every > 0 {
		return l.Every
	}
	return DefaultEvery
}

// recorder 记下状态码;Unwrap 让 http.ResponseController(设读写时限、Flush)照样够到底层连接。
type recorder struct {
	http.ResponseWriter
	code int
}

func (w *recorder) WriteHeader(code int) {
	if w.code == 0 {
		w.code = code
	}
	w.ResponseWriter.WriteHeader(code)
}

func (w *recorder) Write(b []byte) (int, error) {
	if w.code == 0 {
		w.code = http.StatusOK
	}
	return w.ResponseWriter.Write(b)
}

func (w *recorder) Flush() {
	if f, ok := w.ResponseWriter.(http.Flusher); ok {
		if w.code == 0 {
			w.code = http.StatusOK
		}
		f.Flush()
	}
}

func (w *recorder) Unwrap() http.ResponseWriter { return w.ResponseWriter }

func (w *recorder) status() int {
	if w.code == 0 {
		return http.StatusOK // 什么都没写:net/http 收尾时按 200 回
	}
	return w.code
}

type countingBody struct {
	io.ReadCloser
	n int64
}

func (b *countingBody) Read(p []byte) (int, error) {
	n, err := b.ReadCloser.Read(p)
	b.n += int64(n)
	return n, err
}

// RouteShape 把内部路径(已改写到固定前缀 inner 之下)化成路由形状:去掉前缀,纯数字段换成 :id,
// 跟在 users/ 后面的用户名换成 :name(订阅键可能就是用户名),不像固定路由名的段(带不少数字、太长、
// 有路由名里不会出现的字符 —— 令牌、密钥、文件名之类)换成 :token。
// 不在 inner 之下的(对外路径没对上、根路径扫描)一律记成 {other},不把撞上来的路径原样写进日志。
func RouteShape(path, inner string) string {
	if !strings.HasPrefix(path, inner) {
		if path+"/" == inner {
			return "/"
		}
		return "{other}"
	}
	rest := strings.TrimPrefix(path, inner)
	if rest == "" {
		return "/"
	}
	segs := strings.Split(rest, "/")
	const maxSegs = 6
	if len(segs) > maxSegs {
		segs = append(segs[:maxSegs], "…")
	}
	for i, s := range segs {
		switch {
		case s == "" || s == "…":
		case isDigits(s):
			segs[i] = ":id"
		case i > 0 && segs[i-1] == "users" && !userActions[s]:
			segs[i] = ":name" // 外部 API 按用户名找人(/api/v1/users/alice):主面板用户的订阅键就是用户名
		case !isRouteWord(s):
			segs[i] = ":token"
		}
	}
	return "/" + strings.Join(segs, "/")
}

// userActions users/ 下面的固定动作(批量、导出、导入);其余跟在 users/ 后面的都当用户名打码。
var userActions = map[string]bool{"batch": true, "bulk": true, "export": true, "import": true}

func isDigits(s string) bool {
	for _, r := range s {
		if r < '0' || r > '9' {
			return false
		}
	}
	return s != ""
}

// isRouteWord 像不像固定的路由名:小写字母开头,只有小写字母、数字、- _ .,最长 32,数字不超过 3 个
// (v1、ipv6、app.js 这类放行;十六进制 / base64 令牌、随机键都过不了)。
func isRouteWord(s string) bool {
	if len(s) > 32 || s[0] < 'a' || s[0] > 'z' {
		return false
	}
	digits := 0
	for _, r := range s {
		switch {
		case r >= 'a' && r <= 'z', r == '-', r == '_', r == '.':
		case r >= '0' && r <= '9':
			digits++
		default:
			return false
		}
	}
	return digits <= 3
}
