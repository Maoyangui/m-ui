package sub

import (
	"encoding/json"
	"net/url"
	"path/filepath"
	"strings"
	"testing"

	"github.com/Maoyangui/m-ui/database"
	"github.com/Maoyangui/m-ui/database/model"
)

// 流量额度用完 / 到期被停止服务的服务器(model.Node.Suspended)和停用的一样:订阅里不出它的节点 ——
// 它上面的直连节点、转给它的中转入口、经它的中转入口都不出;别的照常。恢复后回来。
func TestHeldNodeLeavesSubscription(t *testing.T) {
	db, err := database.Open(filepath.Join(t.TempDir(), "x.db"))
	if err != nil {
		t.Fatal(err)
	}
	defer database.Close(db)
	gz := model.Node{Name: "广州中转", Domain: "gz.example.com", IsLocal: true, Enabled: true, Sort: 1}
	tokyo := model.Node{Name: "东京", Domain: "jp.example.com", Enabled: true, Sort: 2}
	osaka := model.Node{Name: "大阪", Domain: "osaka.example.com", Enabled: true, Sort: 3}
	for _, n := range []*model.Node{&gz, &tokyo, &osaka} {
		if err := db.Create(n).Error; err != nil {
			t.Fatal(err)
		}
	}
	ids, _ := json.Marshal([]uint{tokyo.Id, osaka.Id})
	fw, _ := json.Marshal([]model.LineForward{{Id: 1, Via: gz.Id, Port: 30001, To: tokyo.Id}, {Id: 2, Via: gz.Id, Port: 30002, To: osaka.Id}})
	line := model.Line{Id: 7, Name: "hy2", Protocol: "hysteria2", Port: 443, Enabled: true, NodeIds: ids, Forwards: fw}
	user := model.User{Name: "u", Credentials: []byte(`{"hysteria2":{"password":"p"}}`)}
	names := func() string {
		t.Helper()
		links := GenerateLinksFor(user, []model.Line{line}, EntriesFromNodes(db, "gz.example.com", "", "", false), nil)
		out := make([]string, 0, len(links))
		for _, l := range links {
			u, err := url.Parse(l)
			if err != nil {
				t.Fatal(err)
			}
			out = append(out, u.Fragment) // 节点名(链接里是转义过的)
		}
		return strings.Join(out, " | ")
	}
	hold := func(id uint, on bool) {
		t.Helper()
		if err := db.Model(&model.Node{}).Where("id = ?", id).Update("suspended", on).Error; err != nil {
			t.Fatal(err)
		}
	}
	all := "hy2-东京 | hy2-大阪 | hy2-东京-广州中转 | hy2-大阪-广州中转"
	if got := names(); got != all {
		t.Fatalf("前提:四个节点都在\n got %s\nwant %s", got, all)
	}
	hold(tokyo.Id, true) // 出口被停止服务:它的直连节点、转给它的中转入口都不出
	if got := names(); got != "hy2-大阪 | hy2-大阪-广州中转" {
		t.Fatalf("东京被停止服务:\n got %s", got)
	}
	hold(tokyo.Id, false)
	hold(gz.Id, true) // 中转那台被停止服务:经它的入口都不出,出口的直连照常
	if got := names(); got != "hy2-东京 | hy2-大阪" {
		t.Fatalf("广州中转被停止服务:\n got %s", got)
	}
	hold(gz.Id, false)
	if got := names(); got != all {
		t.Fatalf("恢复后都回来:\n got %s", got)
	}
	// 老库里这一列是 NULL 的行(升级前建的)按没被停止服务算
	if err := db.Exec("UPDATE nodes SET suspended = NULL").Error; err != nil {
		t.Fatal(err)
	}
	if got := names(); got != all {
		t.Fatalf("suspended 为 NULL 的行应照常出节点:\n got %s", got)
	}
}
