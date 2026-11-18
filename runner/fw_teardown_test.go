package runner

import (
	"net/netip"
	"strings"
	"testing"
	"time"

	"github.com/Maoyangui/m-ui/database/model"
	"github.com/Maoyangui/m-ui/fw"
)

// 卸载收尾:m-ui 自己那张表撤掉;它在 ufw 里放行的端口收回,用户自己放行的(22)不碰;
// 「放开系统防火墙」不在这里撤,只如实报告还开着。
func TestFwTeardownRemovesOnlyOurs(t *testing.T) {
	f := newFwFake("ufw", "nft")
	added := []string{"ufw allow 22/tcp"}
	render := func() {
		f.out["ufw show added"] = "Added user rules (see 'ufw status' for running firewall):\n" + strings.Join(added, "\n") + "\n"
	}
	render()
	f.out["ufw status"] = "Status: active\n"
	f.on = func(f *fwFake, line string) {
		if strings.HasPrefix(line, "ufw allow ") {
			added = append(added, strings.TrimSuffix(line, " comment m-ui")+" comment 'm-ui'")
		}
		if strings.HasPrefix(line, "ufw delete allow ") {
			spec := strings.TrimPrefix(line, "ufw delete allow ")
			var keep []string
			for _, a := range added {
				if !strings.HasPrefix(a, "ufw allow "+spec+" ") && a != "ufw allow "+spec {
					keep = append(keep, a)
				}
			}
			added = keep
		}
		render()
	}
	r := fwTestRunner(t, f)
	r.db.Create(&model.FwRule{Id: 1, Kind: fw.KindDeny, Sources: "203.0.113.0/24", Enabled: true})
	r.fwTick(true)
	if rec := fwAllowedOf(r.db); strings.Join(rec.Ports, ",") != "2053/tcp,2056/tcp" {
		t.Fatalf("对账后应当记着放行了面板与订阅端口: %+v", rec)
	}
	if !strings.Contains(f.lastScript(), "203.0.113.0/24") {
		t.Fatalf("面板规则应当已经落下去: %s", f.lastScript())
	}

	f.reset()
	out := fwTeardownDB(r.db, f)
	if strings.Join(out.Ports, ",") != "2053/tcp,2056/tcp" || out.Opened || len(out.Errs) != 0 {
		t.Fatalf("收尾结果不对: %+v", out)
	}
	if got := strings.Join(f.with("ufw delete"), "|"); got != "ufw delete allow 2053/tcp|ufw delete allow 2056/tcp" {
		t.Fatalf("应当只收回 m-ui 加的两个端口: %s", got)
	}
	if strings.Join(added, "|") != "ufw allow 22/tcp" {
		t.Fatalf("用户自己放行的不该动: %v", added)
	}
	if s := f.lastScript(); strings.Contains(s, "203.0.113.0/24") || !strings.Contains(s, "delete table") {
		t.Fatalf("m-ui 自己那张表应当撤掉,不留规则: %q", s)
	}
	if rec := fwAllowedOf(r.db); len(rec.Ports) != 0 {
		t.Fatalf("记录应当清掉: %+v", rec)
	}
	// 再来一遍:没有可收的了,不报错、不再动 ufw
	f.reset()
	if out := fwTeardownDB(r.db, f); len(out.Ports) != 0 || len(out.Errs) != 0 || len(f.with("ufw delete")) != 0 {
		t.Fatalf("第二遍不该再收: %+v %v", out, f.with("ufw delete"))
	}
}

// 放开过系统防火墙:收尾不撤销它,只报告;收回端口出错也不挡,原因带出来。
func TestFwTeardownReportsOpenedAndErrors(t *testing.T) {
	f := newFwFake("ufw", "nft")
	f.out["ufw status"] = "Status: active\n"
	f.out["ufw show added"] = "Added user rules (see 'ufw status' for running firewall):\nufw allow 2053/tcp comment 'm-ui'\n"
	r := fwTestRunner(t, f)
	_ = saveSettings(r.db, map[string]string{fwAllowedKey: `{"frontend":"ufw","ports":["2053/tcp"]}`})
	f.fail["ufw delete allow 2053/tcp"] = "ERROR: could not delete"
	out := fwTeardownDB(r.db, f)
	if len(out.Errs) != 1 || !strings.Contains(out.Errs[0], "could not delete") {
		t.Fatalf("收不回来应当如实说: %+v", out)
	}
	delete(f.fail, "ufw delete allow 2053/tcp")
	_ = saveSettings(r.db, map[string]string{fwOpenedKey: `{"frontend":"ufw","actions":["ufw"],"at":1}`})
	if out := fwTeardownDB(r.db, f); !out.Opened {
		t.Fatalf("放开过的应当报告 Opened: %+v", out)
	}
	if len(f.with("ufw enable")) != 0 || len(f.with("ufw --force enable")) != 0 {
		t.Fatalf("收尾不该替用户撤销放开: %v", f.with("ufw"))
	}
}

// 只读地看:数得出规则和黑名单,认得出某个地址是不是被拉黑了(单个 IP、网段、IPv6 的 /64、到期的不算)。
func TestFwPeekBanned(t *testing.T) {
	f := newFwFake("nft")
	r := fwTestRunner(t, f)
	now := time.Now().Unix()
	r.db.Create(&model.FwBan{Ip: "203.0.113.5", Auto: true})
	r.db.Create(&model.FwBan{Ip: "198.51.100.0/24"})
	r.db.Create(&model.FwBan{Ip: "2001:db8:1:2::/64", Auto: true})
	r.db.Create(&model.FwBan{Ip: "192.0.2.9", Expiry: now - 10}) // 到期了
	r.db.Create(&model.FwRule{Id: 1, Kind: fw.KindDeny, Sources: "203.0.113.0/24", Enabled: true})
	_ = saveSettings(r.db, map[string]string{fwPausedKey: "true"})
	for ip, want := range map[string]bool{
		"203.0.113.5": true, "203.0.113.6": false, "198.51.100.77": true, "2001:db8:1:2::abcd": true,
		"2001:db8:1:3::1": false, "192.0.2.9": false, "::ffff:203.0.113.5": true, "": false, "不是地址": false,
	} {
		out, banned := fwPeekDB(r.db, f, ip, now)
		if banned != want {
			t.Errorf("%q 在不在黑名单:要 %v,得 %v", ip, want, banned)
		}
		if out.Bans != 3 || out.Rules != 1 || !out.Paused {
			t.Fatalf("计数不对: %+v", out)
		}
	}
	if !banCovers("203.0.113.5/24", netip.MustParseAddr("203.0.113.200")) {
		t.Fatal("没对齐的网段写法也应当按网段算")
	}
}
