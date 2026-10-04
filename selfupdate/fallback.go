package selfupdate

import (
	"context"
	"fmt"
	"net"
	"net/http"
	"net/url"
	"strconv"
	"sync/atomic"
	"time"
)

// 纯 IPv6 机器连不上 GitHub(github.com 与发布包的下载地址都没有 IPv6)。装了 WARP 的,本机有一个
// socks5 代理(默认 127.0.0.1:40000),出口是 Cloudflare 的 IPv4:直连连不上时经它再试一次,
// 检查更新与一键更新就能用。没装 WARP 或它没在跑,照旧报直连的错。下载内容照旧按 SHA256SUMS 校验。

var fallbackPort atomic.Pointer[func() int]

// SetFallbackPort 告诉更新器本机 WARP 代理的端口从哪读(设置里的 warpPort)。
func SetFallbackPort(f func() int) { fallbackPort.Store(&f) }

// warpProxy 本机 WARP 代理在听就返回它的 socks5 地址(域名交给代理解析),否则 nil。
func warpProxy() *url.URL {
	f := fallbackPort.Load()
	if f == nil {
		return nil
	}
	port := (*f)()
	if port <= 0 || port > 65535 {
		return nil
	}
	addr := net.JoinHostPort("127.0.0.1", strconv.Itoa(port))
	c, err := net.DialTimeout("tcp", addr, time.Second)
	if err != nil {
		return nil
	}
	c.Close()
	return &url.URL{Scheme: "socks5", Host: addr}
}

// doGitHub 发一个请求:直连连不上(网络错误,不是 HTTP 状态码)且本机 WARP 在听时,经 WARP 再发一次。
// newReq 每次新建请求;base 是直连用的 client,经 WARP 时沿用它的超时与重定向策略。
func doGitHub(ctx context.Context, base *http.Client, newReq func() (*http.Request, error)) (*http.Response, error) {
	req, err := newReq()
	if err != nil {
		return nil, err
	}
	resp, err := base.Do(req)
	if err == nil || ctx.Err() != nil {
		return resp, err
	}
	proxy := warpProxy()
	if proxy == nil {
		return nil, err
	}
	tr := http.DefaultTransport.(*http.Transport).Clone()
	tr.Proxy = http.ProxyURL(proxy)
	tr.DisableKeepAlives = true
	c := *base
	c.Transport = tr
	req2, e := newReq()
	if e != nil {
		return nil, err
	}
	resp2, err2 := c.Do(req2)
	if err2 != nil {
		return nil, fmt.Errorf("%w;经本机 WARP(%s)重试也失败: %v", err, proxy.Host, err2)
	}
	return resp2, nil
}
