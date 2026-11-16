package runner

import (
	"net/netip"
	"strconv"
	"strings"

	"github.com/Maoyangui/m-ui/core"
	"github.com/Maoyangui/m-ui/database/model"
	"github.com/Maoyangui/m-ui/render"
)

// nodeTable 这台服务器(self)上的节点表:哪些入站上有节点单独设了倍率 / 限速 / 容量(model.NodeOpt),交给数据面在连接建立时查。
//
// 一条线路在这台上的节点 = 直连一个("s<self>")+ 转到这台的各个中转入口("f<入口 id>")。中转只是把加密数据原样转过来,
// 这台认不出是哪个入口,只能看来源 IP,所以每个入口要列出「这台会看到的来源」(ForwardSources)。
// 只要这条线路在这台上有任何一个节点单独设了东西,转到这台的入口就全部列进去(哪怕它自己什么都没设):
// 不列的话经它进来的连接会被当成直连,套上直连节点的设置。
// 同一个来源 IP 对上两个入口(同一台中转上开了两个端口转到同一台)时分不清,按排在前面的那个算。
func nodeTable(lines []model.Line, nodes []model.Node, self uint) core.NodeTable {
	byId := make(map[uint]model.Node, len(nodes))
	for _, n := range nodes {
		byId[n.Id] = n
	}
	out := core.NodeTable{}
	for _, l := range lines {
		if !l.Enabled || !render.LineOnNode(l, self) {
			continue
		}
		opts := model.ParseNodeOpts(l.NodeOpts)
		if len(opts) == 0 {
			continue
		}
		in := core.InboundNodes{Direct: nodeSpec(l.Id, model.DirectKey(self), opts)}
		any := in.Direct.Key != ""
		for _, f := range model.ParseForwards(l.Forwards) {
			if f.Off || f.To != self || f.Id == 0 {
				continue
			}
			spec := nodeSpec(l.Id, model.ForwardKey(f.Id), opts)
			any = any || spec.Key != ""
			for _, a := range ForwardSources(f, byId) {
				if in.Sources == nil {
					in.Sources = map[netip.Addr]core.NodeSpec{}
				}
				if _, dup := in.Sources[a]; !dup {
					in.Sources[a] = spec
				}
			}
		}
		if any {
			out[l.Name] = in // 入站的 tag 就是线路名(render 里定的)
		}
	}
	return out
}

// nodeSpec 一个节点单独设的东西;什么都没设(只关了输出不算:那只影响订阅)返回空的。
func nodeSpec(lineId uint, key string, opts map[string]model.NodeOpt) core.NodeSpec {
	o, ok := opts[key]
	if !ok || (o.Ratio <= 0 && o.Speed <= 0 && o.UserSpeed <= 0 && o.Cap <= 0) {
		return core.NodeSpec{}
	}
	return core.NodeSpec{
		Key:       strconv.FormatUint(uint64(lineId), 10) + ":" + key,
		Ratio:     o.Ratio,
		Speed:     o.Speed,
		UserSpeed: o.UserSpeed,
		Cap:       o.Cap,
	}
}

// ForwardSources 出口这台会看到的、这个中转入口转过来的连接的来源 IP:手填的(LineForward.Src)优先列上;
// 集群里的中转再加上它的公网 IPv4 / IPv6 和连接地址(是 IP 时);别处的中转加上填的地址(是 IP 时)。
// 认不出来的情况(中转经内网连出口而面板不知道它的内网地址、进出不是同一个 IP、地址是域名)要在入口上手填来源,
// 不填就按直连算。
func ForwardSources(f model.LineForward, nodes map[uint]model.Node) []netip.Addr {
	var out []netip.Addr
	seen := map[netip.Addr]bool{}
	add := func(s string) {
		a, err := netip.ParseAddr(strings.TrimSpace(model.BareHost(s)))
		if err != nil {
			return
		}
		a = a.Unmap().WithZone("")
		if !seen[a] {
			seen[a] = true
			out = append(out, a)
		}
	}
	for _, s := range strings.Split(f.Src, ",") {
		add(s)
	}
	if f.Via == 0 {
		add(f.Host)
		return out
	}
	if n, ok := nodes[f.Via]; ok {
		add(n.PublicIP)
		add(n.PublicIP6)
		add(n.Addr)
	}
	return out
}

// readNodeTable 从库里算出这台的节点表。
func (r *Runner) readNodeTable() (core.NodeTable, error) {
	var lines []model.Line
	if err := r.db.Select("id, name, enabled, node_ids, forwards, node_opts").Where("enabled = ? AND node_opts IS NOT NULL AND node_opts <> ''", true).Find(&lines).Error; err != nil {
		return nil, err
	}
	if len(lines) == 0 {
		return nil, nil
	}
	var nodes []model.Node
	if err := r.db.Select("id, public_ip, public_ip6, addr").Find(&nodes).Error; err != nil {
		return nil, err
	}
	return nodeTable(lines, nodes, render.LocalNodeID(r.db)), nil
}
