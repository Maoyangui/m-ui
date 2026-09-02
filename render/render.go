// Package render 把 m-ui 数据模型(线路/上游/用户)渲染成一份 sing-box 配置。
//
// 一条线路 = 一个入站 + 一条 "inbound→outbound" 路由规则。支持的入站协议:
// hysteria2 / anytls / tuic / trojan / vless / vmess / shadowsocks / socks / http / mixed。
// TLS 三种模式:cert(节点证书)、reality、none;vless/vmess/trojan 可选 ws/grpc/httpupgrade/http 传输。
// 该渲染由 Hub 按节点执行,产物整份下发给对应 Agent。
package render

import (
	"encoding/json"
	"fmt"
	"sort"
	"strings"

	"github.com/fangjunsheng555/m-ui/database/model"

	"gorm.io/gorm"
)

// NodeCert 节点的服务端 TLS 材料(证书各机各签,路径固定)。
type NodeCert struct {
	ServerName string
	CertPath   string
	KeyPath    string
}

// TLSConfig 是 Line.Tls 的结构。mode 为空时按协议取默认。
type TLSConfig struct {
	Mode        string `json:"mode"` // cert | reality | none
	Fingerprint string `json:"fingerprint,omitempty"`
	Reality     struct {
		PrivateKey      string   `json:"private_key"`
		PublicKey       string   `json:"public_key"`
		ShortIDs        []string `json:"short_ids"`
		HandshakeServer string   `json:"handshake_server"`
		HandshakePort   int      `json:"handshake_port"`
	} `json:"reality"`
}

// Protocols 是面板支持的入站协议及其特性。
var Protocols = map[string]struct {
	TLSRequired  bool   // 没有 TLS 就无法工作
	TLSDefault   string // Tls 为空时的默认模式
	Transport    bool   // 支持 ws/grpc/httpupgrade/http 传输
	HotUsers     bool   // 支持不断线热换用户表
	CredKey      string // 凭据键(shadowsocks 按算法另算)
	SingleSecret bool   // 单口令入站(用户级凭据不下发)
}{
	"hysteria2":   {TLSRequired: true, TLSDefault: "cert", HotUsers: true, CredKey: "hysteria2"},
	"anytls":      {TLSRequired: true, TLSDefault: "cert", HotUsers: true, CredKey: "anytls"},
	"tuic":        {TLSRequired: true, TLSDefault: "cert", HotUsers: true, CredKey: "tuic"},
	"trojan":      {TLSDefault: "cert", Transport: true, HotUsers: true, CredKey: "trojan"},
	"vless":       {TLSDefault: "reality", Transport: true, HotUsers: true, CredKey: "vless"},
	"vmess":       {TLSDefault: "none", Transport: true, HotUsers: true, CredKey: "vmess"},
	"shadowsocks": {TLSDefault: "none", HotUsers: true, CredKey: "shadowsocks"},
	"socks":       {TLSDefault: "none", CredKey: "socks"},
	"http":        {TLSDefault: "cert", CredKey: "http"},
	"mixed":       {TLSDefault: "none", CredKey: "socks"},
}

// ParseTLS 解析线路的 TLS 配置并补默认模式。
func ParseTLS(line model.Line) TLSConfig {
	var c TLSConfig
	if len(line.Tls) > 0 {
		_ = json.Unmarshal(line.Tls, &c)
	}
	if c.Mode == "" {
		c.Mode = Protocols[line.Protocol].TLSDefault
		if c.Mode == "" {
			c.Mode = "none"
		}
	}
	return c
}

// LineOnNode 报告线路是否部署在某台服务器上:NodeIds 为空 = 全部;selfID 为 0(尚无本机记录)视为全部。
func LineOnNode(line model.Line, selfID uint) bool {
	if len(line.NodeIds) == 0 || selfID == 0 {
		return true
	}
	var ids []uint
	if json.Unmarshal(line.NodeIds, &ids) != nil || len(ids) == 0 {
		return true
	}
	for _, id := range ids {
		if id == selfID {
			return true
		}
	}
	return false
}

// LocalNodeID 返回本机在 nodes 表中的 id(没有则 0)。
func LocalNodeID(db *gorm.DB) uint {
	var n model.Node
	if err := db.Where("is_local = ?", true).First(&n).Error; err != nil {
		return 0
	}
	return n.Id
}

// BuildConfig 从数据库读取本机应部署的线路/全部上游/用户,渲染成 sing-box 配置字节。
func BuildConfig(db *gorm.DB, cert NodeCert) ([]byte, error) {
	var all []model.Line
	if err := db.Where("enabled = ?", true).Order("sort asc, id asc").Find(&all).Error; err != nil {
		return nil, err
	}
	self := LocalNodeID(db)
	lines := make([]model.Line, 0, len(all))
	for _, l := range all {
		if LineOnNode(l, self) {
			lines = append(lines, l)
		}
	}
	var upstreams []model.Upstream
	if err := db.Order("id asc").Find(&upstreams).Error; err != nil {
		return nil, err
	}
	upstreamById := make(map[uint]model.Upstream, len(upstreams))
	for _, u := range upstreams {
		upstreamById[u.Id] = u
	}
	usersByLine, err := loadLineUsers(db)
	if err != nil {
		return nil, err
	}

	inbounds := make([]json.RawMessage, 0, len(lines))
	rules := []json.RawMessage{
		json.RawMessage(`{"action":"sniff"}`),
		json.RawMessage(`{"protocol":["dns"],"action":"hijack-dns"}`),
	}
	for _, line := range lines {
		inbound, err := renderInbound(line, cert, usersByLine[line.Id])
		if err != nil {
			return nil, fmt.Errorf("线路 %q: %w", line.Name, err)
		}
		inbounds = append(inbounds, inbound)

		outboundTag := "direct"
		if line.UpstreamId != 0 {
			up, ok := upstreamById[line.UpstreamId]
			if !ok {
				return nil, fmt.Errorf("线路 %q 指向不存在的上游 #%d", line.Name, line.UpstreamId)
			}
			outboundTag = up.Name
		}
		rule, _ := json.Marshal(map[string]interface{}{"inbound": []string{line.Name}, "action": "route", "outbound": outboundTag})
		rules = append(rules, rule)
	}

	outbounds, err := renderOutbounds(upstreams)
	if err != nil {
		return nil, err
	}
	config := map[string]interface{}{
		"log":       map[string]interface{}{"level": "info"},
		"dns":       map[string]interface{}{"servers": []map[string]interface{}{{"type": "local", "tag": "local"}}},
		"inbounds":  inbounds,
		"outbounds": outbounds,
		"route":     map[string]interface{}{"rules": rules, "final": "direct"},
	}
	return json.MarshalIndent(config, "", "  ")
}

// loadLineUsers 返回 lineId → 启用用户列表(含凭据)。
func loadLineUsers(db *gorm.DB) (map[uint][]model.User, error) {
	var links []model.UserLine
	if err := db.Find(&links).Error; err != nil {
		return nil, err
	}
	var users []model.User
	if err := db.Where("enabled = ?", true).Find(&users).Error; err != nil {
		return nil, err
	}
	userById := make(map[uint]model.User, len(users))
	for _, u := range users {
		userById[u.Id] = u
	}
	byLine := map[uint][]model.User{}
	for _, l := range links {
		if u, ok := userById[l.UserId]; ok {
			byLine[l.LineId] = append(byLine[l.LineId], u)
		}
	}
	for id := range byLine {
		sort.Slice(byLine[id], func(i, j int) bool { return byLine[id][i].Id < byLine[id][j].Id })
	}
	return byLine, nil
}

// InboundJSON 渲染单个线路的入站(含 TLS/传输,不含用户),供保存前校验与整体渲染共用。
func InboundJSON(line model.Line, cert NodeCert) (json.RawMessage, error) {
	inbound, err := inboundBase(line, cert)
	if err != nil {
		return nil, err
	}
	return json.Marshal(inbound)
}

func renderInbound(line model.Line, cert NodeCert, users []model.User) (json.RawMessage, error) {
	inbound, err := inboundBase(line, cert)
	if err != nil {
		return nil, err
	}
	userList, err := renderUsers(line, inbound, users)
	if err != nil {
		return nil, err
	}
	if userList != nil {
		inbound["users"] = userList
	}
	return json.Marshal(inbound)
}

// inboundBase 组装入站的协议参数 + 监听 + TLS + 传输(不含用户)。
func inboundBase(line model.Line, cert NodeCert) (map[string]interface{}, error) {
	spec, ok := Protocols[line.Protocol]
	if !ok {
		return nil, fmt.Errorf("不支持的协议 %s", line.Protocol)
	}
	inbound := map[string]interface{}{}
	if len(line.Options) > 0 {
		if err := json.Unmarshal(line.Options, &inbound); err != nil {
			return nil, fmt.Errorf("解析线路参数: %w", err)
		}
	}
	// 面板自有的开关键,不是 sing-box 字段
	delete(inbound, "vision")
	inbound["type"] = line.Protocol
	inbound["tag"] = line.Name
	inbound["listen"] = "::"
	inbound["listen_port"] = line.Port

	tlsConf := ParseTLS(line)
	switch tlsConf.Mode {
	case "cert":
		inbound["tls"] = map[string]interface{}{
			"enabled": true, "server_name": cert.ServerName,
			"certificate_path": cert.CertPath, "key_path": cert.KeyPath,
		}
	case "reality":
		r := tlsConf.Reality
		if r.PrivateKey == "" || r.HandshakeServer == "" {
			return nil, fmt.Errorf("reality 需要 private_key 与 handshake_server")
		}
		port := r.HandshakePort
		if port == 0 {
			port = 443
		}
		shortIDs := r.ShortIDs
		if len(shortIDs) == 0 {
			shortIDs = []string{""}
		}
		inbound["tls"] = map[string]interface{}{
			"enabled": true, "server_name": r.HandshakeServer,
			"reality": map[string]interface{}{
				"enabled":     true,
				"handshake":   map[string]interface{}{"server": r.HandshakeServer, "server_port": port},
				"private_key": r.PrivateKey,
				"short_id":    shortIDs,
			},
		}
	case "none":
		if spec.TLSRequired {
			return nil, fmt.Errorf("%s 必须启用 TLS", line.Protocol)
		}
		delete(inbound, "tls")
	default:
		return nil, fmt.Errorf("未知 TLS 模式 %q", tlsConf.Mode)
	}

	if spec.Transport && len(line.Transport) > 0 {
		var tr map[string]interface{}
		if err := json.Unmarshal(line.Transport, &tr); err != nil {
			return nil, fmt.Errorf("解析传输配置: %w", err)
		}
		if typ, _ := tr["type"].(string); typ != "" && typ != "tcp" {
			inbound["transport"] = tr
		}
	}
	return inbound, nil
}

// HasTransport 报告线路是否配置了非 TCP 传输。
func HasTransport(line model.Line) bool {
	if len(line.Transport) == 0 {
		return false
	}
	var tr struct {
		Type string `json:"type"`
	}
	_ = json.Unmarshal(line.Transport, &tr)
	return tr.Type != "" && tr.Type != "tcp"
}

// VisionEnabled 报告 vless 线路是否启用 xtls-rprx-vision(仅在有 TLS 且无传输时有效)。
func VisionEnabled(line model.Line) bool {
	if line.Protocol != "vless" {
		return false
	}
	var o struct {
		Vision bool `json:"vision"`
	}
	_ = json.Unmarshal(line.Options, &o)
	return o.Vision && ParseTLS(line).Mode != "none" && !HasTransport(line)
}

// CredKey 返回线路对应的用户凭据键。
func CredKey(line model.Line) string {
	if line.Protocol == "shadowsocks" {
		var o struct {
			Method string `json:"method"`
		}
		_ = json.Unmarshal(line.Options, &o)
		return shadowsocksCredKey(o.Method)
	}
	return Protocols[line.Protocol].CredKey
}

// renderUsers 从用户凭据中取出该线路协议所需字段,渲染成入站 users 项。
func renderUsers(line model.Line, inbound map[string]interface{}, users []model.User) ([]map[string]interface{}, error) {
	spec := Protocols[line.Protocol]
	key := CredKey(line)
	vision := VisionEnabled(line)
	out := make([]map[string]interface{}, 0, len(users))
	for _, u := range users {
		var creds map[string]map[string]interface{}
		if len(u.Credentials) > 0 {
			if err := json.Unmarshal(u.Credentials, &creds); err != nil {
				return nil, fmt.Errorf("用户 %q 凭据解析失败: %w", u.Name, err)
			}
		}
		c := creds[key]
		password, _ := c["password"].(string)
		id, _ := c["uuid"].(string)
		switch line.Protocol {
		case "hysteria2", "anytls", "trojan", "shadowsocks":
			out = append(out, map[string]interface{}{"name": u.Name, "password": password})
		case "tuic":
			out = append(out, map[string]interface{}{"name": u.Name, "uuid": id, "password": password})
		case "vless":
			e := map[string]interface{}{"name": u.Name, "uuid": id}
			if vision {
				e["flow"] = "xtls-rprx-vision"
			}
			out = append(out, e)
		case "vmess":
			out = append(out, map[string]interface{}{"name": u.Name, "uuid": id, "alterId": 0})
		case "socks", "http", "mixed":
			out = append(out, map[string]interface{}{"username": u.Name, "password": password})
		default:
			return nil, fmt.Errorf("协议 %s 不支持按用户下发", line.Protocol)
		}
	}
	_ = spec
	return out, nil
}

func shadowsocksCredKey(method string) string {
	if strings.HasPrefix(method, "2022-blake3-aes-128") {
		return "shadowsocks16"
	}
	return "shadowsocks"
}

func renderOutbounds(upstreams []model.Upstream) ([]json.RawMessage, error) {
	outbounds := []json.RawMessage{
		json.RawMessage(`{"type":"direct","tag":"direct"}`),
		json.RawMessage(`{"type":"block","tag":"block"}`),
	}
	for _, u := range upstreams {
		encoded, err := OutboundJSON(u)
		if err != nil {
			return nil, err
		}
		outbounds = append(outbounds, encoded)
	}
	return outbounds, nil
}

// OutboundJSON 渲染单个上游为 sing-box 出站(整体渲染与保存前隔离校验共用)。
func OutboundJSON(u model.Upstream) (json.RawMessage, error) {
	opts := map[string]interface{}{}
	if len(u.Options) > 0 {
		if err := json.Unmarshal(u.Options, &opts); err != nil {
			return nil, fmt.Errorf("上游 %q 参数解析失败: %w", u.Name, err)
		}
	}
	opts["type"] = u.Type
	opts["tag"] = u.Name
	return json.Marshal(opts)
}
