package model

import (
	"encoding/json"
	"fmt"
	"math"
	"net/netip"
	"strconv"
	"strings"
	"unicode"
)

// LineForward 线路的一个中转入口:客户端连中转的这个地址和端口,中转把收到的加密数据原样转给出口服务器上的这条线路,
// 不解密、不认用户(和「线路作上游」不同,那种要在中转机上解密再转)。
//
// 谁来转,三种:
//   - Via 是集群里的一台服务器、Manual = false:由面板在那台上转 —— sing-box 自带的转发(direct 入站 + override),自动渲染、自动进订阅;
//   - Via 是集群里的一台服务器、Manual = true:那台上已经有别的程序在转(realm / gost / iptables),面板不去占那个端口,
//     只把「那台的地址:Port」写进订阅(那台换了地址订阅跟着变,不用像外部中转那样手填);
//   - Via = 0 是别处的中转(不在集群里的机器、买的中转线路):面板只把 Host:Port 写进订阅。
//
// 订阅里每个中转入口多一个节点:地址是中转,端口是 Port,SNI、证书校验、Reality 参数都照出口那台的来(TLS 在出口上终止)。
// 出口服务器看到的来源都是中转的 IP:同一个人经同一台中转连进来的几台设备,在出口上只算一个 IP(设备数、在线 IP 按中转算)。
type LineForward struct {
	// Id 线路内唯一的编号(保存时补,从 1 起,删了不复用):节点设置(Line.NodeOpts 的 "f<Id>")、用户不要哪些节点(UserLineHide)按它认这个入口
	Id     int    `json:"id,omitempty"`
	Name   string `json:"name,omitempty"`   // 订阅里节点名的后缀;空 = 中转服务器名 / 外部地址
	Via    uint   `json:"via,omitempty"`    // 中转服务器 nodes.id;0 = 别处的中转
	Manual bool   `json:"manual,omitempty"` // Via ≠ 0 时:那台上已有转发,面板不渲染入站,只写订阅
	Host   string `json:"host,omitempty"`   // 别处的中转的地址(Via = 0 时必填,IP 或域名)
	Port   int    `json:"port"`             // 客户端连的端口
	To     uint   `json:"to"`               // 转给哪台服务器上的这条线路(必须在线路的部署范围里)
	Target string `json:"target,omitempty"` // 面板转发时转到的地址;空 = 出口服务器的公网 IP(内网互通的可以填内网地址)
	Off    bool   `json:"off,omitempty"`    // 暂停:不渲染、不进订阅,设置留着
	// Src 出口服务器看到的、这个入口转过来的连接的来源 IP(逗号分隔,最多 8 个,只填 IP)。只用来在出口上认
	// 「这条连接是从哪个入口进来的」(节点单独设的倍率 / 限速 / 容量,见 NodeOpt)。面板自己认得出的不用填:
	// 集群里的中转用它的公网 IP 和连接地址,别处的中转用填的地址(是 IP 时)。中转经内网连出口、进出不是同一个 IP、
	// 地址是域名的才需要填;认不出来的连接按直连算。
	Src string `json:"src,omitempty"`
}

// MaxForwardSrc 一个中转入口最多手填几个来源 IP。
const MaxForwardSrc = 8

// NormForwardSrc 规整手填的来源 IP:逗号 / 空白 / 顿号分隔,去重,IPv4 映射的 IPv6 还原成 IPv4,输出逗号分隔。
func NormForwardSrc(s string) (string, error) {
	fields := strings.FieldsFunc(s, func(r rune) bool { return r == ',' || r == '，' || r == '、' || r == ';' || unicode.IsSpace(r) })
	out := make([]string, 0, len(fields))
	seen := map[string]bool{}
	for _, f := range fields {
		a, err := netip.ParseAddr(BareHost(f))
		if err != nil {
			return "", fmt.Errorf("%q 不是 IP 地址", f)
		}
		v := a.Unmap().WithZone("").String()
		if !seen[v] {
			seen[v] = true
			out = append(out, v)
		}
	}
	if len(out) > MaxForwardSrc {
		return "", fmt.Errorf("最多填 %d 个", MaxForwardSrc)
	}
	return strings.Join(out, ","), nil
}

// 订阅里的「节点」:一条线路在一台服务器上的直连入口,或这条线路的一个中转入口。
// 每个节点可以单独设置(Line.NodeOpts,键见 DirectKey / ForwardKey);用户还可以单独不要某几个节点(UserLineHide)。

// NodeOpt 一个节点自己的设置;没设的项(0)跟随默认。
type NodeOpt struct {
	Hidden bool `json:"hidden,omitempty"` // 不输出到任何人的订阅(入站照开:只是不发给用户)
	// Ratio 这个节点自己的流量倍率:设了就盖过所在服务器的倍率(只管走这个节点的流量,同一台上别的线路照旧按服务器的);
	// 0 = 没单独设,跟随服务器。中转入口没单独设时跟随出口那台服务器(流量是在出口上记的)。
	Ratio float64 `json:"ratio,omitempty"`
	// Speed 这个节点的总限速(Mbps,走这个节点的所有用户合计);UserSpeed 每个用户在这个节点上的最高速率(Mbps,
	// 和用户自己的限速同时生效,取更严的);Cap 同时在线的用户数上限(满了拒新用户,已在线的不踢)。0 都是不限。
	Speed     int `json:"speed,omitempty"`
	UserSpeed int `json:"userSpeed,omitempty"`
	Cap       int `json:"cap,omitempty"`
}

// 节点设置各项的上限(和服务器倍率、用户限速的上限一致)。
const (
	NodeRatioMax = 100
	NodeSpeedMax = 100000
	NodeCapMax   = 1000000
)

// Check 节点设置的取值范围;倍率最多留四位小数(再多没有意义,还会让同一个倍率因为写法不同被当成两个)。
func (o *NodeOpt) Check() error {
	if o.Ratio < 0 || o.Ratio > NodeRatioMax {
		return fmt.Errorf("节点倍率要在 0–%d 之间(0 = 跟随服务器)", NodeRatioMax)
	}
	o.Ratio = math.Round(o.Ratio*10000) / 10000
	if o.Speed < 0 || o.Speed > NodeSpeedMax || o.UserSpeed < 0 || o.UserSpeed > NodeSpeedMax {
		return fmt.Errorf("节点限速要在 0–%d Mbps 之间(0 = 不限)", NodeSpeedMax)
	}
	if o.Cap < 0 || o.Cap > NodeCapMax {
		return fmt.Errorf("节点的用户容量要在 0–%d 之间(0 = 不限)", NodeCapMax)
	}
	return nil
}

// NodeRatio 节点实际生效的倍率:自己设了用自己的,没设用所在服务器的(serverRatio ≤ 0 当 1)。
func NodeRatio(o NodeOpt, serverRatio float64) float64 {
	if o.Ratio > 0 {
		return o.Ratio
	}
	if serverRatio <= 0 {
		return 1
	}
	return serverRatio
}

// DirectKey 线路在服务器 nodeId 上的直连节点的键。
func DirectKey(nodeId uint) string { return "s" + strconv.FormatUint(uint64(nodeId), 10) }

// ForwardKey 线路的中转入口 id 的键。
func ForwardKey(id int) string { return "f" + strconv.Itoa(id) }

// ParseNodeOpts 解出线路的节点设置;格式不对按没有处理。
func ParseNodeOpts(raw json.RawMessage) map[string]NodeOpt {
	if len(raw) == 0 {
		return nil
	}
	var out map[string]NodeOpt
	if json.Unmarshal(raw, &out) != nil {
		return nil
	}
	return out
}

// AssignForwardIds 给还没有编号的中转入口补上编号(比现有最大的大 1 起);有补的返回 true。
// 编号重复的(手改 JSON、复制粘贴)后一个按没有编号处理。floor(可选)= 以前用过的最大编号:新编号从它往上取,
// 删掉的入口的编号不会马上被新入口用上。
func AssignForwardIds(fs []LineForward, floor ...int) bool {
	max, seen, changed := 0, map[int]bool{}, false
	for _, f := range floor {
		if f > max {
			max = f
		}
	}
	for i := range fs {
		if fs[i].Id > 0 && !seen[fs[i].Id] {
			seen[fs[i].Id] = true
			if fs[i].Id > max {
				max = fs[i].Id
			}
		} else if fs[i].Id != 0 {
			fs[i].Id = 0
		}
	}
	for i := range fs {
		if fs[i].Id == 0 {
			max++
			fs[i].Id = max
			changed = true
		}
	}
	return changed
}

// Managed 这个入口由面板在中转服务器上转发(要在那台上开入站、占端口)。
func (f LineForward) Managed() bool { return f.Via != 0 && !f.Manual }

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

// BlockedInbound 一个因为端口被这台机器上别的程序占着而没开的入站(中转入口或线路)。别的入站照常服务;
// 每分钟试一次监听,端口空出来就热加回去。副机随报告上报,主机在服务器页、概况、线路详情里点名。
type BlockedInbound struct {
	Tag   string `json:"tag"`           // 入站 tag:中转入口是「线路名 ⇄端口」,线路就是线路名
	Port  int    `json:"port"`          // 监听端口
	Net   string `json:"net,omitempty"` // 没监听上的是 tcp 还是 udp
	By    string `json:"by,omitempty"`  // 占着端口的程序名(查得到的话,比如 realm)
	Since int64  `json:"since"`         // 从什么时候开始没开
}

// PortProbe 问一台服务器某个端口现在能不能监听;Net 空 = TCP 和 UDP 都要。
type PortProbe struct {
	Port int    `json:"port"`
	Net  string `json:"net,omitempty"`
}

// PortProbeResult 探测结果:Free = 能监听;占着的话 By 是程序名(查得到的话),Self = 占着它的就是 m-ui 自己。
type PortProbeResult struct {
	Port int    `json:"port"`
	Net  string `json:"net,omitempty"`
	Free bool   `json:"free"`
	By   string `json:"by,omitempty"`
	Self bool   `json:"self,omitempty"`
}
