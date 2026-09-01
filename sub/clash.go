package sub

import (
	"encoding/json"

	"github.com/fangjunsheng555/m-ui/database/model"

	"gopkg.in/yaml.v3"
)

// basicClashConfig 是 clash/mihomo 订阅的骨架(tun + dns + 规则),与 s-ui 默认一致。
// 用户如在设置里提供自定义模板则整体替换(P2 设置项 subClashExt)。
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

// BuildClash 生成一个用户的 clash 订阅 YAML。
// notice 为可选的顶部提示节点名(如流量提示);为空则不注入。
func BuildClash(user model.User, lines []model.Line, entries []Entry, template, notice string) (string, error) {
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
	var proxyNames []string

	if notice != "" {
		proxies = append(proxies, noticeClashProxy(notice))
	}
	for _, line := range lines {
		for _, a := range resolveAddrs(line, entries) {
			p := lineToClashProxy(line, user, a)
			if p == nil {
				continue
			}
			proxies = append(proxies, p)
			proxyNames = append(proxyNames, p["name"].(string))
		}
	}
	root["proxies"] = proxies

	// 默认分组:Proxy(手选,含 Auto 与提示节点在前)+ Auto(自动测速)。
	selectList := []string{}
	if notice != "" {
		selectList = append(selectList, notice)
	}
	selectList = append(selectList, "Auto")
	selectList = append(selectList, proxyNames...)
	root["proxy-groups"] = []interface{}{
		map[string]interface{}{"name": "Proxy", "type": "select", "proxies": selectList},
		map[string]interface{}{"name": "Auto", "type": "url-test", "url": "http://www.gstatic.com/generate_204", "interval": 300, "tolerance": 50, "proxies": proxyNames},
	}

	out, err := yaml.Marshal(root)
	if err != nil {
		return "", err
	}
	return string(out), nil
}

func lineToClashProxy(line model.Line, user model.User, a addr) map[string]interface{} {
	name := line.Name + a.remark
	p := map[string]interface{}{
		"name":   name,
		"server": a.server,
		"port":   a.port,
	}
	switch line.Protocol {
	case "hysteria2":
		password, _ := userCred(user, "hysteria2")["password"].(string)
		p["type"] = "hysteria2"
		p["password"] = password
		if a.sni != "" {
			p["sni"] = a.sni
		}
		p["alpn"] = []string{"h3"}
	case "anytls":
		password, _ := userCred(user, "anytls")["password"].(string)
		p["type"] = "anytls"
		p["password"] = password
		if a.sni != "" {
			p["sni"] = a.sni
		}
	case "shadowsocks":
		var opts map[string]interface{}
		_ = json.Unmarshal(line.Options, &opts)
		method, _ := opts["method"].(string)
		password, _ := userCred(user, ssCredKey(method))["password"].(string)
		p["type"] = "ss"
		p["cipher"] = method
		p["password"] = password
		p["udp"] = true
	default:
		return nil
	}
	return p
}

// noticeClashProxy 生成一个不可用的占位代理作为顶部提示(流量/到期信息写在名字里)。
func noticeClashProxy(name string) map[string]interface{} {
	return map[string]interface{}{
		"name":     name,
		"type":     "ss",
		"server":   "127.0.0.1",
		"port":     1,
		"cipher":   "aes-128-gcm",
		"password": "dummy",
	}
}
