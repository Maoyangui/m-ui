package runner

import (
	"path/filepath"
	"strings"
	"testing"

	"github.com/Maoyangui/m-ui/certutil"
	"github.com/Maoyangui/m-ui/database/model"
)

// 面板、订阅、代理面板读不出证书、正用临时自签证书顶着:页面顶部的证书提示要列出来。同一个文件合并成一条
// (面板和代理面板常用同一张),证书库里有这张就用库里的名字、不再另报一条"读不出来";Trouble 仍是 missing,
// 只认这几种的页面也照样显示。Telegram 说一声,同一个服务同一个文件不重复说。
func TestCertAlertsListTempCerts(t *testing.T) {
	r := certTestRunner(t)
	sent := notices(t, r)
	dir := t.TempDir()
	panelCert, panelKey := filepath.Join(dir, "live", "a.example.com", "fullchain.pem"), filepath.Join(dir, "live", "a.example.com", "privkey.pem")
	subCert := filepath.Join(dir, "sub.pem")
	if err := r.saveCertRow(model.Cert{Name: "a.example.com", Source: "external", CertFile: panelCert, KeyFile: panelKey}); err != nil {
		t.Fatal(err)
	}
	saveSettings(r.db, map[string]string{"webCertFile": panelCert, "webKeyFile": panelKey})
	r.TLSWatch().Set("panel", certutil.NewReloaderOrTemp(panelCert, panelKey))
	r.TLSWatch().Set("reseller", certutil.NewReloaderOrTemp(panelCert, panelKey))
	r.TLSWatch().Set("sub", certutil.NewReloaderOrTemp(subCert, subCert+".key"))

	alerts := r.CertAlerts()
	if len(alerts) != 2 {
		t.Fatalf("应是两条:面板与代理面板共用的那张、订阅那张: %+v", alerts)
	}
	if a := alerts[0]; a.Name != "a.example.com" || a.Trouble != "missing" || a.Fallback != "panel,reseller" || !strings.Contains(a.Error, "no such file") {
		t.Fatalf("面板 / 代理面板那条不对: %+v", a)
	}
	if a := alerts[1]; a.Name != subCert || a.Trouble != "missing" || a.Fallback != "sub" {
		t.Fatalf("订阅那条不对: %+v", a)
	}
	msgs := strings.Join(sent(), "\n")
	for _, want := range []string{"面板证书读不出来", "代理面板证书读不出来", "订阅证书读不出来", "校验证书的订阅客户端会更新失败", panelCert} {
		if !strings.Contains(msgs, want) {
			t.Fatalf("Telegram 里没有 %q:\n%s", want, msgs)
		}
	}

	r.TLSWatch().Set("panel", certutil.NewReloaderOrTemp(panelCert, panelKey)) // 重启了一次面板
	if again := sent(); strings.Count(strings.Join(again, "\n"), "<b>面板证书读不出来</b>") != 1 {
		t.Fatalf("同一个文件不重复说: %v", again)
	}

	r.TLSWatch().Set("sub", nil) // 订阅改回 http / 停了
	r.TLSWatch().Set("reseller", nil)
	if alerts := r.CertAlerts(); len(alerts) != 1 || alerts[0].Fallback != "panel" {
		t.Fatalf("不再顶着的不再列: %+v", alerts)
	}
}
