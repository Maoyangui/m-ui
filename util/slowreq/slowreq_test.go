package slowreq

import (
	"io"
	"net/http"
	"net/http/httptest"
	"strconv"
	"strings"
	"sync"
	"testing"
	"time"
)

// clock 可拨的钟:处理函数里拨一下就等于"处理了这么久",测试不用真等 3 秒。
type clock struct {
	mu sync.Mutex
	t  time.Time
}

func (c *clock) now() time.Time {
	c.mu.Lock()
	defer c.mu.Unlock()
	return c.t
}

func (c *clock) add(d time.Duration) {
	c.mu.Lock()
	c.t = c.t.Add(d)
	c.mu.Unlock()
}

func newLogger(c *clock, shape func(*http.Request) string) (*Logger, *[]string) {
	var got []string
	l := &Logger{Name: "面板", Shape: shape, Now: c.now, Log: func(m string) { got = append(got, m) }}
	return l, &got
}

func panelShape(r *http.Request) string { return RouteShape(r.URL.Path, "/app/") }

// 处理超过 3 秒记一条 WARNING:方法、路由形状、状态码、耗时、请求体大小;不到 3 秒的不记。
func TestSlowRequestLoggedFastNot(t *testing.T) {
	c := &clock{t: time.Unix(1_700_000_000, 0)}
	l, got := newLogger(c, panelShape)
	took := 3500 * time.Millisecond
	h := l.Wrap(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		io.Copy(io.Discard, r.Body)
		c.add(took)
		w.WriteHeader(http.StatusBadRequest)
	}))
	body := `{"name":"香港1","port":443}`
	h.ServeHTTP(httptest.NewRecorder(), httptest.NewRequest("PUT", "http://x/app/api/lines/12?token=secret", strings.NewReader(body)))
	if len(*got) != 1 {
		t.Fatalf("慢请求应记一条: %v", *got)
	}
	m := (*got)[0]
	for _, want := range []string{"面板慢请求", "PUT", "/api/lines/:id", "400", "3.5s", "请求体 " + strconv.Itoa(len(body)) + " 字节"} {
		if !strings.Contains(m, want) {
			t.Fatalf("日志缺 %q: %s", want, m)
		}
	}
	if strings.Contains(m, "secret") || strings.Contains(m, "lines/12") || strings.Contains(m, "?") {
		t.Fatalf("日志不能带查询串和 id: %s", m)
	}

	took = 2900 * time.Millisecond
	h.ServeHTTP(httptest.NewRecorder(), httptest.NewRequest("GET", "http://x/app/api/users", nil))
	if len(*got) != 1 {
		t.Fatalf("不到 3 秒的不记: %v", *got)
	}
}

// 没写状态码就是 200;分块上传没有声明长度时按实际读到的字节算。
func TestSlowRequestDefaultStatusAndChunkedSize(t *testing.T) {
	c := &clock{t: time.Unix(1_700_000_000, 0)}
	l, got := newLogger(c, panelShape)
	h := l.Wrap(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		io.Copy(io.Discard, r.Body)
		c.add(5 * time.Second)
	}))
	req := httptest.NewRequest("POST", "http://x/app/api/users/import", io.MultiReader(strings.NewReader("abcde")))
	req.ContentLength = -1
	h.ServeHTTP(httptest.NewRecorder(), req)
	if len(*got) != 1 || !strings.Contains((*got)[0], "→ 200") || !strings.Contains((*got)[0], "请求体 5 字节") || !strings.Contains((*got)[0], "/api/users/import") {
		t.Fatalf("状态码默认 200、请求体按实际读到的算: %v", *got)
	}
}

// 同一路由 1 分钟内最多记 1 条;别的路由不受影响;过了 1 分钟再记,并带出被压掉的条数。
func TestSlowRequestRateLimitedPerRoute(t *testing.T) {
	c := &clock{t: time.Unix(1_700_000_000, 0)}
	l, got := newLogger(c, panelShape)
	h := l.Wrap(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) { c.add(4 * time.Second) }))
	do := func(method, path string) {
		h.ServeHTTP(httptest.NewRecorder(), httptest.NewRequest(method, "http://x"+path, nil))
	}
	for i := 1; i <= 5; i++ {
		do("PUT", "/app/api/lines/"+strconv.Itoa(i)) // 不同 id 是同一路由
	}
	if len(*got) != 1 {
		t.Fatalf("同一路由 1 分钟内只记 1 条: %v", *got)
	}
	do("GET", "/app/api/lines/1") // 方法不同算另一路由
	do("PUT", "/app/api/upstreams/3")
	if len(*got) != 3 {
		t.Fatalf("别的路由各记各的: %v", *got)
	}
	c.add(time.Minute)
	do("PUT", "/app/api/lines/9")
	if len(*got) != 4 || !strings.Contains((*got)[3], "另有 4 次") {
		t.Fatalf("过了 1 分钟再记一条,带出压掉的条数: %v", *got)
	}
}

// 路由形状:去掉内部前缀与查询串,数字 id → :id,令牌 / 随机键 → :token,不在面板路径下的一律 {other}。
func TestRouteShape(t *testing.T) {
	for in, want := range map[string]string{
		"/app/":                     "/",
		"/app":                      "/",
		"/app/api/lines/12":         "/api/lines/:id",
		"/app/api/lines/sort":       "/api/lines/sort",
		"/app/api/nodes/3/reset-fp": "/api/nodes/:id/reset-fp",
		"/app/api/v1/users/alice":   "/api/v1/users/:name",
		"/app/api/v1/users/Alice9x8Kq2Lm7Zp/kick":   "/api/v1/users/:name/kick",
		"/app/api/v1/users/7/plan":                  "/api/v1/users/:id/plan",
		"/app/api/users/import":                     "/api/users/import",
		"/app/api/v1/x/Alice9x8Kq2Lm7Zp":            "/api/v1/x/:token",
		"/app/api/agent/3f9a1c0b7e2d4a6f8b1c9d0e":   "/api/agent/:token",
		"/app/assets/js/app.js":                     "/assets/js/app.js",
		"/app/api/backup/download/m-ui-20261112.db": "/api/backup/download/:token",
		"/secret-panel/api/lines":                   "{other}",
		"/":                                         "{other}",
		"/app/a/b/c/d/e/f/g/h":                      "/a/b/c/d/e/f/…",
	} {
		if got := RouteShape(in, "/app/"); got != want {
			t.Errorf("RouteShape(%q) = %q,应为 %q", in, got, want)
		}
	}
}

// 处理函数用 http.ResponseController 设读时限、Flush 时,套了一层照样够到底层连接。
func TestRecorderUnwrap(t *testing.T) {
	c := &clock{t: time.Unix(1_700_000_000, 0)}
	l, _ := newLogger(c, panelShape)
	srv := httptest.NewServer(l.Wrap(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		rc := http.NewResponseController(w)
		if err := rc.SetReadDeadline(time.Now().Add(time.Second)); err != nil {
			http.Error(w, err.Error(), 500)
			return
		}
		w.Write([]byte("ok"))
		if err := rc.Flush(); err != nil {
			t.Errorf("Flush: %v", err)
		}
	})))
	defer srv.Close()
	resp, err := http.Get(srv.URL + "/app/x")
	if err != nil {
		t.Fatal(err)
	}
	defer resp.Body.Close()
	if resp.StatusCode != 200 {
		b, _ := io.ReadAll(resp.Body)
		t.Fatalf("ResponseController 应能用: %d %s", resp.StatusCode, b)
	}
}
