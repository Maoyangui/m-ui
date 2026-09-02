package hub

import (
	"path/filepath"
	"testing"

	"github.com/fangjunsheng555/m-ui/database"
	"github.com/fangjunsheng555/m-ui/database/model"
)

func openDB(t *testing.T, name string) *database.DBHandle {
	t.Helper()
	db, err := database.Open(filepath.Join(t.TempDir(), name))
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { database.Close(db) })
	return &database.DBHandle{DB: db}
}

func TestSnapshotRevisionAndApply(t *testing.T) {
	src := openDB(t, "hub.db").DB
	src.Create(&model.Node{Name: "香港", Domain: "hk.x", IsLocal: true, Token: "secret"})
	src.Create(&model.Node{Name: "台湾", Domain: "tw.x", ApiUrl: "https://tw:2053/ad/", Token: "tok"})
	src.Create(&model.Upstream{Name: "warp", Type: "socks", Options: []byte(`{"server":"127.0.0.1","server_port":40000}`)})
	src.Create(&model.Line{Name: "l1", Protocol: "hysteria2", Port: 1000, UpstreamId: 1, Enabled: true})
	src.Create(&model.User{Name: "alice", Enabled: true, Up: 999, Down: 999, Credentials: []byte(`{"hysteria2":{"password":"p"}}`), DeviceLimit: 2})
	src.Create(&model.UserLine{UserId: 1, LineId: 1})
	setting := func(k string) string { return map[string]string{"subProfileTitle": "maoyang"}[k] }

	s1, err := BuildSnapshot(src, setting)
	if err != nil {
		t.Fatal(err)
	}
	s2, _ := BuildSnapshot(src, setting)
	if s1.Revision != s2.Revision {
		t.Fatal("修订号应稳定")
	}
	for _, n := range s1.Nodes {
		if n.Token != "" {
			t.Fatal("快照不应包含令牌")
		}
	}
	if s1.Users[0].Up != 0 || s1.Users[0].Down != 0 {
		t.Fatal("快照不应携带主机计量")
	}
	src.Model(&model.User{}).Where("name = ?", "alice").Update("enabled", false)
	s3, _ := BuildSnapshot(src, setting)
	if s3.Revision == s1.Revision {
		t.Fatal("禁用用户后修订号应变化")
	}
	// 只改流量不应改修订号
	src.Model(&model.User{}).Where("name = ?", "alice").Update("up", 12345)
	s4, _ := BuildSnapshot(src, setting)
	if s4.Revision != s3.Revision {
		t.Fatal("仅流量变化不应改修订号")
	}

	// 应用到副机库:整表替换、保留账本、标记本机
	dst := openDB(t, "node.db").DB
	dst.Create(&model.Line{Name: "stale", Protocol: "shadowsocks", Port: 5})
	dst.Create(&model.AgentCounter{UserName: "alice", Up: 10, Down: 20})
	s4.SelfNodeId = 2
	changed, upsChanged, err := ApplySnapshot(dst, s4)
	if err != nil || !changed || !upsChanged {
		t.Fatalf("应用失败: %v lines=%v ups=%v", err, changed, upsChanged)
	}
	var lines []model.Line
	dst.Find(&lines)
	if len(lines) != 1 || lines[0].Name != "l1" || lines[0].Id != 1 {
		t.Fatalf("线路未整表替换: %+v", lines)
	}
	var nodes []model.Node
	dst.Order("id").Find(&nodes)
	if len(nodes) != 2 || nodes[0].IsLocal || !nodes[1].IsLocal {
		t.Fatalf("副机应把自己标为本机: %+v", nodes)
	}
	var ac model.AgentCounter
	if dst.First(&ac).Error != nil || ac.Up != 10 {
		t.Fatal("账本应保留")
	}
	var u model.User
	dst.First(&u)
	if u.Enabled || u.DeviceLimit != 2 || len(u.Credentials) == 0 {
		t.Fatalf("用户字段未同步: %+v", u)
	}
	var rev string
	dst.Raw("SELECT value FROM settings WHERE key='hubRevision'").Scan(&rev)
	if rev != s4.Revision {
		t.Fatal("应记录已应用修订号")
	}
	var title string
	dst.Raw("SELECT value FROM settings WHERE key='subProfileTitle'").Scan(&title)
	if title != "maoyang" {
		t.Fatal("同步设置未写入")
	}
	// 再次应用相同快照:线路与上游都未变
	changed, upsChanged, err = ApplySnapshot(dst, s4)
	if err != nil || changed || upsChanged {
		t.Fatalf("相同快照不应判定变化: %v lines=%v ups=%v", err, changed, upsChanged)
	}
	// 只改上游:线路不变、上游变
	src.Model(&model.Upstream{}).Where("name = ?", "warp").Update("options", []byte(`{"server":"127.0.0.1","server_port":40001}`))
	s5, _ := BuildSnapshot(src, setting)
	s5.SelfNodeId = 2
	changed, upsChanged, err = ApplySnapshot(dst, s5)
	if err != nil || changed || !upsChanged {
		t.Fatalf("仅上游变化应只标记上游: %v lines=%v ups=%v", err, changed, upsChanged)
	}
}

func TestApplyCountersCursor(t *testing.T) {
	db := openDB(t, "hub.db").DB
	db.Create(&model.User{Name: "bob", Enabled: true})
	c := []model.AgentCounter{{UserName: "bob", Up: 100, Down: 200}}
	if n, err := ApplyCounters(db, 2, "台湾", c, 1000, 60, 1); err != nil || n != 1 {
		t.Fatalf("首次并入: %v %d", err, n)
	}
	var u model.User
	db.First(&u)
	if u.Up != 100 || u.Down != 200 || u.OnlineAt != 1000 {
		t.Fatalf("首次应全额计入: %+v", u)
	}
	// 增量
	c[0].Up, c[0].Down = 150, 260
	ApplyCounters(db, 2, "台湾", c, 1060, 60, 1)
	db.First(&u)
	if u.Up != 150 || u.Down != 260 {
		t.Fatalf("应只计增量: %+v", u)
	}
	// 无变化 → 不计
	if n, _ := ApplyCounters(db, 2, "台湾", c, 1120, 60, 1); n != 0 {
		t.Fatal("无增量不应计入")
	}
	// 回绕(副机重装):计数器变小 → 游标归零,全额计入
	c[0].Up, c[0].Down = 5, 7
	ApplyCounters(db, 2, "台湾", c, 1180, 60, 1)
	db.First(&u)
	if u.Up != 155 || u.Down != 267 {
		t.Fatalf("回绕后应重认: %+v", u)
	}
	var cur model.TrafficCursor
	db.Where("node_id = 2").First(&cur)
	if cur.Up != 5 || cur.Down != 7 {
		t.Fatalf("游标应跟随: %+v", cur)
	}
	var st []model.Stats
	db.Where("resource = ? AND tag = ?", "node", "台湾").Find(&st)
	if len(st) == 0 {
		t.Fatal("应记录副机流量时序")
	}
	// 不同副机独立游标
	ApplyCounters(db, 3, "日本", []model.AgentCounter{{UserName: "bob", Up: 1, Down: 1}}, 1240, 60, 1)
	db.First(&u)
	if u.Up != 156 || u.Down != 268 {
		t.Fatalf("多副机应各自计入: %+v", u)
	}
	// 倍率:2 倍服务器的 100/200 增量计为 200/400
	ApplyCounters(db, 4, "贵", []model.AgentCounter{{UserName: "bob", Up: 100, Down: 200}}, 1300, 60, 2)
	db.First(&u)
	if u.Up != 356 || u.Down != 668 {
		t.Fatalf("倍率应按 2 倍计入: %+v", u)
	}
	var cur4 model.TrafficCursor
	db.Where("node_id = 4").First(&cur4)
	if cur4.Up != 100 || cur4.Down != 200 {
		t.Fatalf("游标应记录原始计数而非倍率后的值: %+v", cur4)
	}
}
