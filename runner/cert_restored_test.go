package runner

import (
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/Maoyangui/m-ui/database/model"
)

// 换机还原时放进 cert/restored/ 的证书副本没有谁续:新机上没有 certbot,副本到期后面板、订阅和用它的线路一起失效。
// 以前它和服务器上已有的证书一样标"外部工具续期",剩 14 天的提醒还说"由 certbot 续期,请确认已经续上"。
// 现在证书页标"还原的副本",剩 30 天起每天提醒,写明是备份副本、怎么换掉;顶部提示与报给主机的清单也带上这一项。
// 「续期」按钮与打开自动续期的报错同样说清楚。数据目录外的外部证书照旧:外部工具续期,剩 14 天起提醒。
func TestRestoredCopyNotRenewedByAnyone(t *testing.T) {
	r := certTestRunner(t)
	sent := notices(t, r)
	restored := filepath.Join(r.DataDir(), "cert", "restored")
	copyCert, copyKey := le90(t, restored, "a.example.com", 31*day+time.Hour)
	extCert, extKey := le90(t, t.TempDir(), "ext.example.com", 25*day+time.Hour)
	for _, row := range []model.Cert{
		{Name: "a.example.com", Source: "external", CertFile: copyCert, KeyFile: copyKey},
		{Name: "ext.example.com", Source: "external", CertFile: extCert, KeyFile: extKey},
	} {
		if err := r.saveCertRow(row); err != nil {
			t.Fatal(err)
		}
	}
	if err := saveSettings(r.db, map[string]string{"webCertFile": copyCert, "webKeyFile": copyKey, "certFile": extCert, "keyFile": extKey}); err != nil {
		t.Fatal(err)
	}
	reasons := map[string]string{}
	for _, c := range r.Certs() {
		reasons[c.Name] = c.Reason
	}
	if reasons["a.example.com"] != "restored" || reasons["ext.example.com"] != "external" {
		t.Fatalf("证书页要把还原的副本和外部工具续的分开: %v", reasons)
	}
	r.renewDue()
	if msgs := sent(); len(msgs) != 0 {
		t.Fatalf("副本剩 31 天、外部证书剩 25 天,都还不用提醒: %v", msgs)
	}

	le90(t, restored, "a.example.com", 25*day+time.Hour) // 过了几天:副本剩 25 天
	r.renewDue()
	msgs := sent()
	if len(msgs) != 1 || !strings.Contains(msgs[0], "a.example.com") || !strings.Contains(msgs[0], "从备份放进数据目录的副本") ||
		!strings.Contains(msgs[0], "签一张新的") || strings.Contains(msgs[0], "请确认已经续上") {
		t.Fatalf("副本剩 30 天内要提醒,说清是备份副本、怎么换: %v", msgs)
	}
	r.renewDue()
	if msgs := sent(); len(msgs) != 1 {
		t.Fatalf("同一天不重复提醒: %v", msgs)
	}

	briefs := map[string]model.CertBrief{}
	for _, b := range r.CertBriefs() {
		briefs[b.Name] = b
	}
	if !briefs["a.example.com"].Restored || briefs["ext.example.com"].Restored {
		t.Fatalf("报给主机的清单要标出还原的副本: %+v", briefs)
	}
	alerts := r.CertAlerts()
	if len(alerts) != 1 || alerts[0].Name != "a.example.com" || alerts[0].Trouble != model.CertExpiring || !alerts[0].Restored {
		t.Fatalf("顶部提示:副本剩 25 天就提示并标明是还原的副本,外部证书剩 25 天还不用: %+v", alerts)
	}

	for _, err := range []error{r.RenewCert("a.example.com"), r.SetCertAutoRenew("a.example.com", true)} {
		if err == nil || !strings.Contains(err.Error(), "换机还原时从备份放进来的副本") {
			t.Fatalf("续期、打开自动续期要说清是还原的副本: %v", err)
		}
	}
	if err := r.RenewCert("ext.example.com"); err == nil || !strings.Contains(err.Error(), "certbot") {
		t.Fatalf("外部证书照旧由签发它的工具续: %v", err)
	}
}
