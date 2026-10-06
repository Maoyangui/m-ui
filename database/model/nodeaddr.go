package model

import (
	"net"
	"regexp"
	"strings"
)

// AutoIP 连接地址留空时订阅里用的探测地址,返回地址与它的地址族("v4" / "v6",都没有时为空)。
//
// pub 是 PublicIP(有 IPv4 时是 IPv4;老版本副机在纯 IPv6 机器上报的是 v6),pub6 是 PublicIP6。
// family 为 "v6" 时优先 IPv6,其余优先 IPv4;优先的那个没探测到就用另一个。
func AutoIP(pub, pub6, family string) (string, string) {
	v4, v6 := "", ""
	if ip := net.ParseIP(strings.TrimSpace(pub6)); ip != nil && ip.To4() == nil {
		v6 = ip.String()
	}
	if ip := net.ParseIP(strings.TrimSpace(pub)); ip != nil {
		if ip.To4() != nil {
			v4 = ip.To4().String()
		} else if v6 == "" {
			v6 = ip.String()
		}
	}
	if v6 != "" && (family == "v6" || v4 == "") {
		return v6, "v6"
	}
	if v4 != "" {
		return v4, "v4"
	}
	return "", ""
}

// BareHost 去掉 IPv6 地址外面的方括号([2001:db8::1] → 2001:db8::1);别的写法原样返回。
func BareHost(s string) string {
	s = strings.TrimSpace(s)
	if b := strings.TrimSuffix(strings.TrimPrefix(s, "["), "]"); b != s && net.ParseIP(b) != nil {
		return b
	}
	return s
}

// NormAddrFamily 规整地址族选项:只认 "v6",其余(含 "v4"、空)都是默认的 IPv4。
func NormAddrFamily(s string) string {
	if strings.EqualFold(strings.TrimSpace(s), "v6") {
		return "v6"
	}
	return ""
}

// 域名每段字母数字(含中文等各国文字,也收 punycode 与下划线)和连字符,连字符不打头不结尾
var hostnameRE = regexp.MustCompile(`^[\p{L}\p{N}_]([\p{L}\p{N}_-]*[\p{L}\p{N}_])?(\.[\p{L}\p{N}_]([\p{L}\p{N}_-]*[\p{L}\p{N}_])?)*\.?$`)

// ValidAddr 连接地址只能是 IP 或域名:不带端口、协议头、路径和空格(这些会拼进分享链接把地址弄坏)。
func ValidAddr(s string) bool {
	if net.ParseIP(s) != nil {
		return true
	}
	return len(s) <= 253 && hostnameRE.MatchString(s)
}
