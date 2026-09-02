package sub

import (
	"path/filepath"
	"strings"
	"testing"

	"github.com/fangjunsheng555/m-ui/database"
	"github.com/fangjunsheng555/m-ui/database/model"

	"gopkg.in/yaml.v3"
)

func TestEntriesFromNodesAndClashLineGroups(t *testing.T) {
	db, err := database.Open(filepath.Join(t.TempDir(), "x.db"))
	if err != nil {
		t.Fatal(err)
	}
	defer database.Close(db)

	// 未配置节点 → 面板域名单入口
	e := EntriesFromNodes(db, "hk.example.com", "", false)
	if len(e) != 1 || e[0].Host != "hk.example.com" || e[0].Suffix != "" {
		t.Fatalf("无节点应回落面板域名: %+v", e)
	}
	db.Create(&model.Node{Name: "香港", IsLocal: true, Enabled: true, Sort: 1})
	e = EntriesFromNodes(db, "hk.example.com", "", false)
	if len(e) != 1 || e[0].Host != "hk.example.com" || e[0].Suffix != "" {
		t.Fatalf("单节点不加后缀: %+v", e)
	}
	db.Create(&model.Node{Name: "台湾", Domain: "tw.example.com", Enabled: true, Sort: 2})
	db.Create(&model.Node{Name: "停用", Domain: "x.example.com", Sort: 3})
	db.Model(&model.Node{}).Where("name = ?", "停用").Update("enabled", false) // default:true 会吞掉 Create 的 false
	e = EntriesFromNodes(db, "hk.example.com", "", false)
	if len(e) != 2 || e[0].Suffix != "-香港" || e[1].Host != "tw.example.com" || e[1].Suffix != "-台湾" {
		t.Fatalf("双节点应各带后缀且跳过停用: %+v", e)
	}

	user := model.User{Name: "u", Credentials: []byte(`{"hysteria2":{"password":"p"},"shadowsocks":{"password":"cGFzcw=="}}`)}
	lines := []model.Line{
		{Name: "香港极速1", Protocol: "hysteria2", Port: 1001, Enabled: true},
		{Name: "SS", Protocol: "shadowsocks", Port: 1002, Enabled: true, Options: []byte(`{"method":"aes-128-gcm"}`)},
	}
	out, err := BuildClash(user, lines, e, "", "")
	if err != nil {
		t.Fatal(err)
	}
	var cfg struct {
		Proxies []map[string]interface{} `yaml:"proxies"`
		Groups  []struct {
			Name    string   `yaml:"name"`
			Type    string   `yaml:"type"`
			Proxies []string `yaml:"proxies"`
		} `yaml:"proxy-groups"`
	}
	if err := yaml.Unmarshal([]byte(out), &cfg); err != nil {
		t.Fatal(err)
	}
	if len(cfg.Proxies) != 4 {
		t.Fatalf("2 线路 × 2 入口应有 4 个代理,实际 %d", len(cfg.Proxies))
	}
	names := map[string]bool{}
	for _, p := range cfg.Proxies {
		names[p["name"].(string)] = true
		if p["name"] == "香港极速1-台湾" && p["server"] != "tw.example.com" {
			t.Fatalf("台湾入口地址错误: %v", p)
		}
	}
	if !names["香港极速1-香港"] || !names["香港极速1-台湾"] {
		t.Fatalf("代理名应带入口后缀: %v", names)
	}
	var proxySel, auto, lineGroup *struct {
		Name    string   `yaml:"name"`
		Type    string   `yaml:"type"`
		Proxies []string `yaml:"proxies"`
	}
	for i := range cfg.Groups {
		switch cfg.Groups[i].Name {
		case "Proxy":
			proxySel = &cfg.Groups[i]
		case "Auto":
			auto = &cfg.Groups[i]
		case "香港极速1":
			lineGroup = &cfg.Groups[i]
		}
	}
	if lineGroup == nil || lineGroup.Type != "url-test" || len(lineGroup.Proxies) != 2 {
		t.Fatalf("每条线路应有 url-test 组: %+v", lineGroup)
	}
	if proxySel == nil || strings.Join(proxySel.Proxies, ",") != "Auto,香港极速1,SS" {
		t.Fatalf("Proxy 选择组应列线路组: %+v", proxySel)
	}
	if auto == nil || len(auto.Proxies) != 4 {
		t.Fatalf("Auto 应覆盖全部代理: %+v", auto)
	}

	// 分享链接:每线路每入口一条,带后缀
	links := GenerateLinks(user, lines, e)
	if len(links) != 4 || !strings.Contains(links[0], "#"+"%E9%A6%99%E6%B8%AF%E6%9E%81%E9%80%9F1-%E9%A6%99%E6%B8%AF") {
		t.Fatalf("链接应按入口展开并带后缀: %v", links)
	}
}
