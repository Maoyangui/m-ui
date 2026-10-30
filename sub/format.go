package sub

import "strings"

// 订阅地址不带 ?format= 时按 User-Agent 给格式:把通用地址直接粘进 Clash Verge、SFA 这类只认一种格式的客户端也能用。
// 写了 ?format= 的一律听它的(落地页的一键导入、老用户手工加的地址都带着)。

// linkUATokens 这些客户端读分享链接,节点也最全。UA 里常带着别家的名字(NekoBox 写 "Prefer ClashMeta Format",
// Hiddify、Karing 写 "like ClashMeta"),所以排在 clash 前面判断。
var linkUATokens = []string{
	"hiddify", "karing", "nekobox", "nekoray", "v2rayn", "v2rayng", "v2ray", "xray", "shadowrocket", "quantumult", "loon",
	"surge", "streisand", "v2box", "husi", "sagernet", "matsuri", "pharos", "kitsunebi", "nextin", "happ/",
}

// clashUATokens 只吃 Clash YAML 的客户端(Clash Verge / Mihomo Party / FlClash / Stash / Clash Meta for Android 等)。
var clashUATokens = []string{"clash", "mihomo", "stash"}

// legacyClashUATokens 还在用老内核的 Clash 客户端:配置里只要有一个不认识的协议(hysteria2 / tuic / anytls / vless),
// 整份都导入失败,所以这些协议不发给它们(ClashX Meta、Clash Meta for Android 带 meta,不算)。
var legacyClashUATokens = []string{"clashforwindows", "clash for windows", "clashforandroid", "clash for android", "clashx/", "clashx pro"}

// singBoxUATokens sing-box 官方客户端(SFA / SFI / SFM / SFT)只认 sing-box JSON。
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
		return "clash", containsAny(ua, legacyClashUATokens)
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
