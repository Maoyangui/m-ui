package web

import (
	"fmt"
	"regexp"

	"github.com/Maoyangui/m-ui/database/model"
	"github.com/Maoyangui/m-ui/render"

	"gorm.io/gorm"
)

// 比「线路 × 服务器」再细一层:一条线路在订阅里是几个节点(每台服务器上的直连各一个、每个中转入口各一个),
// 用户可以不要其中某几个 —— 和「只给这台服务器上的入口」一样逐个勾。
//
// 接口里是 LineRef.Hide(节点键,见 model.DirectKey / ForwardKey);库里是 user_line_hides,套餐存在 Plan.LineHides。
// 只影响订阅里发不发:直连和经中转进来的是同一个入站、同一份凭据,数据面分不开,所以不要某个节点的人照样连得上它
// (拿到别人的链接才行),这一层不是权限,是「订阅里列不列」。
// 「要哪些服务器」仍由 NodeIds 定:只要某台的中转入口、不要它的直连 = 服务器范围里有这台 + 不要它的直连节点。

// nodeKeyRe 节点键的写法:s<服务器 id> / f<入口 id>。
var nodeKeyRe = regexp.MustCompile(`^[sf][1-9][0-9]{0,9}$`)

// refNodeKeys 一份分配(scope = 收窄到的服务器,空 = 全部)在这条线路上够得着的节点:
// all 认得的全部(暂停着的中转入口、线路上关了输出的也算:这期间「不要它」照样留着);
// live 现在真会发进订阅的(入口没暂停、线路上没关输出)。出口不在范围里的中转入口本来就不发,不算。
func refNodeKeys(line model.Line, nodes []model.Node, scope []uint) (all, live map[string]bool) {
	in := func(id uint) bool {
		if len(scope) == 0 {
			return true
		}
		for _, x := range scope {
			if x == id {
				return true
			}
		}
		return false
	}
	opts := model.ParseNodeOpts(line.NodeOpts)
	all, live = map[string]bool{}, map[string]bool{}
	for _, n := range nodes {
		if !render.LineOnNode(line, n.Id) || !in(n.Id) {
			continue
		}
		k := model.DirectKey(n.Id)
		all[k] = true
		if !opts[k].Hidden {
			live[k] = true
		}
	}
	for _, f := range model.ParseForwards(line.Forwards) {
		if f.Id <= 0 || !render.LineOnNode(line, f.To) || !in(f.To) {
			continue
		}
		k := model.ForwardKey(f.Id)
		all[k] = true
		if !f.Off && !opts[k].Hidden {
			live[k] = true
		}
	}
	return all, live
}

// checkRefHides 一条线路的节点不能一个都不要:那等于没给这条线路,用户订阅里却什么都没有、凭据还占着。
// refs 是整理过的(normalizeRefs)。
func (s *Server) checkRefHides(refs []model.LineRef) error {
	need := false
	for _, r := range refs {
		if len(r.Hide) > 0 {
			need = true
			break
		}
	}
	if !need {
		return nil
	}
	var lines []model.Line
	s.db.Select("id, name, node_ids, forwards, node_opts").Find(&lines)
	var nodes []model.Node
	s.db.Select("id").Find(&nodes)
	by := make(map[uint]model.Line, len(lines))
	for _, l := range lines {
		by[l.Id] = l
	}
	for _, r := range refs {
		line, ok := by[r.LineId]
		if !ok || len(r.Hide) == 0 {
			continue
		}
		_, live := refNodeKeys(line, nodes, r.NodeIds)
		hide := make(map[string]bool, len(r.Hide))
		for _, k := range r.Hide {
			hide[k] = true
		}
		left := 0
		for k := range live {
			if !hide[k] {
				left++
			}
		}
		if len(live) > 0 && left == 0 {
			return fmt.Errorf("线路「%s」的节点一个都没留:至少留一个,或者把这条线路整条去掉", line.Name)
		}
	}
	return nil
}

// userLineHides 用户不要的节点(userID = 0 取全部用户)。
func userLineHides(db *gorm.DB, userID uint) []model.UserLineHide {
	q := db.Order("user_id asc, line_id asc, node_key asc")
	if userID > 0 {
		q = q.Where("user_id = ?", userID)
	}
	var rows []model.UserLineHide
	q.Find(&rows)
	return rows
}
