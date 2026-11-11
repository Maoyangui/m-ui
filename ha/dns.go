package ha

import (
	"context"
	"errors"
	"fmt"
	"net"
	"strings"
	"time"

	"github.com/Maoyangui/m-ui/acme"
	"github.com/Maoyangui/m-ui/logger"
	"github.com/Maoyangui/m-ui/notify"
)

// 主机变了(接任或设为主机)以后自动改 DNS(Cloudflare,默认不开):对每个要跟着主机走的域名,只把当前值等于
// 原主机公网 IP 的 A / AAAA 记录改成本机的 IP,类型、橙云、TTL 都不动;记下原值。失败只告警,不挡接任。
// 本机缺那个地址族(或公网 IP 还没测到)时,指向原主机的记录改不了:明确记成"跳过",通知里写清要手动处理。
// 误接任后让出主机时,把本任期改过、当前仍指向本机的记录改回原值。
// 令牌单独存(haDnsToken,不复用签证书的 acmeCfToken),不进日志、不经设置接口回显。
// 接口地址只能由进程环境变量 M_UI_TEST_CF_API(只供测试,见 runner)或测试里的 SetDNSAPI 换掉,库里没有这个设置。

// DnsChange 自动改的一条 DNS 记录。Skip 不为空 = 没改(原因代码):noV4 / noV6 本机没有这个地址族,noIP 本机公网 IP 还没测到。
type DnsChange struct {
	Name string `json:"name"`
	Type string `json:"type"`
	From string `json:"from"`
	To   string `json:"to,omitempty"`
	Skip string `json:"skip,omitempty"`
}

// dnsRecord 记下来的改动(本机设置 haDnsChanged)。
type dnsRecord struct {
	At      int64       `json:"at"`
	Changes []DnsChange `json:"changes,omitempty"`
	Err     string      `json:"err,omitempty"`
	Revert  bool        `json:"revert,omitempty"` // 这是让出主机时改回去的记录
}

// DnsOutcome 一次自动改 DNS 的结果。State:done 全部改好;partial 改了一部分(有失败或跳过);failed 一条也没改成;
// todo 要管理员自己改(没配自动改、订阅主机名不在名单里、或没找到指向原主机的记录);"" 不用改(订阅地址是 IP、没有订阅主机名)。
type DnsOutcome struct {
	State            string
	SubDone          bool // 订阅主机名本身的匹配记录全部改好
	Changes, Skipped []DnsChange
	Err              string
}

// SetDNSAPI 换掉 Cloudflare 接口地址(测试接 httptest;真机只经环境变量 M_UI_TEST_CF_API 设)。
func (s *Service) SetDNSAPI(u string) { s.dnsAPI.Store(strings.TrimSpace(u)) }

// DNSAPIOverride 换掉了的 Cloudflare 接口地址(没换为空):设置页据此提示"正在用测试地址"。
func (s *Service) DNSAPIOverride() string {
	if v, ok := s.dnsAPI.Load().(string); ok && v != "" {
		return v
	}
	return s.d.DNSAPI
}

func (s *Service) cloudflare(token string) *acme.Cloudflare {
	return &acme.Cloudflare{Token: token, BaseURL: s.DNSAPIOverride()}
}

// ownIPs 本机的公网 IPv4 / IPv6(纯 IPv6 机器 publicIp 记的是 IPv6)。
func (s *Service) ownIPs() (string, string) {
	v4, v6 := strings.TrimSpace(s.setting("publicIp")), strings.TrimSpace(s.setting("publicIp6"))
	if strings.Contains(v4, ":") {
		if v6 == "" {
			v6 = v4
		}
		v4 = ""
	}
	return v4, v6
}

// dnsRun 一次改 DNS 的明细。
type dnsRun struct {
	changes, skipped []DnsChange
	errs             []error
	errNames         map[string]bool // 出过错的域名
}

// updateDNS 把要跟着主机走的域名里、指向原主机 IP 的 A / AAAA 记录改到本机。没配令牌或域名时什么都不做(返回 nil)。
func (s *Service) updateDNS(formerIP, formerIP6 string) *dnsRun {
	tok := strings.TrimSpace(s.setting(KeyDnsToken))
	names := splitNames(s.setting(KeyDnsNames))
	if tok == "" || len(names) == 0 {
		return nil
	}
	if strings.Contains(formerIP, ":") && formerIP6 == "" {
		formerIP6, formerIP = formerIP, ""
	}
	v4, v6 := s.ownIPs()
	ctx, cancel := context.WithTimeout(context.Background(), 90*time.Second)
	defer cancel()
	cf := s.cloudflare(tok)
	run := &dnsRun{errNames: map[string]bool{}}
	for _, name := range names {
		zone, _, err := cf.ZoneFor(ctx, name)
		if err != nil {
			run.errs = append(run.errs, fmt.Errorf("%s: %w", name, err))
			run.errNames[name] = true
			continue
		}
		for _, typ := range []string{"A", "AAAA"} {
			from, to, skip := formerIP, v4, "noV4"
			if typ == "AAAA" {
				from, to, skip = formerIP6, v6, "noV6"
			}
			if v4 == "" && v6 == "" {
				skip = "noIP"
			}
			if from == "" || from == to {
				continue // 原主机没有这个地址族(没有记录指向它),或本来就是本机
			}
			recs, err := cf.ListRecords(ctx, zone, name, typ)
			if err != nil {
				run.errs = append(run.errs, fmt.Errorf("%s %s: %w", name, typ, err))
				run.errNames[name] = true
				continue
			}
			for _, r := range recs {
				if !sameIP(r.Content, from) {
					continue // 不是指向原主机的:不动
				}
				if to == "" { // 本机缺这个地址族:改不了,明确记下(这条记录还指向原主机,要手动删或改)
					run.skipped = append(run.skipped, DnsChange{Name: name, Type: typ, From: r.Content, Skip: skip})
					continue
				}
				if err := cf.UpdateContent(ctx, zone, r.ID, to); err != nil {
					run.errs = append(run.errs, fmt.Errorf("%s %s: %w", name, typ, err))
					run.errNames[name] = true
					continue
				}
				run.changes = append(run.changes, DnsChange{Name: name, Type: typ, From: r.Content, To: to})
				logger.Info("容灾:DNS ", name, " ", typ, " ", r.Content, " → ", to)
			}
		}
	}
	rec := dnsRecord{At: s.now().Unix(), Changes: append(append([]DnsChange{}, run.changes...), run.skipped...)}
	if err := errors.Join(run.errs...); err != nil {
		rec.Err = shortErr(err.Error(), 2000)
	}
	_ = s.set(map[string]string{KeyDnsChanged: mustJSON(rec)})
	return run
}

// becameDNS 刚成为主机:按设置改 DNS,改动(含跳过的)与错误写进 b,返回三态结果。
func (s *Service) becameDNS(b *Became, ip string) DnsOutcome {
	host := strings.ToLower(strings.TrimSuffix(strings.TrimSpace(b.Identity.SubHost), "."))
	run := s.updateDNS(b.FormerIP, b.FormerIP6)
	out := DnsOutcome{}
	if run != nil {
		out.Changes, out.Skipped = run.changes, run.skipped
		if err := errors.Join(run.errs...); err != nil {
			out.Err = shortErr(err.Error(), 300)
			logger.Warning("容灾:自动改 DNS 出错: ", err)
		}
		b.Dns = append(append([]DnsChange{}, run.changes...), run.skipped...)
		b.DnsErr = out.Err
	}
	needSub := host != "" && !isIPHost(host) && b.Identity.SubURLKept
	if needSub && run != nil && !run.errNames[host] {
		changed, skipped := false, false
		for _, c := range run.changes {
			changed = changed || strings.EqualFold(c.Name, host)
		}
		for _, c := range run.skipped {
			skipped = skipped || strings.EqualFold(c.Name, host)
		}
		out.SubDone = changed && !skipped
	}
	switch {
	case run == nil:
		if needSub {
			out.State = "todo"
		}
	case len(out.Changes) > 0 && len(run.errs) == 0 && len(out.Skipped) == 0:
		out.State = "done"
	case len(out.Changes) > 0:
		out.State = "partial"
	case len(run.errs) > 0 || len(out.Skipped) > 0:
		out.State = "failed"
	default: // 没找到指向原主机的记录:要管理员自己看
		out.State = "todo"
	}
	if out.State == "done" && needSub && !out.SubDone {
		out.State = "partial" // 别的名字改好了,订阅主机名还得自己改
	}
	return out
}

// revertDNS 让出主机时:本任期自动改过、当前仍指向本机的记录改回原值(别人后来改过的不动)。返回改回了哪些。
func (s *Service) revertDNS(b Became) ([]DnsChange, error) {
	tok := strings.TrimSpace(s.setting(KeyDnsToken))
	var todo []DnsChange
	for _, c := range b.Dns {
		if c.Skip == "" && c.To != "" && c.From != "" {
			todo = append(todo, c)
		}
	}
	if tok == "" || len(todo) == 0 {
		return nil, nil
	}
	ctx, cancel := context.WithTimeout(context.Background(), 60*time.Second)
	defer cancel()
	cf := s.cloudflare(tok)
	zones := map[string]string{}
	var done []DnsChange
	var errs []error
	for _, c := range todo {
		zone, ok := zones[c.Name]
		if !ok {
			z, _, err := cf.ZoneFor(ctx, c.Name)
			if err != nil {
				errs = append(errs, fmt.Errorf("%s: %w", c.Name, err))
				continue
			}
			zone, zones[c.Name] = z, z
		}
		recs, err := cf.ListRecords(ctx, zone, c.Name, c.Type)
		if err != nil {
			errs = append(errs, fmt.Errorf("%s %s: %w", c.Name, c.Type, err))
			continue
		}
		for _, r := range recs {
			if !sameIP(r.Content, c.To) {
				continue
			}
			if err := cf.UpdateContent(ctx, zone, r.ID, c.From); err != nil {
				errs = append(errs, fmt.Errorf("%s %s: %w", c.Name, c.Type, err))
				continue
			}
			done = append(done, DnsChange{Name: c.Name, Type: c.Type, From: r.Content, To: c.From})
			logger.Info("容灾:让出主机,DNS ", c.Name, " ", c.Type, " ", r.Content, " → ", c.From)
		}
	}
	err := errors.Join(errs...)
	rec := dnsRecord{At: s.now().Unix(), Changes: done, Revert: true}
	if err != nil {
		rec.Err = shortErr(err.Error(), 2000)
	}
	_ = s.set(map[string]string{KeyDnsChanged: mustJSON(rec)})
	return done, err
}

// shortErr 错误文字截到 n 个字以内(多条错误只留第一条,注明另有几项):通知、提示里不放整段网页。
func shortErr(s string, n int) string {
	lines := strings.Split(strings.TrimSpace(s), "\n")
	out := lines[0]
	if r := []rune(out); len(r) > n {
		out = string(r[:n]) + "…"
	}
	if len(lines) > 1 {
		out += fmt.Sprintf(" (+%d)", len(lines)-1)
	}
	return out
}

func sameIP(a, b string) bool {
	x, y := net.ParseIP(strings.TrimSpace(a)), net.ParseIP(strings.TrimSpace(b))
	return x != nil && y != nil && x.Equal(y)
}

// DNSTestRecord 测试时看到的一条记录。
type DNSTestRecord struct {
	Name    string `json:"name"`
	Type    string `json:"type"`
	Content string `json:"content"`
	Proxied bool   `json:"proxied"`
	Match   bool   `json:"match"` // 指向本机:主机变了会改它
}

// DNSTestResult 「测试令牌与记录」的结果(只读,不改任何记录)。
type DNSTestResult struct {
	Records []DNSTestRecord `json:"records"`
	Error   string          `json:"error,omitempty"`
}

// DNSTest 只读地查一遍:令牌能不能用、这些域名现在有哪些 A / AAAA 记录、哪些指向本机(主机变了会被改)。
// token 为空用已经存的;names 为空用已经存的。
func (s *Service) DNSTest(token, names string) (DNSTestResult, error) {
	if s.IsNode() {
		return DNSTestResult{}, errors.New("副机上不能改主机容灾:到主机面板改")
	}
	tok := strings.TrimSpace(token)
	if tok == "" {
		tok = strings.TrimSpace(s.setting(KeyDnsToken))
	}
	if tok == "" {
		return DNSTestResult{}, errors.New("先填 Cloudflare 令牌")
	}
	norm, err := normDnsNames(names)
	if err != nil {
		return DNSTestResult{}, err
	}
	list := splitNames(norm)
	if len(list) == 0 {
		list = splitNames(s.setting(KeyDnsNames))
	}
	if len(list) == 0 {
		return DNSTestResult{}, errors.New("先填要跟着主机走的域名")
	}
	v4, v6 := s.ownIPs()
	ctx, cancel := context.WithTimeout(context.Background(), 60*time.Second)
	defer cancel()
	cf := s.cloudflare(tok)
	res := DNSTestResult{Records: []DNSTestRecord{}}
	var errs []error
	for _, name := range list {
		zone, _, err := cf.ZoneFor(ctx, name)
		if err != nil {
			errs = append(errs, fmt.Errorf("%s: %w", name, err))
			continue
		}
		for _, typ := range []string{"A", "AAAA"} {
			recs, err := cf.ListRecords(ctx, zone, name, typ)
			if err != nil {
				errs = append(errs, fmt.Errorf("%s: %w", name, err))
				continue
			}
			for _, r := range recs {
				own := v4
				if typ == "AAAA" {
					own = v6
				}
				res.Records = append(res.Records, DNSTestRecord{Name: name, Type: typ, Content: r.Content, Proxied: r.Proxied, Match: own != "" && sameIP(r.Content, own)})
			}
		}
	}
	if err := errors.Join(errs...); err != nil {
		res.Error = shortErr(err.Error(), 600)
	}
	return res, nil
}

// dnsText 成为主机后 Telegram 里 DNS 的那几句:每条记录 原值 → 新值,跳过的、失败的写清要手动做什么(太多时截断)。
func (s *Service) dnsText(b Became, ip string) string {
	host := b.Identity.SubHost
	var changed, skipped []DnsChange
	for _, c := range b.Dns {
		if c.Skip != "" || c.To == "" {
			skipped = append(skipped, c)
		} else {
			changed = append(changed, c)
		}
	}
	var sb strings.Builder
	list := func(cs []DnsChange, f func(DnsChange) string) {
		for i, c := range cs {
			if i == 8 {
				fmt.Fprintf(&sb, "  …另有 %d 条\n", len(cs)-i)
				break
			}
			sb.WriteString("  " + f(c) + "\n")
		}
	}
	if len(changed) > 0 {
		sb.WriteString("DNS 已自动修改:\n")
		list(changed, func(c DnsChange) string {
			return notify.Esc(c.Name) + " " + c.Type + ":" + notify.Esc(c.From) + " → " + notify.Esc(c.To)
		})
	}
	if len(skipped) > 0 {
		sb.WriteString("这些记录还指向原主机,本机没有对应的地址,没法自动改,请手动删掉或改掉:\n")
		list(skipped, func(c DnsChange) string { return notify.Esc(c.Name) + " " + c.Type + ":" + notify.Esc(c.From) })
	}
	if b.DnsErr != "" {
		fmt.Fprintf(&sb, "DNS 自动修改失败(%s)。\n", notify.Esc(shortErr(b.DnsErr, 300)))
	}
	subChanged := false
	for _, c := range changed {
		subChanged = subChanged || strings.EqualFold(c.Name, host)
	}
	switch {
	case host == "" || isIPHost(host) || !b.Identity.SubURLKept:
	case subChanged && len(skipped) == 0 && b.DnsErr == "":
		sb.WriteString("用户的订阅地址不变。\n")
	default:
		fmt.Fprintf(&sb, "请手动把 %s 改指向本机 IP %s(没改好的那部分),用户的订阅地址就不变(改好之前用户刷新订阅会失败,已连上的线路照常可用)。\n", notify.Esc(host), notify.Esc(ip))
	}
	return sb.String()
}
