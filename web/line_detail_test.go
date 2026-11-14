package web

import (
	"encoding/json"
	"fmt"
	"net/http"
	"net/http/httptest"
	"path/filepath"
	"strconv"
	"testing"
	"time"

	"gorm.io/gorm"

	"github.com/Maoyangui/m-ui/database"
	"github.com/Maoyangui/m-ui/database/model"
	"github.com/Maoyangui/m-ui/runner"
)

// 线路详情用到的字段(JSON 字段名大小写不敏感)
type ldPoint struct{ T, Up, Down int64 }
type ldResp struct {
	Total struct {
		Points    []ldPoint
		Span      int64
		TotalUp   int64
		TotalDown int64
	}
	Servers []struct {
		NodeId   uint
		Name     string
		OK       bool
		Disabled bool
		Error    string
		Up, Down int64
		Series   struct{ Points []ldPoint }
	}
	Onlines     []lineOnline
	OnlineUsers int
	OnlineIPs   int
}

func newLDRunner(t *testing.T, name string, settings map[string]string) *runner.Runner {
	t.Helper()
	p := filepath.Join(t.TempDir(), name)
	if len(settings) > 0 {
		if err := runner.SetSettings(p, settings); err != nil {
			t.Fatal(err)
		}
	}
	run, err := runner.New(p)
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() {
		run.Stop()
		_ = database.Close(run.DB())
	})
	return run
}

func addLineStat(t *testing.T, db *gorm.DB, tag string, ts, up, down int64) {
	t.Helper()
	for _, r := range []model.Stats{{DateTime: ts, Resource: "inbound", Tag: tag, Direction: true, Traffic: up}, {DateTime: ts, Resource: "inbound", Tag: tag, Direction: false, Traffic: down}} {
		if err := db.Create(&r).Error; err != nil {
			t.Fatal(err)
		}
	}
}

func lineDetailCall(t *testing.T, s *Server, method, path string) (int, ldResp, map[string]json.RawMessage) {
	t.Helper()
	rec := httptest.NewRecorder()
	s.handleLineItem(rec, httptest.NewRequest(method, path, nil))
	var out ldResp
	var raw map[string]json.RawMessage
	_ = json.Unmarshal(rec.Body.Bytes(), &out)
	_ = json.Unmarshal(rec.Body.Bytes(), &raw)
	return rec.Code, out, raw
}

// 线路详情:主机向部署了这条线路的副机现取流量(真实 agent 接口),和本机的合成总量,按服务器分开列;
// 联系不上 / 停用 / 版本太旧没有这个接口的服务器在表里标出原因、不算进合计;别的线路的流量不混进来。
func TestLineDetailMergesServers(t *testing.T) {
	master := newLDRunner(t, "master.db", nil)
	ms := NewServer(master)
	mdb := master.DB()
	var local model.Node
	if err := mdb.Where("is_local = ?", true).First(&local).Error; err != nil {
		t.Fatal("前提:主机应有本机记录: ", err)
	}

	const token = "ld-agent-token-0123456789"
	node := newLDRunner(t, "node.db", map[string]string{"nodeMode": "true", "nodeToken": token})
	ns := NewServer(node)
	mux := http.NewServeMux()
	mux.HandleFunc(innerBase+"api/agent/", ns.handleAgent)
	agentSrv := httptest.NewServer(mux)
	defer agentSrv.Close()
	oldSrv := httptest.NewServer(http.NotFoundHandler()) // 旧版副机:没有 line-stats
	defer oldSrv.Close()
	dead := httptest.NewServer(http.NotFoundHandler())
	deadURL := dead.URL
	dead.Close()

	a := model.Node{Name: "副机A", ApiUrl: agentSrv.URL + "/app/", Token: token, Enabled: true, Sort: 2}
	b := model.Node{Name: "联系不上的", ApiUrl: deadURL + "/app/", Token: "x", Enabled: true, Sort: 3}
	c := model.Node{Name: "停用的", ApiUrl: agentSrv.URL + "/app/", Token: token, Enabled: false, Sort: 4}
	o := model.Node{Name: "旧版的", ApiUrl: oldSrv.URL + "/app/", Token: "x", Enabled: true, Sort: 5}
	for _, n := range []*model.Node{&a, &b, &c, &o} {
		if err := mdb.Create(n).Error; err != nil {
			t.Fatal(err)
		}
	}
	mdb.Model(&model.Node{}).Where("id = ?", c.Id).Update("enabled", false) // 建记录时 false 会被列默认值 true 顶掉
	line := model.Line{Name: "L1", Protocol: "mixed", Port: 30001, Enabled: true, Options: json.RawMessage(`{}`)}
	mdb.Create(&line)

	now := time.Now().Unix()
	t1, t2 := now-2*3600, now-5*3600
	addLineStat(t, mdb, "L1", t1, 100, 1000)
	addLineStat(t, mdb, "L2", t1, 9999, 9999) // 别的线路
	addLineStat(t, node.DB(), "L1", t1, 50, 500)
	addLineStat(t, node.DB(), "L1", t2, 7, 70)

	code, d, _ := lineDetailCall(t, ms, http.MethodGet, fmt.Sprintf("/app/api/lines/%d/detail?hours=24&bucket=3600&tz=0", line.Id))
	if code != http.StatusOK {
		t.Fatalf("应 200,实际 %d", code)
	}
	if len(d.Servers) != 5 {
		t.Fatalf("线路部署在全部 5 台上,应列 5 台,实际 %d: %+v", len(d.Servers), d.Servers)
	}
	if d.Servers[0].Name != "主机" || d.Servers[0].Up != 100 || d.Servers[0].Down != 1000 || !d.Servers[0].OK {
		t.Fatalf("流量最多的本机排第一、只算 L1: %+v", d.Servers[0])
	}
	if d.Servers[1].Name != "副机A" || !d.Servers[1].OK || d.Servers[1].Up != 57 || d.Servers[1].Down != 570 {
		t.Fatalf("副机 A 应现取到自己库里 L1 的流量 57/570: %+v", d.Servers[1])
	}
	nz := 0
	for _, p := range d.Servers[1].Series.Points {
		if p.Up+p.Down > 0 {
			nz++
		}
	}
	if nz != 2 {
		t.Fatalf("副机 A 的时序应有两格有流量,实际 %d", nz)
	}
	want := map[string]string{"联系不上的": "联系不上这台服务器", "停用的": "服务器已停用", "旧版的": "这台服务器的版本太旧,升级后才有按线路的流量"}
	for _, sv := range d.Servers[2:] {
		if sv.OK || sv.Error != want[sv.Name] || sv.Up+sv.Down != 0 || sv.Disabled != (sv.Name == "停用的") {
			t.Fatalf("%s 应标出原因 %q、不算流量: %+v", sv.Name, want[sv.Name], sv)
		}
	}
	if d.Total.TotalUp != 157 || d.Total.TotalDown != 1570 {
		t.Fatalf("合计应是本机 + 副机 A = 157/1570,实际 %d/%d", d.Total.TotalUp, d.Total.TotalDown)
	}
	var sumUp, atT1 int64
	for _, p := range d.Total.Points {
		sumUp += p.Up
		if p.T <= t1 && t1 < p.T+d.Total.Span {
			atT1 = p.Up
		}
	}
	if sumUp != 157 || atT1 != 150 {
		t.Fatalf("合计时序:各格相加 157(实际 %d),两小时前那格是两台相加 150(实际 %d)", sumUp, atT1)
	}

	// 只部署在本机和副机 A 上的线路:只列这两台
	only := model.Line{Name: "L3", Protocol: "mixed", Port: 30003, Enabled: true, Options: json.RawMessage(`{}`), NodeIds: json.RawMessage(fmt.Sprintf("[%d,%d]", local.Id, a.Id))}
	mdb.Create(&only)
	if code, d, _ := lineDetailCall(t, ms, http.MethodGet, fmt.Sprintf("/app/api/lines/%d/detail", only.Id)); code != http.StatusOK || len(d.Servers) != 2 {
		t.Fatalf("只部署在两台上的线路应只列两台: %d %+v", code, d.Servers)
	}

	// 只要在线部分:不向副机取流量,在线列表是 [] 不是 null
	code, _, raw := lineDetailCall(t, ms, http.MethodGet, fmt.Sprintf("/app/api/lines/%d/detail?part=online", line.Id))
	if code != http.StatusOK || string(raw["onlines"]) != "[]" || raw["servers"] != nil || raw["total"] != nil {
		t.Fatalf("part=online 只回在线部分: %d %v", code, raw)
	}

	// 边界:不存在的线路 404、写方法 405、没有编号的 /lines/detail 不崩
	if code, _, _ := lineDetailCall(t, ms, http.MethodGet, "/app/api/lines/9999/detail"); code != http.StatusNotFound {
		t.Fatalf("不存在的线路应 404,实际 %d", code)
	}
	if code, _, _ := lineDetailCall(t, ms, http.MethodPost, fmt.Sprintf("/app/api/lines/%d/detail", line.Id)); code != http.StatusMethodNotAllowed {
		t.Fatalf("POST 应 405,实际 %d", code)
	}
	if code, _, _ := lineDetailCall(t, ms, http.MethodGet, "/app/api/lines/detail"); code == http.StatusOK {
		t.Fatal("/lines/detail 没有编号,不该当成详情")
	}

	// 副机接口:缺线路名 400;带上就回本机这条线路的时序
	call := func(q string) (int, ldResp) {
		req, _ := http.NewRequest(http.MethodGet, agentSrv.URL+"/app/api/agent/line-stats"+q, nil)
		req.Header.Set("X-Agent-Token", token)
		resp, err := http.DefaultClient.Do(req)
		if err != nil {
			t.Fatal(err)
		}
		defer resp.Body.Close()
		var r struct{ TotalUp, TotalDown int64 }
		_ = json.NewDecoder(resp.Body).Decode(&r)
		var out ldResp
		out.Total.TotalUp, out.Total.TotalDown = r.TotalUp, r.TotalDown
		return resp.StatusCode, out
	}
	if code, _ := call(""); code != http.StatusBadRequest {
		t.Fatalf("缺线路名应 400,实际 %d", code)
	}
	if code, r := call("?tag=L1&hours=24&bucket=3600&tz=0"); code != http.StatusOK || r.Total.TotalUp != 57 || r.Total.TotalDown != 570 {
		t.Fatalf("副机接口应回 L1 的 57/570: %d %+v", code, r.Total)
	}
	req, _ := http.NewRequest(http.MethodGet, agentSrv.URL+"/app/api/agent/line-stats?tag=L1", nil)
	req.Header.Set("X-Agent-Token", "wrong-token-0123456789")
	if resp, err := http.DefaultClient.Do(req); err != nil || resp.StatusCode != http.StatusUnauthorized {
		t.Fatalf("令牌不对应 401: %v %v", err, resp)
	} else {
		resp.Body.Close()
	}

	// 副机自己的面板:只有本机这一份,不去联系别的服务器
	ndb := node.DB()
	ndb.Create(&model.Node{Name: "主机", Enabled: true, Sort: 1})
	self := model.Node{Name: "副机A", IsLocal: true, Enabled: true, Sort: 2}
	ndb.Create(&self)
	ndb.Create(&model.Node{Name: "别的副机", ApiUrl: deadURL + "/app/", Enabled: true, Sort: 3})
	nl := model.Line{Name: "L1", Protocol: "mixed", Port: 30001, Enabled: true, Options: json.RawMessage(`{}`)}
	ndb.Create(&nl)
	code, d, _ = lineDetailCall(t, ns, http.MethodGet, "/app/api/lines/"+strconv.Itoa(int(nl.Id))+"/detail?hours=24&bucket=3600&tz=0")
	if code != http.StatusOK || len(d.Servers) != 1 || d.Servers[0].NodeId != self.Id || !d.Servers[0].OK || d.Total.TotalUp != 57 {
		t.Fatalf("副机面板只列本机: %d %+v total=%+v", code, d.Servers, d.Total)
	}
}

// 在线用户:只挑连着这条线路的 IP;已删除的服务器留下的旧报告不算;本机编号出现在远端报告里不重复算;按用户、服务器排序,IP 排好。
func TestCollectLineOnlines(t *testing.T) {
	nodes := []model.Node{{Id: 1, Name: "主机", IsLocal: true}, {Id: 2, Name: "副机A"}}
	local := map[string]map[string][]string{
		"bob": {"1.1.1.1": {"L1"}, "2.2.2.2": {"L2"}},
		"amy": {"9.9.9.9": {"L2", "L1"}, "3.3.3.3": {"L1"}},
		"cat": {"4.4.4.4": {"L2"}},
	}
	remote := map[uint]map[string]map[string][]string{
		2: {"amy": {"5.5.5.5": {"L1"}}},
		9: {"ghost": {"6.6.6.6": {"L1"}}},
		1: {"dup": {"7.7.7.7": {"L1"}}},
	}
	got := collectLineOnlines("L1", nodes, local, remote)
	want := []lineOnline{
		{User: "amy", NodeId: 1, Server: "主机", IPs: []string{"3.3.3.3", "9.9.9.9"}},
		{User: "amy", NodeId: 2, Server: "副机A", IPs: []string{"5.5.5.5"}},
		{User: "bob", NodeId: 1, Server: "主机", IPs: []string{"1.1.1.1"}},
	}
	if fmt.Sprint(got) != fmt.Sprint(want) {
		t.Fatalf("在线用户:\n got %v\nwant %v", got, want)
	}
	if out := collectLineOnlines("L1", nil, nil, nil); out == nil || len(out) != 0 {
		t.Fatal("没人在线时应是空列表(JSON 为 []),不是 nil")
	}
}
