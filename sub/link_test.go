package sub

import (
	"encoding/json"
	"net/url"
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

// ss 用自定义地址(网址节点指向独立服务器),地址取 addrs;备注按 URL 片段编码,
// 与其它协议一致(线路名里出现空格或 # 时不会把链接弄坏,客户端解码后显示原名)。
func TestShadowsocksLinkWithAddr(t *testing.T) {
	user := mkUser()
	opts, _ := json.Marshal(map[string]interface{}{"method": "aes-256-gcm"})
	addrs, _ := json.Marshal([]map[string]interface{}{{"server": "20.189.113.56", "server_port": 29937}})
	line := model.Line{Name: "网址:www.liumeiti.vip/www.joinvip.vip", Protocol: "shadowsocks", Port: 29937, Options: opts, Addrs: addrs}
	want := "ss://YWVzLTI1Ni1nY206T2hyNU5ZbmRIWURnWVhVYWZFTzlQeG9OVkIyT3dsRkltZTkydVYxaFkzbz0=@20.189.113.56:29937#%E7%BD%91%E5%9D%80:www.liumeiti.vip/www.joinvip.vip"
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

// 线路名里有空格、# 或中文时,所有协议的链接都必须仍然可解析(备注按片段编码)。
func TestRemarkEncodingAcrossProtocols(t *testing.T) {
	user := model.User{Name: "u", Credentials: []byte(`{"hysteria2":{"password":"p"},"anytls":{"password":"p"},
		"tuic":{"uuid":"11111111-1111-1111-1111-111111111111","password":"p"},"trojan":{"password":"p"},
		"vless":{"uuid":"22222222-2222-2222-2222-222222222222"},"vmess":{"uuid":"33333333-3333-3333-3333-333333333333"},
		"shadowsocks16":{"password":"aaaaaaaaaaaaaaaaaaaaaa=="},"socks":{"username":"u","password":"p"},"http":{"username":"u","password":"p"}}`)}
	lines := []model.Line{
		{Name: "香港 1 #促销", Protocol: "hysteria2", Port: 1, Enabled: true},
		{Name: "香港 1 #促销", Protocol: "anytls", Port: 2, Enabled: true},
		{Name: "香港 1 #促销", Protocol: "shadowsocks", Port: 3, Enabled: true, Options: []byte(`{"method":"2022-blake3-aes-128-gcm","password":"c2VydmVycHNrc2VydmVycHNr"}`)},
		{Name: "香港 1 #促销", Protocol: "tuic", Port: 4, Enabled: true},
		{Name: "香港 1 #促销", Protocol: "trojan", Port: 5, Enabled: true, Tls: []byte(`{"mode":"cert"}`)},
		{Name: "香港 1 #促销", Protocol: "vless", Port: 6, Enabled: true},
		{Name: "香港 1 #促销", Protocol: "socks", Port: 7, Enabled: true},
	}
	for _, l := range lines {
		links := GenerateLinks(user, []model.Line{l}, []Entry{{Host: "hk.example"}})
		if len(links) == 0 {
			t.Fatalf("%s 没生成链接", l.Protocol)
		}
		for _, link := range links {
			u, err := url.Parse(link)
			if err != nil {
				t.Fatalf("%s 链接解析失败: %v (%s)", l.Protocol, err, link)
			}
			if u.Fragment != l.Name {
				t.Fatalf("%s 备注应还原成线路名,得 %q(%s)", l.Protocol, u.Fragment, link)
			}
			if strings.Contains(link, " ") {
				t.Fatalf("%s 链接里不该有裸空格: %s", l.Protocol, link)
			}
		}
	}
}
