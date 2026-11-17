package web

import (
	"encoding/json"
	"net/http/httptest"
	"reflect"
	"strings"
	"testing"

	"github.com/Maoyangui/m-ui/database/model"
)

// 用户 / 套餐在一条线路里逐个不要某些节点(LineRef.Hide):整理、落库、回读、不能一个都不留、套餐字段、删除时清理。
func TestLineRefHides(t *testing.T) {
	s, db := newAttentionServer(t) // 带运行器的整套服务:删用户那一步要用到
	var have int64
	db.Model(&model.Node{}).Count(&have)
	if have == 0 { // 本机那一行(id 1)有的环境建库时就有了
		db.Create(&model.Node{Name: "主机", IsLocal: true, Enabled: true, Sort: 1})
	}
	var local model.Node
	if err := db.Order("id asc").First(&local).Error; err != nil || local.Id != 1 {
		t.Fatalf("本机那一行应是 id 1: %+v %v", local, err)
	}
	db.Create(&model.Node{Name: "A", ApiUrl: "http://a", Enabled: true, Sort: 2})
	db.Create(&model.Node{Name: "B", ApiUrl: "http://b", Enabled: true, Sort: 3})
	// 线路 1:全部服务器;两个中转入口(f1 经 A 转到主机,f2 外部中转转到 B),f3 暂停着转到 B
	fw := `[{"id":1,"via":2,"port":40001,"to":1},{"id":2,"host":"relay.example.com","port":40002,"to":3},{"id":3,"host":"old.example.com","port":40003,"to":3,"off":true}]`
	db.Create(&model.Line{Name: "hk", Protocol: "hysteria2", Port: 30443, Enabled: true, Forwards: []byte(fw)})
	db.Create(&model.Line{Name: "ab", Protocol: "anytls", Port: 30444, Enabled: true, NodeIds: []byte(`[2,3]`)}) // 只在 A、B,没有中转
	u := model.User{Name: "u", Enabled: true}
	db.Create(&u)

	// 整理:写法不对的、这条线路上没有的、同一个写两遍的都去掉;排好序
	got := s.normalizeRefs([]model.LineRef{{LineId: 1, Hide: []string{"f2", "s1", "s1", "f9", "s99", "x1", "f0", ""}}, {LineId: 1, Hide: []string{"f1"}}})
	if len(got) != 1 || got[0].NodeIds != nil || !reflect.DeepEqual(got[0].Hide, []string{"f1", "f2", "s1"}) {
		t.Fatalf("整理结果不对: %+v", got)
	}
	// 收窄到主机之后:别的服务器上的直连、出口不在范围里的中转入口,写了也不留(本来就不发)
	got = s.normalizeRefs([]model.LineRef{{LineId: 1, NodeIds: []uint{1}, Hide: []string{"s1", "s2", "f1", "f2"}}})
	if len(got) != 1 || !reflect.DeepEqual(got[0].NodeIds, []uint{1}) || !reflect.DeepEqual(got[0].Hide, []string{"f1", "s1"}) {
		t.Fatalf("范围外的键应去掉: %+v", got)
	}
	// 暂停着的入口也认(暂停期间「不要它」照样留着,恢复了也不会突然发给他)
	if got = s.normalizeRefs([]model.LineRef{{LineId: 1, Hide: []string{"f3"}}}); !reflect.DeepEqual(got[0].Hide, []string{"f3"}) {
		t.Fatalf("暂停着的入口的键应保留: %+v", got)
	}
	// 没有中转的线路:直连的键照认
	if got = s.normalizeRefs([]model.LineRef{{LineId: 2, Hide: []string{"s2", "s1", "f1"}}}); !reflect.DeepEqual(got[0].Hide, []string{"s2"}) {
		t.Fatalf("没部署的服务器 / 没有的入口应去掉: %+v", got)
	}

	// 一个都不留 → 拒(暂停着的不算「留着」);留一个就行
	all := s.normalizeRefs([]model.LineRef{{LineId: 1, Hide: []string{"s1", "s2", "s3", "f1", "f2"}}})
	if err := s.checkRefHides(all); err == nil || !strings.Contains(err.Error(), "hk") {
		t.Fatalf("节点全不要应被拒并点名线路: %v", err)
	}
	if err := s.checkRefHides(s.normalizeRefs([]model.LineRef{{LineId: 1, Hide: []string{"s1", "s2", "s3", "f1"}}})); err != nil {
		t.Fatalf("留着一个中转入口应通过: %v", err)
	}
	if err := s.checkRefHides(s.normalizeRefs([]model.LineRef{{LineId: 1}, {LineId: 2}})); err != nil {
		t.Fatalf("没写不要的应通过: %v", err)
	}
	// 线路上已经关了输出的节点不算「留着」:只剩它一个没勾掉也等于什么都没有
	db.Model(&model.Line{}).Where("id = ?", 2).Update("node_opts", []byte(`{"s3":{"hidden":true}}`))
	if err := s.checkRefHides(s.normalizeRefs([]model.LineRef{{LineId: 2, Hide: []string{"s2"}}})); err == nil {
		t.Fatal("剩下的那个节点线路上关着输出,应被拒")
	}
	db.Model(&model.Line{}).Where("id = ?", 2).Update("node_opts", nil)

	// 落库 + 回读(整体替换)
	if err := s.setUserLineRefs(u.Id, []model.LineRef{{LineId: 1, Hide: []string{"s2", "f2"}}, {LineId: 2, NodeIds: []uint{3}}}); err != nil {
		t.Fatal(err)
	}
	refs := s.userLineRefs(u.Id)
	if len(refs) != 2 || !reflect.DeepEqual(refs[0].Hide, []string{"f2", "s2"}) || refs[1].Hide != nil {
		t.Fatalf("回读不对: %+v", refs)
	}
	if m := s.userLineRefMap(); !reflect.DeepEqual(m[u.Id][0].Hide, []string{"f2", "s2"}) {
		t.Fatalf("整表映射里没带上: %+v", m[u.Id])
	}
	if err := s.setUserLineRefs(u.Id, []model.LineRef{{LineId: 1, Hide: []string{"f1"}}}); err != nil {
		t.Fatal(err)
	}
	var rows []model.UserLineHide
	db.Where("user_id = ?", u.Id).Find(&rows)
	if len(rows) != 1 || rows[0].LineId != 1 || rows[0].Key != "f1" {
		t.Fatalf("应整体替换成一条 f1: %+v", rows)
	}

	// 用户接口:建号带 hide;全不要报错且不建号;改成不带 hide 就清掉;删用户连记录一起删
	post := func(method, url, body string) *httptest.ResponseRecorder {
		w := httptest.NewRecorder()
		s.handleUsers(w, httptest.NewRequest(method, url, strings.NewReader(body)))
		return w
	}
	if w := post("POST", "http://x/app/api/users", `{"name":"v","enabled":true,"lineRefs":[{"lineId":1,"hide":["s1","f2"]}]}`); w.Code != 200 {
		t.Fatalf("建用户应通过: %d %s", w.Code, w.Body.String())
	}
	var v model.User
	db.Where("name = ?", "v").First(&v)
	if refs := s.userLineRefs(v.Id); len(refs) != 1 || !reflect.DeepEqual(refs[0].Hide, []string{"f2", "s1"}) {
		t.Fatalf("接口写入的不对: %+v", refs)
	}
	// 列表接口用的那张整表映射里带着它(前端靠它回填勾选),写成 JSON 是 hide 字段
	if b, _ := json.Marshal(s.userLineRefMap()[v.Id]); string(b) != `[{"lineId":1,"hide":["f2","s1"]}]` {
		t.Fatalf("列表里带出来的不对: %s", b)
	}
	if w := post("POST", "http://x/app/api/users", `{"name":"none","enabled":true,"lineRefs":[{"lineId":2,"hide":["s2","s3"]}]}`); w.Code == 200 {
		t.Fatalf("节点全不要应被拒: %s", w.Body.String())
	}
	var cnt int64
	db.Model(&model.User{}).Where("name = ?", "none").Count(&cnt)
	if cnt != 0 {
		t.Fatal("被拒的请求不该留下用户")
	}

	// 套餐:三个字段读写、校验
	p := model.Plan{Name: "p", LineIds: []byte(`[1,2]`), LineNodes: []byte(`{"2":[3]}`), LineHides: []byte(`{"1":["s2","f1"]}`)}
	if err := validatePlanRefs(&p); err != nil {
		t.Fatalf("套餐应通过: %v", err)
	}
	pr := planRefs(p)
	if len(pr) != 2 || !reflect.DeepEqual(pr[0].Hide, []string{"s2", "f1"}) || pr[1].Hide != nil {
		t.Fatalf("套餐读出来不对: %+v", pr)
	}
	ids, nodes, hides := lineRefsJSON(s.normalizeRefs(pr))
	if string(ids) != "[1,2]" || string(nodes) != `{"2":[3]}` || string(hides) != `{"1":["f1","s2"]}` {
		t.Fatalf("写回套餐字段不对: %s %s %s", ids, nodes, hides)
	}
	for _, bad := range []string{`{"9":["s1"]}`, `{"1":["zzz"]}`, `[1]`} {
		q := model.Plan{LineIds: []byte(`[1,2]`), LineHides: []byte(bad)}
		if err := validatePlanRefs(&q); err == nil {
			t.Fatalf("套餐的节点选择 %s 应被拒", bad)
		}
	}
	empty := model.Plan{LineIds: []byte(`[1]`), LineHides: []byte(`{}`)}
	if err := validatePlanRefs(&empty); err != nil || empty.LineHides != nil {
		t.Fatalf("空的节点选择应收成没有: %v %s", err, empty.LineHides)
	}
	if err := s.validatePlan(&model.Plan{Name: "全不要", LineIds: []byte(`[2]`), LineHides: []byte(`{"2":["s2","s3"]}`)}); err == nil {
		t.Fatal("套餐里一条线路的节点全不要应被拒")
	}

	// 改用户:再提交一份不带 hide 的分配 = 都要,记录清掉;带上新的就整体换成新的
	item := func(method string, id uint, body string) *httptest.ResponseRecorder {
		w := httptest.NewRecorder()
		s.handleUserItem(w, httptest.NewRequest(method, "http://x/app/api/users/"+itoa(int(id)), strings.NewReader(body)))
		return w
	}
	del := func(id uint) *httptest.ResponseRecorder { return item("DELETE", id, ``) }
	if w := item("PUT", v.Id, `{"name":"v","enabled":true,"lineRefs":[{"lineId":1,"hide":["f1"]}]}`); w.Code != 200 {
		t.Fatalf("改用户: %d %s", w.Code, w.Body.String())
	}
	if refs := s.userLineRefs(v.Id); len(refs) != 1 || !reflect.DeepEqual(refs[0].Hide, []string{"f1"}) {
		t.Fatalf("改完应只剩 f1: %+v", refs)
	}
	if w := item("PUT", v.Id, `{"name":"v","enabled":true,"lineRefs":[{"lineId":1,"hide":["s1","s2","s3","f1","f2"]}]}`); w.Code == 200 {
		t.Fatalf("改成全不要应被拒: %s", w.Body.String())
	}
	if refs := s.userLineRefs(v.Id); len(refs) != 1 || !reflect.DeepEqual(refs[0].Hide, []string{"f1"}) {
		t.Fatalf("被拒的修改不该动原来的: %+v", refs)
	}
	if w := item("PUT", v.Id, `{"name":"v","enabled":true,"lineRefs":[{"lineId":1,"hide":["s1","f2"]}]}`); w.Code != 200 {
		t.Fatalf("改用户: %d %s", w.Code, w.Body.String())
	}
	// 删用户:记录跟着删
	db.Model(&model.UserLineHide{}).Where("user_id = ?", v.Id).Count(&cnt)
	if cnt != 2 {
		t.Fatalf("删之前应有 2 条: %d", cnt)
	}
	if w := del(v.Id); w.Code != 200 {
		t.Fatalf("删用户: %d %s", w.Code, w.Body.String())
	}
	db.Model(&model.UserLineHide{}).Where("user_id = ?", v.Id).Count(&cnt)
	if cnt != 0 {
		t.Fatalf("删用户后还留着 %d 条", cnt)
	}
}
