// Package guard 是应用层的黑名单:面板、代理面板、订阅口和外部 API 对名单里的来源直接断开连接。
// 它不碰内核、不动代理流量(同一个出口 IP 后面可能还有正常用户);要连代理一起挡,在面板里把那一条升成防火墙规则(fw 包)。
//
// 名单有两个来路:手动加的,和自动拉黑的。自动拉黑默认是关着的(设置 fwAutoBan),打开以后也只认「明显在扫」的行为:
//
//	面板 / 代理面板登录:10 分钟内失败 10 次            → 封 1 小时
//	订阅口:10 分钟内试了 8 个不同的、不存在的地址      → 封 1 小时(同一个旧地址反复刷新只算一个:那是客户端还存着作废的订阅)
//	外部 API:10 分钟内试了 10 个不同的错密钥            → 封 1 小时
//	24 小时内第 3 次被封                                  → 封 24 小时
//
// 宁可漏、不可错:回环 / 内网地址、集群里的服务器、白名单、24 小时内登录成功过的来源一律不封;
// 24 小时内成功拉过订阅的来源(多半是真用户的出口)门槛放宽到 5 倍。全部是临时的,到点自动解除,面板里随时能移出。
// 计数只在内存里:重启清零,正常人不会被算旧账。
package guard

import (
	"crypto/sha256"
	"encoding/hex"
	"fmt"
	"net/netip"
	"strings"
	"sync"
	"time"

	"github.com/Maoyangui/m-ui/database/model"

	"gorm.io/gorm"
)

const (
	window      = 600    // 计数的时间窗(秒)
	loginLimit  = 10     // 登录失败几次
	subLimit    = 8      // 试了几个不同的坏订阅地址
	apiLimit    = 10     // 试了几个不同的错密钥
	relax       = 5      // 最近成功拉过订阅的来源:门槛放宽的倍数
	banFor      = 3600   // 封多久(秒)
	repeatAfter = 3      // 24 小时内第几次被封起加长
	repeatFor   = 86400  // 屡犯封多久
	goodFor     = 86400  // 「最近成功过」记多久
	maxTrack    = 200000 // 最多同时记多少个来源的计数(防止被海量来源撑爆内存;满了先清过期的,再不够就不记新的)
)

// Key 把来源地址规整成计数、拉黑用的键:IPv4 是它自己,IPv6 是所在的 /64(一个家宽、一台机器手里就是一整段)。
func Key(ip string) (netip.Prefix, bool) {
	a, err := netip.ParseAddr(strings.TrimSpace(ip))
	if err != nil {
		return netip.Prefix{}, false
	}
	a = a.Unmap().WithZone("")
	if a.Is4() {
		return netip.PrefixFrom(a, 32), true
	}
	return netip.PrefixFrom(a, 64).Masked(), true
}

// neverBan 永远不该拉黑的地址:回环、内网、链路本地、未指定。
func neverBan(a netip.Addr) bool {
	return !a.IsValid() || a.IsLoopback() || a.IsPrivate() || a.IsLinkLocalUnicast() || a.IsUnspecified() || a.IsMulticast()
}

// set 黑名单在内存里的一份(库里的 fw_bans 是底):查一次只读锁加两次查表。
type set struct {
	mu   sync.RWMutex
	one  map[netip.Prefix]int64 // /32 与 /64 的整键 → 到期(0 = 永久)
	nets []entry                // 其它网段
}
type entry struct {
	p   netip.Prefix
	exp int64
}

func (s *set) replace(list []model.FwBan) {
	one, nets := map[netip.Prefix]int64{}, []entry(nil)
	for _, b := range list {
		p, ok := parsePrefix(b.Ip)
		if !ok {
			continue
		}
		if (p.Addr().Is4() && p.Bits() == 32) || (p.Addr().Is6() && p.Bits() == 64) {
			one[p] = b.Expiry
		} else {
			nets = append(nets, entry{p, b.Expiry})
		}
	}
	s.mu.Lock()
	s.one, s.nets = one, nets
	s.mu.Unlock()
}

func (s *set) blocked(ip string, now int64) bool {
	k, ok := Key(ip)
	if !ok {
		return false
	}
	live := func(exp int64) bool { return exp == 0 || exp > now }
	s.mu.RLock()
	defer s.mu.RUnlock()
	if exp, hit := s.one[k]; hit && live(exp) {
		return true
	}
	a := k.Addr()
	if a.Is6() { // 手动拉黑的单个 v6 地址存成 /128,不在按 /64 的那张表里
		a, _ = netip.ParseAddr(strings.TrimSpace(ip))
		a = a.Unmap().WithZone("")
	}
	for _, e := range s.nets {
		if e.p.Contains(a) && live(e.exp) {
			return true
		}
	}
	return false
}

// parsePrefix 库里存的 IP 或网段。
func parsePrefix(s string) (netip.Prefix, bool) {
	s = strings.TrimSpace(s)
	if p, err := netip.ParsePrefix(s); err == nil {
		return netip.PrefixFrom(p.Addr().Unmap(), p.Bits()).Masked(), true
	}
	if a, err := netip.ParseAddr(s); err == nil {
		a = a.Unmap().WithZone("")
		return netip.PrefixFrom(a, a.BitLen()), true
	}
	return netip.Prefix{}, false
}

// Guard 黑名单 + 自动拉黑。零值不能用,用 New。
type Guard struct {
	db      *gorm.DB
	bans    set
	enabled func() bool                 // 自动拉黑开没开(设置 fwAutoBan)
	trusted func(a netip.Addr) bool     // 集群里的服务器(主机、各副机的地址):不封
	onBan   func(b model.FwBan)         // 新拉黑了一个(发通知、记审计);可为 nil
	now     func() int64                // 测试里换钟
	mu      sync.Mutex                  // 护着下面的计数
	fails   map[netip.Prefix][]int64    // 登录失败的时间
	seen    map[string]map[string]int64 // "sub|键" / "api|键" → 坏令牌的指纹 → 最近一次
	goodIn  map[netip.Prefix]int64      // 最近一次登录成功
	goodSub map[netip.Prefix]int64      // 最近一次成功拉订阅
	allow   []netip.Prefix              // 白名单
}

// New 建一个。enabled、trusted 可为 nil(= 关着 / 没有可信地址)。
func New(db *gorm.DB, enabled func() bool, trusted func(netip.Addr) bool) *Guard {
	g := &Guard{db: db, enabled: enabled, trusted: trusted, now: func() int64 { return time.Now().Unix() },
		fails: map[netip.Prefix][]int64{}, seen: map[string]map[string]int64{}, goodIn: map[netip.Prefix]int64{}, goodSub: map[netip.Prefix]int64{}}
	g.Reload()
	return g
}

// OnBan 设置「新拉黑了一个」的回调。
func (g *Guard) OnBan(fn func(model.FwBan)) { g.onBan = fn }

// Reload 从库里重读黑名单和白名单;到期的黑名单顺手删掉。名单改过之后、以及定时(到期要靠它生效)调。
func (g *Guard) Reload() {
	if g.db == nil {
		return
	}
	now := g.now()
	g.db.Where("expiry > 0 AND expiry <= ?", now).Delete(&model.FwBan{})
	var bans []model.FwBan
	g.db.Find(&bans)
	g.bans.replace(bans)
	var rows []model.FwAllow
	g.db.Find(&rows)
	allow := make([]netip.Prefix, 0, len(rows))
	for _, r := range rows {
		if p, ok := parsePrefix(r.Ip); ok {
			allow = append(allow, p)
		}
	}
	g.mu.Lock()
	g.allow = allow
	g.reapLocked(now)
	g.mu.Unlock()
}

// Blocked 这个来源在不在黑名单里。各监听在最外层调,命中就断开。
func (g *Guard) Blocked(ip string) bool {
	if g == nil {
		return false
	}
	return g.bans.blocked(ip, g.now())
}

func (g *Guard) on() bool { return g != nil && g.enabled != nil && g.enabled() }

// exempt 这个来源永远不自动拉黑:回环 / 内网、集群里的服务器、白名单。
func (g *Guard) exempt(k netip.Prefix) bool {
	a := k.Addr()
	if neverBan(a) || (g.trusted != nil && g.trusted(a)) {
		return true
	}
	for _, p := range g.allow {
		if p.Overlaps(k) {
			return true
		}
	}
	return false
}

func (g *Guard) reapLocked(now int64) {
	for k, ts := range g.fails {
		if len(ts) == 0 || now-ts[len(ts)-1] > window {
			delete(g.fails, k)
		}
	}
	for k, m := range g.seen {
		for tok, at := range m {
			if now-at > window {
				delete(m, tok)
			}
		}
		if len(m) == 0 {
			delete(g.seen, k)
		}
	}
	for k, at := range g.goodIn {
		if now-at > goodFor {
			delete(g.goodIn, k)
		}
	}
	for k, at := range g.goodSub {
		if now-at > goodFor {
			delete(g.goodSub, k)
		}
	}
}

func (g *Guard) room(now int64) bool {
	if len(g.fails)+len(g.seen) < maxTrack {
		return true
	}
	g.reapLocked(now)
	return len(g.fails)+len(g.seen) < maxTrack
}

// LoginOK 这个来源登录成功了:24 小时内不自动拉黑它(自己人输错几次密码不该被关在门外),之前的失败也不算了。
func (g *Guard) LoginOK(ip string) {
	k, ok := Key(ip)
	if g == nil || !ok {
		return
	}
	g.mu.Lock()
	g.goodIn[k] = g.now()
	delete(g.fails, k)
	g.mu.Unlock()
}

// SubOK 这个来源成功拉到了订阅:多半是真用户的出口,它的门槛放宽。
func (g *Guard) SubOK(ip string) {
	k, ok := Key(ip)
	if g == nil || !ok {
		return
	}
	g.mu.Lock()
	if len(g.goodSub) < maxTrack {
		g.goodSub[k] = g.now()
	}
	g.mu.Unlock()
}

// LoginFail 面板 / 代理面板的一次登录失败。
func (g *Guard) LoginFail(ip string) {
	k, ok := Key(ip)
	if !g.on() || !ok {
		return
	}
	now := g.now()
	g.mu.Lock()
	if g.exempt(k) || now-g.goodIn[k] < goodFor && g.goodIn[k] > 0 || !g.room(now) {
		g.mu.Unlock()
		return
	}
	recent := g.fails[k][:0]
	for _, ts := range g.fails[k] {
		if now-ts < window {
			recent = append(recent, ts)
		}
	}
	recent = append(recent, now)
	g.fails[k] = recent
	n := len(recent)
	if n >= loginLimit {
		delete(g.fails, k)
	}
	g.mu.Unlock()
	if n >= loginLimit {
		g.ban(k, model.BanLogin, fmt.Sprintf("10 分钟内登录失败 %d 次", n))
	}
}

// SubMiss 订阅口上一次对不上任何人的请求;token 是地址里那段(只记它的指纹)。
func (g *Guard) SubMiss(ip, token string) {
	g.miss(model.BanSub, ip, token, subLimit, "10 分钟内试了 %d 个不存在的订阅地址")
}

// APIMiss 外部 API 上一次密钥不对的请求。
func (g *Guard) APIMiss(ip, key string) {
	g.miss(model.BanAPI, ip, key, apiLimit, "10 分钟内试了 %d 个错误的 API 密钥")
}

func (g *Guard) miss(kind, ip, token string, limit int, evidence string) {
	k, ok := Key(ip)
	if !g.on() || !ok {
		return
	}
	now := g.now()
	sum := sha256.Sum256([]byte(token))
	fp := hex.EncodeToString(sum[:8])
	g.mu.Lock()
	if g.exempt(k) || (g.goodIn[k] > 0 && now-g.goodIn[k] < goodFor) || !g.room(now) {
		g.mu.Unlock()
		return
	}
	if g.goodSub[k] > 0 && now-g.goodSub[k] < goodFor {
		limit *= relax
	}
	mk := kind + "|" + k.String()
	m := g.seen[mk]
	if m == nil {
		m = map[string]int64{}
		g.seen[mk] = m
	}
	for tok, at := range m {
		if now-at > window {
			delete(m, tok)
		}
	}
	if len(m) < limit*2 { // 一个来源最多记这么多个指纹:再多也只是同一个结论
		m[fp] = now
	}
	n := len(m)
	if n >= limit {
		delete(g.seen, mk)
	}
	g.mu.Unlock()
	if n >= limit {
		g.ban(k, kind, fmt.Sprintf(evidence, n))
	}
}

// ban 自动拉黑一个来源:临时的;24 小时内第三次起封一天。已经在名单里的(手动加的永久条目)不动。
func (g *Guard) ban(k netip.Prefix, reason, evidence string) {
	if g.db == nil {
		return
	}
	now := g.now()
	ip := k.String()
	if k.Addr().Is4() {
		ip = k.Addr().String()
	}
	var b model.FwBan
	if g.db.Where("ip = ?", ip).First(&b).Error == nil {
		if !b.Auto {
			return
		}
		if now-b.LastAt > repeatFor {
			b.Hits = 0 // 上一次是一天以前的事了:重新数
		}
	} else {
		b = model.FwBan{Ip: ip, Auto: true, FirstAt: now}
	}
	b.Hits++
	b.Reason, b.Evidence, b.LastAt = reason, evidence, now
	b.Expiry = now + banFor
	if b.Hits >= repeatAfter {
		b.Expiry = now + repeatFor
	}
	if err := g.db.Save(&b).Error; err != nil {
		return
	}
	g.Reload()
	if g.onBan != nil {
		g.onBan(b)
	}
}

// Add 手动拉黑一个 IP 或网段;seconds 为 0 = 一直有效。已在名单里的改成这次给的期限和备注。
func (g *Guard) Add(ipOrNet, note string, seconds int64) (model.FwBan, error) {
	p, ok := parsePrefix(ipOrNet)
	if !ok {
		return model.FwBan{}, fmt.Errorf("「%s」不是 IP 或网段(例:1.2.3.4、203.0.113.0/24、2001:db8::/32)", strings.TrimSpace(ipOrNet))
	}
	if p.Bits() == 0 {
		return model.FwBan{}, fmt.Errorf("不能把全部地址加进黑名单")
	}
	if neverBan(p.Addr()) {
		return model.FwBan{}, fmt.Errorf("%s 是回环或内网地址,不能加进黑名单", p.Addr())
	}
	now := g.now()
	ip := p.String()
	if p.IsSingleIP() {
		ip = p.Addr().String()
	}
	var b model.FwBan
	if g.db.Where("ip = ?", ip).First(&b).Error != nil {
		b = model.FwBan{Ip: ip, FirstAt: now}
	}
	b.Auto, b.Reason, b.Evidence, b.LastAt, b.Expiry = false, model.BanManual, strings.TrimSpace(note), now, 0
	if seconds > 0 {
		b.Expiry = now + seconds
	}
	if err := g.db.Save(&b).Error; err != nil {
		return b, err
	}
	g.Reload()
	return b, nil
}

// Remove 把一条移出黑名单(id 为 0 时按 ip 找);返回移出了几条。
func (g *Guard) Remove(id uint, ip string) int64 {
	q := g.db
	if id > 0 {
		q = q.Where("id = ?", id)
	} else {
		q = q.Where("ip = ?", strings.TrimSpace(ip))
	}
	n := q.Delete(&model.FwBan{}).RowsAffected
	g.Reload()
	return n
}

// Clear 清空黑名单(onlyAuto 为真只清自动拉黑的);返回清了几条。命令行的退路「m-ui unban --all」用它。
func (g *Guard) Clear(onlyAuto bool) int64 {
	q := g.db.Session(&gorm.Session{AllowGlobalUpdate: true})
	if onlyAuto {
		q = q.Where("auto = ?", true)
	}
	n := q.Delete(&model.FwBan{}).RowsAffected
	g.Reload()
	return n
}
