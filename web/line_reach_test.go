package web

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"net/http"
	"net/http/httptest"
	"sort"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/Maoyangui/m-ui/database/model"
	"github.com/Maoyangui/m-ui/reach"
)

// fakeLineReach 假的测点服务:记下被要求测了什么,按地址给出事先定好的结果。
type fakeLineReach struct {
	mu      sync.Mutex
	calls   []string
	quota   error         // CanCheckExt 的回复
	busy    bool          // BeginExt 说正在测
	asked   int           // CanCheckExt 被问的个数
	running int           // 还没放开的 BeginExt
	hold    chan struct{} // 不为空时每个检测都等它放行
	results map[string]reach.Result
	v6      map[string]*reach.V6Result
}

func (f *fakeLineReach) CanCheckExt(n int) error {
	f.mu.Lock()
	defer f.mu.Unlock()
	f.asked = n
	return f.quota
}

func (f *fakeLineReach) BeginExt() (func(), error) {
	f.mu.Lock()
	defer f.mu.Unlock()
	if f.busy || f.running > 0 {
		return nil, reach.ErrBusy
	}
	f.running++
	return func() { f.mu.Lock(); f.running--; f.mu.Unlock() }, nil
}

func (f *fakeLineReach) CheckExt(ctx context.Context, name, host string, port int, udp bool) reach.Result {
	if f.hold != nil {
		<-f.hold
	}
	f.mu.Lock()
	defer f.mu.Unlock()
	f.calls = append(f.calls, fmt.Sprintf("v4 %s %s:%d udp=%v", name, host, port, udp))
	if r, ok := f.results[host]; ok {
		return r
	}
	return reach.Result{Verdict: reach.VerdictOK}
}

func (f *fakeLineReach) CheckV6(ctx context.Context, host string, port int) *reach.V6Result {
	if f.hold != nil {
		<-f.hold
	}
	f.mu.Lock()
	defer f.mu.Unlock()
	f.calls = append(f.calls, fmt.Sprintf("v6 %s:%d", host, port))
	return f.v6[host]
}

func (f *fakeLineReach) sorted() []string {
	f.mu.Lock()
	defer f.mu.Unlock()
	out := append([]string(nil), f.calls...)
	sort.Strings(out)
	return out
}

type lineReachResp struct {
	Job      string
	Done     bool
	Total    int
	Finished int
	Items    []lineReachItem
	Error    string
}

func lineReachCall(t *testing.T, s *Server, method, path string) (int, lineReachResp) {
	t.Helper()
	rec := httptest.NewRecorder()
	s.handleLineItem(rec, httptest.NewRequest(method, path, nil))
	var out lineReachResp
	_ = json.Unmarshal(rec.Body.Bytes(), &out)
	return rec.Code, out
}

func waitLineReach(t *testing.T, s *Server, id uint, job string) lineReachResp {
	t.Helper()
	for i := 0; i < 400; i++ {
		code, st := lineReachCall(t, s, http.MethodGet, fmt.Sprintf("/app/api/lines/%d/reach/%s", id, job))
		if code != http.StatusOK {
			t.Fatalf("查进度: %d %+v", code, st)
		}
		if st.Done {
			return st
		}
		time.Sleep(5 * time.Millisecond)
	}
	t.Fatal("等不到检测结束")
	return lineReachResp{}
}

func useFakeLineReach(t *testing.T, f *fakeLineReach) {
	t.Helper()
	old := lineReachSvc
	lineReachSvc = func(*Server) lineReachChecker {
		if f == nil {
			return nil
		}
		return f
	}
	t.Cleanup(func() { lineReachSvc = old })
}

// 三台服务器:本机(IPv4)、一台 IPv4 的副机、一台只有 IPv6 地址的副机。
func lineReachServer(t *testing.T) (*Server, model.Node, model.Node) {
	t.Helper()
	run := newLDRunner(t, "reach.db", map[string]string{"publicIp": "203.0.113.10"})
	s := NewServer(run)
	a := model.Node{Name: "香港", Addr: "198.51.100.20", ApiUrl: "https://a.invalid/app/", Token: "x", Enabled: true, Sort: 2}
	b := model.Node{Name: "六号", Addr: "2001:db8::5", ApiUrl: "https://b.invalid/app/", Token: "x", Enabled: true, Sort: 3}
	for _, n := range []*model.Node{&a, &b} {
		if err := run.DB().Create(n).Error; err != nil {
			t.Fatal(err)
		}
	}
	return s, a, b
}

// TCP 的线路:订阅里的每个地址都连它自己的端口;IPv4 的按大陆三网那一套测,走 IPv6 的按 IPv6 入站那一套测;
// 给页面的结果是轻的(每家运营商一个小结,不带各测点的明细)。不进历史:服务器页的检测结果不受影响。
func TestLineReachTCP(t *testing.T) {
	s, _, _ := lineReachServer(t)
	line := model.Line{Name: "V", Protocol: "vless", Port: 443, Enabled: true, Options: json.RawMessage(`{}`)}
	s.db.Create(&line)
	f := &fakeLineReach{
		results: map[string]reach.Result{
			"203.0.113.10": {Verdict: reach.VerdictOK, Groups: []reach.Group{
				{Key: "cm", State: reach.StateOK, OK: 3, Total: 3, AvgMs: 41.2, Probes: []reach.ProbeResult{{City: "Beijing", OK: true}}},
				{Key: "ct", State: reach.StateOK, OK: 2, Total: 3, AvgMs: 55},
				{Key: "cu", State: reach.StateNone},
				{Key: "hk", State: reach.StateOK, OK: 2, Total: 2, AvgMs: 3.1}}},
			"198.51.100.20": {Verdict: reach.VerdictPortBlocked, Groups: []reach.Group{{Key: "cm", State: reach.StateDown, Total: 3}}},
		},
		v6: map[string]*reach.V6Result{"2001:db8::5": {Target: "2001:db8::5", Port: 443, State: reach.V6CNDown, Scope: reach.V6ScopeBoth, OK: 3, Total: 6, CnTotal: 3,
			Probes: []reach.ProbeResult{{City: "Beijing"}}, Measurement: "m-1"}},
	}
	useFakeLineReach(t, f)

	code, st := lineReachCall(t, s, http.MethodPost, fmt.Sprintf("/app/api/lines/%d/reach", line.Id))
	if code != http.StatusOK || st.Job == "" || len(st.Items) != 3 {
		t.Fatalf("开始检测: %d %+v", code, st)
	}
	for _, it := range st.Items {
		if it.State != "pending" || it.Port != 443 || it.UDP {
			t.Fatalf("刚开始时每个地址都在等: %+v", it)
		}
	}
	if st.Items[2].Target != "2001:db8::5" || !st.Items[2].V6 || st.Items[0].V6 {
		t.Fatalf("哪个地址是 IPv6 要认对: %+v", st.Items)
	}
	if f.asked != 3 {
		t.Fatalf("先问额度够不够测 3 个,问的是 %d", f.asked)
	}
	end := waitLineReach(t, s, line.Id, st.Job)
	if end.Total != 3 || end.Finished != 3 {
		t.Fatalf("结束时: %+v", end)
	}
	want := []string{"v4 V 198.51.100.20:443 udp=false", "v4 V 203.0.113.10:443 udp=false", "v6 2001:db8::5:443"}
	if got := f.sorted(); strings.Join(got, ";") != strings.Join(want, ";") {
		t.Fatalf("测的地址不对:\n got %q\nwant %q", got, want)
	}
	it := end.Items[0]
	if it.Label != "主机" {
		t.Fatalf("名字是服务器名: %+v", it)
	}
	if it.Target != "203.0.113.10" || it.State != "done" || it.Verdict != reach.VerdictOK || len(it.Groups) != 4 || it.Groups[0].AvgMs != 41.2 || it.Groups[1].OK != 2 {
		t.Fatalf("本机的结果: %+v", it)
	}
	if it = end.Items[1]; it.Verdict != reach.VerdictPortBlocked || it.Label != "香港" {
		t.Fatalf("香港的结果: %+v", it)
	}
	if it = end.Items[2]; it.State != "done" || it.V6R == nil || it.V6R.State != reach.V6CNDown || it.V6R.Scope != reach.V6ScopeBoth || it.V6R.OK != 3 || it.V6R.Total != 6 || it.V6R.CnTotal != 3 || it.Verdict != "" {
		t.Fatalf("IPv6 那台的结果: %+v %+v", it, it.V6R)
	}
	// 给页面的结果里不带各测点的明细
	rec := httptest.NewRecorder()
	s.handleLineItem(rec, httptest.NewRequest(http.MethodGet, fmt.Sprintf("/app/api/lines/%d/reach/%s", line.Id, st.Job), nil))
	if strings.Contains(rec.Body.String(), "Beijing") || strings.Contains(rec.Body.String(), "probes") {
		t.Fatalf("结果里不该有测点明细: %s", rec.Body.String())
	}
	if f.running != 0 {
		t.Fatal("测完要把「正在测」放开")
	}
}

// UDP 的线路(hysteria2、tuic):测点连不了 UDP 端口,只 ping 地址 —— 如实标出来;UDP 又走 IPv6 的测不了,不发请求。
func TestLineReachUDP(t *testing.T) {
	s, _, _ := lineReachServer(t)
	line := model.Line{Name: "H", Protocol: "hysteria2", Port: 4545, Enabled: true, Options: json.RawMessage(`{}`)}
	s.db.Create(&line)
	f := &fakeLineReach{results: map[string]reach.Result{"203.0.113.10": {Verdict: reach.VerdictOK, Note: reach.NoteUDPPing}}}
	useFakeLineReach(t, f)
	code, st := lineReachCall(t, s, http.MethodPost, fmt.Sprintf("/app/api/lines/%d/reach", line.Id))
	if code != http.StatusOK || len(st.Items) != 3 {
		t.Fatalf("开始检测: %d %+v", code, st)
	}
	if v6 := st.Items[2]; v6.State != "skipped" || v6.Skip != "udp6" || !v6.UDP || !v6.V6 {
		t.Fatalf("UDP 又走 IPv6 的测不了: %+v", v6)
	}
	if f.asked != 2 {
		t.Fatalf("额度只算真要测的 2 个,问的是 %d", f.asked)
	}
	end := waitLineReach(t, s, line.Id, st.Job)
	want := []string{"v4 H 198.51.100.20:4545 udp=true", "v4 H 203.0.113.10:4545 udp=true"}
	if got := f.sorted(); strings.Join(got, ";") != strings.Join(want, ";") {
		t.Fatalf("测的地址不对:\n got %q\nwant %q", got, want)
	}
	if it := end.Items[0]; !it.UDP || it.Verdict != reach.VerdictOK || it.Note != reach.NoteUDPPing {
		t.Fatalf("UDP 的结果要带着「只 ping 了地址」的提示: %+v", it)
	}
	if end.Items[2].State != "skipped" || end.Finished != 3 {
		t.Fatalf("测不了的那个算已经处理完: %+v", end)
	}

	// 只有测不了的地址时:不问额度、不占「正在测」,直接出结果
	s.db.Model(&line).Update("node_ids", json.RawMessage(`[3]`))
	var only model.Node
	s.db.Where("name = ?", "六号").First(&only)
	s.db.Model(&line).Update("node_ids", json.RawMessage(fmt.Sprintf(`[%d]`, only.Id)))
	f.asked, f.calls = -1, nil
	code, st = lineReachCall(t, s, http.MethodPost, fmt.Sprintf("/app/api/lines/%d/reach", line.Id))
	if code != http.StatusOK || len(st.Items) != 1 || st.Items[0].Skip != "udp6" || f.asked != -1 {
		t.Fatalf("只有测不了的地址: %d %+v asked=%d", code, st, f.asked)
	}
	if end = waitLineReach(t, s, line.Id, st.Job); !end.Done || len(f.sorted()) != 0 {
		t.Fatalf("不该发任何检测: %+v %q", end, f.sorted())
	}
}

// 不该测、测不了的情况都说清楚:线路不存在、停用着、没有地址、额度不够、已经有一轮在跑、副机上;任务号对不上别的线路。
func TestLineReachGuards(t *testing.T) {
	s, _, _ := lineReachServer(t)
	line := model.Line{Name: "V", Protocol: "trojan", Port: 8443, Enabled: true, Options: json.RawMessage(`{}`)}
	other := model.Line{Name: "W", Protocol: "trojan", Port: 8444, Enabled: true, Options: json.RawMessage(`{}`)}
	off := model.Line{Name: "X", Protocol: "trojan", Port: 8445, Enabled: true, Options: json.RawMessage(`{}`)}
	s.db.Create(&line)
	s.db.Create(&other)
	s.db.Create(&off)
	s.db.Model(&model.Line{}).Where("id = ?", off.Id).Update("enabled", false)
	f := &fakeLineReach{}
	useFakeLineReach(t, f)
	path := func(id uint) string { return fmt.Sprintf("/app/api/lines/%d/reach", id) }

	if code, _ := lineReachCall(t, s, http.MethodPost, path(9999)); code != http.StatusNotFound {
		t.Fatalf("线路不存在应 404: %d", code)
	}
	if code, st := lineReachCall(t, s, http.MethodPost, path(off.Id)); code != http.StatusBadRequest || !strings.Contains(st.Error, "停用") {
		t.Fatalf("停用的线路不测: %d %+v", code, st)
	}
	if code, _ := lineReachCall(t, s, http.MethodGet, path(line.Id)); code != http.StatusMethodNotAllowed {
		t.Fatalf("开始要用 POST: %d", code)
	}
	if code, _ := lineReachCall(t, s, http.MethodGet, path(line.Id)+"/nosuchjob"); code != http.StatusNotFound {
		t.Fatalf("没有的任务应 404: %d", code)
	}
	f.quota = errors.New("Globalping 额度不够")
	if code, st := lineReachCall(t, s, http.MethodPost, path(line.Id)); code != http.StatusBadRequest || !strings.Contains(st.Error, "额度不够") {
		t.Fatalf("额度不够要说清楚: %d %+v", code, st)
	}
	f.quota, f.busy = nil, true
	if code, st := lineReachCall(t, s, http.MethodPost, path(line.Id)); code != http.StatusBadRequest || !strings.Contains(st.Error, "正在检测") {
		t.Fatalf("已经有一轮在跑: %d %+v", code, st)
	}
	f.busy = false

	// 一条线路在测的时候,别的线路要等它测完;任务号只认自己的线路
	f.hold = make(chan struct{})
	code, st := lineReachCall(t, s, http.MethodPost, path(line.Id))
	if code != http.StatusOK {
		t.Fatalf("开始检测: %d %+v", code, st)
	}
	if code, st2 := lineReachCall(t, s, http.MethodPost, path(other.Id)); code != http.StatusBadRequest || !strings.Contains(st2.Error, "正在检测") {
		t.Fatalf("同一时间只跑一个: %d %+v", code, st2)
	}
	if code, _ := lineReachCall(t, s, http.MethodGet, path(other.Id)+"/"+st.Job); code != http.StatusNotFound {
		t.Fatalf("拿别的线路的编号查不到这个任务: %d", code)
	}
	if code, mid := lineReachCall(t, s, http.MethodGet, path(line.Id)+"/"+st.Job); code != http.StatusOK || mid.Done || mid.Finished != 0 || mid.Total != 3 {
		t.Fatalf("测到一半: %d %+v", code, mid)
	}
	close(f.hold)
	waitLineReach(t, s, line.Id, st.Job)

	// 线路只部署在停用的服务器上:订阅里没有它的地址
	s.db.Model(&model.Line{}).Where("id = ?", other.Id).Update("node_ids", json.RawMessage(`[987654]`))
	f.hold = nil
	if code, st := lineReachCall(t, s, http.MethodPost, path(other.Id)); code != http.StatusBadRequest || !strings.Contains(st.Error, "没有可连的地址") {
		t.Fatalf("没有地址: %d %+v", code, st)
	}
	useFakeLineReach(t, nil)
	if code, st := lineReachCall(t, s, http.MethodPost, path(line.Id)); code != http.StatusBadRequest || !strings.Contains(st.Error, "没有启用") {
		t.Fatalf("没有测点服务: %d %+v", code, st)
	}
}

func TestLineReachRoute(t *testing.T) {
	for path, want := range map[string]string{
		"/app/api/lines/3/reach":       "3||true",
		"/app/api/lines/3/reach/":      "3||true",
		"/app/api/lines/3/reach/ab12":  "3|ab12|true",
		"/app/api/lines/3/reach/ab/cd": "0||false",
		"/app/api/lines/0/reach":       "0||false",
		"/app/api/lines/x/reach":       "0||false",
		"/app/api/lines/3/detail":      "0||false",
		"/app/api/lines/reach":         "0||false",
		"/app/api/lines/3":             "0||false",
	} {
		id, job, ok := lineReachRoute(path)
		if got := fmt.Sprintf("%d|%s|%v", id, job, ok); got != want {
			t.Errorf("%s: got %s want %s", path, got, want)
		}
	}
}
