package sub

import "encoding/json"

// 订阅模板的现成写法(设置页「订阅格式与模板」里一键填入)。
//
// 留空时用的内置模板不变(全部走代理);这里另给几套主流的分流写法,照着社区里通行的配置整理:
//   - 绕过大陆:大陆的域名和 IP 直连,被屏蔽的、境外的走代理(俗称「白名单」);
//   - 只代理被屏蔽的:只有被屏蔽的网站和 Telegram 走代理,其余直连(俗称「黑名单」/ GFW 列表模式)。
//
// 写法上的取舍:
//   - DNS 用普通的国内公共解析(223.5.5.5、119.29.29.29),不强求 DoH:fake-ip 下走代理的域名本来就不在本地解析,
//     本地解析只用来判断「是不是大陆 IP」,被污染的结果是境外地址,照样判成走代理;想用 DoH 的,模板里留了可替换的两行。
//   - 规则集用 MetaCubeX/meta-rules-dat(mihomo 的 mrs、sing-box 的 srs 两种格式同源),经代理下载、每天更新:
//     jsDelivr 和 GitHub 在大陆直连不稳,跟着代理走才下得到。
//   - Clash 的「精简版」只用各种内核都认的规则(GEOIP,CN 等),不下载规则集,老内核也能用;「规则集版」要 mihomo 内核。
//
// 每一套都在真的客户端内核上跑过(mihomo、sing-box 加载面板生成的订阅,逐个目标看走直连还是代理,见 tests/rs/t70-templates.mjs);
// presets_test.go 守着结构:能解析、规则指到的组和规则集都在、内置那两套和 Default*Template 一字不差。

// Preset 一套现成模板。ID 给前端取名字和介绍(web/assets/js/i18n.js 的 set.tmpl.<ID>),Kind 是 clash / singbox。
type Preset struct {
	ID   string `json:"id"`
	Kind string `json:"kind"`
	Body string `json:"body"`
}

// Presets 全部现成模板,按给人看的顺序:每种格式内置的排第一。
func Presets() []Preset {
	return []Preset{
		{"clash-builtin", "clash", DefaultClashTemplate()},
		{"clash-cn-lite", "clash", clashCNLite},
		{"clash-cn-ruleset", "clash", clashCNRuleSet},
		{"clash-gfw", "clash", clashGFW},
		{"singbox-builtin", "singbox", DefaultSingBoxTemplate()},
		{"singbox-cn", "singbox", singBoxJSON(singBoxCN())},
		{"singbox-gfw", "singbox", singBoxJSON(singBoxGFW())},
	}
}

// clashHead Clash 三套分流模板共用的开头:端口、fake-ip 和国内 DNS。
const clashHead = `mixed-port: 7890
allow-lan: false
mode: rule
log-level: info
external-controller: 127.0.0.1:9090
dns:
  enable: true
  ipv6: false
  enhanced-mode: fake-ip
  fake-ip-range: 198.18.0.1/16
  fake-ip-filter:
    - "*.lan"
    - "*.local"
    - "+.msftconnecttest.com"
    - "+.msftncsi.com"
    - "time.*.com"
    - "ntp.*.com"
    - "+.pool.ntp.org"
  default-nameserver:
    - 223.5.5.5
    - 119.29.29.29
  # 普通 DNS(国内两家公共解析)。想用加密的 DoH,把下面两行换成:
  #   - https://doh.pub/dns-query
  #   - https://dns.alidns.com/dns-query
  nameserver:
    - 223.5.5.5
    - 119.29.29.29
`

const clashCNLite = `# 绕过大陆 · 精简版
# 大陆的网站和 IP 直连,其余走代理。不用在线规则集,各种 Clash 内核都能用。
# 节点和 Proxy、Auto 两个策略组由面板自动填进来,这里不用写。
` + clashHead + `rules:
  - DOMAIN-SUFFIX,local,DIRECT
  - IP-CIDR,127.0.0.0/8,DIRECT,no-resolve
  - IP-CIDR,10.0.0.0/8,DIRECT,no-resolve
  - IP-CIDR,172.16.0.0/12,DIRECT,no-resolve
  - IP-CIDR,192.168.0.0/16,DIRECT,no-resolve
  - IP-CIDR,100.64.0.0/10,DIRECT,no-resolve
  - IP-CIDR,169.254.0.0/16,DIRECT,no-resolve
  - IP-CIDR6,::1/128,DIRECT,no-resolve
  - IP-CIDR6,fc00::/7,DIRECT,no-resolve
  - IP-CIDR6,fe80::/10,DIRECT,no-resolve
  - DOMAIN-SUFFIX,cn,DIRECT
  - GEOIP,CN,DIRECT
  - MATCH,Proxy
`

// mrs mihomo 规则集的地址(MetaCubeX/meta-rules-dat 的 meta 分支,经 jsDelivr)。
const mrs = "https://cdn.jsdelivr.net/gh/MetaCubeX/meta-rules-dat@meta/geo/"

const clashCNRuleSet = `# 绕过大陆 · 规则集版(要 mihomo 内核:Clash Verge Rev、FlClash、Clash Meta for Android、Mihomo Party 等)
# 大陆的域名和 IP 直连;被屏蔽的、境外的域名直接交给代理,不在本地解析。
# 规则集来自 MetaCubeX/meta-rules-dat,经代理下载,每天更新一次。
# 节点和 Proxy、Auto 两个策略组由面板自动填进来,这里不用写。
` + clashHead + `rule-providers:
  private: {type: http, behavior: domain, format: mrs, interval: 86400, proxy: Proxy, url: "` + mrs + `geosite/private.mrs"}
  gfw: {type: http, behavior: domain, format: mrs, interval: 86400, proxy: Proxy, url: "` + mrs + `geosite/gfw.mrs"}
  cn: {type: http, behavior: domain, format: mrs, interval: 86400, proxy: Proxy, url: "` + mrs + `geosite/cn.mrs"}
  global: {type: http, behavior: domain, format: mrs, interval: 86400, proxy: Proxy, url: "` + mrs + `geosite/geolocation-!cn.mrs"}
  private-ip: {type: http, behavior: ipcidr, format: mrs, interval: 86400, proxy: Proxy, url: "` + mrs + `geoip/private.mrs"}
  cn-ip: {type: http, behavior: ipcidr, format: mrs, interval: 86400, proxy: Proxy, url: "` + mrs + `geoip/cn.mrs"}
rules:
  - RULE-SET,private,DIRECT
  - RULE-SET,private-ip,DIRECT,no-resolve
  - RULE-SET,gfw,Proxy
  - RULE-SET,cn,DIRECT
  - RULE-SET,global,Proxy
  - RULE-SET,cn-ip,DIRECT
  - MATCH,Proxy
`

const clashGFW = `# 只代理被屏蔽的 · 规则集版(要 mihomo 内核:Clash Verge Rev、FlClash、Clash Meta for Android、Mihomo Party 等)
# 只有被屏蔽的网站和 Telegram 走代理,其余全部直连:省代理流量,没被屏蔽的网站国内外都用本地网络。
# 规则集来自 MetaCubeX/meta-rules-dat,经代理下载,每天更新一次。
# 节点和 Proxy、Auto 两个策略组由面板自动填进来,这里不用写。
` + clashHead + `rule-providers:
  private: {type: http, behavior: domain, format: mrs, interval: 86400, proxy: Proxy, url: "` + mrs + `geosite/private.mrs"}
  gfw: {type: http, behavior: domain, format: mrs, interval: 86400, proxy: Proxy, url: "` + mrs + `geosite/gfw.mrs"}
  private-ip: {type: http, behavior: ipcidr, format: mrs, interval: 86400, proxy: Proxy, url: "` + mrs + `geoip/private.mrs"}
  telegram-ip: {type: http, behavior: ipcidr, format: mrs, interval: 86400, proxy: Proxy, url: "` + mrs + `geoip/telegram.mrs"}
rules:
  - RULE-SET,private,DIRECT
  - RULE-SET,private-ip,DIRECT,no-resolve
  - RULE-SET,gfw,Proxy
  - RULE-SET,telegram-ip,Proxy,no-resolve
  - MATCH,DIRECT
`

// srs sing-box 规则集的地址(同一个仓库的 sing 分支)。
const srs = "https://cdn.jsdelivr.net/gh/MetaCubeX/meta-rules-dat@sing/geo/"

func singBoxJSON(cfg map[string]interface{}) string {
	b, _ := json.MarshalIndent(cfg, "", "  ")
	return string(b)
}

// remoteRuleSet 一个经代理下载的远程规则集。
func remoteRuleSet(tag, path string) map[string]interface{} {
	return map[string]interface{}{"type": "remote", "tag": tag, "format": "binary", "url": srs + path, "download_detour": "proxy"}
}

// singBoxSplitDNS 分流模板的 DNS:境外的经代理问 8.8.8.8(走 TCP,哪种节点都带得动),国内的问 223.5.5.5;
// rules 是哪些域名用哪一个,final 是剩下的。
func singBoxSplitDNS(rule map[string]interface{}, final string) map[string]interface{} {
	return map[string]interface{}{
		"servers": []map[string]interface{}{
			{"type": "tcp", "tag": "remote", "server": "8.8.8.8", "detour": "proxy"},
			{"type": "udp", "tag": "cn", "server": "223.5.5.5"},
			{"type": "local", "tag": "local"},
		},
		"rules": []map[string]interface{}{
			{"clash_mode": "Direct", "server": "cn"},
			{"clash_mode": "Global", "server": "remote"},
			rule,
		},
		"final":    final,
		"strategy": "prefer_ipv4",
	}
}

// singBoxCN 绕过大陆:大陆的域名和 IP 直连,被屏蔽的、其余的走代理。入站、实验项和内置的一样。
func singBoxCN() map[string]interface{} {
	cfg := singBoxBase()
	cfg["dns"] = singBoxSplitDNS(map[string]interface{}{"rule_set": "geosite-cn", "server": "cn"}, "remote")
	route := cfg["route"].(map[string]interface{})
	route["rules"] = append(route["rules"].([]map[string]interface{}),
		map[string]interface{}{"rule_set": "geosite-gfw", "outbound": "proxy"},
		map[string]interface{}{"rule_set": []string{"geosite-cn", "geoip-cn"}, "outbound": "direct"},
	)
	route["rule_set"] = []map[string]interface{}{
		remoteRuleSet("geosite-gfw", "geosite/gfw.srs"),
		remoteRuleSet("geosite-cn", "geosite/cn.srs"),
		remoteRuleSet("geoip-cn", "geoip/cn.srs"),
	}
	return cfg
}

// singBoxGFW 只代理被屏蔽的:被屏蔽的域名和 Telegram 的 IP 走代理,其余直连。
func singBoxGFW() map[string]interface{} {
	cfg := singBoxBase()
	cfg["dns"] = singBoxSplitDNS(map[string]interface{}{"rule_set": "geosite-gfw", "server": "remote"}, "cn")
	route := cfg["route"].(map[string]interface{})
	route["rules"] = append(route["rules"].([]map[string]interface{}),
		map[string]interface{}{"rule_set": []string{"geosite-gfw", "geoip-telegram"}, "outbound": "proxy"},
	)
	route["rule_set"] = []map[string]interface{}{
		remoteRuleSet("geosite-gfw", "geosite/gfw.srs"),
		remoteRuleSet("geoip-telegram", "geoip/telegram.srs"),
	}
	route["final"] = "direct"
	return cfg
}
