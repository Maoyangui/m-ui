package hub

import (
	"encoding/json"
	"errors"
	"fmt"
	"runtime/debug"
	"strconv"
	"strings"
	"time"

	"github.com/Maoyangui/m-ui/database/model"
	"github.com/Maoyangui/m-ui/logger"

	"gorm.io/gorm"
	"gorm.io/gorm/clause"
)

// Decommission 停用 / 删除副机时调用:给它推一份空用户表(线路照旧、一个用户都没有),那台立刻停止为任何人服务。
// 以前停用 / 删除只是不再同步,那台的数据面一直按最后一份用户表放行所有人,之后的停用、到期、超额、重置在它上面
// 都不生效,流量也不记账(审计 M042 / M061)。
//
// before 是改库之前的快照(BuildSnapshot):删掉的副机在改库之后的快照里已经没有自己那一行、独占的线路也停了,
// 拿那份去推,它认不出本机、会把所有线路都当成自己的去渲染。调用时库里已经把它停用 / 删掉:在途的定时推送拿到闸后
// 看它不再启用,不会把空表盖回去;重新启用后修订号不同,定时同步推回完整快照。
//
// 下线推送先落库(NodeDecommission)再推:这一次没推到(失联、闸被占着、主机重启)不再就此作罢 —— 以前只试一次,
// 失败了那台副机就一直按旧用户表为所有人服务(审计 I4)。定时同步每轮接着重试,直到它确认。
func (h *Hub) Decommission(n model.Node, before Snapshot) error {
	if n.IsLocal || h.d.IsNode() {
		return nil
	}
	if n.ApiUrl == "" || n.Token == "" {
		return errors.New("这台副机没有配置 API 地址或令牌,发不出下线通知")
	}
	rec, err := h.queueDecommission(n, before)
	if err != nil {
		return fmt.Errorf("记录下线通知: %w", err)
	}
	if err := h.waitNode(n.Id, nodeWait); err != nil {
		h.noteDecommissionFailure(rec, err)
		return err
	}
	defer h.releaseNode(n.Id)
	return h.deliverDecommission(rec)
}

// queueDecommission 把要推给这台的空用户表连同它的地址、令牌落库。同一台已有一份没送到的就换成这份。
func (h *Hub) queueDecommission(n model.Node, before Snapshot) (model.NodeDecommission, error) {
	seq, err := h.currentSequence()
	if err != nil {
		return model.NodeDecommission{}, err
	}
	snap := withoutUsers(before)
	// 外部节点只给用户用,下线推送里没有用户,不带它们:否则用了按指纹隐藏 / 改名时版本门槛会抬到 0.7.17,
	// 老副机就会拒收这份下线通知、继续为所有人服务
	snap.Exts = nil
	// logo 也只给用户的落地页用:不带(nil,副机按"一张都没有"清掉本机的;它已经不为任何人出落地页,
	// 重新启用时推回的完整快照会带回来),免得把图片字节落进这条记录、每轮重试都重推一遍
	snap.Logos = nil
	snap.SelfNodeId, snap.Sequence, snap.Version, snap.MinNode = n.Id, seq, h.d.Version, minNodeFor(snap.Lines, snap.Upstreams, nil)
	b, err := json.Marshal(snap)
	if err != nil {
		return model.NodeDecommission{}, err
	}
	rec := model.NodeDecommission{NodeId: n.Id, Name: n.Name, ApiUrl: n.ApiUrl, Token: n.Token, Insecure: n.Insecure, CertFP: n.CertFP,
		Revision: snap.Revision, Snapshot: b, CreatedAt: time.Now().Unix()}
	err = h.d.DB.Clauses(clause.OnConflict{Columns: []clause.Column{{Name: "node_id"}}, UpdateAll: true}).Create(&rec).Error
	return rec, err
}

// currentSequence 此刻最新的推送序号:序号先落库再推,所以它不低于任何一台副机已经收过的,下线推送不会被当成过期快照拒掉。
func (h *Hub) currentSequence() (uint64, error) {
	var raw string
	if err := h.d.DB.Model(&model.Setting{}).Select("value").Where("key = ?", "hubPushSequence").Scan(&raw).Error; err != nil {
		return 0, err
	}
	seq, _ := strconv.ParseUint(raw, 10, 64)
	h.mu.Lock()
	if h.pushSeq > seq {
		seq = h.pushSeq
	}
	h.mu.Unlock()
	if seq == 0 { // 还没推过任何快照
		seq = uint64(time.Now().UnixNano())
	}
	return seq, nil
}

// deliverDecommission 推一份落库的下线通知(调用方持有这台的闸):确认了就删掉记录,没确认记下原因等下一轮。
func (h *Hub) deliverDecommission(rec model.NodeDecommission) error {
	n := model.Node{Id: rec.NodeId, Name: rec.Name, ApiUrl: rec.ApiUrl, Token: rec.Token, Insecure: rec.Insecure, CertFP: rec.CertFP}
	var cur model.Node
	if h.d.DB.Select("id, cert_fp").Where("id = ?", rec.NodeId).Limit(1).Find(&cur).Error == nil && cur.Id != 0 && cur.CertFP != "" {
		n.CertFP = cur.CertFP // 停用的副机还在表里:证书指纹以表里记住的为准
	}
	ack, err := h.pushWire(n, fixedWire(rec.Revision, rec.Snapshot))
	if err == nil {
		h.forgetDecommission(rec)
		logger.Info("副机 ", rec.Name, " 已确认收到下线通知(空用户表),不再为任何人服务")
		if !ack.LedgerKept {
			// 老副机应用空用户表时把账本整本清掉了:重新启用后同一个人从 0 重开,第一份按重开计,不当账本回退。
			// 打标记之前拉一份报告核对它真清了(见 oldAgentCleared):没清成的照常按增量,不把整本旧账再计一遍。
			// 停用期间不拉报告,拉到的这份只用来核对;标记改了这台的游标,下一次推送重新看要不要打
			h.oldAgentCleared(n, nil, nil)
			h.cursorsChanged(rec.NodeId)
		}
		return nil
	}
	var hse *httpStatusError
	if errors.As(err, &hse) && strings.Contains(hse.Msg, "拒绝过期快照") {
		// 它已经收过更新的配置:这台机器又以别的身份加回来了,在正常同步,不能再给它推空表
		h.forgetDecommission(rec)
		logger.Info("副机 ", rec.Name, " 已经在用更新的配置(重新加回来了),不再重推下线通知")
		return nil
	}
	h.noteDecommissionFailure(rec, err)
	logger.Warning("副机 ", rec.Name, " 还没收到下线通知(会一直重试): ", err)
	return err
}

func (h *Hub) forgetDecommission(rec model.NodeDecommission) {
	if err := h.d.DB.Where("node_id = ? AND revision = ? AND created_at = ?", rec.NodeId, rec.Revision, rec.CreatedAt).
		Delete(&model.NodeDecommission{}).Error; err != nil {
		logger.Warning("清除副机 ", rec.Name, " 的下线通知记录失败: ", err)
	}
}

func (h *Hub) noteDecommissionFailure(rec model.NodeDecommission, cause error) {
	msg := cause.Error()
	if r := []rune(msg); len(r) > 300 {
		msg = string(r[:300]) + "…"
	}
	h.d.DB.Model(&model.NodeDecommission{}).Where("node_id = ? AND revision = ? AND created_at = ?", rec.NodeId, rec.Revision, rec.CreatedAt).
		Updates(map[string]interface{}{"attempts": gorm.Expr("attempts + 1"), "last_try": time.Now().Unix(), "last_error": msg})
}

// retryDecommissions 定时同步每轮把还没送到的下线通知再推一遍(按失败次数退避,最长 5 分钟一次)。
// 已经不需要的直接作废:那台又启用了(定时同步推回完整快照),或者同一个地址又加成了另一台启用的副机。
func (h *Hub) retryDecommissions() {
	var recs []model.NodeDecommission
	if err := h.d.DB.Select("node_id, name, api_url, revision, created_at, attempts, last_try").Find(&recs).Error; err != nil {
		logger.Warning("读取待发的下线通知失败: ", err)
		return
	}
	if len(recs) == 0 {
		return
	}
	live, err := h.liveNodeAddrs()
	if err != nil {
		logger.Warning("读取副机列表失败: ", err)
		return
	}
	now := time.Now()
	for _, rec := range recs {
		if decommissionObsolete(rec, live) { // 不用拿闸:启用着的副机每轮都被定时同步占着闸
			h.forgetDecommission(rec)
			logger.Info("副机 ", rec.Name, " 已经重新启用(或同一地址又加了回来),作废之前没送到的下线通知")
			continue
		}
		if rec.Attempts > 0 && now.Sub(time.Unix(rec.LastTry, 0)) < pushBackoff(rec.Attempts) {
			continue
		}
		if !h.tryNode(rec.NodeId) {
			continue // 这台还有请求在途(比如面板上那次下线推送正在发)
		}
		h.rounds.Add(1)
		go func(rec model.NodeDecommission) {
			defer h.rounds.Done()
			defer h.releaseNode(rec.NodeId)
			defer func() {
				if v := recover(); v != nil {
					logger.Warning("重推副机 ", rec.Name, " 的下线通知异常: ", v, " | ", string(debug.Stack()))
				}
			}()
			if live, err := h.liveNodeAddrs(); err != nil || decommissionObsolete(rec, live) { // 拿到闸之后再看一眼
				return
			}
			var full model.NodeDecommission
			if err := h.d.DB.Where("node_id = ? AND revision = ? AND created_at = ?", rec.NodeId, rec.Revision, rec.CreatedAt).
				Limit(1).Find(&full).Error; err != nil || full.NodeId == 0 {
				return // 拿到闸之前已经送到了(面板上那次)
			}
			_ = h.deliverDecommission(full)
		}(rec)
	}
}

// liveNodeAddrs 启用着的副机(只取 id 与 API 地址)。
func (h *Hub) liveNodeAddrs() ([]model.Node, error) {
	var nodes []model.Node
	err := h.d.DB.Select("id, api_url").Where("enabled = ? AND is_local = ?", true, false).Find(&nodes).Error
	return nodes, err
}

// decommissionObsolete 这份下线通知是不是已经用不着了:那台又启用了,或者同一个地址又加成了另一台启用的副机。
func decommissionObsolete(rec model.NodeDecommission, live []model.Node) bool {
	addr := strings.TrimRight(strings.TrimSpace(rec.ApiUrl), "/")
	for _, n := range live {
		if n.Id == rec.NodeId || (addr != "" && strings.EqualFold(strings.TrimRight(strings.TrimSpace(n.ApiUrl), "/"), addr)) {
			return true
		}
	}
	return false
}
