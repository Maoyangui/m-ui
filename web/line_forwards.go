package web

import (
	"bytes"
	"encoding/json"
	"fmt"
	"strconv"
	"strings"
	"unicode/utf8"

	"github.com/Maoyangui/m-ui/database/model"
	"github.com/Maoyangui/m-ui/ha"
	"github.com/Maoyangui/m-ui/hop"
	"github.com/Maoyangui/m-ui/render"
	"gorm.io/gorm"
)

// 线路的中转入口(Line.Forwards,见 model.LineForward)保存前的规整与校验,以及和端口校验的互相避让。
//
// 端口:集群中转的端口开在中转服务器上,和那台的面板 / 订阅 / 代理面板端口、部署在那台的线路(包括这条线路自己)、
// 别的中转入口、那台 hysteria2 线路的端口跳跃范围(转 UDP 的)都不能撞;反过来,线路改端口 / 改部署范围时也要避开
// 部署范围里各台服务器上已有的中转入口。外部中转的端口在别人的机器上,不查。

// forwardRef 库里某条线路的一个集群中转入口(只列 Via ≠ 0 的)。
type forwardRef struct {
	LineId   uint
	LineName string
	Via      uint
	Port     int
	Network  string
}

// forwardRefs 库里全部线路(不含 skip 这条)的集群中转入口;暂停的也算 —— 端口留给它,恢复时不用再挑。
func (s *Server) forwardRefs(skip uint) []forwardRef {
	var lines []model.Line
	s.db.Select("id, name, protocol, transport, forwards").Where("id <> ? AND forwards IS NOT NULL AND forwards <> '' AND forwards <> 'null'", skip).Find(&lines)
	var out []forwardRef
	for _, l := range lines {
		for _, f := range model.ParseForwards(l.Forwards) {
			if f.Via != 0 {
				out = append(out, forwardRef{l.Id, l.Name, f.Via, f.Port, render.ForwardNetwork(l)})
			}
		}
	}
	return out
}

// servicePortsOf 一台服务器自己监听的服务端口:本机按本机设置;副机按它上报的;没上报的按安装默认值 + 主机自己的端口
// (和 checkServicePorts 同一个保守口径)。
func (s *Server) servicePortsOf(n model.Node) []model.ServicePort {
	var out []model.ServicePort
	local := func() {
		for _, p := range s.localServicePorts() {
			out = append(out, model.ServicePort{Label: p.label, Port: p.port})
		}
	}
	if n.IsLocal {
		local()
		return out
	}
	if len(n.ServicePorts) == 0 || json.Unmarshal(n.ServicePorts, &out) != nil || len(out) == 0 {
		out = append([]model.ServicePort{}, defaultServicePorts...)
		local()
	}
	return out
}

// normForwards 规整并校验 line(要保存的新状态)的中转入口,回写规整后的;没有就存空。
// old 是库里原来的这条线路(新建为 nil),用来判断本机上的中转端口是不是新占用的(新占用的要试着监听一次)。
func (s *Server) normForwards(line *model.Line, old *model.Line) error {
	raw := bytes.TrimSpace(line.Forwards)
	if len(raw) == 0 || string(raw) == "null" || string(raw) == "[]" {
		line.Forwards = nil
		return nil
	}
	var fs []model.LineForward
	if err := json.Unmarshal(raw, &fs); err != nil {
		return fmt.Errorf("中转入口的格式不对: %w", err)
	}
	if len(fs) == 0 {
		line.Forwards = nil
		return nil
	}
	if len(fs) > model.MaxForwards {
		return fmt.Errorf("一条线路最多 %d 个中转入口", model.MaxForwards)
	}
	var nodes []model.Node
	s.db.Order("sort asc, id asc").Find(&nodes)
	byId := make(map[uint]model.Node, len(nodes))
	var deployed []uint
	for _, n := range nodes {
		byId[n.Id] = n
		if render.LineOnNode(*line, n.Id) {
			deployed = append(deployed, n.Id)
		}
	}
	had := map[string]bool{} // 原来就有的「中转服务器:端口」:端口没变的不再试监听(占着它的正是本机运行中的数据面)
	if old != nil {
		for _, f := range model.ParseForwards(old.Forwards) {
			if f.Managed() { // 原来是「已有转发」的不算:那时面板没占这个端口
				had[fmt.Sprintf("%d:%d", f.Via, f.Port)] = true
			}
		}
	}
	refs := s.forwardRefs(line.Id)
	seen := map[string]bool{}
	for i := range fs {
		f, no := &fs[i], i+1
		f.Name = strings.TrimSpace(f.Name)
		if utf8.RuneCountInString(f.Name) > 40 {
			return fmt.Errorf("第 %d 个中转入口的名字太长(最多 40 个字)", no)
		}
		if f.Port < 1 || f.Port > 65535 {
			return fmt.Errorf("第 %d 个中转入口的端口需在 1-65535 之间", no)
		}
		src, err := model.NormForwardSrc(f.Src)
		if err != nil {
			return fmt.Errorf("第 %d 个中转入口的来源 IP 不对: %w", no, err)
		}
		f.Src = src
		if f.To == 0 {
			if len(deployed) != 1 {
				return fmt.Errorf("第 %d 个中转入口没选转给哪台服务器", no)
			}
			f.To = deployed[0]
		}
		exit, ok := byId[f.To]
		if !ok {
			return fmt.Errorf("第 %d 个中转入口转给的服务器已经不在了,删掉它或换一台", no)
		}
		if !render.LineOnNode(*line, f.To) {
			return fmt.Errorf("第 %d 个中转入口转给「%s」,但这条线路没部署在那台", no, exit.Name)
		}
		if f.Via == 0 { // 别处的中转:只进订阅
			f.Host, f.Target, f.Manual = model.BareHost(f.Host), "", false
			if f.Host == "" {
				return fmt.Errorf("第 %d 个中转入口没填中转地址", no)
			}
			if !model.ValidAddr(f.Host) {
				return fmt.Errorf("第 %d 个中转入口的地址 %q 不对:只填 IP 或域名,不带端口、协议头和路径", no, f.Host)
			}
			key := fmt.Sprintf("x:%s:%d", strings.ToLower(f.Host), f.Port)
			if seen[key] {
				return fmt.Errorf("第 %d 个中转入口和前面的一个重复了", no)
			}
			seen[key] = true
			continue
		}
		via, ok := byId[f.Via]
		if !ok {
			return fmt.Errorf("第 %d 个中转入口的中转服务器已经不在了,删掉它或换一台", no)
		}
		if f.Via == f.To {
			return fmt.Errorf("第 %d 个中转入口的中转服务器和出口是同一台「%s」", no, via.Name)
		}
		f.Host, f.Target = "", model.BareHost(f.Target)
		if f.Manual {
			f.Target = "" // 那台上已有的转发转到哪里是那边配的,这里不存
		}
		if f.Target != "" && !model.ValidAddr(f.Target) {
			return fmt.Errorf("第 %d 个中转入口的转到地址 %q 不对:只填 IP 或域名,不带端口(端口就是线路的端口)", no, f.Target)
		}
		key := fmt.Sprintf("%d:%d", f.Via, f.Port)
		if seen[key] {
			return fmt.Errorf("第 %d 个中转入口和前面的一个在「%s」上用了同一个端口 %d", no, via.Name, f.Port)
		}
		seen[key] = true
		if err := s.forwardPortFree(*line, *f, via, refs); err != nil {
			return fmt.Errorf("第 %d 个中转入口:%w", no, err)
		}
		// 面板来转的、新占用的端口:让那台服务器自己试着监听一次(本机直接试,副机经 agent/port-check;问不到不拦)。
		// 「已有转发」的不试 —— 占着端口的正是那个转发
		if f.Managed() && !had[key] {
			if by, busy := s.portBusyOn(via, f.Port, render.ForwardNetwork(*line)); busy {
				return fmt.Errorf("第 %d 个中转入口:端口 %d 在「%s」上已被别的程序占用%s。换一个端口;如果那正是你给这条线路做的转发,把这个入口改成「这台上已有转发」,面板就不去占这个端口", no, f.Port, via.Name, busyWho(by))
			}
		}
	}
	// 新加的入口补编号(节点设置、用户不要哪些节点按编号认它)。编号从库里原来用过的最大值往上取:
	// 这一次保存里「删掉 2 号、再加一个」时新入口不能又叫 2 号,否则会把 2 号留下的节点设置和用户的选择接过去
	floor := 0
	if old != nil {
		for _, f := range model.ParseForwards(old.Forwards) {
			if f.Id > floor {
				floor = f.Id
			}
		}
	}
	model.AssignForwardIds(fs, floor)
	b, err := json.Marshal(fs)
	if err != nil {
		return err
	}
	line.Forwards = b
	return nil
}

// lineNodeKeys 这条线路现在有哪些节点(键见 model.DirectKey / model.ForwardKey):部署到的每台服务器一个直连节点,每个中转入口一个。
func (s *Server) lineNodeKeys(line model.Line) map[string]bool {
	var nodes []model.Node
	s.db.Select("id").Find(&nodes)
	keys := map[string]bool{}
	for _, n := range nodes {
		if render.LineOnNode(line, n.Id) {
			keys[model.DirectKey(n.Id)] = true
		}
	}
	for _, f := range model.ParseForwards(line.Forwards) {
		if f.Id > 0 {
			keys[model.ForwardKey(f.Id)] = true
		}
	}
	return keys
}

// normNodeOpts 规整线路的节点设置(Line.NodeOpts):只留这条线路现有节点的键,什么都没设的键去掉;全空就存空。
// 要在 normForwards 之后调(入口编号已经补齐)。
func (s *Server) normNodeOpts(line *model.Line) error {
	raw := bytes.TrimSpace(line.NodeOpts)
	if len(raw) == 0 || string(raw) == "null" || string(raw) == "{}" {
		line.NodeOpts = nil
		return nil
	}
	var in map[string]model.NodeOpt
	if err := json.Unmarshal(raw, &in); err != nil {
		return fmt.Errorf("节点设置的格式不对: %w", err)
	}
	valid := s.lineNodeKeys(*line)
	out := map[string]model.NodeOpt{}
	for k, v := range in {
		if !valid[k] {
			continue
		}
		if err := v.Check(); err != nil {
			return err
		}
		if v != (model.NodeOpt{}) {
			out[k] = v
		}
	}
	if len(out) == 0 {
		line.NodeOpts = nil
		return nil
	}
	b, err := json.Marshal(out) // 键按字典序输出:同样的设置总是同样的字节(快照比对靠它)
	if err != nil {
		return err
	}
	line.NodeOpts = b
	return nil
}

// pruneLineHidesTx 清掉用户在这条线路上「不要」的、但已经不存在的节点(valid = 现有节点的键)。
func pruneLineHidesTx(tx *gorm.DB, lineID uint, valid map[string]bool) error {
	var rows []model.UserLineHide
	if err := tx.Where("line_id = ?", lineID).Find(&rows).Error; err != nil {
		return err
	}
	for _, r := range rows {
		if valid[r.Key] {
			continue
		}
		if err := tx.Where("user_id = ? AND line_id = ? AND node_key = ?", r.UserId, lineID, r.Key).Delete(&model.UserLineHide{}).Error; err != nil {
			return err
		}
	}
	return nil
}

// forwardPortFree 集群中转入口 f 的端口在中转服务器 via 上空着(refs 是别的线路的集群中转入口)。
func (s *Server) forwardPortFree(line model.Line, f model.LineForward, via model.Node, refs []forwardRef) error {
	for _, p := range s.servicePortsOf(via) {
		if p.Port == f.Port {
			return fmt.Errorf("端口 %d 是服务器「%s」的%s端口,换一个", f.Port, via.Name, p.Label)
		}
	}
	if h := s.haSvc(); h != nil && !via.IsLocal && render.ForwardNetwork(line) != "udp" { // 开着主机容灾:避开它接任后要用的端口
		for _, p := range h.TakeoverPorts(via.Id) {
			if p.Port != f.Port {
				continue
			}
			if p.What == ha.TakeoverPanel {
				return fmt.Errorf("开着主机容灾、全量接管:端口 %d 是主机的面板端口,服务器「%s」接任后要用它提供面板和外部 API,换一个", f.Port, via.Name)
			}
			if p.What == ha.TakeoverSub {
				return fmt.Errorf("开着主机容灾、全量接管:端口 %d 是订阅端口,服务器「%s」接任后要用它提供订阅,换一个", f.Port, via.Name)
			}
			return fmt.Errorf("开着主机容灾:端口 %d 是服务器「%s」的代理面板端口,它接任后要用,换一个", f.Port, via.Name)
		}
	}
	if render.LineOnNode(line, via.Id) && line.Port == f.Port {
		return fmt.Errorf("这条线路自己在「%s」上就用着端口 %d,换一个", via.Name, f.Port)
	}
	var others []model.Line
	s.db.Select("id, name, port, node_ids").Where("port = ? AND id <> ?", f.Port, line.Id).Find(&others)
	for _, o := range others {
		if render.LineOnNode(o, via.Id) {
			return fmt.Errorf("端口 %d 在「%s」上已被线路「%s」用着,换一个", f.Port, via.Name, o.Name)
		}
	}
	for _, r := range refs {
		if r.Via == via.Id && r.Port == f.Port {
			return fmt.Errorf("端口 %d 在「%s」上已被线路「%s」的中转入口用着,换一个", f.Port, via.Name, r.LineName)
		}
	}
	if render.ForwardNetwork(line) != "tcp" { // 转 UDP 的不能落进那台 hysteria2 线路的端口跳跃范围(那段 UDP 被整段转走)
		var hy []model.Line
		s.db.Where("protocol = ? AND id <> ?", "hysteria2", line.Id).Find(&hy)
		if line.Protocol == "hysteria2" {
			hy = append(hy, line)
		}
		for _, o := range hy {
			var oo struct {
				PortHopping string `json:"port_hopping"`
			}
			if json.Unmarshal(o.Options, &oo) != nil || oo.PortHopping == "" || !render.LineOnNode(o, via.Id) {
				continue
			}
			if a, b, err := hop.ParseRange(oo.PortHopping); err == nil && f.Port >= a && f.Port <= b {
				return fmt.Errorf("端口 %d 落在线路「%s」在「%s」上的端口跳跃范围 %s 里,UDP 包会被转给那条线路,换一个", f.Port, o.Name, via.Name, oo.PortHopping)
			}
		}
	}
	return nil
}

// nodeLabel 服务器名(找不到写编号)。
func nodeLabel(nodes []model.Node, id uint) string {
	for _, n := range nodes {
		if n.Id == id {
			return n.Name
		}
	}
	return "#" + strconv.Itoa(int(id))
}

// forwardOnLinePort 线路 line(新状态)部署范围里的服务器上,有没有别的线路的中转入口占着它的端口。
func (s *Server) forwardOnLinePort(line model.Line, refs []forwardRef, nodes []model.Node) error {
	for _, r := range refs {
		if r.Port == line.Port && render.LineOnNode(line, r.Via) {
			return fmt.Errorf("端口 %d 在「%s」上已被线路「%s」的中转入口用着,换一个", line.Port, nodeLabel(nodes, r.Via), r.LineName)
		}
	}
	return nil
}

// forwardsOutOfScope 线路改了部署范围后,中转入口转给的服务器不在新范围里、或中转服务器落进新范围且和线路端口相同。
func forwardsOutOfScope(line model.Line, nodes []model.Node) error {
	for i, f := range model.ParseForwards(line.Forwards) {
		if !render.LineOnNode(line, f.To) {
			return fmt.Errorf("线路「%s」的第 %d 个中转入口转给「%s」,改完后那台不再部署这条线路;先在线路里改掉这个中转入口", line.Name, i+1, nodeLabel(nodes, f.To))
		}
		if f.Via != 0 && f.Port == line.Port && render.LineOnNode(line, f.Via) {
			return fmt.Errorf("线路「%s」改完后也部署到「%s」,和它经那台的中转入口撞了端口 %d", line.Name, nodeLabel(nodes, f.Via), f.Port)
		}
	}
	return nil
}

// pruneForwardsTx 删除服务器时,去掉所有线路里经这台中转、或转给这台的中转入口;返回改动了的线路名。
func pruneForwardsTx(tx *gorm.DB, nodeId uint) ([]string, error) {
	var lines []model.Line
	if err := tx.Select("id, name, forwards").Where("forwards IS NOT NULL AND forwards <> '' AND forwards <> 'null'").Find(&lines).Error; err != nil {
		return nil, err
	}
	var changed []string
	for _, l := range lines {
		fs := model.ParseForwards(l.Forwards)
		keep := fs[:0]
		for _, f := range fs {
			if f.Via != nodeId && f.To != nodeId {
				keep = append(keep, f)
			}
		}
		if len(keep) == len(fs) {
			continue
		}
		var v interface{}
		if len(keep) > 0 {
			b, err := json.Marshal(keep)
			if err != nil {
				return nil, err
			}
			v = json.RawMessage(b)
		}
		if err := tx.Model(&model.Line{}).Where("id = ?", l.Id).Update("forwards", v).Error; err != nil {
			return nil, err
		}
		changed = append(changed, l.Name)
	}
	return changed, nil
}
