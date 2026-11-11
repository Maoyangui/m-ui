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
// 令牌单独存(haDnsToken,不复用签证书的 acmeCfToken),不进日志、不经设置接口回显。

// DnsChange 自动改的一条 DNS 记录。
type DnsChange struct {
	Name string `json:"name"`
	Type string `json:"type"`
	From string `json:"from"`
	To   string `json:"to"`
}

// dnsRecord 记下来的改动(本机设置 haDnsChanged)。
type dnsRecord struct {
	At      int64       `json:"at"`
	Changes []DnsChange `json:"changes,omitempty"`
	Err     string      `json:"err,omitempty"`
}

func (s *Service) cloudflare(token string) *acme.Cloudflare {
	return &acme.Cloudflare{Token: token, BaseURL: s.d.DNSAPI}
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

// updateDNS 把要跟着主机走的域名里、指向原主机 IP 的 A / AAAA 记录改到本机。没配令牌或域名时什么都不做。
func (s *Service) updateDNS(formerIP, formerIP6 string) ([]DnsChange, error) {
	tok := strings.TrimSpace(s.setting(KeyDnsToken))
	names := splitNames(s.setting(KeyDnsNames))
	if tok == "" || len(names) == 0 {
		return nil, nil
	}
	if strings.Contains(formerIP, ":") && formerIP6 == "" {
		formerIP6, formerIP = formerIP, ""
	}
	v4, v6 := s.ownIPs()
	ctx, cancel := context.WithTimeout(context.Background(), 90*time.Second)
	defer cancel()
	cf := s.cloudflare(tok)
	var changes []DnsChange
	var errs []error
	for _, name := range names {
		zone, _, err := cf.ZoneFor(ctx, name)
		if err != nil {
			errs = append(errs, fmt.Errorf("%s: %w", name, err))
			continue
		}
		for _, typ := range []string{"A", "AAAA"} {
			from, to := formerIP, v4
			if typ == "AAAA" {
				from, to = formerIP6, v6
			}
			if from == "" || to == "" || from == to {
				continue
			}
			recs, err := cf.ListRecords(ctx, zone, name, typ)
			if err != nil {
				errs = append(errs, fmt.Errorf("%s: %w", name, err))
				continue
			}
			for _, r := range recs {
				if !sameIP(r.Content, from) {
					continue // 不是指向原主机的:不动
				}
				if err := cf.UpdateContent(ctx, zone, r.ID, to); err != nil {
					errs = append(errs, fmt.Errorf("%s %s: %w", name, typ, err))
					continue
				}
				changes = append(changes, DnsChange{Name: name, Type: typ, From: r.Content, To: to})
				logger.Info("容灾:DNS ", name, " ", typ, " ", r.Content, " → ", to)
			}
		}
	}
	err := errors.Join(errs...)
	rec := dnsRecord{At: s.now().Unix(), Changes: changes}
	if err != nil {
		rec.Err = err.Error()
	}
	_ = s.set(map[string]string{KeyDnsChanged: mustJSON(rec)})
	return changes, err
}

// DnsOutcome 一次自动改 DNS 的结果(契约 A0)。
type DnsOutcome struct {
	State            string // done | partial | failed | todo | ""
	SubDone          bool   // 订阅主机名本身的匹配记录全部改好
	Changes, Skipped []DnsChange
	Err              string
}

// becameDNS 刚成为主机:改 DNS,结果写进 b(契约 A0:包装 updateDNS)。
func (s *Service) becameDNS(b *Became, ip string) DnsOutcome {
	changes, err := s.updateDNS(b.FormerIP, b.FormerIP6)
	b.Dns = changes
	out := DnsOutcome{Changes: changes, SubDone: len(changes) > 0}
	if err != nil {
		b.DnsErr = err.Error()
		out.Err = b.DnsErr
		logger.Warning("容灾:自动改 DNS 失败: ", err)
	}
	return out
}

// revertDNS 让出主机时把本任期改过的 DNS 改回去(契约 A0 的桩)。
func (s *Service) revertDNS(b Became) ([]DnsChange, error) { return nil, nil }

// SetDNSAPI 换掉 Cloudflare 接口地址(测试用)。
func (s *Service) SetDNSAPI(u string) { s.d.DNSAPI = u }

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
		res.Error = err.Error()
	}
	return res, nil
}

// dnsText 订阅地址要不要改 DNS 的那一句。
func (s *Service) dnsText(b Became, ip string) string {
	host := b.Identity.SubHost
	switch {
	case host == "" || isIPHost(host) || !b.Identity.SubURLKept:
		return ""
	case len(b.Dns) > 0:
		var names []string
		for _, c := range b.Dns {
			names = append(names, c.Name)
		}
		return fmt.Sprintf("DNS:已自动把 %s 改指向本机(%s),用户的订阅地址不变。\n", notify.Esc(strings.Join(names, "、")), notify.Esc(ip))
	case b.DnsErr != "":
		return fmt.Sprintf("DNS:自动修改失败(%s)。请手动把 %s 改指向本机 IP %s,用户的订阅地址不变。\n", notify.Esc(b.DnsErr), notify.Esc(host), notify.Esc(ip))
	}
	return fmt.Sprintf("DNS:把 %s 改指向本机 IP %s,用户的订阅地址就不变(改好之前用户刷新订阅会失败,已连上的线路照常可用)。\n", notify.Esc(host), notify.Esc(ip))
}
