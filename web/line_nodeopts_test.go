package web

import (
	"encoding/json"
	"fmt"
	"path/filepath"
	"strings"
	"testing"

	"github.com/Maoyangui/m-ui/database"
	"github.com/Maoyangui/m-ui/database/model"
	"gorm.io/gorm"
)

// 线路的节点设置(Line.NodeOpts)保存时的规整,以及和中转入口编号、用户「不要的节点」的联动:
//   - 只留这条线路现有节点的键(部署到的服务器的直连、还在的中转入口),什么都没设的键去掉;
//   - 同一次保存里「删掉 2 号入口、再加一个」:新入口不能又叫 2 号,不然会把 2 号留下的设置和用户的选择接过去;
//   - 入口删了、服务器撤了之后,用户在这条线路上「不要」的那个节点的记录跟着清掉,还在的留着。
func TestLineNodeOptsNormalizeAndPrune(t *testing.T) {
	db, err := database.Open(filepath.Join(t.TempDir(), "x.db"))
	if err != nil {
		t.Fatal(err)
	}
	defer database.Close(db)
	s := &Server{db: db}
	master := model.Node{Name: "主机", IsLocal: true, Enabled: true, Sort: 1}
	relay := model.Node{Name: "中转", ApiUrl: "http://r", Enabled: true, Sort: 2}
	exit := model.Node{Name: "出口", ApiUrl: "http://e", Enabled: true, Sort: 3}
	for _, n := range []*model.Node{&master, &relay, &exit} {
		if err := db.Create(n).Error; err != nil {
			t.Fatal(err)
		}
	}
	ids := func(v ...uint) json.RawMessage { b, _ := json.Marshal(v); return b }
	exitKey := model.DirectKey(exit.Id)

	l := model.Line{Name: "L", Protocol: "trojan", Port: 8443, Enabled: true, NodeIds: ids(exit.Id),
		Forwards: fwRaw(model.LineForward{Via: relay.Id, Port: 31000, To: exit.Id}, model.LineForward{Host: "relay.example.net", Port: 443, To: exit.Id}),
		NodeOpts: json.RawMessage(fmt.Sprintf(`{"%s":{"hidden":true},"s999":{"hidden":true},"f1":{},"f2":{"hidden":true},"f9":{"hidden":true}}`, exitKey))}
	if err := s.validateLine(&l); err != nil {
		t.Fatalf("新建应通过: %v", err)
	}
	fs := model.ParseForwards(l.Forwards)
	if len(fs) != 2 || fs[0].Id != 1 || fs[1].Id != 2 {
		t.Fatalf("入口编号应补成 1、2: %+v", fs)
	}
	opts := model.ParseNodeOpts(l.NodeOpts)
	if len(opts) != 2 || !opts[exitKey].Hidden || !opts["f2"].Hidden {
		t.Fatalf("节点设置应只留出口的直连和 2 号入口: %s", l.NodeOpts)
	}
	if err := db.Create(&l).Error; err != nil {
		t.Fatal(err)
	}

	// 删掉 2 号入口、同时加一个新的:新入口拿 3 号;2 号留下的设置清掉,出口直连的设置留着
	upd := l
	upd.Forwards = fwRaw(model.LineForward{Id: 1, Via: relay.Id, Port: 31000, To: exit.Id}, model.LineForward{Host: "other.example.net", Port: 8443, To: exit.Id})
	if err := s.validateLine(&upd); err != nil {
		t.Fatalf("改入口应通过: %v", err)
	}
	fs = model.ParseForwards(upd.Forwards)
	if len(fs) != 2 || fs[0].Id != 1 || fs[1].Id != 3 {
		t.Fatalf("新入口应拿 3 号(不复用刚删掉的 2 号): %+v", fs)
	}
	opts = model.ParseNodeOpts(upd.NodeOpts)
	if _, ok := opts["f2"]; ok || !opts[exitKey].Hidden || len(opts) != 1 {
		t.Fatalf("删掉的入口的设置应清掉、出口直连的留着: %s", upd.NodeOpts)
	}

	// 用户「不要的节点」:2 号入口已经不在 → 清;出口的直连还在 → 留
	for _, k := range []string{"f2", exitKey} {
		if err := db.Create(&model.UserLineHide{UserId: 7, LineId: l.Id, Key: k}).Error; err != nil {
			t.Fatal(err)
		}
	}
	if err := db.Transaction(func(tx *gorm.DB) error { return pruneLineHidesTx(tx, l.Id, s.lineNodeKeys(upd)) }); err != nil {
		t.Fatal(err)
	}
	var rows []model.UserLineHide
	db.Where("line_id = ?", l.Id).Find(&rows)
	if len(rows) != 1 || rows[0].Key != exitKey {
		t.Fatalf("应只剩出口直连那一条: %+v", rows)
	}

	// 线路撤出这台服务器:它的直连节点的设置也清掉
	gone := upd
	gone.NodeIds = ids(relay.Id)
	gone.Forwards = nil
	if err := s.validateLine(&gone); err != nil {
		t.Fatalf("改部署应通过: %v", err)
	}
	if gone.NodeOpts != nil {
		t.Fatalf("撤掉的服务器的设置应清掉: %s", gone.NodeOpts)
	}

	// 格式不对要报错;空对象存空
	bad := l
	bad.NodeOpts = json.RawMessage(`[1]`)
	if err := s.validateLine(&bad); err == nil || !strings.Contains(err.Error(), "节点设置的格式不对") {
		t.Fatalf("格式不对应报错,实际 %v", err)
	}
	empty := l
	empty.NodeOpts = json.RawMessage(`{}`)
	if err := s.validateLine(&empty); err != nil || empty.NodeOpts != nil {
		t.Fatalf("空对象应存空: %v %s", err, empty.NodeOpts)
	}
}
