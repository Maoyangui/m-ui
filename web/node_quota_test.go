package web

import (
	"encoding/json"
	"net/http/httptest"
	"path/filepath"
	"strconv"
	"strings"
	"testing"
	"time"

	"github.com/Maoyangui/m-ui/database"
	"github.com/Maoyangui/m-ui/database/model"

	"gorm.io/gorm"
)

// 服务器的流量额度与到期:接口校验、判定(用完 / 到期 / 重置)、两种处置(整台停用、只停代理服务)和自动恢复。

type quotaFixture struct {
	t     *testing.T
	s     *Server
	db    *gorm.DB
	local model.Node
	a, b  model.Node
}

func newQuotaFixture(t *testing.T) *quotaFixture {
	t.Helper()
	db, err := database.Open(filepath.Join(t.TempDir(), "q.db"))
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { database.Close(db) })
	f := &quotaFixture{t: t, s: &Server{db: db}, db: db}
	f.local = model.Node{Name: "主机", IsLocal: true, Enabled: true, Sort: 1}
	f.a = model.Node{Name: "按量 A", ApiUrl: "http://a.test", Enabled: true, Sort: 2}
	f.b = model.Node{Name: "按量 B", ApiUrl: "http://b.test", Enabled: true, Sort: 3}
	for _, n := range []*model.Node{&f.local, &f.a, &f.b} {
		if err := db.Create(n).Error; err != nil {
			t.Fatal(err)
		}
	}
	return f
}

func (f *quotaFixture) call(method, path, body string) (int, map[string]interface{}) {
	f.t.Helper()
	w := httptest.NewRecorder()
	f.s.handleNodeItem(w, httptest.NewRequest(method, "http://x/app/api/nodes/"+path, strings.NewReader(body)))
	var out map[string]interface{}
	_ = json.Unmarshal(w.Body.Bytes(), &out)
	return w.Code, out
}

func (f *quotaFixture) put(id uint, body string) (int, map[string]interface{}) {
	return f.call("PUT", strconv.Itoa(int(id))+"/quota", body)
}

func (f *quotaFixture) node(id uint) model.Node {
	var n model.Node
	f.db.First(&n, id)
	return n
}

func (f *quotaFixture) quota(id uint) model.NodeQuota {
	var q model.NodeQuota
	f.db.Where("node_id = ?", id).Limit(1).Find(&q)
	return q
}

// use 模拟这台服务器又跑了 n 字节(主机并账后的样子);第一次只建基线。
func (f *quotaFixture) use(id uint, n int64) {
	f.t.Helper()
	q := f.quota(id)
	if q.CurLedger == 0 {
		if _, err := model.MergeNodeTotal(f.db, id, model.NodeTotal{Up: 1, Down: 1, LedgerId: 77}, 1); err != nil {
			f.t.Fatal(err)
		}
		q = f.quota(id)
	}
	if _, err := model.MergeNodeTotal(f.db, id, model.NodeTotal{Up: q.CurUp, Down: q.CurDown + n, LedgerId: q.CurLedger}, time.Now().Unix()); err != nil {
		f.t.Fatal(err)
	}
}

func (f *quotaFixture) audits(action string) int64 {
	var n int64
	f.db.Model(&model.Change{}).Where("key = ? AND action = ?", "node", action).Count(&n)
	return n
}

func TestNodeQuotaValidation(t *testing.T) {
	f := newQuotaFixture(t)
	past := strconv.FormatInt(time.Now().Unix()-10, 10)
	for _, body := range []string{
		`{"volume":-1}`, `{"volume":2305843009213693952}`, `{"expiry":-5}`, `{"expiry":` + past + `}`,
		`{"resetMode":"weekly"}`, `{"resetMode":"days","resetDays":0}`, `{"resetMode":"days","resetDays":3651}`,
		`{"resetMode":"month","resetDay":0}`, `{"resetMode":"month","resetDay":32}`,
		`{"resetMode":"days","resetDays":30,"nextReset":` + past + `}`,
		`{"resetMode":"days","resetDays":30,"nextReset":` + strconv.FormatInt(time.Now().Unix()+31*86400, 10) + `}`,
		`{bad json`,
	} {
		if code, out := f.put(f.a.Id, body); code != 400 {
			t.Errorf("%s 应被拒,得到 %d %v", body, code, out)
		}
	}
	if f.quota(f.a.Id).NodeId != 0 {
		t.Fatal("被拒的请求不该留下记录")
	}
	// 合法的:按月 15 日重置、到期在将来
	future := time.Now().Unix() + 90*86400
	code, out := f.put(f.a.Id, `{"volume":536870912000,"resetMode":"month","resetDay":15,"expiry":`+strconv.FormatInt(future, 10)+`}`)
	if code != 200 {
		t.Fatalf("保存失败: %d %v", code, out)
	}
	q := out["quota"].(map[string]interface{})
	if q["volume"].(float64) != 536870912000 || q["resetMode"] != "month" || q["resetDay"].(float64) != 15 || q["autoDisable"] != true || q["state"] != "" {
		t.Fatalf("返回的额度不对: %v", q)
	}
	next := int64(q["nextReset"].(float64))
	if d := time.Unix(next, 0).In(f.s.panelLocation()); d.Day() != 15 || d.Hour() != 0 || next <= time.Now().Unix() {
		t.Fatalf("下次重置应是下一个 15 日 0 点: %s", d)
	}
	// 只改一项,别的不动;换成按天数时可以自己定从哪天开始重置
	at := time.Now().Unix() + 5*86400
	code, out = f.put(f.a.Id, `{"resetMode":"days","resetDays":30,"nextReset":`+strconv.FormatInt(at, 10)+`}`)
	if code != 200 {
		t.Fatalf("改周期失败: %d %v", code, out)
	}
	got := f.quota(f.a.Id)
	if got.Volume != 536870912000 || got.Expiry != future || got.ResetMode != "days" || got.ResetDays != 30 || got.ResetDay != 0 || got.NextReset != at {
		t.Fatalf("只该动周期: %+v", got)
	}
	// 不重置:下次重置清掉;关掉自动停用
	if code, _ = f.put(f.a.Id, `{"resetMode":"","autoDisable":false}`); code != 200 {
		t.Fatal("改成不重置失败")
	}
	if got = f.quota(f.a.Id); got.NextReset != 0 || got.ResetDays != 0 || !got.KeepOn {
		t.Fatalf("不重置 / 关掉自动停用: %+v", got)
	}
	if f.audits("quota-set") != 3 {
		t.Fatalf("三次保存各记一笔审计,实际 %d", f.audits("quota-set"))
	}
	// 不存在的服务器、不认识的子路径、不支持的方法
	if code, _ = f.put(999, `{"volume":1}`); code != 404 {
		t.Fatalf("不存在的服务器应 404,得到 %d", code)
	}
	if code, _ = f.call("PUT", strconv.Itoa(int(f.a.Id))+"/quota/other", `{}`); code != 404 {
		t.Fatalf("不认识的子路径应 404,得到 %d", code)
	}
	if code, _ = f.call("DELETE", strconv.Itoa(int(f.a.Id))+"/quota", ``); code != 405 {
		t.Fatalf("DELETE 应 405,得到 %d", code)
	}
	if code, _ = f.call("GET", strconv.Itoa(int(f.a.Id))+"/quota/reset", ``); code != 405 {
		t.Fatalf("重置只认 POST,得到 %d", code)
	}
}

// 默认(自动停用):流量用完 → 这台服务器被停用、记下是自动停的;重置用量之后自动启用。手动停用的不会被自动启用。
func TestNodeQuotaAutoDisableAndRestore(t *testing.T) {
	f := newQuotaFixture(t)
	if code, _ := f.put(f.a.Id, `{"volume":1000}`); code != 200 {
		t.Fatal("保存额度失败")
	}
	f.use(f.a.Id, 999)
	f.s.NodeQuotaTick()
	if n := f.node(f.a.Id); !n.Enabled || n.Suspended || f.quota(f.a.Id).State != "" {
		t.Fatalf("差 1 字节不该停: %+v", n)
	}
	f.use(f.a.Id, 1)
	f.s.NodeQuotaTick()
	n, q := f.node(f.a.Id), f.quota(f.a.Id)
	if n.Enabled || n.Suspended || !q.AutoOff || q.State != model.QuotaTraffic || q.StateAt == 0 {
		t.Fatalf("用完应自动停用并记下是自动停的: 服务器 %+v 额度 %+v", n, q)
	}
	if f.audits("quota-off:traffic") != 1 || f.audits("quota-hold:traffic") != 0 {
		t.Fatalf("审计: 停用 %d 停止服务 %d", f.audits("quota-off:traffic"), f.audits("quota-hold:traffic"))
	}
	var actor model.Change
	f.db.Where("action = ?", "quota-off:traffic").First(&actor)
	if actor.Actor != model.ActorQuotaJob {
		t.Fatalf("自动停用的执行者应是 %s,实际 %s", model.ActorQuotaJob, actor.Actor)
	}
	// 别的服务器不受影响;再判几轮不重复记
	f.s.NodeQuotaTick()
	f.s.NodeQuotaTick()
	if !f.node(f.b.Id).Enabled || !f.node(f.local.Id).Enabled || f.audits("quota-off:traffic") != 1 {
		t.Fatal("别的服务器被带上了,或者重复记了审计")
	}
	// 被自动停用着:手动启用被拒,并说清楚该改什么
	body := `{"name":"按量 A","apiUrl":"http://a.test","enabled":true,"ratio":1}`
	code, out := f.call("PUT", strconv.Itoa(int(f.a.Id)), body)
	if code != 400 || !strings.Contains(out["error"].(string), "流量已用完") {
		t.Fatalf("自动停用着手动启用应被拒: %d %v", code, out)
	}
	// 看得到状态
	_, out = f.call("GET", strconv.Itoa(int(f.a.Id))+"/quota", ``)
	if v := out["quota"].(map[string]interface{}); v["state"] != "traffic" || v["autoOff"] != true || v["used"].(float64) != 1000 || v["volume"].(float64) != 1000 || v["autoDisable"] != true || v["tracked"] != true || out["enabled"] != false {
		t.Fatalf("A 的额度: %v", out)
	}
	if _, out = f.call("GET", strconv.Itoa(int(f.b.Id))+"/quota", ``); out["quota"] != nil {
		t.Fatalf("没设也没用量的服务器没有额度记录: %v", out)
	}
	// 重置用量 → 自动启用
	code, out = f.call("POST", strconv.Itoa(int(f.a.Id))+"/quota/reset", ``)
	if code != 200 || out["enabled"] != true {
		t.Fatalf("重置用量后应自动启用: %d %v", code, out)
	}
	n, q = f.node(f.a.Id), f.quota(f.a.Id)
	if !n.Enabled || q.AutoOff || q.State != "" || q.Used() != 0 || q.TotalDown != 1000 {
		t.Fatalf("重置后: 服务器 %+v 额度 %+v", n, q)
	}
	if f.audits("quota-on") != 1 || f.audits("quota-ok") != 0 || f.audits("quota-reset") != 1 {
		t.Fatalf("审计: 启用 %d 恢复 %d(自动启用的只记启用那一笔) 重置 %d", f.audits("quota-on"), f.audits("quota-ok"), f.audits("quota-reset"))
	}
	// 手动停用的:用完、再重置,都不归额度管,不会被自动启用
	if code, _ = f.call("PUT", strconv.Itoa(int(f.a.Id)), `{"name":"按量 A","apiUrl":"http://a.test","enabled":false,"ratio":1}`); code != 200 {
		t.Fatal("手动停用失败")
	}
	f.use(f.a.Id, 5000)
	f.s.NodeQuotaTick()
	if q = f.quota(f.a.Id); q.State != model.QuotaTraffic || q.AutoOff {
		t.Fatalf("手动停用的:状态照记,但不算自动停的: %+v", q)
	}
	f.call("POST", strconv.Itoa(int(f.a.Id))+"/quota/reset", ``)
	if f.node(f.a.Id).Enabled {
		t.Fatal("手动停用的服务器不该被自动启用")
	}
}

// 服务器列表的每一行带着它的额度(界面上的用量条、状态徽标按它画);没有记录的不带。
func TestNodesListCarriesQuota(t *testing.T) {
	s, db := newAttentionServer(t)
	var local model.Node
	if db.Where("is_local = ?", true).Limit(1).Find(&local); local.Id == 0 {
		local = model.Node{Name: "主机", IsLocal: true, Enabled: true}
		db.Create(&local)
	}
	a := model.Node{Name: "按量 A", Enabled: true}
	b := model.Node{Name: "没设的", Enabled: true}
	db.Create(&a)
	db.Create(&b)
	db.Create(&model.NodeQuota{NodeId: a.Id, Volume: 1000, Up: 300, Down: 500, TotalUp: 10, TotalDown: 20, ResetMode: model.QuotaResetMonth, ResetDay: 1, NextReset: 4102444800, Tracked: true})
	db.Create(&model.NodeQuota{NodeId: local.Id, Volume: 1000, Up: 2000, State: model.QuotaTraffic, Tracked: true})
	w := httptest.NewRecorder()
	s.handleNodes(w, httptest.NewRequest("GET", "http://x/app/api/nodes", nil))
	var list struct {
		Nodes []struct {
			Id    uint
			Quota *nodeQuotaView
		}
	}
	if err := json.Unmarshal(w.Body.Bytes(), &list); err != nil {
		t.Fatal(err)
	}
	seen := 0
	for _, row := range list.Nodes {
		switch row.Id {
		case a.Id:
			seen++
			if q := row.Quota; q == nil || q.Volume != 1000 || q.Used != 800 || q.Up != 300 || q.Down != 500 || q.Total != 830 || q.ResetMode != "month" || q.ResetDay != 1 || !q.AutoDisable || q.State != "" || !q.Tracked {
				t.Fatalf("A 的额度: %+v", row.Quota)
			}
		case local.Id:
			seen++
			if q := row.Quota; q == nil || q.AutoDisable || q.State != "traffic" || q.Used != 2000 {
				t.Fatalf("主机自己那台(自动停用恒为否): %+v", row.Quota)
			}
		case b.Id:
			seen++
			if row.Quota != nil {
				t.Fatalf("没有记录的服务器不带额度: %+v", row.Quota)
			}
		}
	}
	if seen != 3 {
		t.Fatalf("三台都该在列表里,只看到 %d 台", seen)
	}
	// 记录里的游标不出接口
	if strings.Contains(w.Body.String(), "curLedger") || strings.Contains(w.Body.String(), "CurLedger") {
		t.Fatal("计量游标不该出现在接口里")
	}
}

// 额度随新增 / 编辑服务器一起保存:和服务器本身同一笔事务(额度不合适,服务器的改动也不存);同一次保存里
// 调高额度再启用是放行的;保存完马上判一次,结果一起回来。
func TestNodeQuotaSavedWithNode(t *testing.T) {
	f := newQuotaFixture(t)
	id := strconv.Itoa(int(f.a.Id))
	// 额度不合适:整份不存(名字也没改)
	code, out := f.call("PUT", id, `{"name":"改名了","apiUrl":"http://a.test","enabled":true,"ratio":1,"quota":{"volume":-5}}`)
	if code != 400 || f.node(f.a.Id).Name != "按量 A" || f.quota(f.a.Id).NodeId != 0 {
		t.Fatalf("额度不合适时服务器的改动也不该存: %d %v", code, out)
	}
	// 一起存
	code, out = f.call("PUT", id, `{"name":"按量 A","apiUrl":"http://a.test","enabled":true,"ratio":2,"quota":{"volume":1000,"resetMode":"month","resetDay":5}}`)
	if code != 200 {
		t.Fatalf("一起保存失败: %d %v", code, out)
	}
	if q, n := f.quota(f.a.Id), f.node(f.a.Id); q.Volume != 1000 || q.ResetMode != "month" || q.ResetDay != 5 || q.NextReset == 0 || n.Ratio != 2 {
		t.Fatalf("服务器和额度都该存下: %+v %+v", n, q)
	}
	if f.audits("quota-set") != 1 || f.audits("update") != 1 {
		t.Fatalf("审计: 额度 %d 服务器 %d", f.audits("quota-set"), f.audits("update"))
	}
	// 不带 quota 的保存不动额度
	f.call("PUT", id, `{"name":"按量 A","apiUrl":"http://a.test","enabled":true,"ratio":1}`)
	if q := f.quota(f.a.Id); q.Volume != 1000 || f.audits("quota-set") != 1 {
		t.Fatalf("没给 quota 不该动额度: %+v", q)
	}
	// 用完 → 自动停用;同一次保存里调高额度 + 启用:放行,保存完就是启用的
	f.use(f.a.Id, 5000)
	f.s.NodeQuotaTick()
	if f.node(f.a.Id).Enabled {
		t.Fatal("前提:用完已自动停用")
	}
	if code, _ = f.call("PUT", id, `{"name":"按量 A","apiUrl":"http://a.test","enabled":true,"ratio":1,"quota":{"volume":800}}`); code != 400 {
		t.Fatalf("额度还是不够时启用应被拒,得到 %d", code)
	}
	code, out = f.call("PUT", id, `{"name":"按量 A","apiUrl":"http://a.test","enabled":true,"ratio":1,"quota":{"volume":9000}}`)
	if code != 200 || out["enabled"] != true || out["suspended"] != false {
		t.Fatalf("调高额度并启用应放行: %d %v", code, out)
	}
	if n, q := f.node(f.a.Id), f.quota(f.a.Id); !n.Enabled || q.State != "" || q.AutoOff {
		t.Fatalf("保存后应是启用、正常: %+v %+v", n, q)
	}
	// 保存时把额度调低到已用以下:马上停,结果回给界面
	code, out = f.call("PUT", id, `{"name":"按量 A","apiUrl":"http://a.test","enabled":true,"ratio":1,"quota":{"volume":100}}`)
	if code != 200 || out["enabled"] != false {
		t.Fatalf("调低到已用以下应马上停并回给界面: %d %v", code, out)
	}

	// 新增服务器时一起给:额度不合适不建;合适的建好就有记录
	w := httptest.NewRecorder()
	f.s.handleNodes(w, httptest.NewRequest("POST", "http://x/app/api/nodes", strings.NewReader(`{"name":"新机","enabled":true,"quota":{"resetMode":"days","resetDays":0}}`)))
	var cnt int64
	f.db.Model(&model.Node{}).Where("name = ?", "新机").Count(&cnt)
	if w.Code != 400 || cnt != 0 {
		t.Fatalf("额度不合适不该建服务器: %d,已建 %d 台", w.Code, cnt)
	}
	w = httptest.NewRecorder()
	f.s.handleNodes(w, httptest.NewRequest("POST", "http://x/app/api/nodes", strings.NewReader(`{"name":"新机","enabled":true,"suspended":true,"quota":{"volume":2000,"resetMode":"days","resetDays":30,"autoDisable":false}}`)))
	if w.Code != 200 {
		t.Fatalf("新增失败: %d %s", w.Code, w.Body.String())
	}
	var created model.Node
	f.db.Where("name = ?", "新机").First(&created)
	if q := f.quota(created.Id); created.Suspended || q.Volume != 2000 || q.ResetDays != 30 || !q.KeepOn || q.NextReset == 0 || q.PeriodStart == 0 {
		t.Fatalf("新增时的额度: 服务器 %+v 额度 %+v(停止服务的标记不收客户端给的)", created, q)
	}
}

// 调高额度、去掉额度同样恢复;调低到已用以下马上停。
func TestNodeQuotaEditTakesEffectAtOnce(t *testing.T) {
	f := newQuotaFixture(t)
	f.put(f.a.Id, `{"volume":1000}`)
	f.use(f.a.Id, 1500)
	f.s.NodeQuotaTick()
	if f.node(f.a.Id).Enabled {
		t.Fatal("前提:用完已停用")
	}
	if code, out := f.put(f.a.Id, `{"volume":2000}`); code != 200 || out["enabled"] != true || out["quota"].(map[string]interface{})["state"] != "" {
		t.Fatalf("调高额度应马上恢复: %v", out)
	}
	if code, out := f.put(f.a.Id, `{"volume":1200}`); code != 200 || out["enabled"] != false || out["quota"].(map[string]interface{})["autoOff"] != true {
		t.Fatalf("调低到已用以下应马上停: %v", out)
	}
	if code, out := f.put(f.a.Id, `{"volume":0}`); code != 200 || out["enabled"] != true {
		t.Fatalf("去掉额度应恢复: %v", out)
	}
	if q := f.quota(f.a.Id); q.Used() != 1500 || q.State != "" {
		t.Fatalf("去掉额度后用量照记: %+v", q)
	}
}

// 关掉「自动停用」:用完后服务器保持启用、照常同步,只打上停止服务的标记;主机自己那台停用不了,也是这样。
func TestNodeQuotaHoldInsteadOfDisable(t *testing.T) {
	f := newQuotaFixture(t)
	f.put(f.a.Id, `{"volume":1000,"autoDisable":false}`)
	f.put(f.local.Id, `{"volume":1000}`) // 主机自己那台:autoDisable 给不给都一样
	f.use(f.a.Id, 2000)
	f.use(f.local.Id, 2000)
	f.s.NodeQuotaTick()
	a, l := f.node(f.a.Id), f.node(f.local.Id)
	if !a.Enabled || !a.Suspended || f.quota(f.a.Id).AutoOff {
		t.Fatalf("关了自动停用:应保持启用、只停服务: %+v", a)
	}
	if !l.Enabled || !l.Suspended || f.quota(f.local.Id).AutoOff {
		t.Fatalf("主机自己那台停用不了,只停服务: %+v", l)
	}
	if f.audits("quota-hold:traffic") != 2 || f.audits("quota-off:traffic") != 0 {
		t.Fatalf("审计: 停止服务 %d 停用 %d", f.audits("quota-hold:traffic"), f.audits("quota-off:traffic"))
	}
	_, out := f.call("GET", strconv.Itoa(int(f.local.Id))+"/quota", ``)
	if q := out["quota"].(map[string]interface{}); q["autoDisable"] != false || out["suspended"] != true {
		t.Fatalf("主机自己那台:自动停用恒为否: %v", out)
	}
	// 恢复:标记去掉,记一笔「恢复」
	f.call("POST", strconv.Itoa(int(f.a.Id))+"/quota/reset", ``)
	if a = f.node(f.a.Id); a.Suspended || !a.Enabled || f.audits("quota-ok") != 1 || f.audits("quota-on") != 0 {
		t.Fatalf("重置后停止服务的标记应去掉: %+v(恢复 %d 启用 %d)", a, f.audits("quota-ok"), f.audits("quota-on"))
	}
	// 用完着的时候把开关打开:改成整台停用,停止服务的标记去掉
	f.use(f.b.Id, 0)
	f.put(f.b.Id, `{"volume":1000,"autoDisable":false}`)
	f.use(f.b.Id, 3000)
	f.s.NodeQuotaTick()
	if b := f.node(f.b.Id); !b.Enabled || !b.Suspended {
		t.Fatalf("前提:B 只停了服务: %+v", b)
	}
	f.put(f.b.Id, `{"autoDisable":true}`)
	if b := f.node(f.b.Id); b.Enabled || b.Suspended || !f.quota(f.b.Id).AutoOff {
		t.Fatalf("打开自动停用:应改成整台停用: %+v", b)
	}
	// 再关掉:自动启用回来,换回只停服务
	f.put(f.b.Id, `{"autoDisable":false}`)
	if b := f.node(f.b.Id); !b.Enabled || !b.Suspended || f.quota(f.b.Id).AutoOff {
		t.Fatalf("关掉自动停用:应启用回来、只停服务: %+v", b)
	}
}

// 到期:到点停用,延期后自动启用;到期优先于流量,重置用量救不回来。周期到点自己重置。
func TestNodeQuotaExpiryAndCycle(t *testing.T) {
	f := newQuotaFixture(t)
	now := time.Now().Unix()
	f.put(f.a.Id, `{"volume":1000,"resetMode":"days","resetDays":30,"expiry":`+strconv.FormatInt(now+86400, 10)+`}`)
	f.use(f.a.Id, 10)
	// 到期时间到了(时间是自然流逝的:直接把库里的值挪到过去)
	f.db.Model(&model.NodeQuota{}).Where("node_id = ?", f.a.Id).Update("expiry", now-5)
	f.s.NodeQuotaTick()
	if n, q := f.node(f.a.Id), f.quota(f.a.Id); n.Enabled || q.State != model.QuotaExpired || !q.AutoOff {
		t.Fatalf("到期应自动停用: %+v %+v", n, q)
	}
	f.call("POST", strconv.Itoa(int(f.a.Id))+"/quota/reset", ``)
	if f.node(f.a.Id).Enabled {
		t.Fatal("到期的:重置用量不该让它恢复")
	}
	code, out := f.call("PUT", strconv.Itoa(int(f.a.Id)), `{"name":"按量 A","apiUrl":"http://a.test","enabled":true,"ratio":1}`)
	if code != 400 || !strings.Contains(out["error"].(string), "已到期") {
		t.Fatalf("到期着手动启用应被拒: %d %v", code, out)
	}
	// 只改别的、不动到期时间:原来就过期的不拦
	if code, _ = f.put(f.a.Id, `{"volume":5000}`); code != 200 {
		t.Fatal("已过期时改别的项不该被拒")
	}
	// 延期 → 自动启用
	if code, out = f.put(f.a.Id, `{"expiry":`+strconv.FormatInt(now+30*86400, 10)+`}`); code != 200 || out["enabled"] != true {
		t.Fatalf("延期后应自动启用: %d %v", code, out)
	}
	// 去掉到期时间也行
	f.db.Model(&model.NodeQuota{}).Where("node_id = ?", f.a.Id).Update("expiry", now-5)
	f.s.NodeQuotaTick()
	if code, out = f.put(f.a.Id, `{"expiry":0}`); code != 200 || out["enabled"] != true {
		t.Fatalf("去掉到期时间应恢复: %d %v", code, out)
	}

	// 周期到点:用完停着的,到了重置时间自己清零、自己启用
	f.use(f.a.Id, 9000)
	f.s.NodeQuotaTick()
	if f.node(f.a.Id).Enabled {
		t.Fatal("前提:用完已停用")
	}
	f.db.Model(&model.NodeQuota{}).Where("node_id = ?", f.a.Id).Update("next_reset", now-1)
	f.s.NodeQuotaTick()
	n, q := f.node(f.a.Id), f.quota(f.a.Id)
	if !n.Enabled || q.Used() != 0 || q.State != "" || q.AutoOff || q.NextReset < now+29*86400 || q.NextReset > now+31*86400 {
		t.Fatalf("到了重置时间应清零并自动启用,排好下一次: %+v %+v", n, q)
	}
	var job model.Change
	f.db.Where("key = ? AND action = ? AND actor = ?", "node", "quota-reset", model.ActorQuotaJob).First(&job)
	if job.Id == 0 {
		t.Fatal("周期重置应以定时任务的名义记一笔审计")
	}
	// 设了周期却还没排时间的(老记录):这一轮补上,不当场重置
	f.use(f.a.Id, 100)
	f.db.Model(&model.NodeQuota{}).Where("node_id = ?", f.a.Id).Update("next_reset", 0)
	f.s.NodeQuotaTick()
	if q = f.quota(f.a.Id); q.NextReset <= now || q.Used() != 100 {
		t.Fatalf("没排时间的补上、不清用量: %+v", q)
	}
}

// 删服务器:额度记录一起删;删了之后还在路上的并账不会把记录写回去,万一留下了下一轮清掉。副机上不判、不让改。
func TestNodeQuotaCleanupAndNodeRole(t *testing.T) {
	f := newQuotaFixture(t)
	f.put(f.a.Id, `{"volume":1000}`)
	f.use(f.a.Id, 10)
	if code, _ := f.call("DELETE", strconv.Itoa(int(f.a.Id)), ``); code != 200 {
		t.Fatal("删服务器失败")
	}
	if f.quota(f.a.Id).NodeId != 0 {
		t.Fatal("服务器删了,额度记录应一起删")
	}
	if _, err := model.MergeNodeTotal(f.db, f.a.Id, model.NodeTotal{Up: 5, Down: 5, LedgerId: 9}, 1); err != nil || f.quota(f.a.Id).NodeId != 0 {
		t.Fatal("已删的服务器不该再有额度记录")
	}
	f.db.Create(&model.NodeQuota{NodeId: 4242, Volume: 1}) // 没主的记录
	f.put(f.b.Id, `{"volume":1000}`)
	f.s.NodeQuotaTick()
	if f.quota(4242).NodeId != 0 {
		t.Fatal("没主的记录应清掉")
	}
	// 本机是副机:不判,不让改
	f.use(f.b.Id, 5000)
	f.db.Create(&model.Setting{Key: "nodeMode", Value: "true"})
	f.s.NodeQuotaTick()
	if !f.node(f.b.Id).Enabled || f.quota(f.b.Id).State != "" {
		t.Fatal("副机上不判额度")
	}
	if code, _ := f.put(f.b.Id, `{"volume":1}`); code != 403 {
		t.Fatalf("副机上不让改,得到 %d", code)
	}
	if code, _ := f.call("POST", strconv.Itoa(int(f.b.Id))+"/quota/reset", ``); code != 403 {
		t.Fatalf("副机上不让重置,得到 %d", code)
	}
	if code, _ := f.call("GET", strconv.Itoa(int(f.b.Id))+"/quota", ``); code != 200 {
		t.Fatalf("副机上可以看,得到 %d", code)
	}
}

// 外部 API 的 GET /servers 带着每台的额度(有记录的才带)和「只停了代理服务」的标记;代理的密钥照旧看不了服务器。
func TestAPIServersCarriesQuota(t *testing.T) {
	s, db := newAttentionServer(t)
	var local model.Node
	if db.Where("is_local = ?", true).Limit(1).Find(&local); local.Id == 0 {
		local = model.Node{Name: "主机", IsLocal: true, Enabled: true}
		db.Create(&local)
	}
	a := model.Node{Name: "按量 A", Enabled: true}
	b := model.Node{Name: "没设的", Enabled: true}
	db.Create(&a)
	db.Create(&b)
	db.Model(&model.Node{}).Where("id = ?", a.Id).Update("suspended", true)
	db.Create(&model.NodeQuota{NodeId: a.Id, Volume: 1000, Up: 400, Down: 700, KeepOn: true, State: model.QuotaTraffic, StateAt: 1700000000, Tracked: true})
	w := httptest.NewRecorder()
	s.apiServers(w, apiScope{})
	if w.Code != 200 {
		t.Fatalf("GET /servers: %d %s", w.Code, w.Body.String())
	}
	var list []struct {
		Id        uint
		Name      string
		Enabled   bool
		Suspended bool
		Quota     *nodeQuotaView
	}
	if err := json.Unmarshal(w.Body.Bytes(), &list); err != nil {
		t.Fatal(err)
	}
	seen := 0
	for _, row := range list {
		switch row.Id {
		case a.Id:
			seen++
			if q := row.Quota; !row.Suspended || !row.Enabled || q == nil || q.Volume != 1000 || q.Used != 1100 || q.State != "traffic" || q.AutoDisable || q.AutoOff || q.StateAt != 1700000000 {
				t.Fatalf("A(只停了代理服务): suspended=%v enabled=%v quota=%+v", row.Suspended, row.Enabled, row.Quota)
			}
		case b.Id, local.Id:
			seen++
			if row.Suspended || row.Quota != nil {
				t.Fatalf("%s 没有额度记录,不该带 quota / suspended: %+v", row.Name, row)
			}
		}
	}
	if seen != 3 {
		t.Fatalf("三台都该在列表里,只看到 %d 台: %s", seen, w.Body.String())
	}
	if strings.Contains(w.Body.String(), "curLedger") || strings.Contains(w.Body.String(), "CurLedger") {
		t.Fatal("计量游标不该出现在外部 API 里")
	}
	w = httptest.NewRecorder()
	s.apiServers(w, apiScope{rid: 7})
	if w.Code != 403 {
		t.Fatalf("代理的密钥看服务器应当 403,实际 %d", w.Code)
	}
}
