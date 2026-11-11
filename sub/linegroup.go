package sub

import (
	"fmt"
	"strings"

	"github.com/Maoyangui/m-ui/database/model"
)

// 线路测速组:部署在多台服务器上的线路,Clash 订阅出 url-test 组、sing-box JSON 出 urltest 组,
// 客户端选这条线路,由组在这几台服务器里挑延迟低的。两种格式按同一套规则起组名,
// 同一份输入两边的组名一样,用户换客户端还认得:
//
//   - 本站节点先定名(名字和以前一样),接着给线路组起名,外部节点最后:外部订阅里冒出和线路同名的节点时,
//     改名的是外部节点,选过这个线路组的客户端不会被悄悄换到第三方节点上;
//   - 组名就是线路名,撞了两种格式的内置名(lineGroupReserved)、模板里写的名字、本站节点或前面的线路组就加序号
//     (「香港 2」「香港 3」……)。mihomo 和 sing-box 里节点与组共用一张名字表,重名整份配置导入失败。

// clashBuiltins mihomo 内置的出站:节点、策略组都不能和它们重名。
var clashBuiltins = []string{"DIRECT", "REJECT", "REJECT-DROP", "PASS", "PASS-RULE", "COMPATIBLE"}

// lineGroupReserved 线路组不用的名字:两种格式生成的组和内置出站合在一起,两边起出来的组名才一样。
var lineGroupReserved = func() map[string]bool {
	m := map[string]bool{
		"proxy": true, "auto": true, "direct": true, // sing-box JSON 生成的选择组、测速组、直连
		"Proxy": true, "Auto": true, // Clash 生成的选择组、测速组
		"GLOBAL": true, // mihomo 自动生成的全局选择组(配置里写了同名的组,它就拿那个组当全局组)
	}
	for _, n := range clashBuiltins {
		m[n] = true
	}
	return m
}()

// lineGroup 一条线路的测速组:line 是线路名,members 是这条线路在用户手里的节点(名字已定)。
type lineGroup struct {
	line    string
	members []string
}

// wantsLineGroup 这条线路出不出测速组:用户手里有不止一个入口(没被收窄到一台);
// mixed 一个入口本来就出 socks + http 两个节点,不算。
func wantsLineGroup(line model.Line, members int) bool {
	return members > 1 && line.Protocol != "mixed" && strings.TrimSpace(line.Name) != ""
}

// nameLineGroups 按上面的规则给线路组起名,名字和 groups 一一对应,并记进 used(外部节点随后照它让名)。
//   - used:到此为止占用的名字(内置出站、模板里写的、本站节点);
//   - avoid:另外只拦线路组、不拦节点的名字(Clash 模板的 proxy-providers:列了成员的组在 mihomo 里还占一个同名 provider);
//   - tmplGroup(name):模板里叫 name 的是一个组,可以拿来当这条线路的组 —— 线路组就叫这个名字,生成的同名组由模板的顶替。
func nameLineGroups(groups []lineGroup, used, avoid map[string]bool, tmplGroup func(string) bool) []string {
	taken := func(n string) bool { return used[n] || avoid[n] || lineGroupReserved[n] }
	names := make([]string, len(groups))
	mine := map[string]bool{} // 已经给了线路组的名字:模板的一个组只顶替一条线路
	for i, g := range groups {
		name := g.line
		if mine[name] || avoid[name] || lineGroupReserved[name] || !tmplGroup(name) {
			for n := 2; taken(name); n++ {
				name = fmt.Sprintf("%s %d", g.line, n)
			}
		}
		used[name], mine[name] = true, true
		names[i] = name
	}
	return names
}
