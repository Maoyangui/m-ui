package render

import (
	"sort"
	"strings"

	"github.com/Maoyangui/m-ui/database/model"

	"gorm.io/gorm"
)

// EffectiveLines 读用户的线路分配(user_lines + user_line_nodes),代理名下的用户再与代理当前的授权取交集:
// 授权收回的线路不算;授权收窄到部分服务器时,只留这些服务器(用户原来是"全部"的,就是授权的那几台)。
// 库里的分配原样保留 —— 重新授权即恢复。数据面、订阅、落地页、下发给副机的快照都经过这里,口径一致。
//
// 副机不再求交集:它的授权表不由旧版主机下发,主机发来的快照里已经是交集后的分配。
// userID 为 0 读全部用户。返回按 (user, line[, node]) 排好序,快照修订号据此稳定。
func EffectiveLines(db *gorm.DB, userID uint) ([]model.UserLine, []model.UserLineNode, error) {
	lq := db.Order("user_id asc, line_id asc")
	sq := db.Order("user_id asc, line_id asc, node_id asc")
	if userID > 0 {
		lq, sq = lq.Where("user_id = ?", userID), sq.Where("user_id = ?", userID)
	}
	var links []model.UserLine
	if err := lq.Find(&links).Error; err != nil {
		return nil, nil, err
	}
	var scopes []model.UserLineNode
	if err := sq.Find(&scopes).Error; err != nil {
		return nil, nil, err
	}
	if isNode(db) {
		return links, scopes, nil
	}
	var owned []model.User // 代理名下的用户 → 所属代理
	uq := db.Model(&model.User{}).Select("id, reseller_id").Where("reseller_id > 0")
	if userID > 0 {
		uq = uq.Where("id = ?", userID)
	}
	if err := uq.Find(&owned).Error; err != nil {
		return nil, nil, err
	}
	if len(owned) == 0 {
		return links, scopes, nil
	}
	owner := make(map[uint]uint, len(owned))
	for _, u := range owned {
		owner[u.Id] = u.ResellerId
	}
	var grantLines []model.ResellerLine
	if err := db.Find(&grantLines).Error; err != nil {
		return nil, nil, err
	}
	var grantNodes []model.ResellerLineNode
	if err := db.Find(&grantNodes).Error; err != nil {
		return nil, nil, err
	}
	type pair struct{ a, b uint }
	granted := map[pair]bool{}           // (代理, 线路) 已授权
	grantSet := map[pair]map[uint]bool{} // (代理, 线路) → 收窄到的服务器;没有 = 全部
	for _, g := range grantLines {
		granted[pair{g.ResellerId, g.LineId}] = true
	}
	for _, g := range grantNodes {
		k := pair{g.ResellerId, g.LineId}
		if grantSet[k] == nil {
			grantSet[k] = map[uint]bool{}
		}
		grantSet[k][g.NodeId] = true
	}
	userSet := map[pair][]uint{} // (用户, 线路) → 用户收窄到的服务器
	for _, sc := range scopes {
		k := pair{sc.UserId, sc.LineId}
		userSet[k] = append(userSet[k], sc.NodeId)
	}

	outLinks := make([]model.UserLine, 0, len(links))
	outScopes := make([]model.UserLineNode, 0, len(scopes))
	for _, l := range links {
		rid, isResold := owner[l.UserId]
		mine := userSet[pair{l.UserId, l.LineId}]
		if !isResold {
			outLinks = append(outLinks, l)
			for _, n := range mine {
				outScopes = append(outScopes, model.UserLineNode{UserId: l.UserId, LineId: l.LineId, NodeId: n})
			}
			continue
		}
		g := pair{rid, l.LineId}
		if !granted[g] {
			continue // 授权已收回
		}
		allowed := grantSet[g]
		var keep []uint
		switch {
		case allowed == nil: // 授权是全部服务器:用户自己的范围原样
			keep = mine
		case len(mine) == 0: // 用户是全部,授权收窄了:就是授权的那几台
			for n := range allowed {
				keep = append(keep, n)
			}
		default:
			for _, n := range mine {
				if allowed[n] {
					keep = append(keep, n)
				}
			}
			if len(keep) == 0 {
				continue // 用户选的服务器都不在授权里了
			}
		}
		outLinks = append(outLinks, l)
		for _, n := range keep {
			outScopes = append(outScopes, model.UserLineNode{UserId: l.UserId, LineId: l.LineId, NodeId: n})
		}
	}
	sort.Slice(outScopes, func(i, j int) bool {
		a, b := outScopes[i], outScopes[j]
		if a.UserId != b.UserId {
			return a.UserId < b.UserId
		}
		if a.LineId != b.LineId {
			return a.LineId < b.LineId
		}
		return a.NodeId < b.NodeId
	})
	return outLinks, outScopes, nil
}

// isNode 本机是否以副机角色运行(设置 nodeMode)。
func isNode(db *gorm.DB) bool {
	var v string
	db.Raw("SELECT value FROM settings WHERE key = ?", "nodeMode").Scan(&v)
	return strings.EqualFold(strings.TrimSpace(v), "true")
}
