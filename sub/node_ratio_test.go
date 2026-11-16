package sub

import (
	"net/url"
	"testing"

	"github.com/Maoyangui/m-ui/database/model"
)

// 节点单独设了倍率(model.NodeOpt.Ratio):订阅里节点名末尾标的是这个节点实际生效的倍率 —— 盖过服务器的
// (改回 1 倍就不标);没单独设的照旧跟服务器(中转入口跟出口那台)。
func TestNodeRatioInNames(t *testing.T) {
	user := model.User{Name: "u", Credentials: []byte(`{"hysteria2":{"password":"p"}}`)}
	line := fwLine(t,
		model.LineForward{Id: 1, Via: 1, Port: 30001, To: 2},
		model.LineForward{Id: 2, Host: "hk.relay.example", Port: 31000, To: 3, Name: "IPLC"},
		model.LineForward{Id: 3, Via: 1, Port: 30002, To: 3},
	)
	line.NodeOpts = []byte(`{"s2":{"ratio":1.5},"s3":{"ratio":1},"f1":{"ratio":3},"f3":{"ratio":0.5}}`)
	links := GenerateLinksFor(user, []model.Line{line}, fwEntries, nil)
	var names []string
	for _, l := range links {
		u, err := url.Parse(l)
		if err != nil {
			t.Fatal(err)
		}
		names = append(names, u.Fragment)
	}
	want := []string{
		"hy2-东京 x1.5",      // 服务器 1 倍,节点单独设了 1.5
		"hy2-大阪",           // 服务器 2 倍,节点单独设回 1 倍:不标
		"hy2-东京-广州中转 x3",   // 中转入口单独设了 3
		"hy2-大阪-IPLC x2",   // 没单独设:跟出口那台(大阪 2 倍),不跟大阪直连节点的 1 倍
		"hy2-大阪-广州中转 x0.5", // 出口 2 倍,入口单独设了 0.5
	}
	if len(names) != len(want) {
		t.Fatalf("应有 %d 个节点,实际 %v", len(want), names)
	}
	for i := range want {
		if names[i] != want[i] {
			t.Errorf("第 %d 个节点名应是 %q,实际 %q", i+1, want[i], names[i])
		}
	}
}
