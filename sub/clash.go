package sub

import (
	"encoding/json"
	"fmt"

	"github.com/Maoyangui/m-ui/database/model"
	"github.com/Maoyangui/m-ui/render"

	"gopkg.in/yaml.v3"
)

// basicClashConfig 是 clash/mihomo 订阅的骨架(dns + 规则),与常见默认一致。
// 用户如在设置里提供自定义模板则整体替换(设置项 subClashExt)。
const basicClashConfig = `mixed-port: 7890
allow-lan: false
mode: rule
log-level: info
external-controller: 127.0.0.1:9090
dns:
  enable: true
  ipv6: false
  enhanced-mode: fake-ip
  fake-ip-range: 198.18.0.1/16
  nameserver:
    - https://doh.pub/dns-query
    - https://1.0.0.1/dns-query
rules:
  - GEOIP,Private,DIRECT
  - MATCH,Proxy
`

// BuildClash 生成一个用户的 clash 订阅 YAML(不含外部节点)。
func BuildClash(user model.User, lines []model.Line, entries []Entry, template, notice string) (string, error) {
	return BuildClashFull(user, lines, entries, template, notice, nil)
}

// BuildClashFull 生成一个用户的 clash 订阅 YAML,外部节点追加在本站节点之后并加入 Proxy/Auto 组。
// notice 为可选的顶部提示节点名(如流量提示);为空则不注入。
func BuildClashFull(user model.User, lines []model.Line, entries []Entry, template, notice string, external []ExtItem) (string, error) {
	base := template
	if base == "" {
		base = basicClashConfig
	}
	var root map[string]interface{}
	if err := yaml.Unmarshal([]byte(base), &root); err != nil {
		return "", err
	}
	if root == nil {
		root = map[string]interface{}{}
	}

	var proxies []interface{}
	var proxyNames []string // 全部代理(Auto 组)
	var topNames []string   // Proxy 选择组里的条目:单入口=代理本身;多入口=每条线路一个 url-test 组
	var lineGroups []interface{}
	if notice != "" {
		proxies = append(proxies, noticeClashProxy(notice))
	}
	for _, line := range lines {
		var names []string
		for _, a := range resolveAddrs(line, entries) {
			for _, p := range lineToClashProxies(line, user, a) {
				if a.insecure {
					applyInsecure(p)
				}
				proxies = append(proxies, p)
				proxyNames = append(proxyNames, p["name"].(string))
				names = append(names, p["name"].(string))
			}
		}
		if len(names) > 1 && line.Protocol != "mixed" {
			// 多入口(如 香港/台湾):同一线路做成 url-test 组,客户端按延迟自动选入口
			lineGroups = append(lineGroups, map[string]interface{}{
				"name": line.Name, "type": "url-test", "url": "http://www.gstatic.com/generate_204",
				"interval": 300, "tolerance": 50, "proxies": names,
			})
			topNames = append(topNames, line.Name)
		} else {
			topNames = append(topNames, names...)
		}
	}
	// 外部节点:名字去重后追加
	used := map[string]bool{}
	for _, n := range proxyNames {
		used[n] = true
	}
	for _, e := range external {
		for _, p := range e.Clash {
			name, _ := p["name"].(string)
			if name == "" {
				continue
			}
			for i := 2; used[name]; i++ {
				name = fmt.Sprintf("%s %d", p["name"], i)
			}
			used[name] = true
			q := make(map[string]interface{}, len(p))
			for k, v := range p {
				q[k] = v
			}
			q["name"] = name
			proxies = append(proxies, q)
			proxyNames = append(proxyNames, name)
			topNames = append(topNames, name)
		}
	}
	root["proxies"] = proxies

	selectList := []string{}
	if notice != "" {
		selectList = append(selectList, notice)
	}
	selectList = append(selectList, "Auto")
	selectList = append(selectList, topNames...)
	groups := []interface{}{
		map[string]interface{}{"name": "Proxy", "type": "select", "proxies": selectList},
		map[string]interface{}{"name": "Auto", "type": "url-test", "url": "http://www.gstatic.com/generate_204", "interval": 300, "tolerance": 50, "proxies": proxyNames},
	}
	groups = append(groups, lineGroups...)
	root["proxy-groups"] = groups
	out, err := yaml.Marshal(root)
	if err != nil {
		return "", err
	}
	return string(out), nil
}

// lineToClashProxies 把一条线路的一个地址渲染成 mihomo 代理(mixed 出 socks5 + http 两个)。
func lineToClashProxies(line model.Line, user model.User, a addr) []map[string]interface{} {
	name := line.Name + a.remark
	base := func() map[string]interface{} {
		return map[string]interface{}{"name": name, "server": a.server, "port": a.port}
	}
	tlsConf := render.ParseTLS(line)
	fp := tlsConf.Fingerprint

	// applyTLS 按线路 TLS 模式填充 mihomo 字段;sniKey 为 sni 或 servername(vmess/vless 用后者)
	applyTLS := func(p map[string]interface{}, sniKey string) {
		switch tlsConf.Mode {
		case "cert":
			p["tls"] = true
			if a.sni != "" {
				p[sniKey] = a.sni
			}
			if fp != "" {
				p["client-fingerprint"] = fp
			}
		case "reality":
			p["tls"] = true
			p[sniKey] = tlsConf.Reality.HandshakeServer
			if fp == "" {
				fp = "chrome"
			}
			p["client-fingerprint"] = fp
			opts := map[string]interface{}{"public-key": tlsConf.Reality.PublicKey}
			if len(tlsConf.Reality.ShortIDs) > 0 {
				opts["short-id"] = tlsConf.Reality.ShortIDs[0]
			}
			p["reality-opts"] = opts
		}
	}
	applyTransport := func(p map[string]interface{}) {
		tr := transportOf(line)
		switch typ, _ := tr["type"].(string); typ {
		case "ws", "httpupgrade":
			p["network"] = "ws"
			opts := map[string]interface{}{}
			if path, _ := tr["path"].(string); path != "" {
				opts["path"] = path
			}
			if h := wsHost(tr); h != "" {
				opts["headers"] = map[string]interface{}{"Host": h}
			}
			if typ == "httpupgrade" {
				opts["v2ray-http-upgrade"] = true
			}
			p["ws-opts"] = opts
		case "grpc":
			p["network"] = "grpc"
			s, _ := tr["service_name"].(string)
			p["grpc-opts"] = map[string]interface{}{"grpc-service-name": s}
		case "http":
			path, _ := tr["path"].(string)
			hosts := stringList(tr["host"])
			if tlsConf.Mode != "none" {
				p["network"] = "h2"
				p["h2-opts"] = map[string]interface{}{"path": path, "host": hosts}
			} else {
				p["network"] = "http"
				p["http-opts"] = map[string]interface{}{"path": []string{path}, "headers": map[string]interface{}{"Host": hosts}}
			}
		}
	}

	switch line.Protocol {
	case "hysteria2":
		p := base()
		p["type"] = "hysteria2"
		p["password"], _ = userCred(user, "hysteria2")["password"].(string)
		if a.sni != "" {
			p["sni"] = a.sni
		}
		p["alpn"] = []string{"h3"}
		var opts map[string]interface{}
		_ = json.Unmarshal(line.Options, &opts)
		if obfs, ok := opts["obfs"].(map[string]interface{}); ok {
			p["obfs"], _ = obfs["type"].(string)
			p["obfs-password"], _ = obfs["password"].(string)
		}
		if ph := portHopping(line); ph != "" {
			p["ports"] = ph // mihomo 端口跳跃
		}
		return []map[string]interface{}{p}
	case "anytls":
		p := base()
		p["type"] = "anytls"
		p["password"], _ = userCred(user, "anytls")["password"].(string)
		if a.sni != "" {
			p["sni"] = a.sni
		}
		return []map[string]interface{}{p}
	case "shadowsocks":
		var opts map[string]interface{}
		_ = json.Unmarshal(line.Options, &opts)
		method, _ := opts["method"].(string)
		p := base()
		p["type"] = "ss"
		p["cipher"] = method
		p["password"] = ssClientPassword(line, user)
		p["udp"] = true
		return []map[string]interface{}{p}
	case "tuic":
		c := userCred(user, "tuic")
		var opts map[string]interface{}
		_ = json.Unmarshal(line.Options, &opts)
		cc, _ := opts["congestion_control"].(string)
		if cc == "" {
			cc = "cubic"
		}
		p := base()
		p["type"] = "tuic"
		p["uuid"], _ = c["uuid"].(string)
		p["password"], _ = c["password"].(string)
		if a.sni != "" {
			p["sni"] = a.sni
		}
		p["alpn"] = []string{"h3"}
		p["congestion-controller"] = cc
		p["udp-relay-mode"] = "native"
		return []map[string]interface{}{p}
	case "trojan":
		p := base()
		p["type"] = "trojan"
		p["password"], _ = userCred(user, "trojan")["password"].(string)
		p["udp"] = true
		applyTLS(p, "sni")
		applyTransport(p)
		return []map[string]interface{}{p}
	case "vless":
		p := base()
		p["type"] = "vless"
		p["uuid"], _ = userCred(user, "vless")["uuid"].(string)
		p["udp"] = true
		p["packet-encoding"] = "xudp"
		if render.VisionEnabled(line) {
			p["flow"] = "xtls-rprx-vision"
		}
		applyTLS(p, "servername")
		applyTransport(p)
		return []map[string]interface{}{p}
	case "vmess":
		p := base()
		p["type"] = "vmess"
		p["uuid"], _ = userCred(user, "vmess")["uuid"].(string)
		p["alterId"] = 0
		p["cipher"] = "auto"
		p["udp"] = true
		applyTLS(p, "servername")
		applyTransport(p)
		return []map[string]interface{}{p}
	case "socks", "http", "mixed":
		var out []map[string]interface{}
		c := userCred(user, "socks")
		if line.Protocol != "http" {
			p := base()
			if line.Protocol == "mixed" {
				p["name"] = name + "-socks"
			}
			p["type"] = "socks5"
			p["username"], _ = c["username"].(string)
			p["password"], _ = c["password"].(string)
			p["udp"] = true
			out = append(out, p)
		}
		if line.Protocol != "socks" {
			hc := userCred(user, "http")
			p := base()
			if line.Protocol == "mixed" {
				p["name"] = name + "-http"
			}
			p["type"] = "http"
			p["username"], _ = hc["username"].(string)
			p["password"], _ = hc["password"].(string)
			if tlsConf.Mode == "cert" {
				p["tls"] = true
				if a.sni != "" {
					p["sni"] = a.sni
				}
			}
			out = append(out, p)
		}
		return out
	}
	return nil
}

// applyInsecure 给使用 TLS 的代理加 skip-cert-verify(服务端自签证书时必需)。
func applyInsecure(p map[string]interface{}) {
	if _, reality := p["reality-opts"]; reality {
		return // Reality 用公钥校验,与证书无关
	}
	switch p["type"] {
	case "hysteria2", "anytls", "tuic":
		p["skip-cert-verify"] = true
	case "trojan", "vless", "vmess", "http":
		if tls, _ := p["tls"].(bool); tls {
			p["skip-cert-verify"] = true
		}
	}
}

// noticeClashProxy 生成一个不可用的占位代理作为顶部提示(流量/到期信息写在名字里)。
func noticeClashProxy(name string) map[string]interface{} {
	return map[string]interface{}{
		"name": name, "type": "ss", "server": "127.0.0.1", "port": 1, "cipher": "aes-128-gcm", "password": "dummy",
	}
}
