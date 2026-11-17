package ha

import (
	"fmt"
	"net"
	"sort"
	"strconv"
	"strings"
	"sync"
	"time"

	"github.com/Maoyangui/m-ui/hub"
	"github.com/Maoyangui/m-ui/logger"
	"github.com/Maoyangui/m-ui/notify"
)

// 备用上的端口提前自检(用户 11-15:「这些端口 m-ui 自己的线路会避开,但可能被别的程序占着」)。
// 主机在推给备用的容灾状态里带上它接任后要用的端口(TakeoverPorts:它自己的代理面板端口,全量接管时还有主机的
// 订阅、面板、代理面板端口);备用每 10 分钟试着绑一下(只试 TCP、马上关掉;自己的面板、订阅正监听着的端口不算),
// 被别的程序占着的随报告报上来。主机的就绪检查列出来、Telegram 告一次 —— 不等真接任才发现。
// 真到了接任那一刻还是被占:先退回新主机自己原来的端口,还不行自动挑一个空闲端口顶上(见 FreePortFallback),
// 通知里写清实际地址、提醒放行云防火墙。

const portCheckEvery = 10 * time.Minute

type portCheckState struct {
	mu   sync.Mutex
	at   time.Time
	key  string
	busy []hub.BusyPort
}

var portChecks sync.Map // *Service → *portCheckState

func (s *Service) portCheckState() *portCheckState {
	v, _ := portChecks.LoadOrStore(s, &portCheckState{})
	return v.(*portCheckState)
}

// bindTest 端口能不能绑(TCP,所有地址);能绑说明空着(马上关掉)。
var bindTest = func(port int) bool {
	ln, err := net.Listen("tcp", ":"+strconv.Itoa(port))
	if err != nil {
		return false
	}
	_ = ln.Close()
	return true
}

// portCheckTick 备用上:到了时间、或要检查的端口变了,就试一遍主机给的端口。
func (s *Service) portCheckTick() {
	if !s.IsNode() {
		return
	}
	st, ok := s.NodeState()
	h := s.portCheckState()
	if !ok || st.Mode == ModeOff || len(st.Ports) == 0 {
		h.mu.Lock()
		h.at, h.key, h.busy = time.Time{}, "", nil
		h.mu.Unlock()
		return
	}
	own := map[int]bool{s.settingInt("webPort", 2053, 1, 65535): true, s.settingInt("subPort", 2056, 1, 65535): true} // 自己正监听的
	key := fmt.Sprint(st.Ports, own)
	h.mu.Lock()
	fresh := h.key == key && !h.at.IsZero() && s.now().Sub(h.at) < portCheckEvery
	h.mu.Unlock()
	if fresh {
		return
	}
	var busy []hub.BusyPort
	for _, p := range st.Ports {
		if p.Port <= 0 || own[p.Port] {
			continue
		}
		if !bindTest(p.Port) {
			busy = append(busy, hub.BusyPort{Port: p.Port, What: p.What})
		}
	}
	h.mu.Lock()
	h.at, h.key, h.busy = s.now(), key, busy
	h.mu.Unlock()
	if len(busy) > 0 {
		logger.Warning("容灾:接任后要用的端口在本机被别的程序占着:", busy)
	}
}

// busyPorts 备用最近一次自检查到的被占端口(随报告上报)。
func (s *Service) busyPorts() []hub.BusyPort {
	h := s.portCheckState()
	h.mu.Lock()
	defer h.mu.Unlock()
	return append([]hub.BusyPort(nil), h.busy...)
}

// busyList 主机上:各备用报上来的被占端口,「备用名:端口(用途)」(就绪检查、告警用)。
func busyList(infos []StandbyInfo) []string {
	var out []string
	for _, i := range infos {
		if i.Rank == 0 && !i.Eligible {
			continue
		}
		for _, b := range i.BusyPorts {
			out = append(out, i.Name+":"+strconv.Itoa(b.Port))
		}
	}
	sort.Strings(out)
	return out
}

// alertBusyPorts 主机上:备用报上来新的被占端口时告一次(同一台、同一组端口不重复)。
func (s *Service) alertBusyPorts(infos []StandbyInfo) {
	c := s.core()
	for _, i := range infos {
		if i.Rank == 0 || len(i.BusyPorts) == 0 {
			continue
		}
		var parts []string
		for _, b := range i.BusyPorts {
			parts = append(parts, strconv.Itoa(b.Port)+"("+busyWhat(b.What)+")")
		}
		key := strconv.FormatUint(uint64(i.Id), 10) + ":" + strings.Join(parts, ",")
		c.mu.Lock()
		done := c.busyAlerted[key]
		c.busyAlerted[key] = true
		c.mu.Unlock()
		if done {
			continue
		}
		s.notify("🟠 <b>备用上有端口被别的程序占着</b>:「" + notify.Esc(i.Name) + "」上 " + notify.Esc(strings.Join(parts, "、")) +
			"\n它接任后要用这些端口:到时会先退回它原来的端口,还不行自动挑一个空闲端口,订阅 / 面板的地址会变。到那台上执行 ss -ltnp 看是哪个程序占着,停掉或换掉它")
	}
}

func busyWhat(w string) string {
	switch w {
	case TakeoverSub:
		return "订阅"
	case TakeoverPanel:
		return "面板"
	case TakeoverReseller:
		return "代理面板"
	}
	return w
}

// FreePortFallback 刚接任 / 被设为主机(第一次启动、还没发通知)时,迁来的端口和本机原来的端口都绑不上:
// 记下改用空闲端口 to(svc:"sub" | "panel" | "reseller"),写进成为主机的告警;返回要不要改(不是刚成为主机时不改)。
func (s *Service) FreePortFallback(svc string, from, to int, cause error) bool {
	if s.IsNode() || to <= 0 {
		return false
	}
	var b Became
	if !s.getJSON(KeyBecame, &b) || b.Notified {
		return false
	}
	key := map[string]string{"sub": "subPort", "panel": "webPort", "reseller": "resellerPort"}[svc]
	if key == "" {
		return false
	}
	logger.Warning("容灾:", busyWhat(map[string]string{"sub": TakeoverSub, "panel": TakeoverPanel, "reseller": TakeoverReseller}[svc]),
		"端口 ", from, " 和本机原来的都绑不上(", cause, "),临时改用空闲端口 ", to)
	b.Identity.Warnings = append(b.Identity.Warnings, item(idwPortAuto, "svc", svc, "port", strconv.Itoa(from), "to", strconv.Itoa(to), "err", errText(cause)))
	switch svc {
	case "sub":
		b.Identity.SubURLKept = false
	case "panel":
		b.Identity.PanelKept = false
	case "reseller":
		b.Identity.ResellerKept = false
	}
	return s.set(map[string]string{key: strconv.Itoa(to), KeyBecame: mustJSON(b)}) == nil
}

// JustBecame 本机刚成为主机(第一次启动、成为主机的通知还没发):启动时端口绑不上才自动改。
func (s *Service) JustBecame() bool {
	if s.IsNode() {
		return false
	}
	var b Became
	return s.getJSON(KeyBecame, &b) && !b.Notified
}

const idwPortAuto = "portAuto" // 迁来的端口和本机原来的都被别的程序占着,临时改用空闲端口:{svc, port, to, err}
