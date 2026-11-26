package render

import (
	"errors"
	"fmt"
	"strings"
	"sync"
	"time"

	"github.com/Maoyangui/m-ui/database/model"
	"github.com/Maoyangui/m-ui/logger"
)

// 线路的「协议 × 加密方式」哪些配得上。
//
// 以前三种加密方式(证书 / Reality / 不加密)对哪个协议都能选、都存得下,其中好几种根本用不了。73 个组合在实机上逐个建线路、
// 取订阅、真连了一遍(tests/rs/t60-combos.mjs),配不上的分三类:
//
//  1. hysteria2 / tuic 配 Reality —— 入站起不来。这两个协议走 QUIC,sing-box 的 Reality 只做了 TCP 上的 TLS
//     (common/tls/reality_server.go 的 STDConfig 直接返回 "unsupported usage for reality")。保存前的干跑只构造、不启动,
//     查不出来;存下之后数据面带着它重启就失败:运行中的回滚到上一份(这台服务器上所有人断一次线,之后改别的线路也不再生效),
//     进程重启后没有上一份可回,整台服务器一条线路都起不来。
//  2. anytls / vmess / http 配 Reality —— 入站起得来,订阅给出去的节点连不上:这几个协议的分享链接没有 Reality 的写法,
//     链接、由链接转出来的 sing-box 配置都是按普通 TLS 写的。
//  3. shadowsocks / socks / mixed 配 TLS —— 这三个协议不带 TLS,订阅里也没有地方写。
//
// 所以:选的时候只给配得上的(web/assets/js/pages/lines.js 的 PROTOCOLS,web/combo_test.go 核对两边一致),
// 保存、启用时拦(CheckCombo),库里已经有的第 1 类渲染时跳过(Unstartable)。
//
// trojan 配「不加密」不在此列:订阅里的 Trojan 节点一律按 TLS 连,直连连不上,但前面另有 CDN / 反向代理替它做 TLS 的
// 用法是通的(界面上「不加密」本来就写着只用于 CDN 回源或内网),只在表单里提示。
var tlsModes = map[string][]string{
	"hysteria2":   {"cert"},
	"tuic":        {"cert"},
	"anytls":      {"cert"},
	"trojan":      {"cert", "reality", "none"},
	"vless":       {"reality", "cert", "none"},
	"vmess":       {"none", "cert"},
	"http":        {"cert", "none"},
	"shadowsocks": {"none"},
	"socks":       {"none"},
	"mixed":       {"none"},
}

// protoLabel 报错里写的协议名(和界面上的一致)。
var protoLabel = map[string]string{
	"hysteria2": "Hysteria2", "tuic": "TUIC", "anytls": "AnyTLS", "trojan": "Trojan", "vless": "VLESS", "vmess": "VMess",
	"http": "HTTP", "shadowsocks": "Shadowsocks", "socks": "SOCKS5", "mixed": "Mixed",
}

// TLSModes 这个协议能选的加密方式(cert / reality / none),第一个是默认的;不认识的协议为空。
func TLSModes(protocol string) []string {
	return append([]string(nil), tlsModes[protocol]...)
}

func tlsModeAllowed(protocol, mode string) bool {
	for _, m := range tlsModes[protocol] {
		if m == mode {
			return true
		}
	}
	return false
}

// CheckCombo 线路的协议和加密方式配不上时返回原因(保存、启用线路时拦下);配得上返回 nil。
// 不认识的协议、不认识的加密方式不归这里管(各有各的报错)。
func CheckCombo(line model.Line) error {
	modes, ok := tlsModes[line.Protocol]
	if !ok {
		return nil
	}
	mode := ParseTLS(line).Mode
	if tlsModeAllowed(line.Protocol, mode) {
		return nil
	}
	name := protoLabel[line.Protocol]
	switch mode {
	case "reality":
		if UDPOnly(line.Protocol) {
			return fmt.Errorf("%s 配不了 Reality:它走 QUIC(UDP),Reality 只能用在走 TCP 的协议上,这样的线路起不来。加密方式请选「证书」;想用 Reality 就把协议换成 VLESS 或 Trojan", name)
		}
		if len(modes) == 1 && modes[0] == "none" {
			return fmt.Errorf("%s 不带 TLS,加密方式只能是「不加密」", name)
		}
		if tlsModeAllowed(line.Protocol, "none") { // 两句分开写:英文界面按整句翻译(web/assets/js/errmsg.js)
			return fmt.Errorf("%s 配不了 Reality:这个协议的分享链接没有 Reality 的写法,订阅给出去的节点连不上。加密方式请选「证书」或「不加密」;想用 Reality 就把协议换成 VLESS 或 Trojan", name)
		}
		return fmt.Errorf("%s 配不了 Reality:这个协议的分享链接没有 Reality 的写法,订阅给出去的节点连不上。加密方式请选「证书」;想用 Reality 就把协议换成 VLESS 或 Trojan", name)
	case "cert":
		return fmt.Errorf("%s 不带 TLS,加密方式只能是「不加密」", name)
	case "none":
		return fmt.Errorf("%s 必须启用 TLS", line.Protocol)
	}
	return nil
}

// Unstartable 这条线路的入站注定起不来时返回原因(现在只有一种:走 QUIC 的协议配了 Reality),否则是空串。
//
// 这种线路以前存得下,老库里可能留着;副机也可能从还没升级的主机那里收到。带着它渲染,整份配置启动失败,
// 连累这台服务器上别的线路(见上面第 1 类)。所以渲染时当它没部署在本机:不出入站、不出规则,别的线路照常;
// 线路页把它标成「配不上」,改掉加密方式或协议就恢复。
func Unstartable(line model.Line) string {
	if UDPOnly(line.Protocol) && ParseTLS(line).Mode == "reality" {
		return protoLabel[line.Protocol] + " 走 QUIC,配不了 Reality"
	}
	return ""
}

var (
	unstartableMu     sync.Mutex
	unstartableWarned = map[string]time.Time{}
)

// warnUnstartable 渲染时跳过了一条起不来的线路:记一行日志。配置渲染得很勤(每次保存、每次同步),同一条线路一小时只记一次。
func warnUnstartable(name, why string) {
	now := time.Now()
	unstartableMu.Lock()
	if now.Sub(unstartableWarned[name]) < time.Hour {
		unstartableMu.Unlock()
		return
	}
	if len(unstartableWarned) > 1000 {
		unstartableWarned = map[string]time.Time{}
	}
	unstartableWarned[name] = now
	unstartableMu.Unlock()
	// 拼成一段再给:logger 会在各个参数之间加空格,「 名字 」那样不好看
	logger.Warning("线路「" + name + "」没有启动(" + why + "):到线路页把它的加密方式改成证书,或者把协议换成 VLESS / Trojan。别的线路不受影响")
}

// SSMethodProblem shadowsocks 的加密算法在面板上用不了时返回原因(保存时拦下),能用返回 nil。
// 面板的入站一律按多用户渲染(每个用户一份密码),下面两种在多用户下 sing-box 起不来,保存前的干跑会拒掉,
// 但报的是 "invalid argument" / "unsupported method: none",看不出该怎么办。别的算法照旧交给干跑。
func SSMethodProblem(method string) error {
	switch strings.ToLower(strings.TrimSpace(method)) {
	case "2022-blake3-chacha20-poly1305":
		return errors.New("shadowsocks 的 2022-blake3-chacha20-poly1305 不支持多个用户共用一条线路,面板用不了。换成 2022-blake3-aes-128-gcm 或 2022-blake3-aes-256-gcm")
	case "none", "plain":
		return errors.New("shadowsocks 不能选不加密(none),换一个加密算法")
	}
	return nil
}
