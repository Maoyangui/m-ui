package hub

import (
	"encoding/json"
	"strings"
	"testing"

	"github.com/Maoyangui/m-ui/database/model"
	"github.com/Maoyangui/m-ui/ext"
	"github.com/Maoyangui/m-ui/selfupdate"
)

// 审计 I5:隐藏 / 改名 / 排除关键词是 0.7.16 加的字段,按指纹的记录是 0.7.17 加的。以前不看外部节点,门槛一直是 0.6.0:
// 旧副机照收快照、解码时把这些字段丢掉,订阅地址指向它的用户照样拿到被隐藏的节点,主机还显示"已同步"。
func TestMinNodeRaisedForExtFilters(t *testing.T) {
	if got := minNodeFor(nil, nil, []model.ExtNode{{Name: "a", Enabled: true, Hidden: json.RawMessage(`[]`), Renames: json.RawMessage(`{}`),
		HiddenKeys: json.RawMessage(`{}`), RenameKeys: json.RawMessage(`{}`), Exclude: " "}}); got != MinNodeVersion {
		t.Fatalf("没用筛选时门槛不变: %s", got)
	}
	for name, e := range map[string]model.ExtNode{
		"按名字隐藏": {Enabled: true, Hidden: json.RawMessage(`["x"]`)},
		"按名字改名": {Enabled: true, Renames: json.RawMessage(`{"x":"y"}`)},
		"排除关键词": {Enabled: true, Exclude: "官网"},
	} {
		if got := minNodeFor(nil, nil, []model.ExtNode{e}); got != ExtFilterMinNode {
			t.Errorf("%s:门槛应为 %s,得到 %s", name, ExtFilterMinNode, got)
		}
	}
	keys := model.ExtNode{Enabled: true, HiddenKeys: json.RawMessage(`{"0123456789abcdef":""}`)}
	if got := minNodeFor(nil, []model.Upstream{{Name: "u", FromLine: 1}}, []model.ExtNode{keys}); got != ExtKeyMinNode {
		t.Fatalf("按指纹的记录要求 %s(高于线路上游的 %s),得到 %s", ExtKeyMinNode, LineUpstreamMinNode, got)
	}
	if got := minNodeFor(nil, nil, []model.ExtNode{{Enabled: true, RenameKeys: json.RawMessage(`{"k":{"to":"n"}}`)}}); got != ExtKeyMinNode {
		t.Fatalf("按指纹改名同样要求 %s: %s", ExtKeyMinNode, got)
	}
	if got := minNodeFor(nil, nil, []model.ExtNode{{Enabled: false, Exclude: "官网", HiddenKeys: json.RawMessage(`{"k":""}`)}}); got != MinNodeVersion {
		t.Fatalf("停用的外部节点不输出,不抬门槛: %s", got)
	}
	if !selfupdate.Newer(ExtKeyMinNode, ExtFilterMinNode) || !selfupdate.Newer(ExtFilterMinNode, RouteRulesMinNode) || selfupdate.Newer(LineUpstreamMinNode, ExtFilterMinNode) {
		t.Fatal("门槛的先后关系不对(minNodeFor 按从高到低检查)")
	}
}

const infoYAML = "proxies:\n" +
	"  - {name: '剩余流量:120.53 GB', type: ss, server: 127.0.0.1, port: 1, cipher: aes-128-gcm, password: x}\n" +
	"  - {name: '香港 01', type: ss, server: 1.2.3.4, port: 443, cipher: aes-128-gcm, password: x}\n"

// 修订号对外部节点只看输出到订阅的节点:被隐藏的信息节点数字变了不算配置变了(以前每半小时一刷新就向所有副机推一次全量);
// 输出的节点、筛选条件变了照样算。抓取时间、节点数、内容指纹这些主机自己的记录不算。
func TestRevisionStableWhenOnlyInfoNodeNumbersChange(t *testing.T) {
	info := ext.Parse(infoYAML).Nodes[0]
	hk, _ := json.Marshal(map[string]string{info.Key: info.Name})
	snap := func(cache string, mut func(*model.ExtNode)) Snapshot {
		e := model.ExtNode{Id: 1, Name: "A", Type: "sub", Enabled: true, Cache: cache, HiddenKeys: hk}
		if mut != nil {
			mut(&e)
		}
		return Snapshot{Exts: []model.ExtNode{e}}
	}
	base := revisionOf(snap(infoYAML, nil))
	refreshed := strings.Replace(infoYAML, "120.53", "118.07", 1)
	if got := revisionOf(snap(refreshed, func(e *model.ExtNode) {
		e.LastFetch, e.LastTry, e.NodeCount, e.NodeTotal, e.ContentSig, e.FetchAgent, e.LastError = 99, 99, 1, 2, "x", "v2rayn", "e"
	})); got != base {
		t.Fatal("只有被隐藏的信息节点数字变了、外加主机自己的记录变了,修订号不该变")
	}
	if revisionOf(snap(strings.Replace(infoYAML, "1.2.3.4", "1.2.3.5", 1), nil)) == base {
		t.Fatal("输出的节点变了,修订号要变")
	}
	if revisionOf(snap(infoYAML, func(e *model.ExtNode) { e.HiddenKeys = nil })) == base {
		t.Fatal("隐藏设置变了,修订号要变")
	}
	if revisionOf(snap(infoYAML, func(e *model.ExtNode) { e.Prefix = "[x] " })) == base {
		t.Fatal("前缀变了,修订号要变")
	}
	// 快照里下发给副机的仍是完整缓存(副机自己按筛选条件输出)
	s := snap(refreshed, nil)
	_ = revisionOf(s)
	if s.Exts[0].Cache != refreshed {
		t.Fatal("算修订号不能改到要下发的快照")
	}
}
