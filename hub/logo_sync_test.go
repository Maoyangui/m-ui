package hub

import (
	"bytes"
	"encoding/json"
	"path/filepath"
	"strings"
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
// 旧版主机不发 logos(nil)时副机保留本机的;主机全删了副机跟着清空。
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

	// 旧版主机的快照没有 logos(解码为 nil):副机保留本机的,不当成"全删了"
	old := snap3
	old.Logos = nil
	apply(old)
	if rows := nodeLogos(t, node); len(rows) != 2 || rows[0].Sum != "aaaaaaaaaaaaaaaa" {
		t.Fatalf("旧版主机的快照不该动副机的 logo: %+v", rows)
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

// 下线推送(空用户表)不带 logo:图片字节不落进下线通知记录、不随每轮重试重推;副机收到的是 nil,保留本机的。
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
		t.Fatalf("下线推送不该带 logo(应为 nil,副机据此保留本机的): 第 %d 份,%d 张", n, len(got.Logos))
	}
}
