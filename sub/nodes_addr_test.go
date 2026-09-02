package sub

import (
	"path/filepath"
	"strings"
	"testing"

	"github.com/Maoyangui/m-ui/database"
	"github.com/Maoyangui/m-ui/database/model"
	"github.com/Maoyangui/m-ui/render"
)

// 节点地址策略:默认给 IP(SNI 仍是域名),倍率进后缀,线路按 NodeIds 只在部署的服务器上出节点。
func TestEntriesPreferIPRatioAndPerLineServers(t *testing.T) {
	db, _ := database.Open(filepath.Join(t.TempDir(), "x.db"))
	defer database.Close(db)
	db.Create(&model.Node{Name: "日本", Domain: "jp.example.com", IsLocal: true, Enabled: true, Sort: 1, Ratio: 1})
	db.Create(&model.Node{Name: "香港", Domain: "hk.example.com", PublicIP: "5.6.7.8", Enabled: true, Sort: 2, Ratio: 2})
	db.Create(&model.Node{Name: "手填", Domain: "tw.example.com", Addr: "9.9.9.9", PublicIP: "1.1.1.1", Enabled: true, Sort: 3, Ratio: 1})

	e := EntriesFromNodes(db, "jp.example.com", "1.2.3.4", true)
	if len(e) != 3 {
		t.Fatalf("应有 3 个入口: %+v", e)
	}
	if e[0].Host != "1.2.3.4" || e[0].SNI != "jp.example.com" || e[0].Suffix != "-日本" {
		t.Fatalf("本机应用探测到的公网 IP、SNI 为域名: %+v", e[0])
	}
	if e[1].Host != "5.6.7.8" || e[1].SNI != "hk.example.com" || e[1].Suffix != "-香港 x2" {
		t.Fatalf("副机应用上报的公网 IP 并带倍率后缀: %+v", e[1])
	}
	if e[2].Host != "9.9.9.9" || e[2].Suffix != "-手填" {
		t.Fatalf("手填地址优先: %+v", e[2])
	}
	// 域名策略
	d := EntriesFromNodes(db, "jp.example.com", "1.2.3.4", false)
	if d[0].Host != "jp.example.com" || d[1].Host != "hk.example.com" {
		t.Fatalf("域名策略应用域名: %+v", d)
	}

	// 线路只部署在香港(id=2):订阅只出香港入口;渲染时本机(日本 id=1)不包含它
	user := model.User{Name: "u", Credentials: []byte(`{"hysteria2":{"password":"p"}}`)}
	only := model.Line{Name: "仅港", Protocol: "hysteria2", Port: 1, Enabled: true, NodeIds: []byte(`[2]`)}
	all := model.Line{Name: "全部", Protocol: "hysteria2", Port: 2, Enabled: true}
	links := GenerateLinks(user, []model.Line{only, all}, e)
	if len(links) != 4 {
		t.Fatalf("仅港 1 条 + 全部 3 条 = 4,实际 %d: %v", len(links), links)
	}
	if !strings.Contains(links[0], "5.6.7.8:1") || strings.Contains(strings.Join(links, "\n"), "1.2.3.4:1?") {
		t.Fatalf("仅港线路不应出日本入口: %v", links)
	}
	if !render.LineOnNode(only, 2) || render.LineOnNode(only, 1) || !render.LineOnNode(all, 1) || !render.LineOnNode(only, 0) {
		t.Fatal("LineOnNode 判定错误")
	}
	out, _ := BuildClash(user, []model.Line{only, all}, e, "", "")
	if strings.Contains(out, "仅港-日本") || !strings.Contains(out, "仅港-香港 x2") || !strings.Contains(out, "全部-日本") {
		t.Fatalf("clash 应按服务器过滤并带倍率后缀:\n%s", out)
	}
}
