// Package acme 用 Let's Encrypt 签发证书:http-01(临时监听 :80)或 dns-01(Cloudflare API),
// 并提供证书信息读取与签发前的 DNS/端口预检。
package acme

import (
	"context"
	"crypto/ecdsa"
	"crypto/elliptic"
	"crypto/rand"
	"crypto/x509"
	"crypto/x509/pkix"
	"encoding/pem"
	"errors"
	"fmt"
	"io"
	"net"
	"net/http"
	"os"
	"path/filepath"
	"strings"
	"sync"
	"time"

	"golang.org/x/crypto/acme"
)

const (
	LetsEncrypt        = "https://acme-v02.api.letsencrypt.org/directory"
	LetsEncryptStaging = "https://acme-staging-v02.api.letsencrypt.org/directory"
)

type Config struct {
	Email      string
	Domain     string
	Method     string // http | cloudflare
	CFToken    string
	Staging    bool
	CertPath   string
	KeyPath    string
	AccountKey string // PEM,空则新建
	Logf       func(format string, a ...interface{})
	// 可替换的目录地址(测试用)
	DirectoryURL string
}

type Result struct {
	AccountKey string
	NotAfter   time.Time
	Issuer     string
}

func (c Config) logf(format string, a ...interface{}) {
	if c.Logf != nil {
		c.Logf(format, a...)
	}
}

// Issue 完成一次签发,把 fullchain 与私钥写到 CertPath/KeyPath。
func Issue(ctx context.Context, cfg Config) (Result, error) {
	var res Result
	cfg.Domain = strings.TrimSpace(strings.ToLower(cfg.Domain))
	if cfg.Domain == "" || strings.ContainsAny(cfg.Domain, " /:") {
		return res, errors.New("域名无效")
	}
	if net.ParseIP(cfg.Domain) != nil {
		return res, errors.New("签发失败:Let's Encrypt 不给 IP 签发证书,请用域名或自签")
	}
	if cfg.Method == "cloudflare" && strings.TrimSpace(cfg.CFToken) == "" {
		return res, errors.New("dns-01 需要 Cloudflare API Token")
	}
	if cfg.CertPath == "" || cfg.KeyPath == "" {
		return res, errors.New("证书输出路径为空")
	}

	key, keyPEM, err := loadOrCreateAccountKey(cfg.AccountKey)
	if err != nil {
		return res, err
	}
	res.AccountKey = keyPEM

	dir := cfg.DirectoryURL
	if dir == "" {
		dir = LetsEncrypt
		if cfg.Staging {
			dir = LetsEncryptStaging
		}
	}
	client := &acme.Client{Key: key, DirectoryURL: dir, UserAgent: "m-ui"}

	cfg.logf("注册/查询 ACME 账户(%s)", dir)
	acct := &acme.Account{}
	if cfg.Email != "" {
		acct.Contact = []string{"mailto:" + cfg.Email}
	}
	if _, err := client.Register(ctx, acct, acme.AcceptTOS); err != nil && !errors.Is(err, acme.ErrAccountAlreadyExists) {
		return res, fmt.Errorf("注册账户: %w", err)
	}

	cfg.logf("创建订单: %s", cfg.Domain)
	order, err := client.AuthorizeOrder(ctx, acme.DomainIDs(cfg.Domain))
	if err != nil {
		return res, fmt.Errorf("创建订单: %w", err)
	}

	for _, authzURL := range order.AuthzURLs {
		authz, err := client.GetAuthorization(ctx, authzURL)
		if err != nil {
			return res, fmt.Errorf("读取授权: %w", err)
		}
		if authz.Status == acme.StatusValid {
			cfg.logf("授权已有效,跳过验证")
			continue
		}
		var chal *acme.Challenge
		want := "http-01"
		if cfg.Method == "cloudflare" {
			want = "dns-01"
		}
		for _, c := range authz.Challenges {
			if c.Type == want {
				chal = c
				break
			}
		}
		if chal == nil {
			return res, fmt.Errorf("CA 未提供 %s 验证方式", want)
		}

		var cleanup func()
		if want == "http-01" {
			cleanup, err = serveHTTP01(client, chal, cfg.logf)
		} else {
			cleanup, err = setDNS01(ctx, client, chal, cfg, cfg.logf)
		}
		if err != nil {
			return res, err
		}
		cfg.logf("通知 CA 开始验证(%s)", want)
		if _, err := client.Accept(ctx, chal); err != nil {
			cleanup()
			return res, fmt.Errorf("接受验证: %w", err)
		}
		_, err = client.WaitAuthorization(ctx, authz.URI)
		cleanup()
		if err != nil {
			return res, fmt.Errorf("验证失败: %w", err)
		}
		cfg.logf("验证通过")
	}

	cfg.logf("生成私钥与 CSR")
	certKey, err := ecdsa.GenerateKey(elliptic.P256(), rand.Reader)
	if err != nil {
		return res, err
	}
	csr, err := x509.CreateCertificateRequest(rand.Reader, &x509.CertificateRequest{
		Subject: pkix.Name{CommonName: cfg.Domain}, DNSNames: []string{cfg.Domain},
	}, certKey)
	if err != nil {
		return res, err
	}
	cfg.logf("提交订单,等待签发")
	der, _, err := client.CreateOrderCert(ctx, order.FinalizeURL, csr, true)
	if err != nil {
		return res, fmt.Errorf("签发: %w", err)
	}
	if len(der) == 0 {
		return res, errors.New("CA 返回空证书")
	}
	leaf, err := x509.ParseCertificate(der[0])
	if err != nil {
		return res, err
	}
	res.NotAfter, res.Issuer = leaf.NotAfter, leaf.Issuer.CommonName

	if err := writeCert(cfg.CertPath, cfg.KeyPath, der, certKey); err != nil {
		return res, err
	}
	cfg.logf("证书已写入 %s(到期 %s)", cfg.CertPath, leaf.NotAfter.Format("2006-01-02"))
	return res, nil
}

func loadOrCreateAccountKey(pemStr string) (*ecdsa.PrivateKey, string, error) {
	if strings.TrimSpace(pemStr) != "" {
		block, _ := pem.Decode([]byte(pemStr))
		if block != nil {
			if k, err := x509.ParseECPrivateKey(block.Bytes); err == nil {
				return k, pemStr, nil
			}
		}
	}
	k, err := ecdsa.GenerateKey(elliptic.P256(), rand.Reader)
	if err != nil {
		return nil, "", err
	}
	b, err := x509.MarshalECPrivateKey(k)
	if err != nil {
		return nil, "", err
	}
	return k, string(pem.EncodeToMemory(&pem.Block{Type: "EC PRIVATE KEY", Bytes: b})), nil
}

// serveHTTP01 在 :80 临时提供验证文件。
func serveHTTP01(client *acme.Client, chal *acme.Challenge, logf func(string, ...interface{})) (func(), error) {
	resp, err := client.HTTP01ChallengeResponse(chal.Token)
	if err != nil {
		return nil, err
	}
	path := client.HTTP01ChallengePath(chal.Token)
	ln, err := net.Listen("tcp", ":80")
	if err != nil {
		return nil, fmt.Errorf("监听 80 端口失败(http-01 需要 80 端口空闲且公网可达): %w", err)
	}
	mux := http.NewServeMux()
	mux.HandleFunc(path, func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "text/plain")
		io.WriteString(w, resp)
	})
	srv := &http.Server{Handler: mux, ReadHeaderTimeout: 5 * time.Second}
	go srv.Serve(ln)
	logf("已在 :80 提供验证文件 %s", path)
	return func() { srv.Close() }, nil
}

// setDNS01 通过 Cloudflare 写入 _acme-challenge TXT,并等待公共 DNS 可见。
func setDNS01(ctx context.Context, client *acme.Client, chal *acme.Challenge, cfg Config, logf func(string, ...interface{})) (func(), error) {
	val, err := client.DNS01ChallengeRecord(chal.Token)
	if err != nil {
		return nil, err
	}
	cf := &Cloudflare{Token: cfg.CFToken}
	zone, zoneName, err := cf.ZoneFor(ctx, cfg.Domain)
	if err != nil {
		return nil, err
	}
	name := "_acme-challenge." + cfg.Domain
	logf("Cloudflare 区域 %s,写入 TXT %s", zoneName, name)
	id, err := cf.CreateTXT(ctx, zone, name, val)
	if err != nil {
		return nil, err
	}
	cleanup := func() {
		c, cancel := context.WithTimeout(context.Background(), 15*time.Second)
		defer cancel()
		if err := cf.DeleteRecord(c, zone, id); err != nil {
			logf("清理 TXT 失败(可手动删除): %v", err)
		}
	}
	logf("等待公共 DNS 生效…")
	deadline := time.Now().Add(2 * time.Minute)
	for time.Now().Before(deadline) {
		if txtVisible(ctx, name, val) {
			logf("TXT 已可见")
			return cleanup, nil
		}
		select {
		case <-ctx.Done():
			cleanup()
			return nil, ctx.Err()
		case <-time.After(5 * time.Second):
		}
	}
	logf("2 分钟内未在 1.1.1.1 看到 TXT,仍尝试让 CA 验证")
	return cleanup, nil
}

func txtVisible(ctx context.Context, name, want string) bool {
	r := resolverAt(publicDNS[0].v4+":53", "["+publicDNS[0].v6+"]:53")
	vals, err := r.LookupTXT(ctx, name)
	if err != nil {
		return false
	}
	for _, v := range vals {
		if v == want {
			return true
		}
	}
	return false
}

// publicDNS 查解析用的公共 DNS:IPv4 地址与同一家的 IPv6 地址(纯 IPv6 机器连不上 IPv4 的 DNS)。
var publicDNS = []struct{ v4, v6 string }{
	{"1.1.1.1", "2606:4700:4700::1111"},
	{"8.8.8.8", "2001:4860:4860::8888"},
	{"223.5.5.5", "2400:3200::1"},
}

// resolverAt 用这几个 DNS 服务器(host:port)查,前一个连不上(纯 IPv6 机器连 IPv4 地址)就换下一个。
func resolverAt(addrs ...string) *net.Resolver {
	return &net.Resolver{PreferGo: true, Dial: func(ctx context.Context, _, _ string) (net.Conn, error) {
		d := net.Dialer{Timeout: 4 * time.Second}
		var err error
		for _, a := range addrs {
			var c net.Conn
			if c, err = d.DialContext(ctx, "udp", a); err == nil {
				return c, nil
			}
		}
		return nil, err
	}}
}

func writeCert(certPath, keyPath string, der [][]byte, key *ecdsa.PrivateKey) error {
	if err := os.MkdirAll(filepath.Dir(certPath), 0o755); err != nil {
		return err
	}
	if err := os.MkdirAll(filepath.Dir(keyPath), 0o755); err != nil {
		return err
	}
	var certPEM []byte
	for _, d := range der {
		certPEM = append(certPEM, pem.EncodeToMemory(&pem.Block{Type: "CERTIFICATE", Bytes: d})...)
	}
	kb, err := x509.MarshalECPrivateKey(key)
	if err != nil {
		return err
	}
	keyPEM := pem.EncodeToMemory(&pem.Block{Type: "EC PRIVATE KEY", Bytes: kb})
	// 先写临时文件再改名,避免服务读到半截文件
	if err := atomicWrite(certPath, certPEM, 0o644); err != nil {
		return err
	}
	return atomicWrite(keyPath, keyPEM, 0o600)
}

func atomicWrite(path string, data []byte, mode os.FileMode) error {
	tmp := path + ".tmp"
	if err := os.WriteFile(tmp, data, mode); err != nil {
		return err
	}
	return os.Rename(tmp, path)
}

// ---- 证书信息 ----

type CertInfo struct {
	Exists     bool      `json:"exists"`
	Path       string    `json:"path"`
	Subject    string    `json:"subject"`
	DNSNames   []string  `json:"dnsNames"`
	IPs        []string  `json:"ips"`
	Issuer     string    `json:"issuer"`
	NotBefore  time.Time `json:"notBefore"`
	NotAfter   time.Time `json:"notAfter"`
	DaysLeft   int       `json:"daysLeft"`
	SelfSigned bool      `json:"selfSigned"`
	Error      string    `json:"error,omitempty"`
}

// Info 读取证书文件的基本信息。
func Info(certPath string) CertInfo {
	ci := CertInfo{Path: certPath}
	if certPath == "" {
		return ci
	}
	b, err := os.ReadFile(certPath)
	if err != nil {
		ci.Error = err.Error()
		return ci
	}
	block, _ := pem.Decode(b)
	if block == nil {
		ci.Error = "不是 PEM 证书"
		return ci
	}
	c, err := x509.ParseCertificate(block.Bytes)
	if err != nil {
		ci.Error = err.Error()
		return ci
	}
	ci.Exists = true
	ci.Subject, ci.DNSNames, ci.Issuer = c.Subject.CommonName, c.DNSNames, c.Issuer.CommonName
	for _, ip := range c.IPAddresses {
		ci.IPs = append(ci.IPs, ip.String())
	}
	ci.NotBefore, ci.NotAfter = c.NotBefore, c.NotAfter
	ci.DaysLeft = int(time.Until(c.NotAfter).Hours() / 24)
	ci.SelfSigned = c.Issuer.String() == c.Subject.String()
	return ci
}

// ---- 预检 ----

type PrecheckResult struct {
	Domain     string            `json:"domain"`
	PublicIP   string            `json:"publicIp"`
	Family     string            `json:"family"`   // v4 = 比的是 A 记录;v6 = 本机只有 IPv6,比的是 AAAA 记录
	Resolved   map[string]string `json:"resolved"` // resolver → A(或 AAAA)记录
	DNSOk      bool              `json:"dnsOk"`
	Port80     string            `json:"port80"`     // free | busy | error text
	Cloudflare bool              `json:"cloudflare"` // A 记录像 Cloudflare 代理 IP 段(不完全判断)
}

// Precheck 检查域名解析是否指向本机公网 IP、80 端口是否空闲。
// 本机有 IPv4 时比 A 记录;只有 IPv6(纯 v6 机器)时比 AAAA 记录。
func Precheck(ctx context.Context, domain string) PrecheckResult {
	res := PrecheckResult{Domain: domain, Resolved: map[string]string{}, Family: "v4"}
	v4, v6 := PublicIPs(ctx)
	res.PublicIP = v4
	if v4 == "" && v6 != "" {
		res.PublicIP, res.Family = v6, "v6"
	}
	all := true
	for _, dns := range publicDNS {
		r := dns.v4 // 纯 IPv6 机器连不上 IPv4 的 DNS,用同一家的 IPv6 地址查
		if res.Family == "v6" {
			r = dns.v6
		}
		ips, err := resolverAt(net.JoinHostPort(r, "53")).LookupIPAddr(ctx, domain)
		v := ""
		for _, ip := range ips {
			if (ip.IP.To4() != nil) == (res.Family == "v4") {
				v = ip.IP.String()
				break
			}
		}
		if err != nil || v == "" {
			v = "<无记录>"
			all = false
		} else if res.PublicIP != "" && v != res.PublicIP {
			all = false
		}
		res.Resolved[strings.TrimSuffix(r, ":53")] = v
	}
	res.DNSOk = all && res.PublicIP != ""
	if ln, err := net.Listen("tcp", ":80"); err == nil {
		ln.Close()
		res.Port80 = "free"
	} else {
		res.Port80 = "busy: " + err.Error()
	}
	return res
}

// PublicIP 探测本机公网 IP,优先 IPv4,纯 IPv6 机器给 IPv6;失败返回空。
//
// 双栈机器上默认连接走 IPv6,测出来的是 v6 地址:订阅里留空的节点地址会变成大陆多数网络连不上的 v6,
// 大陆连通检测(只测 IPv4)也测不了。所以两个地址族分开强制测,见 PublicIPs。
func PublicIP(ctx context.Context) string { return publicIP(ctx) }

// 探测地址按地址族分:api.ipify.org 只有 IPv4,IPv6 用 api64。
var publicIPURLs = map[string][]string{
	"tcp4": {"https://www.cloudflare.com/cdn-cgi/trace", "https://api.ipify.org"},
	"tcp6": {"https://www.cloudflare.com/cdn-cgi/trace", "https://api64.ipify.org"},
}

func publicIP(ctx context.Context) string {
	v4, v6 := PublicIPs(ctx)
	if v4 != "" {
		return v4
	}
	return v6
}

// PublicIPs 分别强制走 IPv4、IPv6 探测本机公网地址(两路同时测);哪一路不通哪个就是空。
func PublicIPs(ctx context.Context) (v4, v6 string) {
	var wg sync.WaitGroup
	wg.Add(2)
	go func() { defer wg.Done(); v4 = probePublicIP(ctx, "tcp4") }()
	go func() { defer wg.Done(); v6 = probePublicIP(ctx, "tcp6") }()
	wg.Wait()
	return v4, v6
}

// probePublicIP 按指定地址族(tcp4 / tcp6)连探测地址,只认同一地址族的结果。
func probePublicIP(ctx context.Context, network string) string {
	d := &net.Dialer{Timeout: 5 * time.Second}
	tr := http.DefaultTransport.(*http.Transport).Clone()
	tr.DialContext = func(ctx context.Context, _, addr string) (net.Conn, error) {
		return d.DialContext(ctx, network, addr)
	}
	defer tr.CloseIdleConnections()
	c := &http.Client{Timeout: 6 * time.Second, Transport: tr}
	for _, u := range publicIPURLs[network] {
		req, _ := http.NewRequestWithContext(ctx, "GET", u, nil)
		resp, err := c.Do(req)
		if err != nil {
			continue
		}
		b, _ := io.ReadAll(io.LimitReader(resp.Body, 4096))
		resp.Body.Close()
		s := strings.TrimSpace(string(b))
		if strings.Contains(s, "ip=") {
			s = ""
			for _, line := range strings.Split(strings.TrimSpace(string(b)), "\n") {
				if strings.HasPrefix(line, "ip=") {
					s = strings.TrimSpace(strings.TrimPrefix(line, "ip="))
					break
				}
			}
		}
		ip := net.ParseIP(s)
		if ip == nil || (ip.To4() != nil) != (network == "tcp4") {
			continue
		}
		return ip.String()
	}
	return ""
}
