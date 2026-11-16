package core

import (
	"net/netip"
	"strconv"
	"strings"
)

// 节点 = 一条线路在这台服务器上的直连入口,或者经某个中转入口进来的那一路(见 model.NodeOpt)。
// 数据面只需要知道「这条连接是从哪个节点进来的、这个节点单独设了什么」:倍率给记账用(StatsTracker),
// 限速和容量给 Limiter 用。表在渲染配置时由 runner 算好交进来(SetNodes),连接建立时查一次。

// NodeSpec 一个节点自己的设置;0 都是没设。
type NodeSpec struct {
	Key       string  // 节点键("<线路 id>:s<服务器 id>" / "<线路 id>:f<入口 id>"):限速、容量按它分桶;空 = 这个节点什么都没单独设
	Ratio     float64 // 流量倍率;0 = 跟随服务器
	Speed     int     // 节点总限速(Mbps)
	UserSpeed int     // 每个用户在这个节点上的限速(Mbps)
	Cap       int     // 同时在线的用户数上限
}

// InboundNodes 一个入站上的节点:直连一个,加上若干中转入口。中转只是把加密数据原样转过来,
// 出口这边认不出是哪个入口,只能看来源 IP(中转服务器的地址)。
type InboundNodes struct {
	Direct  NodeSpec
	Sources map[netip.Addr]NodeSpec // 来源 IP → 那个中转入口的设置(入口什么都没设也要列在这里:它不是直连,不能套直连的设置)
}

// NodeTable 入站 tag → 这个入站上的节点。只列有东西可查的入站;没列的入站 = 所有连接都按服务器的默认。
type NodeTable map[string]InboundNodes

// Lookup 这条连接是从哪个节点进来的。来源对得上某个中转入口就是那个入口(哪怕它什么都没设),否则算直连。
func (t NodeTable) Lookup(inbound string, src netip.Addr) NodeSpec {
	in, ok := t[inbound]
	if !ok {
		return NodeSpec{}
	}
	if len(in.Sources) > 0 && src.IsValid() {
		if spec, ok := in.Sources[src.Unmap()]; ok {
			return spec
		}
	}
	return in.Direct
}

// StatUserX 统计行的一种:走了「单独设倍率的节点」的用户流量。Tag = 用户名 + userXSep + 倍率。
// 这部分流量只记在这里,不再记进 "user" 行 —— 每个字节只在一个计数器里,两边不用对账。
const StatUserX = "userx"

const userXSep = "\x1f"

// UserXTag 拼 StatUserX 行的 Tag。
func UserXTag(user string, ratio float64) string {
	return user + userXSep + strconv.FormatFloat(ratio, 'f', -1, 64)
}

// SplitUserX 拆 StatUserX 行的 Tag;格式不对或倍率不是正数返回 false。
func SplitUserX(tag string) (user string, ratio float64, ok bool) {
	i := strings.LastIndex(tag, userXSep)
	if i <= 0 {
		return "", 0, false
	}
	r, err := strconv.ParseFloat(tag[i+len(userXSep):], 64)
	if err != nil || !(r > 0) {
		return "", 0, false
	}
	return tag[:i], r, true
}
