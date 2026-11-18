package runner

import (
	"crypto/tls"
	"fmt"
	"io"
	mrand "math/rand/v2"
	"net"
	"net/http"
	"path/filepath"
	"strconv"
	"strings"
	"testing"
	"time"

	"github.com/Maoyangui/m-ui/camo"
	"github.com/Maoyangui/m-ui/certutil"
)

func newCamoRunner(t *testing.T) *Runner {
	t.Helper()
	r, err := New(filepath.Join(t.TempDir(), "m.db"))
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { r.camoPortsClose(); r.Stop() })
	return r
}

func mustSet(t *testing.T, r *Runner, kv ...string) {
	t.Helper()
	for i := 0; i+1 < len(kv); i += 2 {
		if err := r.setSetting(kv[i], kv[i+1]); err != nil {
			t.Fatal(err)
		}
	}
}

// 页面伪装用不用 80 / 443 时要知道线路占着哪些 TCP 端口:只走 UDP 的 hysteria2 / tuic / hysteria 不算,配置坏了当没有。
func TestLinePortsTCP(t *testing.T) {
	got := linePortsTCP([]byte(`{"inbounds":[{"type":"anytls","listen_port":443},{"type":"hysteria2","listen_port":80},{"type":"tuic","listen_port":8443},` +
		`{"type":"vless","listen_port":2083},{"type":"direct","listen_port":0}]}`))
	if !got[443] || !got[2083] || got[80] || got[8443] || len(got) != 2 {
		t.Fatalf("%v", got)
	}
	if len(linePortsTCP(nil)) != 0 || len(linePortsTCP([]byte("{bad"))) != 0 {
		t.Fatal("没有配置 / 配置坏了应当没有")
	}
}

// 伪装站跟着设置走:没开是 nil;开了按模板生成,种子存进本机设置;改了设置 10 秒内用缓存,CamoApply 之后马上换;
// 跳转模式给 *camo.Redirect,网址坏了退回伪装站;「换一个样子」换种子;关掉又是 nil。测试里没有对账循环,CamoApply 不去占端口。
func TestCamoSourceFollowsSettings(t *testing.T) {
	r := newCamoRunner(t)
	if r.camoSource() != nil {
		t.Fatal("没开时应为 nil")
	}
	mustSet(t, r, "camoEnabled", "true")
	if r.camoSource() != nil {
		t.Fatal("10 秒的缓存里还是没开")
	}
	r.CamoApply()
	h := r.camoSource()
	if _, ok := h.(*camo.Site); !ok {
		t.Fatalf("开了应给伪装站: %T", h)
	}
	seed := r.setting("camoSeed")
	if seed == "" {
		t.Fatal("种子应存进本机设置")
	}
	mustSet(t, r, "camoTemplate", "video")
	if r.camoSource() != h {
		t.Fatal("缓存里不该变")
	}
	r.CamoApply()
	if s, ok := r.camoSource().(*camo.Site); !ok || s.Template() != "video" {
		t.Fatalf("CamoApply 之后应马上换成视频站: %T", r.camoSource())
	}
	mustSet(t, r, "camoTemplate", CamoRedirectMode, "camoRedirect", "https://www.example.com")
	r.CamoApply()
	if rd, ok := r.camoSource().(*camo.Redirect); !ok || rd.Target() != "https://www.example.com" {
		t.Fatalf("跳转模式: %T", r.camoSource())
	}
	mustSet(t, r, "camoRedirect", "ftp://x")
	r.CamoApply()
	if _, ok := r.camoSource().(*camo.Site); !ok {
		t.Fatalf("网址坏了应退回伪装站: %T", r.camoSource())
	}
	mustSet(t, r, "camoTemplate", "mirror")
	r.CamoApply()
	before := r.camoSource().(*camo.Site)
	if err := r.CamoReseed(); err != nil {
		t.Fatal(err)
	}
	after := r.camoSource().(*camo.Site)
	if r.setting("camoSeed") == seed || after == before {
		t.Fatal("换一个样子应换种子、重新生成")
	}
	mustSet(t, r, "camoEnabled", "false")
	r.CamoApply()
	if r.camoSource() != nil {
		t.Fatal("关掉后应为 nil")
	}
}

// freeTCP 测试用的端口挑在临时端口范围(Linux 默认 32768~60999)以外:系统给 ":0" 的端口就在这个范围里,
// 测试里发出去的连接也从这个范围取本地端口,恰好取到同一个号时伪装站再监听就撞上 address already in use(CI 上遇到过)。
func freeTCP(t *testing.T) int {
	t.Helper()
	for i := 0; i < 500; i++ {
		p := 20000 + mrand.IntN(12000)
		ln, err := net.Listen("tcp", ":"+strconv.Itoa(p))
		if err == nil {
			ln.Close()
			return p
		}
	}
	t.Fatal("找不到空闲端口")
	return 0
}

func camoPortStates(r *Runner) map[int]CamoPortState {
	out := map[int]CamoPortState{}
	for _, s := range r.CamoPorts() {
		out[s.Port] = s
	}
	return out
}

// 80 / 443(测试里换成两个空闲端口)的对账:空着才占;没有证书 443 不开;有了证书 443 开起来、80 跳到 https;
// 签证书时让出 80(防火墙照样放行),放回去后下一轮重新占上;面板要改到这个端口时让出来、两分钟内不占;
// 线路要用时重载前让出、重载完之前也不去占;这台的面板端口不占;别的程序占着写明原因;关掉就全放掉。
func TestCamoPorts(t *testing.T) {
	r := newCamoRunner(t)
	oldHTTP, oldHTTPS := camoHTTP, camoHTTPS
	camoHTTP, camoHTTPS = freeTCP(t), freeTCP(t)
	t.Cleanup(func() { camoHTTP, camoHTTPS = oldHTTP, oldHTTPS })
	camo.SetSource(r.camoSource)
	t.Cleanup(func() { camo.SetSource(nil) })
	mustSet(t, r, "camoEnabled", "true", "camoPorts", "true", "camoTemplate", "blog")
	r.CamoApply()
	r.camoPortsSync()
	want := func(port int, on bool, why string) {
		t.Helper()
		st := camoPortStates(r)[port]
		if st.On != on || st.Why != why {
			t.Fatalf("端口 %d: %+v,应为 on=%v why=%q", port, st, on, why)
		}
	}
	want(camoHTTP, true, "")
	want(camoHTTPS, false, "nocert")
	noFollow := &http.Client{Timeout: 5 * time.Second, Transport: &http.Transport{DisableKeepAlives: true}, CheckRedirect: func(*http.Request, []*http.Request) error { return http.ErrUseLastResponse }}
	get := func(url, host string) (int, string, string) {
		t.Helper()
		req, _ := http.NewRequest(http.MethodGet, url, nil)
		if host != "" {
			req.Host = host
		}
		resp, err := noFollow.Do(req)
		if err != nil {
			t.Fatal(err)
		}
		defer resp.Body.Close()
		b, _ := io.ReadAll(resp.Body)
		return resp.StatusCode, resp.Header.Get("Location"), string(b)
	}
	site := r.camoSource().(*camo.Site)
	if code, _, body := get(fmt.Sprintf("http://127.0.0.1:%d/", camoHTTP), ""); code != 200 || !strings.Contains(body, site.Name()) {
		t.Fatalf("80 上应是伪装站: %d", code)
	}

	dir := t.TempDir()
	certFile, keyFile := filepath.Join(dir, "c.pem"), filepath.Join(dir, "k.pem")
	if err := certutil.GenerateSelfSigned([]string{"localhost"}, certFile, keyFile, 30); err != nil {
		t.Fatal(err)
	}
	mustSet(t, r, "certFile", certFile, "keyFile", keyFile)
	r.camoPortsSync()
	want(camoHTTPS, true, "")
	tc := &http.Client{Timeout: 5 * time.Second, Transport: &http.Transport{DisableKeepAlives: true, TLSClientConfig: &tls.Config{InsecureSkipVerify: true, ServerName: "localhost"}}}
	resp, err := tc.Get(fmt.Sprintf("https://127.0.0.1:%d/", camoHTTPS))
	if err != nil {
		t.Fatal(err)
	}
	b, _ := io.ReadAll(resp.Body)
	resp.Body.Close()
	if resp.StatusCode != 200 || !strings.Contains(string(b), site.Name()) || resp.TLS == nil {
		t.Fatalf("443 上应是 https 的伪装站: %d", resp.StatusCode)
	}
	if code, loc, _ := get(fmt.Sprintf("http://127.0.0.1:%d/posts/x?y=1", camoHTTP), "localhost:"+strconv.Itoa(camoHTTP)); code != 301 || loc != fmt.Sprintf("https://localhost:%d/posts/x?y=1", camoHTTPS) {
		t.Fatalf("443 也开着时 80 应 301 到 https: %d %q", code, loc)
	}

	release := r.camoHold80()
	want(camoHTTP, false, "acme")
	fw := r.camoFirewallPorts()
	if len(fw) != 2 {
		t.Fatalf("签证书时让出的 80 防火墙照样放行: %v", fw)
	}
	r.camoPortsSync()
	want(camoHTTP, false, "acme")
	release()
	release() // 多调一次不会把计数减成负的
	r.camoPortsSync()
	want(camoHTTP, true, "")

	r.CamoReleasePort(camoHTTP)
	want(camoHTTP, false, "service")
	r.camoPortsSync()
	want(camoHTTP, false, "service")
	r.camo.mu.Lock()
	delete(r.camo.holdUntil, camoHTTP)
	r.camo.mu.Unlock()
	r.camoPortsSync()
	want(camoHTTP, true, "")

	r.camoPortsYield([]byte(fmt.Sprintf(`{"inbounds":[{"type":"anytls","listen_port":%d},{"type":"hysteria2","listen_port":%d}]}`, camoHTTPS, camoHTTP)))
	want(camoHTTPS, false, "line")
	want(camoHTTP, true, "") // 只走 UDP 的 hysteria2 不冲突
	r.camoPortsSync()
	want(camoHTTPS, false, "line") // 重载完之前已生效的配置里还没有它,也不去占
	r.camoPortsYield([]byte(`{"inbounds":[]}`))
	r.camoPortsSync()
	want(camoHTTPS, true, "")

	mustSet(t, r, "webPort", strconv.Itoa(camoHTTP))
	r.camoPortsSync()
	want(camoHTTP, false, "service")
	mustSet(t, r, "webPort", "2053")
	r.camoPortsSync()
	want(camoHTTP, true, "")

	mustSet(t, r, "camoPorts", "false")
	r.camoPortsSync()
	if len(r.CamoPorts()) != 0 || len(r.camoFirewallPorts()) != 0 {
		t.Fatalf("关掉后应全放掉: %v", r.CamoPorts())
	}
	if _, err := noFollow.Get(fmt.Sprintf("http://127.0.0.1:%d/", camoHTTP)); err == nil {
		t.Fatal("关掉后端口不该还在听")
	}
	other, err := net.Listen("tcp", ":"+strconv.Itoa(camoHTTP))
	if err != nil {
		t.Fatal(err)
	}
	defer other.Close()
	mustSet(t, r, "camoPorts", "true")
	r.camoPortsSync()
	if st := camoPortStates(r)[camoHTTP]; st.On || st.Why != "busy" || st.Info == "" {
		t.Fatalf("别的程序占着应写明: %+v", st)
	}
}

// 跳到 https 的地址:去掉原来的端口,IPv6 加方括号,不是 443 时带上端口;没有主机名不跳。
func TestHTTPSURL(t *testing.T) {
	for _, c := range []struct {
		host string
		port int
		uri  string
		want string
	}{
		{"example.com", 443, "/a?b=1", "https://example.com/a?b=1"},
		{"example.com:80", 443, "/", "https://example.com/"},
		{"[2001:db8::1]:80", 443, "/x", "https://[2001:db8::1]/x"},
		{"2001:db8::1", 8443, "/x", "https://[2001:db8::1]:8443/x"},
		{"example.com", 8443, "*", "https://example.com:8443/"},
		{"", 443, "/", ""},
	} {
		if got := httpsURL(c.host, c.port, c.uri); got != c.want {
			t.Errorf("%q %d %q: %q,应为 %q", c.host, c.port, c.uri, got, c.want)
		}
	}
}
