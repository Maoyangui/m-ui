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
	"net"
	"net/http"
	"net/url"
	"runtime/debug"
	"sort"
	"strconv"
	"strings"
	"sync"
	"time"

	"github.com/Maoyangui/m-ui/certutil"
	"github.com/Maoyangui/m-ui/database"
	"github.com/Maoyangui/m-ui/database/model"
	"github.com/Maoyangui/m-ui/ext"
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
	"subProfileTitle", "subEncode", "subShowNotice", "subClashExt", "subSingboxExt", "subAutoFormat", "blockBT", "supportOnline", "subUpdates",
	"subPageEnabled", "subPageTitle", "subPageSupport", "subPageNotice", "subShareEnabled", "subPageBuyURL", "subPageBrand",
}

// MinNodeVersion 副机至少要这个版本才能正确应用当前快照(快照里新增了它必须理解的字段时抬高它)。
// 0.5.0:用户的停用原因、代理的额度用尽标志、私网屏蔽开关。0.6.0:规则限速状态表(副机要叠加到限速上)。
const MinNodeVersion = "0.6.0"

// RouteRulesMinNode 有线路用了分流规则时副机至少要这个版本:旧版副机不认识这个字段,会照常应用快照却把规则丢掉,
// 该走别的出口、该拦的流量全走线路默认的上游 —— 宁可让它拒收并提示升级。没有线路用分流规则时不抬门槛,
// 只升级主机不影响旧副机同步。
const RouteRulesMinNode = "0.6.14"

// LineUpstreamMinNode 有上游用的是本面板的线路时副机至少要这个版本:旧版副机不会把专用用户挂到目标线路上,链路连不通。
const LineUpstreamMinNode = "0.7.16"

// ExtFilterMinNode 有启用的外部节点用了隐藏、改名或排除关键词时副机至少要这个版本:更早的副机不认识这三个字段,
// 照收快照、解码时把它们丢掉,订阅地址指向它的用户照样拿到被隐藏的节点,主机还显示"已同步"(副机回报的是主机算好的修订号)。
const ExtFilterMinNode = "0.7.16"

// ExtKeyMinNode 有启用的外部节点按节点指纹隐藏 / 改名时副机至少要这个版本:0.7.16 只认按名字的记录,
// 指纹记录(面板上点隐藏、改名都会记)会被它丢掉。
const ExtKeyMinNode = "0.7.17"

// minNodeFor 这份快照要求的副机最低版本。没用到新功能时不抬门槛,只升级主机不影响旧副机同步。
// 按版本从高到低检查,命中即返回。
func minNodeFor(lines []model.Line, upstreams []model.Upstream, exts []model.ExtNode) string {
	var usesFilter bool
	for _, e := range exts {
		if !e.Enabled {
			continue
		}
		f := ext.FilterOf(e)
		if f.UsesKeys() {
			return ExtKeyMinNode
		}
		usesFilter = usesFilter || f.UsesNames()
	}
	for _, u := range upstreams {
		if u.FromLine > 0 || render.IsGroup(u) { // 上游组同一版加的:旧版副机不会把成员渲染出来,整份配置起不来
			return LineUpstreamMinNode
		}
	}
	if usesFilter {
		return ExtFilterMinNode
	}
	for _, l := range lines {
		if render.HasRouteRules(l) {
			return RouteRulesMinNode
		}
	}
	return MinNodeVersion
}

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
	// Logos 落地页 logo(主面板与各代理的,图片原样字节):副机的订阅口也出落地页。
	// 副机整表替换;旧版主机不发(解码为 nil)也按"一张都没有"清空:旧版主机没有 logo,副机显示内置的才和它一致。
	// 旧版副机不认识这个字段,照常应用、显示内置 logo,所以不抬版本门槛;它升级后第一份快照会整份应用一次
	// (副机记着"哪一版程序应用了哪一份",见 web 的 agentAppliedKey),logo 随之落库。
	Logos []model.PageLogo `json:"logos"`
}

// BuildSnapshot 从主机数据库构造快照并计算修订号(只含会影响副机行为的字段)。
// 十来条查询放在同一个只读事务里:读到的是同一时刻的一份(以前各查各的,并发写入时可能推出 user_lines 引用了
// 不在 users 里的用户这种短暂不一致的快照),而且不拿写锁 —— 别的写事务占着锁时照样构造得出来。
func BuildSnapshot(db *gorm.DB, setting func(string) string) (Snapshot, error) {
	var s Snapshot
	if err := database.ReadTx(db, func(tx *gorm.DB) error { return readSnapshot(tx, &s) }); err != nil {
		return s, err
	}
	s.Settings = map[string]string{}
	for _, k := range SyncedSettings {
		s.Settings[k] = setting(k)
	}
	s.Revision = revisionOf(s)
	return s, nil
}

func readSnapshot(db *gorm.DB, s *Snapshot) error {
	if err := db.Order("sort asc, id asc").Find(&s.Nodes).Error; err != nil {
		return err
	}
	for i := range s.Nodes {
		s.Nodes[i].Token = ""
		if s.Nodes[i].IsLocal {
			s.MasterId = s.Nodes[i].Id
		}
	}
	if err := db.Order("sort asc, id asc").Find(&s.Upstreams).Error; err != nil {
		return err
	}
	if err := db.Order("sort asc, id asc").Find(&s.Lines).Error; err != nil {
		return err
	}
	if err := db.Order("id asc").Find(&s.Users).Error; err != nil {
		return err
	}
	for i := range s.Users {
		// 副机不需要也不该拥有主机的计量;凭据/配额/限速/启用状态才是它要的
		s.Users[i].Up, s.Users[i].Down, s.Users[i].TotalUp, s.Users[i].TotalDown, s.Users[i].OnlineAt = 0, 0, 0, 0, 0
	}
	if err := db.Order("id asc").Find(&s.Resellers).Error; err != nil {
		return err
	}
	for i := range s.Resellers {
		s.Resellers[i].Password, s.Resellers[i].TotpSecret, s.Resellers[i].ApiToken = "", "", "" // 副机不跑代理面板,不需要这些
	}
	// 下发的是与代理授权取过交集的分配:副机直接照此渲染,不用(也不能)自己再算 —— 旧版主机不下发授权表
	links, scopes, err := render.EffectiveLines(db, 0)
	if err != nil {
		return err
	}
	s.UserLines, s.UserLineNodes = links, scopes
	s.ResellerLines, s.ResellerLineNodes = []model.ResellerLine{}, []model.ResellerLineNode{}
	if err := db.Order("reseller_id asc, line_id asc").Find(&s.ResellerLines).Error; err != nil {
		return err
	}
	if err := db.Order("reseller_id asc, line_id asc, node_id asc").Find(&s.ResellerLineNodes).Error; err != nil {
		return err
	}
	if err := db.Order("sort asc, id asc").Find(&s.Exts).Error; err != nil {
		return err
	}
	if err := db.Order("user_id asc, ext_id asc").Find(&s.UserExts).Error; err != nil {
		return err
	}
	s.Logos = []model.PageLogo{} // 一张都没有也发空表(不是 nil):副机据此删掉主机已经删了的 logo
	// 只发主面板与现存代理的:删代理时他的 logo 一起删,万一留下没主的行也不往副机推(没有落地页会用它)
	if err := db.Where("reseller_id = 0 OR reseller_id IN (SELECT id FROM resellers)").Order("reseller_id asc").Find(&s.Logos).Error; err != nil {
		return err
	}
	return db.Order("id asc").Find(&s.LimitStates).Error
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
		nodes[i].ServicePorts = nil // 副机端口只给主机做校验用,变了不必整份重推
		nodes[i].Certs = certsForRevision(nodes[i].Certs)
	}
	s.Nodes = nodes
	// 外部订阅的抓取时间 / 报错 / 节点数只是主机自己的记录:每半小时抓一次就推一次全量快照,
	// 副机热更新一轮、限速桶清一遍,毫无意义。抓到的内容也只看"输出到订阅的节点"(筛选、改名、前缀之后)的指纹:
	// 被隐藏的信息节点里"剩余流量"的数字每次刷新都变,输出没变就不算配置变了;顺带不必每 5 秒把整份缓存序列化一遍。
	// 筛选条件本身(隐藏、改名、关键词、前缀)照样计入:它们变了副机要按新的重新筛。
	exts := make([]model.ExtNode, len(s.Exts))
	copy(exts, s.Exts)
	for i := range exts {
		exts[i].Cache = ext.OutputSig(exts[i])
		exts[i].LastFetch, exts[i].LastTry, exts[i].LastError, exts[i].NodeCount, exts[i].NodeTotal = 0, 0, "", 0, 0
		exts[i].ContentSig, exts[i].FetchAgent, exts[i].PrefsRev = "", "", 0
	}
	s.Exts = exts
	// logo 只看摘要(Sum 就是图片字节的哈希):每 5 秒算一次修订号不用把图片编码一遍;同一张图重新上传只改了时间,不触发推送
	if s.Logos != nil {
		logos := make([]model.PageLogo, len(s.Logos))
		copy(logos, s.Logos)
		for i := range logos {
			logos[i].Data, logos[i].UpdatedAt = nil, 0
		}
		s.Logos = logos
	}
	b, _ := json.Marshal(s)
	sum := sha256.Sum256(b)
	return hex.EncodeToString(sum[:8])
}

// certsForRevision 证书清单里影响副机的只有名字、域名、到期时间这些(订阅的 SNI 用);续期状态(失败原因、
// 重试时间……)是给主机告警和面板看的,变了不该把整份快照重推给所有副机。
func certsForRevision(raw json.RawMessage) json.RawMessage {
	if len(raw) == 0 {
		return raw
	}
	var list []model.CertBrief
	if json.Unmarshal(raw, &list) != nil {
		return raw
	}
	for i, b := range list {
		list[i] = model.CertBrief{Name: b.Name, Domains: b.Domains, SelfSigned: b.SelfSigned, NotAfter: b.NotAfter, Default: b.Default}
	}
	out, err := json.Marshal(list)
	if err != nil {
		return raw
	}
	return out
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
			out[i].NodeFamily = normJSON(out[i].NodeFamily)
			out[i].Tls = normJSON(out[i].Tls)
			out[i].Transport = normJSON(out[i].Transport)
			out[i].RouteRules = normJSON(out[i].RouteRules)
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
	for _, key := range []string{"allowPrivate", "blockBT"} {
		v, ok := snap.Settings[key]
		if !ok {
			continue
		}
		var old string
		if err := db.Model(&model.Setting{}).Select("value").Where("key = ?", key).Scan(&old).Error; err != nil {
			return false, false, err
		}
		if strings.EqualFold(v, "true") != strings.EqualFold(old, "true") { // 与 render 里同一判法
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
		// 落地页 logo 整表替换。旧版主机不发(nil)也清空:它没有 logo 功能,副机留着的(本机当主机时传的、
		// 之前接过的别的主机推来的)副机上既改不了也删不掉,还可能按代理 id 撞到别家代理的用户头上
		if err := tx.Session(&gorm.Session{AllowGlobalUpdate: true}).Delete(&model.PageLogo{}).Error; err != nil {
			return err
		}
		if len(snap.Logos) > 0 {
			if err := tx.CreateInBatches(&snap.Logos, snapshotBatch).Error; err != nil {
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

// LogosMatch 副机库里的落地页 logo(只看归属与摘要)是不是正好是快照里的这些;快照不带(nil,旧版主机)= 一张都没有。
// 副机判断"这份快照已经应用过、可以直接确认"时用:logo 对不上就整份重新应用。
func LogosMatch(have, want []model.PageLogo) bool {
	if len(have) != len(want) {
		return false
	}
	key := func(l []model.PageLogo) []string {
		out := make([]string, len(l))
		for i, x := range l {
			out[i] = strconv.FormatUint(uint64(x.ResellerId), 10) + ":" + x.Sum
		}
		sort.Strings(out)
		return out
	}
	a, b := key(have), key(want)
	for i := range a {
		if a[i] != b[i] {
			return false
		}
	}
	return true
}

func upsertSetting(tx *gorm.DB, k, v string) error {
	return tx.Clauses(clause.OnConflict{
		Columns:   []clause.Column{{Name: "key"}},
		DoUpdates: clause.AssignmentColumns([]string{"value"}),
	}).Create(&model.Setting{Key: k, Value: v}).Error
}

// RecentConn 一条"源 IP × 线路"入站记录(诊断用,数据面的连接跟踪器记的)。
type RecentConn struct {
	IP       string `json:"ip"`
	User     string `json:"user,omitempty"` // 认证成功的用户名(日志里带 [用户] 的那条)
	Line     string `json:"line"`
	Protocol string `json:"protocol"`
	Count    int    `json:"count"`
	Ts       int64  `json:"ts"`               // 最近一次的绝对时间(unix 秒),由各机按本机时区解析
	Server   string `json:"server,omitempty"` // 主机汇总时标注来自哪台服务器(多服务器时才有)
	NodeId   uint   `json:"nodeId,omitempty"` // 主机汇总时标注来自哪台服务器(服务器 id,单服务器也有)
	Family   string `json:"family,omitempty"` // 主机汇总时按来源 IP 标:v4 / v6
	// Rejected 这条最近一次被拒:device(设备数超限)/ pool(代理设备池已满);放行的为空
	Rejected string `json:"rejected,omitempty"`
	// Failed 只在"到了但没过握手 / 认证"那份里有:tls(TLS / REALITY 握手没过)/ auth(协议认证没过);Detail 是原因原文
	Failed string `json:"failed,omitempty"`
	Detail string `json:"detail,omitempty"`
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
	Certs           []model.CertBrief              `json:"certs,omitempty"`        // 这台的证书库清单(老版本副机不报)
	PublicIP        string                         `json:"publicIp"`               // 副机探测到的公网 IP,主机存入 nodes.public_ip 供订阅使用
	PublicIP6       string                         `json:"publicIp6"`              // 副机探测到的公网 IPv6(没有为空;老版本副机不带,也是空),主机存入 nodes.public_ip6
	Conns           []RecentConn                   `json:"conns,omitempty"`        // 最近入站连接,主机概览汇总展示
	ConnFails       []RecentConn                   `json:"connFails,omitempty"`    // 到了但没过握手 / 认证的来源(老副机不报,老主机不认)
	ConnFailsOK     bool                           `json:"connFailsOk,omitempty"`  // 新版恒为 true。老副机不带:主机据此标出不上报握手失败的服务器(ConnFails 为空时被 omitempty 省掉,单看它分不清)
	Groups          map[string]GroupState          `json:"groups,omitempty"`       // 代理池在这台机器上的状态(在线设备、设备池满被拒次数)
	Upstreams       []UpstreamHealth               `json:"upstreams,omitempty"`    // 本机线路真正用到的那些上游的巡检结果
	Reload          *ReloadState                   `json:"reload,omitempty"`       // 副机最近一次重载的结果,失败要让主机面板看见
	LedgerEpoch     string                         `json:"ledgerEpoch,omitempty"`  // 副机流量账本的纪元(见 LedgerEpochKey);老副机不报
	ServicePorts    []model.ServicePort            `json:"servicePorts,omitempty"` // 副机自己监听的面板 / 订阅端口(老副机不报)
	// 以下几项老副机都不报(零值),主机按老办法处理
	Sequence    uint64 `json:"sequence,omitempty"`    // 副机库里那份快照的推送序号
	KickPending bool   `json:"kickPending,omitempty"` // 还有旧凭据上的连接没踢完(下次应用时接着踢)
	ApplyGzip   bool   `json:"applyGzip,omitempty"`   // 收得了 gzip 压缩的快照;不报的照旧发不压缩的
	LedgerFails int    `json:"ledgerFails,omitempty"` // 流量记账连续失败的轮数
}

// saveNodeCerts 记下副机报上来的证书清单:线路编辑框与订阅的 SNI 用它。老版本副机不报(nil)就不动;
// 没变不写库,否则每 5 秒写一次,快照修订号也跟着变。
func (h *Hub) saveNodeCerts(n model.Node, certs []model.CertBrief) {
	if certs == nil {
		return
	}
	b, err := json.Marshal(certs)
	if err != nil {
		return
	}
	var cur model.Node
	if h.d.DB.Select("id, certs").First(&cur, n.Id).Error != nil || bytes.Equal(cur.Certs, b) {
		return
	}
	if err := h.d.DB.Model(&model.Node{}).Where("id = ?", n.Id).Update("certs", json.RawMessage(b)).Error; err != nil {
		logger.Warning("记录副机 ", n.Name, " 的证书清单失败: ", err)
	}
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
	NoConnFails     bool   `json:"noConnFails,omitempty"` // 这台副机版本较旧,不上报"到了但没过握手 / 认证"(概览里它那份永远是空的,不能当成包没到)
	// LedgerFails 副机自己报的流量记账连续失败轮数:失败期间这段流量只在它的内存里,主机收不到、它一重启就丢
	LedgerFails int `json:"ledgerFails,omitempty"`
	// LedgerRegressAt / LedgerRegressUsers 最近一次发现这台副机的账本回退(计数比上次小)的时间与人数
	LedgerRegressAt    int64 `json:"ledgerRegressAt,omitempty"`
	LedgerRegressUsers int   `json:"ledgerRegressUsers,omitempty"`
	applyGzip          bool  // 副机报过收得了 gzip 压缩的快照
	alerted            bool
	failSince          int64 // 这一轮连续失败从什么时候开始;0 = 正常
	unsyncedSince      int64 // 在线但配置没同步上从什么时候开始;0 = 已同步或不在线
	unsyncedAlerted    bool
	conns              []RecentConn
	connFails          []RecentConn
}

// RemoteConns 汇总所有在线副机最近上报的入站连接,标注服务器名。
func (h *Hub) RemoteConns() []RecentConn {
	return h.remoteConns(func(st *NodeStatus) []RecentConn { return st.conns })
}

// RemoteConnFails 同 RemoteConns,"到了但没过握手 / 认证"的那份。
func (h *Hub) RemoteConnFails() []RecentConn {
	return h.remoteConns(func(st *NodeStatus) []RecentConn { return st.connFails })
}

func (h *Hub) remoteConns(pick func(*NodeStatus) []RecentConn) []RecentConn {
	now := time.Now().Unix()
	h.mu.Lock()
	defer h.mu.Unlock()
	var out []RecentConn
	for id, st := range h.status {
		if !h.freshLocked(id, now) {
			continue
		}
		for _, c := range pick(st) {
			c.Server, c.NodeId = st.Name, id
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
	// OnceDurable 去重记录落库的节流(notify.Notifier.OnceDurable):代副机发的证书告警每台每张每天一次,重启不重复;nil = 不发
	OnceDurable func(key string, ttl time.Duration) bool
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
	// mergeQueue / mergePending 等着落库的各副机结果:网络部分做完就放闸,结果交给唯一的并账协程(单写者)。
	// 每台只留最新一份 —— 账本是累计值,丢掉没来得及并的旧报告不丢流量;队列再长也就是副机台数
	mergeMu      sync.Mutex
	mergeIdle    *sync.Cond
	mergeQueue   []uint
	mergePending map[uint]*nodeResult
	merging      bool
	// pushSem 同时推整份快照的副机数上限(见 pushConcurrency)
	pushSem chan struct{}
	// wire 当前修订编码好的推送字节(见 wireFor);noGzip 压缩包被拒、不压缩的却成了的副机,以后只发不压缩的
	wireMu sync.Mutex
	wire   *wireSnapshot
	noGzip map[uint]bool
	// pushRev / pushSeq 上一次落库的推送修订号与序号:修订号没变就直接用,不再每 5 秒开一笔写事务(见 buildPushSnapshot)
	pushRev string
	pushSeq uint64
	// swept 启动后的第一轮同步清过指向已删副机的游标与纪元(见 SweepOrphanLedgers)
	swept bool
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

// clientTimeout 发往副机的 HTTP 客户端的总时限,只是兜底:每个请求都带着自己的时限(推送快照按大小放宽,最长 applyTimeoutMax)。
const clientTimeout = 6 * time.Minute

func New(d Deps) *Hub {
	ctx, cancel := context.WithCancel(context.Background())
	return &Hub{d: d, status: map[uint]*NodeStatus{}, pushed: map[uint]string{}, remote: map[uint]map[string][]string{}, remoteAt: map[uint]int64{},
		remoteLines: map[uint]map[string]map[string][]string{}, nodeNames: map[uint]string{}, stop: make(chan struct{}), rejects: map[string][]rejectAt{},
		upHealth: map[uint][]UpstreamHealth{}, kicking: map[string]*kickCall{}, gates: map[uint]chan struct{}{}, ipsBusy: map[uint]bool{},
		ctx: ctx, cancel: cancel,
		verified: &http.Client{Timeout: clientTimeout, Transport: tempCertExplained{http.DefaultTransport}}, pinned: map[string]*http.Client{}, pushFail: map[uint]*pushFailure{},
		mergePending: map[uint]*nodeResult{}, pushSem: make(chan struct{}, pushConcurrency)}
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
	h.waitMerged()  // 并账协程看到停机就丢下还没并的(账本是累计值,下次启动后的第一份报告全补上)
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
	snap.Version, snap.MinNode = h.d.Version, minNodeFor(snap.Lines, snap.Upstreams, snap.Exts)
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

// nodeResult 一台副机这一轮同步的结果:网络部分各台各跑,落库部分交给唯一的并账协程串行做(见 enqueueResult)。
type nodeResult struct {
	pushErr string // 这一轮推送没成功(退避中也算),但报告照样拉到了
	n       model.Node
	rep     Report
	err     string // 非空 = 这一轮失败(推送或拉报告)
}

// tick 一轮定时同步:按当前库构造快照,给每台副机各发一轮请求就返回,不等任何一台回来。
// 以前整轮等所有副机:一台黑洞 / 挂起的副机(推送、拉报告、下发设备租约各 25 秒超时)把所有健康副机的
// 同步周期从 5 秒拖到 75 秒左右 —— 停用、换凭据、超额停用在每台机器上都晚一分多钟生效。
// 现在上一轮对某台的请求还在途,这一轮就跳过它;网络部分一做完就放闸,结果交给并账协程排队落库。
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
	if !h.swept {
		if err := SweepOrphanLedgers(h.d.DB); err != nil {
			logger.Warning("清理已删副机留下的游标与账本纪元失败: ", err)
		} else {
			h.swept = true
		}
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
			if r := h.syncGated(n); r != nil {
				h.enqueueResult(r)
			}
		}(n)
	}
	h.forgetNodes(live)
	h.retryDecommissions()
	h.distributeIPs(nodes)
	// 代理池被拒次数:主机本机这一轮的;各副机的在 applyResult 里随报告记
	if h.d.LocalGroups != nil {
		h.recordRejects(h.d.LocalGroups())
	}
}

// syncGated 在这台副机的闸里做网络部分(推送 → 拉报告),返回时闸已经放开。
// 以前并账也在闸里做:落库排队(别的副机的大事务占着)时闸一直被拿着,停用 / 删除时的下线推送等满 25 秒就放弃(审计 I4)。
func (h *Hub) syncGated(n model.Node) (r *nodeResult) {
	defer h.releaseNode(n.Id)
	defer func() {
		if v := recover(); v != nil {
			logger.Warning("同步副机 ", n.Name, " 异常: ", v, " | ", string(debug.Stack()))
			r = nil
		}
	}()
	// 同一台机器内部仍是 推送 → 拉报告 的顺序。拿到闸之后再确认它还启用着:停用 / 删除时推的空用户表
	// 不能被这一轮在途的定时推送盖回去
	if !h.stillEnabled(n.Id) {
		return nil
	}
	return h.syncNode(n, h.latestSnapshot())
}

// enqueueResult 把一台副机这一轮的结果交给并账协程。同一台还没并的旧结果直接换成新的。
func (h *Hub) enqueueResult(r *nodeResult) {
	h.mergeMu.Lock()
	if h.mergePending == nil {
		h.mergePending = map[uint]*nodeResult{}
	}
	if _, queued := h.mergePending[r.n.Id]; !queued {
		h.mergeQueue = append(h.mergeQueue, r.n.Id)
	}
	h.mergePending[r.n.Id] = r
	start := !h.merging
	h.merging = true
	h.mergeMu.Unlock()
	if start {
		go h.mergeLoop()
	}
}

// mergeLoop 唯一的并账协程(单写者):队列空了就退出,下一份结果来了再起。
func (h *Hub) mergeLoop() {
	for {
		h.mergeMu.Lock()
		if len(h.mergeQueue) == 0 || h.baseCtx().Err() != nil {
			h.mergeQueue, h.mergePending = nil, map[uint]*nodeResult{}
			h.merging = false
			h.mergeCond().Broadcast()
			h.mergeMu.Unlock()
			return
		}
		id := h.mergeQueue[0]
		h.mergeQueue = h.mergeQueue[1:]
		r := h.mergePending[id]
		delete(h.mergePending, id)
		h.mergeMu.Unlock()
		func() {
			defer func() {
				if v := recover(); v != nil {
					logger.Warning("并入副机 ", r.n.Name, " 的报告异常: ", v, " | ", string(debug.Stack()))
				}
			}()
			h.applyResult(r)
		}()
	}
}

// mergeCond 并账协程退出时广播(调用方持有 mergeMu)。
func (h *Hub) mergeCond() *sync.Cond {
	if h.mergeIdle == nil {
		h.mergeIdle = sync.NewCond(&h.mergeMu)
	}
	return h.mergeIdle
}

// waitMerged 等并账队列清空、并账协程退出。
func (h *Hub) waitMerged() {
	h.mergeMu.Lock()
	for h.merging {
		h.mergeCond().Wait()
	}
	h.mergeMu.Unlock()
}

// applyResult 一台副机这一轮的结果落库、更新状态。只在并账协程里(或测试里)调用,dbMu 再兜一层串行。
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
	mr, err := MergeCounters(h.d.DB, r.n.Id, r.n.Name, r.rep.LedgerEpoch, r.rep.Counters, time.Now().Unix(), bucket, r.n.Ratio)
	if mr.NodeGone {
		return // 副机在这一轮途中被删了:报告整份丢掉,也不再给它记状态
	}
	if err != nil {
		logger.Warning("并入副机 ", r.n.Name, " 流量失败: ", err)
		var dup *DuplicateLedgerError
		if errors.As(err, &dup) { // 要让管理员看见:这台的流量没在计
			errStr = strings.TrimPrefix(errStr+";"+err.Error(), ";")
		}
	}
	if mr.Merged > 0 && h.d.CountersMerged != nil {
		h.d.CountersMerged() // 中途失败也可能已经并进去几块:照样判一次超量
	}
	if mr.Regressed > 0 {
		st := h.getStatus(r.n)
		h.mu.Lock()
		st.LedgerRegressAt, st.LedgerRegressUsers = time.Now().Unix(), mr.Regressed
		h.mu.Unlock()
	}
	h.resyncOnVersionChange(r.n, r.rep.Version) // 要在 setStatus 记下新版本号之前比
	h.setStatus(r.n, true, errStr, &r.rep)
	if r.rep.PublicIP != "" && r.rep.PublicIP != r.n.PublicIP {
		h.d.DB.Model(&model.Node{}).Where("id = ?", r.n.Id).Update("public_ip", r.rep.PublicIP)
	}
	if r.rep.PublicIP6 != r.n.PublicIP6 {
		h.d.DB.Model(&model.Node{}).Where("id = ?", r.n.Id).Update("public_ip6", r.rep.PublicIP6)
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

// resyncOnVersionChange 副机换了版本(升级 / 降级)就当它没推过、下一轮立即重推一次,同一修订的推送退避也清掉。
// 旧版收快照时会丢掉自己不认识的字段(比如落地页 logo)、修订号却照记;新版那边只有"本版程序完整应用过的"
// 才直接确认,所以这一推会整份应用。不重推的话要等 10 分钟一次的定期重推,这期间服务器页还显示已同步;
// 因为版本过低被拒而在退避的,升级后也不必再等退避。
func (h *Hub) resyncOnVersionChange(n model.Node, version string) {
	h.mu.Lock()
	defer h.mu.Unlock()
	st := h.status[n.Id]
	if st == nil || st.Version == "" || version == "" || st.Version == version {
		return
	}
	delete(h.pushed, n.Id)
	delete(h.pushFail, n.Id)
	logger.Info("副机 ", n.Name, " 的版本从 ", st.Version, " 变为 ", version, ",下一轮重推一次配置")
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

// pushFailure 某台副机对某个修订的连续推送失败记录(只记"拒绝应用"和"连上了但等确认超时",不记失联)。
type pushFailure struct {
	rev   string
	n     int       // 连续失败次数
	at    time.Time // 最近一次失败
	cause string
	slow  bool // 最近一次是等确认超时:副机可能还在应用,看报告确认(见 confirmByReport)
}

// slowPushBackoffMax 等确认超时的退避上限:副机多半只是慢,不像被拒那样等到 5 分钟。
const slowPushBackoffMax = time.Minute

// backoff 下一次重推至少要隔多久。
func (p *pushFailure) backoff() time.Duration {
	d := pushBackoff(p.n)
	if p.slow && d > slowPushBackoffMax {
		d = slowPushBackoffMax
	}
	return d
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
	pushedRev, lastPush, nodeRev := h.pushed[n.Id], st.LastPush, st.Revision
	pf := h.pushFail[n.Id]
	h.mu.Unlock()
	res := &nodeResult{n: n}
	// 副机报告的修订号和要推的不一样也要推:推一份新配置失败后(副机库里已经是那份失败的修订、数据面回滚了),
	// 管理员把改动撤回,内容回到上一次推成功的那份,修订号(内容哈希)也就一样 —— 只比 pushed 会以为副机早已是它,
	// 一直不推,副机库里留着撤掉的线路、待重载不清、一直未同步,要等下面 10 分钟一次的定期重推。
	// 同一修订连续失败的退避照旧(下面按 pushFail 判),不会因此每 5 秒去拆一次副机的数据面。
	behind := nodeRev != "" && nodeRev != snap.Revision
	if pushedRev != snap.Revision || behind || time.Now().Unix()-lastPush > 600 {
		switch {
		case pf != nil && pf.rev == snap.Revision && time.Since(pf.at) < pf.backoff():
			res.pushErr = fmt.Sprintf("推送失败 %d 次,%s 后再试: %s", pf.n, (pf.backoff() - time.Since(pf.at)).Round(time.Second), pf.cause)
		default:
			if err := h.push(n, snap); err != nil {
				// 等确认超时也退避:以前超时不算被拒,下一轮又整份重推,副机永远在重做同一份(审计 I4)
				if slow := isApplyTimeout(err); slow || pushRejected(err) {
					h.mu.Lock()
					if pf == nil || pf.rev != snap.Revision {
						pf = &pushFailure{rev: snap.Revision}
						h.pushFail[n.Id] = pf
					}
					pf.n++
					pf.at, pf.cause, pf.slow = time.Now(), err.Error(), slow
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
	if h.confirmByReport(n, snap, rep) {
		res.pushErr = ""
	}
	return res
}

// confirmByReport 推送等确认超时了(或正为此退避):报告说副机已经在这个修订上、序号不旧、没有待重载也没有待踢线,
// 它其实应用完了 —— 记为已推送,不再整份重推。返回是否这样确认了。
func (h *Hub) confirmByReport(n model.Node, snap Snapshot, rep Report) bool {
	h.mu.Lock()
	pf := h.pushFail[n.Id]
	ok := pf != nil && pf.slow && pf.rev == snap.Revision && reportApplied(rep, snap)
	if ok {
		delete(h.pushFail, n.Id)
	}
	h.mu.Unlock()
	if ok {
		logger.Info("副机 ", n.Name, " 回确认超时了,但报告显示它已经应用完 ", snap.Revision)
		h.markPushed(n, snap.Revision)
	}
	return ok
}

// reportApplied 副机报告里的状态是不是"这份快照已经应用完"。老副机不报序号和待踢线,只看修订号与待重载。
func reportApplied(rep Report, snap Snapshot) bool {
	return rep.Revision == snap.Revision && !rep.ReloadPending && !rep.KickPending && (rep.Sequence == 0 || rep.Sequence >= snap.Sequence)
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
		st.LedgerFails, st.applyGzip = rep.LedgerFails, rep.ApplyGzip
		h.saveNodeCerts(n, rep.Certs)
		h.saveNodeServicePorts(n, rep.ServicePorts)
		// hubRevision 在副机 ApplySnapshot 的事务里就写了,数据面有没有真的应用成功要看 ReloadPending。
		// 不看它,一台新配置起不来、正在跑回滚旧配置的副机会显示"在线 · 已同步",管理员看不出任何异常。
		st.Synced = rep.Revision == h.revision && !rep.ReloadPending
		if rep.ReloadPending && st.Error == "" {
			st.Error = "配置已收到,但这台副机的数据面没应用成功(仍在跑上一份可用配置),看副机日志"
		}
		st.OnlineUsers = len(rep.Onlines)
		st.conns, st.connFails = rep.Conns, rep.ConnFails
		st.NoConnFails = !rep.ConnFailsOK
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
		if ok && rep != nil {
			for _, m := range h.nodeCertAlerts(n, rep.Certs) {
				h.d.Notify("tgOnCert", m)
			}
		}
	}
}

// nodeCertAlerts 副机报上来的证书里要主机出面的:自动续期在失败的、正用着又不会自动续的快到期了(判断见
// model.CertBrief.Trouble)。副机一般没配通知(通知设置不随快照同步),续期失败以前只写进副机自己的日志,
// 没人知道;现在主机用自己的通知渠道代它说。去重记录落库:每台每张每天一次,重启不重复。
func (h *Hub) nodeCertAlerts(n model.Node, certs []model.CertBrief) []string {
	if h.d.OnceDurable == nil {
		return nil
	}
	now := time.Now()
	var msgs []string
	for _, c := range certs {
		trouble := c.Trouble(now)
		if trouble == "" || !h.d.OnceDurable(fmt.Sprintf("node-cert:%d:%s:%s", n.Id, c.Name, trouble), 23*time.Hour) {
			continue
		}
		node, name, left := alertText(n.Name, 64), alertText(c.Name, 128), notify.LeftText(time.Unix(c.NotAfter, 0).Sub(now))
		switch {
		case trouble == model.CertFailing:
			msgs = append(msgs, fmt.Sprintf("🔴 <b>副机证书自动续期失败</b>:%s 的 %s %s,已连续失败 %d 次\n%s", node, name, left, c.RenewFails, alertText(c.RenewErr, 300)))
		case c.Source == "acme" && c.Method == "manual":
			msgs = append(msgs, fmt.Sprintf("🟡 <b>副机证书要手动续期</b>:%s 的 %s %s。它用手动 DNS 验证,不能自动续期:到那台服务器的证书页点「续期」,按提示加 TXT 记录", node, name, left))
		case c.Restored:
			msgs = append(msgs, fmt.Sprintf("🟡 <b>副机证书快到期</b>:%s 的 %s %s。它是那台换机还原时从备份放进来的副本,没有谁续它:"+
				"到那台服务器的证书页签一张新的,或者把它上面 certbot 的证书作为「已有证书」添加进来,再换过去", node, name, left))
		default:
			msgs = append(msgs, fmt.Sprintf("🟡 <b>副机证书快到期</b>:%s 的 %s %s,自动续期管不到它:到那台服务器的证书页看看", node, name, left))
		}
	}
	return msgs
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

// push 把一份快照推给一台副机并等它确认(调用方持有这台的闸)。同一修订、同一序号的快照只编码一次,各台共用。
func (h *Hub) push(n model.Node, snap Snapshot) error {
	w, err := h.wireFor(snap)
	if err != nil {
		return err
	}
	return h.pushWire(n, w)
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
	// 拿到闸之后再确认它仍是启用的:停用时已经给它推过一份空用户表(Decommission),这里再推完整快照
	// 就把服务重新打开了 —— 面板上对停用的副机点「推送」、或停用前一刻开始的「立即同步全部」都会走到这里。
	if !h.stillEnabled(n.Id) {
		return ErrNodeDisabled
	}
	return h.push(n, h.latestSnapshot())
}

// ErrNodeDisabled 停用的副机不再推送配置(停用时已撤下它的用户表,推完整快照等于重新开放服务)。
var ErrNodeDisabled = errors.New("这台副机已停用,不推送配置(停用时已撤下它的用户表);要恢复服务请先启用")

// buildPushSnapshot 在 syncMu 内调用。序号持久化，进程重启后也不会重用旧序号。
func (h *Hub) buildPushSnapshot() (Snapshot, error) {
	snap, err := BuildSnapshot(h.d.DB, h.d.Setting)
	if err != nil {
		return snap, err
	}
	// 修订号没变、序号已经就绪:直接用内存里记的,不开事务。以前每 5 秒都开一笔 BEGIN IMMEDIATE 读序号,
	// 别的写事务占锁超过 10 秒时这一轮"构造同步快照失败",推送、拉报告、设备租约整轮跳过(审计 I4)
	h.mu.Lock()
	primed, lastRev, lastSeq := h.sequencePrimed, h.pushRev, h.pushSeq
	h.mu.Unlock()
	if primed && lastSeq != 0 && lastRev == snap.Revision {
		snap.Sequence = lastSeq
		return snap, nil
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
		if raw != "" && previousRevision == snap.Revision && primed {
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
		h.mu.Lock()
		h.sequencePrimed, h.pushRev, h.pushSeq = true, snap.Revision, snap.Sequence
		h.mu.Unlock()
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
			if err := h.pushLatest(n); err != nil && !errors.Is(err, ErrNodeDisabled) { // 中途被停用的不再算它
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

// agentURL 副机 agent 接口的地址。
func agentURL(n model.Node, path string) string {
	base := strings.TrimRight(n.ApiUrl, "/")
	if !strings.HasSuffix(base, "/api") {
		base += "/api"
	}
	return base + "/agent/" + path
}

func (h *Hub) requestTimeout(n model.Node, method, path string, body interface{}, out interface{}, timeout time.Duration) error {
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
	req, err := http.NewRequestWithContext(ctx, method, agentURL(n, path), rd)
	if err != nil {
		return err
	}
	return h.do(n, req, out)
}

// do 带上令牌发请求,非 200 归类成 httpStatusError,200 的响应体解进 out。
func (h *Hub) do(n model.Node, req *http.Request, out interface{}) error {
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
	key := fmt.Sprintf("%d:%s:%s", n.Id, n.CertFP, n.Domain)
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
	// API 地址填的是 IP 时,副机的证书多半是给服务器域名签的(Let's Encrypt 不给 IP 签):按 IP 校验必然失败,
	// 副机一续期主机就报"指纹变了"失联,看到的证书天数从此不再更新。这时也按服务器的域名校验
	names := []string{host}
	if d := strings.TrimSpace(n.Domain); net.ParseIP(host) != nil && d != "" {
		names = append(names, d)
	}
	c := &http.Client{Timeout: clientTimeout, Transport: &http.Transport{TLSClientConfig: &tls.Config{
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
			if got != want && h.chainValidFor(raw, names) {
				// 正规 CA 自动续期换了证书:新证书能按 API 地址的主机名过根证书校验,就换记新指纹。以前每次续期都失联,
				// 得手点「重置指纹」(审计 M063)。自签证书换了照旧拒绝 —— 那正是钉指纹要防的
				h.d.DB.Model(&model.Node{}).Where("id = ? AND cert_fp = ?", id, want).Update("cert_fp", got)
				logger.Info("副机 ", name, " 的证书已由正规 CA 续期,指纹换记为 ", got[:16])
				return nil
			}
			if got != want && tempCertPresented(raw) {
				// 副机的证书文件读不出来(换机还原后不在、被删……),面板正用临时自签证书顶着:说清楚是这个原因,
				// 不让人以为是重签了证书或者中间人。临时证书存在副机数据目录里、重启不变,急着先连上可以重置指纹信任它
				return fmt.Errorf("副机的证书文件读不出来,正用临时自签证书顶着,和记住的指纹(%s…)对不上:打开副机面板看顶部提示,"+
					"把证书文件放回原处(放回后自动恢复连接)或者换一张证书;急着先连上,可以到服务器页点「重置指纹」信任这张临时证书(它在副机重启后不变)", want[:16])
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

// tempCertPresented 副机出示的是不是 m-ui 的临时自签证书(它的证书文件读不出来时顶上的那张,见 certutil.IsTempCert)。
func tempCertPresented(raw [][]byte) bool {
	if len(raw) == 0 {
		return false
	}
	c, err := x509.ParseCertificate(raw[0])
	return err == nil && certutil.IsTempCert(c)
}

// tempCertExplained 正常校验证书的副机(没勾"跳过证书校验")出示的是临时自签证书时,校验必然失败,原来的报错只有
// "x509: certificate signed by unknown authority",看不出是副机的证书文件读不出来了。这里换成说清原因的错误,原错误接在后面。
type tempCertExplained struct{ base http.RoundTripper }

func (t tempCertExplained) RoundTrip(req *http.Request) (*http.Response, error) {
	resp, err := t.base.RoundTrip(req)
	var ve *tls.CertificateVerificationError
	if err != nil && errors.As(err, &ve) && len(ve.UnverifiedCertificates) > 0 && certutil.IsTempCert(ve.UnverifiedCertificates[0]) {
		return nil, fmt.Errorf("副机的证书文件读不出来,正用临时自签证书顶着,校验不过:打开副机面板看顶部提示,把证书文件放回原处(放回后自动恢复连接)或者换一张证书(%w)", err)
	}
	return resp, err
}

// CloseIdleConnections 让 http.Client.CloseIdleConnections 照样关到底层的连接。
func (t tempCertExplained) CloseIdleConnections() {
	if c, ok := t.base.(interface{ CloseIdleConnections() }); ok {
		c.CloseIdleConnections()
	}
}

// chainValidFor 证书链能按其中任一个名字过根证书校验就算(空名字跳过)。
func (h *Hub) chainValidFor(raw [][]byte, names []string) bool {
	for _, n := range names {
		if n != "" && h.chainValid(raw, n) {
			return true
		}
	}
	return false
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

// saveNodeServicePorts 记下副机报上来的服务端口:主机建线路 / 改部署范围 / 启用线路时按它避让。老副机不报(nil)就不动;没变不写库。
func (h *Hub) saveNodeServicePorts(n model.Node, ports []model.ServicePort) {
	if ports == nil {
		return
	}
	b, err := json.Marshal(ports)
	if err != nil {
		return
	}
	var cur model.Node
	if h.d.DB.Select("id, service_ports").First(&cur, n.Id).Error != nil || bytes.Equal(cur.ServicePorts, b) {
		return
	}
	if err := h.d.DB.Model(&model.Node{}).Where("id = ?", n.Id).Update("service_ports", json.RawMessage(b)).Error; err != nil {
		logger.Warning("记录副机 ", n.Name, " 的服务端口失败: ", err)
	}
}
