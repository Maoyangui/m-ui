package web

import (
	"testing"

	"github.com/Maoyangui/m-ui/database/model"
	"github.com/Maoyangui/m-ui/hub"

	"gorm.io/gorm"
	"gorm.io/gorm/clause"
)

// 副机"同一份快照已经应用过、直接确认"的判断(agentSnapshotInDB)在各种升级顺序下库里的样子。
// 以前只比修订号与序号:旧版程序收快照时丢掉了自己不认识的落地页 logo、修订号序号却照记,副机升级后主机重推的
// 同一份永远被直接确认,logo 一直落不了库。现在要"本版程序完整应用过这一份"、logo 也一张不差才直接确认。
func TestAgentSnapshotInDBUpgradeOrders(t *testing.T) {
	db := openAgentTestDB(t, "node.db")
	db.Create(&model.Node{Id: 1, Name: "主机", Enabled: true})
	db.Create(&model.Node{Id: 2, Name: "本机", IsLocal: true, Enabled: true})
	logoA := model.PageLogo{ResellerId: 0, Mime: "image/png", Data: []byte("\x89PNG\r\n\x1a\na"), Sum: "aaaaaaaaaaaaaaaa"}
	logoB := model.PageLogo{ResellerId: 0, Mime: "image/png", Data: []byte("\x89PNG\r\n\x1a\nb"), Sum: "bbbbbbbbbbbbbbbb"}
	snap := func(rev string, seq uint64, logos []model.PageLogo) hub.Snapshot {
		return hub.Snapshot{Revision: rev, Sequence: seq, SelfNodeId: 2, Logos: logos}
	}
	set := func(kv map[string]string) {
		t.Helper()
		for k, v := range kv {
			if err := db.Clauses(clause.OnConflict{Columns: []clause.Column{{Name: "key"}}, DoUpdates: clause.AssignmentColumns([]string{"value"})}).
				Create(&model.Setting{Key: k, Value: v}).Error; err != nil {
				t.Fatal(err)
			}
		}
	}
	setLogos := func(rows ...model.PageLogo) {
		t.Helper()
		if err := db.Session(&gorm.Session{AllowGlobalUpdate: true}).Delete(&model.PageLogo{}).Error; err != nil {
			t.Fatal(err)
		}
		for _, r := range rows {
			r.Id = 0
			if err := db.Create(&r).Error; err != nil {
				t.Fatal(err)
			}
		}
	}

	// 1. 主机先升级、传了 logo,旧版副机应用了 r1/100:修订号序号照记、没有本版的标记、logo 丢了(升级时新建的空表)
	cur := snap("r1", 100, []model.PageLogo{logoA})
	set(map[string]string{"hubRevision": "r1", "hubSnapshotSequence": "100"})
	setLogos()
	if agentSnapshotInDB(db, cur) {
		t.Fatal("旧版程序应用过的(logo 丢了)不能直接确认")
	}
	// logo 碰巧都在也不行:旧版可能还丢了别的新字段,没有本版程序的标记就整份重来
	setLogos(logoA)
	if agentSnapshotInDB(db, cur) {
		t.Fatal("没有本版程序应用过的标记就不能直接确认")
	}
	// 2. 本版程序完整应用过:直接确认(主机的超时重推、10 分钟定期重推都走这条捷径)
	set(map[string]string{agentAppliedKey: agentAppliedStamp(cur)})
	if !agentSnapshotInDB(db, cur) {
		t.Fatal("本版程序完整应用过的同一份应直接确认")
	}
	// 3. 换了程序版本(标记是别的版本写的):整份重来一次
	set(map[string]string{agentAppliedKey: "0.0.1#r1#100"})
	if agentSnapshotInDB(db, cur) {
		t.Fatal("别的版本应用的不能直接确认")
	}
	// 4. 降级期间旧版收了换过 logo 的 r2/200(旧版不碰 logo 表、也不碰标记),升回来以后主机重推 r2/200
	next := snap("r2", 200, []model.PageLogo{logoB})
	set(map[string]string{"hubRevision": "r2", "hubSnapshotSequence": "200", agentAppliedKey: agentAppliedStamp(cur)})
	setLogos(logoA)
	if agentSnapshotInDB(db, next) {
		t.Fatal("降级期间旧版收的快照,升回来后不能直接确认")
	}
	// 5. 标记对、logo 对不上(库被动过):整份重来;对上了才确认
	set(map[string]string{agentAppliedKey: agentAppliedStamp(next)})
	if agentSnapshotInDB(db, next) {
		t.Fatal("logo 和快照对不上不能直接确认")
	}
	setLogos(logoB)
	if !agentSnapshotInDB(db, next) {
		t.Fatal("修订号、序号、标记、logo 都对上了应直接确认")
	}
	// 6. 旧版主机(快照不带 logo):本机留着 logo(本机当主机时传的、别的主机推来的)就整份重来,应用时清掉
	old := snap("r3", 300, nil)
	set(map[string]string{"hubRevision": "r3", "hubSnapshotSequence": "300", agentAppliedKey: agentAppliedStamp(old)})
	if agentSnapshotInDB(db, old) {
		t.Fatal("旧版主机没有 logo、本机却留着,不能直接确认")
	}
	setLogos()
	if !agentSnapshotInDB(db, old) {
		t.Fatal("旧版主机的快照、本机也没有 logo,应直接确认")
	}
	// 7. 还有待踢线、或本机 id 对不上:不确认
	set(map[string]string{agentKickPendingKey: `["alice"]`})
	if agentSnapshotInDB(db, old) {
		t.Fatal("还有待踢线不能直接确认")
	}
	set(map[string]string{agentKickPendingKey: ""})
	other := old
	other.SelfNodeId = 1
	if agentSnapshotInDB(db, other) {
		t.Fatal("本机 id 对不上不能直接确认")
	}
	if !agentSnapshotInDB(db, old) {
		t.Fatal("恢复后应直接确认")
	}
}
