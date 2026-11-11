package sub

import (
	"encoding/json"
	"fmt"
	"strings"

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
	return BuildClashFor(user, lines, entries, template, notice, external, nil)
}

// BuildClashFor 同 BuildClashFull,但按 lineNodes(线路 → 允许的服务器,缺省 = 全部)过滤入口。
func BuildClashFor(user model.User, lines []model.Line, entries []Entry, template, notice string, external []ExtItem, lineNodes map[uint]map[uint]bool) (string, error) {
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

	// mihomo 里代理、策略组和内置的 DIRECT / REJECT 等共用一张名字表,重名整份配置导入失败。
	// 模板里自己写的节点留着,排在最前;本站节点、外部节点给模板的节点与策略组、内置名、生成的 Proxy / Auto、提示节点让名
	var proxies []interface{}
	used := map[string]bool{"Proxy": true, "Auto": true}
	for _, n := range clashBuiltins {
		used[n] = true
	}
	if own, ok := root["proxies"].([]interface{}); ok {
		for _, p := range own {
			if m, ok := p.(map[string]interface{}); ok {
				if n, _ := m["name"].(string); n != "" {
					used[n] = true
				}
			}
			proxies = append(proxies, p)
		}
	}
	tmplGroups := map[string][]string{} // 模板里写的策略组 → 它列的成员
	if own, ok := root["proxy-groups"].([]interface{}); ok {
		for _, g := range own {
			m, _ := g.(map[string]interface{})
			if n, _ := m["name"].(string); n != "" {
				used[n], tmplGroups[n] = true, stringList(m["proxies"])
			}
		}
	}
	var proxyNames []string // 全部代理(Auto 组)
	var topNames []string   // Proxy 选择组里的条目:单入口=代理本身;多入口=每条线路一个 url-test 组
	if notice != "" {
		used[notice] = true
		proxies = append(proxies, noticeClashProxy(notice))
	}
	var pending []lineGroup
	var groupAt []int // 线路组在 topNames 里的位置:组名等本站节点都定完名再起,先占个位
	for _, line := range lines {
		var names []string
		for _, a := range resolveAddrs(line, entries, lineNodes[line.Id]) {
			for _, p := range lineToClashProxies(line, user, a) {
				if a.insecure {
					applyInsecure(p)
				}
				name, _ := p["name"].(string)
				for i := 2; used[name]; i++ {
					name = fmt.Sprintf("%s %d", p["name"], i)
				}
				used[name] = true
				p["name"] = name
				proxies = append(proxies, p)
				proxyNames = append(proxyNames, name)
				names = append(names, name)
			}
		}
		if wantsLineGroup(line, len(names)) {
			// 多入口(如 香港/台湾):同一线路做成 url-test 组,客户端按延迟自动选入口
			groupAt = append(groupAt, len(topNames))
			topNames = append(topNames, "")
			pending = append(pending, lineGroup{line.Name, names})
		} else {
			topNames = append(topNames, names...)
		}
	}
	// 线路组在本站节点之后、外部节点之前起名,规则和 sing-box JSON 一样(见 linegroup.go)。
	// 模板里同名的策略组照旧顶替生成的(mergeClashGroups),线路组就叫这个名字;只是它顺着成员走得回 Proxy 时
	// (生成的 Proxy 要列它,成环,mihomo 整份拒收)不顶替,线路组改名另起。模板自己写了 Proxy 时生成的那个不出,不会成环
	providers := map[string]bool{} // 列了成员的策略组在 mihomo 里还占一个同名 provider,不能和模板的 proxy-providers 重名
	if pp, ok := root["proxy-providers"].(map[string]interface{}); ok {
		for n := range pp {
			providers[n] = true
		}
	}
	_, ownProxy := tmplGroups["Proxy"]
	groupNames := nameLineGroups(pending, used, providers, func(name string) bool {
		_, ok := tmplGroups[name]
		return ok && (ownProxy || !reaches(name, "Proxy", func(n string) []string { return tmplGroups[n] }))
	})
	var lineGroups []interface{}
	for i, g := range pending {
		topNames[groupAt[i]] = groupNames[i]
		lineGroups = append(lineGroups, map[string]interface{}{
			"name": groupNames[i], "type": "url-test", "url": "http://www.gstatic.com/generate_204",
			"interval": 300, "tolerance": 50, "proxies": g.members,
		})
	}
	// 外部节点:名字去重后追加(撞上线路组名也是外部节点改名)
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
	// 有写成 IPv6 地址的节点就得打开 ipv6 总开关:老内核默认关着,关着时 mihomo 连不上这些节点。
	// 模板里明写了 ipv6: false 也改成 true(以前只在模板没写时才补,自定义模板照抄内置骨架写了 false 的,
	// v6 节点全连不上);设置页保存这种模板时会提示(见 ClashTemplateDisablesIPv6)
	if hasIPv6Server(proxies) {
		root["ipv6"] = true
	}

	selectList := []string{}
	if notice != "" {
		selectList = append(selectList, notice)
	}
	if len(proxyNames) == 0 { // 一个节点都没有(线路全被过滤掉):空分组会让整份配置导入失败
		proxyNames = []string{"DIRECT"}
	}
	selectList = append(selectList, "Auto")
	selectList = append(selectList, topNames...)
	groups := []interface{}{
		map[string]interface{}{"name": "Proxy", "type": "select", "proxies": selectList},
		map[string]interface{}{"name": "Auto", "type": "url-test", "url": "http://www.gstatic.com/generate_204", "interval": 300, "tolerance": 50, "proxies": proxyNames},
	}
	groups = append(groups, lineGroups...)
	root["proxy-groups"] = mergeClashGroups(groups, root["proxy-groups"])
	if _, ok := root["rules"]; !ok { // 模板没写规则:Clash 没规则时全部直连,等于没走代理
		root["rules"] = []string{"GEOIP,Private,DIRECT", "MATCH,Proxy"}
	}
	out, err := yaml.Marshal(root)
	if err != nil {
		return "", err
	}
	return string(out), nil
}

// ClashTemplateDisablesIPv6 Clash 模板顶层写了 ipv6: false:订阅里有 IPv6 节点时生成的配置会把它改成 true,
// 设置页保存这种模板时据此提示一句。
func ClashTemplateDisablesIPv6(tmpl string) bool {
	if strings.TrimSpace(tmpl) == "" {
		return false
	}
	var root map[string]interface{}
	if yaml.Unmarshal([]byte(tmpl), &root) != nil {
		return false
	}
	v, ok := root["ipv6"].(bool)
	return ok && !v
}

// hasIPv6Server 节点里有没有写成 IPv6 地址的。
func hasIPv6Server(proxies []interface{}) bool {
	for _, p := range proxies {
		if m, ok := p.(map[string]interface{}); ok {
			if s, _ := m["server"].(string); strings.Contains(s, ":") {
				return true
			}
		}
	}
	return false
}

// mergeClashGroups 模板里写的策略组和生成的合起来:同名的用模板的(放在生成的那个位置),其余的接在后面。
func mergeClashGroups(generated []interface{}, tmpl interface{}) []interface{} {
	own, _ := tmpl.([]interface{})
	if len(own) == 0 {
		return generated
	}
	nameOf := func(g interface{}) string {
		m, _ := g.(map[string]interface{})
		n, _ := m["name"].(string)
		return n
	}
	byName := make(map[string]interface{}, len(own))
	for _, g := range own {
		byName[nameOf(g)] = g
	}
	out := make([]interface{}, 0, len(generated)+len(own))
	taken := map[string]bool{}
	for _, g := range generated {
		n := nameOf(g)
		if t, ok := byName[n]; ok {
			g = t
		}
		taken[n] = true
		out = append(out, g)
	}
	for _, g := range own {
		if !taken[nameOf(g)] {
			out = append(out, g)
		}
	}
	return out
}

// legacyClashProtocols 老内核 Clash 认识的线路协议。
var legacyClashProtocols = map[string]bool{"shadowsocks": true, "vmess": true, "trojan": true, "socks": true, "http": true, "mixed": true}

// legacyClashTypes 同上,外部节点按 clash 的 type 字段判断。
var legacyClashTypes = map[string]bool{"ss": true, "vmess": true, "trojan": true, "socks5": true, "http": true}

// legacySSCipher 老内核(go-shadowsocks2)认识的加密方式:SS-2022(2022-blake3-*)与 none 都不认,一条就让整份配置报错。
func legacySSCipher(m string) bool {
	m = strings.ToLower(strings.TrimSpace(m))
	return m != "" && m != "none" && !strings.HasPrefix(m, "2022-")
}

// legacyClashOnly 只留老内核 Clash 认识的线路与外部节点。
func legacyClashOnly(lines []model.Line, external []ExtItem) ([]model.Line, []ExtItem) {
	keep := make([]model.Line, 0, len(lines))
	for _, l := range lines {
		if !legacyClashProtocols[l.Protocol] {
			continue
		}
		if l.Protocol == "shadowsocks" {
			var o struct {
				Method string `json:"method"`
			}
			_ = json.Unmarshal(l.Options, &o)
			if !legacySSCipher(o.Method) {
				continue
			}
		}
		keep = append(keep, l)
	}
	ext := make([]ExtItem, 0, len(external))
	for _, e := range external {
		c := make([]map[string]interface{}, 0, len(e.Clash))
		for _, p := range e.Clash {
			t, _ := p["type"].(string)
			if !legacyClashTypes[t] {
				continue
			}
			if cipher, _ := p["cipher"].(string); t == "ss" && !legacySSCipher(cipher) {
				continue
			}
			c = append(c, p)
		}
		ext = append(ext, ExtItem{Name: e.Name, Links: e.Links, Clash: c})
	}
	return keep, ext
}

// DefaultClashTemplate 内置的 Clash 骨架,面板上「填入内置模板」用它。
func DefaultClashTemplate() string { return basicClashConfig }

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
			p["username"] = user.Name // 和数据面一样用当前用户名(见 link.go 的 socksURI)
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
			p["username"] = user.Name
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
