package ext

import (
	"context"
	"errors"
	"fmt"
	"net/http"
	"net/http/httptest"
	"strings"
	"sync"
	"sync/atomic"
	"testing"
	"time"
)

const clashTwo = `proxies:
  - {name: a, type: hysteria2, server: 1.2.3.4, port: 443, password: p, sni: x.example.com, skip-cert-verify: true}
  - {name: b, type: tuic, server: 1.2.3.4, port: 444, uuid: 00000000-0000-0000-0000-000000000000, password: p, sni: x.example.com}
`

// 有的面板按客户端身份发不同内容:Clash 系给全量,通用身份只给一两种协议。抓取要先以 Clash Meta 身份要。
func TestFetchPrefersClashMetaAgent(t *testing.T) {
	var mu sync.Mutex
	var agents []string
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		ua := r.Header.Get("User-Agent")
		mu.Lock()
		agents = append(agents, ua)
		mu.Unlock()
		if strings.Contains(ua, "ClashMeta") {
			w.Header().Set("Content-Type", "text/yaml")
			w.Write([]byte(clashTwo))
			return
		}
		w.Write([]byte(b64("hysteria2://p@1.2.3.4:443?sni=x.example.com#only\n")))
	}))
	defer srv.Close()
	b, agent, err := Fetch(context.Background(), srv.URL, "")
	if err != nil {
		t.Fatal(err)
	}
	if it := Parse(string(b)); len(it.Nodes) != 2 || agent != "clash" {
		t.Fatalf("应拿到 Clash 身份那份的 2 个节点,得到 %d (%s)", len(it.Nodes), agent)
	}
	if len(agents) != 1 || !strings.Contains(agents[0], "ClashMeta") {
		t.Fatalf("第一份就解析出节点时不该再请求第二次: %v", agents)
	}
}

// Clash 身份拿到的解析不出节点(比如面板只对它发一段说明),要退回通用身份。
func TestFetchFallsBackToGenericAgent(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if strings.Contains(r.Header.Get("User-Agent"), "ClashMeta") {
			w.Write([]byte("please use another client"))
			return
		}
		w.Write([]byte(b64("hysteria2://p@1.2.3.4:443?sni=x.example.com#only\n")))
	}))
	defer srv.Close()
	for _, prev := range []string{"", "clash", "v2rayn"} {
		b, agent, err := Fetch(context.Background(), srv.URL, prev)
		if err != nil {
			t.Fatal(err)
		}
		if it := Parse(string(b)); len(it.Nodes) != 1 || agent != "v2rayn" {
			t.Fatalf("prev=%q:应退回通用身份拿到 1 个节点,得到 %d (%s)", prev, len(it.Nodes), agent)
		}
	}
}

// 两种身份都解析不出:把第一份交出去,调用方据此报"没有可识别的节点";不能当抓取失败。
func TestFetchReturnsFirstBodyWhenNothingParses(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Write([]byte("nothing here " + r.Header.Get("User-Agent")))
	}))
	defer srv.Close()
	b, _, err := Fetch(context.Background(), srv.URL, "")
	if err != nil {
		t.Fatal(err)
	}
	if !strings.Contains(string(b), "ClashMeta") {
		t.Fatalf("应返回第一份(Clash 身份)的原文,得到 %q", b)
	}
}

// 审计 I5:以前 Clash 身份遇到任何错误(429、403、5xx、超时)都改用通用身份,机场在 Cloudflare 后面间歇限流时
// 缓存在两种内容之间来回翻,隐藏随之失配。现在上次成功的身份出错就报错(调用方保留旧缓存),不换身份。
func TestFetchDoesNotFlipOnTransientError(t *testing.T) {
	var clashStatus atomic.Int32
	var generic atomic.Int32
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if strings.HasPrefix(r.Header.Get("User-Agent"), "ClashMeta") {
			if s := clashStatus.Load(); s != 0 {
				w.WriteHeader(int(s))
				return
			}
			fmt.Fprint(w, clashTwo)
			return
		}
		generic.Add(1)
		fmt.Fprint(w, b64("hysteria2://p@1.2.3.4:443?sni=x.example.com#only"))
	}))
	defer srv.Close()
	for _, code := range []int{http.StatusTooManyRequests, http.StatusForbidden, http.StatusBadGateway} {
		clashStatus.Store(int32(code))
		if _, _, err := Fetch(context.Background(), srv.URL, "clash"); err == nil || !strings.Contains(err.Error(), fmt.Sprint(code)) {
			t.Fatalf("HTTP %d:上次用 Clash 身份成功的,这次出错应报错保留旧缓存,得到 %v", code, err)
		}
	}
	if generic.Load() != 0 {
		t.Fatalf("不该换通用身份去抓: %d 次", generic.Load())
	}
	// 老数据没记身份时按 Clash 身份算
	if _, _, err := Fetch(context.Background(), srv.URL, "?"); err == nil {
		t.Fatal("老数据也不该换身份")
	}
	// 缓存本来就是通用身份给的:Clash 身份出错不算数,照样拿通用身份的
	b, agent, err := Fetch(context.Background(), srv.URL, "v2rayn")
	if err != nil || agent != "v2rayn" || len(Parse(string(b)).Nodes) != 1 {
		t.Fatalf("上次是通用身份时 Clash 身份出错不该影响: %v %s", err, agent)
	}
	// 还没有缓存(第一次抓):Clash 身份被拒就试通用身份
	if _, agent, err := Fetch(context.Background(), srv.URL, ""); err != nil || agent != "v2rayn" {
		t.Fatalf("第一次抓应退回通用身份: %v %s", err, agent)
	}
	// Clash 身份恢复了:哪怕上次是通用身份,也换回节点更全的那份
	clashStatus.Store(0)
	if _, agent, err := Fetch(context.Background(), srv.URL, "v2rayn"); err != nil || agent != "clash" {
		t.Fatalf("Clash 身份恢复后应换回: %v %s", err, agent)
	}
}

// 审计 L3:超过上限的订阅以前截到上限照样解析,截在节点列表中间时后面的节点悄悄丢了,没有任何提示。
// 现在超限的那份整份不用:换下一个身份(上次就是它给的也换);都没有能用的就明确报订阅太大。正好等于上限的照常用。
func TestFetchRejectsOversizedBody(t *testing.T) {
	var big strings.Builder
	big.WriteString("proxies:\n")
	for i := 0; i < 40; i++ {
		fmt.Fprintf(&big, "  - name: n%d\n    type: hysteria2\n    server: 1.2.3.4\n    port: %d\n    password: p\n    sni: x.example.com\n", i, 1000+i)
	}
	body := big.String()
	// 上限落在第 21 个节点的 sni 值中间:截到这里仍是合法 YAML,能解析出前 21 个(0.8.0 就会这样用,丢掉后面 19 个)
	cut := strings.Index(body, "name: n20\n")
	cut += strings.Index(body[cut:], "sni: x.exa") + len("sni: x.exa")
	old := maxBody
	maxBody = int64(cut)
	defer func() { maxBody = old }()
	if n := len(Parse(body[:cut]).Clash()); n == 0 || n >= 40 {
		t.Fatalf("测试数据不对:截到上限后应能解析出一部分节点,得 %d", n)
	}
	var clashBody, generic atomic.Value
	clashBody.Store(body)
	generic.Store(b64("hysteria2://p@1.2.3.4:443?sni=x.example.com#only"))
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if strings.HasPrefix(r.Header.Get("User-Agent"), "ClashMeta") {
			fmt.Fprint(w, clashBody.Load().(string))
			return
		}
		fmt.Fprint(w, generic.Load().(string))
	}))
	defer srv.Close()
	for _, prev := range []string{"", "clash", "v2rayn", "?"} {
		b, agent, err := Fetch(context.Background(), srv.URL, prev)
		if err != nil || agent != "v2rayn" || len(Parse(string(b)).Nodes) != 1 {
			t.Fatalf("prev=%q:超限的那份不该用,应换通用身份: %v %s", prev, err, agent)
		}
	}
	// 通用身份给的也解析不出节点:报订阅太大,而不是「没有可识别的节点」
	generic.Store("please use another client")
	if _, _, err := Fetch(context.Background(), srv.URL, "clash"); !errors.Is(err, errTooBig) {
		t.Fatalf("都用不了时应明确报订阅太大,得 %v", err)
	}
	// 通用身份给的有节点但进不了 Clash(本站不认识的链接):照旧交出这份,不因为另一份超限报错
	generic.Store("unknownproto://abc@1.2.3.4:443#x")
	if b, agent, err := Fetch(context.Background(), srv.URL, ""); err != nil || agent != "v2rayn" || len(Parse(string(b)).Nodes) != 1 {
		t.Fatalf("另一份有节点时应交出它: %v %s", err, agent)
	}
	// 正好等于上限:照常用
	clashBody.Store(body[:cut])
	if b, agent, err := Fetch(context.Background(), srv.URL, "clash"); err != nil || agent != "clash" || len(b) != cut {
		t.Fatalf("正好等于上限的应照常用: %v %s %d", err, agent, len(b))
	}
}

// 每个身份单独计时:以前两次请求共用调用方的 30 秒,第一次超时 25 秒后第二次只剩不到 5 秒。
func TestFetchEachAttemptHasOwnTimeout(t *testing.T) {
	old := fetchAttemptTimeout
	fetchAttemptTimeout = 300 * time.Millisecond
	defer func() { fetchAttemptTimeout = old }()
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if strings.HasPrefix(r.Header.Get("User-Agent"), "ClashMeta") {
			select {
			case <-time.After(2 * time.Second): // 比单次时限长:这一次一定超时
			case <-r.Context().Done():
			}
			return
		}
		time.Sleep(200 * time.Millisecond) // 只有单独计时才来得及
		fmt.Fprint(w, b64("hysteria2://p@1.2.3.4:443?sni=x.example.com#only"))
	}))
	defer srv.Close()
	ctx, cancel := context.WithTimeout(context.Background(), 700*time.Millisecond) // 两次各自的时限加起来
	defer cancel()
	start := time.Now()
	b, agent, err := Fetch(ctx, srv.URL, "")
	if err != nil || agent != "v2rayn" || len(Parse(string(b)).Nodes) != 1 {
		t.Fatalf("第一次超时后第二次应有自己的时限: %v %s (%v)", err, agent, time.Since(start))
	}
}
