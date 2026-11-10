package web

import (
	"context"
	"crypto/rand"
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
	"github.com/Maoyangui/m-ui/reach"
	"github.com/Maoyangui/m-ui/render"
)

// ---- 外部订阅展开:节点明细、逐台服务器测速、大陆连通、添加为上游、隐藏与改名 ----
//
// 外部节点只是并进用户订阅的"别人的节点",本站自己不用它;但管理员常常要从里面挑几个当上游。
// 节点用指纹(ext.Node.Key)认:订阅刷新后对方增删、调换了节点,按指纹记的测速、连通结果和隐藏改名都还对得上。

// extServer 测速结果表的一列:一台启用的服务器。
type extServer struct {
	Id      uint   `json:"id"`
	Name    string `json:"name"`
	IsLocal bool   `json:"isLocal"`
}

// 测速格的状态。
const (
	cellPending     = "pending"
	cellOK          = "ok"
	cellFail        = "fail"        // 真连了,不通
	cellUnsupported = "unsupported" // 协议 / 参数转不成 sing-box 出站,测不了(不是不通)
	cellSkip        = "skip"        // 这台服务器离线,没测
	cellCancelled   = "cancelled"   // 点了停止,或超过总时限
)

// extCell 某个节点在某台服务器上的一次测速。
type extCell struct {
	State   string `json:"state"`
	DelayMs int    `json:"delayMs,omitempty"`
	Method  string `json:"method,omitempty"`
	Error   string `json:"error,omitempty"`
	At      int64  `json:"at,omitempty"` // 测完的时间
}

// extTestJob 一次测速任务:前端每秒来取一次进度,结果一格一格冒出来。结果按节点指纹记。
type extTestJob struct {
	mu       sync.Mutex
	extId    uint // 哪条外部节点:同一条同时只跑一个测速任务
	started  time.Time
	done     bool
	total    int
	finished int
	results  map[string]map[uint]*extCell // 节点指纹 → 服务器 id → 结果
	cancel   context.CancelFunc
}

var extJobs = struct {
	sync.Mutex
	m map[string]*extTestJob
}{m: map[string]*extTestJob{}}

const (
	extTestParallel  = 8                // 每台服务器同时测几个节点:太多会让机场觉得被扫(同一 IP 短时间大量握手)
	extTestTimeLimit = 10 * time.Minute // 一个测速任务的总时限,超过的格子记"未测完"
	extTestSamples   = 2                // 通了再测一次取较小延迟
)

// errExtNodeOffline 失联的副机这次不测。
var errExtNodeOffline = errors.New("服务器离线,这次没测")

// errExtStale 节点列表已经变了(HTTP 409,前端据此重新展开)。
var errExtStale = errors.New("节点列表已更新,请重新展开后再操作")

// errExtPrefsConflict 隐藏 / 改名设置在别处(另一个标签页、另一位管理员)改过了(HTTP 409,前端重新拉取后再试一次)。
var errExtPrefsConflict = errors.New("节点的隐藏 / 改名设置已在别处修改,请刷新后重试")

// errExtTestStopped / errExtTestTimeout 没测完的格子。
var (
	errExtTestStopped = errors.New("已停止,没测")
	errExtTestTimeout = errors.New("超过总时限,没测完")
)

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

// extNodesPayload 展开时给前端的全部内容:节点明细(带隐藏、改名状态)、服务器列、列表指纹、设置版本号、未匹配的记录数。
func (s *Server) extNodesPayload(e model.ExtNode) map[string]interface{} {
	_, servers := s.extTestServers()
	it := ext.ItemsOf(e)
	infos := ext.Details(it)
	f := ext.FilterOf(e)
	ext.Annotate(infos, f)
	return map[string]interface{}{"nodes": infos, "servers": servers, "fetchedAt": e.LastFetch, "sig": ext.ContentSig(it),
		"rev": e.PrefsRev, "stale": f.Stale(ext.RosterOf(it))}
}

// extPick 前端选中的节点:新前端给指纹(跨刷新也认得);老前端给下标,要带展开时的列表指纹。
type extPick struct {
	Keys    []string `json:"keys"`
	Indexes []int    `json:"indexes"`
	Sig     string   `json:"sig"`
}

// resolve 把选中的节点对到当前列表上。指纹有一个对不上(那个节点已经没了)、或下标的列表指纹对不上,返回 errExtStale。
// 同一指纹只取一次(信息节点常共用一个假地址,测一次就够)。
func (p extPick) resolve(infos []ext.NodeInfo, sig string) ([]ext.NodeInfo, error) {
	var out []ext.NodeInfo
	seen := map[string]bool{}
	add := func(n ext.NodeInfo) {
		if !seen[n.Key] {
			seen[n.Key] = true
			out = append(out, n)
		}
	}
	switch {
	case len(p.Keys) > 0:
		byKey := map[string]ext.NodeInfo{}
		for _, n := range infos {
			if _, ok := byKey[n.Key]; !ok {
				byKey[n.Key] = n
			}
		}
		for _, k := range p.Keys {
			n, ok := byKey[k]
			if !ok {
				return nil, errExtStale
			}
			add(n)
		}
	case len(p.Indexes) > 0:
		if p.Sig != sig {
			return nil, errExtStale
		}
		for _, i := range p.Indexes {
			if i >= 0 && i < len(infos) {
				add(infos[i])
			}
		}
	}
	return out, nil
}

// handleExtNodes 处理 /exts/{id}/nodes、/nodes/prefs、/nodes/test[/{job}]、/nodes/reach[/{job}]、/nodes/add-upstream。
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
		writeJSON(w, http.StatusOK, s.extNodesPayload(e))
	case sub[0] == "prefs" && len(sub) == 1: // 隐藏 / 显示 / 改名:只发这次的增量,服务端合并
		if r.Method != http.MethodPut {
			writeJSON(w, http.StatusMethodNotAllowed, map[string]string{"error": "方法不允许"})
			return
		}
		s.saveExtPrefs(w, r, e)
	case sub[0] == "reach" && len(sub) == 1: // 大陆连通:只测勾选的,一次测点额度有限
		if r.Method != http.MethodPost {
			writeJSON(w, http.StatusMethodNotAllowed, map[string]string{"error": "方法不允许"})
			return
		}
		var body extPick
		if err := json.NewDecoder(r.Body).Decode(&body); err != nil {
			badRequest(w, err)
			return
		}
		it := ext.ItemsOf(e)
		targets, err := body.resolve(ext.Details(it), ext.ContentSig(it))
		if err != nil {
			writeJSON(w, http.StatusConflict, map[string]string{"error": err.Error()})
			return
		}
		id, err := s.startExtReach(e, targets)
		if err != nil {
			badRequest(w, err)
			return
		}
		s.audit(r, "ext", "reach-nodes", e.Name)
		writeJSON(w, http.StatusOK, map[string]interface{}{"job": id, "keys": keysOf(targets)})
	case sub[0] == "reach" && len(sub) == 2:
		extReachJobs.Lock()
		job := extReachJobs.m[sub[1]]
		extReachJobs.Unlock()
		if job == nil || job.extId != e.Id {
			writeJSON(w, http.StatusNotFound, map[string]string{"error": "测速任务不存在或已过期"})
			return
		}
		writeJSON(w, http.StatusOK, job.snapshot())
	case sub[0] == "test" && len(sub) == 1:
		if r.Method != http.MethodPost {
			writeJSON(w, http.StatusMethodNotAllowed, map[string]string{"error": "方法不允许"})
			return
		}
		var body struct {
			extPick
			IncludeHidden bool `json:"includeHidden"` // "测速全部"默认只测会输出的节点
		}
		if r.Body != nil && r.ContentLength != 0 {
			if err := json.NewDecoder(r.Body).Decode(&body); err != nil {
				badRequest(w, err)
				return
			}
		}
		it := ext.ItemsOf(e)
		infos := ext.Details(it)
		ext.Annotate(infos, ext.FilterOf(e))
		var targets []ext.NodeInfo
		if len(body.Keys) == 0 && len(body.Indexes) == 0 {
			seen := map[string]bool{}
			for _, n := range infos {
				if (n.Hidden && !body.IncludeHidden) || seen[n.Key] {
					continue
				}
				seen[n.Key] = true
				targets = append(targets, n)
			}
		} else {
			var err error
			if targets, err = body.resolve(infos, ext.ContentSig(it)); err != nil {
				writeJSON(w, http.StatusConflict, map[string]string{"error": err.Error()})
				return
			}
		}
		id, err := s.startExtTest(e, it, targets)
		if err != nil {
			badRequest(w, err)
			return
		}
		s.audit(r, "ext", "test-nodes", e.Name)
		writeJSON(w, http.StatusOK, map[string]interface{}{"job": id, "keys": keysOf(targets)})
	case sub[0] == "test" && len(sub) == 2:
		extJobs.Lock()
		job := extJobs.m[sub[1]]
		extJobs.Unlock()
		if job == nil || job.extId != e.Id {
			writeJSON(w, http.StatusNotFound, map[string]string{"error": "测速任务不存在或已过期"})
			return
		}
		switch r.Method {
		case http.MethodGet:
		case http.MethodDelete: // 停止:没测的格子记"已停止",在途的结果回来也不再记
			job.stop(errExtTestStopped)
			s.audit(r, "ext", "test-stop", e.Name)
		default:
			writeJSON(w, http.StatusMethodNotAllowed, map[string]string{"error": "方法不允许"})
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
		it := ext.ItemsOf(e)
		legacy := false
		for _, x := range body.Items {
			if x.Key == "" {
				legacy = true
			}
		}
		if legacy && body.Sig != ext.ContentSig(it) { // 老前端按下标指节点:列表变了下标就错位(审计 MB30)
			writeJSON(w, http.StatusConflict, map[string]string{"error": errExtStale.Error()})
			return
		}
		if len(body.Items) == 0 {
			badRequest(w, errors.New("没有选择节点"))
			return
		}
		writeJSON(w, http.StatusOK, map[string]interface{}{"results": s.addUpstreamsFromExt(r, e, it, body.Items)})
	default:
		http.NotFound(w, r)
	}
}

func keysOf(list []ext.NodeInfo) []string {
	out := make([]string, 0, len(list))
	for _, n := range list {
		out = append(out, n.Key)
	}
	return out
}

// extPrefOp 面板上的一次隐藏 / 显示 / 改名:节点按名字 + 指纹给。
type extPrefOp struct {
	Name string `json:"name"`
	Key  string `json:"key"`
	To   string `json:"to"` // 改名用:新名字,空 = 恢复原名
}

// saveExtPrefs 合并保存隐藏 / 改名。以前前端用"当前展开的列表"重建整份隐藏名单、服务端整份替换:
// 此刻不在列表里的隐藏项(节点临时下线、名字变了、这次只抓到一部分)下一次随便保存点什么就被抹掉,节点回来后又输出了。
// 现在只收这一次的增量,在事务里读出现有记录合并后写回;rev 是前端展开时拿到的版本号,对不上(别的标签页刚改过)返回 409。
func (s *Server) saveExtPrefs(w http.ResponseWriter, r *http.Request, e model.ExtNode) {
	var body struct {
		Rev        *int64      `json:"rev"`
		Hide       []extPrefOp `json:"hide"`
		Show       []extPrefOp `json:"show"`
		Rename     []extPrefOp `json:"rename"`
		ClearStale bool        `json:"clearStale"` // 清理当前列表里对不上任何节点的记录
	}
	if err := json.NewDecoder(r.Body).Decode(&body); err != nil {
		badRequest(w, err)
		return
	}
	if body.Rev == nil { // 老页面整份提交的格式:不再接受,否则又会抹掉列表外的记录
		writeJSON(w, http.StatusConflict, map[string]string{"error": errExtPrefsConflict.Error()})
		return
	}
	for _, list := range [][]extPrefOp{body.Hide, body.Show, body.Rename} {
		if len(list) > ext.MaxPrefEntries {
			badRequest(w, ext.ErrTooManyPrefs)
			return
		}
		for _, op := range list {
			if op.Key == "" || len(op.Key) > 64 || !ext.ValidName(op.Name) || !ext.ValidRename(op.To) {
				badRequest(w, errors.New("节点参数无效"))
				return
			}
		}
	}
	var cur model.ExtNode
	err := s.db.Transaction(func(tx *gorm.DB) error {
		if err := tx.First(&cur, e.Id).Error; err != nil {
			return err
		}
		if cur.PrefsRev != *body.Rev {
			return errExtPrefsConflict
		}
		f := ext.FilterOf(cur)
		roster := ext.RosterOf(ext.ItemsOf(cur))
		for _, op := range body.Hide {
			f.Hide(roster, op.Name, op.Key)
		}
		for _, op := range body.Show {
			f.Show(roster, op.Name, op.Key)
		}
		for _, op := range body.Rename {
			f.Rename(roster, op.Name, op.Key, op.To)
		}
		if body.ClearStale {
			f.DropStale(roster)
		}
		if err := f.Check(); err != nil {
			return err
		}
		cur.Hidden, cur.Renames, cur.HiddenKeys, cur.RenameKeys = f.Encode()
		out, total := ext.Counts(cur)
		// 老库升级上来这一列可能是 NULL(读出来是 0):按 0 比
		res := tx.Model(&model.ExtNode{}).Where("id = ? AND COALESCE(prefs_rev, 0) = ?", cur.Id, cur.PrefsRev).Updates(map[string]interface{}{
			"hidden": cur.Hidden, "renames": cur.Renames, "hidden_keys": cur.HiddenKeys, "rename_keys": cur.RenameKeys,
			"prefs_rev": cur.PrefsRev + 1, "node_count": out, "node_total": total,
		})
		if res.Error != nil {
			return res.Error
		}
		if res.RowsAffected == 0 {
			return errExtPrefsConflict
		}
		cur.PrefsRev++
		cur.NodeCount, cur.NodeTotal = out, total
		return nil
	})
	switch {
	case errors.Is(err, errExtPrefsConflict):
		writeJSON(w, http.StatusConflict, map[string]string{"error": err.Error()})
		return
	case err != nil:
		badRequest(w, err)
		return
	}
	s.audit(r, "ext", "prefs", e.Name)
	writeJSON(w, http.StatusOK, s.extNodesPayload(cur))
}

// ---- 大陆连通 ----

// extReachCell 一个外部节点的大陆连通结果。
type extReachCell struct {
	State   string        `json:"state"` // pending | done
	Verdict string        `json:"verdict,omitempty"`
	Note    string        `json:"note,omitempty"` // 提示代码(reach.Note*),逗号分隔
	Error   string        `json:"error,omitempty"`
	Groups  []reach.Group `json:"groups,omitempty"`
	At      int64         `json:"at,omitempty"`
}

type extReachJob struct {
	mu       sync.Mutex
	extId    uint
	started  time.Time
	done     bool
	total    int
	finished int
	results  map[string]*extReachCell // 节点指纹 → 结果
}

func (j *extReachJob) snapshot() map[string]interface{} {
	j.mu.Lock()
	defer j.mu.Unlock()
	res := make(map[string]extReachCell, len(j.results))
	for k, c := range j.results {
		res[k] = *c
	}
	return map[string]interface{}{"done": j.done, "total": j.total, "finished": j.finished, "results": res}
}

var extReachJobs = struct {
	sync.Mutex
	m map[string]*extReachJob
}{m: map[string]*extReachJob{}}

// udpTypes 走 UDP 的协议:大陆测点只能 ping 它的地址,连不了 UDP 端口。
var udpTypes = map[string]bool{"hysteria": true, "hysteria2": true, "hy2": true, "tuic": true, "wireguard": true, "juicity": true}

// startExtReach 从大陆三网测勾选的外部节点(不进历史、不告警,判定见 reach.verdictExt)。
// 每个节点要用十几个测点次,免登录额度一小时只够十来个,所以要先勾选,额度不够直接说清楚;
// 和服务器的检测共用额度,同一时间只跑一个(不论服务器还是外部节点),还要给定时巡检留出额度。
func (s *Server) startExtReach(e model.ExtNode, targets []ext.NodeInfo) (string, error) {
	if s.run == nil || s.run.Reach() == nil {
		return "", errors.New("大陆连通检测没有启用")
	}
	if len(targets) == 0 {
		return "", errors.New("先勾选要测的节点")
	}
	svc := s.run.Reach()
	if err := svc.CanCheckExt(len(targets)); err != nil {
		return "", err
	}
	release, err := svc.BeginExt()
	if err != nil {
		return "", err
	}
	job := &extReachJob{extId: e.Id, started: time.Now(), total: len(targets), results: map[string]*extReachCell{}}
	for _, n := range targets {
		job.results[n.Key] = &extReachCell{State: "pending"}
	}
	id := newJobID()
	extReachJobs.Lock()
	for k, j := range extReachJobs.m {
		if time.Since(j.started) > 30*time.Minute {
			delete(extReachJobs.m, k)
		}
	}
	extReachJobs.m[id] = job
	extReachJobs.Unlock()
	go func() {
		defer release()
		sem := make(chan struct{}, 3)
		var wg sync.WaitGroup
		for _, n := range targets {
			wg.Add(1)
			sem <- struct{}{}
			go func(n ext.NodeInfo) {
				defer wg.Done()
				defer func() { <-sem }()
				ctx, cancel := context.WithTimeout(context.Background(), 2*time.Minute)
				defer cancel()
				res := svc.CheckExt(ctx, n.Name, n.Server, n.Port, udpTypes[strings.ToLower(n.Type)])
				job.mu.Lock()
				*job.results[n.Key] = extReachCell{State: "done", Verdict: res.Verdict, Note: res.Note, Error: res.Error, Groups: res.Groups, At: time.Now().Unix()}
				job.finished++
				job.done = job.finished >= job.total
				job.mu.Unlock()
			}(n)
		}
		wg.Wait()
	}()
	return id, nil
}

// ---- 测速 ----

// startExtTest 起一个测速任务:每台服务器各自并发测(上限 extTestParallel),失联的副机不测,
// 协议转不成出站的直接记"测不了",总时限 extTestTimeLimit。
func (s *Server) startExtTest(e model.ExtNode, it ext.Items, targets []ext.NodeInfo) (string, error) {
	// 失联的副机不测:每个节点都要等满超时,前端早停了轮询后台还在跑(审计 MB31)
	offline := map[uint]bool{}
	if s.run != nil && s.run.Hub() != nil {
		for id, st := range s.run.Hub().Statuses() {
			offline[id] = !st.OK
		}
	}
	return s.startExtTestWith(e, it, targets, offline)
}

// extTarget 一个要测的节点:出站参数,或测不了的原因。
type extTarget struct {
	key    string
	up     model.Upstream
	reason string
}

func (s *Server) startExtTestWith(e model.ExtNode, it ext.Items, infos []ext.NodeInfo, offline map[uint]bool) (string, error) {
	if len(it.Nodes) == 0 {
		return "", errors.New("这条外部节点还没有解析到节点(先刷新)")
	}
	if len(infos) == 0 {
		return "", errors.New("没有可测的节点")
	}
	nodes, _ := s.extTestServers()
	if len(nodes) == 0 {
		return "", errors.New("没有启用的服务器")
	}
	targets := make([]extTarget, 0, len(infos))
	for _, n := range infos {
		tg := extTarget{key: n.Key}
		typ, opts, err := ext.Outbound(it.Nodes[n.Index])
		if err != nil {
			tg.reason = err.Error()
		} else {
			b, _ := json.Marshal(opts)
			tg.up = model.Upstream{Name: fmt.Sprintf("ext-test-%d-%s", e.Id, n.Key), Type: typ, Options: b}
		}
		targets = append(targets, tg)
	}
	ctx, cancel := context.WithTimeout(context.Background(), extTestTimeLimit)
	job := &extTestJob{extId: e.Id, started: time.Now(), results: map[string]map[uint]*extCell{}, cancel: cancel}
	now := time.Now().Unix()
	for _, tg := range targets {
		row := map[uint]*extCell{}
		for _, n := range nodes {
			c := &extCell{State: cellPending}
			switch {
			case tg.reason != "":
				c.State, c.Error, c.At = cellUnsupported, tg.reason, now
				job.finished++
			case !n.IsLocal && offline[n.Id]:
				c.State, c.Error, c.At = cellSkip, errExtNodeOffline.Error(), now
				job.finished++
			}
			row[n.Id] = c
		}
		job.results[tg.key] = row
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
			cancel()
			return "", errors.New("这条外部节点正在测速,请等这一轮结束")
		}
	}
	extJobs.m[id] = job
	extJobs.Unlock()
	if job.done {
		cancel()
		return id, nil
	}
	go func() { // 停止或超过总时限:没测的格子记下原因,任务结束
		<-ctx.Done()
		if errors.Is(ctx.Err(), context.DeadlineExceeded) {
			job.stop(errExtTestTimeout)
		} else {
			job.stop(errExtTestStopped)
		}
	}()
	for _, n := range nodes {
		if !n.IsLocal && offline[n.Id] {
			continue
		}
		go func(n model.Node) {
			sem := make(chan struct{}, extTestParallel)
			var wg sync.WaitGroup
			defer wg.Wait()
			for _, tg := range targets {
				if tg.reason != "" {
					continue
				}
				select {
				case sem <- struct{}{}:
				case <-ctx.Done():
					return
				}
				wg.Add(1)
				go func(tg extTarget) {
					defer wg.Done()
					defer func() { <-sem }()
					job.set(tg.key, n.Id, s.extTestOn(ctx, n, tg.up))
				}(tg)
			}
		}(n)
	}
	return id, nil
}

// extTestOn 在一台服务器上测一个出站。
func (s *Server) extTestOn(ctx context.Context, n model.Node, up model.Upstream) extCell {
	c := extCell{State: cellFail}
	switch {
	case s.run == nil:
		c.Error = "数据面未就绪"
	case n.IsLocal:
		ok, ms, meth, errStr := s.run.TestOutbound(ctx, up)
		c.Method, c.Error = meth, errStr
		switch {
		case ok:
			c.State, c.DelayMs, c.Error = cellOK, ms, ""
		case meth == "config":
			c.State = cellUnsupported
		}
	case s.run.Hub() == nil:
		c.Error = "主机未启用多服务器"
	default:
		type result struct {
			h   healthResult
			err error
		}
		ch := make(chan result, 1)
		go func() {
			h, err := s.run.Hub().TestOutboundOn(n, up)
			ch <- result{healthResult{h.OK, h.DelayMs, h.Method, h.Error}, err}
		}()
		select {
		case <-ctx.Done(): // 停止了:在途的请求自己超时结束,结果不再记
			c.State, c.Error = cellCancelled, errExtTestStopped.Error()
		case res := <-ch:
			switch {
			case res.err != nil:
				c.Error = res.err.Error()
			case res.h.ok:
				c.State, c.DelayMs, c.Method = cellOK, res.h.delayMs, res.h.method
			case res.h.method == "config":
				c.State, c.Method, c.Error = cellUnsupported, res.h.method, res.h.err
			default:
				c.Method, c.Error = res.h.method, res.h.err
			}
		}
	}
	c.At = time.Now().Unix()
	return c
}

type healthResult struct {
	ok      bool
	delayMs int
	method  string
	err     string
}

// set 记下一格结果;已经结束的格子(停止、超时)不再改,也不重复计数。
func (j *extTestJob) set(key string, nodeID uint, c extCell) {
	j.mu.Lock()
	defer j.mu.Unlock()
	cell := j.results[key][nodeID]
	if cell == nil || cell.State != cellPending {
		return
	}
	*cell = c
	j.finished++
	if j.finished >= j.total && !j.done {
		j.done = true
		if j.cancel != nil {
			j.cancel()
		}
	}
}

// stop 结束任务:还在等的格子记下原因。
func (j *extTestJob) stop(reason error) {
	j.mu.Lock()
	if !j.done {
		now := time.Now().Unix()
		for _, row := range j.results {
			for _, c := range row {
				if c.State == cellPending {
					c.State, c.Error, c.At = cellCancelled, reason.Error(), now
					j.finished++
				}
			}
		}
		j.done = true
	}
	cancel := j.cancel
	j.mu.Unlock()
	if cancel != nil {
		cancel()
	}
}

func newJobID() string {
	b := make([]byte, 8)
	_, _ = rand.Read(b)
	return hex.EncodeToString(b)
}

// snapshot 给前端的进度:结果按节点指纹 → 服务器 id(JSON 的键只能是字符串)。
func (j *extTestJob) snapshot() map[string]interface{} {
	j.mu.Lock()
	defer j.mu.Unlock()
	res := make(map[string]map[string]extCell, len(j.results))
	for key, row := range j.results {
		m := make(map[string]extCell, len(row))
		for nid, c := range row {
			m[strconv.FormatUint(uint64(nid), 10)] = *c
		}
		res[key] = m
	}
	return map[string]interface{}{"done": j.done, "total": j.total, "finished": j.finished, "results": res, "started": j.started.Unix()}
}

// ---- 添加为上游 ----

// extAddItem 「添加为上游」的一项:哪个节点(指纹;老前端给下标)、叫什么名。
type extAddItem struct {
	Key   string `json:"key"`
	Index int    `json:"index"`
	Name  string `json:"name"`
}

type extAddResult struct {
	Key   string `json:"key,omitempty"`
	Index int    `json:"index"`
	Name  string `json:"name"`
	OK    bool   `json:"ok"`
	Error string `json:"error,omitempty"`
	Id    uint   `json:"id,omitempty"`
}

// addUpstreamsFromExt 把选中的外部节点各加成一条上游:出站由 ext.Outbound 无损转换(和测速同一份),
// 走和手动新增上游同一套校验(名字、参数干跑、整份配置校验),逐条给结果,重名直接报错不自动改名。
func (s *Server) addUpstreamsFromExt(r *http.Request, e model.ExtNode, it ext.Items, items []extAddItem) []extAddResult {
	byKey := map[string]int{}
	for i, n := range it.Nodes {
		if _, ok := byKey[n.Key]; !ok {
			byKey[n.Key] = i
		}
	}
	out := make([]extAddResult, 0, len(items))
	created := 0
	var nodeCert render.NodeCert
	if s.run != nil {
		nodeCert = s.run.NodeCert()
	}
	for _, x := range items {
		res := extAddResult{Key: x.Key, Index: x.Index, Name: strings.TrimSpace(x.Name)}
		fail := func(msg string) {
			res.Error = msg
			out = append(out, res)
		}
		idx := x.Index
		if x.Key != "" {
			i, ok := byKey[x.Key]
			if !ok {
				idx = -1
			} else {
				idx = i
			}
		}
		if idx < 0 || idx >= len(it.Nodes) {
			fail("节点不存在(列表可能已刷新,请重新展开)")
			continue
		}
		typ, opts, err := ext.Outbound(it.Nodes[idx])
		if err != nil {
			fail(err.Error())
			continue
		}
		b, _ := json.Marshal(opts)
		up := model.Upstream{Name: res.Name, Type: typ, Options: b}
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
