package runner

import (
	"net"
	"strconv"

	"github.com/Maoyangui/m-ui/database/model"
	"github.com/Maoyangui/m-ui/render"
)

// FreePortNear 从 start 往后找一个本机空闲的 TCP 端口(避开本机的面板 / 订阅 / 代理面板 / WARP 端口和部署在本机的线路):
// 刚接任 / 被设为主机、迁来的端口和本机原来的端口都被别的程序占着时最后的退路(见 ha.FreePortFallback)。找不到返回 0。
func (r *Runner) FreePortNear(start int) int {
	avoid := map[int]bool{}
	for _, k := range []string{"webPort", "subPort", "resellerPort", "warpPort"} {
		if p, err := strconv.Atoi(r.setting(k)); err == nil {
			avoid[p] = true
		}
	}
	var lines []model.Line
	r.db.Select("id, port, protocol, node_ids").Find(&lines)
	self := r.LocalNodeId()
	for _, l := range lines {
		if render.LineOnNode(l, self) {
			avoid[l.Port] = true
		}
	}
	if start < 1024 {
		start = 1024
	}
	for p := start + 1; p <= 65535 && p <= start+500; p++ {
		if avoid[p] {
			continue
		}
		ln, err := net.Listen("tcp", ":"+strconv.Itoa(p))
		if err != nil {
			continue
		}
		_ = ln.Close()
		return p
	}
	return 0
}
