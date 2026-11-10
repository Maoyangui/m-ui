package sub

import "strings"

// 订阅地址不带 ?format= 时按 User-Agent 给格式:把通用地址直接粘进 Clash Verge、SFA 这类只认一种格式的客户端也能用。
// 写了 ?format= 的一律听它的(落地页的一键导入、老用户手工加的地址都带着)。

// linkUATokens 这些客户端读分享链接,节点也最全。UA 里常带着别家的名字(NekoBox 写 "Prefer ClashMeta Format",
// Hiddify、Karing 写 "like ClashMeta"),所以排在 clash 前面判断。
var linkUATokens = []string{
	"hiddify", "karing", "nekobox", "nekoray", "throne", "v2rayn", "v2rayng", "v2ray", "xray", "shadowrocket", "quantumult", "loon",
	"surge", "streisand", "v2box", "husi", "sagernet", "matsuri", "pharos", "kitsunebi", "nextin", "happ/",
	// 只是声明"也吃 ClashMeta"的多格式客户端(NekoRay 一系的 "(Prefer ClashMeta Format)"、Hiddify 一系的 "like ClashMeta"):
	// 名字不在上面时也按链接给,别因为 UA 里有 clash 字样就当成只吃 YAML 的客户端
	"prefer clashmeta", "like clashmeta",
}

// clashUATokens 只吃 Clash YAML 的客户端(Clash Verge / Mihomo Party / FlClash / Stash / Clash Meta for Android 等)。
var clashUATokens = []string{"clash", "mihomo", "stash"}

// legacyClashUATokens 还在用老内核的 Clash 客户端:配置里只要有一个不认识的协议(hysteria2 / tuic / anytls / vless),
// 整份都导入失败,所以这些协议不发给它们(ClashX Meta、Clash Meta for Android 带 meta,不算)。
var legacyClashUATokens = []string{"clashforwindows", "clash for windows", "clashforandroid", "clash for android", "clashx/", "clashx pro"}

// metaUATokens 带这些字样的一定是 mihomo 内核,哪怕前缀像老客户端。
var metaUATokens = []string{"meta", "mihomo"}

// singBoxUATokens sing-box 官方客户端只认 sing-box JSON。1.14 起 UA 是 "SFA (sing-box 1.14.0; ...)"(不再带 "/版本"),
// Windows / Linux 版是 SFW / SFL,靠 "sing-box" 认;老版本的 "SFA/1.x" 仍留着。
var singBoxUATokens = []string{"sing-box", "singbox", "sfa/", "sfi/", "sfm/", "sft/"}

// FormatForUA 返回该客户端该拿的格式("" = 分享链接)与是否老内核 Clash。认不出的一律给分享链接,和以前一样。
func FormatForUA(ua string) (format string, legacyClash bool) {
	ua = strings.ToLower(ua)
	if containsAny(ua, linkUATokens) {
		return "", false
	}
	if containsAny(ua, singBoxUATokens) {
		return "json", false
	}
	if containsAny(ua, clashUATokens) {
		return "clash", LegacyClashUA(ua)
	}
	return "", false
}

// LegacyClashUA 是不是老内核 Clash。不管格式是按 UA 选的还是 ?format=clash 写明的,都要按它过滤协议。
func LegacyClashUA(ua string) bool {
	ua = strings.ToLower(ua)
	return containsAny(ua, legacyClashUATokens) && !containsAny(ua, metaUATokens)
}

// NormalizeFormat 把 ?format= 的写法收成 clash / json / page / ""(分享链接)。大小写、常见别名都认;
// forceMeta 为真表示写明了 mihomo / meta:用户自己说了是新内核,不做老内核过滤(UA 冒充 CFW 的 mihomo 客户端用)。
func NormalizeFormat(v string) (format string, forceMeta bool) {
	switch strings.ToLower(strings.TrimSpace(v)) {
	case "clash", "yaml", "clash-yaml":
		return "clash", false
	case "mihomo", "meta", "clash.meta", "clashmeta", "clash-meta":
		return "clash", true
	case "json", "sing-box", "singbox", "sfa", "sfi", "sfm", "sft", "sfw", "sfl":
		return "json", false
	case "page", "html":
		return "page", false
	}
	return "", false
}

func containsAny(s string, tokens []string) bool {
	for _, t := range tokens {
		if strings.Contains(s, t) {
			return true
		}
	}
	return false
}
