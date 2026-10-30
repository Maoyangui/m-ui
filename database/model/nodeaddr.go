package model

import (
	"encoding/json"
	"errors"
	"net"
	"regexp"
	"strconv"
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

// FamilyIPs 这台服务器按地址族分开的地址(线路单独选 IP 版本时用):探测到的两个地址,手填的连接地址是哪一族就顶替哪一族。
func FamilyIPs(addr, pub, pub6 string) (v4, v6 string) {
	if ip, fam := AutoIP(pub, pub6, ""); fam == "v4" {
		v4 = ip
	}
	if ip, fam := AutoIP(pub, pub6, "v6"); fam == "v6" {
		v6 = ip
	}
	if ip := net.ParseIP(BareHost(addr)); ip != nil {
		if ip4 := ip.To4(); ip4 != nil {
			v4 = ip4.String()
		} else {
			v6 = ip.String()
		}
	}
	return v4, v6
}

// ParseNodeFamily 解出线路的按服务器 IP 版本(Line.NodeFamily):服务器 id → v4 | v6;没选的不在里面。
func ParseNodeFamily(raw json.RawMessage) map[uint]string {
	if len(raw) == 0 {
		return nil
	}
	var m map[string]string
	if json.Unmarshal(raw, &m) != nil {
		return nil
	}
	out := make(map[uint]string, len(m))
	for k, v := range m {
		if id, err := strconv.ParseUint(k, 10, 32); err == nil && (v == "v4" || v == "v6") {
			out[uint(id)] = v
		}
	}
	return out
}

// NormNodeFamily 保存前规整:只留 {"服务器 id":"v4"|"v6"},空值(跟随服务器)去掉,一条都没有就是 nil。
func NormNodeFamily(raw json.RawMessage) (json.RawMessage, error) {
	if len(raw) == 0 || string(raw) == "null" {
		return nil, nil
	}
	var m map[string]string
	if err := json.Unmarshal(raw, &m); err != nil {
		return nil, errors.New("线路的 IP 版本设置格式不对")
	}
	out := make(map[string]string, len(m))
	for k, v := range m {
		if _, err := strconv.ParseUint(k, 10, 32); err != nil {
			return nil, errors.New("线路的 IP 版本设置格式不对")
		}
		switch v {
		case "":
		case "v4", "v6":
			out[k] = v
		default:
			return nil, errors.New("线路的 IP 版本只能是 v4 或 v6")
		}
	}
	if len(out) == 0 {
		return nil, nil
	}
	return json.Marshal(out)
}

// URLHost 拼 URL 的主机部分:协议的默认端口(https 443、http 80)不写,IPv6 加方括号。
// 只有和协议对得上才能省:http 跑在 443 上省掉的话,浏览器会去连 80。
func URLHost(scheme, host string, port int) string {
	if (scheme == "https" && port == 443) || (scheme == "http" && port == 80) {
		if strings.Contains(host, ":") {
			return "[" + host + "]"
		}
		return host
	}
	return net.JoinHostPort(host, strconv.Itoa(port))
}

// IPKey 按来源计数时用的键:IPv4 就是地址本身;IPv6 取 /64。一台设备同时有好几个 IPv6 地址、临时地址还会换,
// 同一个局域网的设备也都在同一个 /64 里,这样才和 IPv4 下"同一 WiFi 的多台算一个"一致,限流也不会被换地址绕过。
func IPKey(ip string) string {
	p := net.ParseIP(ip)
	if p == nil {
		return ip
	}
	if p4 := p.To4(); p4 != nil {
		return p4.String()
	}
	return p.Mask(net.CIDRMask(64, 128)).String() + "/64"
}

// DeviceCount 一组来源地址算几台设备(见 IPKey)。
func DeviceCount(ips []string) int {
	set := make(map[string]bool, len(ips))
	for _, ip := range ips {
		set[IPKey(ip)] = true
	}
	return len(set)
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
