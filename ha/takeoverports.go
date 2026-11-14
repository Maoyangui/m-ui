package ha

import (
	"sort"
	"strconv"
	"strings"

	"github.com/Maoyangui/m-ui/database/model"
	"github.com/Maoyangui/m-ui/render"
)

// 副机接任(或被设为主机)后要用、现在就得留着的端口(用户 11-15:开了主机容灾,副机的端口要避开,不然接任时冲突)。
// 面板、代理面板的端口是每台机器自己的设置,接任后新主机用的是它自己的端口(见 settings.go 的 localKeys),所以:
//   - 它自己的代理面板端口:副机平时不监听,接任后才起 —— 线路占着就起不来;
//   - 选了「连订阅一起接管」时主机的订阅端口:随主机身份迁过去 —— 线路占着就只能退回它自己的订阅端口,用户的订阅地址会变。
// 主机自己的面板端口不迁过去,不留(不然开了容灾,副机上连 443 这样常用的端口都不能给线路用)。
// 只走 UDP 的线路(hysteria2、tuic)和这些 TCP 服务不冲突。

// 留端口的原因(TakeoverPort.What)。
const (
	TakeoverReseller = "reseller" // 它接任后的代理面板
	TakeoverSub      = "sub"      // 连订阅一起接管时迁过去的订阅
)

// TakeoverPort 一台副机接任后要用的一个端口。
type TakeoverPort struct {
	Port int    `json:"port"`
	What string `json:"what"`
}

// TakeoverPorts 开着主机容灾时,副机 id 接任后要用的端口(没开、本机是副机时为空)。它的代理面板端口看它上报的
// (还没上报过就不知道,不留);订阅端口按本机现在的设置(cfg 给了就按它,SaveConfig 用来检查将要保存的设置)。
func (s *Service) TakeoverPorts(id uint) []TakeoverPort {
	return s.takeoverPortsWith(s.Config(), id)
}

func (s *Service) takeoverPortsWith(c Config, id uint) []TakeoverPort {
	if c.Mode == ModeOff || s.IsNode() {
		return nil
	}
	var out []TakeoverPort
	if !strings.EqualFold(strings.TrimSpace(s.setting("resellerEnabled")), "false") && s.d.Hub != nil {
		if st, ok := s.d.Hub.StatusOf(id); ok && st.HA != nil && st.HA.ResellerPort > 0 {
			out = append(out, TakeoverPort{Port: st.HA.ResellerPort, What: TakeoverReseller})
		}
	}
	if c.TakeSub {
		out = append(out, TakeoverPort{Port: s.settingInt("subPort", 2056, 1, 65535), What: TakeoverSub})
	}
	return out
}

// PortConflict 一条线路在一台副机上占着那台接任后要用的端口。
type PortConflict struct {
	Line, Node string
	Port       int
	What       string
}

// String「线路@服务器:端口」(中英文界面都能直接显示)。
func (p PortConflict) String() string {
	return p.Line + "@" + p.Node + ":" + strconv.Itoa(p.Port)
}

// takeoverConflicts 现有启用的线路里,占着副机接任后要用的端口的(按 c 算;只看 TCP 的)。
func (s *Service) takeoverConflicts(c Config) []PortConflict {
	if c.Mode == ModeOff || s.IsNode() {
		return nil
	}
	var lines []model.Line
	s.d.DB.Order("sort asc, id asc").Find(&lines) // 停用的线路也算:端口留给它,启用时不该才发现冲突
	var out []PortConflict
	for _, n := range s.remoteNodes() {
		ports := s.takeoverPortsWith(c, n.Id)
		if len(ports) == 0 {
			continue
		}
		for _, l := range lines {
			if render.UDPOnly(l.Protocol) {
				continue
			}
			for _, p := range ports {
				if render.LineOnNode(l, n.Id) && l.Port == p.Port {
					out = append(out, PortConflict{Line: l.Name, Node: n.Name, Port: p.Port, What: p.What})
				}
				for _, f := range model.ParseForwards(l.Forwards) { // 这条线路开在这台副机上的中转入口
					if f.Via == n.Id && f.Port == p.Port && render.ForwardNetwork(l) != "udp" {
						out = append(out, PortConflict{Line: l.Name, Node: n.Name, Port: p.Port, What: p.What})
					}
				}
			}
		}
	}
	sort.SliceStable(out, func(i, j int) bool { return out[i].String() < out[j].String() })
	return out
}

// conflictList 冲突的前几条拼成一句(太多的写「等 N 条」)。
func conflictList(cs []PortConflict) string {
	const max = 5
	var parts []string
	for i, c := range cs {
		if i == max {
			parts = append(parts, "… +"+strconv.Itoa(len(cs)-max))
			break
		}
		parts = append(parts, c.String())
	}
	return strings.Join(parts, ", ")
}

// SubPortConflicts 开着主机容灾、连订阅一起接管时,把本机的订阅端口改成 port:副机上占着它的线路(只看 TCP 的)。
// 设置页改订阅端口时拦(订阅端口随主机身份迁到接任的那台,线路占着就起不来)。
func (s *Service) SubPortConflicts(port int) []PortConflict {
	c := s.Config()
	if c.Mode == ModeOff || !c.TakeSub || s.IsNode() || port <= 0 {
		return nil
	}
	var lines []model.Line
	s.d.DB.Order("sort asc, id asc").Find(&lines)
	var out []PortConflict
	for _, n := range s.remoteNodes() {
		for _, l := range lines {
			if render.UDPOnly(l.Protocol) {
				continue
			}
			if l.Port == port && render.LineOnNode(l, n.Id) {
				out = append(out, PortConflict{Line: l.Name, Node: n.Name, Port: port, What: TakeoverSub})
			}
			for _, f := range model.ParseForwards(l.Forwards) {
				if f.Via == n.Id && f.Port == port && render.ForwardNetwork(l) != "udp" {
					out = append(out, PortConflict{Line: l.Name, Node: n.Name, Port: port, What: TakeoverSub})
				}
			}
		}
	}
	return out
}

// ConflictList 冲突的前几条拼成一句(给 web 包报错用)。
func ConflictList(cs []PortConflict) string { return conflictList(cs) }
