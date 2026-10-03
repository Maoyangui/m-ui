package web

import (
	"crypto/rand"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"net/http"
	"strconv"
	"strings"
	"sync"
	"time"

	"gorm.io/gorm"

	"github.com/Maoyangui/m-ui/database/model"
	"github.com/Maoyangui/m-ui/ext"
	"github.com/Maoyangui/m-ui/render"
	"github.com/Maoyangui/m-ui/upstream"
)

// ---- 外部订阅展开:节点明细、逐台服务器测速、添加为上游 ----
//
// 外部节点只是并进用户订阅的"别人的节点",本站自己不用它;但管理员常常要从里面挑几个当上游。
// 这里给三样东西:看清每个节点的全部参数、在每台服务器上真连一次看通不通、勾选后起个名直接加成上游。

// extServer 测速结果表的一列:一台启用的服务器。
type extServer struct {
	Id      uint   `json:"id"`
	Name    string `json:"name"`
	IsLocal bool   `json:"isLocal"`
}

// extCell 某个节点在某台服务器上的一次测速。
type extCell struct {
	State   string `json:"state"` // pending | ok | fail
	DelayMs int    `json:"delayMs,omitempty"`
	Method  string `json:"method,omitempty"`
	Error   string `json:"error,omitempty"`
}

// extTestJob 一次测速任务:前端每秒来取一次进度,结果一格一格冒出来。
type extTestJob struct {
	mu       sync.Mutex
	extId    uint // 哪条外部节点:同一条同时只跑一个测速任务
	started  time.Time
	done     bool
	total    int
	finished int
	results  map[int]map[uint]*extCell // 节点下标 → 服务器 id → 结果
}

var extJobs = struct {
	sync.Mutex
	m map[string]*extTestJob
}{m: map[string]*extTestJob{}}

func extItems(e model.ExtNode) ext.Items {
	if e.Type == "sub" {
		return ext.Parse(e.Cache)
	}
	return ext.Parse(e.Value)
}

// extSig 节点列表的内容指纹。测速与添加为上游按下标指节点,前端拿的是展开时那一份;订阅自动刷新后对方增删、
// 调换了节点,下标就错位,以前会把别的节点加成上游还显示成功、测速结果贴到错的节点旁(审计 MB30)。
// 提交时带上指纹,对不上就让前端重新展开。
func extSig(infos []ext.NodeInfo) string {
	h := sha256.New()
	for _, n := range infos {
		fmt.Fprintf(h, "%s\x00%s\x00%s\x00%d\x00%s\n", n.Name, n.Type, n.Server, n.Port, n.Link)
	}
	return hex.EncodeToString(h.Sum(nil))[:16]
}

// errExtNodeOffline 失联的副机这次不测。
var errExtNodeOffline = errors.New("服务器离线,这次没测")

// errExtStale 节点列表已经变了(HTTP 409,前端据此重新展开)。
var errExtStale = errors.New("节点列表已更新,请重新展开后再操作")

// extTestServers 测速的目标是所有启用的服务器(含主机),不看有没有线路用它:
// 它还不是上游,管理员要看的正是"加成上游之前,每台机器连它通不通"。
func (s *Server) extTestServers() ([]model.Node, []extServer) {
	var nodes []model.Node
	s.db.Where("enabled = ?", true).Order("sort asc, id asc").Find(&nodes)
	out := make([]extServer, 0, len(nodes))
	for _, n := range nodes {
		out = append(out, extServer{Id: n.Id, Name: n.Name, IsLocal: n.IsLocal})
	}
	return nodes, out
}

// handleExtNodes 处理 /exts/{id}/nodes、/nodes/test、/nodes/test/{job}、/nodes/add-upstream。
func (s *Server) handleExtNodes(w http.ResponseWriter, r *http.Request, e model.ExtNode, sub []string) {
	if n := len(sub); n > 0 && sub[n-1] == "" {
		sub = sub[:n-1]
	}
	switch {
	case len(sub) == 0:
		if r.Method != http.MethodGet {
			writeJSON(w, http.StatusMethodNotAllowed, map[string]string{"error": "方法不允许"})
			return
		}
		_, servers := s.extTestServers()
		infos := ext.Details(extItems(e))
		writeJSON(w, http.StatusOK, map[string]interface{}{"nodes": infos, "servers": servers, "fetchedAt": e.LastFetch, "sig": extSig(infos)})
	case sub[0] == "test" && len(sub) == 1:
		if r.Method != http.MethodPost {
			writeJSON(w, http.StatusMethodNotAllowed, map[string]string{"error": "方法不允许"})
			return
		}
		var body struct {
			Indexes []int  `json:"indexes"`
			Sig     string `json:"sig"`
		}
		if r.Body != nil && r.ContentLength != 0 {
			if err := json.NewDecoder(r.Body).Decode(&body); err != nil {
				badRequest(w, err)
				return
			}
		}
		if body.Sig != extSig(ext.Details(extItems(e))) {
			writeJSON(w, http.StatusConflict, map[string]string{"error": errExtStale.Error()})
			return
		}
		id, err := s.startExtTest(e, body.Indexes)
		if err != nil {
			badRequest(w, err)
			return
		}
		s.audit(r, "ext", "test-nodes", e.Name)
		writeJSON(w, http.StatusOK, map[string]string{"job": id})
	case sub[0] == "test" && len(sub) == 2:
		extJobs.Lock()
		job := extJobs.m[sub[1]]
		extJobs.Unlock()
		if job == nil {
			writeJSON(w, http.StatusNotFound, map[string]string{"error": "测速任务不存在或已过期"})
			return
		}
		writeJSON(w, http.StatusOK, job.snapshot())
	case sub[0] == "add-upstream" && len(sub) == 1:
		if r.Method != http.MethodPost {
			writeJSON(w, http.StatusMethodNotAllowed, map[string]string{"error": "方法不允许"})
			return
		}
		var body struct {
			Sig   string       `json:"sig"`
			Items []extAddItem `json:"items"`
		}
		if err := json.NewDecoder(r.Body).Decode(&body); err != nil {
			badRequest(w, err)
			return
		}
		items := body.Items
		if body.Sig != extSig(ext.Details(extItems(e))) {
			writeJSON(w, http.StatusConflict, map[string]string{"error": errExtStale.Error()})
			return
		}
		if len(items) == 0 {
			badRequest(w, errors.New("没有选择节点"))
			return
		}
		writeJSON(w, http.StatusOK, map[string]interface{}{"results": s.addUpstreamsFromExt(r, e, items)})
	default:
		http.NotFound(w, r)
	}
}

// startExtTest 起一个测速任务:每台服务器各一个工人,逐个真连(临时实例本来就是串行的,并发再大也快不了)。
// indexes 为空 = 全部节点。
func (s *Server) startExtTest(e model.ExtNode, indexes []int) (string, error) {
	// 失联的副机不测:每个节点都要等满 25 秒超时,60 个节点就是二十多分钟,前端早停了轮询后台还在跑(审计 MB31)
	offline := map[uint]bool{}
	if s.run != nil && s.run.Hub() != nil {
		for id, st := range s.run.Hub().Statuses() {
			offline[id] = !st.OK
		}
	}
	return s.startExtTestWith(e, indexes, offline)
}

func (s *Server) startExtTestWith(e model.ExtNode, indexes []int, offline map[uint]bool) (string, error) {
	infos := ext.Details(extItems(e))
	if len(infos) == 0 {
		return "", errors.New("这条外部节点还没有解析到节点(先刷新)")
	}
	if len(indexes) == 0 {
		for i := range infos {
			indexes = append(indexes, i)
		}
	}
	nodes, _ := s.extTestServers()
	if len(nodes) == 0 {
		return "", errors.New("没有启用的服务器")
	}
	type target struct {
		idx int
		up  model.Upstream
		err string
	}
	var targets []target
	seen := map[int]bool{}
	for _, i := range indexes {
		if i < 0 || i >= len(infos) || seen[i] {
			continue
		}
		seen[i] = true
		n := infos[i]
		tg := target{idx: i}
		switch {
		case !n.Upstream || n.Link == "":
			tg.err = "协议不受支持或无法转成链接"
		default:
			p, err := upstream.ParseLink(n.Link)
			if err != nil {
				tg.err = err.Error()
			} else {
				tg.up = model.Upstream{Name: fmt.Sprintf("ext-test-%d-%d", e.Id, i), Type: p.Type, Options: p.OptionsJSON()}
			}
		}
		targets = append(targets, tg)
	}
	if len(targets) == 0 {
		return "", errors.New("没有可测的节点")
	}
	job := &extTestJob{extId: e.Id, started: time.Now(), results: map[int]map[uint]*extCell{}}
	for _, tg := range targets {
		row := map[uint]*extCell{}
		for _, n := range nodes {
			c := &extCell{State: "pending"}
			switch {
			case tg.err != "":
				c.State, c.Error = "fail", tg.err
				job.finished++
			case !n.IsLocal && offline[n.Id]:
				c.State, c.Error = "fail", errExtNodeOffline.Error()
				job.finished++
			}
			row[n.Id] = c
		}
		job.results[tg.idx] = row
	}
	job.total = len(targets) * len(nodes)
	job.done = job.finished >= job.total
	id := newJobID()
	extJobs.Lock()
	for k, j := range extJobs.m { // 半小时前的任务没人再看了
		if time.Since(j.started) > 30*time.Minute {
			delete(extJobs.m, k)
		}
	}
	for _, j := range extJobs.m { // 同一条外部节点同时只跑一个:重复点击以前会起多组任务一起占着各台服务器
		j.mu.Lock()
		busy := j.extId == e.Id && !j.done
		j.mu.Unlock()
		if busy {
			extJobs.Unlock()
			return "", errors.New("这条外部节点正在测速,请等这一轮结束")
		}
	}
	extJobs.m[id] = job
	extJobs.Unlock()
	if job.done {
		return id, nil
	}
	for _, n := range nodes {
		if !n.IsLocal && offline[n.Id] {
			continue
		}
		go func(n model.Node) {
			for _, tg := range targets {
				if tg.err != "" {
					continue
				}
				c := extCell{State: "fail"}
				switch {
				case s.run == nil:
					c.Error = "数据面未就绪"
				case n.IsLocal:
					ok, ms, meth, errStr := s.run.CheckUpstream(tg.up)
					c.Method, c.Error = meth, errStr
					if ok {
						c.State, c.DelayMs, c.Error = "ok", ms, ""
					}
				case s.run.Hub() == nil:
					c.Error = "主机未启用多服务器"
				default:
					res, err := s.run.Hub().TestOutboundOn(n, tg.up)
					switch {
					case err != nil:
						c.Error = err.Error()
					case res.OK:
						c.State, c.DelayMs, c.Method = "ok", res.DelayMs, res.Method
					default:
						c.Method, c.Error = res.Method, res.Error
					}
				}
				job.mu.Lock()
				*job.results[tg.idx][n.Id] = c
				job.finished++
				if job.finished >= job.total {
					job.done = true
				}
				job.mu.Unlock()
			}
		}(n)
	}
	return id, nil
}

func newJobID() string {
	b := make([]byte, 8)
	_, _ = rand.Read(b)
	return hex.EncodeToString(b)
}

// snapshot 给前端的进度:JSON 的键只能是字符串,下标与服务器 id 都转成字符串。
func (j *extTestJob) snapshot() map[string]interface{} {
	j.mu.Lock()
	defer j.mu.Unlock()
	res := make(map[string]map[string]extCell, len(j.results))
	for idx, row := range j.results {
		m := make(map[string]extCell, len(row))
		for nid, c := range row {
			m[strconv.FormatUint(uint64(nid), 10)] = *c
		}
		res[strconv.Itoa(idx)] = m
	}
	return map[string]interface{}{"done": j.done, "total": j.total, "finished": j.finished, "results": res}
}

// extAddItem 「添加为上游」的一项:哪个节点、叫什么名。
type extAddItem struct {
	Index int    `json:"index"`
	Name  string `json:"name"`
}

type extAddResult struct {
	Index int    `json:"index"`
	Name  string `json:"name"`
	OK    bool   `json:"ok"`
	Error string `json:"error,omitempty"`
	Id    uint   `json:"id,omitempty"`
}

// addUpstreamsFromExt 把选中的外部节点各加成一条上游:走和手动新增上游同一套校验(名字、参数干跑、整份配置校验),
// 逐条给结果,重名直接报错不自动改名。
func (s *Server) addUpstreamsFromExt(r *http.Request, e model.ExtNode, items []extAddItem) []extAddResult {
	infos := ext.Details(extItems(e))
	out := make([]extAddResult, 0, len(items))
	created := 0
	var nodeCert render.NodeCert
	if s.run != nil {
		nodeCert = s.run.NodeCert()
	}
	for _, it := range items {
		res := extAddResult{Index: it.Index, Name: strings.TrimSpace(it.Name)}
		fail := func(msg string) {
			res.Error = msg
			out = append(out, res)
		}
		if it.Index < 0 || it.Index >= len(infos) {
			fail("节点不存在(列表可能已刷新,请重新展开)")
			continue
		}
		n := infos[it.Index]
		if !n.Upstream || n.Link == "" {
			fail("协议不受支持或无法转成链接")
			continue
		}
		p, err := upstream.ParseLink(n.Link)
		if err != nil {
			fail(err.Error())
			continue
		}
		up := model.Upstream{Name: res.Name, Type: p.Type, Options: p.OptionsJSON()}
		if err := s.validateUpstream(&up); err != nil {
			fail(err.Error())
			continue
		}
		if err := s.dryRunUpstream(&up); err != nil {
			fail(err.Error())
			continue
		}
		err = s.db.Transaction(func(tx *gorm.DB) error {
			if err := tx.Create(&up).Error; err != nil {
				return err
			}
			if s.run == nil {
				return nil
			}
			return validateFullConfig(tx, nodeCert)
		})
		if err != nil {
			fail(err.Error())
			continue
		}
		res.OK, res.Id = true, up.Id
		created++
		s.audit(r, "upstream", "create", up.Name+"(来自外部节点 "+e.Name+")")
		out = append(out, res)
	}
	if created > 0 && s.run != nil {
		s.reloadUpstreams("从外部节点添加上游")
	}
	return out
}
