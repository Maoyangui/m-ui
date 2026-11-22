package web

import (
	"context"
	"net/http/httptest"
	"path/filepath"
	"reflect"
	"strings"
	"testing"

	"github.com/Maoyangui/m-ui/database"
	"github.com/Maoyangui/m-ui/database/model"
	"gorm.io/gorm"
)

func userExtRows(t *testing.T, db *gorm.DB, uid uint) map[uint]bool {
	t.Helper()
	var rows []model.UserExt
	db.Where("user_id = ?", uid).Find(&rows)
	out := map[uint]bool{}
	for _, r := range rows {
		out[r.ExtId] = r.NoGroup
	}
	return out
}

// 外部节点分配上的「单独成组」开关:调用方不管这个开关时(套餐变更、公开 API、老前端传 nil),留下的订阅沿用原来的开关,
// 新加的默认成组;给了就照给的。以前一律重置成「成组」:续一次套餐,管理员逐个关掉的开关就悄悄全打开了。
func TestWriteUserExtsKeepsNoGroup(t *testing.T) {
	db, err := database.Open(filepath.Join(t.TempDir(), "x.db"))
	if err != nil {
		t.Fatal(err)
	}
	defer database.Close(db)
	write := func(ext, noGroup []uint) {
		t.Helper()
		if err := db.Transaction(func(tx *gorm.DB) error { return writeUserExts(tx, 7, ext, noGroup) }); err != nil {
			t.Fatal(err)
		}
	}
	write([]uint{1, 2, 3}, []uint{2, 3})
	if got := userExtRows(t, db, 7); !reflect.DeepEqual(got, map[uint]bool{1: false, 2: true, 3: true}) {
		t.Fatalf("照给的开关写:%v", got)
	}
	write([]uint{2, 4, 2}, nil) // 3 不再分配;2 沿用关着;4 新加默认成组;重复的 id 只记一次
	if got := userExtRows(t, db, 7); !reflect.DeepEqual(got, map[uint]bool{2: true, 4: false}) {
		t.Fatalf("不管开关时应沿用原来的:%v", got)
	}
	write([]uint{2, 4}, []uint{}) // 前端给了空列表:都成组
	if got := userExtRows(t, db, 7); !reflect.DeepEqual(got, map[uint]bool{2: false, 4: false}) {
		t.Fatalf("给了空列表应全部成组:%v", got)
	}
	write([]uint{}, nil)
	if got := userExtRows(t, db, 7); len(got) != 0 {
		t.Fatalf("清空分配:%v", got)
	}
}

// 主面板改用户:extNoGroupIds 写进分配,列表接口原样带回;不在 extIds 里的 id 不算。
// 代理改自己名下的用户:主面板给这个用户分配的外部节点(连同开关)原样留着 —— 以前代理改一次就全清空了。
func TestUserExtGroupPanelAndReseller(t *testing.T) {
	db, err := database.Open(filepath.Join(t.TempDir(), "x.db"))
	if err != nil {
		t.Fatal(err)
	}
	defer database.Close(db)
	s := &Server{db: db}
	db.Create(&model.Line{Name: "hk", Protocol: "hysteria2", Port: 30443, Enabled: true})
	for _, n := range []string{"机场A", "机场B"} {
		db.Create(&model.ExtNode{Name: n, Type: "sub", Value: "https://example.com/" + n, Enabled: true})
	}
	rs := model.Reseller{Name: "dl", Enabled: true, UserLimit: 5}
	db.Create(&rs)
	s.setResellerLines(rs.Id, []uint{1})
	u := model.User{Name: "ru", ResellerId: rs.Id, Enabled: true, SubToken: "r1r1r1r1r1r1r1r1r1r1r1r1"}
	db.Create(&u)
	db.Create(&model.UserLine{UserId: u.Id, LineId: 1})

	put := func(rid uint, body string) {
		t.Helper()
		r := httptest.NewRequest("PUT", "http://x/app/api/users/"+itoa(int(u.Id)), strings.NewReader(body))
		if rid > 0 {
			r = r.WithContext(context.WithValue(r.Context(), scopeKey, rid))
		}
		w := httptest.NewRecorder()
		s.handleUserItem(w, r)
		if w.Code != 200 {
			t.Fatalf("改用户失败: %d %s", w.Code, w.Body.String())
		}
	}
	put(0, `{"name":"ru","enabled":true,"lineIds":[1],"extIds":[1,2],"extNoGroupIds":[2,9]}`)
	if got := userExtRows(t, db, u.Id); !reflect.DeepEqual(got, map[uint]bool{1: false, 2: true}) {
		t.Fatalf("主面板写入的分配:%v", got)
	}
	// 列表接口按这两张表带回 extIds、extNoGroupIds(handleUsers 要数据面,这里直接查同一份)
	if got, ng := s.userExtMap()[u.Id], s.userExtNoGroupMap()[u.Id]; !reflect.DeepEqual(got, []uint{1, 2}) || !reflect.DeepEqual(ng, []uint{2}) {
		t.Fatalf("列表应带回 extIds [1 2]、extNoGroupIds [2],实际 %v %v", got, ng)
	}
	put(rs.Id, `{"name":"ru","enabled":true,"remark":"代理改的","lineIds":[1],"extIds":[]}`)
	if got := userExtRows(t, db, u.Id); !reflect.DeepEqual(got, map[uint]bool{1: false, 2: true}) {
		t.Fatalf("代理改用户不该动主面板分配的外部节点:%v", got)
	}
	var cur model.User
	db.First(&cur, u.Id)
	if cur.Remark != "代理改的" {
		t.Fatalf("代理的修改本身应生效: %q", cur.Remark)
	}
}
