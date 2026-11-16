package sub

import (
	"encoding/json"
	"net/url"
	"strings"
	"testing"

	"github.com/Maoyangui/m-ui/database/model"
)

// 订阅里的「节点」可以逐个关:线路上关了输出的(NodeOpts 的 hidden)谁都不给,用户自己不要的(LineHides)只对他不给;
// 直连按服务器认("s<服务器 id>"),中转入口按编号认("f<入口 id>")。三种订阅格式同一口径。
// 「这台上已有转发」(Manual)和面板转发一样用那台服务器的地址。
func TestNodeOptsAndUserHides(t *testing.T) {
	user := model.User{Name: "u", Credentials: []byte(`{"hysteria2":{"password":"p"}}`)}
	line := fwLine(t,
		model.LineForward{Id: 1, Via: 1, Port: 30001, To: 2},
		model.LineForward{Id: 2, Via: 1, Manual: true, Port: 13024, To: 3, Name: "realm"},
	)
	hosts := func(links []string) []string {
		t.Helper()
		out := make([]string, 0, len(links))
		for _, l := range links {
			u, err := url.Parse(l)
			if err != nil {
				t.Fatal(err)
			}
			out = append(out, u.Host)
		}
		return out
	}
	eq := func(what string, got []string, want ...string) {
		t.Helper()
		if strings.Join(got, " ") != strings.Join(want, " ") {
			t.Fatalf("%s:\n got %v\nwant %v", what, got, want)
		}
	}
	all := []string{"jp.example.com:443", "203.0.113.3:443", "gz.example.com:30001", "gz.example.com:13024"}
	eq("什么都没关:两台直连 + 两个中转(已有转发的那个也用中转服务器的地址)", hosts(GenerateLinksFor(user, []model.Line{line}, fwEntries, nil)), all...)

	// 线路上把东京的直连关掉(只走中转)、把 2 号入口关掉
	opts, _ := json.Marshal(map[string]model.NodeOpt{model.DirectKey(2): {Hidden: true}, model.ForwardKey(2): {Hidden: true}})
	hidden := line
	hidden.NodeOpts = opts
	eq("线路上关掉的谁都不给", hosts(GenerateLinksFor(user, []model.Line{hidden}, fwEntries, nil)), "203.0.113.3:443", "gz.example.com:30001")

	// 用户自己不要大阪的直连和 1 号入口
	hides := LineHides{7: {model.DirectKey(3): true, model.ForwardKey(1): true}}
	eq("用户不要的只对他不给", hosts(GenerateLinksFor(user, []model.Line{line}, fwEntries, nil, hides)), "jp.example.com:443", "gz.example.com:13024")
	eq("两层一起算", hosts(GenerateLinksFor(user, []model.Line{hidden}, fwEntries, nil, hides)))
	eq("别的线路的不要不相干", hosts(GenerateLinksFor(user, []model.Line{line}, fwEntries, nil, LineHides{8: {model.DirectKey(2): true}})), all...)

	// Clash 与 sing-box 订阅同一口径
	clash, err := BuildClashFor(user, []model.Line{hidden}, fwEntries, "", "", nil, nil, hides)
	if err != nil {
		t.Fatal(err)
	}
	if strings.Contains(clash, "30001") || strings.Contains(clash, "13024") || strings.Contains(clash, "jp.example.com") || strings.Contains(clash, "203.0.113.3") {
		t.Fatalf("Clash 订阅里不该有被关掉 / 不要的节点:\n%s", clash)
	}
	sb, err := BuildSingBoxSub(user, []model.Line{hidden}, Options{Entries: fwEntries, LineHides: hides})
	if err != nil {
		t.Fatal(err)
	}
	if strings.Contains(sb.Body, "30001") || strings.Contains(sb.Body, "gz.example.com") {
		t.Fatalf("sing-box 订阅里不该有被关掉 / 不要的节点:\n%s", sb.Body)
	}
	sb, err = BuildSingBoxSub(user, []model.Line{hidden}, Options{Entries: fwEntries})
	if err != nil {
		t.Fatal(err)
	}
	if !strings.Contains(sb.Body, "30001") || strings.Contains(sb.Body, "13024") || strings.Contains(sb.Body, `"server": "jp.example.com"`) || !strings.Contains(sb.Body, `"server": "203.0.113.3"`) {
		t.Fatalf("sing-box 订阅应只剩大阪直连和 1 号入口:\n%s", sb.Body)
	}
}

// 没有编号的老入口(0.7.17 建的)保存 / 启动时补编号:从现有最大的往后排,已有的不动,重复的后一个重排。
func TestAssignForwardIds(t *testing.T) {
	fs := []model.LineForward{{Port: 1}, {Id: 5, Port: 2}, {Port: 3}, {Id: 5, Port: 4}}
	if !model.AssignForwardIds(fs) {
		t.Fatal("有没编号的应返回 true")
	}
	if fs[1].Id != 5 || fs[0].Id != 6 || fs[2].Id != 7 || fs[3].Id != 8 {
		t.Fatalf("编号不对: %+v", fs)
	}
	if model.AssignForwardIds(fs) {
		t.Fatal("都有编号了不该再改")
	}
}
