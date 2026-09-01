// Package render 把 m-ui 数据模型(线路/上游/用户)渲染成一份 sing-box 配置。
//
// 一条线路 = 一个入站(hy2/anytls/ss) + 一条 "inbound→outbound" 路由规则。
// hy2/anytls 使用节点共享 TLS 并按用户下发凭据;ss 为固定单密码入站(无 TLS、无用户)。
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

// tlsProtocols 的入站挂载节点共享 TLS。
// 三种协议的入站都按用户下发凭据(ss 为多用户模式:各用户用自己的 ss 密码,
// 与 s-ui 行为和既有分享链接一致;线路自身 password 保留,2022 算法时作为服务端 PSK)。
var tlsProtocols = map[string]bool{"hysteria2": true, "anytls": true}

// BuildConfig 从数据库读取全部线路/上游/用户,渲染成 sing-box 配置字节。
func BuildConfig(db *gorm.DB, cert NodeCert) ([]byte, error) {
	var lines []model.Line
	if err := db.Where("enabled = ?", true).Order("sort asc, id asc").Find(&lines).Error; err != nil {
		return nil, err
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
	usedUpstreams := map[uint]bool{}

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
			usedUpstreams[up.Id] = true
		}
		rule, _ := json.Marshal(map[string]interface{}{
			"inbound":  []string{line.Name},
			"action":   "route",
			"outbound": outboundTag,
		})
		rules = append(rules, rule)
	}

	outbounds, err := renderOutbounds(upstreams)
	if err != nil {
		return nil, err
	}

	config := map[string]interface{}{
		"log": map[string]interface{}{"level": "info"},
		"dns": map[string]interface{}{
			"servers": []map[string]interface{}{{"type": "local", "tag": "local"}},
		},
		"inbounds":  inbounds,
		"outbounds": outbounds,
		"route": map[string]interface{}{
			"rules": rules,
			"final": "direct",
		},
	}
	return json.MarshalIndent(config, "", "  ")
}

// loadLineUsers 返回 lineId → 启用用户列表(含凭据),用于按用户下发。
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
	// 稳定顺序:按用户 id
	for id := range byLine {
		sort.Slice(byLine[id], func(i, j int) bool { return byLine[id][i].Id < byLine[id][j].Id })
	}
	return byLine, nil
}

func renderInbound(line model.Line, cert NodeCert, users []model.User) (json.RawMessage, error) {
	inbound := map[string]interface{}{}
	if len(line.Options) > 0 {
		if err := json.Unmarshal(line.Options, &inbound); err != nil {
			return nil, fmt.Errorf("解析线路参数: %w", err)
		}
	}
	inbound["type"] = line.Protocol
	inbound["tag"] = line.Name
	inbound["listen"] = "::"
	inbound["listen_port"] = line.Port

	if tlsProtocols[line.Protocol] {
		inbound["tls"] = tlsServerBlock(cert)
	}
	userList, err := renderUsers(line.Protocol, inbound, users)
	if err != nil {
		return nil, err
	}
	if userList != nil {
		inbound["users"] = userList
	}
	return json.Marshal(inbound)
}

func tlsServerBlock(cert NodeCert) map[string]interface{} {
	return map[string]interface{}{
		"enabled":          true,
		"server_name":      cert.ServerName,
		"certificate_path": cert.CertPath,
		"key_path":         cert.KeyPath,
	}
}

// renderUsers 从用户凭据中取出对应协议的字段,渲染成入站 users 项。
// shadowsocks 的凭据键随加密方式而定(2022-blake3-aes-128-* 用 shadowsocks16),
// 与 s-ui 及既有分享链接保持一致。
func renderUsers(protocol string, inbound map[string]interface{}, users []model.User) ([]map[string]interface{}, error) {
	credKey := protocol
	if protocol == "shadowsocks" {
		method, _ := inbound["method"].(string)
		credKey = shadowsocksCredKey(method)
	}

	out := make([]map[string]interface{}, 0, len(users))
	for _, u := range users {
		var creds map[string]map[string]interface{}
		if len(u.Credentials) > 0 {
			if err := json.Unmarshal(u.Credentials, &creds); err != nil {
				return nil, fmt.Errorf("用户 %q 凭据解析失败: %w", u.Name, err)
			}
		}
		cred := creds[credKey]
		switch protocol {
		case "hysteria2", "anytls", "shadowsocks":
			password, _ := cred["password"].(string)
			out = append(out, map[string]interface{}{"name": u.Name, "password": password})
		default:
			return nil, fmt.Errorf("协议 %s 不支持按用户下发", protocol)
		}
	}
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
		var opts map[string]interface{}
		if len(u.Options) > 0 {
			if err := json.Unmarshal(u.Options, &opts); err != nil {
				return nil, fmt.Errorf("上游 %q 参数解析失败: %w", u.Name, err)
			}
		} else {
			opts = map[string]interface{}{}
		}
		opts["type"] = u.Type
		opts["tag"] = u.Name
		encoded, err := json.Marshal(opts)
		if err != nil {
			return nil, err
		}
		outbounds = append(outbounds, encoded)
	}
	return outbounds, nil
}
