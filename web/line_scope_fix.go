package web

import (
	"encoding/json"
	"sort"
	"strconv"

	"github.com/Maoyangui/m-ui/database/model"

	"gorm.io/gorm"
)

// scopeWidened 收窄线路的部署服务器后,哪些分配 / 授权 / 套餐因为原来只选了被撤掉的服务器,被改回了「这条线路的全部服务器」。
// 返回给管理员看(界面提示 + 审计),都为空时不出现在返回里。
type scopeWidened struct {
	Users     []string `json:"users,omitempty"`
	Resellers []string `json:"resellers,omitempty"`
	Plans     []string `json:"plans,omitempty"`
}

func (w *scopeWidened) empty() bool {
	return w == nil || len(w.Users)+len(w.Resellers)+len(w.Plans) == 0
}

func (w *scopeWidened) merge(o *scopeWidened) {
	if o == nil {
		return
	}
	w.Users = append(w.Users, o.Users...)
	w.Resellers = append(w.Resellers, o.Resellers...)
	w.Plans = append(w.Plans, o.Plans...)
}

// fixLineScopesTx 线路的部署服务器改成 nodeIDs(空 = 全部服务器)后,整理指向已撤服务器的「线路 × 服务器」收窄
// (用户分配 user_line_nodes、代理授权 reseller_line_nodes、套餐 LineNodes),审计 Q-32 #4:
//   - 还收窄到别的(仍部署着的)服务器的:只去掉已撤的那几台;
//   - 只剩已撤服务器的:改回「这条线路的全部服务器」,返回名字给管理员看。
//
// 为什么改回全部而不是拒绝保存或撤掉分配:渲染与订阅按「有收窄行就只给这几台」处理,留着这些行用户会悄悄没了这条线路;
// 撤掉分配同样是没了线路。改回全部后用户照常能用,而且能拿到的正是管理员这一次亲手选定的部署服务器,不会多出
// 管理员没让这条线路去的机器;拒绝保存则要求先逐个去改用户 / 代理 / 套餐(可能成百上千个),日常调整部署做不下去。
// 名字在返回里点名、记进审计,想再收窄的可以照名单去改。删除服务器走的是另一条路(revokeSoleNodeScopes):
// 那时管理员并没有为这条线路选服务器,保持撤销并列出。
func fixLineScopesTx(tx *gorm.DB, lineID uint, nodeIDs []uint) (*scopeWidened, error) {
	if len(nodeIDs) == 0 {
		return nil, nil // 全部服务器:任何收窄都还有效
	}
	keep := make(map[uint]bool, len(nodeIDs))
	for _, id := range nodeIDs {
		keep[id] = true
	}
	out := &scopeWidened{}

	// 用户分配
	var ul []model.UserLineNode
	if err := tx.Where("line_id = ?", lineID).Find(&ul).Error; err != nil {
		return nil, err
	}
	stale, left := map[uint]bool{}, map[uint]bool{}
	for _, x := range ul {
		if keep[x.NodeId] {
			left[x.UserId] = true
		} else {
			stale[x.UserId] = true
		}
	}
	var widenUsers []uint
	for uid := range stale {
		if !left[uid] {
			widenUsers = append(widenUsers, uid)
		}
	}
	if len(stale) > 0 {
		if err := tx.Where("line_id = ? AND node_id NOT IN ?", lineID, nodeIDs).Delete(&model.UserLineNode{}).Error; err != nil {
			return nil, err
		}
	}

	// 代理授权
	var rl []model.ResellerLineNode
	if err := tx.Where("line_id = ?", lineID).Find(&rl).Error; err != nil {
		return nil, err
	}
	rStale, rLeft := map[uint]bool{}, map[uint]bool{}
	for _, x := range rl {
		if keep[x.NodeId] {
			rLeft[x.ResellerId] = true
		} else {
			rStale[x.ResellerId] = true
		}
	}
	var widenResellers []uint
	for rid := range rStale {
		if !rLeft[rid] {
			widenResellers = append(widenResellers, rid)
		}
	}
	if len(rStale) > 0 {
		if err := tx.Where("line_id = ? AND node_id NOT IN ?", lineID, nodeIDs).Delete(&model.ResellerLineNode{}).Error; err != nil {
			return nil, err
		}
	}

	// 套餐(主面板与代理自建的都在这张表里)
	key := strconv.Itoa(int(lineID))
	var plans []model.Plan
	if err := tx.Select("id, name, line_nodes").Where("line_nodes IS NOT NULL AND line_nodes <> ''").Find(&plans).Error; err != nil {
		return nil, err
	}
	for _, p := range plans {
		nodes := map[string][]uint{}
		if json.Unmarshal(p.LineNodes, &nodes) != nil {
			continue
		}
		old, ok := nodes[key]
		if !ok || len(old) == 0 {
			continue
		}
		kept := make([]uint, 0, len(old))
		for _, n := range old {
			if keep[n] {
				kept = append(kept, n)
			}
		}
		if len(kept) == len(old) {
			continue
		}
		if len(kept) == 0 {
			delete(nodes, key)
			out.Plans = append(out.Plans, p.Name)
		} else {
			nodes[key] = kept
		}
		var raw json.RawMessage // 一条收窄都不剩就清空字段(和 lineRefsJSON 一致)
		if len(nodes) > 0 {
			raw, _ = json.Marshal(nodes)
		}
		if err := tx.Model(&model.Plan{}).Where("id = ?", p.Id).Update("line_nodes", raw).Error; err != nil {
			return nil, err
		}
	}

	if len(widenUsers) > 0 {
		if err := tx.Model(&model.User{}).Where("id IN ?", widenUsers).Order("name").Pluck("name", &out.Users).Error; err != nil {
			return nil, err
		}
	}
	if len(widenResellers) > 0 {
		if err := tx.Model(&model.Reseller{}).Where("id IN ?", widenResellers).Order("name").Pluck("name", &out.Resellers).Error; err != nil {
			return nil, err
		}
	}
	sort.Strings(out.Plans)
	if out.empty() {
		return nil, nil
	}
	return out, nil
}

// deployNodeIDs 线路的部署服务器;空 = 全部服务器(解析不了也按全部,不去动任何收窄)。
func deployNodeIDs(raw json.RawMessage) []uint {
	if len(raw) == 0 {
		return nil
	}
	var ids []uint
	if json.Unmarshal(raw, &ids) != nil {
		return nil
	}
	return ids
}

// dedupSorted 排序去重(批量改多条线路时同一个用户可能在几条线路上都被改回全部)。
func dedupSorted(xs []string) []string {
	if len(xs) == 0 {
		return xs
	}
	sort.Strings(xs)
	out := xs[:1]
	for _, x := range xs[1:] {
		if x != out[len(out)-1] {
			out = append(out, x)
		}
	}
	return out
}
