package render

import (
	"encoding/json"
	"path/filepath"
	"strings"
	"testing"

	"github.com/Maoyangui/m-ui/database"
	"github.com/Maoyangui/m-ui/database/model"
)

// 线路只写了上游组:组和成员都要进配置;组外的、没人用的不进。
func TestGroupMembersRendered(t *testing.T) {
	db, err := database.Open(filepath.Join(t.TempDir(), "x.db"))
	if err != nil {
		t.Fatal(err)
	}
	defer database.Close(db)
	db.Create(&model.Node{Name: "本机", IsLocal: true, Enabled: true})
	socks := func(name string, port int) model.Upstream {
		b, _ := json.Marshal(map[string]interface{}{"server": "127.0.0.1", "server_port": port})
		u := model.Upstream{Name: name, Type: "socks", Options: b}
		db.Create(&u)
		return u
	}
	socks("a", 1)
	socks("b", 2)
	socks("idle", 3)
	g := model.Upstream{Name: "g", Type: GroupType, Options: []byte(`{"outbounds":["a","b"],"url":"http://x/","interval":"30s","tolerance":300}`)}
	db.Create(&g)
	db.Create(&model.Line{Name: "l", Protocol: "shadowsocks", Port: 31000, Enabled: true, UpstreamId: g.Id,
		Options: []byte(`{"method":"aes-128-gcm","password":"pw12345678"}`)})
	raw, err := BuildConfig(db, NodeCert{})
	if err != nil {
		t.Fatal(err)
	}
	var cfg struct {
		Outbounds []struct {
			Tag  string `json:"tag"`
			Type string `json:"type"`
		} `json:"outbounds"`
	}
	if err := json.Unmarshal(raw, &cfg); err != nil {
		t.Fatal(err)
	}
	got := map[string]string{}
	for _, o := range cfg.Outbounds {
		got[o.Tag] = o.Type
	}
	if got["g"] != GroupType || got["a"] != "socks" || got["b"] != "socks" {
		t.Fatalf("组和成员都要进配置: %v", got)
	}
	if _, ok := got["idle"]; ok {
		t.Fatalf("没人用的不进: %v", got)
	}
	if !strings.Contains(string(raw), `"outbound":"g"`) && !strings.Contains(string(raw), `"outbound": "g"`) {
		t.Fatalf("线路的路由要指向组: %s", raw)
	}
}

func TestExpandIDs(t *testing.T) {
	all := []model.Upstream{
		{Id: 1, Name: "a", Type: "socks"}, {Id: 2, Name: "b", Type: "socks"}, {Id: 3, Name: "c", Type: "socks"},
		{Id: 4, Name: "g", Type: GroupType, Options: []byte(`{"outbounds":["b","a","gone"]}`)},
	}
	if got := ExpandIDs([]uint{4}, all); len(got) != 3 || got[0] != 1 || got[1] != 2 || got[2] != 4 {
		t.Fatalf("组展开成成员,不存在的成员跳过,结果排好序: %v", got)
	}
	if got := ExpandIDs([]uint{3}, all); len(got) != 1 || got[0] != 3 {
		t.Fatalf("不是组就是它自己: %v", got)
	}
}
