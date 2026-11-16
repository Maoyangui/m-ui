package web

import (
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"path/filepath"
	"strconv"
	"strings"
	"testing"

	"github.com/Maoyangui/m-ui/database"
	"github.com/Maoyangui/m-ui/database/model"
	"github.com/Maoyangui/m-ui/runner"

	"gorm.io/gorm"
)

// PUT /lines/{id}/node:改一个节点的设置 —— 只动这条线路的 node_opts(和这个入口手填的来源 IP),别的原样。
func TestLineNodeAPI(t *testing.T) {
	run, err := runner.New(filepath.Join(t.TempDir(), "node.db"))
	if err != nil {
		t.Fatal(err)
	}
	defer database.Close(run.DB())
	s, db := NewServer(run), run.DB()
	var local model.Node
	if db.Where("is_local = ?", true).First(&local).Error != nil {
		db.Create(&model.Node{Name: "主机", IsLocal: true, Enabled: true, Sort: 1})
	}
	itoa := func(n uint) string { return strconv.FormatUint(uint64(n), 10) }
	doJSON := func(method, path, body string) *httptest.ResponseRecorder {
		w := httptest.NewRecorder()
		s.handleLineItem(w, httptest.NewRequest(method, "http://x/app/api"+path, strings.NewReader(body)))
		return w
	}
	exit := model.Node{Name: "出口", Enabled: true}
	db.Create(&exit)
	relay := model.Node{Name: "中转", Enabled: true}
	db.Create(&relay)
	fs, _ := json.Marshal([]model.LineForward{{Id: 1, Via: relay.Id, Port: 30001, To: exit.Id}})
	ids, _ := json.Marshal([]uint{exit.Id})
	line := model.Line{Name: "a", Protocol: "trojan", Port: 443, Enabled: true, NodeIds: ids, Forwards: fs,
		NodeOpts: []byte(`{"f1":{"hidden":true}}`)}
	if err := db.Create(&line).Error; err != nil {
		t.Fatal(err)
	}
	put := func(body string) (int, string) {
		t.Helper()
		w := doJSON(http.MethodPut, "/lines/"+itoa(line.Id)+"/node", body)
		return w.Code, w.Body.String()
	}
	reload := func() model.Line {
		t.Helper()
		var l model.Line
		if err := db.First(&l, line.Id).Error; err != nil {
			t.Fatal(err)
		}
		return l
	}
	direct := model.DirectKey(exit.Id)

	// 直连节点设倍率和限速:写进 node_opts,原来入口 1 的设置还在
	if code, body := put(`{"key":"` + direct + `","opt":{"ratio":2.5,"speed":100,"userSpeed":20,"cap":30}}`); code != http.StatusOK {
		t.Fatalf("设置直连节点失败:%d %s", code, body)
	}
	opts := model.ParseNodeOpts(reload().NodeOpts)
	if opts[direct] != (model.NodeOpt{Ratio: 2.5, Speed: 100, UserSpeed: 20, Cap: 30}) || !opts["f1"].Hidden {
		t.Fatalf("node_opts 不对:%+v", opts)
	}
	// 整份替换:只留倍率
	if code, body := put(`{"key":"` + direct + `","opt":{"ratio":3}}`); code != http.StatusOK {
		t.Fatalf("%d %s", code, body)
	}
	if got := model.ParseNodeOpts(reload().NodeOpts)[direct]; got != (model.NodeOpt{Ratio: 3}) {
		t.Fatalf("应整份替换成只有倍率:%+v", got)
	}
	// 全清:这个键从 node_opts 里去掉
	if code, body := put(`{"key":"` + direct + `","opt":{}}`); code != http.StatusOK {
		t.Fatalf("%d %s", code, body)
	}
	if _, has := model.ParseNodeOpts(reload().NodeOpts)[direct]; has {
		t.Fatalf("恢复默认后不该还留着这个键:%s", reload().NodeOpts)
	}
	// 中转入口:设置 + 手填来源 IP(规整后存进 forwards),入口别的字段不动
	if code, body := put(`{"key":"f1","opt":{"ratio":5},"src":" 192.0.2.7, ::ffff:192.0.2.8 "}`); code != http.StatusOK {
		t.Fatalf("%d %s", code, body)
	}
	l := reload()
	got := model.ParseForwards(l.Forwards)
	if len(got) != 1 || got[0].Src != "192.0.2.7,192.0.2.8" || got[0].Port != 30001 || got[0].Via != relay.Id || got[0].To != exit.Id {
		t.Fatalf("入口不对:%+v", got)
	}
	if o := model.ParseNodeOpts(l.NodeOpts)["f1"]; o != (model.NodeOpt{Ratio: 5}) {
		t.Fatalf("入口的设置应整份替换:%+v", o)
	}
	// 不带 src:来源 IP 不动
	if code, body := put(`{"key":"f1","opt":{"ratio":4}}`); code != http.StatusOK {
		t.Fatalf("%d %s", code, body)
	}
	if got := model.ParseForwards(reload().Forwards); got[0].Src != "192.0.2.7,192.0.2.8" {
		t.Fatalf("没带 src 不该动来源 IP:%+v", got)
	}
	// 线路的其它字段没被碰过
	if l := reload(); l.Name != "a" || l.Port != 443 || string(l.NodeIds) != string(ids) || !l.Enabled {
		t.Fatalf("线路别的字段被改了:%+v", l)
	}

	// 拒绝:不存在的节点、范围外的值、直连带来源 IP、来源不是 IP、线路不存在
	for name, body := range map[string]string{
		"没有这个节点":   `{"key":"s999","opt":{"ratio":2}}`,
		"没有这个入口":   `{"key":"f9","opt":{"ratio":2}}`,
		"倍率超范围":    `{"key":"` + direct + `","opt":{"ratio":101}}`,
		"倍率是负的":    `{"key":"` + direct + `","opt":{"ratio":-1}}`,
		"限速超范围":    `{"key":"` + direct + `","opt":{"speed":100001}}`,
		"容量是负的":    `{"key":"` + direct + `","opt":{"cap":-3}}`,
		"直连带来源 IP": `{"key":"` + direct + `","opt":{},"src":"192.0.2.1"}`,
		"来源不是 IP":  `{"key":"f1","opt":{},"src":"relay.example.com"}`,
		"不是 JSON":  `{`,
	} {
		before := string(reload().NodeOpts)
		if code, resp := put(body); code != http.StatusBadRequest {
			t.Errorf("%s 应 400:%d %s", name, code, resp)
		}
		if after := string(reload().NodeOpts); after != before {
			t.Errorf("%s 被拒了却改了库:%s → %s", name, before, after)
		}
	}
	if w := doJSON(http.MethodPut, "/lines/99999/node", `{"key":"s1","opt":{}}`); w.Code != http.StatusNotFound {
		t.Errorf("线路不存在应 404:%d", w.Code)
	}
	if w := doJSON(http.MethodPost, "/lines/"+itoa(line.Id)+"/node", `{}`); w.Code != http.StatusMethodNotAllowed {
		t.Errorf("只接受 PUT:%d", w.Code)
	}
	// 操作记录里有这一笔
	var n int64
	db.Model(&model.Change{}).Where("action = ?", "node").Count(&n)
	if n == 0 {
		t.Errorf("改节点设置没有进操作记录")
	}
}

// PUT /lines/nodes:勾选的一批节点改同样的几项 —— set 里没带的项不动;有一个节点不在就整批不做。
func TestLineNodesBatchAPI(t *testing.T) {
	run, err := runner.New(filepath.Join(t.TempDir(), "nodes.db"))
	if err != nil {
		t.Fatal(err)
	}
	defer database.Close(run.DB())
	s, db := NewServer(run), run.DB()
	var local model.Node
	if db.Where("is_local = ?", true).First(&local).Error != nil {
		local = model.Node{Name: "主机", IsLocal: true, Enabled: true, Sort: 1}
		db.Create(&local)
	}
	exit := model.Node{Name: "出口", Enabled: true}
	db.Create(&exit)
	fs, _ := json.Marshal([]model.LineForward{{Id: 1, Via: local.Id, Port: 30001, To: exit.Id}})
	ids, _ := json.Marshal([]uint{exit.Id})
	a := model.Line{Name: "a", Protocol: "trojan", Port: 443, Enabled: true, NodeIds: ids, Forwards: fs,
		NodeOpts: []byte(`{"f1":{"hidden":true,"cap":9}}`)}
	b := model.Line{Name: "b", Protocol: "trojan", Port: 444, Enabled: true}
	for _, l := range []*model.Line{&a, &b} {
		if err := db.Create(l).Error; err != nil {
			t.Fatal(err)
		}
	}
	itoa := func(n uint) string { return strconv.FormatUint(uint64(n), 10) }
	put := func(body string) (int, string) {
		t.Helper()
		w := httptest.NewRecorder()
		s.handleLineItem(w, httptest.NewRequest(http.MethodPut, "http://x/app/api/lines/nodes", strings.NewReader(body)))
		return w.Code, w.Body.String()
	}
	optsOf := func(id uint) map[string]model.NodeOpt {
		t.Helper()
		var l model.Line
		if err := db.First(&l, id).Error; err != nil {
			t.Fatal(err)
		}
		return model.ParseNodeOpts(l.NodeOpts)
	}
	direct, bLocal, bExit := model.DirectKey(exit.Id), model.DirectKey(local.Id), model.DirectKey(exit.Id)
	items := `[{"line":` + itoa(a.Id) + `,"key":"` + direct + `"},{"line":` + itoa(a.Id) + `,"key":"f1"},{"line":` + itoa(b.Id) + `,"key":"` + bLocal + `"}]`

	// 只改倍率:三个节点都变,入口 1 原来的「不输出」和容量还在
	code, body := put(`{"items":` + items + `,"set":{"ratio":2.5}}`)
	if code != http.StatusOK {
		t.Fatalf("批量设倍率失败:%d %s", code, body)
	}
	var res struct {
		Lines   []model.Line `json:"lines"`
		Changed int          `json:"changed"`
	}
	if err := json.Unmarshal([]byte(body), &res); err != nil || len(res.Lines) != 2 || res.Changed != 3 {
		t.Fatalf("返回的应是改到的两条线路、三个节点:%v %s", err, body)
	}
	oa, ob := optsOf(a.Id), optsOf(b.Id)
	if oa[direct] != (model.NodeOpt{Ratio: 2.5}) || oa["f1"] != (model.NodeOpt{Hidden: true, Ratio: 2.5, Cap: 9}) || ob[bLocal] != (model.NodeOpt{Ratio: 2.5}) {
		t.Fatalf("批量设倍率后:a=%+v b=%+v", oa, ob)
	}
	if _, has := ob[bExit]; has {
		t.Fatalf("没选的节点不该被改:%+v", ob)
	}
	// 只改输出和限速:倍率不动
	if code, body := put(`{"items":` + items + `,"set":{"hidden":false,"speed":100}}`); code != http.StatusOK {
		t.Fatalf("%d %s", code, body)
	}
	if got := optsOf(a.Id)["f1"]; got != (model.NodeOpt{Ratio: 2.5, Cap: 9, Speed: 100}) {
		t.Fatalf("改输出和限速后入口 1 应是 倍率 2.5 / 容量 9 / 限速 100 且输出:%+v", got)
	}
	// 恢复默认:键从 node_opts 里去掉
	if code, body := put(`{"items":` + items + `,"set":{"hidden":false,"ratio":0,"speed":0,"userSpeed":0,"cap":0}}`); code != http.StatusOK {
		t.Fatalf("%d %s", code, body)
	}
	if oa, ob := optsOf(a.Id), optsOf(b.Id); len(oa) != 0 || len(ob) != 0 {
		t.Fatalf("恢复默认后不该还留着设置:a=%+v b=%+v", oa, ob)
	}

	// 拒绝的情形:都不能改到库
	put(`{"items":` + items + `,"set":{"ratio":3}}`)
	before := string(mustLine(t, db, a.Id).NodeOpts) + "|" + string(mustLine(t, db, b.Id).NodeOpts)
	for name, body := range map[string]string{
		"没有要改的项":  `{"items":` + items + `,"set":{}}`,
		"没有选节点":   `{"items":[],"set":{"ratio":2}}`,
		"倍率超范围":   `{"items":` + items + `,"set":{"ratio":500}}`,
		"容量是负的":   `{"items":` + items + `,"set":{"cap":-1}}`,
		"有一个节点不在": `{"items":[{"line":` + itoa(a.Id) + `,"key":"` + direct + `"},{"line":` + itoa(a.Id) + `,"key":"f9"}],"set":{"ratio":9}}`,
		"线路不在":    `{"items":[{"line":` + itoa(a.Id) + `,"key":"` + direct + `"},{"line":99999,"key":"s1"}],"set":{"ratio":9}}`,
		"不是 JSON": `[`,
	} {
		if code, resp := put(body); code != http.StatusBadRequest {
			t.Errorf("%s 应 400:%d %s", name, code, resp)
		}
		if after := string(mustLine(t, db, a.Id).NodeOpts) + "|" + string(mustLine(t, db, b.Id).NodeOpts); after != before {
			t.Errorf("%s 被拒了却改了库:%s → %s", name, before, after)
		}
	}
	w := httptest.NewRecorder()
	s.handleLineItem(w, httptest.NewRequest(http.MethodPost, "http://x/app/api/lines/nodes", strings.NewReader(`{}`)))
	if w.Code != http.StatusMethodNotAllowed {
		t.Errorf("只接受 PUT:%d", w.Code)
	}
	var n int64
	db.Model(&model.Change{}).Where("action = ?", "nodes").Count(&n)
	if n != 4 {
		t.Errorf("成功的四次批量各进一条操作记录,实际 %d 条", n)
	}
}

func mustLine(t *testing.T, db *gorm.DB, id uint) model.Line {
	t.Helper()
	var l model.Line
	if err := db.First(&l, id).Error; err != nil {
		t.Fatal(err)
	}
	return l
}

// 各节点的在线人数:连接在哪台服务器上、连的哪条线路;来源对得上转到这台的中转入口就算那个入口,否则算这台的直连。
// 同一个人几条连接、几台设备只算一个;已删服务器的旧报告、不认识的线路名不算。
func TestNodeOnlines(t *testing.T) {
	nodes := []model.Node{
		{Id: 1, Name: "主机", IsLocal: true},
		{Id: 2, Name: "出口", PublicIP: "198.51.100.2"},
		{Id: 3, Name: "中转", PublicIP: "203.0.113.3"},
	}
	fs, _ := json.Marshal([]model.LineForward{
		{Id: 1, Via: 3, Port: 30001, To: 2},
		{Id: 2, Via: 0, Host: "203.0.113.50", Port: 30002, To: 2},
		{Id: 3, Via: 3, Port: 30003, To: 1},
	})
	lines := []model.Line{{Id: 10, Name: "a", Forwards: fs}, {Id: 11, Name: "b"}}
	local := map[string]map[string][]string{
		"alice": {"8.8.8.8": {"a", "b"}, "8.8.4.4": {"a"}}, // 两台设备连 a:只算一个
		"bob":   {"203.0.113.3": {"a"}},                    // 经入口 3(转到主机)
	}
	remote := map[uint]map[string]map[string][]string{
		2: {
			"alice": {"9.9.9.9": {"a"}},                                  // 出口上的直连
			"carol": {"203.0.113.3": {"a"}},                              // 经集群中转(入口 1)
			"dave":  {"203.0.113.50": {"a"}},                             // 经别处的中转(入口 2)
			"erin":  {"::ffff:203.0.113.3": {"a"}, "203.0.113.3": {"b"}}, // v4 映射的地址也认;线路 b 没有入口:算直连
		},
		9: {"ghost": {"1.1.1.1": {"a"}}}, // 已经删掉的服务器
		1: {"dup": {"1.1.1.1": {"a"}}},   // 报告里出现本机的 id:本机以连接跟踪器为准,不重复算
	}
	got := nodeOnlines(lines, nodes, local, remote)
	want := map[string]int{"10:s1": 1, "11:s1": 1, "10:f3": 1, "10:s2": 1, "10:f1": 2, "10:f2": 1, "11:s2": 1}
	if len(got) != len(want) {
		t.Fatalf("节点在线人数不对:\n got %v\nwant %v", got, want)
	}
	for k, v := range want {
		if got[k] != v {
			t.Errorf("%s 应有 %d 人在线,实际 %d(全部:%v)", k, v, got[k], got)
		}
	}
}
