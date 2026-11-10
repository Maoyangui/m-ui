package ext

import (
	"context"
	"crypto/ecdsa"
	"crypto/elliptic"
	"crypto/rand"
	"crypto/x509"
	"crypto/x509/pkix"
	"encoding/json"
	"encoding/pem"
	"math/big"
	"net"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"testing"
	"time"

	"github.com/Maoyangui/m-ui/core"
)

// 真连核验:起一个带各种入站的数据面(本机端口),把"机场 YAML 里会这么写"的 Clash 代理用 ClashToOutbound 转出来,
// 走外部节点测速同一条路径(core.TestOutboundIsolated)经它访问本机的 204 服务。转换丢一个参数(跳过证书校验、
// httpupgrade 的 Host、h2 的 Host、salamander 混淆……)这里就连不通。要开真实端口跑 QUIC,默认跳过:
//
//	M_UI_E2E=1 go test -tags with_utls ./ext/ -run E2E -v
func TestClashToOutboundE2E(t *testing.T) {
	if os.Getenv("M_UI_E2E") != "1" {
		t.Skip("需要真实端口与 QUIC:M_UI_E2E=1 时才跑")
	}
	target := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) { w.WriteHeader(http.StatusNoContent) }))
	defer target.Close()
	certPath, keyPath := e2eCert(t)
	const uuid = "0e2d8bd4-6a5b-4e3a-9a6c-1c1d1e1f2a3b"
	tlsIn := map[string]interface{}{"enabled": true, "server_name": "localhost", "certificate_path": certPath, "key_path": keyPath}
	h3 := func(m map[string]interface{}) map[string]interface{} {
		out := map[string]interface{}{"alpn": []string{"h3"}}
		for k, v := range m {
			out[k] = v
		}
		return out
	}
	type tc struct {
		name    string
		inbound map[string]interface{}
		proxy   map[string]interface{}
	}
	var cases []tc
	add := func(name string, udp bool, in, proxy map[string]interface{}) {
		port := e2ePort(t, udp)
		in["tag"], in["listen"], in["listen_port"] = name, "127.0.0.1", port
		proxy["name"], proxy["server"], proxy["port"] = name, "127.0.0.1", port
		cases = append(cases, tc{name, in, proxy})
	}
	add("vmess-tls-insecure", false,
		map[string]interface{}{"type": "vmess", "users": []map[string]interface{}{{"name": "a", "uuid": uuid, "alterId": 0}}, "tls": tlsIn},
		map[string]interface{}{"type": "vmess", "uuid": uuid, "alterId": 0, "cipher": "auto", "tls": true, "skip-cert-verify": true, "servername": "localhost"})
	add("vless-httpupgrade", false,
		map[string]interface{}{"type": "vless", "users": []map[string]interface{}{{"name": "a", "uuid": uuid}},
			"transport": map[string]interface{}{"type": "httpupgrade", "path": "/up", "host": "cdn.example.com"}},
		map[string]interface{}{"type": "vless", "uuid": uuid, "network": "ws",
			"ws-opts": map[string]interface{}{"path": "/up", "v2ray-http-upgrade": true, "headers": map[string]interface{}{"Host": "cdn.example.com"}}})
	add("trojan-ws", false,
		map[string]interface{}{"type": "trojan", "users": []map[string]interface{}{{"name": "a", "password": "pw"}}, "tls": tlsIn,
			"transport": map[string]interface{}{"type": "ws", "path": "/ws"}},
		map[string]interface{}{"type": "trojan", "password": "pw", "sni": "localhost", "skip-cert-verify": true, "network": "ws", "ws-opts": map[string]interface{}{"path": "/ws"}})
	add("vless-h2", false,
		map[string]interface{}{"type": "vless", "users": []map[string]interface{}{{"name": "a", "uuid": uuid}}, "tls": tlsIn,
			"transport": map[string]interface{}{"type": "http", "host": []string{"h.example.com"}, "path": "/h2"}},
		map[string]interface{}{"type": "vless", "uuid": uuid, "tls": true, "skip-cert-verify": true, "servername": "localhost",
			"network": "h2", "h2-opts": map[string]interface{}{"host": []interface{}{"h.example.com"}, "path": "/h2"}})
	add("hysteria2-salamander", true,
		map[string]interface{}{"type": "hysteria2", "users": []map[string]interface{}{{"name": "a", "password": "pw"}}, "tls": h3(tlsIn),
			"obfs": map[string]interface{}{"type": "salamander", "password": "op"}},
		map[string]interface{}{"type": "hysteria2", "password": "pw", "sni": "localhost", "skip-cert-verify": true, "obfs": "salamander", "obfs-password": "op"})
	add("tuic", true,
		map[string]interface{}{"type": "tuic", "users": []map[string]interface{}{{"name": "a", "uuid": uuid, "password": "pw"}}, "congestion_control": "bbr", "tls": h3(tlsIn)},
		map[string]interface{}{"type": "tuic", "uuid": uuid, "password": "pw", "sni": "localhost", "skip-cert-verify": true, "congestion-controller": "bbr"})
	add("anytls", false,
		map[string]interface{}{"type": "anytls", "users": []map[string]interface{}{{"name": "a", "password": "pw"}}, "tls": tlsIn},
		map[string]interface{}{"type": "anytls", "password": "pw", "sni": "localhost", "skip-cert-verify": true})
	add("shadowsocks", false,
		map[string]interface{}{"type": "shadowsocks", "method": "aes-128-gcm", "password": "secret"},
		map[string]interface{}{"type": "ss", "cipher": "aes-128-gcm", "password": "secret"})
	add("socks5", false,
		map[string]interface{}{"type": "socks", "users": []map[string]interface{}{{"username": "u", "password": "p"}}},
		map[string]interface{}{"type": "socks5", "username": "u", "password": "p"})

	var inbounds []map[string]interface{}
	for _, c := range cases {
		inbounds = append(inbounds, c.inbound)
	}
	cfg, _ := json.Marshal(map[string]interface{}{
		"log":       map[string]interface{}{"level": "error"},
		"inbounds":  inbounds,
		"outbounds": []map[string]interface{}{{"type": "direct", "tag": "direct"}},
	})
	c := core.NewCore()
	if err := c.Start(cfg); err != nil {
		t.Fatalf("数据面起不来: %v", err)
	}
	defer c.Stop()
	time.Sleep(300 * time.Millisecond)
	for _, cs := range cases {
		typ, opts, err := ClashToOutbound(cs.proxy)
		if err != nil {
			t.Errorf("%s: 转换失败: %v", cs.name, err)
			continue
		}
		ctx, cancel := context.WithTimeout(context.Background(), 20*time.Second)
		res := core.TestOutboundIsolated(ctx, outboundJSON(t, typ, opts), target.URL, 2)
		cancel()
		if !res.OK {
			t.Errorf("%s: 经转换出的出站连不通: %+v\n出站=%s", cs.name, res, outboundJSON(t, typ, opts))
		} else {
			t.Logf("%s: 通,%d ms", cs.name, res.DelayMs)
		}
	}
	// 反过来:凭据错了要报不通(而不是测不了)
	bad := cases[len(cases)-1].proxy
	bad["password"] = "wrong"
	typ, opts, _ := ClashToOutbound(bad)
	ctx, cancel := context.WithTimeout(context.Background(), 20*time.Second)
	defer cancel()
	if res := core.TestOutboundIsolated(ctx, outboundJSON(t, typ, opts), target.URL, 2); res.OK || res.Config {
		t.Fatalf("凭据错误应是不通: %+v", res)
	}
}

func e2ePort(t *testing.T, udp bool) int {
	t.Helper()
	if udp {
		pc, err := net.ListenPacket("udp", "127.0.0.1:0")
		if err != nil {
			t.Fatal(err)
		}
		defer pc.Close()
		return pc.LocalAddr().(*net.UDPAddr).Port
	}
	ln, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatal(err)
	}
	defer ln.Close()
	return ln.Addr().(*net.TCPAddr).Port
}

func e2eCert(t *testing.T) (certPath, keyPath string) {
	t.Helper()
	key, err := ecdsa.GenerateKey(elliptic.P256(), rand.Reader)
	if err != nil {
		t.Fatal(err)
	}
	tpl := &x509.Certificate{
		SerialNumber: big.NewInt(1),
		Subject:      pkix.Name{CommonName: "localhost"},
		DNSNames:     []string{"localhost"},
		NotBefore:    time.Now().Add(-time.Hour),
		NotAfter:     time.Now().Add(24 * time.Hour),
		KeyUsage:     x509.KeyUsageDigitalSignature,
		ExtKeyUsage:  []x509.ExtKeyUsage{x509.ExtKeyUsageServerAuth},
	}
	der, err := x509.CreateCertificate(rand.Reader, tpl, tpl, &key.PublicKey, key)
	if err != nil {
		t.Fatal(err)
	}
	keyDER, err := x509.MarshalECPrivateKey(key)
	if err != nil {
		t.Fatal(err)
	}
	dir := t.TempDir()
	certPath, keyPath = filepath.Join(dir, "cert.pem"), filepath.Join(dir, "key.pem")
	if err := os.WriteFile(certPath, pem.EncodeToMemory(&pem.Block{Type: "CERTIFICATE", Bytes: der}), 0o600); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(keyPath, pem.EncodeToMemory(&pem.Block{Type: "EC PRIVATE KEY", Bytes: keyDER}), 0o600); err != nil {
		t.Fatal(err)
	}
	return certPath, keyPath
}
