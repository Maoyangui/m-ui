package hub

import (
	"bytes"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"path/filepath"
	"strings"
	"sync"
	"testing"

	"github.com/Maoyangui/m-ui/database"
	"github.com/Maoyangui/m-ui/database/model"

	"gorm.io/gorm"
)

func nodeLogos(t *testing.T, db *gorm.DB) map[uint]model.PageLogo {
	t.Helper()
	var rows []model.PageLogo
	if err := db.Find(&rows).Error; err != nil {
		t.Fatal(err)
	}
	out := map[uint]model.PageLogo{}
	for _, r := range rows {
		out[r.ResellerId] = r
	}
	return out
}

// overWire 和真实推送一样过一遍 JSON(图片字节在 JSON 里是 base64)。
func overWire(t *testing.T, s Snapshot) Snapshot {
	t.Helper()
	b, err := json.Marshal(s)
	if err != nil {
		t.Fatal(err)
	}
	var out Snapshot
	if err := json.Unmarshal(b, &out); err != nil {
		t.Fatal(err)
	}
	return out
}

// 落地页 logo 随快照到副机(副机的订阅口也出落地页):快照总带 logos(一张都没有也是空表,不是 nil);
// 修订号只看摘要 —— 同一张图重新上传(只改了时间)不触发推送,换图才变;副机整表替换;
// 旧版主机不发 logos(nil)时副机也清空(旧版主机没有 logo,副机显示内置的才一致);主机全删了副机跟着清空。
func TestLogosRideTheSnapshot(t *testing.T) {
	master, err := database.Open(filepath.Join(t.TempDir(), "m.db"))
	if err != nil {
		t.Fatal(err)
	}
	defer database.Close(master)
	node, err := database.Open(filepath.Join(t.TempDir(), "n.db"))
	if err != nil {
		t.Fatal(err)
	}
	defer database.Close(node)
	master.Create(&model.Node{Name: "主机", IsLocal: true, Enabled: true})
	master.Create(&model.Node{Name: "B", ApiUrl: "http://b", Enabled: true})
	setting := func(string) string { return "" }
	apply := func(snap Snapshot) {
		t.Helper()
		snap = overWire(t, snap)
		snap.SelfNodeId = 2
		if _, _, err := ApplySnapshot(node, snap); err != nil {
			t.Fatal(err)
		}
	}

	snap0, err := BuildSnapshot(master, setting)
	if err != nil {
		t.Fatal(err)
	}
	if snap0.Logos == nil || len(snap0.Logos) != 0 {
		t.Fatalf("没有 logo 时也要发空表(不是 nil): %#v", snap0.Logos)
	}
	if b, _ := json.Marshal(snap0); !strings.Contains(string(b), `"logos":[]`) {
		t.Fatal("快照 JSON 里应带 \"logos\":[]")
	}
	node.Create(&model.PageLogo{ResellerId: 7, Mime: "image/png", Data: []byte{1, 2, 3}, Sum: "stale"}) // 副机上留着的旧图
	apply(snap0)
	if rows := nodeLogos(t, node); len(rows) != 0 {
		t.Fatalf("主机一张都没有,副机应整表清空: %+v", rows)
	}

	imgA, imgB := []byte("\x89PNG\r\n\x1a\nmaster-logo"), []byte("GIF89a-reseller-logo")
	master.Create(&model.PageLogo{ResellerId: 0, Mime: "image/png", Data: imgA, Sum: "aaaaaaaaaaaaaaaa", Width: 480, Height: 160, UpdatedAt: 100})
	rs := model.Reseller{Name: "r", Enabled: true}
	master.Create(&rs)
	master.Create(&model.PageLogo{ResellerId: rs.Id, Mime: "image/gif", Data: imgB, Sum: "bbbbbbbbbbbbbbbb", Width: 120, Height: 40, UpdatedAt: 100})
	snap1, _ := BuildSnapshot(master, setting)
	if len(snap1.Logos) != 2 || snap1.Logos[0].ResellerId != 0 || snap1.Logos[1].ResellerId != rs.Id || !bytes.Equal(snap1.Logos[0].Data, imgA) {
		t.Fatalf("快照应按归属带上两张 logo(连图片字节): %+v", snap1.Logos)
	}
	if snap1.Revision == snap0.Revision {
		t.Fatal("加了 logo 修订号应变")
	}
	apply(snap1)
	rows := nodeLogos(t, node)
	if len(rows) != 2 || !bytes.Equal(rows[0].Data, imgA) || rows[0].Mime != "image/png" || rows[0].Sum != "aaaaaaaaaaaaaaaa" ||
		!bytes.Equal(rows[rs.Id].Data, imgB) || rows[rs.Id].Mime != "image/gif" || rows[rs.Id].Width != 120 {
		t.Fatalf("副机应和主机一模一样: %+v", rows)
	}

	// 同一张图重新上传:只有时间变了,修订号不变(不为此推一轮);每 5 秒算修订号也不必哈希图片字节
	master.Model(&model.PageLogo{}).Where("reseller_id = ?", 0).Update("updated_at", 200)
	if snap2, _ := BuildSnapshot(master, setting); snap2.Revision != snap1.Revision {
		t.Fatal("同一张图重新上传不该改变修订号")
	}
	// 换图:摘要变了,修订号跟着变
	master.Model(&model.PageLogo{}).Where("reseller_id = ?", 0).Updates(map[string]interface{}{"data": []byte("\x89PNG\r\n\x1a\nnew"), "sum": "cccccccccccccccc"})
	snap3, _ := BuildSnapshot(master, setting)
	if snap3.Revision == snap1.Revision {
		t.Fatal("换了 logo 修订号应变")
	}

	// 旧版主机的快照没有 logos(解码为 nil):副机清空本机的。旧版主机没有 logo 功能,副机留着的
	// (本机当主机时传的、之前接过的别的主机推来的)在副机上改不了也删不掉,还会按代理 id 撞到别家代理的用户头上
	old := snap3
	old.Logos = nil
	apply(old)
	if rows := nodeLogos(t, node); len(rows) != 0 {
		t.Fatalf("旧版主机的快照没有 logo,副机应清空: %+v", rows)
	}
	apply(snap3)
	if rows := nodeLogos(t, node); rows[0].Sum != "cccccccccccccccc" || !bytes.Equal(rows[0].Data, []byte("\x89PNG\r\n\x1a\nnew")) {
		t.Fatalf("换图后副机应拿到新图: %+v", rows[0])
	}

	// 主机把 logo 全删了:副机跟着清空(删代理时他的 logo 也随之消失)
	master.Session(&gorm.Session{AllowGlobalUpdate: true}).Delete(&model.PageLogo{})
	snap4, _ := BuildSnapshot(master, setting)
	apply(snap4)
	if rows := nodeLogos(t, node); len(rows) != 0 {
		t.Fatalf("主机删光了,副机也应清空: %+v", rows)
	}
}

// 下线推送(空用户表)不带 logo:图片字节不落进下线通知记录、不随每轮重试重推;副机收到的是 nil,清掉本机的
// (它已经不为任何人出落地页,重新启用时推回的完整快照会带回来)。
func TestDecommissionCarriesNoLogos(t *testing.T) {
	db, err := database.Open(filepath.Join(t.TempDir(), "x.db"))
	if err != nil {
		t.Fatal(err)
	}
	defer database.Close(db)
	fake := newRecordingNode(t)
	db.Create(&model.Node{Name: "本机", IsLocal: true, Enabled: true})
	node := model.Node{Name: "n1", ApiUrl: fake.srv.URL + "/app/", Token: "t", Enabled: true}
	db.Create(&node)
	db.Create(&model.Line{Name: "l1", Protocol: "vless", Port: 20001, Enabled: true, NodeIds: json.RawMessage(`[2]`)})
	db.Create(&model.PageLogo{ResellerId: 0, Mime: "image/png", Data: []byte("\x89PNG\r\n\x1a\nlogo-bytes"), Sum: "dddddddddddddddd"})
	h := New(Deps{DB: db, Setting: func(string) string { return "" }, IsNode: func() bool { return false }})
	before, err := BuildSnapshot(db, h.d.Setting)
	if err != nil {
		t.Fatal(err)
	}
	if len(before.Logos) != 1 {
		t.Fatalf("前提:完整快照里有 logo,实际 %d 张", len(before.Logos))
	}
	db.Model(&model.Node{}).Where("id = ?", node.Id).Update("enabled", false)
	if err := h.Decommission(node, before); err != nil {
		t.Fatal(err)
	}
	got, n := fake.last()
	if n == 0 || got.Logos != nil {
		t.Fatalf("下线推送不该带 logo(应为 nil): 第 %d 份,%d 张", n, len(got.Logos))
	}
}

// LogosMatch 只看归属与摘要、不看顺序;快照不带(nil,旧版主机)等于一张都没有。
func TestLogosMatch(t *testing.T) {
	a := []model.PageLogo{{ResellerId: 0, Sum: "x"}, {ResellerId: 3, Sum: "y"}}
	for _, c := range []struct {
		name       string
		have, want []model.PageLogo
		ok         bool
	}{
		{"都没有", nil, []model.PageLogo{}, true},
		{"旧版主机不发、本机也没有", []model.PageLogo{}, nil, true},
		{"旧版主机不发、本机留着", a, nil, false},
		{"顺序不同也算一样", []model.PageLogo{a[1], a[0]}, a, true},
		{"换了图", a, []model.PageLogo{{ResellerId: 0, Sum: "x"}, {ResellerId: 3, Sum: "z"}}, false},
		{"归属不同", a, []model.PageLogo{{ResellerId: 0, Sum: "x"}, {ResellerId: 4, Sum: "y"}}, false},
		{"少一张", a[:1], a, false},
		{"本机多一张", a, a[:1], false},
	} {
		if got := LogosMatch(c.have, c.want); got != c.ok {
			t.Errorf("%s: 得 %v,应为 %v", c.name, got, c.ok)
		}
	}
}

// 快照只带主面板与现存代理的 logo:删代理与上传并发留下的没主的行不往副机推。
func TestSnapshotSkipsOrphanLogos(t *testing.T) {
	db, err := database.Open(filepath.Join(t.TempDir(), "m.db"))
	if err != nil {
		t.Fatal(err)
	}
	defer database.Close(db)
	db.Create(&model.Node{Name: "主机", IsLocal: true, Enabled: true})
	rs := model.Reseller{Name: "r", Enabled: true}
	db.Create(&rs)
	png := []byte("\x89PNG\r\n\x1a\nx")
	db.Create(&model.PageLogo{ResellerId: 0, Mime: "image/png", Data: png, Sum: "aaaaaaaaaaaaaaaa"})
	db.Create(&model.PageLogo{ResellerId: rs.Id, Mime: "image/png", Data: png, Sum: "bbbbbbbbbbbbbbbb"})
	db.Create(&model.PageLogo{ResellerId: rs.Id + 40, Mime: "image/png", Data: png, Sum: "cccccccccccccccc"}) // 代理已经删了
	snap, err := BuildSnapshot(db, func(string) string { return "" })
	if err != nil {
		t.Fatal(err)
	}
	if len(snap.Logos) != 2 || snap.Logos[0].ResellerId != 0 || snap.Logos[1].ResellerId != rs.Id {
		t.Fatalf("快照应只带主面板与现存代理的 logo: %+v", snap.Logos)
	}
}

// versionNode 一台假副机:报告里的版本可以改,可以设成拒收快照(版本过低那种 409)。
type versionNode struct {
	srv     *httptest.Server
	mu      sync.Mutex
	version string
	reject  bool
	applied string // 最近应用的修订号
	pushes  int    // 收到的推送次数(含拒收的)
}

func newVersionNode(t *testing.T, version string) *versionNode {
	f := &versionNode{version: version}
	f.srv = httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		f.mu.Lock()
		defer f.mu.Unlock()
		switch {
		case strings.HasSuffix(r.URL.Path, "/agent/apply"):
			var snap Snapshot
			_ = json.NewDecoder(r.Body).Decode(&snap)
			f.pushes++
			if f.reject {
				w.WriteHeader(http.StatusConflict)
				json.NewEncoder(w).Encode(map[string]string{"error": "副机版本低于主机要求的版本,请先升级这台副机"})
				return
			}
			f.applied = snap.Revision
			json.NewEncoder(w).Encode(map[string]string{"ok": "1", "revision": snap.Revision})
		case strings.HasSuffix(r.URL.Path, "/agent/report"):
			json.NewEncoder(w).Encode(Report{Version: f.version, Revision: f.applied, CoreRunning: true})
		default:
			w.Write([]byte(`{"ok":"1"}`))
		}
	}))
	t.Cleanup(f.srv.Close)
	return f
}

func (f *versionNode) set(version string, reject bool) {
	f.mu.Lock()
	f.version, f.reject = version, reject
	f.mu.Unlock()
}

func (f *versionNode) count() int {
	f.mu.Lock()
	defer f.mu.Unlock()
	return f.pushes
}

// 副机换了版本(升级 / 降级):主机下一轮就重推一次同一份快照,不等 10 分钟的定期重推,也不等之前被拒的退避。
// 旧版副机收快照时丢掉了自己不认识的 logo、修订号却照记;升级后要靠这一推(新版按"本版程序应用过"才直接确认)整份应用。
func TestNodeVersionChangeTriggersRepush(t *testing.T) {
	db, err := database.Open(filepath.Join(t.TempDir(), "m.db"))
	if err != nil {
		t.Fatal(err)
	}
	defer database.Close(db)
	db.Create(&model.Node{Name: "主机", IsLocal: true, Enabled: true})
	db.Create(&model.PageLogo{ResellerId: 0, Mime: "image/png", Data: []byte("\x89PNG\r\n\x1a\nx"), Sum: "aaaaaaaaaaaaaaaa"})
	h := New(Deps{DB: db, Setting: func(string) string { return "" }, IsNode: func() bool { return false }, Version: "0.7.17"})
	defer h.Stop()
	round := func() { h.tick(); h.settle() }

	// 1. 正常同步后,版本不变就不再推
	fake := newVersionNode(t, "0.7.16")
	node := model.Node{Name: "n1", ApiUrl: fake.srv.URL + "/app/", Token: "t", Enabled: true}
	db.Create(&node)
	round()
	round()
	if n := fake.count(); n != 1 {
		t.Fatalf("前提:同一修订只推一次,实际 %d 次", n)
	}
	// 2. 副机升级:这一轮的报告里看到新版本,下一轮重推同一份
	fake.set("0.7.17", false)
	round()
	round()
	if n := fake.count(); n != 2 {
		t.Fatalf("副机换了版本后应重推一次,实际共 %d 次", n)
	}
	round()
	if n := fake.count(); n != 2 {
		t.Fatalf("重推一次就够了,实际共 %d 次", n)
	}

	// 3. 版本过低被拒、正在退避:升级后不等退避
	fake2 := newVersionNode(t, "0.6.0")
	fake2.set("0.6.0", true)
	node2 := model.Node{Name: "n2", ApiUrl: fake2.srv.URL + "/app/", Token: "t", Enabled: true}
	db.Create(&node2)
	round()
	round()
	if n := fake2.count(); n != 1 {
		t.Fatalf("前提:被拒后退避,5 秒内不再推,实际 %d 次", n)
	}
	fake2.set("0.7.17", false)
	round()
	round()
	if n := fake2.count(); n != 2 {
		t.Fatalf("升级后应立即重推,不等退避,实际共 %d 次", n)
	}
	h.mu.Lock()
	pushed := h.pushed[node2.Id]
	h.mu.Unlock()
	if pushed == "" {
		t.Fatal("重推成功后应记为已推送")
	}
}
