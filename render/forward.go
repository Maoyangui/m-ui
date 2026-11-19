package render

import (
	"encoding/json"
	"net"
	"sort"
	"strconv"
	"strings"
	"sync"

	"github.com/Maoyangui/m-ui/database/model"
	"github.com/Maoyangui/m-ui/logger"
	"gorm.io/gorm"
)

// 中转入口(线路的 Forwards,见 model.LineForward):Via 是本机、由面板来转的,本机开一个 sing-box 自带的 direct 入站,
// 把收到的字节原样转给出口服务器上的这条线路(override 到出口的地址 + 线路端口),不解密、不认用户。
// 别处的中转(Via = 0)和「这台上已有转发」(Manual)只进订阅,不在这里渲染。
//
// 路由:这些入站的规则排在所有规则最前面 —— 不嗅探、不劫持 DNS,也不受私网屏蔽影响
// (手填的转到地址可能是内网)。目标固定,不能被当成开放代理用。

// ForwardNetwork 中转转哪种流量:hysteria2 / tuic 和走 QUIC 传输的只有 UDP;shadowsocks 两种都转("" = TCP + UDP);其余只转 TCP。
func ForwardNetwork(line model.Line) string {
	if UDPOnly(line.Protocol) {
		return "udp"
	}
	if line.Protocol == "shadowsocks" {
		return ""
	}
	if Protocols[line.Protocol].Transport && len(line.Transport) > 0 {
		var tr struct {
			Type string `json:"type"`
		}
		if json.Unmarshal(line.Transport, &tr) == nil && tr.Type == "quic" {
			return "udp"
		}
	}
	return "tcp"
}

// ForwardTag 中转入站的 tag:线路名 + " ⇄" + 端口。同一台服务器上端口不会重复,tag 也就不重复;
// 「最近入站连接」里一眼看得出是哪条线路的中转。
func ForwardTag(line model.Line, f model.LineForward) string {
	return line.Name + " ⇄" + strconv.Itoa(f.Port)
}

// forwardMark 中转入站 tag 里线路名和端口之间的记号。
const forwardMark = " ⇄"

// IsForwardTag 这个入站 tag 是不是中转入口的(ForwardTag 生成的样子)。线路名里不会有这个记号后面紧跟纯数字结尾的写法就认定是。
func IsForwardTag(tag string) bool {
	i := strings.LastIndex(tag, forwardMark)
	if i <= 0 {
		return false
	}
	p, err := strconv.Atoi(tag[i+len(forwardMark):])
	return err == nil && p >= 1 && p <= 65535
}

// ForwardTarget 集群中转转到出口服务器的哪个地址:手填的优先;否则是出口探测到的公网 IP,IPv4 还是 IPv6 这样定 ——
//
//  1. 这条线路在出口那台按哪个 IP 版本用(线路里单独选的;没选就看那台服务器在订阅里给的是不是 IPv6,见 usesIPv6):
//     直连给的是 IPv6,中转也转到 IPv6。以前不看这两个设置:线路里给出口选了 IPv6,中转照样转到它的 IPv4。
//  2. 上面没指定 IPv6、转的又只是 UDP(hysteria2 / tuic)时,中转机自己在订阅里按 IPv6 用的(连接地址是 IPv6,
//     或「自动地址用 IPv6」)也先走 IPv6:这种机器的 IPv4 往往只是运营商 NAT 出来的出站地址,带不动 QUIC 的大 UDP 包
//     —— 中转是原样转发,包长由客户端和出口两头定,中转改不了;TCP 两端会自己缩包,所以以前只有这两种协议的中转「连上没网」。
//     转 TCP 的线路不动(本来就是通的,不去换它走的路)。
//  3. 其余和以前一样:中转有 IPv4(或两种都不知道)先用出口的 IPv4,只有 IPv6 的中转用出口的 IPv6。
//
// 要走 IPv6 得两头都有(出口探测到了、中转也有或不知道),否则照第 3 条。都没探测到用出口的连接地址、域名。空 = 算不出来。
// 界面上显示「会转到哪个地址」的 fwExitAddr(web/assets/js/pages/lines.js)是同一套挑法,改这里要一起改。
func ForwardTarget(line model.Line, f model.LineForward, exit, self model.Node) string {
	if t := model.BareHost(f.Target); t != "" {
		return t
	}
	v4, v6 := model.FamilyIPs("", exit.PublicIP, exit.PublicIP6)
	s4, s6 := model.FamilyIPs("", self.PublicIP, self.PublicIP6)
	fam := model.ParseNodeFamily(line.NodeFamily)[exit.Id]
	if fam == "" && usesIPv6(exit) { // 线路没单独选:跟出口那台服务器在订阅里给的地址走(手填了 IPv4 / 域名的连接地址时,「自动地址用 IPv6」不生效)
		fam = "v6"
	}
	prefer6 := fam == "v6" || (fam == "" && ForwardNetwork(line) == "udp" && usesIPv6(self))
	switch {
	case prefer6 && v6 != "" && (s6 != "" || s4 == ""):
		return v6
	case v4 != "" && (s4 != "" || s6 == ""):
		return v4
	case v6 != "":
		return v6
	case v4 != "":
		return v4
	}
	if a := model.BareHost(exit.Addr); a != "" {
		return a
	}
	return strings.TrimSpace(exit.Domain)
}

// usesIPv6 这台服务器在订阅里是按 IPv6 给的:连接地址填的是 IPv6;没填的话是「自动地址用 IPv6」。
func usesIPv6(n model.Node) bool {
	if a := model.BareHost(n.Addr); a != "" {
		ip := net.ParseIP(a)
		return ip != nil && ip.To4() == nil
	}
	return model.NormAddrFamily(n.AddrFamily) == "v6"
}

type forwardIn struct {
	tag     string
	inbound map[string]interface{}
}

// forwardInbounds 本机要开的中转入站:全部启用的线路里 Via 是本机、没暂停的那些。taken 是本机已经占用的端口
// (本机的线路、面板、订阅)。撞端口的、出口服务器不存在 / 停用 / 不再部署这条线路的、算不出转到哪里的都跳过 ——
// 保存时已经拦过,这里是兜底:一个坏掉的中转不能让整台服务器的数据面起不来。
func forwardInbounds(db *gorm.DB, self uint, all []model.Line, taken map[int]bool) []forwardIn {
	if self == 0 || SelfHeld(db, self) { // 本机被停止服务(额度用完 / 到期):经本机的中转入口一个都不开
		return nil
	}
	var nodes map[uint]model.Node
	var out []forwardIn
	tags := map[string]bool{}
	for _, l := range all {
		for _, f := range model.ParseForwards(l.Forwards) {
			if f.Off || f.Manual || f.Via != self || f.To == self || f.Port < 1 || f.Port > 65535 {
				continue
			}
			if nodes == nil {
				var list []model.Node
				db.Find(&list)
				nodes = make(map[uint]model.Node, len(list))
				for _, n := range list {
					nodes[n.Id] = n
				}
			}
			exit, ok := nodes[f.To]
			if !ok || !exit.Serving() || !LineOnNode(l, f.To) { // 出口停用了、被停止服务了:转过去也没人接
				continue
			}
			target := ForwardTarget(l, f, exit, nodes[self])
			tag := ForwardTag(l, f)
			if target == "" || taken[f.Port] || tags[tag] {
				warnForwardOnce(tag, "线路「"+l.Name+"」经本机的中转入口(端口 "+strconv.Itoa(f.Port)+")没有开:端口被占用或算不出转到哪里")
				continue
			}
			taken[f.Port], tags[tag] = true, true
			in := map[string]interface{}{
				"type": "direct", "tag": tag, "listen": listenAddr(), "listen_port": f.Port,
				"override_address": target, "override_port": l.Port,
			}
			if n := ForwardNetwork(l); n != "" {
				in["network"] = n
			}
			out = append(out, forwardIn{tag, in})
		}
	}
	return out
}

// SelfHeld 本机(nodes 表里的 self)是不是被停止服务了:流量额度用完 / 已到期,又没有整台停用(见 model.Node.Suspended)。
func SelfHeld(db *gorm.DB, self uint) bool {
	if self == 0 {
		return false
	}
	var held []bool
	if err := db.Model(&model.Node{}).Where("id = ?", self).Limit(1).Pluck("COALESCE(suspended, 0)", &held).Error; err != nil {
		return false
	}
	return len(held) == 1 && held[0]
}

// warnForwardOnce 同一个中转入口的问题只记一次日志(BuildConfig 每次热更新用户都会跑)。
var forwardWarned sync.Map

func warnForwardOnce(key, msg string) {
	if _, loaded := forwardWarned.LoadOrStore(key, true); !loaded {
		logger.Warning(msg)
	}
}

// localPorts 本机线路与面板、订阅占着的端口(中转入口不能再开在上面)。
func localPorts(db *gorm.DB, lines []model.Line) map[int]bool {
	taken := map[int]bool{}
	for _, l := range lines {
		taken[l.Port] = true
	}
	for key, def := range map[string]int{"webPort": 2053, "subPort": 2056} {
		var v string
		db.Model(&model.Setting{}).Select("value").Where("key = ?", key).Scan(&v)
		if p, err := strconv.Atoi(strings.TrimSpace(v)); err == nil && p > 0 {
			taken[p] = true
		} else {
			taken[def] = true
		}
	}
	return taken
}

// ForwardSig 本机中转入站的摘要(tag、端口、转到哪里、转哪种流量)。runner 每分钟比对:出口服务器换了 IP、
// 停用 / 启用了,变了就重新渲染(这些只改 nodes 表,走的是用户热更新,不会重新渲染入站)。
func ForwardSig(db *gorm.DB) string {
	var all []model.Line
	db.Where("enabled = ?", true).Order("sort asc, id asc").Find(&all)
	has := false
	for _, l := range all {
		if len(l.Forwards) > 0 {
			has = true
			break
		}
	}
	if !has {
		return ""
	}
	self := LocalNodeID(db)
	lines := make([]model.Line, 0, len(all))
	for _, l := range all {
		if LineOnNode(l, self) {
			lines = append(lines, l)
		}
	}
	var parts []string
	for _, f := range forwardInbounds(db, self, all, localPorts(db, lines)) {
		parts = append(parts, f.tag+"="+f.inbound["override_address"].(string)+":"+strconv.Itoa(f.inbound["override_port"].(int)))
	}
	sort.Strings(parts)
	return strings.Join(parts, "|")
}
