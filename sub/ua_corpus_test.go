package sub

import (
	"net/http/httptest"
	"testing"
)

// 真实客户端 UA(取自各客户端当前源码与常见实测值):按 UA 给的格式、是否老内核 Clash、是否出落地页都要对。
// 新增客户端时在这里补一行。
func TestFormatForRealUserAgents(t *testing.T) {
	cases := []struct {
		who, ua  string
		wantFmt  string // "" = 分享链接
		wantLeg  bool
		wantPage bool
	}{
		// sing-box 官方:1.14 起 UA 是 "SFA (sing-box x; ...)",不再带 "SFA/版本"
		{"SFA 1.14", "SFA (sing-box 1.14.0; language zh_CN)", "json", false, false},
		{"SFI 1.14", "SFI (sing-box 1.14.0; language zh-Hans_CN)", "json", false, false},
		{"SFW 1.14", "SFW (sing-box 1.14.0; language zh_CN)", "json", false, false},
		{"SFL 1.14", "SFL (sing-box 1.14.0; language en_US)", "json", false, false},
		{"佛跳墙", "godusevpn/0.7.15 (sing-box)", "json", false, false},
		// Clash 系
		{"Clash Verge Rev", "clash-verge/v2.5.2", "clash", false, false},
		{"FlClash", "FlClash/v0.8.96 clash-verge Platform/android", "clash", false, false},
		{"Clash Party", "mihomo.party/v1.8.9 (clash.meta)", "clash", false, false},
		{"CMFA", "ClashMetaForAndroid/2.11.33", "clash", false, false},
		{"Stash", "Stash/3.1.1 Clash/1.11.0", "clash", false, false},
		{"CFW", "ClashforWindows/0.20.39", "clash", true, false},
		// 和 CFW 一字不差的 UA 分不出内核,只能按老内核给;用户可写 ?format=mihomo 强制新内核
		{"FlClash 选了 CFW 预设 UA", "ClashforWindows/0.19.23", "clash", true, false},
		{"旧 CMFA(CFA 前缀但带 Meta)", "ClashForAndroid/2.5.12.Meta", "clash", false, false},
		// 读分享链接的多格式客户端:UA 里有 clash 字样也不能当成只吃 YAML
		{"NekoBox", "NekoBox/Android/1.4.1 (Prefer ClashMeta Format)", "", false, false},
		{"Throne 1.0.x", "Throne/1.0.12 (Prefer ClashMeta Format)", "", false, false},
		{"Throne 新版", "Throne/1.1.0", "", false, false},
		{"Hiddify", "HiddifyNext/2.5.7 (android) like ClashMeta v2ray sing-box", "", false, false},
		{"v2rayN", "v2rayN/7.13.2", "", false, false},
		{"Shadowrocket", "Shadowrocket/2070 CFNetwork/1568.100.1 Darwin/24.0.0", "", false, false},
		// 浏览器(含 iOS 上不带 Safari 字样的 App 内置浏览器)出落地页
		{"Safari iOS", "Mozilla/5.0 (iPhone; CPU iPhone OS 17_0 like Mac OS X) AppleWebKit/605.1.15 (KHTML, like Gecko) Version/17.0 Mobile/15E148 Safari/604.1", "", false, true},
		{"微信 iOS", "Mozilla/5.0 (iPhone; CPU iPhone OS 17_5 like Mac OS X) AppleWebKit/605.1.15 (KHTML, like Gecko) Mobile/15E148 MicroMessenger/8.0.50(0x1800322f) NetType/WIFI Language/zh_CN", "", false, true},
		{"QQ iOS", "Mozilla/5.0 (iPhone; CPU iPhone OS 17_5 like Mac OS X) AppleWebKit/605.1.15 (KHTML, like Gecko) Mobile/15E148 QQ/9.0.75.609 V1_IPH_SQ_9.0.75_1_APP_A Core/WKWebView NetType/WIFI", "", false, true},
		{"飞书 iOS", "Mozilla/5.0 (iPhone; CPU iPhone OS 17_5 like Mac OS X) AppleWebKit/605.1.15 (KHTML, like Gecko) Mobile/15E148 Lark/7.20.0 LarkLocale/zh_CN ChannelName/Feishu", "", false, true},
		{"微信 Android", "Mozilla/5.0 (Linux; Android 14; 23013RK75C Build/UKQ1.230804.001; wv) AppleWebKit/537.36 (KHTML, like Gecko) Version/4.0 Chrome/122.0.6261.120 Mobile Safari/537.36 MicroMessenger/8.0.49.2600(0x28003133) WeChat/arm64 Weixin NetType/WIFI", "", false, true},
	}
	for _, c := range cases {
		r := httptest.NewRequest("GET", "/sub/u", nil)
		r.Header.Set("User-Agent", c.ua)
		page := WantsPage(r)
		if page != c.wantPage {
			t.Errorf("%s:出落地页应为 %v,得 %v", c.who, c.wantPage, page)
			continue
		}
		if page {
			continue
		}
		f, leg := FormatForUA(c.ua)
		if f != c.wantFmt || leg != c.wantLeg {
			t.Errorf("%s:格式应为 %q(老内核 %v),得 %q(%v)", c.who, c.wantFmt, c.wantLeg, f, leg)
		}
	}
}

// ?format= 的写法不区分大小写、认常见别名;写明 mihomo / meta 表示新内核,不做老内核过滤。
func TestNormalizeFormat(t *testing.T) {
	cases := map[string]struct {
		f    string
		meta bool
	}{
		"clash": {"clash", false}, "Clash": {"clash", false}, "YAML": {"clash", false},
		"mihomo": {"clash", true}, "Meta": {"clash", true}, "clash.meta": {"clash", true},
		"json": {"json", false}, "JSON": {"json", false}, "sing-box": {"json", false}, "SFI": {"json", false},
		"page": {"page", false}, "": {"", false}, "base64": {"", false},
	}
	for in, want := range cases {
		f, meta := NormalizeFormat(in)
		if f != want.f || meta != want.meta {
			t.Errorf("NormalizeFormat(%q) = %q,%v,应为 %q,%v", in, f, meta, want.f, want.meta)
		}
	}
}

// 老内核 Clash 不认 SS-2022 与 none:这类 shadowsocks 线路要过滤掉,其余照发。
func TestLegacySSCipher(t *testing.T) {
	for m, want := range map[string]bool{"aes-128-gcm": true, "chacha20-ietf-poly1305": true, "2022-blake3-aes-128-gcm": false, "none": false, "": false} {
		if got := legacySSCipher(m); got != want {
			t.Errorf("legacySSCipher(%q) = %v,应为 %v", m, got, want)
		}
	}
}

func TestAutoFormatOn(t *testing.T) {
	for v, want := range map[string]bool{"": true, "true": true, "TRUE": true, "1": true, "false": false, "0": false, "off": false, "No": false} {
		if got := autoFormatOn(v); got != want {
			t.Errorf("autoFormatOn(%q) = %v,应为 %v", v, got, want)
		}
	}
}
