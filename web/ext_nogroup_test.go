package web

import (
	"encoding/json"
	"fmt"
	"net/http"
	"net/http/httptest"
	"reflect"
	"strings"
	"testing"

	"github.com/Maoyangui/m-ui/database/model"
	"gorm.io/gorm"
)

func mkExt(t *testing.T, db *gorm.DB, name, typ string, noGroup bool) uint {
	t.Helper()
	value := "https://example.com/" + name
	if typ == "link" {
		value = "trojan://p@203.0.113.30:443?sni=de.example.net#" + name
	}
	x := model.ExtNode{Name: name, Type: typ, Value: value, Enabled: true, NoGroup: noGroup, Cache: "x"}
	if err := db.Create(&x).Error; err != nil {
		t.Fatal(err)
	}
	return x.Id
}

// D-61:调用方不管「单独成组」时(传 nil),新加的分配按外部订阅自己的默认值,留下的沿用原来的;分享链接不算。
// 以前新加的一律成组 —— 外部订阅定了「分给用户时不成组」,换套餐、外部 API 加分配时却又成了组。
func TestWriteUserExtsNewFollowsExtDefault(t *testing.T) {
	e := newPCEnv(t)
	a, b := mkExt(t, e.db, "机场A", "sub", false), mkExt(t, e.db, "机场B", "sub", true)
	l := mkExt(t, e.db, "链接", "link", true) // 库里带着 true 也不算(接口建的时候就清掉,见 TestExtDefaultNoGroupPanel)
	write := func(ext, ng []uint) {
		t.Helper()
		if err := e.db.Transaction(func(tx *gorm.DB) error { return writeUserExts(tx, 7, ext, ng) }); err != nil {
			t.Fatal(err)
		}
	}
	write([]uint{a, b, l}, nil)
	if got := userExtRows(t, e.db, 7); !reflect.DeepEqual(got, map[uint]bool{a: false, b: true, l: false}) {
		t.Fatalf("新分配应按外部订阅的默认值:%v", got)
	}
	write([]uint{a, b, l}, []uint{a}) // 照给的写:a 关、b 开
	e.db.Model(&model.ExtNode{}).Where("id = ?", a).Update("no_group", false)
	e.db.Model(&model.ExtNode{}).Where("id = ?", b).Update("no_group", true)
	write([]uint{a, b}, nil)
	if got := userExtRows(t, e.db, 7); !reflect.DeepEqual(got, map[uint]bool{a: true, b: false}) {
		t.Fatalf("留下的应沿用原来的开关,不看默认值:%v", got)
	}
}

type extNoGroupView struct {
	ExtIds        []uint `json:"extIds"`
	ExtNoGroupIds []uint `json:"extNoGroupIds"`
}

// D-60:外部 API 读写「单独成组」—— 详情带 extNoGroupIds;和 extIds 一起给是整份替换,单独给只改现有分配的开关,
// 只给 extIds 时留下的沿用、新加的按默认值;列表里有不存在的、没分配的、分享链接都 400 并说明是哪个,库不动;
// 带套餐修改(换套餐那条路)也认;代理令牌和 extIds 一样不认。
func TestPublicAPIExtNoGroupIds(t *testing.T) {
	e := newPCEnv(t)
	a, b := mkExt(t, e.db, "机场A", "sub", false), mkExt(t, e.db, "机场B", "sub", true)
	l := mkExt(t, e.db, "链接", "link", false)
	ok := func(w *httptest.ResponseRecorder, what string) {
		t.Helper()
		if w.Code != 200 {
			t.Fatalf("%s 应 200,得 %d %s", what, w.Code, w.Body.String())
		}
	}
	rows := func(want map[uint]bool, what string) {
		t.Helper()
		if got := userExtRows(t, e.db, e.user("alice").Id); !reflect.DeepEqual(got, want) {
			t.Fatalf("%s:分配应为 %v,实际 %v", what, want, got)
		}
	}
	ok(e.api("POST", "users", fmt.Sprintf(`{"name":"alice","extIds":[%d,%d,%d],"extNoGroupIds":[%d]}`, a, b, l, a)), "新建带两个字段")
	rows(map[uint]bool{a: true, b: false, l: false}, "新建时整份替换(b 的默认值是不成组,但给了列表就照列表)")
	w := e.api("GET", "users/alice", "")
	ok(w, "GET")
	var v extNoGroupView
	if err := json.Unmarshal(w.Body.Bytes(), &v); err != nil || !reflect.DeepEqual(v.ExtNoGroupIds, []uint{a}) || len(v.ExtIds) != 3 {
		t.Fatalf("详情应带 extNoGroupIds [%d]:%s", a, w.Body.String())
	}
	ok(e.api("PATCH", "users/alice", fmt.Sprintf(`{"extNoGroupIds":[%d]}`, b)), "只改开关")
	rows(map[uint]bool{a: false, b: true, l: false}, "只给 extNoGroupIds:分配不动、只改开关")
	c := mkExt(t, e.db, "机场C", "sub", true)
	ok(e.api("PATCH", "users/alice", fmt.Sprintf(`{"extIds":[%d,%d,%d]}`, a, b, c)), "只给 extIds")
	rows(map[uint]bool{a: false, b: true, c: true}, "只给 extIds:留下的沿用、新加的 c 按默认值不成组")
	ok(e.api("PATCH", "users/alice", `{"extNoGroupIds":[]}`), "空列表")
	rows(map[uint]bool{a: false, b: false, c: false}, "给空列表:全部成组")
	for _, tc := range []struct{ body, want string }{
		{`{"extNoGroupIds":[999]}`, "999 不存在"},
		{fmt.Sprintf(`{"extIds":[%d],"extNoGroupIds":[%d]}`, a, b), fmt.Sprintf("%d 没有分给这个用户", b)},
		{fmt.Sprintf(`{"extIds":[%d,%d],"extNoGroupIds":[%d]}`, a, l, l), fmt.Sprintf("%d 是分享链接", l)},
	} {
		w := e.api("PATCH", "users/alice", tc.body)
		if w.Code != 400 || !strings.Contains(w.Body.String(), tc.want) {
			t.Fatalf("%s 应 400 且说明「%s」,得 %d %s", tc.body, tc.want, w.Code, w.Body.String())
		}
	}
	rows(map[uint]bool{a: false, b: false, c: false}, "报错的请求不应改动分配")
	e.mkPlan(model.Plan{Name: "月付", VolumeGB: 10, Days: 30})
	ok(e.api("PATCH", "users/alice", fmt.Sprintf(`{"plan":"月付","extNoGroupIds":[%d,%d]}`, a, c)), "带套餐修改")
	rows(map[uint]bool{a: true, b: false, c: true}, "带套餐修改也认 extNoGroupIds")
	// 代理令牌:和 extIds 一样不认(外部节点只由主面板分配)
	rs := model.Reseller{Name: "dl", Enabled: true, ApiEnabled: true, ApiToken: "tok-dl-0123456789"}
	e.db.Create(&rs)
	e.s.setResellerLines(rs.Id, []uint{1})
	ru := e.mkUser(model.User{Name: "dl-u", ResellerId: rs.Id, SubToken: "dddddddddddddddddddddddd"}, 1)
	e.db.Create(&model.UserExt{UserId: ru.Id, ExtId: a})
	ok(e.rapi("tok-dl-0123456789", "PATCH", "users/dl-u", fmt.Sprintf(`{"extNoGroupIds":[%d]}`, a)), "代理令牌")
	if got := userExtRows(t, e.db, ru.Id); !reflect.DeepEqual(got, map[uint]bool{a: false}) {
		t.Fatalf("代理令牌不该改外部节点的开关:%v", got)
	}
}

// D-61:外部节点编辑框里的「分给用户时单独成组」—— 只改默认值不动已分配的;勾了同步就把不一样的改过来,返回改了几个;
// 改成分享链接时默认值和分配上的「不成组」一并清掉;列表接口给 userCount / noGroupCount;接口新建分享链接时清掉默认值。
func TestExtDefaultNoGroupPanel(t *testing.T) {
	e := newPCEnv(t)
	x := mkExt(t, e.db, "机场A", "sub", false)
	var uids []uint
	for i, ng := range []bool{false, false, true} {
		u := e.mkUser(model.User{Name: fmt.Sprintf("u%d", i), SubToken: fmt.Sprintf("tok%021d", i)})
		e.db.Create(&model.UserExt{UserId: u.Id, ExtId: x, NoGroup: ng})
		uids = append(uids, u.Id)
	}
	flags := func() []bool {
		out := []bool{}
		for _, id := range uids {
			out = append(out, userExtRows(t, e.db, id)[x])
		}
		return out
	}
	put := func(body string) map[string]interface{} {
		t.Helper()
		code, out := extCall(t, e.s, http.MethodPut, fmt.Sprint(x), body)
		if code != 200 {
			t.Fatalf("PUT %s 应 200,得 %d %v", body, code, out)
		}
		return out
	}
	sub := `"name":"机场A","type":"sub","value":"https://example.com/机场A","enabled":true`
	if out := put(`{` + sub + `,"noGroup":true}`); out["applied"] != float64(0) {
		t.Fatalf("没勾同步不该改用户:%v", out)
	}
	var cur model.ExtNode
	e.db.First(&cur, x)
	if !cur.NoGroup || !reflect.DeepEqual(flags(), []bool{false, false, true}) {
		t.Fatalf("只改默认值:默认值 %v、分配 %v", cur.NoGroup, flags())
	}
	if out := put(`{` + sub + `,"noGroup":true,"applyNoGroup":true}`); out["applied"] != float64(2) || !reflect.DeepEqual(flags(), []bool{true, true, true}) {
		t.Fatalf("同步应改掉不一样的 2 个:%v %v", out, flags())
	}
	if out := put(`{` + sub + `,"noGroup":false,"applyNoGroup":true}`); out["applied"] != float64(3) || !reflect.DeepEqual(flags(), []bool{false, false, false}) {
		t.Fatalf("改回成组并同步应改 3 个:%v %v", out, flags())
	}
	// 列表的计数
	e.db.Model(&model.UserExt{}).Where("user_id = ?", uids[0]).Update("no_group", true)
	req := httptest.NewRequest(http.MethodGet, innerBase+"api/exts", nil)
	rec := httptest.NewRecorder()
	e.s.handleExts(rec, req)
	var list []struct {
		Id           uint  `json:"id"`
		NoGroup      bool  `json:"noGroup"`
		UserCount    int64 `json:"userCount"`
		NoGroupCount int64 `json:"noGroupCount"`
	}
	if err := json.Unmarshal(rec.Body.Bytes(), &list); err != nil || len(list) != 1 || list[0].UserCount != 3 || list[0].NoGroupCount != 1 {
		t.Fatalf("列表应给 userCount 3、noGroupCount 1:%s", rec.Body.String())
	}
	// 改成分享链接:默认值、分配上的「不成组」一并清掉
	put(`{"name":"机场A","type":"link","value":"trojan://p@203.0.113.30:443?sni=de.example.net#A","enabled":true,"noGroup":true}`)
	e.db.First(&cur, x)
	if cur.NoGroup || !reflect.DeepEqual(flags(), []bool{false, false, false}) {
		t.Fatalf("改成分享链接后应清掉:默认值 %v、分配 %v", cur.NoGroup, flags())
	}
	// 接口新建分享链接:noGroup 清掉
	req = httptest.NewRequest(http.MethodPost, innerBase+"api/exts", strings.NewReader(`{"name":"链接B","type":"link","value":"trojan://p@203.0.113.31:443?sni=b.example.net#B","enabled":true,"noGroup":true}`))
	rec = httptest.NewRecorder()
	e.s.handleExts(rec, req)
	var nb model.ExtNode
	if rec.Code != 200 || e.db.Where("name = ?", "链接B").First(&nb).Error != nil || nb.NoGroup {
		t.Fatalf("新建分享链接不该带默认值:%d %s %+v", rec.Code, rec.Body.String(), nb)
	}
}
