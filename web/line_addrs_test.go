package web

import (
	"encoding/json"
	"path/filepath"
	"strings"
	"testing"

	"github.com/Maoyangui/m-ui/database"
	"github.com/Maoyangui/m-ui/database/model"
)

// 线路自定义地址保存时规整:IPv6 写成 [v6](和服务器页"连接地址"一样的习惯写法)、前后带空格都存成裸地址,
// 以后不会再拼出 [[v6]]:端口;写成 地址:端口、带协议头、端口越界、类型不对的直接拦下并说清哪一条。
func TestValidateLineAddrsNormalizesIPv6(t *testing.T) {
	db, err := database.Open(filepath.Join(t.TempDir(), "x.db"))
	if err != nil {
		t.Fatal(err)
	}
	defer database.Close(db)
	s := &Server{db: db}
	save := func(addrs string) (json.RawMessage, error) {
		l := model.Line{Name: "a", Protocol: "trojan", Port: 30461, Tls: []byte(`{"mode":"cert"}`)}
		if addrs != "" {
			l.Addrs = json.RawMessage(addrs)
		}
		err := s.validateLine(&l)
		return l.Addrs, err
	}

	got, err := save(` [ {"server":" [2001:db8::1] ","server_port":443,"remark":"-备用","sni":"a.example.com","node_id":2},
		{"server":"hk.example.com"}, {"server":"203.0.113.9","server_port":0}, {"server":"2001:db8::1:443"} ] `)
	if err != nil {
		t.Fatal(err)
	}
	var list []map[string]interface{}
	if err := json.Unmarshal(got, &list); err != nil || len(list) != 4 {
		t.Fatalf("回写的不是数组: %s %v", got, err)
	}
	if list[0]["server"] != "2001:db8::1" || list[0]["server_port"] != float64(443) || list[0]["remark"] != "-备用" ||
		list[0]["sni"] != "a.example.com" || list[0]["node_id"] != float64(2) {
		t.Fatalf("第 1 条应去掉方括号和空格、其余字段照留: %v", list[0])
	}
	if list[1]["server"] != "hk.example.com" || list[2]["server_port"] != float64(0) {
		t.Fatalf("域名、端口 0(用线路端口)照留: %v", list)
	}
	// 2001:db8::1:443 本身就是一个合法的 IPv6 地址(最后一段是 443),没法当成"地址:端口"拆,按地址原样存
	if list[3]["server"] != "2001:db8::1:443" {
		t.Fatalf("合法的 IPv6 地址照存: %v", list[3])
	}

	for _, c := range []struct{ in, want string }{
		{`[{"server":"[2001:db8::1]:443"}]`, "不带端口"},
		{`[{"server":"1.2.3.4:443"}]`, "不带端口"},
		{`[{"server":"https://a.example.com/x"}]`, "不带端口"},
		{`[{"server":"a b.example.com"}]`, "不带端口"},
		{`[{"server":"1.2.3.4"},{"server":""}]`, "第 2 个"},
		{`[{"remark":"x"}]`, `{"server":"IP 或域名"}`},
		{`["1.2.3.4"]`, `{"server":"IP 或域名"}`},
		{`[{"server":"1.2.3.4","server_port":70000}]`, "0-65535"},
		{`[{"server":"1.2.3.4","server_port":-1}]`, "0-65535"},
		{`[{"server":"1.2.3.4","server_port":"443"}]`, "0-65535"},
		{`[{"server":"1.2.3.4","server_port":44.5}]`, "0-65535"},
		{`[{"server":"1.2.3.4","node_id":-1}]`, "node_id"},
		{`[{"server":"1.2.3.4","sni":5}]`, "sni"},
		{`{"server":"1.2.3.4"}`, "JSON 数组"},
		{`[{"server":"1.2.3.4"}] [1]`, "JSON 数组"},
	} {
		if _, err := save(c.in); err == nil || !strings.Contains(err.Error(), c.want) {
			t.Errorf("%s 应被拦下(含 %q),得 %v", c.in, c.want, err)
		}
	}
	for _, empty := range []string{"", "null", " [ ] "} {
		if got, err := save(empty); err != nil || got != nil {
			t.Errorf("%q 应存空,得 %s %v", empty, got, err)
		}
	}
}
