package web

import (
	"context"
	"crypto/tls"
	"crypto/x509"
	"encoding/json"
	"errors"
	"net"
	"net/http"
	"sort"
	"strconv"
	"strings"
	"sync"
	"time"
)

// Reality 的握手目标选得不好就连不上、或者很容易被看出来:它要支持 TLS 1.3、H2、X25519,离这台服务器要近。
// 这个接口从主机实际连一次,给出行不行、延迟多少。不填目标时把一串常见的大站测一遍,按延迟排好给你挑。

var realityCandidates = []string{
	"www.microsoft.com", "www.apple.com", "www.icloud.com", "www.amazon.com", "www.cloudflare.com", "www.nvidia.com",
	"www.samsung.com", "www.tesla.com", "www.oracle.com", "www.intel.com", "www.amd.com", "www.ubuntu.com",
	"addons.mozilla.org", "www.speedtest.net", "www.lovelive-anime.jp", "dl.google.com",
}

// realityRoots 校验证书用的根,nil = 系统的。测试里换成测试 CA。
var realityRoots *x509.CertPool

type realityResult struct {
	Server string `json:"server"`
	Port   int    `json:"port"`
	OK     bool   `json:"ok"`
	Ms     int    `json:"ms"`
	Error  string `json:"error,omitempty"` // 错误代码,文案在页面上:connect / tls13 / h2 / cert
}

// checkRealityTarget 真连一次:TCP + TLS 1.3 握手,只给 X25519,要协商出 h2,证书要对得上域名。
func checkRealityTarget(ctx context.Context, server string, port int) realityResult {
	res := realityResult{Server: server, Port: port}
	addr := net.JoinHostPort(server, strconv.Itoa(port))
	start := time.Now()
	var d net.Dialer
	raw, err := d.DialContext(ctx, "tcp", addr)
	if err != nil {
		res.Error = "connect"
		return res
	}
	defer raw.Close()
	conn := tls.Client(raw, &tls.Config{
		ServerName: server, MinVersion: tls.VersionTLS13, NextProtos: []string{"h2", "http/1.1"},
		CurvePreferences: []tls.CurveID{tls.X25519}, RootCAs: realityRoots,
	})
	if err := conn.HandshakeContext(ctx); err != nil {
		var verr *tls.CertificateVerificationError
		switch {
		case ctx.Err() != nil: // 超时不是对方不支持
			res.Error = "connect"
		case errors.As(err, &verr):
			res.Error = "cert"
		default:
			res.Error = "tls13"
		}
		return res
	}
	res.Ms = int(time.Since(start).Milliseconds())
	if conn.ConnectionState().NegotiatedProtocol != "h2" {
		res.Error = "h2"
		return res
	}
	res.OK = true
	return res
}

// handleRealityCheck POST /reality-check {server, port}:测一个;不给 server:把常见大站测一遍。
func (s *Server) handleRealityCheck(w http.ResponseWriter, r *http.Request) {
	if r.Method != http.MethodPost {
		writeJSON(w, http.StatusMethodNotAllowed, map[string]string{"error": "方法不允许"})
		return
	}
	var req struct {
		Server string `json:"server"`
		Port   int    `json:"port"`
	}
	_ = json.NewDecoder(http.MaxBytesReader(w, r.Body, 4<<10)).Decode(&req)
	req.Server = strings.TrimSpace(req.Server)
	if req.Port <= 0 || req.Port > 65535 {
		req.Port = 443
	}
	ctx, cancel := context.WithTimeout(r.Context(), 6*time.Second)
	defer cancel()
	if req.Server != "" {
		if strings.ContainsAny(req.Server, " /\\:") {
			badRequest(w, errors.New("握手目标要填域名,不带协议和端口"))
			return
		}
		writeJSON(w, http.StatusOK, checkRealityTarget(ctx, req.Server, req.Port))
		return
	}
	results := make([]realityResult, len(realityCandidates))
	var wg sync.WaitGroup
	for i, host := range realityCandidates {
		wg.Add(1)
		go func(i int, host string) {
			defer wg.Done()
			results[i] = checkRealityTarget(ctx, host, 443)
		}(i, host)
	}
	wg.Wait()
	sort.SliceStable(results, func(a, b int) bool {
		if results[a].OK != results[b].OK {
			return results[a].OK
		}
		return results[a].Ms < results[b].Ms
	})
	writeJSON(w, http.StatusOK, map[string]interface{}{"results": results})
}
