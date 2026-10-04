package acme

import (
	"context"
	"fmt"
	"net"
	"net/http"
	"strconv"
	"testing"
	"time"
)

// 探测服务:像 Cloudflare trace 那样把对端地址写成 ip=...
func traceServer(t *testing.T, ln net.Listener) {
	t.Helper()
	srv := &http.Server{Handler: http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		host, _, _ := net.SplitHostPort(r.RemoteAddr)
		fmt.Fprintf(w, "fl=1\nh=test\nip=%s\nts=1\n", host)
	})}
	go srv.Serve(ln)
	t.Cleanup(func() { srv.Close() })
}

// 两个地址族都用同一组探测地址
func withURLs(t *testing.T, urls ...string) {
	old := publicIPURLs
	publicIPURLs = map[string][]string{"tcp4": urls, "tcp6": urls}
	t.Cleanup(func() { publicIPURLs = old })
}

func testCtx(t *testing.T) context.Context {
	ctx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
	t.Cleanup(cancel)
	return ctx
}

// 双栈:同一个主机名既有 v4 又有 v6 时,两个地址各自测出来;PublicIP 给 IPv4(默认连接会优先走 v6)。
func TestPublicIPsDualStack(t *testing.T) {
	ips, err := net.DefaultResolver.LookupIP(context.Background(), "ip", "localhost")
	has4, has6 := false, false
	for _, ip := range ips {
		if ip.To4() != nil {
			has4 = true
		} else {
			has6 = true
		}
	}
	if err != nil || !has4 || !has6 {
		t.Skip("localhost 在这台机器上不是双栈解析,复现不了")
	}
	ln4, err := net.Listen("tcp4", "127.0.0.1:0")
	if err != nil {
		t.Fatal(err)
	}
	port := ln4.Addr().(*net.TCPAddr).Port
	ln6, err := net.Listen("tcp6", "[::1]:"+strconv.Itoa(port))
	if err != nil {
		ln4.Close()
		t.Skip("本机没有 IPv6 回环,复现不了: ", err)
	}
	traceServer(t, ln4)
	traceServer(t, ln6)
	withURLs(t, "http://localhost:"+strconv.Itoa(port)+"/")

	ctx := testCtx(t)
	if v4, v6 := PublicIPs(ctx); v4 != "127.0.0.1" || v6 != "::1" {
		t.Fatalf("双栈下应分别测到 127.0.0.1 与 ::1,得到 %q %q", v4, v6)
	}
	if got := publicIP(ctx); got != "127.0.0.1" {
		t.Fatalf("双栈下 PublicIP 应给 IPv4,得到 %q", got)
	}
}

// 纯 IPv6:IPv4 测不到,PublicIP 给 v6 地址(不让纯 v6 机器变成没地址)。
func TestPublicIPIPv6Only(t *testing.T) {
	ln6, err := net.Listen("tcp6", "[::1]:0")
	if err != nil {
		t.Skip("本机没有 IPv6 回环: ", err)
	}
	traceServer(t, ln6)
	withURLs(t, "http://"+ln6.Addr().String()+"/")
	ctx := testCtx(t)
	if v4, v6 := PublicIPs(ctx); v4 != "" || v6 != "::1" {
		t.Fatalf("纯 IPv6 应只测到 ::1,得到 %q %q", v4, v6)
	}
	if got := publicIP(ctx); got != "::1" {
		t.Fatalf("纯 IPv6 时 PublicIP 应给 v6 地址,得到 %q", got)
	}
}

// 回落的 ipify 只回一个裸 IP;不是 IP 的内容一律不认;地址族对不上的结果也不认。
func TestPublicIPParsesPlainAndRejectsMismatch(t *testing.T) {
	body := "203.0.113.5"
	ln, err := net.Listen("tcp4", "127.0.0.1:0")
	if err != nil {
		t.Fatal(err)
	}
	srv := &http.Server{Handler: http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) { fmt.Fprint(w, body) })}
	go srv.Serve(ln)
	t.Cleanup(func() { srv.Close() })
	withURLs(t, "http://"+ln.Addr().String()+"/")
	ctx := testCtx(t)
	if got := publicIP(ctx); got != "203.0.113.5" {
		t.Fatalf("裸 IP 应原样返回,得到 %q", got)
	}
	body = "2001:db8::5"
	if got := probePublicIP(ctx, "tcp4"); got != "" {
		t.Fatalf("走 IPv4 探测却回了 v6 地址,应不认,得到 %q", got)
	}
	body = "<html>blocked</html>"
	if got := publicIP(ctx); got != "" {
		t.Fatalf("不是 IP 的内容应返回空,得到 %q", got)
	}
}
