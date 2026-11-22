package sub

import (
	"crypto/sha256"
	"encoding/json"
	"fmt"
	"strings"
	"sync"

	"github.com/Maoyangui/m-ui/database/model"
	"github.com/Maoyangui/m-ui/logger"
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
// notice 为可选的顶部提示节点名(如流量提示);为空则不注入。默认生成 Proxy/Auto(见 BuildClashFor)。
func BuildClashFull(user model.User, lines []model.Line, entries []Entry, template, notice string, external []ExtItem) (string, error) {
	return BuildClashFor(user, lines, entries, template, notice, external, nil, true)
}

// BuildClashFor 同 BuildClashFull,但按 lineNodes(线路 → 允许的服务器,缺省 = 全部)过滤入口。
// genDefaultGroups 为 false 时不再强制生成 Proxy/Auto(设置 subClashDefaultGroups,见 defaultGroups)。
func BuildClashFor(user model.User, lines []model.Line, entries []Entry, template, notice string, external []ExtItem, lineNodes map[uint]map[uint]bool, genDefaultGroups bool, hides ...LineHides) (string, error) {
	hide := hidesOf(hides)
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
	// 本站节点、外部节点也不叫 GLOBAL(撞了改「GLOBAL 2」):模板没写 GLOBAL 组时 mihomo 自己补一个全局组,
	// 运行中名字表里的 GLOBAL 就是那个组,叫 GLOBAL 的节点被顶掉 —— 客户端里显示成组、测速测到组上(mihomo 1.19.34 实测);
	// 模板写了 GLOBAL 组时同名节点整份拒收,那种下面登记模板组名时也会占上。组名让开 GLOBAL 另有 lineGroupReserved
	used["GLOBAL"] = true
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
		for _, a := range subAddrs(line, entries, lineNodes[line.Id], hide[line.Id]) {
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
	tmplGroup := func(name string) bool { // 模板里叫 name 的组能不能直接拿来用:是个组,且不会顺着成员绕回生成的 Proxy
		_, ok := tmplGroups[name]
		return ok && (ownProxy || !reaches(name, "Proxy", func(n string) []string { return tmplGroups[n] }))
	}
	groupNames := nameLineGroups(pending, used, providers, tmplGroup)
	var lineGroups []interface{}
	for i, g := range pending {
		topNames[groupAt[i]] = groupNames[i]
		lineGroups = append(lineGroups, map[string]interface{}{
			"name": groupNames[i], "type": "url-test", "url": "http://www.gstatic.com/generate_204",
			"interval": 300, "tolerance": 50, "proxies": g.members,
		})
	}
	reserveLineNames(lines, used) // 没出组的线路名也不让给外部节点(见 linegroup.go)
	// 外部节点:名字去重后追加(撞上线路组名、线路名也是外部节点改名),和以前一字不差;
	// 外部订阅各成一个策略组,组名等全部节点定完名再起,撞名时改的是组名(见文件末尾「外部订阅 → 策略组」)
	proxies, extNames, subGroups := appendExternal(proxies, external, used)
	proxyNames = append(proxyNames, extNames...)
	nameExtGroups(subGroups, used, providers, extGroupClaimed(lines, groupNames), tmplGroup)
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
	// 外部订阅的组排在本站之后、外部节点之前:在 Proxy 里选「机场A」,再到「机场A」组里挑节点(组不进 Proxy 的话,
	// 规则模式下在组里怎么选都不起作用)。外部节点照旧平铺在后面:升级前在 Proxy 里直接选过外部节点的客户端,选择不会丢
	for _, g := range subGroups {
		selectList = append(selectList, g.Name)
	}
	selectList = append(selectList, extNames...)
	// 开关(subClashDefaultGroups,默认开)关着时不再强制生成 Proxy/Auto:模板作者已经
	// 自己写了策略组与规则,生成的两个组只会多余(有些客户端的全局模式下 hidden 也藏不住组,
	// 模板里加 hidden 治标不治本)。但模板还引用着它们时,被引用的那个照旧生成并记一条警告,
	// 免得订阅悬空、客户端整份拒收;Proxy 的选单里列着 Auto,生成 Proxy 时 Auto 也得跟着。
	genProxy, genAuto, keepLine := defaultGroups(genDefaultGroups, base, root, tmplGroups, proxies, groupNames)
	groups := []interface{}{} // 空 slice:一个组都不生成时订阅里写 proxy-groups: [],而不是 null
	if genProxy {
		groups = append(groups, map[string]interface{}{"name": "Proxy", "type": "select", "proxies": selectList})
	}
	if genAuto {
		groups = append(groups, map[string]interface{}{"name": "Auto", "type": "url-test", "url": "http://www.gstatic.com/generate_204", "interval": 300, "tolerance": 50, "proxies": proxyNames})
	}
	for i, g := range lineGroups { // 线路测速组:开关关着时只留被引用的(见 defaultGroups)
		if keepLine[i] {
			groups = append(groups, g)
		}
	}
	merged := mergeClashGroups(groups, root["proxy-groups"])
	root["proxy-groups"] = orderClashGroups(merged, subGroups)
	if _, ok := root["rules"]; !ok { // 模板没写规则:Clash 没规则时全部直连,等于没走代理
		root["rules"] = []string{"GEOIP,Private,DIRECT", "MATCH,Proxy"}
	}
	out, err := yaml.Marshal(root)
	if err != nil {
		return "", err
	}
	return string(out), nil
}

// defaultGroups 决定生成哪些默认策略组(设置 subClashDefaultGroups)。开关开着时 Proxy、Auto 和各线路测速组都生成;
// 关着时只生成模板里被引用到、且模板自己没定义的:Proxy 的选单里列着 Auto 和各线路组,生成 Proxy 时它们也得跟着。
// 关着却还得生成 Proxy / Auto 的,记一条警告(同一份模板、同样的结果只记一次,免得每次拉订阅都刷一条)。
// keepLine 与 lineNames(线路测速组的名字,和生成顺序一致)一一对应。
func defaultGroups(gen bool, tmpl string, root map[string]interface{}, tmplGroups map[string][]string, proxies []interface{}, lineNames []string) (proxy, auto bool, keepLine []bool) {
	keepLine = make([]bool, len(lineNames))
	if gen {
		for i := range keepLine {
			keepLine[i] = true
		}
		return true, true, keepLine
	}
	refs := clashRefNames(root, proxies)
	proxy, auto = missingDefaults(refs, tmplGroups)
	for i, n := range lineNames {
		keepLine[i] = proxy || refs[n]
	}
	var names []string
	if proxy {
		names = append(names, "Proxy")
	}
	if auto {
		names = append(names, "Auto")
	}
	if len(names) > 0 && firstClashGroupsWarn(tmpl, names) {
		logger.Warningf("Clash 订阅:默认 Proxy/Auto 策略组已关闭,但模板仍引用 %s,已生成被引用的组以免订阅悬空;要彻底去掉,把模板里用到它的地方(规则、策略组、DNS、节点的 dialer-proxy 等)换成自己的组", strings.Join(names, "、"))
	}
	return proxy, auto, keepLine
}

// missingDefaults 关着开关时还得生成的 Proxy / Auto:被引用、模板自己又没定义的;
// 生成的 Proxy 的选单里列着 Auto,生成 Proxy 时 Auto 也得跟着。
func missingDefaults(refs map[string]bool, tmplGroups map[string][]string) (proxy, auto bool) {
	_, hasProxy := tmplGroups["Proxy"]
	_, hasAuto := tmplGroups["Auto"]
	proxy = refs["Proxy"] && !hasProxy
	return proxy, (refs["Auto"] || proxy) && !hasAuto
}

// clashWarned 关着默认策略组却还得生成时已经记过警告的(模板 + 结果的摘要):同一份模板只记一次。
// 模板只有管理员改设置时才变,这张表长不大。
var clashWarned sync.Map

// firstClashGroupsWarn 这份模板、这个结果是不是头一回遇到(是才记警告)。
func firstClashGroupsWarn(tmpl string, names []string) bool {
	_, seen := clashWarned.LoadOrStore(sha256.Sum256([]byte(tmpl+"\x00"+strings.Join(names, ","))), true)
	return !seen
}

// clashRefNames 模板(连同已经并进来的节点)里用到的名字。mihomo 里能写策略组名的地方很多:规则(含 sub-rules)、
// 策略组的成员、DNS 服务器的 #出站、入站 listeners 与 tunnels 的 proxy、proxy-providers / rule-providers 的 proxy
// 与 override.dialer-proxy……不逐个认字段,把模板里每个字符串值都拿来比:整串、按逗号切开的每段(规则的目标在
// 逗号后)、# 后面按 & 切开的每段(DNS 服务器的出站)。误伤只会多生成,不会悬空。
// 节点只看 dialer-proxy(密码、地址之类不算)。模板没写 rules 键时后面会补默认规则 MATCH,Proxy,按引用了 Proxy 算。
func clashRefNames(root map[string]interface{}, proxies []interface{}) map[string]bool {
	refs := map[string]bool{}
	add := func(s string) {
		if s = strings.TrimSpace(s); s != "" {
			refs[s] = true
		}
	}
	var walk func(v interface{})
	walk = func(v interface{}) {
		switch x := v.(type) {
		case string:
			add(x)
			for _, f := range strings.Split(x, ",") {
				add(f)
			}
			if i := strings.Index(x, "#"); i >= 0 {
				for _, f := range strings.Split(x[i+1:], "&") {
					add(f)
				}
			}
		case []interface{}:
			for _, e := range x {
				walk(e)
			}
		case map[string]interface{}:
			for _, e := range x {
				walk(e)
			}
		case map[interface{}]interface{}:
			for _, e := range x {
				walk(e)
			}
		}
	}
	for k, v := range root {
		if k != "proxies" {
			walk(v)
		}
	}
	if _, ok := root["rules"]; !ok {
		refs["Proxy"] = true // 没写规则:后面补默认规则 MATCH,Proxy
	}
	for _, p := range proxies {
		m, _ := p.(map[string]interface{})
		if d, _ := m["dialer-proxy"].(string); d != "" {
			refs[d] = true
		}
	}
	return refs
}

// ClashTemplateNeedsDefaultGroups 关掉「自动生成默认策略组」时,这份模板(空 = 内置)还引用着、自己又没定义的 Proxy / Auto:
// 订阅里仍会生成它们,设置页保存时据此提示一句。模板不是合法 YAML 时返回 nil(保存时另有校验)。
// 外部节点的 dialer-proxy 这时还不知道,只看模板本身。
func ClashTemplateNeedsDefaultGroups(tmpl string) []string {
	base := tmpl
	if strings.TrimSpace(base) == "" {
		base = basicClashConfig
	}
	var root map[string]interface{}
	if err := yaml.Unmarshal([]byte(base), &root); err != nil {
		return nil
	}
	if root == nil {
		root = map[string]interface{}{}
	}
	tmplGroups := map[string][]string{}
	if own, ok := root["proxy-groups"].([]interface{}); ok {
		for _, g := range own {
			m, _ := g.(map[string]interface{})
			if n, _ := m["name"].(string); n != "" {
				tmplGroups[n] = stringList(m["proxies"])
			}
		}
	}
	own, _ := root["proxies"].([]interface{})
	proxy, auto := missingDefaults(clashRefNames(root, own), tmplGroups)
	var out []string
	if proxy {
		out = append(out, "Proxy")
	}
	if auto {
		out = append(out, "Auto")
	}
	return out
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
		ext = append(ext, ExtItem{Name: e.Name, Links: e.Links, Clash: c, NoGroup: e.NoGroup})
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
		if ph := portHopping(line); ph != "" && !a.relay {
			p["ports"] = ph // mihomo 端口跳跃(中转入口只转一个端口,不带)
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

// ---------- 外部订阅 → 策略组 ----------
// 每个外部订阅一个 select 组(组名 = 订阅名,成员 = 这个订阅输出的节点),列在 Proxy 里:外部订阅多了,几十上百个节点平铺在
// Proxy 里不好找,改成在 Proxy 里选「机场A」、再到「机场A」组里挑节点。不成组的:分享链接(只有一个节点)、用户的外部节点
// 分配上关了「单独成组」的(两种都是 ExtItem.NoGroup,见 sub/server.go externalFor)、没有节点的(空组让整份配置导入失败)。
// 组名的规则和线路组一样(见 linegroup.go 开头),只是排在最后,见 nameExtGroups。

// extGroup 一个外部订阅的策略组:Members 是这个订阅里节点的最终名字;own 为真表示模板里写了同名的组,用模板的,不另生成。
type extGroup struct {
	Name    string
	Members []string
	own     bool
}

// appendExternal 追加外部节点:名字和以前一样按出现顺序去重(撞上前面任何名字就改「X 2」),分组不改任何一个节点的名字。
// 节点 map 先复制再改名,不污染多个用户共用的 ExtItem。返回追加后的 proxies、全部外部节点的名字(依次,进 Auto 和 Proxy)、
// 要成组的订阅(组名还没起,见 nameExtGroups)。
func appendExternal(proxies []interface{}, external []ExtItem, used map[string]bool) ([]interface{}, []string, []extGroup) {
	var names []string
	var groups []extGroup
	for _, e := range external {
		var members []string
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
			names = append(names, name)
			members = append(members, name)
		}
		if n := strings.TrimSpace(e.Name); !e.NoGroup && n != "" && len(members) > 0 {
			groups = append(groups, extGroup{Name: n, Members: members})
		}
	}
	return proxies, names, groups
}

// nameExtGroups 给订阅组起名。本站节点、线路组、线路名、外部节点到这时都已定名,订阅组撞上谁都是组加序号(「机场A 2」):
// 已有的节点名一个不变 —— 分组是新加的,不能因为它把客户端里选过的节点悄悄改名(#90 原先是节点让组、节点改名)。
// 模板里写了同名的组(以前想按订阅分组只能这样手写)就用模板的,不另生成一份,Proxy 里列的就是它(和线路组、Proxy / Auto
// 一样「写了用模板的」);它会顺着成员绕回生成的 Proxy、已经被线路组拿去用、或者和线路同名时不用它,订阅组加序号另起。
//   - used:到此为止占用的名字(含模板里写的节点与组);avoid:模板的 proxy-providers(列了成员的组在 mihomo 里还占一个同名 provider);
//   - claimed:线路组的名字与线路名(模板的一个组只顶替一次;线路名为本站保留);tmplGroup 同 nameLineGroups。
func nameExtGroups(groups []extGroup, used, avoid, claimed map[string]bool, tmplGroup func(string) bool) {
	taken := func(n string) bool { return used[n] || avoid[n] || lineGroupReserved[n] }
	for i := range groups {
		base := groups[i].Name
		name := base
		if taken(name) {
			if !avoid[name] && !lineGroupReserved[name] && !claimed[name] && tmplGroup(name) {
				groups[i].own = true
			} else {
				for n := 2; taken(name); n++ {
					name = fmt.Sprintf("%s %d", base, n)
				}
			}
		}
		used[name], claimed[name] = true, true
		groups[i].Name = name
	}
}

// extGroupClaimed 订阅组不能拿来用的模板组名:线路组已经用了的、线路名(为本站保留,见 reserveLineNames)。
func extGroupClaimed(lines []model.Line, lineGroupNames []string) map[string]bool {
	m := make(map[string]bool, len(lineGroupNames)+len(lines))
	for _, n := range lineGroupNames {
		m[n] = true
	}
	for _, l := range lines {
		if strings.TrimSpace(l.Name) != "" {
			m[l.Name] = true
		}
	}
	return m
}

// orderClashGroups 排最终的组顺序:merge 的结果原样在前(生成的组在前,模板写的组照模板的顺序,见 mergeClashGroups),
// 订阅组接在最后(用模板同名组的那几个已经在 merge 的结果里)。模板里的 GLOBAL 组不往前挪:mihomo 不看它排第几,
// 挪了只会改掉管理员模板里写的顺序(#90 原先挪到第一)。
func orderClashGroups(merged []interface{}, subs []extGroup) []interface{} {
	out := make([]interface{}, 0, len(merged)+len(subs))
	out = append(out, merged...)
	for _, s := range subs {
		if s.own {
			continue
		}
		members := make([]interface{}, len(s.Members))
		for i, n := range s.Members {
			members[i] = n
		}
		out = append(out, map[string]interface{}{
			"name": s.Name, "type": "select", "proxies": members,
		})
	}
	return out
}
