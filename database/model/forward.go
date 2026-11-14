package model

import "encoding/json"

// LineForward 线路的一个中转入口:客户端连中转的这个地址和端口,中转把收到的加密数据原样转给出口服务器上的这条线路,
// 不解密、不认用户(和「线路作上游」不同,那种要在中转机上解密再转)。
//
//   - Via 是集群里的一台服务器:由那台用 sing-box 自带的转发(direct 入站 + override)来转,面板自动渲染、自动进订阅;
//   - Via = 0 是外部中转(自己在别处跑的 realm / iptables、买的中转):面板只把 Host:Port 写进订阅。
//
// 订阅里每个中转入口多一个节点:地址是中转,端口是 Port,SNI、证书校验、Reality 参数都照出口那台的来(TLS 在出口上终止)。
// 出口服务器看到的来源都是中转的 IP:同一个人经同一台中转连进来的几台设备,在出口上只算一个 IP(设备数、在线 IP 按中转算)。
type LineForward struct {
	Name   string `json:"name,omitempty"`   // 订阅里节点名的后缀;空 = 中转服务器名 / 外部地址
	Via    uint   `json:"via,omitempty"`    // 中转服务器 nodes.id;0 = 外部中转
	Host   string `json:"host,omitempty"`   // 外部中转的地址(Via = 0 时必填,IP 或域名)
	Port   int    `json:"port"`             // 客户端连的端口
	To     uint   `json:"to"`               // 转给哪台服务器上的这条线路(必须在线路的部署范围里)
	Target string `json:"target,omitempty"` // 集群中转转到的地址;空 = 出口服务器的公网 IP(内网互通的可以填内网地址)
	Off    bool   `json:"off,omitempty"`    // 暂停:不渲染、不进订阅,设置留着
}

// MaxForwards 一条线路最多几个中转入口。
const MaxForwards = 32

// ParseForwards 解出线路的中转入口;格式不对按没有处理(保存时已校验)。
func ParseForwards(raw json.RawMessage) []LineForward {
	if len(raw) == 0 {
		return nil
	}
	var out []LineForward
	if json.Unmarshal(raw, &out) != nil {
		return nil
	}
	return out
}
