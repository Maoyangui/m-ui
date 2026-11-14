package ha

import (
	"strings"
	"sync"
	"time"

	"github.com/Maoyangui/m-ui/dnsapi"
	"github.com/Maoyangui/m-ui/logger"
	"github.com/Maoyangui/m-ui/notify"
)

// 「连订阅一起接管」靠的 DNS 接口定期自检:令牌被删、权限被改、域名挪到了别的账号,要在真接任之前就知道,
// 不能等主机挂了、备用接任时才发现改不了解析、签不了证书。每 6 小时用存着的凭据只读地查一遍(和「测试 DNS 接口与记录」
// 同一个查法);失效时就绪检查标红、Telegram 告一次,恢复了再说一声。订阅域名没有指向本机 IP 的 A / AAAA 记录
// (开了 CDN 代理、用的 CNAME)时接任也改不了它,就绪检查提醒。

const dnsHealthEvery = 6 * time.Hour

type dnsHealthState struct {
	mu      sync.Mutex
	at      time.Time
	key     string // 查的是哪套凭据(服务商 + 凭据 + 名单):换了就重查
	err     string // 最近一次失败的原因(成功为空)
	noMatch bool   // 订阅域名没有指向本机 IP 的记录
	alerted bool
}

var dnsHealths sync.Map // *Service → *dnsHealthState

func (s *Service) dnsHealthState() *dnsHealthState {
	v, _ := dnsHealths.LoadOrStore(s, &dnsHealthState{})
	return v.(*dnsHealthState)
}

// dnsHealthTick 后台循环调用(每 10 分钟):到了时间、或凭据换过就查一遍。
func (s *Service) dnsHealthTick() {
	c := s.Config()
	h := s.dnsHealthState()
	if s.IsNode() || c.Mode == ModeOff || !c.TakeSub || !c.DnsTokenSet {
		h.mu.Lock()
		h.at, h.key, h.err, h.noMatch, h.alerted = time.Time{}, "", "", false, false
		h.mu.Unlock()
		return
	}
	key := c.DnsProvider + "\x00" + s.setting(KeyDnsToken) + "\x00" + c.DnsNames
	h.mu.Lock()
	fresh := h.key == key && !h.at.IsZero() && s.now().Sub(h.at) < dnsHealthEvery
	h.mu.Unlock()
	if fresh {
		return
	}
	res, err := s.DNSTest("", "", "")
	msg := ""
	if err != nil {
		msg = err.Error()
	} else if res.Error != "" {
		msg = res.Error
	}
	noMatch := false
	if msg == "" {
		host := strings.ToLower(strings.TrimSpace(s.subHost()))
		noMatch = host != "" && !isIPHost(host)
		for _, r := range res.Records {
			if strings.EqualFold(r.Name, host) && r.Match {
				noMatch = false
			}
		}
	}
	h.mu.Lock()
	wasAlerted := h.alerted
	h.at, h.key, h.err, h.noMatch = s.now(), key, shortErr(msg, 300), noMatch
	alert := msg != "" && !h.alerted
	recovered := msg == "" && h.alerted
	if alert {
		h.alerted = true
	}
	if recovered {
		h.alerted = false
	}
	h.mu.Unlock()
	label := dnsapi.Label(c.DnsProvider)
	switch {
	case alert:
		logger.Warning("容灾:DNS 接口自检失败(", label, "):", msg)
		s.notify("🔴 <b>主机容灾的 DNS 接口用不了了</b>(" + notify.Esc(label) + "):" + notify.Esc(shortErr(msg, 300)) +
			"\n选了连订阅一起接管,接任时要靠它改解析、签订阅证书:到主机面板的主机容灾设置里重新填凭据,点「测试 DNS 接口与记录」核对")
	case recovered && wasAlerted:
		logger.Info("容灾:DNS 接口自检恢复(", label, ")")
		s.notify("🟢 <b>主机容灾的 DNS 接口恢复了</b>(" + notify.Esc(label) + ")")
	}
}

// dnsHealthView 最近一次自检的结果(没查过:ok = false)。
func (s *Service) dnsHealthView() (checked bool, err string, noMatch bool) {
	h := s.dnsHealthState()
	h.mu.Lock()
	defer h.mu.Unlock()
	return !h.at.IsZero(), h.err, h.noMatch
}
