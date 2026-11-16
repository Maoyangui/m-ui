package guard

import (
	"fmt"
	"net/netip"
	"path/filepath"
	"testing"

	"github.com/Maoyangui/m-ui/database"
	"github.com/Maoyangui/m-ui/database/model"

	"gorm.io/gorm"
)

// rig 一个带假时钟的 Guard:on 控制自动拉黑的开关,trusted 是集群里的服务器。
type rig struct {
	t   *testing.T
	db  *gorm.DB
	g   *Guard
	now int64
	on  bool
}

func newRig(t *testing.T, trusted ...string) *rig {
	t.Helper()
	db, err := database.Open(filepath.Join(t.TempDir(), "guard.db"))
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { database.Close(db) })
	r := &rig{t: t, db: db, now: 1_800_000_000, on: true}
	trust := map[netip.Addr]bool{}
	for _, s := range trusted {
		trust[netip.MustParseAddr(s)] = true
	}
	r.g = New(db, func() bool { return r.on }, func(a netip.Addr) bool { return trust[a] })
	r.g.now = func() int64 { return r.now }
	return r
}

func (r *rig) loginFails(ip string, n int) {
	for i := 0; i < n; i++ {
		r.g.LoginFail(ip)
	}
}

func (r *rig) ban(ip string) model.FwBan {
	r.t.Helper()
	var b model.FwBan
	if err := r.db.Where("ip = ?", ip).First(&b).Error; err != nil {
		r.t.Fatalf("库里应有 %s 的黑名单记录:%v", ip, err)
	}
	return b
}

func TestKey(t *testing.T) {
	for in, want := range map[string]string{
		"203.0.113.7":          "203.0.113.7/32",
		" 203.0.113.7 ":        "203.0.113.7/32",
		"::ffff:203.0.113.7":   "203.0.113.7/32",
		"2001:db8:1:2:3:4::5":  "2001:db8:1:2::/64",
		"2001:db8:1:2::1%eth0": "2001:db8:1:2::/64",
	} {
		k, ok := Key(in)
		if !ok || k.String() != want {
			t.Errorf("Key(%q) = %v, %v;应是 %s", in, k, ok, want)
		}
	}
	for _, bad := range []string{"", "example.com", "1.2.3", "1.2.3.4/24"} {
		if _, ok := Key(bad); ok {
			t.Errorf("Key(%q) 不该成功", bad)
		}
	}
}

// 自动拉黑默认是关着的:关着时怎么失败都不拉黑;手动加的照常生效。
func TestAutoBanOffByDefault(t *testing.T) {
	r := newRig(t)
	r.on = false
	r.loginFails("203.0.113.7", 50)
	for i := 0; i < 50; i++ {
		r.g.SubMiss("203.0.113.7", fmt.Sprint("tok", i))
		r.g.APIMiss("203.0.113.7", fmt.Sprint("key", i))
	}
	if r.g.Blocked("203.0.113.7") {
		t.Fatal("自动拉黑关着的时候不该拉黑任何来源")
	}
	if _, err := r.g.Add("203.0.113.8", "手动", 0); err != nil {
		t.Fatal(err)
	}
	if !r.g.Blocked("203.0.113.8") {
		t.Fatal("手动加的黑名单和自动拉黑的开关无关,应该生效")
	}
}

// 登录:10 分钟内失败 10 次封 1 小时;到点自动解除;窗口之外的失败不算。
func TestLoginBan(t *testing.T) {
	r := newRig(t)
	r.loginFails("203.0.113.7", loginLimit-1)
	if r.g.Blocked("203.0.113.7") {
		t.Fatal("还差一次,不该拉黑")
	}
	r.g.LoginFail("203.0.113.7")
	if !r.g.Blocked("203.0.113.7") {
		t.Fatal("第 10 次失败应拉黑")
	}
	b := r.ban("203.0.113.7")
	if !b.Auto || b.Reason != model.BanLogin || b.Hits != 1 || b.Expiry != r.now+banFor || b.Evidence == "" {
		t.Fatalf("黑名单记录不对:%+v", b)
	}
	if r.g.Blocked("203.0.113.8") {
		t.Fatal("别的来源不受影响")
	}
	// 到点:不用等 Reload,Blocked 自己认到期
	r.now += banFor + 1
	if r.g.Blocked("203.0.113.7") {
		t.Fatal("到期后应自动解除")
	}
	r.g.Reload()
	var n int64
	r.db.Model(&model.FwBan{}).Count(&n)
	if n != 0 {
		t.Fatalf("到期的记录应在 Reload 时清掉,还剩 %d 条", n)
	}
	// 稀稀拉拉的失败(每次隔两分钟,10 分钟窗口里凑不够 10 次)不拉黑
	for i := 0; i < 30; i++ {
		r.g.LoginFail("203.0.113.9")
		r.now += 120
	}
	if r.g.Blocked("203.0.113.9") {
		t.Fatal("窗口里凑不够次数的不该拉黑")
	}
}

// 登录成功过的来源 24 小时内不拉黑(自己人输错密码不该被关在门外),之前的失败也清零。
func TestLoginOKExempts(t *testing.T) {
	r := newRig(t)
	r.loginFails("203.0.113.7", loginLimit-1)
	r.g.LoginOK("203.0.113.7")
	r.loginFails("203.0.113.7", loginLimit*3)
	if r.g.Blocked("203.0.113.7") {
		t.Fatal("24 小时内登录成功过的来源不该被拉黑")
	}
	r.now += goodFor + 1
	r.loginFails("203.0.113.7", loginLimit)
	if !r.g.Blocked("203.0.113.7") {
		t.Fatal("豁免过期后照常计数")
	}
}

// 订阅口:数的是「不同的」坏地址。同一个作废的地址反复刷新(客户端还存着旧订阅)只算一个;
// 成功拉过订阅的来源(多半是真用户的出口)门槛放宽 5 倍。
func TestSubMiss(t *testing.T) {
	r := newRig(t)
	for i := 0; i < 500; i++ {
		r.g.SubMiss("203.0.113.7", "same-old-token")
	}
	if r.g.Blocked("203.0.113.7") {
		t.Fatal("同一个旧地址反复刷新不该拉黑")
	}
	for i := 0; i < subLimit-2; i++ { // 加上上面那一个,共 subLimit-1 个不同的
		r.g.SubMiss("203.0.113.7", fmt.Sprint("guess-", i))
	}
	if r.g.Blocked("203.0.113.7") {
		t.Fatal("还差一个不同的地址,不该拉黑")
	}
	r.g.SubMiss("203.0.113.7", "one-more")
	if !r.g.Blocked("203.0.113.7") || r.ban("203.0.113.7").Reason != model.BanSub {
		t.Fatal("试到第 8 个不同的坏地址应拉黑,原因是订阅")
	}
	// 成功拉过订阅的来源:8 个不够,要 40 个
	r.g.SubOK("198.51.100.5")
	for i := 0; i < subLimit*relax-1; i++ {
		r.g.SubMiss("198.51.100.5", fmt.Sprint("t", i))
	}
	if r.g.Blocked("198.51.100.5") {
		t.Fatal("成功拉过订阅的来源门槛应放宽 5 倍")
	}
	r.g.SubMiss("198.51.100.5", "last")
	if !r.g.Blocked("198.51.100.5") {
		t.Fatal("放宽后的门槛到了也要拉黑")
	}
}

func TestAPIMiss(t *testing.T) {
	r := newRig(t)
	for i := 0; i < apiLimit-1; i++ {
		r.g.APIMiss("203.0.113.7", fmt.Sprint("k", i))
	}
	if r.g.Blocked("203.0.113.7") {
		t.Fatal("还差一把,不该拉黑")
	}
	r.g.APIMiss("203.0.113.7", "k-last")
	if !r.g.Blocked("203.0.113.7") || r.ban("203.0.113.7").Reason != model.BanAPI {
		t.Fatal("试到第 10 把不同的错密钥应拉黑,原因是 API")
	}
}

// 永远不自动拉黑的:回环 / 内网、集群里的服务器、白名单里的。
func TestExemptions(t *testing.T) {
	r := newRig(t, "198.51.100.20")
	if err := r.db.Create(&model.FwAllow{Ip: "192.0.2.0/24", Note: "办公网"}).Error; err != nil {
		t.Fatal(err)
	}
	if err := r.db.Create(&model.FwAllow{Ip: "2001:db8:aa::/48"}).Error; err != nil {
		t.Fatal(err)
	}
	r.g.Reload()
	for _, ip := range []string{"127.0.0.1", "10.1.2.3", "192.168.1.9", "::1", "fd00::1", "198.51.100.20", "192.0.2.77", "2001:db8:aa:1::5"} {
		r.loginFails(ip, loginLimit*3)
		for i := 0; i < subLimit*3; i++ {
			r.g.SubMiss(ip, fmt.Sprint(ip, i))
		}
		if r.g.Blocked(ip) {
			t.Errorf("%s 属于永不自动拉黑的来源,却被拉黑了", ip)
		}
	}
	// 白名单外的照常
	r.loginFails("192.0.3.1", loginLimit)
	if !r.g.Blocked("192.0.3.1") {
		t.Fatal("白名单外的来源照常拉黑")
	}
}

// IPv6 按 /64 计数和拉黑:同一个 /64 里换着地址试,算同一个来源。
func TestIPv6ByPrefix(t *testing.T) {
	r := newRig(t)
	for i := 0; i < loginLimit; i++ {
		r.g.LoginFail(fmt.Sprintf("2001:db8:5:6::%x", i+1))
	}
	if !r.g.Blocked("2001:db8:5:6:ffff::1") {
		t.Fatal("同一个 /64 里的地址应一起被拉黑")
	}
	if r.g.Blocked("2001:db8:5:7::1") {
		t.Fatal("隔壁 /64 不受影响")
	}
	if b := r.ban("2001:db8:5:6::/64"); !b.Auto {
		t.Fatalf("库里应记成 /64:%+v", b)
	}
}

// 屡犯:24 小时内第 3 次被封起封一天;隔了一天以上重新数。
func TestRepeatOffender(t *testing.T) {
	r := newRig(t)
	for round := 1; round <= 3; round++ {
		r.loginFails("203.0.113.7", loginLimit)
		b := r.ban("203.0.113.7")
		want := int64(banFor)
		if round >= repeatAfter {
			want = repeatFor
		}
		if b.Hits != round || b.Expiry-r.now != want {
			t.Fatalf("第 %d 次:应封 %d 秒、Hits=%d,实际 %+v", round, want, round, b)
		}
		r.now = b.Expiry + 10 // 解封后马上又来
		// 记录还在库里(没 Reload 清掉),下一轮接着数
	}
	// 一天多以后再犯:从头数
	r.now += repeatFor + 100
	r.loginFails("203.0.113.7", loginLimit)
	if b := r.ban("203.0.113.7"); b.Hits != 1 || b.Expiry-r.now != banFor {
		t.Fatalf("隔了一天以上应重新数:%+v", b)
	}
}

// 手动加的:IP、网段、单个 IPv6;可设期限;已有的自动记录改成手动;不合理的输入拒绝。自动拉黑不动手动加的永久条目。
func TestManualAddRemove(t *testing.T) {
	r := newRig(t)
	if _, err := r.g.Add("203.0.113.0/24", "扫号的一段", 0); err != nil {
		t.Fatal(err)
	}
	if !r.g.Blocked("203.0.113.200") || r.g.Blocked("203.0.114.1") {
		t.Fatal("网段应整段生效,段外不受影响")
	}
	if _, err := r.g.Add("2001:db8::9", "", 600); err != nil {
		t.Fatal(err)
	}
	if !r.g.Blocked("2001:db8::9") || r.g.Blocked("2001:db8::a") {
		t.Fatal("手动加的单个 IPv6 只封这一个地址,不连累同一个 /64")
	}
	r.now += 601
	if r.g.Blocked("2001:db8::9") {
		t.Fatal("手动加的带期限的到点也要解除")
	}
	for _, bad := range []string{"", "abc", "0.0.0.0/0", "::/0", "127.0.0.1", "10.0.0.0/8", "192.168.1.1"} {
		if _, err := r.g.Add(bad, "", 0); err == nil {
			t.Errorf("Add(%q) 应该被拒绝", bad)
		}
	}
	// 自动拉黑的记录,手动再加一次 = 改成手动、永久
	r.loginFails("198.51.100.9", loginLimit)
	if b, err := r.g.Add("198.51.100.9", "确认是坏人", 0); err != nil || b.Auto || b.Expiry != 0 || b.Reason != model.BanManual {
		t.Fatalf("应改成手动的永久条目:%+v %v", b, err)
	}
	// 手动的永久条目,自动拉黑碰不到它(不会被改成 1 小时后到期)
	r.loginFails("198.51.100.9", loginLimit*2)
	if b := r.ban("198.51.100.9"); b.Auto || b.Expiry != 0 {
		t.Fatalf("自动拉黑不该改动手动加的条目:%+v", b)
	}
	// 移出、清空
	if n := r.g.Remove(0, "198.51.100.9"); n != 1 || r.g.Blocked("198.51.100.9") {
		t.Fatalf("按 IP 移出应成功:%d", n)
	}
	r.loginFails("198.51.100.10", loginLimit)
	if n := r.g.Clear(true); n != 1 {
		t.Fatalf("只清自动拉黑的应清掉 1 条,实际 %d", n)
	}
	if !r.g.Blocked("203.0.113.5") {
		t.Fatal("只清自动的,手动加的网段应该还在")
	}
	if n := r.g.Clear(false); n < 1 || r.g.Blocked("203.0.113.5") {
		t.Fatalf("全部清空后不该再拦:%d", n)
	}
}

// 新拉黑时回调一次(发通知、记审计用)。
func TestOnBan(t *testing.T) {
	r := newRig(t)
	var got []model.FwBan
	r.g.OnBan(func(b model.FwBan) { got = append(got, b) })
	r.loginFails("203.0.113.7", loginLimit)
	if len(got) != 1 || got[0].Ip != "203.0.113.7" || got[0].Reason != model.BanLogin {
		t.Fatalf("应回调一次:%+v", got)
	}
}

// nil 的 Guard(测试里、还没初始化)所有入口都安全。
func TestNilGuard(t *testing.T) {
	var g *Guard
	if g.Blocked("203.0.113.7") {
		t.Fatal("nil 不拦任何人")
	}
	g.LoginOK("203.0.113.7")
	g.SubOK("203.0.113.7")
	g.LoginFail("203.0.113.7")
	g.SubMiss("203.0.113.7", "x")
	g.APIMiss("203.0.113.7", "x")
}
