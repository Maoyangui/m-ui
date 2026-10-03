// Package hub 实现主机(Hub)与副机(Agent)之间的同步:
//
//   - 主机每 5 秒计算配置快照(线路/上游/用户/用户线路/入口/少量设置)的修订号,
//     变化时推送给每台副机;副机整表替换并热重载
//   - 主机每 5 秒拉取副机报告:单调流量账本(按游标回收增量并入用户与时序)、在线 IP、运行状态
//   - 设备数跨机:主机把"其他机器上在线的 IP"下发给每台机器,本机限制时一并计数
//   - 副机失联/恢复告警
package hub

import (
	"bytes"
	"context"
	"crypto/rand"
	"crypto/sha256"
	"crypto/tls"
	"crypto/x509"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net/http"
	"net/url"
	"runtime/debug"
	"sort"
	"strconv"
	"strings"
	"sync"
	"time"

	"github.com/Maoyangui/m-ui/database/model"
	"github.com/Maoyangui/m-ui/logger"
	"github.com/Maoyangui/m-ui/notify"
	"github.com/Maoyangui/m-ui/render"

	"gorm.io/gorm"
	"gorm.io/gorm/clause"
)

// SyncedSettings 是需要在主副机之间保持一致的设置项(订阅展示相关)。
var SyncedSettings = []string{
	"timezone", "allowPrivate",
	"upstreamTestUrl", "upstreamCheckMinutes", "upstreamCheckFailThreshold",
	"subProfileTitle", "subEncode", "subShowNotice", "subClashExt", "subUpdates",
	"subPageEnabled", "subPageTitle", "subPageSupport", "subPageNotice", "subShareEnabled", "subPageBuyURL", "subPageBrand",
}

// MinNodeVersion 副机至少要这个版本才能正确应用当前快照(快照里新增了它必须理解的字段时抬高它)。
// 0.5.0:用户的停用原因、代理的额度用尽标志、私网屏蔽开关。0.6.0:规则限速状态表(副机要叠加到限速上)。
const MinNodeVersion = "0.6.0"

// Snapshot 是主机下发给副机的完整配置。
type Snapshot struct {
	Revision      string               `json:"revision"`
	Sequence      uint64               `json:"sequence,omitempty"` // 推送序号，拒绝延迟到达的旧快照；不参与内容修订号
	Version       string               `json:"version,omitempty"`  // 主机版本(只做展示与比对,不进修订号)
	MinNode       string               `json:"minNode,omitempty"`  // 副机最低版本,低于它拒绝应用并报错
	SelfNodeId    uint                 `json:"selfNodeId"`         // 接收方在 nodes 表里的 id
	MasterId      uint                 `json:"masterId"`           // 主机自己在 nodes 表里的 id
	Nodes         []model.Node         `json:"nodes"`
	Upstreams     []model.Upstream     `json:"upstreams"`
	Lines         []model.Line         `json:"lines"`
	Users         []model.User         `json:"users"`
	UserLines     []model.UserLine     `json:"userLines"`
	UserLineNodes []model.UserLineNode `json:"userLineNodes,omitempty"` // 用户在某线路上收窄到的服务器;没有 = 全部
	Exts          []model.ExtNode      `json:"exts"`
	UserExts      []model.UserExt      `json:"userExts"`
	Resellers     []model.Reseller     `json:"resellers"`   // 副机据此给代理用户出对应的订阅页文案(不含密码/2FA)
	LimitStates   []model.LimitState   `json:"limitStates"` // 主机判定的规则限速,副机照单叠加到用户限速上
	Settings      map[string]string    `json:"settings"`
	// 代理的线路授权:副机渲染不用它(UserLines 已取过交集),只为副机提升为主机后授权还在。
	// 旧版主机不发这两项(解码为 nil),副机就不动本机的授权表。
	ResellerLines     []model.ResellerLine     `json:"resellerLines"`
	ResellerLineNodes []model.ResellerLineNode `json:"resellerLineNodes"`
}

// BuildSnapshot 从主机数据库构造快照并计算修订号(只含会影响副机行为的字段)。
func BuildSnapshot(db *gorm.DB, setting func(string) string) (Snapshot, error) {
	var s Snapshot
	if err := db.Order("sort asc, id asc").Find(&s.Nodes).Error; err != nil {
		return s, err
	}
	for i := range s.Nodes {
		s.Nodes[i].Token = ""
		if s.Nodes[i].IsLocal {
			s.MasterId = s.Nodes[i].Id
		}
	}
	if err := db.Order("sort asc, id asc").Find(&s.Upstreams).Error; err != nil {
		return s, err
	}
	if err := db.Order("sort asc, id asc").Find(&s.Lines).Error; err != nil {
		return s, err
	}
	if err := db.Order("id asc").Find(&s.Users).Error; err != nil {
		return s, err
	}
	for i := range s.Users {
		// 副机不需要也不该拥有主机的计量;凭据/配额/限速/启用状态才是它要的
		s.Users[i].Up, s.Users[i].Down, s.Users[i].TotalUp, s.Users[i].TotalDown, s.Users[i].OnlineAt = 0, 0, 0, 0, 0
	}
	if err := db.Order("id asc").Find(&s.Resellers).Error; err != nil {
		return s, err
	}
	for i := range s.Resellers {
		s.Resellers[i].Password, s.Resellers[i].TotpSecret, s.Resellers[i].ApiToken = "", "", "" // 副机不跑代理面板,不需要这些
	}
	// 下发的是与代理授权取过交集的分配:副机直接照此渲染,不用(也不能)自己再算 —— 旧版主机不下发授权表
	links, scopes, err := render.EffectiveLines(db, 0)
	if err != nil {
		return s, err
	}
	s.UserLines, s.UserLineNodes = links, scopes
	s.ResellerLines, s.ResellerLineNodes = []model.ResellerLine{}, []model.ResellerLineNode{}
	if err := db.Order("reseller_id asc, line_id asc").Find(&s.ResellerLines).Error; err != nil {
		return s, err
	}
	if err := db.Order("reseller_id asc, line_id asc, node_id asc").Find(&s.ResellerLineNodes).Error; err != nil {
		return s, err
	}
	if err := db.Order("sort asc, id asc").Find(&s.Exts).Error; err != nil {
		return s, err
	}
	if err := db.Order("user_id asc, ext_id asc").Find(&s.UserExts).Error; err != nil {
		return s, err
	}
	if err := db.Order("id asc").Find(&s.LimitStates).Error; err != nil {
		return s, err
	}
	s.Settings = map[string]string{}
	for _, k := range SyncedSettings {
		s.Settings[k] = setting(k)
	}
	s.Revision = revisionOf(s)
	return s, nil
}

// revisionOf 对快照内容做哈希;字段顺序固定,故稳定。
func revisionOf(s Snapshot) string {
	s.Revision, s.SelfNodeId, s.Version, s.MinNode = "", 0, "", ""
	s.Sequence = 0
	// nodes 表中的 IsLocal 因接收方不同而不同,不参与修订号
	nodes := make([]model.Node, len(s.Nodes))
	copy(nodes, s.Nodes)
	for i := range nodes {
		nodes[i].IsLocal = false
	}
	s.Nodes = nodes
	// 外部订阅的抓取时间 / 报错 / 节点数只是主机自己的记录:每半小时抓一次就推一次全量快照,
	// 副机热更新一轮、限速桶清一遍,毫无意义。只有抓到的内容(Cache)变了才算配置变了
	exts := make([]model.ExtNode, len(s.Exts))
	copy(exts, s.Exts)
	for i := range exts {
		exts[i].LastFetch, exts[i].LastError, exts[i].NodeCount = 0, "", 0
	}
	s.Exts = exts
	b, _ := json.Marshal(s)
	sum := sha256.Sum256(b)
	return hex.EncodeToString(sum[:8])
}

// sameLines / sameUpstreams 按 id 排好序、把空的 JSON 字段归一(nil 与 "" 与 "null" 一律视为空)后逐条比较,
// 顺序与存储形态的差异不算变化。
func sameLines(a, b []model.Line) bool {
	if len(a) != len(b) {
		return false
	}
	norm := func(in []model.Line) []model.Line {
		out := make([]model.Line, len(in))
		copy(out, in)
		sort.Slice(out, func(i, j int) bool { return out[i].Id < out[j].Id })
		for i := range out {
			out[i].Sort = 0 // 排序只影响面板与订阅里的展示顺序,不影响数据面
			out[i].Options = normJSON(out[i].Options)
			out[i].Addrs = normJSON(out[i].Addrs)
			out[i].NodeIds = normJSON(out[i].NodeIds)
			out[i].Tls = normJSON(out[i].Tls)
			out[i].Transport = normJSON(out[i].Transport)
		}
		return out
	}
	x, _ := json.Marshal(norm(a))
	y, _ := json.Marshal(norm(b))
	return bytes.Equal(x, y)
}

func sameUpstreams(a, b []model.Upstream) bool {
	if len(a) != len(b) {
		return false
	}
	norm := func(in []model.Upstream) []model.Upstream {
		out := make([]model.Upstream, len(in))
		copy(out, in)
		sort.Slice(out, func(i, j int) bool { return out[i].Id < out[j].Id })
		for i := range out {
			out[i].Sort = 0
			out[i].Options = normJSON(out[i].Options)
		}
		return out
	}
	x, _ := json.Marshal(norm(a))
	y, _ := json.Marshal(norm(b))
	return bytes.Equal(x, y)
}

// normJSON 把"没有值"的各种写法统一成 nil,有值的重新紧凑序列化(去掉空白差异)。
func normJSON(raw json.RawMessage) json.RawMessage {
	s := strings.TrimSpace(string(raw))
	if s == "" || s == "null" {
		return nil
	}
	var v interface{}
	if json.Unmarshal([]byte(s), &v) != nil {
		return raw
	}
	b, err := json.Marshal(v)
	if err != nil {
		return raw
	}
	return b
}

// RevokedShares 返回本次快照里临时共享被取消或换新的用户名(要在 ApplySnapshot 之前调用)。
// 副机据此在热更新后断开这些用户的连接,否则借用者已经建立的连接还能接着用。
func RevokedShares(db *gorm.DB, snap Snapshot) []string {
	out, _ := RevokedSharesChecked(db, snap)
	return out
}

// RevokedSharesChecked is the error-reporting form used by the apply path.
// A database read failure must not be treated as "nothing to revoke" because
// that would leave an old shared credential connected on the node.
func RevokedSharesChecked(db *gorm.DB, snap Snapshot) ([]string, error) {
	var old []model.User
	if err := db.Where("share_token <> ''").Find(&old).Error; err != nil {
		return nil, err
	}
	if len(old) == 0 {
		return nil, nil
	}
	now := make(map[string]string, len(snap.Users))
	for _, u := range snap.Users {
		now[u.Name] = u.ShareToken
	}
	var out []string
	for _, u := range old {
		if now[u.Name] != u.ShareToken {
			out = append(out, u.Name)
		}
	}
	return out, nil
}

// RotatedUsers 返回本次快照里重置过订阅链接的用户名(要在 ApplySnapshot 之前调用):
// 订阅令牌换了、凭据也换了才算(只比凭据会把本机补全协议键之类的差异误当成换新)。
// 副机热更新后还要把这些用户旧凭据上的连接断掉,否则旧设备能一直连到自己断开为止。
func RotatedUsers(db *gorm.DB, snap Snapshot) []string {
	out, _ := RotatedUsersChecked(db, snap)
	return out
}

// RotatedUsersChecked is the error-reporting form used by the apply path.
func RotatedUsersChecked(db *gorm.DB, snap Snapshot) ([]string, error) {
	var old []model.User
	if err := db.Select("name, sub_token, credentials").Find(&old).Error; err != nil {
		return nil, err
	}
	if len(old) == 0 {
		return nil, nil
	}
	now := make(map[string]model.User, len(snap.Users))
	for _, u := range snap.Users {
		now[u.Name] = u
	}
	var out []string
	for _, u := range old {
		cur, ok := now[u.Name]
		if !ok || cur.SubToken == u.SubToken || len(cur.Credentials) == 0 {
			continue // 被删的用户热更新会自然断开;令牌没换就不是重置
		}
		if !bytes.Equal(normJSON(u.Credentials), normJSON(cur.Credentials)) {
			out = append(out, u.Name)
		}
	}
	return out, nil
}

// ApplySnapshot 在副机上整表替换配置。返回线路、上游是否变化,副机据此选择重载级别:
// 线路变 → 全量重载;仅上游变 → 热换出站;都没变 → 热换用户。
func ApplySnapshot(db *gorm.DB, snap Snapshot) (linesChanged, upstreamsChanged bool, err error) {
	// 比"变没变"必须两边同序、同形:快照按 sort 排、库里按 id 排,直接比会把每次推送都当成线路变了,
	// 副机于是次次全量重启数据面,所有人掉线几秒——只是有人生成了一条临时共享。
	var oldLines []model.Line
	var oldUps []model.Upstream
	if err := db.Find(&oldLines).Error; err != nil {
		return false, false, err
	}
	if err := db.Find(&oldUps).Error; err != nil {
		return false, false, err
	}
	linesChanged = !sameLines(oldLines, snap.Lines)
	upstreamsChanged = !sameUpstreams(oldUps, snap.Upstreams)
	// 渲染要读的同步设置变了也得全量重载:私网屏蔽是路由规则,热换用户不会重建它。以前主机改了「私网访问」,
	// 副机只热更新用户表,屏蔽规则在副机上一直按旧开关(审计 M058)
	if v, ok := snap.Settings["allowPrivate"]; ok {
		var old string
		if err := db.Model(&model.Setting{}).Select("value").Where("key = ?", "allowPrivate").Scan(&old).Error; err != nil {
			return false, false, err
		}
		if strings.EqualFold(v, "true") != strings.EqualFold(old, "true") { // 与 render.AllowPrivate 同一判法
			linesChanged = true
		}
	}

	// gorm 对带 default:true 的 bool 字段:Create 时零值 false 会写成默认 true,并把 true 回填进结构体。
	// 所以要在插入之前记下被禁用的 id,插入后再显式写回 false,否则主机禁用的用户会在副机上"复活"。
	type rsFlags struct{ enabled, page, share bool }
	rsSwitches := map[uint]rsFlags{}
	for _, rs := range snap.Resellers {
		rsSwitches[rs.Id] = rsFlags{rs.Enabled, rs.PageEnabled, rs.ShareOn}
	}
	var offUsers, offLines, offNodes, offExts []uint
	for _, u := range snap.Users {
		if !u.Enabled {
			offUsers = append(offUsers, u.Id)
		}
	}
	for _, e := range snap.Exts {
		if !e.Enabled {
			offExts = append(offExts, e.Id)
		}
	}
	for _, l := range snap.Lines {
		if !l.Enabled {
			offLines = append(offLines, l.Id)
		}
	}
	for _, n := range snap.Nodes {
		if !n.Enabled {
			offNodes = append(offNodes, n.Id)
		}
	}
	err = db.Transaction(func(tx *gorm.DB) error {
		var previous string
		if err := tx.Model(&model.Setting{}).Select("value").Where("key = ?", "hubSnapshotSequence").Scan(&previous).Error; err != nil {
			return err
		}
		seq, err := strconv.ParseUint(previous, 10, 64)
		if previous != "" && err != nil {
			return fmt.Errorf("读取快照序号: %w", err)
		}
		// Older masters do not send a sequence field.  Keep that wire format
		// usable during a rolling upgrade; only snapshots carrying a sequence
		// participate in stale-snapshot rejection.  Do not overwrite a newer
		// node's persisted sequence with the legacy zero value.
		legacySequence := snap.Sequence == 0
		if !legacySequence && snap.Sequence < seq {
			return fmt.Errorf("拒绝过期快照:序号 %d 低于已接收的 %d", snap.Sequence, seq)
		}
		for _, t := range []interface{}{&model.UserLine{}, &model.UserLineNode{}, &model.UserExt{}, &model.User{}, &model.Line{}, &model.Upstream{}, &model.Node{}, &model.ExtNode{}, &model.Reseller{}, &model.LimitState{}} {
			if err := tx.Session(&gorm.Session{AllowGlobalUpdate: true}).Delete(t).Error; err != nil {
				return err
			}
		}
		if len(snap.Exts) > 0 {
			if err := tx.CreateInBatches(&snap.Exts, snapshotBatch).Error; err != nil {
				return err
			}
		}
		if len(snap.UserExts) > 0 {
			if err := tx.CreateInBatches(&snap.UserExts, snapshotBatch).Error; err != nil {
				return err
			}
		}
		for i := range snap.Nodes {
			snap.Nodes[i].IsLocal = snap.Nodes[i].Id == snap.SelfNodeId
		}
		if len(snap.Nodes) > 0 {
			if err := tx.CreateInBatches(&snap.Nodes, snapshotBatch).Error; err != nil {
				return err
			}
		}
		if len(snap.Upstreams) > 0 {
			if err := tx.CreateInBatches(&snap.Upstreams, snapshotBatch).Error; err != nil {
				return err
			}
		}
		if len(snap.Lines) > 0 {
			if err := tx.CreateInBatches(&snap.Lines, snapshotBatch).Error; err != nil {
				return err
			}
		}
		if len(snap.Users) > 0 {
			if err := tx.CreateInBatches(&snap.Users, snapshotBatch).Error; err != nil {
				return err
			}
		}
		if len(snap.UserLines) > 0 {
			if err := tx.CreateInBatches(&snap.UserLines, snapshotBatch).Error; err != nil {
				return err
			}
		}
		if len(snap.UserLineNodes) > 0 {
			if err := tx.CreateInBatches(&snap.UserLineNodes, snapshotBatch).Error; err != nil {
				return err
			}
		}
		if len(snap.Resellers) > 0 {
			if err := tx.CreateInBatches(&snap.Resellers, snapshotBatch).Error; err != nil {
				return err
			}
			// gorm 的 default:true 会把 false 写成 true,插入后按插入前记下的值写回
			for id, f := range rsSwitches {
				if err := tx.Model(&model.Reseller{}).Where("id = ?", id).Updates(map[string]interface{}{
					"enabled": f.enabled, "page_enabled": f.page, "share_on": f.share,
				}).Error; err != nil {
					return err
				}
			}
		}
		if snap.ResellerLines != nil { // 旧版主机不发授权表:保留本机的,不当成"全部收回"
			for _, t := range []interface{}{&model.ResellerLine{}, &model.ResellerLineNode{}} {
				if err := tx.Session(&gorm.Session{AllowGlobalUpdate: true}).Delete(t).Error; err != nil {
					return err
				}
			}
			if len(snap.ResellerLines) > 0 {
				if err := tx.CreateInBatches(&snap.ResellerLines, snapshotBatch).Error; err != nil {
					return err
				}
			}
			if len(snap.ResellerLineNodes) > 0 {
				if err := tx.CreateInBatches(&snap.ResellerLineNodes, snapshotBatch).Error; err != nil {
					return err
				}
			}
		}
		if len(snap.LimitStates) > 0 { // 主机判定的规则限速,副机照单执行
			if err := tx.CreateInBatches(&snap.LimitStates, snapshotBatch).Error; err != nil {
				return err
			}
		}
		if err := disableIDs(tx, &model.User{}, offUsers); err != nil {
			return err
		}
		if err := disableIDs(tx, &model.Line{}, offLines); err != nil {
			return err
		}
		if err := disableIDs(tx, &model.Node{}, offNodes); err != nil {
			return err
		}
		if err := disableIDs(tx, &model.ExtNode{}, offExts); err != nil {
			return err
		}
		for k, v := range snap.Settings {
			if err := upsertSetting(tx, k, v); err != nil {
				return err
			}
		}
		settingsToWrite := map[string]string{
			"hubRevision":      snap.Revision,
			"hubMasterId":      fmt.Sprintf("%d", snap.MasterId),
			"hubAppliedAt":     fmt.Sprintf("%d", time.Now().Unix()),
			"hubReloadPending": snap.Revision,
		}
		if !legacySequence {
			settingsToWrite["hubSnapshotSequence"] = strconv.FormatUint(snap.Sequence, 10)
		}
		for k, v := range settingsToWrite {
			if err := upsertSetting(tx, k, v); err != nil {
				return err
			}
		}
		return nil
	})
	return linesChanged, upstreamsChanged, err
}

// snapshotBatch 快照整表插入每批的行数。SQLite 一条语句最多 32766 个变量:用户表一行约 25 列,
// 一条 INSERT 插一千三百来个用户(或一万六千多条"用户 × 线路")就超限,整份快照应用失败,
// 所有副机从此一份配置都收不到。500 行 × 最宽的用户表也只有一万出头个变量。
const snapshotBatch = 500

// disableIDs 把 ids 这些行的 enabled 写回 false,按批进行(理由同 snapshotBatch)。
func disableIDs(tx *gorm.DB, table interface{}, ids []uint) error {
	for len(ids) > 0 {
		n := min(len(ids), snapshotBatch)
		if err := tx.Model(table).Where("id IN ?", ids[:n]).Update("enabled", false).Error; err != nil {
			return err
		}
		ids = ids[n:]
	}
	return nil
}

func upsertSetting(tx *gorm.DB, k, v string) error {
	return tx.Clauses(clause.OnConflict{
		Columns:   []clause.Column{{Name: "key"}},
		DoUpdates: clause.AssignmentColumns([]string{"value"}),
	}).Create(&model.Setting{Key: k, Value: v}).Error
}

// RecentConn 数据面日志里聚合出的一条"源 IP × 线路"入站记录(诊断用)。
type RecentConn struct {
	IP       string `json:"ip"`
	User     string `json:"user,omitempty"` // 认证成功的用户名(日志里带 [用户] 的那条)
	Line     string `json:"line"`
	Protocol string `json:"protocol"`
	Count    int    `json:"count"`
	Ts       int64  `json:"ts"`               // 最近一次的绝对时间(unix 秒),由各机按本机时区解析
	Server   string `json:"server,omitempty"` // 主机汇总时标注来自哪台服务器
}

// Report 是副机上报的状态。
type Report struct {
	Version         string                         `json:"version"`
	Hostname        string                         `json:"hostname"`
	CoreRunning     bool                           `json:"coreRunning"`
	Uptime          uint32                         `json:"uptime"`
	Revision        string                         `json:"revision"`                // 副机库里的修订号(ApplySnapshot 落库就写,不等数据面)
	ReloadPending   bool                           `json:"reloadPending,omitempty"` // 库里是新修订、数据面还没应用成功(起不来、回滚了)
	Counters        []model.AgentCounter           `json:"counters"`
	Onlines         map[string][]string            `json:"onlines"`                   // 用户 → 在线源 IP
	OnlineLinesByIP map[string]map[string][]string `json:"onlineLinesByIp,omitempty"` // 用户 → 源 IP → 线路名
	OnlineLines     []string                       `json:"onlineLines"`
	CertDays        int                            `json:"certDays"`
	PublicIP        string                         `json:"publicIp"`              // 副机探测到的公网 IP,主机存入 nodes.public_ip 供订阅使用
	Conns           []RecentConn                   `json:"conns,omitempty"`       // 最近入站连接,主机概览汇总展示
	Groups          map[string]GroupState          `json:"groups,omitempty"`      // 代理池在这台机器上的状态(在线设备、设备池满被拒次数)
	Upstreams       []UpstreamHealth               `json:"upstreams,omitempty"`   // 本机线路真正用到的那些上游的巡检结果
	Reload          *ReloadState                   `json:"reload,omitempty"`      // 副机最近一次重载的结果,失败要让主机面板看见
	LedgerEpoch     string                         `json:"ledgerEpoch,omitempty"` // 副机流量账本的纪元(见 LedgerEpochKey);老副机不报
}

// ReloadState 一台机器最近一次数据面重载的结果。
type ReloadState struct {
	At    int64  `json:"at"`
	Op    string `json:"op"`
	OK    bool   `json:"ok"`
	Error string `json:"error,omitempty"`
}

// UpstreamHealth 一台服务器上某条上游的最近巡检结果。
// 上游通不通要在真正跑这条线路的机器上量:主机在香港、落地从高带宽走,主机测得通不代表高带宽通。
type UpstreamHealth struct {
	Id        uint   `json:"id"`
	Name      string `json:"name"`
	OK        bool   `json:"ok"`
	DelayMs   int    `json:"delayMs"`
	Method    string `json:"method"`
	Error     string `json:"error,omitempty"`
	CheckedAt int64  `json:"checkedAt"`
	Fails     int    `json:"fails"` // 该机连续失败次数,主机据此决定告不告警
}

// GroupState 一个代理池在某台机器上的状态。
type GroupState struct {
	Devices int   `json:"devices"`
	Rejects int64 `json:"rejects"` // 自上次上报以来设备池满被拒的新设备连接数
}

// ApplyCounters 把副机的单调账本按游标并入主机:只计增量;计数器回绕(副机重装)时游标归零重认。
// ratio 为该服务器的流量倍率(≤0 视为 1),增量按倍率计入用户用量与时序。返回并入的用户数。
func ApplyCounters(db *gorm.DB, nodeId uint, nodeName, epoch string, counters []model.AgentCounter, now int64, bucketSeconds int64, ratio float64) (int, error) {
	if bucketSeconds < 1 {
		bucketSeconds = 60
	}
	if ratio <= 0 {
		ratio = 1
	}
	bucket := now - now%bucketSeconds
	n := 0
	err := db.Transaction(func(tx *gorm.DB) error {
		baseline, err := ledgerBaseline(tx, nodeId, epoch)
		if err != nil {
			return err
		}
		for _, c := range counters {
			var cur model.TrafficCursor
			tx.Where("node_id = ? AND user_name = ?", nodeId, c.UserName).First(&cur)
			if baseline {
				cur.Up, cur.Down = c.Up, c.Down // 只建基线:游标对齐当前计数,这一份不计增量
			} else if c.Up < cur.Up || c.Down < cur.Down {
				cur.Up, cur.Down = 0, 0 // 副机计数器回绕(同名用户删了再建,计数从 0 起)
			}
			dUp, dDown := c.Up-cur.Up, c.Down-cur.Down
			if baseline {
				if err := tx.Clauses(clause.OnConflict{
					Columns:   []clause.Column{{Name: "node_id"}, {Name: "user_name"}},
					DoUpdates: clause.AssignmentColumns([]string{"up", "down"}),
				}).Create(&model.TrafficCursor{NodeId: nodeId, UserName: c.UserName, Up: c.Up, Down: c.Down}).Error; err != nil {
					return err
				}
				continue
			}
			if dUp <= 0 && dDown <= 0 {
				continue
			}
			if ratio != 1 {
				dUp, dDown = int64(float64(dUp)*ratio), int64(float64(dDown)*ratio)
			}
			update := map[string]interface{}{"online_at": now}
			if dUp > 0 {
				update["up"] = gorm.Expr("up + ?", dUp)
			}
			if dDown > 0 {
				update["down"] = gorm.Expr("down + ?", dDown)
			}
			if err := tx.Model(&model.User{}).Where("name = ?", c.UserName).Updates(update).Error; err != nil {
				return err
			}
			rows := []model.Stats{}
			if dUp > 0 {
				rows = append(rows, model.Stats{DateTime: bucket, Resource: "user", Tag: c.UserName, Direction: true, Traffic: dUp})
			}
			if dDown > 0 {
				rows = append(rows, model.Stats{DateTime: bucket, Resource: "user", Tag: c.UserName, Direction: false, Traffic: dDown})
			}
			rows = append(rows, model.Stats{DateTime: bucket, Resource: "node", Tag: nodeName, Direction: true, Traffic: maxInt64(dUp, 0)})
			rows = append(rows, model.Stats{DateTime: bucket, Resource: "node", Tag: nodeName, Direction: false, Traffic: maxInt64(dDown, 0)})
			if err := tx.Clauses(clause.OnConflict{
				Columns:   []clause.Column{{Name: "resource"}, {Name: "tag"}, {Name: "date_time"}, {Name: "direction"}},
				DoUpdates: clause.Assignments(map[string]interface{}{"traffic": gorm.Expr("stats.traffic + excluded.traffic")}),
			}).Create(&rows).Error; err != nil {
				return err
			}
			if err := tx.Clauses(clause.OnConflict{
				Columns:   []clause.Column{{Name: "node_id"}, {Name: "user_name"}},
				DoUpdates: clause.AssignmentColumns([]string{"up", "down"}),
			}).Create(&model.TrafficCursor{NodeId: nodeId, UserName: c.UserName, Up: c.Up, Down: c.Down}).Error; err != nil {
				return err
			}
			n++
		}
		return nil
	})
	return n, err
}

// LedgerEpochKey 副机账本纪元在 settings 里的键(副机本机的,不随快照同步)。数据面启动时没有就生成,
// 从备份还原后换新:账本换了一本,主机据此只建基线、不把整段历史当增量再计一遍(审计 M057)。
const LedgerEpochKey = "agentLedgerEpoch"

// NewLedgerEpoch 一个新的随机账本纪元。
func NewLedgerEpoch() string {
	b := make([]byte, 8)
	_, _ = rand.Read(b)
	return hex.EncodeToString(b)
}

// LedgerEpochSetting 主机记下的某台副机的账本纪元(删除副机时一并删掉)。
func LedgerEpochSetting(nodeId uint) string { return fmt.Sprintf("ledgerEpoch:%d", nodeId) }

// DuplicateLedgerError 这份报告的账本纪元已经记在另一台副机上:同一台机器加了两次(或克隆出来的),流量只按先记下的那台算。
type DuplicateLedgerError struct{ Other uint }

func (e *DuplicateLedgerError) Error() string {
	return fmt.Sprintf("和副机 #%d 是同一本流量账本(同一台机器加了两次,或者是克隆出来的),这台的流量不重复计入", e.Other)
}

// ledgerBaseline 按账本纪元判断这一份报告是不是只建基线。以前副机账本没有身份:删了再加(主机游标随副机删掉)、
// 副机从备份还原(计数倒退,被当成回绕从 0 计)、同一台机器加两次,都会把整段历史流量重算一遍。
//   - 纪元变了:换了一本账,只建基线;
//   - 这台第一次报纪元:有游标说明是升级上来的老副机,照常计(不丢流量);没有游标(新加的、删了再加的)只建基线;
//   - 同一纪元记在另一台副机上:不计入(DuplicateLedgerError);
//   - 老副机不报纪元(epoch 为空):照旧。
func ledgerBaseline(tx *gorm.DB, nodeId uint, epoch string) (bool, error) {
	if epoch == "" {
		return false, nil
	}
	key := LedgerEpochSetting(nodeId)
	var prev string
	if err := tx.Model(&model.Setting{}).Select("value").Where("key = ?", key).Scan(&prev).Error; err != nil {
		return false, err
	}
	if prev == epoch {
		return false, nil
	}
	var other model.Setting
	if err := tx.Where("key LIKE ? AND key <> ? AND value = ?", "ledgerEpoch:%", key, epoch).Limit(1).Find(&other).Error; err != nil {
		return false, err
	}
	if other.Key != "" {
		id, _ := strconv.ParseUint(strings.TrimPrefix(other.Key, "ledgerEpoch:"), 10, 64)
		return false, &DuplicateLedgerError{Other: uint(id)}
	}
	baseline := prev != ""
	if !baseline {
		var cursors int64
		if err := tx.Model(&model.TrafficCursor{}).Where("node_id = ?", nodeId).Count(&cursors).Error; err != nil {
			return false, err
		}
		baseline = cursors == 0
	}
	if baseline {
		logger.Info("副机 #", nodeId, " 的流量账本是新的一本(纪元 ", epoch, "),这一份只建基线、不计增量")
	}
	return baseline, upsertSetting(tx, key, epoch)
}

func maxInt64(a, b int64) int64 {
	if a > b {
		return a
	}
	return b
}

// ---- 主机侧运行时 ----

type NodeStatus struct {
	Id          uint   `json:"id"`
	Name        string `json:"name"`
	OK          bool   `json:"ok"`
	Error       string `json:"error,omitempty"`
	LastSeen    int64  `json:"lastSeen"`
	LastPush    int64  `json:"lastPush"`
	Version     string `json:"version"`
	Hostname    string `json:"hostname"`
	CoreRunning bool   `json:"coreRunning"`
	Uptime      uint32 `json:"uptime"`
	Revision    string `json:"revision"`
	Synced      bool   `json:"synced"`
	OnlineUsers int    `json:"onlineUsers"`
	CertDays    int    `json:"certDays"`
	// ReloadError 副机最近一次重载失败的原因(空 = 正常);VersionMismatch 副机版本与主机不同
	ReloadError     string `json:"reloadError,omitempty"`
	ReloadAt        int64  `json:"reloadAt,omitempty"`
	VersionMismatch bool   `json:"versionMismatch,omitempty"`
	alerted         bool
	failSince       int64 // 这一轮连续失败从什么时候开始;0 = 正常
	unsyncedSince   int64 // 在线但配置没同步上从什么时候开始;0 = 已同步或不在线
	unsyncedAlerted bool
	conns           []RecentConn
}

// RemoteConns 汇总所有在线副机最近上报的入站连接,标注服务器名。
func (h *Hub) RemoteConns() []RecentConn {
	now := time.Now().Unix()
	h.mu.Lock()
	defer h.mu.Unlock()
	var out []RecentConn
	for id, st := range h.status {
		if !h.freshLocked(id, now) {
			continue
		}
		for _, c := range st.conns {
			c.Server = st.Name
			out = append(out, c)
		}
	}
	return out
}

type Deps struct {
	DB             *gorm.DB
	Setting        func(string) string
	IsNode         func() bool
	Version        string
	Notify         func(toggle, text string)
	LocalIPs       func(user string) []string // 主机本机在线 IP,用于合并下发
	SetExternalIPs func(map[string][]string)
	LocalGroups    func() map[string]GroupState // 主机本机的代理池状态(与副机上报的合并给面板看)
	CountersMerged func()                       // 并入了副机用量:触发一次超量 / 到期判定(可为 nil,不能阻塞)
}

type Hub struct {
	// syncMu 只串行"构造快照 + 分配推送序号",不跨网络往返:一台挂起的副机拖不住别的机器,也拖不住面板上的推送。
	syncMu sync.Mutex
	// sequencePrimed is process-local: after startup (including a restored
	// database) the first snapshot raises the durable sequence to the clock
	// floor once, while unchanged five-second polls reuse it without a SQLite
	// write. A content revision change still advances it monotonically.
	sequencePrimed bool
	d              Deps
	mu             sync.Mutex
	// latest 最近一次构造的快照。推送一律在那台机器的闸里取它来发,同一台副机上不会先到新快照、后到旧快照
	latest *Snapshot
	// gates 每台副机一个容量为 1 的信号量:推送 → ACK、拉报告都在里面。定时同步拿不到就跳过这台(上一轮对它的
	// 请求还没回来),手动推送、立即同步、下线推送最多等 nodeWait
	gates map[uint]chan struct{}
	// dbMu 各副机的结果串行落库(并入流量、改状态);网络部分各走各的
	dbMu sync.Mutex
	// ipsBusy 正在向这台副机下发外部设备租约:同一台机器同一时间最多一个在途请求
	ipsBusy map[uint]bool
	// rounds 定时同步发出去、还没回来的请求(各副机的同步、设备租约);Stop 等它们收尾
	rounds   sync.WaitGroup
	ctx      context.Context // Stop 时取消,在途请求立刻返回
	cancel   context.CancelFunc
	status   map[uint]*NodeStatus
	pushed   map[uint]string
	pushFail map[uint]*pushFailure        // 同一修订连续推送失败:次数与时间,用来退避
	remote   map[uint]map[string][]string // node → user → ips
	// remoteAt 各副机最近一次成功上报的时间。失联的副机报告本身留着(页面还要显示它最后的样子),
	// 但超过 remoteReportGrace 没上报,它的在线 IP 就不再计入设备数并集:那些设备是不是还在线已经无从得知,
	// 拿几分钟前的名单去拒新设备、断别的机器上回来的老连接,比短暂超限更伤人。
	remoteAt map[uint]int64
	// remoteLines 副机上报的 用户 → 源 IP → 线路名;nodeNames 用于在面板里给线路加服务器后缀
	remoteLines map[uint]map[string]map[string][]string
	nodeNames   map[uint]string
	revision    string
	stop        chan struct{}
	wg          sync.WaitGroup
	// verified 正常校验证书的副机共用一个;pinned 勾了"跳过证书校验"的副机按 id+指纹各一个,指纹一变就换新的
	verified *http.Client
	pinned   map[string]*http.Client
	roots    *x509.CertPool // 判断"正规 CA 续期"用的根证书;nil = 系统的(测试里注入)
	// rejects 各代理池最近被拒的新设备连接(主机 + 各副机上报),只留 rejectWindow 内的,面板给代理看"设备池已满"
	rejects map[string][]rejectAt
	// upHealth 各副机上报的上游巡检结果(副机 id → 结果),面板按服务器展示、主机据此告警
	upHealth map[uint][]UpstreamHealth
	// kicking 按用户名单飞的踢线派发:面板连点两下只向副机发一轮请求,后到的等同一份结果
	kickMu  sync.Mutex
	kicking map[string]*kickCall
}

type rejectAt struct {
	at int64
	n  int64
}

const rejectWindow = 10 * 60 // 秒

func New(d Deps) *Hub {
	ctx, cancel := context.WithCancel(context.Background())
	return &Hub{d: d, status: map[uint]*NodeStatus{}, pushed: map[uint]string{}, remote: map[uint]map[string][]string{}, remoteAt: map[uint]int64{},
		remoteLines: map[uint]map[string]map[string][]string{}, nodeNames: map[uint]string{}, stop: make(chan struct{}), rejects: map[string][]rejectAt{},
		upHealth: map[uint][]UpstreamHealth{}, kicking: map[string]*kickCall{}, gates: map[uint]chan struct{}{}, ipsBusy: map[uint]bool{},
		ctx: ctx, cancel: cancel,
		verified: &http.Client{Timeout: 25 * time.Second}, pinned: map[string]*http.Client{}, pushFail: map[uint]*pushFailure{}}
}

func (h *Hub) Start() {
	h.wg.Add(1)
	go func() {
		defer h.wg.Done()
		t := time.NewTicker(5 * time.Second)
		defer t.Stop()
		for {
			select {
			case <-t.C:
				func() { // 同步里出一次 panic 不该带走整个进程
					defer func() {
						if v := recover(); v != nil {
							logger.Warning("主副机同步异常: ", v, " | ", string(debug.Stack()))
						}
					}()
					h.tick()
				}()
			case <-h.stop:
				return
			}
		}
	}()
}

func (h *Hub) Stop() {
	close(h.stop)
	if h.cancel != nil {
		h.cancel()
	}
	h.wg.Wait()     // 定时循环先退出,之后不会再有新的 rounds
	h.rounds.Wait() // 在途请求随 ctx 取消立刻返回
}

// baseCtx 所有发往副机的请求都挂在它下面;测试里直接拼出来的 Hub 没有它。
func (h *Hub) baseCtx() context.Context {
	if h.ctx == nil {
		return context.Background()
	}
	return h.ctx
}

// nodeWait 手动推送、立即同步、下线推送等这台副机上一轮同步收尾的上限:和一次请求的超时一样长,
// 再久多半是它挂起了,如实报错比让面板转一分钟强。
const nodeWait = 25 * time.Second

// gate 这台副机的信号量(见 Hub.gates)。
func (h *Hub) gate(id uint) chan struct{} {
	h.mu.Lock()
	defer h.mu.Unlock()
	if h.gates == nil {
		h.gates = map[uint]chan struct{}{}
	}
	g := h.gates[id]
	if g == nil {
		g = make(chan struct{}, 1)
		h.gates[id] = g
	}
	return g
}

// tryNode 拿到这台副机的闸就返回 true;上一轮还在途就立刻返回 false。
func (h *Hub) tryNode(id uint) bool {
	select {
	case h.gate(id) <- struct{}{}:
		return true
	default:
		return false
	}
}

// waitNode 最多等 d 拿这台副机的闸。
func (h *Hub) waitNode(id uint, d time.Duration) error {
	t := time.NewTimer(d)
	defer t.Stop()
	select {
	case h.gate(id) <- struct{}{}:
		return nil
	case <-t.C:
		return errors.New("上一轮对这台副机的同步还没结束(它可能失联了),稍后再试")
	case <-h.baseCtx().Done():
		return h.baseCtx().Err()
	}
}

func (h *Hub) releaseNode(id uint) { <-h.gate(id) }

// stillEnabled 这台副机此刻是否仍是启用的:拿到它的闸之后再看一眼,中途被停用 / 删掉的就不再推送
// (停用时推的空用户表不能被一轮在途的定时推送盖回去)。
func (h *Hub) stillEnabled(id uint) bool {
	var n int64
	if err := h.d.DB.Model(&model.Node{}).Where("id = ? AND enabled = ?", id, true).Count(&n).Error; err != nil {
		return false
	}
	return n > 0
}

// refreshSnapshot 按当前库构造一份快照记为 latest(推送一律发它)。
func (h *Hub) refreshSnapshot() (Snapshot, error) {
	h.syncMu.Lock()
	defer h.syncMu.Unlock()
	snap, err := h.buildPushSnapshot()
	if err != nil {
		return snap, err
	}
	snap.Version, snap.MinNode = h.d.Version, MinNodeVersion
	h.mu.Lock()
	h.latest, h.revision = &snap, snap.Revision
	h.mu.Unlock()
	return snap, nil
}

// latestSnapshot 调用前必须至少 refreshSnapshot 过一次。
func (h *Hub) latestSnapshot() Snapshot {
	h.mu.Lock()
	defer h.mu.Unlock()
	return *h.latest
}

// Revision 返回主机当前配置修订号。
func (h *Hub) Revision() string {
	h.mu.Lock()
	defer h.mu.Unlock()
	return h.revision
}

func (h *Hub) remoteNodes() ([]model.Node, error) {
	var nodes []model.Node
	err := h.d.DB.Where("enabled = ? AND is_local = ?", true, false).Order("sort asc, id asc").Find(&nodes).Error
	return nodes, err
}

// nodeResult 一台副机这一轮同步的结果:网络部分各台各跑,落库部分经 dbMu 串行做。
type nodeResult struct {
	pushErr string // 这一轮推送没成功(退避中也算),但报告照样拉到了
	n       model.Node
	rep     Report
	err     string // 非空 = 这一轮失败(推送或拉报告)
}

// tick 一轮定时同步:按当前库构造快照,给每台副机各发一轮请求就返回,不等任何一台回来。
// 以前整轮等所有副机:一台黑洞 / 挂起的副机(推送、拉报告、下发设备租约各 25 秒超时)把所有健康副机的
// 同步周期从 5 秒拖到 75 秒左右 —— 停用、换凭据、超额停用在每台机器上都晚一分多钟生效。
// 现在上一轮对某台的请求还在途,这一轮就跳过它;结果谁先回来谁先落库。
func (h *Hub) tick() {
	if h.d.IsNode() {
		return
	}
	if _, err := h.refreshSnapshot(); err != nil {
		logger.Warning("构造同步快照失败: ", err)
		return
	}
	nodes, err := h.remoteNodes()
	if err != nil {
		logger.Warning("读取副机列表失败，保留已有缓存: ", err)
		return
	}
	live := map[uint]bool{}
	for _, n := range nodes {
		live[n.Id] = true
		if n.ApiUrl == "" || n.Token == "" {
			h.setStatus(n, false, "未配置 API 地址或令牌", nil)
			continue
		}
		if !h.tryNode(n.Id) {
			continue // 上一轮对它的请求还没回来
		}
		h.rounds.Add(1)
		go func(n model.Node) {
			defer h.rounds.Done()
			defer h.releaseNode(n.Id)
			defer func() {
				if v := recover(); v != nil {
					logger.Warning("同步副机 ", n.Name, " 异常: ", v, " | ", string(debug.Stack()))
				}
			}()
			// 同一台机器内部仍是 推送 → 拉报告 的顺序。拿到闸之后再确认它还启用着:停用 / 删除时推的空用户表
			// 不能被这一轮在途的定时推送盖回去
			if !h.stillEnabled(n.Id) {
				return
			}
			h.applyResult(h.syncNode(n, h.latestSnapshot()))
		}(n)
	}
	h.forgetNodes(live)
	h.distributeIPs(nodes)
	// 代理池被拒次数:主机本机这一轮的;各副机的在 applyResult 里随报告记
	if h.d.LocalGroups != nil {
		h.recordRejects(h.d.LocalGroups())
	}
}

// applyResult 一台副机这一轮的结果落库、更新状态。各副机的结果经 dbMu 串行,不给 SQLite 添堵。
func (h *Hub) applyResult(r *nodeResult) {
	h.dbMu.Lock()
	defer h.dbMu.Unlock()
	if r.err != "" {
		h.setStatus(r.n, false, r.err, nil)
		// Keep the last report and all line/node configuration intact during
		// an outage so the panel still shows the node's last known state. Its
		// device set stays in the cross-node union only for remoteReportGrace
		// (see remoteForDeviceLimits): a short blip keeps the limit intact, a
		// real outage stops a stale list from rejecting new devices or evicting
		// old connections on the other servers.
		return
	}
	bucket := int64(60)
	if v := h.d.Setting("statsBucketSeconds"); v != "" {
		fmt.Sscanf(v, "%d", &bucket)
	}
	errStr := r.pushErr // 在线但配置未同步:Error 里写推送原因,Synced 由报告的修订号判
	if merged, err := ApplyCounters(h.d.DB, r.n.Id, r.n.Name, r.rep.LedgerEpoch, r.rep.Counters, time.Now().Unix(), bucket, r.n.Ratio); err != nil {
		logger.Warning("并入副机 ", r.n.Name, " 流量失败: ", err)
		var dup *DuplicateLedgerError
		if errors.As(err, &dup) { // 要让管理员看见:这台的流量没在计
			errStr = strings.TrimPrefix(errStr+";"+err.Error(), ";")
		}
	} else if merged > 0 && h.d.CountersMerged != nil {
		h.d.CountersMerged()
	}
	h.setStatus(r.n, true, errStr, &r.rep)
	if r.rep.PublicIP != "" && r.rep.PublicIP != r.n.PublicIP {
		h.d.DB.Model(&model.Node{}).Where("id = ?", r.n.Id).Update("public_ip", r.rep.PublicIP)
	}
	h.mu.Lock()
	h.remote[r.n.Id] = r.rep.Onlines
	h.remoteAt[r.n.Id] = time.Now().Unix()
	h.remoteLines[r.n.Id] = r.rep.OnlineLinesByIP
	h.nodeNames[r.n.Id] = r.n.Name
	h.upHealth[r.n.Id] = r.rep.Upstreams // 这一轮没有结果就清空:副机改了线路、不再用任何上游时不该留着旧数据
	h.mu.Unlock()
	h.recordRejects(r.rep.Groups)
}

// recordRejects 把一台机器这一轮上报的设备池被拒次数记进滚动窗口。
func (h *Hub) recordRejects(groups map[string]GroupState) {
	now := time.Now().Unix()
	h.mu.Lock()
	defer h.mu.Unlock()
	for g, st := range groups {
		if st.Rejects > 0 {
			h.rejects[g] = append(h.rejects[g], rejectAt{at: now, n: st.Rejects})
		}
	}
	for g, list := range h.rejects {
		keep := list[:0]
		for _, x := range list {
			if now-x.at <= rejectWindow {
				keep = append(keep, x)
			}
		}
		if len(keep) == 0 {
			delete(h.rejects, g)
		} else {
			h.rejects[g] = keep
		}
	}
}

// Rejects 某代理池最近 10 分钟内因设备池满被拒的新设备连接数(所有机器合计)。
func (h *Hub) Rejects(group string) int64 {
	now := time.Now().Unix()
	h.mu.Lock()
	defer h.mu.Unlock()
	var n int64
	for _, x := range h.rejects[group] {
		if now-x.at <= rejectWindow {
			n += x.n
		}
	}
	return n
}

// syncNode 一台副机的网络部分:该推就推,再拉报告。不碰数据库,可以和别的机器并发。
// errApplyUnconfirmed 副机收到了、但没确认应用(ok 不是 1、或修订号对不上)。
var errApplyUnconfirmed = errors.New("副机未确认应用")

// pushRejected 这次推送失败是"副机明确拒绝 / 没应用成功"(该退避),还是"根本没连上"(不该退避)?
// 失联的副机恢复后必须在下一轮 tick 就拿到最新配置 —— 失联期间做的停用、换凭据都等着它。
// 把连接失败也算进退避,副机一回来还要再等最长 5 分钟,旧凭据在那台机上就多活 5 分钟。
func pushRejected(err error) bool {
	var hse *httpStatusError
	return errors.As(err, &hse) || errors.Is(err, errApplyUnconfirmed)
}

// pushFailure 某台副机对某个修订的连续推送失败记录(只记"拒绝应用",不记失联)。
type pushFailure struct {
	rev   string
	n     int       // 连续失败次数
	at    time.Time // 最近一次失败
	cause string
}

// pushBackoff 同一修订连续失败 n 次之后,下一次重推至少要隔多久:5s、10s、20s … 封顶 5 分钟。
// 副机上一份配置起不来时,每 5 秒硬推一次没有任何意义 —— 只会让副机每 5 秒重来一遍
// "停数据面 → 起失败 → 回滚"。管理员改了配置(修订号变了)立刻重推,不受退避影响。
func pushBackoff(n int) time.Duration {
	if n <= 0 {
		return 0
	}
	if n > 7 {
		n = 7
	}
	d := 5 * time.Second << uint(n-1) // 5s, 10s, 20s, 40s, 80s, 160s, 320s
	if d > 5*time.Minute {
		d = 5 * time.Minute
	}
	return d
}

func (h *Hub) syncNode(n model.Node, snap Snapshot) *nodeResult {
	st := h.getStatus(n) // 先把状态项建出来,首次推送才能记下 LastPush,不会下一轮又推一遍
	h.mu.Lock()
	pushedRev, lastPush := h.pushed[n.Id], st.LastPush
	pf := h.pushFail[n.Id]
	h.mu.Unlock()
	res := &nodeResult{n: n}
	if pushedRev != snap.Revision || time.Now().Unix()-lastPush > 600 {
		switch {
		case pf != nil && pf.rev == snap.Revision && time.Since(pf.at) < pushBackoff(pf.n):
			res.pushErr = fmt.Sprintf("推送失败 %d 次,%s 后再试: %s", pf.n, (pushBackoff(pf.n) - time.Since(pf.at)).Round(time.Second), pf.cause)
		default:
			if err := h.push(n, snap); err != nil {
				if pushRejected(err) {
					h.mu.Lock()
					if pf == nil || pf.rev != snap.Revision {
						pf = &pushFailure{rev: snap.Revision}
						h.pushFail[n.Id] = pf
					}
					pf.n++
					pf.at, pf.cause = time.Now(), err.Error()
					h.mu.Unlock()
				}
				res.pushErr = "推送失败: " + err.Error()
			} else {
				h.mu.Lock()
				delete(h.pushFail, n.Id)
				h.mu.Unlock()
			}
		}
	}
	// 推送成没成,报告都要拉:流量回收、超额停用、跨机设备并集全靠它。0.6.10 在推送失败时直接返回,
	// 于是一台配置没应用成功的副机,它的流量不再并入主机、它上面的设备 300 秒后就不再计入设备数。
	rep, err := h.fetchReport(n)
	if err != nil {
		res.err = "拉取报告失败: " + err.Error()
		if res.pushErr != "" {
			res.err = res.pushErr + ";" + res.err
		}
		return res
	}
	res.rep = rep
	return res
}

// forgetNodes 清掉已经不存在的副机留下的所有按节点缓存。
// 漏清 remoteLines / nodeNames 的话,用户的"在线设备"里会一直挂着已删服务器的线路。
func (h *Hub) forgetNodes(live map[uint]bool) {
	h.mu.Lock()
	defer h.mu.Unlock()
	for id := range h.status {
		if live[id] {
			continue
		}
		delete(h.status, id)
		delete(h.remote, id)
		delete(h.remoteAt, id)
		delete(h.pushed, id)
		delete(h.pushFail, id)
		delete(h.remoteLines, id)
		delete(h.nodeNames, id)
		delete(h.upHealth, id)
	}
}

// remoteReportGrace 副机多久没上报,它的在线 IP 就不再计入设备数并集(秒)。同步是 5 秒一轮,几分钟足够跨过抖动。
const remoteReportGrace = 300

// remoteForDeviceLimits 参与跨机设备数并集的副机报告:只要最近 remoteReportGrace 内上报过的。
// 失联的副机不计入 —— 它上面的设备是不是还在线无从得知;短暂超限比拿陈旧名单误伤别的机器上的用户轻。
// 报告本身不删(forgetNodes 才删),页面照常显示它最后的样子。
func (h *Hub) remoteForDeviceLimits() map[uint]map[string][]string {
	now := time.Now().Unix()
	h.mu.Lock()
	defer h.mu.Unlock()
	out := make(map[uint]map[string][]string, len(h.remote))
	for id, report := range h.remote {
		if h.freshLocked(id, now) {
			out[id] = report
		}
	}
	return out
}

// freshLocked 这台副机的报告还算数吗(最近 remoteReportGrace 内上报过),调用方持有 h.mu。
// 在线用户、在线 IP、最近连接这些汇总都只取还算数的:失联副机最后一份报告里的人不能一直挂着"在线"(审计 M067);
// 服务器页那一行照旧显示它最后的样子(Statuses 不过滤)。
func (h *Hub) freshLocked(id uint, now int64) bool {
	at, ok := h.remoteAt[id]
	return ok && now-at <= remoteReportGrace
}

// distributeIPs 把"其他机器上的在线 IP"下发给每台机器(含主机自身)。
func (h *Hub) distributeIPs(nodes []model.Node) {
	remote := h.remoteForDeviceLimits()
	// 需要跨机并集的用户:自己有设备数限制的,以及所属代理有设备池的(池按名下所有用户的 IP 并集判定)
	var users []model.User
	if err := h.d.DB.Where("device_limit > 0 OR reseller_id IN (SELECT id FROM resellers WHERE device_limit > 0)").Find(&users).Error; err != nil {
		// 数据库瞬时失败时不能把外部设备表当成空表下发，否则会暂时放宽
		// 设备限制并与副机恢复后的连接状态失去一致。
		logger.Warning("读取跨机设备限制用户失败,保留现有设备租约: ", err)
		return
	}
	if len(users) == 0 {
		if h.d.SetExternalIPs != nil {
			h.d.SetExternalIPs(map[string][]string{})
		}
		return
	}
	// 主机自身:外部 IP = 所有副机的并集
	local := map[string][]string{}
	for _, u := range users {
		set := map[string]bool{}
		for _, m := range remote {
			for _, ip := range m[u.Name] {
				set[ip] = true
			}
		}
		if len(set) > 0 {
			local[u.Name] = keys(set)
		}
	}
	if h.d.SetExternalIPs != nil {
		h.d.SetExternalIPs(local)
	}
	// 每台副机:外部 IP = 主机本机 + 其他副机。载荷在这里串行算好,发送各台各发、不等:失联的那台不能拖住这一轮。
	// 同一台上一份还在途就跳过这一轮(下一轮的载荷更新)
	for _, n := range nodes {
		if n.ApiUrl == "" || n.Token == "" {
			continue
		}
		h.mu.Lock()
		busy := h.ipsBusy[n.Id]
		if !busy {
			if h.ipsBusy == nil {
				h.ipsBusy = map[uint]bool{}
			}
			h.ipsBusy[n.Id] = true
		}
		h.mu.Unlock()
		if busy {
			continue
		}
		ext := map[string][]string{}
		for _, u := range users {
			set := map[string]bool{}
			if h.d.LocalIPs != nil {
				for _, ip := range h.d.LocalIPs(u.Name) {
					set[ip] = true
				}
			}
			for id, m := range remote {
				if id == n.Id {
					continue
				}
				for _, ip := range m[u.Name] {
					set[ip] = true
				}
			}
			if len(set) > 0 {
				ext[u.Name] = keys(set)
			}
		}
		h.rounds.Add(1)
		go func(n model.Node, ext map[string][]string) {
			defer h.rounds.Done()
			defer func() {
				h.mu.Lock()
				delete(h.ipsBusy, n.Id)
				h.mu.Unlock()
			}()
			var ack struct {
				OK string `json:"ok"`
			}
			if err := h.request(n, "POST", "external-ips", ext, &ack); err != nil {
				logger.Warning("向副机 ", n.Name, " 下发外部设备租约失败: ", err)
				return
			}
			if !strings.EqualFold(ack.OK, "1") && !strings.EqualFold(ack.OK, "true") {
				logger.Warning("副机 ", n.Name, " 未确认外部设备租约")
			}
		}(n, ext)
	}
}

func keys(m map[string]bool) []string {
	out := make([]string, 0, len(m))
	for k := range m {
		out = append(out, k)
	}
	sort.Strings(out)
	return out
}

func (h *Hub) getStatus(n model.Node) *NodeStatus {
	h.mu.Lock()
	defer h.mu.Unlock()
	st := h.status[n.Id]
	if st == nil {
		st = &NodeStatus{Id: n.Id, Name: n.Name}
		h.status[n.Id] = st
	}
	return st
}

func (h *Hub) setStatus(n model.Node, ok bool, errStr string, rep *Report) {
	st := h.getStatus(n)
	h.mu.Lock()
	wasOK, alerted := st.OK, st.alerted
	st.Name, st.OK, st.Error = n.Name, ok, errStr
	now := time.Now().Unix()
	if ok {
		st.LastSeen, st.failSince = now, 0
	} else if st.failSince == 0 {
		st.failSince = now
	}
	if rep != nil {
		st.Version, st.Hostname, st.CoreRunning, st.Uptime, st.Revision, st.CertDays = rep.Version, rep.Hostname, rep.CoreRunning, rep.Uptime, rep.Revision, rep.CertDays
		// hubRevision 在副机 ApplySnapshot 的事务里就写了,数据面有没有真的应用成功要看 ReloadPending。
		// 不看它,一台新配置起不来、正在跑回滚旧配置的副机会显示"在线 · 已同步",管理员看不出任何异常。
		st.Synced = rep.Revision == h.revision && !rep.ReloadPending
		if rep.ReloadPending && st.Error == "" {
			st.Error = "配置已收到,但这台副机的数据面没应用成功(仍在跑上一份可用配置),看副机日志"
		}
		st.OnlineUsers = len(rep.Onlines)
		st.conns = rep.Conns
		st.ReloadError, st.ReloadAt = "", 0
		if rep.Reload != nil && !rep.Reload.OK {
			st.ReloadError, st.ReloadAt = rep.Reload.Op+":"+rep.Reload.Error, rep.Reload.At
		}
		st.VersionMismatch = rep.Version != "" && h.d.Version != "" && rep.Version != h.d.Version
	} else if !ok {
		// A failed request makes the node offline, but does not invalidate its
		// last report: the panel keeps showing the node's last known state, and
		// its device set stays in the cross-node union for remoteReportGrace
		// (see remoteForDeviceLimits) so a short blip cannot admit a second set
		// of devices, while a real outage stops a stale list from rejecting or
		// evicting anyone.
		st.Synced = false
		st.CoreRunning = false
	}
	var msgs []string
	// 按"连续失败了多久"告警,不看上次在线时间:从来没连上过的副机(配错了)也得有人知道
	if !ok && !alerted && now-st.failSince > 60 {
		st.alerted = true
		msgs = append(msgs, "🔴 <b>副机失联</b>:"+alertText(n.Name, 64)+"\n"+alertText(errStr, 300))
	}
	if ok && alerted {
		st.alerted = false
		msgs = append(msgs, "🟢 <b>副机恢复</b>:"+alertText(n.Name, 64))
	}
	// 在线但配置一直没同步上(推送被拒、版本过低、数据面应用失败):失联告警管不到,以前一声不吭(审计 M064)。
	// 刚改完配置到下一份报告之间本来就有几秒不同步,推送退避最长 5 分钟,所以持续 unsyncedAlertAfter 才告警
	switch {
	case !ok:
		st.unsyncedSince = 0 // 失联由上面那条管;恢复在线后重新计时
	case rep == nil:
	case st.Synced:
		st.unsyncedSince = 0
		if st.unsyncedAlerted {
			st.unsyncedAlerted = false
			msgs = append(msgs, "🟢 <b>副机配置已同步</b>:"+alertText(n.Name, 64))
		}
	case st.unsyncedSince == 0:
		st.unsyncedSince = now
	case !st.unsyncedAlerted && now-st.unsyncedSince > unsyncedAlertAfter:
		st.unsyncedAlerted = true
		why := st.Error
		if why == "" && st.VersionMismatch {
			why = "副机版本 " + st.Version + " 与主机不同"
		}
		if why == "" {
			why = "副机上报的配置修订号一直对不上"
		}
		msgs = append(msgs, "🟠 <b>副机配置没同步上</b>:"+alertText(n.Name, 64)+"(在线,已持续 "+strconv.FormatInt((now-st.unsyncedSince)/60, 10)+" 分钟)\n"+alertText(why, 300))
	}
	_ = wasOK
	h.mu.Unlock()
	if h.d.Notify != nil {
		for _, m := range msgs {
			h.d.Notify("tgOnCore", m)
		}
	}
}

// unsyncedAlertAfter 副机在线但配置持续多久没同步上才告警(秒)。
const unsyncedAlertAfter = 10 * 60

// alertText 告警里的外部文本(服务器名、副机返回的错误)先截断再转义:Telegram 按 HTML 解析,
// 一个裸的 < 或超长正文就整条被拒,告警被吞掉(审计 M060)。
func alertText(s string, max int) string {
	if r := []rune(s); len(r) > max {
		s = string(r[:max]) + "…"
	}
	return notify.Esc(s)
}

func (h *Hub) push(n model.Node, snap Snapshot) error {
	snap.SelfNodeId = n.Id
	var out struct {
		OK       json.RawMessage `json:"ok"`
		Revision string          `json:"revision"`
	}
	if err := h.request(n, "POST", "apply", snap, &out); err != nil {
		return err
	}
	ok := strings.Trim(strings.TrimSpace(string(out.OK)), `"`)
	if ok != "1" && !strings.EqualFold(ok, "true") {
		return fmt.Errorf("%w:修订 %s (ok=%q)", errApplyUnconfirmed, snap.Revision, ok)
	}
	if out.Revision != snap.Revision {
		return fmt.Errorf("%w:确认的修订号不匹配,期望 %s,收到 %s", errApplyUnconfirmed, snap.Revision, out.Revision)
	}
	h.mu.Lock()
	h.pushed[n.Id] = snap.Revision
	if st := h.status[n.Id]; st != nil {
		st.LastPush = time.Now().Unix()
	}
	h.mu.Unlock()
	logger.Info("已向副机 ", n.Name, " 推送配置 ", snap.Revision)
	return nil
}

func (h *Hub) fetchReport(n model.Node) (Report, error) {
	var rep Report
	err := h.request(n, "GET", "report", nil, &rep)
	return rep, err
}

// Ping 主动探测一台副机(面板"测试"按钮)。
func (h *Hub) Ping(n model.Node) (map[string]interface{}, error) {
	var out map[string]interface{}
	err := h.request(n, "GET", "ping", nil, &out)
	return out, err
}

// PushNow 立即向某副机推送当前配置。
func (h *Hub) PushNow(n model.Node) error {
	if _, err := h.refreshSnapshot(); err != nil {
		return err
	}
	return h.pushLatest(n)
}

// Decommission 停用 / 删除副机时调用:给它推一份空用户表(线路照旧、一个用户都没有),那台立刻停止为任何人服务。
// 以前停用 / 删除只是不再同步,那台的数据面一直按最后一份用户表放行所有人,之后的停用、到期、超额、重置在它上面
// 都不生效,流量也不记账(审计 M042 / M061)。
//
// before 是改库之前的快照(BuildSnapshot):删掉的副机在改库之后的快照里已经没有自己那一行、独占的线路也停了,
// 拿那份去推,它认不出本机、会把所有线路都当成自己的去渲染。调用时库里已经把它停用 / 删掉:在途的定时推送拿到闸后
// 看它不再启用,不会把空表盖回去;重新启用后修订号不同,定时同步推回完整快照。
func (h *Hub) Decommission(n model.Node, before Snapshot) error {
	if n.IsLocal || h.d.IsNode() {
		return nil
	}
	if n.ApiUrl == "" || n.Token == "" {
		return errors.New("这台副机没有配置 API 地址或令牌,发不出下线通知")
	}
	if err := h.waitNode(n.Id, nodeWait); err != nil {
		return err
	}
	defer h.releaseNode(n.Id)
	cur, err := h.refreshSnapshot() // 序号取此刻最新的:不低于这台已经收过的任何一份,否则被它当成过期快照拒掉
	if err != nil {
		return err
	}
	snap := withoutUsers(before)
	snap.Sequence, snap.Version, snap.MinNode = cur.Sequence, cur.Version, cur.MinNode
	return h.push(n, snap)
}

// withoutUsers 同一份快照去掉所有用户及挂在用户上的分配,重算修订号。线路留着:有线路没用户,谁连都认证不过
// (TestEveryProtocolConstructsWithoutUsers 钉着这样的配置内核照样起得来)。
func withoutUsers(s Snapshot) Snapshot {
	s.Users, s.UserLines, s.UserLineNodes, s.UserExts, s.LimitStates = []model.User{}, []model.UserLine{}, []model.UserLineNode{}, []model.UserExt{}, []model.LimitState{}
	s.Revision = revisionOf(s)
	return s
}

// pushLatest 在这台副机的闸里推最新快照(等不到闸就报错,见 nodeWait)。
func (h *Hub) pushLatest(n model.Node) error {
	if err := h.waitNode(n.Id, nodeWait); err != nil {
		return err
	}
	defer h.releaseNode(n.Id)
	return h.push(n, h.latestSnapshot())
}

// buildPushSnapshot 在 syncMu 内调用。序号持久化，进程重启后也不会重用旧序号。
func (h *Hub) buildPushSnapshot() (Snapshot, error) {
	snap, err := BuildSnapshot(h.d.DB, h.d.Setting)
	if err != nil {
		return snap, err
	}
	err = h.d.DB.Transaction(func(tx *gorm.DB) error {
		var raw string
		if err := tx.Model(&model.Setting{}).Select("value").Where("key = ?", "hubPushSequence").Scan(&raw).Error; err != nil {
			return err
		}
		seq, err := strconv.ParseUint(raw, 10, 64)
		if raw != "" && err != nil {
			return fmt.Errorf("读取主机推送序号: %w", err)
		}
		var previousRevision string
		if err := tx.Model(&model.Setting{}).Select("value").Where("key = ?", "hubPushRevision").Scan(&previousRevision).Error; err != nil {
			return err
		}
		// The content revision, rather than the five-second polling tick, is
		// what normally orders snapshots.  A restored database can contain an
		// old sequence, however, while a node has already accepted a newer one.
		// Raise the sequence to the current wall-clock floor even when the
		// revision is unchanged; this lets a normal post-restore clock advance
		// past the sequence remembered by the node without inventing an epoch
		// that an old snapshot could replay.  The +1 floor also handles clock
		// rollback and the legacy small counter values.
		clockNow := time.Now().UnixNano()
		clockSeq := uint64(0)
		if clockNow > 0 {
			clockSeq = uint64(clockNow)
		}
		if raw != "" && previousRevision == snap.Revision && h.sequencePrimed {
			snap.Sequence = seq
			return nil
		}
		if raw != "" && previousRevision == snap.Revision {
			if seq == ^uint64(0) {
				return errors.New("主机推送序号已耗尽")
			}
			next := seq + 1
			if clockSeq > next {
				next = clockSeq
			}
			snap.Sequence = next
			if err := upsertSetting(tx, "hubPushSequence", strconv.FormatUint(next, 10)); err != nil {
				return err
			}
			return nil
		}
		if seq == ^uint64(0) {
			return errors.New("主机推送序号已耗尽")
		}
		next := seq + 1
		if clockSeq > next {
			next = clockSeq
		}
		snap.Sequence = next
		if err := upsertSetting(tx, "hubPushSequence", strconv.FormatUint(snap.Sequence, 10)); err != nil {
			return err
		}
		return upsertSetting(tx, "hubPushRevision", snap.Revision)
	})
	if err == nil {
		h.sequencePrimed = true
	}
	return snap, err
}

// SyncNow 等待当前快照在所有启用副机上得到实际应用 ACK。
func (h *Hub) SyncNow() error {
	if h.d.IsNode() {
		return nil
	}
	if _, err := h.refreshSnapshot(); err != nil {
		return err
	}
	var nodes []model.Node
	if err := h.d.DB.Where("enabled = ? AND is_local = ?", true, false).Find(&nodes).Error; err != nil {
		return err
	}
	var wg sync.WaitGroup
	errs := make(chan error, len(nodes))
	for _, n := range nodes {
		wg.Add(1)
		go func(n model.Node) {
			defer wg.Done()
			if n.ApiUrl == "" || n.Token == "" {
				errs <- fmt.Errorf("副机 %s 未配置 API 地址或令牌", n.Name)
				return
			}
			if err := h.pushLatest(n); err != nil {
				errs <- fmt.Errorf("副机 %s: %w", n.Name, err)
			}
		}(n)
	}
	wg.Wait()
	close(errs)
	var all []error
	for e := range errs {
		all = append(all, e)
	}
	return errors.Join(all...)
}

// KickResult 一次踢线的结果:合计 + 各机明细。本机那一行由 runner 填(Local=true),这里只管副机。
type KickResult struct {
	Closed   int          `json:"closed"`   // 断开的连接数,所有机器合计
	Sessions int          `json:"sessions"` // 关掉的整条会话数(hysteria2 / tuic / anytls),合计
	Failed   int          `json:"failed"`   // 没派发成功的副机数(失联、版本过旧)
	Servers  []KickServer `json:"servers"`
}

// KickServer 一台机器上的踢线结果。
type KickServer struct {
	Id       uint   `json:"id,omitempty"`
	Name     string `json:"name,omitempty"`
	Local    bool   `json:"local,omitempty"` // 本机:名字由前端按语言渲染
	Closed   int    `json:"closed"`
	Sessions int    `json:"sessions"`
	Error    string `json:"error,omitempty"`    // 派发失败的原因
	Outdated bool   `json:"outdated,omitempty"` // 副机版本过旧,还没有踢线接口
	// Unconfigured 这台副机没填 API 地址或令牌,根本没法派发(和"失联"不是一回事)
	Unconfigured bool `json:"unconfigured,omitempty"`
}

// Scrubbed 给代理作用域看的版本:去掉错误原文(拨号错误里带副机地址),只留"失败 / 版本过旧"的事实。
func (r KickResult) Scrubbed() KickResult {
	out := r
	out.Servers = make([]KickServer, len(r.Servers))
	for i, s := range r.Servers {
		if s.Error != "" {
			s.Error = "failed"
		}
		out.Servers[i] = s
	}
	return out
}

// kickCall 一次进行中的派发;同名的后来者等它的结果。
type kickCall struct {
	done chan struct{}
	res  KickResult
}

// kickPerNode 每台副机的踢线预算。踢线是面板上的交互请求,失联的副机不能把整个请求拖到同步 API 的 25 秒;
// 各副机并发跑,总耗时也就这么久。
const kickPerNode = 3 * time.Second

// KickUser 把踢线派发到所有副机(主机才做;副机上调用直接返回空结果,不会递归派发)。
// 同一用户名并发的调用单飞:只发一轮请求,后到的拿同一份结果。
// 回 404 的副机是版本过旧(还没有 kick 接口):算失败并标出来,但**不提高 MinNodeVersion** —— 提高会让它连快照都收不到。
func (h *Hub) KickUser(name string) KickResult {
	name = strings.TrimSpace(name)
	if h == nil || name == "" || h.d.IsNode == nil || h.d.IsNode() {
		return KickResult{}
	}
	h.kickMu.Lock()
	if c := h.kicking[name]; c != nil {
		h.kickMu.Unlock()
		<-c.done
		return c.res
	}
	c := &kickCall{done: make(chan struct{})}
	h.kicking[name] = c
	h.kickMu.Unlock()
	c.res = h.kickRemote(name)
	h.kickMu.Lock()
	delete(h.kicking, name)
	h.kickMu.Unlock()
	close(c.done)
	return c.res
}

func (h *Hub) kickRemote(name string) KickResult {
	nodes, err := h.remoteNodes()
	if err != nil {
		return KickResult{Failed: 1, Servers: []KickServer{{Error: "读取副机列表失败: " + err.Error()}}}
	}
	results := make([]KickServer, len(nodes))
	var wg sync.WaitGroup
	for i, n := range nodes {
		results[i] = KickServer{Id: n.Id, Name: n.Name}
		if n.ApiUrl == "" || n.Token == "" {
			results[i].Error, results[i].Unconfigured = "副机没有配置 API 地址或令牌", true
			continue
		}
		wg.Add(1)
		go func(i int, n model.Node) {
			defer wg.Done()
			var out struct {
				Closed   int `json:"closed"`
				Sessions int `json:"sessions"`
			}
			err := h.requestTimeout(n, http.MethodPost, "kick", map[string]string{"name": name}, &out, kickPerNode)
			if err != nil {
				var he *httpStatusError
				if errors.As(err, &he) && he.Status == http.StatusNotFound {
					results[i].Outdated, results[i].Error = true, "副机版本过旧,没有踢线接口"
				} else {
					results[i].Error = err.Error()
				}
				logger.Warning("向副机 ", n.Name, " 派发用户踢线失败: ", err)
				return
			}
			results[i].Closed, results[i].Sessions = out.Closed, out.Sessions
		}(i, n)
	}
	wg.Wait()
	res := KickResult{Servers: results}
	for _, s := range results {
		if s.Error != "" {
			res.Failed++
			continue
		}
		res.Closed += s.Closed
		res.Sessions += s.Sessions
	}
	return res
}

// UpstreamHealthAll 各副机上报的上游巡检结果(副机 id → 结果)。
func (h *Hub) UpstreamHealthAll() map[uint][]UpstreamHealth {
	h.mu.Lock()
	defer h.mu.Unlock()
	out := make(map[uint][]UpstreamHealth, len(h.upHealth))
	for id, list := range h.upHealth {
		cp := make([]UpstreamHealth, len(list))
		copy(cp, list)
		out[id] = cp
	}
	return out
}

// SetUpstreamHealth 面板「测试」按钮在副机上测出来的结果直接写进汇总:比那台上一轮上报的新才写,
// 概览不用等它下一轮上报才变。连续失败次数接着原来的算。
func (h *Hub) SetUpstreamHealth(nodeID uint, uh UpstreamHealth) {
	h.mu.Lock()
	defer h.mu.Unlock()
	list := h.upHealth[nodeID]
	for i := range list {
		if list[i].Id != uh.Id {
			continue
		}
		if uh.CheckedAt >= list[i].CheckedAt {
			uh.Fails = list[i].Fails + 1
			if uh.OK {
				uh.Fails = 0
			}
			list[i] = uh
		}
		return
	}
	uh.Fails = 0
	if !uh.OK {
		uh.Fails = 1
	}
	h.upHealth[nodeID] = append(list, uh)
}

// CheckUpstreamsOn 让一台副机立刻跑一轮巡检,并把它的全部结果写进汇总(概览的「立即巡检」用:
// 只刷主机那份、副机还是上一轮的,概览就会一直挂着旧故障)。
func (h *Hub) CheckUpstreamsOn(n model.Node) error {
	var out []UpstreamHealth
	if err := h.request(n, "POST", "upstream-check", nil, &out); err != nil {
		return err
	}
	h.mu.Lock()
	h.upHealth[n.Id] = out
	h.mu.Unlock()
	return nil
}

// TestOutboundOn 让某台副机实测一个还不是上游的出站(外部订阅展开后的逐台测速),副机不落库。
func (h *Hub) TestOutboundOn(n model.Node, up model.Upstream) (UpstreamHealth, error) {
	var out UpstreamHealth
	err := h.request(n, "POST", "outbound-test", up, &out)
	return out, err
}

// TestUpstreamOn 让某台副机立刻测一条上游(面板的"测试"按钮:在真正用它的机器上测才有意义)。
func (h *Hub) TestUpstreamOn(n model.Node, id uint) (UpstreamHealth, error) {
	var out UpstreamHealth
	err := h.request(n, "POST", "upstream-test", map[string]uint{"id": id}, &out)
	return out, err
}

// Statuses 返回所有副机的运行状态。
func (h *Hub) Statuses() map[uint]NodeStatus {
	h.mu.Lock()
	defer h.mu.Unlock()
	out := map[uint]NodeStatus{}
	for id, st := range h.status {
		out[id] = *st
	}
	return out
}

// RemoteIPs 返回某用户在所有副机上的在线 IP。
// RemoteIPsAll 一次锁拿全量:用户 → 副机上报的在线 IP(面板列表用)。
func (h *Hub) RemoteIPsAll() map[string][]string {
	now := time.Now().Unix()
	h.mu.Lock()
	defer h.mu.Unlock()
	out := map[string]map[string]bool{}
	for id, m := range h.remote {
		if !h.freshLocked(id, now) {
			continue
		}
		for user, ips := range m {
			if out[user] == nil {
				out[user] = map[string]bool{}
			}
			for _, ip := range ips {
				out[user][ip] = true
			}
		}
	}
	res := make(map[string][]string, len(out))
	for user, set := range out {
		res[user] = keys(set)
	}
	return res
}

func (h *Hub) RemoteIPs(user string) []string {
	now := time.Now().Unix()
	h.mu.Lock()
	defer h.mu.Unlock()
	set := map[string]bool{}
	for id, m := range h.remote {
		if !h.freshLocked(id, now) {
			continue
		}
		for _, ip := range m[user] {
			set[ip] = true
		}
	}
	if len(set) == 0 {
		return nil
	}
	return keys(set)
}

// RemoteIPLines 返回各副机上 该用户的 源 IP → 线路名(线路名已带服务器后缀,如 "香港1-台湾")。
// RemoteIPLinesAll 一次锁拿全量:用户 → 源 IP → 线路名(带服务器后缀)。
func (h *Hub) RemoteIPLinesAll() map[string]map[string][]string {
	now := time.Now().Unix()
	h.mu.Lock()
	defer h.mu.Unlock()
	out := map[string]map[string][]string{}
	for id, m := range h.remoteLines {
		if !h.freshLocked(id, now) {
			continue
		}
		name := h.nodeNames[id]
		for user, ips := range m {
			if out[user] == nil {
				out[user] = map[string][]string{}
			}
			for ip, lines := range ips {
				for _, l := range lines {
					if name != "" {
						l += "-" + name
					}
					out[user][ip] = append(out[user][ip], l)
				}
			}
		}
	}
	return out
}

func (h *Hub) RemoteIPLines(user string) map[string][]string {
	now := time.Now().Unix()
	h.mu.Lock()
	defer h.mu.Unlock()
	out := map[string][]string{}
	for id, m := range h.remoteLines {
		if !h.freshLocked(id, now) {
			continue
		}
		name := h.nodeNames[id]
		for ip, lines := range m[user] {
			for _, l := range lines {
				if name != "" {
					l += "-" + name
				}
				out[ip] = append(out[ip], l)
			}
		}
	}
	if len(out) == 0 {
		return nil
	}
	return out
}

// RemoteOnlineUsers 返回在任一副机上在线的用户名。
func (h *Hub) RemoteOnlineUsers() []string {
	now := time.Now().Unix()
	h.mu.Lock()
	defer h.mu.Unlock()
	set := map[string]bool{}
	for id, m := range h.remote {
		if !h.freshLocked(id, now) {
			continue
		}
		for u, ips := range m {
			if len(ips) > 0 {
				set[u] = true
			}
		}
	}
	return keys(set)
}

// httpStatusError 副机回了非 200:调用方按状态码归类(404 = 这台副机还没有这个接口)。
type httpStatusError struct {
	Status int
	Msg    string
}

func (e *httpStatusError) Error() string { return fmt.Sprintf("HTTP %d: %s", e.Status, e.Msg) }

func (h *Hub) request(n model.Node, method, path string, body interface{}, out interface{}) error {
	return h.requestTimeout(n, method, path, body, out, 25*time.Second)
}

func (h *Hub) requestTimeout(n model.Node, method, path string, body interface{}, out interface{}, timeout time.Duration) error {
	base := strings.TrimRight(n.ApiUrl, "/")
	if !strings.HasSuffix(base, "/api") {
		base += "/api"
	}
	var rd io.Reader
	if body != nil {
		b, err := json.Marshal(body)
		if err != nil {
			return err
		}
		rd = bytes.NewReader(b)
	}
	if timeout <= 0 {
		timeout = 25 * time.Second
	}
	ctx, cancel := context.WithTimeout(h.baseCtx(), timeout) // Stop 时在途请求立刻返回
	defer cancel()
	req, err := http.NewRequestWithContext(ctx, method, base+"/agent/"+path, rd)
	if err != nil {
		return err
	}
	req.Header.Set("X-Agent-Token", n.Token)
	req.Header.Set("Content-Type", "application/json")
	resp, err := h.clientFor(n).Do(req)
	if err != nil {
		return err
	}
	defer resp.Body.Close()
	b, _ := io.ReadAll(io.LimitReader(resp.Body, 64<<20))
	if resp.StatusCode != 200 {
		var e struct{ Error string }
		json.Unmarshal(b, &e)
		if e.Error == "" {
			e.Error = errBody(b, resp.StatusCode)
		}
		return &httpStatusError{Status: resp.StatusCode, Msg: e.Error}
	}
	if out != nil {
		if err := json.Unmarshal(b, out); err != nil {
			return errors.New("响应不是 JSON(API 地址是否指向面板路径,如 https://tw:2053/ad/?)")
		}
	}
	return nil
}

// errBody 副机回了非 JSON 的错误体(反代的报错页、被劫持的页面):HTML 只留状态码,其余只留前 200 字节。
// 原文整段进状态、告警和日志,动辄几 KB 的网页(审计 M060)。
func errBody(b []byte, status int) string {
	s := strings.TrimSpace(string(b))
	if strings.HasPrefix(s, "<") {
		return http.StatusText(status)
	}
	if r := []rune(s); len(r) > 200 {
		s = string(r[:200]) + "…"
	}
	return s
}

// clientFor 副机用哪个 HTTP 客户端。勾了"跳过证书校验"的不是完全不看证书:第一次连上把它的证书
// 指纹记进库(信任首次连接),之后指纹变了就拒绝 —— 主副之间传的是令牌和整份用户快照,完全不校验
// 等于把这些交给线路上任何一个中间人。副机重签过证书的,在服务器页点「重置指纹」重新信任。
func (h *Hub) clientFor(n model.Node) *http.Client {
	if !n.Insecure {
		return h.verified
	}
	key := fmt.Sprintf("%d:%s", n.Id, n.CertFP)
	h.mu.Lock()
	defer h.mu.Unlock()
	if c := h.pinned[key]; c != nil {
		return c
	}
	id, want, name := n.Id, n.CertFP, n.Name
	host := ""
	if u, err := url.Parse(n.ApiUrl); err == nil {
		host = u.Hostname()
	}
	c := &http.Client{Timeout: 25 * time.Second, Transport: &http.Transport{TLSClientConfig: &tls.Config{
		InsecureSkipVerify: true, // 不查签发链(自签 / IP 证书都过不了),只认下面记住的指纹
		VerifyPeerCertificate: func(raw [][]byte, _ [][]*x509.Certificate) error {
			if len(raw) == 0 {
				return errors.New("副机没有出示证书")
			}
			sum := sha256.Sum256(raw[0])
			got := hex.EncodeToString(sum[:])
			if want == "" {
				h.d.DB.Model(&model.Node{}).Where("id = ? AND COALESCE(cert_fp, '') = ''", id).Update("cert_fp", got)
				logger.Info("已记住副机 ", name, " 的证书指纹 ", got[:16])
				return nil
			}
			if got != want && host != "" && h.chainValid(raw, host) {
				// 正规 CA 自动续期换了证书:新证书能按 API 地址的主机名过根证书校验,就换记新指纹。以前每次续期都失联,
				// 得手点「重置指纹」(审计 M063)。自签证书换了照旧拒绝 —— 那正是钉指纹要防的
				h.d.DB.Model(&model.Node{}).Where("id = ? AND cert_fp = ?", id, want).Update("cert_fp", got)
				logger.Info("副机 ", name, " 的证书已由正规 CA 续期,指纹换记为 ", got[:16])
				return nil
			}
			if got != want {
				return fmt.Errorf("副机证书指纹变了(记住的 %s…,现在 %s…):要是你重签了证书,到服务器页点「重置指纹」重新信任", want[:16], got[:16])
			}
			return nil
		},
	}}}
	// 这台机器旧指纹的客户端不再用了
	for k, old := range h.pinned {
		if strings.HasPrefix(k, fmt.Sprintf("%d:", id)) {
			old.CloseIdleConnections()
			delete(h.pinned, k)
		}
	}
	h.pinned[key] = c
	return c
}

// chainValid 副机出示的证书链能不能按 host 过根证书校验(正规 CA 签的)。
func (h *Hub) chainValid(raw [][]byte, host string) bool {
	certs := make([]*x509.Certificate, 0, len(raw))
	for _, r := range raw {
		c, err := x509.ParseCertificate(r)
		if err != nil {
			return false
		}
		certs = append(certs, c)
	}
	inter := x509.NewCertPool()
	for _, c := range certs[1:] {
		inter.AddCert(c)
	}
	_, err := certs[0].Verify(x509.VerifyOptions{DNSName: host, Intermediates: inter, Roots: h.roots})
	return err == nil
}

// CloseIdleConnections 关掉所有副机客户端的空闲连接(测试里数协程用)。
func (h *Hub) CloseIdleConnections() {
	h.verified.CloseIdleConnections()
	h.mu.Lock()
	defer h.mu.Unlock()
	for _, c := range h.pinned {
		c.CloseIdleConnections()
	}
}
