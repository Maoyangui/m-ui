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
