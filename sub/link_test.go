package sub

import (
	"encoding/json"
	"os"
	"sort"
	"strings"
	"testing"

	"github.com/Maoyangui/m-ui/database"
	"github.com/Maoyangui/m-ui/database/model"
)

func mkUser() model.User {
	creds := map[string]map[string]interface{}{
		"hysteria2":   {"name": "one", "password": "qJIq2Qzhug"},
		"anytls":      {"name": "one", "password": "qJIq2Qzhug"},
		"shadowsocks": {"name": "one", "password": "Ohr5NYndHYDgYXUafEO9PxoNVB2OwlFIme92uV1hY3o="},
	}
	b, _ := json.Marshal(creds)
	return model.User{Name: "one", Enabled: true, Credentials: b}
}

var hkEntry = []Entry{{Name: "香港", Host: "hk.joinvip.vip", SNI: "hk.joinvip.vip"}}

// 合成用例:三种协议链接与 旧面板 实测 golden 字节级一致。
func TestLinkFormatsMatchSUI(t *testing.T) {
	user := mkUser()
	cases := []struct {
		line model.Line
		want string
	}{
		{
			model.Line{Name: "日本2(流媒体解锁)", Protocol: "anytls", Port: 42119},
			"anytls://qJIq2Qzhug@hk.joinvip.vip:42119?security=tls&sni=hk.joinvip.vip#%E6%97%A5%E6%9C%AC2(%E6%B5%81%E5%AA%92%E4%BD%93%E8%A7%A3%E9%94%81)",
		},
		{
			model.Line{Name: "充值续费上方网址，用不了刷新订阅", Protocol: "hysteria2", Port: 44369},
			"hysteria2://qJIq2Qzhug@hk.joinvip.vip:44369?security=tls&sni=hk.joinvip.vip&fastopen=0#%E5%85%85%E5%80%BC%E7%BB%AD%E8%B4%B9%E4%B8%8A%E6%96%B9%E7%BD%91%E5%9D%80%EF%BC%8C%E7%94%A8%E4%B8%8D%E4%BA%86%E5%88%B7%E6%96%B0%E8%AE%A2%E9%98%85",
		},
	}
	for _, c := range cases {
		got := GenerateLinks(user, []model.Line{c.line}, hkEntry)
		if len(got) != 1 || got[0] != c.want {
			t.Errorf("线路 %q\n want: %s\n got:  %v", c.line.Name, c.want, got)
		}
	}
}

// ss 用自定义地址(网址节点指向独立服务器),备注原样、地址取 addrs。
func TestShadowsocksLinkWithAddr(t *testing.T) {
	user := mkUser()
	opts, _ := json.Marshal(map[string]interface{}{"method": "aes-256-gcm"})
	addrs, _ := json.Marshal([]map[string]interface{}{{"server": "20.189.113.56", "server_port": 29937}})
	line := model.Line{Name: "网址:www.liumeiti.vip/www.joinvip.vip", Protocol: "shadowsocks", Port: 29937, Options: opts, Addrs: addrs}
	want := "ss://YWVzLTI1Ni1nY206T2hyNU5ZbmRIWURnWVhVYWZFTzlQeG9OVkIyT3dsRkltZTkydVYxaFkzbz0=@20.189.113.56:29937#网址:www.liumeiti.vip/www.joinvip.vip"
	got := GenerateLinks(user, []model.Line{line}, hkEntry)
	if len(got) != 1 || got[0] != want {
		t.Errorf("ss link\n want: %s\n got:  %v", want, got)
	}
}

// 真实数据黄金对比:设置 MUI_OLD_DB=旧库 与 MUI_NEW_DB=导入后的 m-ui 库时运行。
// 逐用户比对 m-ui 生成的 local 链接与 旧面板 存储的 local 链接(忽略顺序)。
func TestGoldenAgainstRealDB(t *testing.T) {
	oldPath := os.Getenv("MUI_OLD_DB")
	newPath := os.Getenv("MUI_NEW_DB")
	if oldPath == "" || newPath == "" {
		t.Skip("设置 MUI_OLD_DB 与 MUI_NEW_DB 以运行真实数据黄金对比")
	}

	oldDB, err := database.OpenReadOnly(oldPath)
	if err != nil {
		t.Fatal(err)
	}
	newDB, err := database.Open(newPath)
	if err != nil {
		t.Fatal(err)
	}

	// 旧面板 原始链接用域名(Flask 后才改 IP),故入口用域名对齐。
	var domain string
	newDB.Raw("SELECT value FROM settings WHERE key='webDomain'").Scan(&domain)
	entries := []Entry{{Host: domain, SNI: domain}}

	type oldClient struct {
		Name  string
		Links []byte
	}
	var clients []oldClient
	if err := oldDB.Raw("SELECT name, links FROM clients").Scan(&clients).Error; err != nil {
		t.Fatal(err)
	}

	total, mismatched := 0, 0
	for _, c := range clients {
		var stored []struct {
			Type string `json:"type"`
			URI  string `json:"uri"`
		}
		_ = json.Unmarshal(c.Links, &stored)
		goldenSet := map[string]bool{}
		for _, l := range stored {
			if l.Type == "local" {
				goldenSet[l.URI] = true
			}
		}

		var user model.User
		if err := newDB.Where("name = ?", c.Name).First(&user).Error; err != nil {
			continue
		}
		var lines []model.Line
		newDB.Raw(`SELECT l.* FROM lines l JOIN user_lines ul ON ul.line_id = l.id
			WHERE ul.user_id = ? AND l.enabled = 1 ORDER BY l.sort`, user.Id).Scan(&lines)
		got := GenerateLinks(user, lines, entries)

		gotSet := map[string]bool{}
		for _, g := range got {
			gotSet[g] = true
		}

		total++
		if !sameSet(goldenSet, gotSet) {
			mismatched++
			if mismatched <= 3 {
				t.Errorf("用户 %q 链接集合不一致\n  仅 golden 有:\n%s\n  仅 m-ui 有:\n%s",
					c.Name, diffLines(goldenSet, gotSet), diffLines(gotSet, goldenSet))
			}
		}
	}
	t.Logf("真实数据黄金对比:%d 个用户,%d 个不一致", total, mismatched)
}

func sameSet(a, b map[string]bool) bool {
	if len(a) != len(b) {
		return false
	}
	for k := range a {
		if !b[k] {
			return false
		}
	}
	return true
}

func diffLines(a, b map[string]bool) string {
	var only []string
	for k := range a {
		if !b[k] {
			only = append(only, k)
		}
	}
	sort.Strings(only)
	if len(only) > 8 {
		only = only[:8]
	}
	return strings.Join(only, "\n")
}
