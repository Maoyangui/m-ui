package web

import (
	"bytes"
	"encoding/json"
	"errors"
	"fmt"
	"strings"

	"github.com/Maoyangui/m-ui/database/model"
)

// normLineAddrs 线路自定义地址(Line.Addrs,[{"server","server_port","remark","sni","node_id"}])保存前规整:
// server 去空白、去掉 IPv6 外面的方括号,必须是 IP 或域名(不带端口、协议头、路径);server_port 是 0-65535 的整数
// (0 = 用线路端口);其余字段类型要对。规整后回写,空数组存空。
// 键名不分大小写(订阅那边 json.Unmarshal 本来就这么读,写成 "Server"、"Server_Port" 的老数据一直能用),保存时改写成小写;
// 同一个键大小写不同写了两遍的,分不清以哪个为准,拦下来说清楚。
//
// 以前原样存:把 IPv6 写成 [2001:db8::1](和服务器页"连接地址"一样的习惯写法)会拼出 [[2001:db8::1]]:端口 的链接、
// Clash 里是带方括号的 server,sing-box 订阅里这个节点被静默丢掉;写成 地址:端口 也会拼出坏地址。
func normLineAddrs(raw json.RawMessage) (json.RawMessage, error) {
	trimmed := bytes.TrimSpace(raw)
	if len(trimmed) == 0 || string(trimmed) == "null" {
		return nil, nil
	}
	var list []interface{}
	dec := json.NewDecoder(bytes.NewReader(trimmed))
	dec.UseNumber()
	if err := dec.Decode(&list); err != nil || dec.More() {
		return nil, errors.New(`线路的自定义地址要是 JSON 数组,例如 [{"server":"1.2.3.4","server_port":443}]`)
	}
	if len(list) == 0 {
		return nil, nil
	}
	for i, item := range list {
		m, ok := item.(map[string]interface{})
		if !ok {
			return nil, fmt.Errorf(`第 %d 个自定义地址要写成 {"server":"IP 或域名"} 这样的对象`, i+1)
		}
		for k, v := range m {
			for _, want := range addrKeys {
				if k != want && strings.EqualFold(k, want) {
					if _, dup := m[want]; dup {
						return nil, fmt.Errorf("第 %d 个自定义地址里 %s 写了不止一次(键名大小写不同)", i+1, want)
					}
					delete(m, k)
					m[want] = v
				}
			}
		}
		s, _ := m["server"].(string)
		host := model.BareHost(s)
		if host == "" {
			return nil, fmt.Errorf(`第 %d 个自定义地址要写成 {"server":"IP 或域名"} 这样的对象`, i+1)
		}
		if !model.ValidAddr(host) {
			return nil, fmt.Errorf("第 %d 个自定义地址 %q 不对:server 只填 IP 或域名,不带端口、协议头和路径(端口写在 server_port)", i+1, s)
		}
		m["server"] = host
		if v, has := m["server_port"]; has && v != nil {
			n, ok := v.(json.Number)
			p, err := n.Int64()
			if !ok || err != nil || p < 0 || p > 65535 {
				return nil, fmt.Errorf("第 %d 个自定义地址的 server_port 要是 0-65535 的整数(0 = 用线路端口)", i+1)
			}
			m["server_port"] = p
		}
		if v, has := m["node_id"]; has && v != nil {
			n, ok := v.(json.Number)
			id, err := n.Int64()
			if !ok || err != nil || id < 0 || id > 1<<32-1 {
				return nil, fmt.Errorf("第 %d 个自定义地址的 %s 格式不对", i+1, "node_id")
			}
			m["node_id"] = id
		}
		for _, k := range []string{"remark", "sni"} {
			if v, has := m[k]; has && v != nil {
				if _, ok := v.(string); !ok {
					return nil, fmt.Errorf("第 %d 个自定义地址的 %s 格式不对", i+1, k)
				}
			}
		}
	}
	return json.Marshal(list)
}

// addrKeys 自定义地址里认的键(sub.resolveAddrs 读的就是这几个)。
var addrKeys = []string{"server", "server_port", "node_id", "remark", "sni"}
