package web

import (
	"encoding/json"
	"net/http/httptest"
	"path/filepath"
	"testing"
	"time"

	"github.com/Maoyangui/m-ui/database"
	"github.com/Maoyangui/m-ui/database/model"
	"github.com/Maoyangui/m-ui/runner"
)

// 流量明细:同一个合计按用户 / 服务器 / 线路三种分法各自对得上;时间范围外的不算;
// 中转入口的入站不重复计;联系不上的副机列进 linesMissing;删掉的用户、已不在的服务器标 gone。
func TestStatsBreakdown(t *testing.T) {
	run, err := runner.New(filepath.Join(t.TempDir(), "m.db"))
	if err != nil {
		t.Fatal(err)
	}
	defer database.Close(run.DB())
	s := NewServer(run)
	db := run.DB()

	// 本机倍率 2;一台副机(没联系上:问不到它的线路流量)
	var local model.Node
	if db.Where("is_local = ?", true).First(&local).Error != nil {
		local = model.Node{Name: "主机", IsLocal: true, Enabled: true, Sort: 1}
		db.Create(&local)
	}
	db.Model(&model.Node{}).Where("id = ?", local.Id).Updates(map[string]interface{}{"name": "hk", "ratio": 2.0})
	db.Create(&model.Node{Name: "tw", ApiUrl: "http://127.0.0.1:1/app/", Token: "x", Enabled: true, Ratio: 1, Sort: 2})

	rs := model.Reseller{Name: "rs1", Enabled: true}
	db.Create(&rs)
	db.Create(&model.User{Name: "alice", Enabled: true, SubToken: "a1a1a1a1a1a1a1a1a1a1a1a1", Credentials: []byte(`{}`)})
	db.Create(&model.User{Name: "bob", Enabled: true, SubToken: "b1b1b1b1b1b1b1b1b1b1b1b1", Credentials: []byte(`{}`), ResellerId: rs.Id})

	now := time.Now().Unix()
	add := func(at int64, resource, tag string, up, down int64) {
		t.Helper()
		if up > 0 {
			if err := db.Create(&model.Stats{DateTime: at, Resource: resource, Tag: tag, Direction: true, Traffic: up}).Error; err != nil {
				t.Fatal(err)
			}
		}
		if down > 0 {
			if err := db.Create(&model.Stats{DateTime: at, Resource: resource, Tag: tag, Direction: false, Traffic: down}).Error; err != nil {
				t.Fatal(err)
			}
		}
	}
	recent, old := now-600, now-3*86400
	add(recent, "user", "alice", 10, 100)
	add(recent, "user", "bob", 0, 50)
	add(recent, "user", "ghost", 0, 5) // 用户已经删了,流量记录还在
	add(old, "user", "alice", 1000, 1000)
	add(recent, "node", "tw", 3, 30)
	add(recent, "node", "gone-node", 0, 7) // 那台服务器已经不在
	add(recent, "inbound", "line-a", 1, 20)
	add(recent, "inbound", "line-a ⇄13024", 0, 999) // 中转入口的入站:出口那台的线路上已经算过
	add(old, "inbound", "line-a", 500, 500)

	type item struct {
		Name     string `json:"name"`
		Up, Down int64
		Owner    string `json:"owner"`
		Gone     bool   `json:"gone"`
		Local    bool   `json:"local"`
	}
	type resp struct {
		Hours        int      `json:"hours"`
		Total        item     `json:"total"`
		Users        []item   `json:"users"`
		UserCount    int      `json:"userCount"`
		Nodes        []item   `json:"nodes"`
		Lines        []item   `json:"lines"`
		LinesMissing []string `json:"linesMissing"`
	}
	get := func(q string) resp {
		t.Helper()
		w := httptest.NewRecorder()
		s.handleStatsBreakdown(w, httptest.NewRequest("GET", "http://x/app/api/stats/breakdown"+q, nil))
		if w.Code != 200 {
			t.Fatalf("%s: %d %s", q, w.Code, w.Body.String())
		}
		var out resp
		if err := json.Unmarshal(w.Body.Bytes(), &out); err != nil {
			t.Fatalf("%s: %v %s", q, err, w.Body.String())
		}
		return out
	}

	d := get("?hours=24")
	if d.Hours != 24 || d.Total.Up != 10 || d.Total.Down != 155 || d.UserCount != 3 {
		t.Fatalf("合计不对: %+v", d)
	}
	if len(d.Users) != 3 || d.Users[0].Name != "alice" || d.Users[1].Name != "bob" || d.Users[2].Name != "ghost" {
		t.Fatalf("用户排行不对: %+v", d.Users)
	}
	if d.Users[0].Gone || d.Users[0].Owner != "" || d.Users[1].Owner != "rs1" || !d.Users[2].Gone {
		t.Fatalf("用户的归属 / 已删标记不对: %+v", d.Users)
	}
	// 服务器:本机 = 本机线路合计 × 本机倍率(中转入口不算);副机 = 并账时记下的;已不在的标出来
	if len(d.Nodes) != 3 || d.Nodes[0].Name != "hk" || !d.Nodes[0].Local || d.Nodes[0].Up != 2 || d.Nodes[0].Down != 40 {
		t.Fatalf("本机那一行不对: %+v", d.Nodes)
	}
	if d.Nodes[1].Name != "tw" || d.Nodes[1].Gone || d.Nodes[1].Down != 30 || d.Nodes[2].Name != "gone-node" || !d.Nodes[2].Gone {
		t.Fatalf("副机那几行不对: %+v", d.Nodes)
	}
	// 线路:只有本机的那条(按本机倍率),中转入口那条入站不出现;没联系上的副机列出来
	if len(d.Lines) != 1 || d.Lines[0].Name != "line-a" || d.Lines[0].Up != 2 || d.Lines[0].Down != 40 {
		t.Fatalf("线路不对: %+v", d.Lines)
	}
	if len(d.LinesMissing) != 1 || d.LinesMissing[0] != "tw" {
		t.Fatalf("没问到的服务器应该列出来: %+v", d.LinesMissing)
	}

	// 范围放宽到 7 天:三天前的那些也算进来
	w := get("?hours=168")
	if w.Total.Up != 1010 || w.Total.Down != 1155 || w.Lines[0].Up != 1002 || w.Lines[0].Down != 1040 {
		t.Fatalf("7 天的合计不对: %+v", w)
	}
	// 乱填的范围按 24 小时
	if x := get("?hours=-5"); x.Hours != 24 || x.Total.Down != 155 {
		t.Fatalf("范围不合法时应按 24 小时: %+v", x)
	}

	// 副机面板:只给用户那一栏
	if err := db.Exec("INSERT INTO settings(key, value) VALUES('nodeMode', 'true') ON CONFLICT(key) DO UPDATE SET value = 'true'").Error; err != nil {
		t.Fatal(err)
	}
	if n := get("?hours=24"); n.Nodes != nil || n.Lines != nil || len(n.Users) != 3 {
		t.Fatalf("副机面板不该有服务器 / 线路两栏: %+v", n)
	}
}
