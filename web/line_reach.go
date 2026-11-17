package web

import (
	"context"
	"errors"
	"net/http"
	"net/netip"
	"strconv"
	"strings"
	"sync"
	"time"

	"github.com/Maoyangui/m-ui/database/model"
	"github.com/Maoyangui/m-ui/reach"
	"github.com/Maoyangui/m-ui/safe"
	"github.com/Maoyangui/m-ui/sub"
)

// 线路详情里的「测大陆连通」:从大陆三网的家宽测点连这条线路在订阅里给用户的每个地址(地址 + 端口),香港测点作对照。
//
// 和服务器页的检测是两回事:那边每台服务器挑一个端口定时巡检、记历史、会告警;这里是点一下,测这一条线路自己的端口
// (用户实际连的那个),不进历史、不告警。两边共用同一份测点额度,同一时间只跑一个。
//
//	POST /lines/{id}/reach        开始 → {job, items}
//	GET  /lines/{id}/reach/{job}  进度和结果 → {done, total, finished, items}
//
// 测点能做的只有 TCP 握手和 ping:
//   - TCP 的线路(vless、trojan、anytls、shadowsocks…):连端口,测的就是用户走的那条路;
//   - UDP 的线路(hysteria2、tuic):测点连不了 UDP 端口,只 ping 地址。能看出地址通不通,看不出端口或 QUIC 被针对,结果里写明;
//   - 走 IPv6 的地址:按 IPv6 入站那一套测(大陆有 IPv6 的测点少,没有时用境外测点并注明);UDP 又走 IPv6 的测不了。

// lineReachChecker 测点服务里这里用到的那几样(测试里换成假的)。
type lineReachChecker interface {
	CanCheckExt(n int) error
	BeginExt() (func(), error)
	CheckExt(ctx context.Context, name, host string, port int, udp bool) reach.Result
	CheckV6(ctx context.Context, host string, port int) *reach.V6Result
}

// lineReachSvc 取测点服务;没有(副机、没启用)返回 nil。测试里替换。
var lineReachSvc = func(s *Server) lineReachChecker {
	if s.run == nil || s.run.Reach() == nil {
		return nil
	}
	return s.run.Reach()
}

// lineReachGroup 一家运营商(或香港对照)的小结:界面上一个小标。各测点的明细不带,这里要轻。
type lineReachGroup struct {
	Key   string  `json:"key"`   // cm / ct / cu / hk
	State string  `json:"state"` // ok / weak / down / none
	OK    int     `json:"ok"`
	Total int     `json:"total"`
	AvgMs float64 `json:"avgMs,omitempty"`
}

// lineReachItem 一个地址的结果。
type lineReachItem struct {
	Label  string `json:"label"`           // 服务器名、「出口-中转」或自定义地址的备注
	Target string `json:"target"`          // 地址
	Port   int    `json:"port"`            // 端口
	Relay  bool   `json:"relay,omitempty"` // 中转入口
	UDP    bool   `json:"udp,omitempty"`   // UDP 协议:只 ping 了地址
	V6     bool   `json:"v6,omitempty"`    // IPv6 地址
	State  string `json:"state"`           // pending | done | skipped(测不了,原因在 skip)
	Skip   string `json:"skip,omitempty"`  // udp6 = UDP 协议又走 IPv6,测点测不了

	// IPv4 / 域名:结论(reach.Verdict*)、提示代码(reach.Note*,逗号分隔)、分运营商的小结
	Verdict string           `json:"verdict,omitempty"`
	Note    string           `json:"note,omitempty"`
	Groups  []lineReachGroup `json:"groups,omitempty"`
	Error   string           `json:"error,omitempty"`
	// IPv6:和服务器页 IPv6 入站检测同一种结果(结论、哪里的测点得出的、连上 / 测到的测点数),不带各测点的明细
	V6R *reach.V6Result `json:"v6r,omitempty"`
}

type lineReachJob struct {
	mu       sync.Mutex
	lineId   uint
	started  time.Time
	finished int
	items    []lineReachItem
}

func (j *lineReachJob) snapshot() map[string]interface{} {
	j.mu.Lock()
	defer j.mu.Unlock()
	items := make([]lineReachItem, len(j.items))
	copy(items, j.items)
	return map[string]interface{}{"done": j.finished >= len(j.items), "total": len(j.items), "finished": j.finished, "items": items}
}

var lineReachJobs = struct {
	sync.Mutex
	m map[string]*lineReachJob
}{m: map[string]*lineReachJob{}}

// maxLineReach 一次最多测多少个地址:每个约用十几个测点次,免登录额度一小时只有 250 个。
const maxLineReach = 12

// udpOnly 只走 UDP 的协议:测点连不了它的端口。shadowsocks 两种都听,按 TCP 测。
func udpOnly(protocol string) bool { return protocol == "hysteria2" || protocol == "tuic" }

// isIPv6Host 地址是不是 IPv6 字面量(域名、IPv4 都不是)。
func isIPv6Host(host string) bool {
	a, err := netip.ParseAddr(host)
	return err == nil && a.Is6() && !a.Is4In6()
}

// lineReachRoute 认出 /lines/{id}/reach 和 /lines/{id}/reach/{job},返回线路 id 和任务号(开始时为空)。
func lineReachRoute(path string) (uint, string, bool) {
	path = strings.TrimSuffix(path, "/")
	idx := strings.LastIndex(path, "/lines/")
	if idx < 0 {
		return 0, "", false
	}
	parts := strings.Split(path[idx+len("/lines/"):], "/")
	if len(parts) < 2 || len(parts) > 3 || parts[1] != "reach" {
		return 0, "", false
	}
	id, err := strconv.ParseUint(parts[0], 10, 64)
	if err != nil || id == 0 {
		return 0, "", false
	}
	job := ""
	if len(parts) == 3 {
		if job = parts[2]; job == "" {
			return 0, "", false
		}
	}
	return uint(id), job, true
}

func (s *Server) handleLineReach(w http.ResponseWriter, r *http.Request, id uint, jobID string) {
	if jobID != "" {
		if r.Method != http.MethodGet {
			writeJSON(w, http.StatusMethodNotAllowed, map[string]string{"error": "方法不允许"})
			return
		}
		lineReachJobs.Lock()
		job := lineReachJobs.m[jobID]
		lineReachJobs.Unlock()
		if job == nil || job.lineId != id {
			writeJSON(w, http.StatusNotFound, map[string]string{"error": "检测任务不存在或已过期"})
			return
		}
		writeJSON(w, http.StatusOK, job.snapshot())
		return
	}
	if r.Method != http.MethodPost {
		writeJSON(w, http.StatusMethodNotAllowed, map[string]string{"error": "方法不允许"})
		return
	}
	var line model.Line
	if err := s.db.First(&line, id).Error; err != nil {
		writeJSON(w, http.StatusNotFound, map[string]string{"error": "线路不存在"})
		return
	}
	jobID, items, err := s.startLineReach(line)
	if err != nil {
		badRequest(w, err)
		return
	}
	s.audit(r, "line", "reach", line.Name)
	writeJSON(w, http.StatusOK, map[string]interface{}{"job": jobID, "items": items})
}

// startLineReach 开始测这条线路的各个地址。额度不够、已经有一轮在跑(服务器的巡检、外部节点的检测、别的线路)时直接说清楚。
func (s *Server) startLineReach(line model.Line) (string, []lineReachItem, error) {
	svc := lineReachSvc(s)
	if svc == nil {
		return "", nil, errors.New("大陆连通检测没有启用")
	}
	if !line.Enabled {
		return "", nil, errors.New("这条线路停用着,没有在监听,启用后再测")
	}
	eps := sub.LineEndpoints(s.db, line)
	if len(eps) == 0 {
		return "", nil, errors.New("这条线路在订阅里没有可连的地址(没有部署到启用的服务器,或节点都关了输出)")
	}
	udp := udpOnly(line.Protocol)
	items := make([]lineReachItem, 0, len(eps))
	seen := map[string]bool{} // 同一个地址和端口只测一次(自定义地址里写重了的)
	cost := 0
	for _, e := range eps {
		k := e.Server + "|" + strconv.Itoa(e.Port)
		if seen[k] || e.Server == "" || e.Port < 1 || e.Port > 65535 {
			continue
		}
		seen[k] = true
		it := lineReachItem{Label: e.Label, Target: e.Server, Port: e.Port, Relay: e.Relay, UDP: udp, V6: isIPv6Host(e.Server), State: "pending"}
		if it.V6 && udp {
			it.State, it.Skip = "skipped", "udp6"
		} else {
			cost++
		}
		items = append(items, it)
	}
	if len(items) > maxLineReach {
		items = items[:maxLineReach]
		cost = 0
		for _, it := range items {
			if it.State == "pending" {
				cost++
			}
		}
	}
	// items 从这里起不再改动(各个检测只读它);任务手里是另一份,结果在锁里写进去
	job := &lineReachJob{lineId: line.Id, started: time.Now(), items: append([]lineReachItem(nil), items...)}
	for _, it := range items {
		if it.State != "pending" {
			job.finished++
		}
	}
	if cost > 0 {
		if err := svc.CanCheckExt(cost); err != nil {
			return "", nil, err
		}
		release, err := svc.BeginExt()
		if err != nil {
			return "", nil, err
		}
		safe.Go("web.startLineReach", func() {
			defer release()
			sem := make(chan struct{}, 3)
			var wg sync.WaitGroup
			for i := range items {
				if items[i].State != "pending" {
					continue
				}
				wg.Add(1)
				sem <- struct{}{}
				safe.Go1("web.startLineReach", i, func(i int) {
					defer wg.Done()
					defer func() { <-sem }()
					ctx, cancel := context.WithTimeout(context.Background(), 2*time.Minute)
					defer cancel()
					done := items[i]
					if done.V6 {
						v := reach.V6Result{Target: done.Target, Port: done.Port, State: reach.V6Error, Error: "没测成"}
						if res := svc.CheckV6(ctx, done.Target, done.Port); res != nil {
							v = *res
						}
						v.Probes, v.Measurement = nil, ""
						done.V6R = &v
					} else {
						res := svc.CheckExt(ctx, line.Name, done.Target, done.Port, done.UDP)
						done.Verdict, done.Note, done.Error = res.Verdict, res.Note, res.Error
						for _, g := range res.Groups {
							done.Groups = append(done.Groups, lineReachGroup{Key: g.Key, State: g.State, OK: g.OK, Total: g.Total, AvgMs: g.AvgMs})
						}
					}
					done.State = "done"
					job.mu.Lock()
					job.items[i] = done
					job.finished++
					job.mu.Unlock()
				})
			}
			wg.Wait()
		})
	}
	id := newJobID()
	lineReachJobs.Lock()
	for k, j := range lineReachJobs.m {
		if time.Since(j.started) > 30*time.Minute {
			delete(lineReachJobs.m, k)
		}
	}
	lineReachJobs.m[id] = job
	lineReachJobs.Unlock()
	return id, items, nil
}
