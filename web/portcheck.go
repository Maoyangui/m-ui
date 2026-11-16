package web

import (
	"encoding/json"
	"fmt"
	"net/http"
	"sync"
	"time"

	"github.com/Maoyangui/m-ui/database/model"
	"github.com/Maoyangui/m-ui/render"
	"github.com/Maoyangui/m-ui/safe"
	"github.com/Maoyangui/m-ui/util/portprobe"
)

// 端口占用探测。
//
// 线路和面板转发的中转入口都要在某台服务器上监听端口。以前只在本机试监听(portBindable),副机上那个端口被别的程序占着
// (最常见:中转机上已经跑着 realm / gost)要等配置推过去、数据面启动失败才知道。现在保存时让要开端口的那台自己探一次:
// 本机直接探,副机经 agent/port-check。问不到(离线、老版本没有这个接口、超时)不拦保存 —— 推过去真监听不上,
// 那台只摘掉这一个入站、照常应用其余的,并把它报回来(见 runner/blocked.go)。

// portCheckMax 一次最多问几个端口(防止被当成端口扫描器用)。
const portCheckMax = 64

// handleAgentPortCheck 副机:主机问这些端口现在能不能监听。占着它的是本机运行中的数据面 / 面板时 Self = true。
func (s *Server) handleAgentPortCheck(w http.ResponseWriter, r *http.Request) {
	if r.Method != http.MethodPost {
		writeJSON(w, http.StatusMethodNotAllowed, map[string]string{"error": "方法不允许"})
		return
	}
	var req struct {
		Ports []model.PortProbe `json:"ports"`
	}
	if err := json.NewDecoder(http.MaxBytesReader(w, r.Body, 64<<10)).Decode(&req); err != nil {
		writeJSON(w, http.StatusBadRequest, map[string]string{"error": "请求格式错误"})
		return
	}
	if len(req.Ports) > portCheckMax {
		writeJSON(w, http.StatusBadRequest, map[string]string{"error": fmt.Sprintf("一次最多问 %d 个端口", portCheckMax)})
		return
	}
	out := make([]model.PortProbeResult, 0, len(req.Ports))
	for _, p := range req.Ports {
		out = append(out, portprobe.Probe(p))
	}
	writeJSON(w, http.StatusOK, map[string]interface{}{"results": out})
}

// portBusyOn 端口 port(network: tcp / udp / 空 = 两种)在服务器 n 上是不是被别的程序占着;by 是占着它的程序名(查得到的话)。
// 本机直接探;副机问它自己,问不到一律当没占(不拦保存)。占着它的是那台自己的 m-ui(运行中的数据面)不算。
func (s *Server) portBusyOn(n model.Node, port int, network string) (by string, busy bool) {
	res, asked := s.probePortOn(n, port, network)
	if !asked || res.Free || res.Self {
		return "", false
	}
	return res.By, true
}

// probePortOn 让服务器 n 自己探一次端口。asked = false 是没问到(副机离线 / 停用 / 老版本没有这个接口 / 超时)。
func (s *Server) probePortOn(n model.Node, port int, network string) (res model.PortProbeResult, asked bool) {
	if n.IsLocal {
		return portprobe.Probe(model.PortProbe{Port: port, Net: network}), true
	}
	if s.run == nil || s.run.Hub() == nil || !n.Enabled {
		return res, false
	}
	got, err := s.run.Hub().PortCheck(n, []model.PortProbe{{Port: port, Net: network}}, 4*time.Second)
	if err != nil || len(got) != 1 {
		return res, false
	}
	return got[0], true
}

// handleNodePortCheck POST nodes/{id}/port-check {port, net}:编辑中转入口时现问这台服务器这个端口空不空
// (net: tcp / udp / 空 = 两种都要)。返回 {asked, free, self, by}:asked = false 是没问到,界面不下结论;
// self = 占着它的是那台自己的 m-ui(这个入口本来就开着);by = 占着它的别的程序名(查得到的话,比如 realm)。
func (s *Server) handleNodePortCheck(w http.ResponseWriter, r *http.Request, n model.Node) {
	if r.Method != http.MethodPost {
		writeJSON(w, http.StatusMethodNotAllowed, map[string]string{"error": "方法不允许"})
		return
	}
	var req model.PortProbe
	if err := json.NewDecoder(http.MaxBytesReader(w, r.Body, 4<<10)).Decode(&req); err != nil {
		writeJSON(w, http.StatusBadRequest, map[string]string{"error": "请求格式错误"})
		return
	}
	if req.Port < 1 || req.Port > 65535 {
		writeJSON(w, http.StatusBadRequest, map[string]string{"error": "端口需在 1-65535 之间"})
		return
	}
	if req.Net != "tcp" && req.Net != "udp" {
		req.Net = ""
	}
	res, asked := s.probePortOn(n, req.Port, req.Net)
	writeJSON(w, http.StatusOK, map[string]interface{}{"asked": asked, "free": !asked || res.Free, "self": res.Self, "by": res.By})
}

// busyWho 报错里写占用端口的程序:"(realm)";不知道是谁就不写。
func busyWho(by string) string {
	if by == "" {
		return ""
	}
	return "(" + by + ")"
}

// linePortBusyRemote 线路 line(要保存的新状态)新占用的端口,在部署范围里的副机上有没有被别的程序占着。
// 新占用 = 新建、改了端口、或这次才部署到那台。本机在 validateLine 里另查(portBindable);几台副机并发问,总共最多等 4 秒。
func (s *Server) linePortBusyRemote(line *model.Line) error {
	var cur *model.Line
	if line.Id != 0 {
		var c model.Line
		if s.db.Select("id, port, node_ids").First(&c, line.Id).Error == nil {
			cur = &c
		}
	}
	var nodes []model.Node
	s.db.Where("is_local = ? AND enabled = ?", false, true).Order("sort asc, id asc").Find(&nodes)
	network := "tcp"
	if line.Protocol == "hysteria2" || line.Protocol == "tuic" {
		network = "udp"
	} else if line.Protocol == "shadowsocks" {
		network = ""
	}
	type hit struct {
		name, by string
	}
	var (
		wg   sync.WaitGroup
		mu   sync.Mutex
		hits []hit
	)
	for _, n := range nodes {
		if !render.LineOnNode(*line, n.Id) {
			continue
		}
		if cur != nil && cur.Port == line.Port && render.LineOnNode(*cur, n.Id) {
			continue // 端口没变、原来就部署在这台:占着它的正是那台运行中的数据面
		}
		wg.Add(1)
		safe.Go1("web.linePortBusyRemote", n, func(n model.Node) {
			defer wg.Done()
			if by, busy := s.portBusyOn(n, line.Port, network); busy {
				mu.Lock()
				hits = append(hits, hit{n.Name, by})
				mu.Unlock()
			}
		})
	}
	wg.Wait()
	if len(hits) == 0 {
		return nil
	}
	h := hits[0]
	return fmt.Errorf("端口 %d 在服务器「%s」上已被别的程序占用%s:换一个端口,或先在那台上停掉占用它的程序", line.Port, h.name, busyWho(h.by))
}
