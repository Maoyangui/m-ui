package web

import (
	"encoding/json"
	"net/http/httptest"
	"path/filepath"
	"reflect"
	"sort"
	"strings"
	"testing"

	"github.com/Maoyangui/m-ui/database"
	"github.com/Maoyangui/m-ui/database/model"
	"github.com/Maoyangui/m-ui/runner"

	"gorm.io/gorm"
)

// 收窄线路的部署服务器时,分配 / 授权 / 套餐里指向已撤服务器的收窄行一并整理(审计 Q-32 #4):
//   - 只剩已撤服务器的:改回「这条线路的全部服务器」,并在接口返回里点名(不能悄悄没了这条线路);
//   - 还有别的服务器的:只去掉已撤的那几台,不提示。
func seedScopeFix(t *testing.T, db *gorm.DB) {
	t.Helper()
	var local int64
	if db.Model(&model.Node{}).Where("is_local = ?", true).Count(&local); local == 0 { // runner.New 可能已经建了本机那一行(id 1)
		db.Create(&model.Node{Name: "主机", IsLocal: true, Enabled: true, Sort: 1})
	}
	db.Create(&model.Node{Name: "副机B", ApiUrl: "http://b", Enabled: true, Sort: 2})
	db.Create(&model.Node{Name: "副机C", ApiUrl: "http://c", Enabled: true, Sort: 3})
	db.Create(&model.Line{Name: "a", Protocol: "shadowsocks", Port: 31071, Enabled: true, Options: []byte(rrSS), NodeIds: []byte(`[1,2,3]`)})
	db.Create(&model.Line{Name: "other", Protocol: "shadowsocks", Port: 31072, Enabled: true, Options: []byte(rrSS), NodeIds: []byte(`[2]`)})
	db.Create(&model.Reseller{Name: "代理甲"})
	db.Create(&model.Reseller{Name: "代理乙"})
	for _, n := range []string{"只在B", "B和主机", "全部", "别的线路"} {
		db.Create(&model.User{Name: n, Enabled: true})
	}
	for uid := uint(1); uid <= 3; uid++ {
		db.Create(&model.UserLine{UserId: uid, LineId: 1})
	}
	db.Create(&model.UserLine{UserId: 4, LineId: 2})
	db.Create(&model.UserLineNode{UserId: 1, LineId: 1, NodeId: 2})
	db.Create(&model.UserLineNode{UserId: 2, LineId: 1, NodeId: 1})
	db.Create(&model.UserLineNode{UserId: 2, LineId: 1, NodeId: 2})
	db.Create(&model.UserLineNode{UserId: 4, LineId: 2, NodeId: 2}) // 别的线路上的 B 不动
	db.Create(&model.ResellerLine{ResellerId: 1, LineId: 1})
	db.Create(&model.ResellerLine{ResellerId: 2, LineId: 1})
	db.Create(&model.ResellerLineNode{ResellerId: 1, LineId: 1, NodeId: 2})
	db.Create(&model.ResellerLineNode{ResellerId: 2, LineId: 1, NodeId: 1})
	db.Create(&model.ResellerLineNode{ResellerId: 2, LineId: 1, NodeId: 3})
	db.Create(&model.Plan{Name: "套餐只在B", LineIds: []byte(`[1,2]`), LineNodes: []byte(`{"1":[2],"2":[2]}`)})
	db.Create(&model.Plan{Name: "套餐B和C", LineIds: []byte(`[1]`), LineNodes: []byte(`{"1":[2,3]}`)})
}

func scopeRows(db *gorm.DB) (users, resellers map[uint][]uint, plans map[string]string) {
	users, resellers, plans = map[uint][]uint{}, map[uint][]uint{}, map[string]string{}
	var ul []model.UserLineNode
	db.Where("line_id = 1").Order("user_id, node_id").Find(&ul)
	for _, x := range ul {
		users[x.UserId] = append(users[x.UserId], x.NodeId)
	}
	var rl []model.ResellerLineNode
	db.Where("line_id = 1").Order("reseller_id, node_id").Find(&rl)
	for _, x := range rl {
		resellers[x.ResellerId] = append(resellers[x.ResellerId], x.NodeId)
	}
	var ps []model.Plan
	db.Find(&ps)
	for _, p := range ps {
		plans[p.Name] = string(p.LineNodes)
	}
	return
}

func checkScopeFix(t *testing.T, db *gorm.DB, body string) {
	t.Helper()
	var resp struct {
		ScopeWidened struct {
			Users, Resellers, Plans []string
		} `json:"scopeWidened"`
	}
	if err := json.Unmarshal([]byte(body), &resp); err != nil {
		t.Fatalf("返回不是 JSON: %s", body)
	}
	w := resp.ScopeWidened
	sort.Strings(w.Plans)
	if !reflect.DeepEqual(w.Users, []string{"只在B"}) || !reflect.DeepEqual(w.Resellers, []string{"代理甲"}) || !reflect.DeepEqual(w.Plans, []string{"套餐只在B"}) {
		t.Fatalf("只剩已撤服务器的分配 / 授权 / 套餐要在返回里点名: %s", body)
	}
	users, resellers, plans := scopeRows(db)
	if !reflect.DeepEqual(users, map[uint][]uint{2: {1}}) {
		t.Fatalf("用户:只在 B 的改回全部(没有行),B 和主机的只剩主机: %v", users)
	}
	if !reflect.DeepEqual(resellers, map[uint][]uint{2: {1, 3}}) {
		t.Fatalf("代理:只在 B 的改回全部,主机和 C 的不动: %v", resellers)
	}
	if plans["套餐只在B"] != `{"2":[2]}` || plans["套餐B和C"] != `{"1":[3]}` {
		t.Fatalf("套餐:这条线路只在 B 的去掉范围(=全部),别的线路不动;B 和 C 的只剩 C: %v", plans)
	}
	var ul4 int64
	db.Model(&model.UserLineNode{}).Where("line_id = 2 AND node_id = 2").Count(&ul4)
	var links int64
	db.Model(&model.UserLine{}).Where("line_id = 1").Count(&links)
	if ul4 != 1 || links != 3 {
		t.Fatalf("别的线路的收窄行、这条线路的分配都不能动: %d %d", ul4, links)
	}
}

func TestLineBatchNodesNarrowFixesScopes(t *testing.T) {
	db, err := database.Open(filepath.Join(t.TempDir(), "x.db"))
	if err != nil {
		t.Fatal(err)
	}
	defer database.Close(db)
	s := &Server{db: db}
	seedScopeFix(t, db)
	req := httptest.NewRequest("POST", "http://x/app/api/lines/batch", strings.NewReader(`{"ids":[1],"action":"nodes","nodeIds":[1,3]}`))
	w := httptest.NewRecorder()
	s.handleLineItem(w, req)
	if w.Code != 200 {
		t.Fatalf("批量改部署应通过: %d %s", w.Code, w.Body.String())
	}
	checkScopeFix(t, db, w.Body.String())

	// 改成全部服务器:收窄行都还有效,什么都不动、不提示
	req = httptest.NewRequest("POST", "http://x/app/api/lines/batch", strings.NewReader(`{"ids":[1],"action":"nodes","nodeIds":[]}`))
	w = httptest.NewRecorder()
	s.handleLineItem(w, req)
	if w.Code != 200 || strings.Contains(w.Body.String(), "scopeWidened") {
		t.Fatalf("改成全部服务器不该有整理提示: %d %s", w.Code, w.Body.String())
	}
}

func TestLinePutNarrowFixesScopes(t *testing.T) {
	run, err := runner.New(filepath.Join(t.TempDir(), "m.db"))
	if err != nil {
		t.Fatal(err)
	}
	defer database.Close(run.DB())
	defer run.Stop() // 数据面真的起在 31071 上,用完关掉,不占着端口影响后面的用例
	db := run.DB()
	s := NewServer(run)
	seedScopeFix(t, db)
	r := httptest.NewRequest("PUT", "http://x/app/api/lines/1", strings.NewReader(`{"name":"a","protocol":"shadowsocks","port":31071,"enabled":true,"options":`+rrSS+`,"nodeIds":[1,3]}`))
	w := httptest.NewRecorder()
	s.handleLineItem(w, r)
	if w.Code != 200 {
		t.Fatalf("改线路失败: %d %s", w.Code, w.Body.String())
	}
	if !strings.Contains(w.Body.String(), `"name":"a"`) {
		t.Fatalf("返回里线路本身的字段照旧: %s", w.Body.String())
	}
	checkScopeFix(t, db, w.Body.String())
}
