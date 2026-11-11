package main

import (
	"crypto/tls"
	"crypto/x509"
	"net"
	"os"
	"path/filepath"
	"strconv"
	"strings"
	"testing"
	"time"

	"github.com/Maoyangui/m-ui/certutil"
	"github.com/Maoyangui/m-ui/database"
	"github.com/Maoyangui/m-ui/database/model"
	"github.com/Maoyangui/m-ui/runner"
	"github.com/Maoyangui/m-ui/web"
)

// certbotFiles 按 certbot 的样子放一张证书:live/<域名>/ 下是指向 archive/<域名>/ 的符号链接。返回 live/ 下的两个路径。
func certbotFiles(t *testing.T, le, domain string) (string, string) {
	t.Helper()
	arch, live := filepath.Join(le, "archive", domain), filepath.Join(le, "live", domain)
	ac, ak := filepath.Join(arch, "fullchain1.pem"), filepath.Join(arch, "privkey1.pem")
	if err := certutil.GenerateSelfSigned([]string{domain}, ac, ak, 90); err != nil {
		t.Fatal(err)
	}
	if err := os.MkdirAll(live, 0o755); err != nil {
		t.Fatal(err)
	}
	c, k := filepath.Join(live, "fullchain.pem"), filepath.Join(live, "privkey.pem")
	if err := os.Symlink(ac, c); err != nil {
		t.Fatal(err)
	}
	if err := os.Symlink(ak, k); err != nil {
		t.Fatal(err)
	}
	return c, k
}

func freeLoopbackPorts(t *testing.T, n int) []int {
	t.Helper()
	var out []int
	var lns []net.Listener
	for i := 0; i < n; i++ {
		ln, err := net.Listen("tcp", "127.0.0.1:0")
		if err != nil {
			t.Fatal(err)
		}
		lns = append(lns, ln)
		out = append(out, ln.Addr().(*net.TCPAddr).Port)
	}
	for _, ln := range lns {
		ln.Close()
	}
	return out
}

func servedCert(t *testing.T, port int) *x509.Certificate {
	t.Helper()
	conn, err := tls.DialWithDialer(&net.Dialer{Timeout: 3 * time.Second}, "tcp", net.JoinHostPort("127.0.0.1", strconv.Itoa(port)), &tls.Config{InsecureSkipVerify: true})
	if err != nil {
		t.Fatalf("端口 %d 连不上 HTTPS: %v", port, err)
	}
	defer conn.Close()
	return conn.ConnectionState().PeerCertificates[0]
}

// oldServer 旧机:面板、订阅、代理面板、线路都用数据目录外的「已有证书」(certbot)。返回库路径与端口(面板、订阅、代理面板)。
func oldServer(t *testing.T, base, le string) (string, []int) {
	t.Helper()
	cert, key := certbotFiles(t, le, "a.example.com")
	dbPath := filepath.Join(base, "old", "m-ui.db")
	db, err := database.Open(dbPath)
	if err != nil {
		t.Fatal(err)
	}
	ports := freeLoopbackPorts(t, 3)
	for k, v := range map[string]string{
		"webListen": "127.0.0.1", "subListen": "127.0.0.1", "resellerListen": "127.0.0.1",
		"webPort": strconv.Itoa(ports[0]), "subPort": strconv.Itoa(ports[1]), "resellerPort": strconv.Itoa(ports[2]),
		"webDomain":   "a.example.com",
		"webCertFile": cert, "webKeyFile": key, "subCertFile": cert, "subKeyFile": key,
		"resellerCertFile": cert, "resellerKeyFile": key, "certFile": cert, "keyFile": key,
	} {
		db.Create(&model.Setting{Key: k, Value: v})
	}
	db.Create(&model.Cert{Name: "a.example.com", Source: "external", CertFile: cert, KeyFile: key})
	database.Close(db)
	return dbPath, ports
}

// startRestored 在新机上按还原后的库起订阅服务与面板(含代理面板),返回 runner。
func startRestored(t *testing.T, dbPath string) *runner.Runner {
	t.Helper()
	r, err := runner.New(dbPath)
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { database.Close(r.DB()) })
	if err := r.RestartSub(); err != nil {
		t.Fatalf("还原后订阅服务起不来(runner.Run 会跟着返回,整个进程退出): %v", err)
	}
	panel := web.NewServer(r)
	if err := panel.Start(); err != nil {
		t.Fatalf("还原后面板起不来: %v", err)
	}
	t.Cleanup(func() { panel.Stop() })
	return r
}

// 按帮助换机:旧机用的是数据目录外的证书(certbot、宝塔、nginx 的文件,或旧面板导入的路径),新机上没有这些文件。
// 以前还原跳过它们,设置仍指着不存在的文件,订阅与面板起不来,systemd 每 2 秒重启一次,面板再也打不开。
// 现在还原把备份里的证书放进数据目录并改指过去:面板、订阅、代理面板都起来,用的就是原来那张证书;
// m-ui restore 的输出如实说清楚每个文件的去向。
func TestRestoredServerStartsWithExternalCerts(t *testing.T) {
	base := t.TempDir()
	le := filepath.Join(base, "letsencrypt")
	oldDB, ports := oldServer(t, base, le)
	zip := filepath.Join(base, "m-ui.zip")
	if err := runBackup(oldDB, zip); err != nil {
		t.Fatal(err)
	}
	os.RemoveAll(le) // 新机上没有 certbot 的文件

	newDB := filepath.Join(base, "new", "m-ui.db")
	var out strings.Builder
	if err := runRestore(newDB, zip, &out); err != nil {
		t.Fatalf("%v\n%s", err, out.String())
	}
	text := out.String()
	for _, want := range []string{"已还原:0 用户 / 0 线路 / 0 上游(之前没有数据库)", "证书:已还原 2 个,跳过 0 个(已还原的里有 2 个原在数据目录外,放进了数据目录)",
		filepath.Join(base, "new", "cert", "restored", "a.example.com-fullchain.pem") + ",设置已改指这里", "以后不会再跟着更新"} {
		if !strings.Contains(text, want) {
			t.Fatalf("m-ui restore 的输出里没有 %q:\n%s", want, text)
		}
	}
	if strings.Contains(text, "注意") {
		t.Fatalf("证书都还原了,不该再有要处理的: \n%s", text)
	}
	r := startRestored(t, newDB)
	for _, port := range ports {
		if c := servedCert(t, port); len(c.DNSNames) != 1 || c.DNSNames[0] != "a.example.com" {
			t.Fatalf("端口 %d 用的不是还原回来的那张证书: %v", port, c.DNSNames)
		}
	}
	if alerts := r.CertAlerts(); len(alerts) != 0 {
		t.Fatalf("证书都在,不该有提示: %+v", alerts)
	}
}

// 备份里本来就没有证书文件(备份时已经读不到,或者老备份没带代理面板的证书):还原后照样起得来,
// 面板、订阅、代理面板先用临时自签证书顶着,仍走 HTTPS;m-ui restore 说清楚缺了哪个,面板顶部也提示。
func TestRestoredServerStartsWithoutCertFiles(t *testing.T) {
	base := t.TempDir()
	le := filepath.Join(base, "letsencrypt")
	oldDB, ports := oldServer(t, base, le)
	os.RemoveAll(le) // 备份时这些文件已经不在了
	zip := filepath.Join(base, "m-ui.zip")
	if err := runBackup(oldDB, zip); err != nil {
		t.Fatal(err)
	}
	newDB := filepath.Join(base, "new", "m-ui.db")
	var out strings.Builder
	if err := runRestore(newDB, zip, &out); err != nil {
		t.Fatalf("%v\n%s", err, out.String())
	}
	text := out.String()
	cert := filepath.Join(le, "live", "a.example.com", "fullchain.pem")
	for _, want := range []string{"证书:备份里没有证书文件", "注意:" + cert + " (线路默认证书、面板 HTTPS、订阅 HTTPS、代理面板 HTTPS要用):备份时就读不到这个文件,本机也没有",
		"临时自签证书顶着", "用这张证书的线路起不来"} {
		if !strings.Contains(text, want) {
			t.Fatalf("m-ui restore 的输出里没有 %q:\n%s", want, text)
		}
	}
	r := startRestored(t, newDB)
	for _, port := range ports {
		if c := servedCert(t, port); c.Issuer.String() != c.Subject.String() {
			t.Fatalf("端口 %d 顶上的应是临时自签证书: %s", port, c.Subject)
		}
	}
	alerts := r.CertAlerts()
	if len(alerts) != 1 || alerts[0].Name != "a.example.com" || alerts[0].Trouble != "missing" || alerts[0].Fallback != "panel,sub,reseller" || alerts[0].Error == "" {
		t.Fatalf("面板顶部应提示这张证书读不出来、面板 / 订阅 / 代理面板在用临时证书: %+v", alerts)
	}
}
