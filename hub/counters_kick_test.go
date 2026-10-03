package hub

import (
	"testing"

	"github.com/Maoyangui/m-ui/database/model"
)

// 并入了副机用量就通知判定超量(审计 MB10);这一轮没有增量不通知。
func TestCountersMergedKicksDeplete(t *testing.T) {
	db := openDB(t, "kick.db").DB
	db.Create(&model.User{Name: "bob", Enabled: true})
	node := model.Node{Name: "台湾", ApiUrl: "http://tw", Enabled: true}
	db.Create(&node)
	kicks := 0
	h := New(Deps{DB: db, Setting: func(string) string { return "" }, IsNode: func() bool { return false }, CountersMerged: func() { kicks++ }})
	rep := Report{Counters: []model.AgentCounter{{UserName: "bob", Up: 100, Down: 100}}}
	h.applyResult(&nodeResult{n: node, rep: rep})
	if kicks != 1 {
		t.Fatalf("并入增量后应通知一次,实际 %d", kicks)
	}
	h.applyResult(&nodeResult{n: node, rep: rep}) // 账本没涨
	if kicks != 1 {
		t.Fatalf("没有增量不该通知,实际 %d", kicks)
	}
}
