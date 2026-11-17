package sub

import (
	"strings"

	"github.com/Maoyangui/m-ui/database/model"
	"gorm.io/gorm"
)

// Endpoint 订阅里这条线路给用户的一个地址(一个节点):连哪个地址、哪个端口。
type Endpoint struct {
	Key    string // 节点键(model.DirectKey / ForwardKey);没指定服务器的自定义地址为空
	Label  string // 这个节点叫什么:订阅里节点名的后缀(服务器名、「出口-中转」),没有后缀时是服务器名或地址本身
	Server string // 地址(IP 或域名;IPv6 不带方括号)
	Port   int
	NodeId uint // 直连地址落在哪台服务器;中转入口、没指定服务器的自定义地址为 0
	Relay  bool // 中转入口
}

// LineEndpoints 这条线路在订阅里的全部地址:部署到的每台服务器(或线路的自定义地址),接着是它的中转入口;
// 线路上关了输出的节点不给。不看某个用户(用户被收窄到哪几台、用户自己关掉了哪些节点都不管)。
// 线路详情里的「测大陆连通」按它来测:测的就是用户实际连的那个地址和端口。
func LineEndpoints(db *gorm.DB, line model.Line) []Endpoint {
	return lineEndpoints(line, NewServer(db).options().Entries)
}

func lineEndpoints(line model.Line, entries []Entry) []Endpoint {
	names := make(map[uint]string, len(entries))
	for _, e := range entries {
		names[e.NodeId] = e.Name
	}
	addrs := subAddrs(line, entries, nil, nil)
	out := make([]Endpoint, 0, len(addrs))
	for _, a := range addrs {
		label := strings.TrimSpace(strings.TrimPrefix(a.remark, "-"))
		if label == "" && !a.relay {
			label = names[a.node] // 只有一台服务器时节点名没有后缀
		}
		if label == "" {
			label = a.server
		}
		out = append(out, Endpoint{Key: a.key(), Label: label, Server: a.server, Port: a.port, NodeId: a.node, Relay: a.relay})
	}
	return out
}
