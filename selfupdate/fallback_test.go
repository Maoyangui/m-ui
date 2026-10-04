package selfupdate

import (
	"context"
	"encoding/binary"
	"fmt"
	"io"
	"net"
	"net/http"
	"strconv"
	"strings"
	"sync/atomic"
	"testing"
	"time"
)

// fakeSocks 极简 socks5 服务(无认证、只支持 CONNECT):不管目标写的是什么域名,都连到 target。
// 用来冒充本机 WARP:直连解析不了的域名,经它能连上。
func fakeSocks(t *testing.T, target string) (port int, used *atomic.Int32) {
	t.Helper()
	ln, err := net.Listen("tcp4", "127.0.0.1:0")
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { ln.Close() })
	used = &atomic.Int32{}
	go func() {
		for {
			c, err := ln.Accept()
			if err != nil {
				return
			}
			go func(c net.Conn) {
				defer c.Close()
				buf := make([]byte, 262)
				if _, err := io.ReadFull(c, buf[:2]); err != nil { // VER NMETHODS
					return
				}
				if _, err := io.ReadFull(c, buf[:buf[1]]); err != nil {
					return
				}
				c.Write([]byte{5, 0})
				if _, err := io.ReadFull(c, buf[:4]); err != nil { // VER CMD RSV ATYP
					return
				}
				switch buf[3] {
				case 1:
					io.ReadFull(c, buf[:4])
				case 4:
					io.ReadFull(c, buf[:16])
				case 3:
					io.ReadFull(c, buf[:1])
					io.ReadFull(c, buf[:buf[0]])
				}
				io.ReadFull(c, buf[:2]) // 端口
				up, err := net.Dial("tcp4", target)
				if err != nil {
					c.Write([]byte{5, 5, 0, 1, 0, 0, 0, 0, 0, 0})
					return
				}
				defer up.Close()
				used.Add(1)
				rep := []byte{5, 0, 0, 1, 127, 0, 0, 1, 0, 0}
				binary.BigEndian.PutUint16(rep[8:], 1)
				c.Write(rep)
				go io.Copy(up, c)
				io.Copy(c, up)
			}(c)
		}
	}()
	return ln.Addr().(*net.TCPAddr).Port, used
}

func okServer(t *testing.T) string {
	t.Helper()
	ln, err := net.Listen("tcp4", "127.0.0.1:0")
	if err != nil {
		t.Fatal(err)
	}
	srv := &http.Server{Handler: http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) { fmt.Fprint(w, "ok") })}
	go srv.Serve(ln)
	t.Cleanup(func() { srv.Close() })
	return ln.Addr().String()
}

func get(ctx context.Context, url string) (string, error) {
	resp, err := doGitHub(ctx, &http.Client{Timeout: 5 * time.Second}, func() (*http.Request, error) {
		return http.NewRequestWithContext(ctx, "GET", url, nil)
	})
	if err != nil {
		return "", err
	}
	defer resp.Body.Close()
	b, _ := io.ReadAll(resp.Body)
	return string(b), nil
}

func TestDoGitHubFallsBackToWarp(t *testing.T) {
	ctx, cancel := context.WithTimeout(context.Background(), 20*time.Second)
	defer cancel()
	target := okServer(t)
	// 直连到本机一个关着的端口:必定连不上(相当于纯 IPv6 机器连 github.com),而且不出本机
	closed := func() int {
		ln, _ := net.Listen("tcp4", "127.0.0.1:0")
		defer ln.Close()
		return ln.Addr().(*net.TCPAddr).Port
	}
	url := "http://127.0.0.1:" + strconv.Itoa(closed()) + "/"

	// 没有 WARP:报直连的错
	SetFallbackPort(func() int { return 0 })
	if _, err := get(ctx, url); err == nil {
		t.Fatal("直连连不上、也没有 WARP,应当失败")
	}

	// WARP 在听:直连失败后经它成功
	port, used := fakeSocks(t, target)
	SetFallbackPort(func() int { return port })
	t.Cleanup(func() { SetFallbackPort(func() int { return 0 }) })
	body, err := get(ctx, url)
	if err != nil || body != "ok" || used.Load() != 1 {
		t.Fatalf("应经 WARP 拿到 ok,得到 %q %v(代理用了 %d 次)", body, err, used.Load())
	}

	// 直连能通:不经 WARP
	body, err = get(ctx, "http://"+target+"/")
	if err != nil || body != "ok" || used.Load() != 1 {
		t.Fatalf("直连能通时不该用 WARP,得到 %q %v(代理用了 %d 次)", body, err, used.Load())
	}

	// 端口配了但没人听(WARP 停了):报直连的错,不卡住
	dead := closed()
	SetFallbackPort(func() int { return dead })
	if _, err := get(ctx, url); err == nil || strings.Contains(err.Error(), strconv.Itoa(dead)) {
		t.Fatalf("WARP 没在听时应只报直连的错,得到 %v", err)
	}
}
