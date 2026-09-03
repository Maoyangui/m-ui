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
	"crypto/sha256"
	"crypto/tls"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net/http"
	"sort"
	"strings"
	"sync"
	"time"

	"github.com/Maoyangui/m-ui/database/model"
	"github.com/Maoyangui/m-ui/logger"

	"gorm.io/gorm"
	"gorm.io/gorm/clause"
)

// SyncedSettings 是需要在主副机之间保持一致的设置项(订阅展示相关)。
var SyncedSettings = []string{
	"timezone",
	"upstreamTestUrl", "subProfileTitle", "subEncode", "subShowNotice", "subClashExt", "subUpdates",
	"subPageEnabled", "subPageTitle", "subPageSupport", "subPageNotice",
}

// Snapshot 是主机下发给副机的完整配置。
type Snapshot struct {
	Revision   string            `json:"revision"`
	SelfNodeId uint              `json:"selfNodeId"` // 接收方在 nodes 表里的 id
	MasterId   uint              `json:"masterId"`   // 主机自己在 nodes 表里的 id
	Nodes      []model.Node      `json:"nodes"`
	Upstreams  []model.Upstream  `json:"upstreams"`
	Lines      []model.Line      `json:"lines"`
	Users      []model.User      `json:"users"`
	UserLines  []model.UserLine  `json:"userLines"`
	Exts       []model.ExtNode   `json:"exts"`
	UserExts   []model.UserExt   `json:"userExts"`
	Settings   map[string]string `json:"settings"`
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
	if err := db.Order("user_id asc, line_id asc").Find(&s.UserLines).Error; err != nil {
		return s, err
	}
	if err := db.Order("sort asc, id asc").Find(&s.Exts).Error; err != nil {
		return s, err
	}
	if err := db.Order("user_id asc, ext_id asc").Find(&s.UserExts).Error; err != nil {
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
	s.Revision, s.SelfNodeId = "", 0
	// nodes 表中的 IsLocal 因接收方不同而不同,不参与修订号
	nodes := make([]model.Node, len(s.Nodes))
	copy(nodes, s.Nodes)
	for i := range nodes {
		nodes[i].IsLocal = false
	}
	s.Nodes = nodes
	b, _ := json.Marshal(s)
	sum := sha256.Sum256(b)
	return hex.EncodeToString(sum[:8])
}

// ApplySnapshot 在副机上整表替换配置。返回线路、上游是否变化,副机据此选择重载级别:
// 线路变 → 全量重载;仅上游变 → 热换出站;都没变 → 热换用户。
func ApplySnapshot(db *gorm.DB, snap Snapshot) (linesChanged, upstreamsChanged bool, err error) {
	var oldLines []model.Line
	var oldUps []model.Upstream
	db.Order("id asc").Find(&oldLines)
	db.Order("id asc").Find(&oldUps)
	ol, _ := json.Marshal(oldLines)
	nl, _ := json.Marshal(snap.Lines)
	ou, _ := json.Marshal(oldUps)
	nu, _ := json.Marshal(snap.Upstreams)
	linesChanged = !bytes.Equal(ol, nl)
	upstreamsChanged = !bytes.Equal(ou, nu)

	// gorm 对带 default:true 的 bool 字段:Create 时零值 false 会写成默认 true,并把 true 回填进结构体。
	// 所以要在插入之前记下被禁用的 id,插入后再显式写回 false,否则主机禁用的用户会在副机上"复活"。
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
		for _, t := range []interface{}{&model.UserLine{}, &model.UserExt{}, &model.User{}, &model.Line{}, &model.Upstream{}, &model.Node{}, &model.ExtNode{}} {
			if err := tx.Session(&gorm.Session{AllowGlobalUpdate: true}).Delete(t).Error; err != nil {
				return err
			}
		}
		if len(snap.Exts) > 0 {
			if err := tx.Create(&snap.Exts).Error; err != nil {
				return err
			}
		}
		if len(snap.UserExts) > 0 {
			if err := tx.Create(&snap.UserExts).Error; err != nil {
				return err
			}
		}
		for i := range snap.Nodes {
			snap.Nodes[i].IsLocal = snap.Nodes[i].Id == snap.SelfNodeId
		}
		if len(snap.Nodes) > 0 {
			if err := tx.Create(&snap.Nodes).Error; err != nil {
				return err
			}
		}
		if len(snap.Upstreams) > 0 {
			if err := tx.Create(&snap.Upstreams).Error; err != nil {
				return err
			}
		}
		if len(snap.Lines) > 0 {
			if err := tx.Create(&snap.Lines).Error; err != nil {
				return err
			}
		}
		if len(snap.Users) > 0 {
			if err := tx.Create(&snap.Users).Error; err != nil {
				return err
			}
		}
		if len(snap.UserLines) > 0 {
			if err := tx.Create(&snap.UserLines).Error; err != nil {
				return err
			}
		}
		if len(offUsers) > 0 {
			if err := tx.Model(&model.User{}).Where("id IN ?", offUsers).Update("enabled", false).Error; err != nil {
				return err
			}
		}
		if len(offLines) > 0 {
			if err := tx.Model(&model.Line{}).Where("id IN ?", offLines).Update("enabled", false).Error; err != nil {
				return err
			}
		}
		if len(offNodes) > 0 {
			if err := tx.Model(&model.Node{}).Where("id IN ?", offNodes).Update("enabled", false).Error; err != nil {
				return err
			}
		}
		if len(offExts) > 0 {
			if err := tx.Model(&model.ExtNode{}).Where("id IN ?", offExts).Update("enabled", false).Error; err != nil {
				return err
			}
		}
		for k, v := range snap.Settings {
			upsertSetting(tx, k, v)
		}
		upsertSetting(tx, "hubRevision", snap.Revision)
		upsertSetting(tx, "hubMasterId", fmt.Sprintf("%d", snap.MasterId))
		upsertSetting(tx, "hubAppliedAt", fmt.Sprintf("%d", time.Now().Unix()))
		return nil
	})
	return linesChanged, upstreamsChanged, err
}

func upsertSetting(tx *gorm.DB, k, v string) {
	tx.Clauses(clause.OnConflict{
		Columns:   []clause.Column{{Name: "key"}},
		DoUpdates: clause.AssignmentColumns([]string{"value"}),
	}).Create(&model.Setting{Key: k, Value: v})
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
	Revision        string                         `json:"revision"` // 副机当前已应用的修订号
	Counters        []model.AgentCounter           `json:"counters"`
	Onlines         map[string][]string            `json:"onlines"`                   // 用户 → 在线源 IP
	OnlineLinesByIP map[string]map[string][]string `json:"onlineLinesByIp,omitempty"` // 用户 → 源 IP → 线路名
	OnlineLines     []string                       `json:"onlineLines"`
	CertDays        int                            `json:"certDays"`
	PublicIP        string                         `json:"publicIp"`        // 副机探测到的公网 IP,主机存入 nodes.public_ip 供订阅使用
	Conns           []RecentConn                   `json:"conns,omitempty"` // 最近入站连接,主机概览汇总展示
}

// ApplyCounters 把副机的单调账本按游标并入主机:只计增量;计数器回绕(副机重装)时游标归零重认。
// ratio 为该服务器的流量倍率(≤0 视为 1),增量按倍率计入用户用量与时序。返回并入的用户数。
func ApplyCounters(db *gorm.DB, nodeId uint, nodeName string, counters []model.AgentCounter, now int64, bucketSeconds int64, ratio float64) (int, error) {
	if len(counters) == 0 {
		return 0, nil
	}
	if bucketSeconds < 1 {
		bucketSeconds = 60
	}
	if ratio <= 0 {
		ratio = 1
	}
	bucket := now - now%bucketSeconds
	n := 0
	err := db.Transaction(func(tx *gorm.DB) error {
		for _, c := range counters {
			var cur model.TrafficCursor
			tx.Where("node_id = ? AND user_name = ?", nodeId, c.UserName).First(&cur)
			if c.Up < cur.Up || c.Down < cur.Down {
				cur.Up, cur.Down = 0, 0 // 副机计数器回绕
			}
			dUp, dDown := c.Up-cur.Up, c.Down-cur.Down
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
	alerted     bool
	conns       []RecentConn
}

// RemoteConns 汇总所有在线副机最近上报的入站连接,标注服务器名。
func (h *Hub) RemoteConns() []RecentConn {
	h.mu.Lock()
	defer h.mu.Unlock()
	var out []RecentConn
	for _, st := range h.status {
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
}

type Hub struct {
	d      Deps
	mu     sync.Mutex
	status map[uint]*NodeStatus
	pushed map[uint]string
	remote map[uint]map[string][]string // node → user → ips
	// remoteLines 副机上报的 用户 → 源 IP → 线路名;nodeNames 用于在面板里给线路加服务器后缀
	remoteLines map[uint]map[string]map[string][]string
	nodeNames   map[uint]string
	revision    string
	stop        chan struct{}
	wg          sync.WaitGroup
	clients     map[bool]*http.Client
}

func New(d Deps) *Hub {
	return &Hub{d: d, status: map[uint]*NodeStatus{}, pushed: map[uint]string{}, remote: map[uint]map[string][]string{},
		remoteLines: map[uint]map[string]map[string][]string{}, nodeNames: map[uint]string{}, stop: make(chan struct{}),
		clients: map[bool]*http.Client{
			false: {Timeout: 25 * time.Second},
			true:  {Timeout: 25 * time.Second, Transport: &http.Transport{TLSClientConfig: &tls.Config{InsecureSkipVerify: true}}},
		}}
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
				h.tick()
			case <-h.stop:
				return
			}
		}
	}()
}

func (h *Hub) Stop() {
	close(h.stop)
	h.wg.Wait()
}

// Revision 返回主机当前配置修订号。
func (h *Hub) Revision() string {
	h.mu.Lock()
	defer h.mu.Unlock()
	return h.revision
}

func (h *Hub) remoteNodes() []model.Node {
	var nodes []model.Node
	h.d.DB.Where("enabled = ? AND is_local = ?", true, false).Order("sort asc, id asc").Find(&nodes)
	return nodes
}

func (h *Hub) tick() {
	if h.d.IsNode() {
		return
	}
	snap, err := BuildSnapshot(h.d.DB, h.d.Setting)
	if err != nil {
		logger.Warning("构造同步快照失败: ", err)
		return
	}
	h.mu.Lock()
	h.revision = snap.Revision
	h.mu.Unlock()

	nodes := h.remoteNodes()
	live := map[uint]bool{}
	for _, n := range nodes {
		live[n.Id] = true
		if n.ApiUrl == "" || n.Token == "" {
			h.setStatus(n, false, "未配置 API 地址或令牌", nil)
			continue
		}
		st := h.getStatus(n)
		if h.pushed[n.Id] != snap.Revision || time.Now().Unix()-st.LastPush > 600 {
			if err := h.push(n, snap); err != nil {
				h.setStatus(n, false, "推送失败: "+err.Error(), nil)
				continue
			}
		}
		rep, err := h.fetchReport(n)
		if err != nil {
			h.setStatus(n, false, "拉取报告失败: "+err.Error(), nil)
			continue
		}
		h.setStatus(n, true, "", &rep)
		if rep.PublicIP != "" && rep.PublicIP != n.PublicIP {
			h.d.DB.Model(&model.Node{}).Where("id = ?", n.Id).Update("public_ip", rep.PublicIP)
		}
		bucket := int64(60)
		if v := h.d.Setting("statsBucketSeconds"); v != "" {
			fmt.Sscanf(v, "%d", &bucket)
		}
		if _, err := ApplyCounters(h.d.DB, n.Id, n.Name, rep.Counters, time.Now().Unix(), bucket, n.Ratio); err != nil {
			logger.Warning("并入副机 ", n.Name, " 流量失败: ", err)
		}
		h.mu.Lock()
		h.remote[n.Id] = rep.Onlines
		h.remoteLines[n.Id] = rep.OnlineLinesByIP
		h.nodeNames[n.Id] = n.Name
		h.mu.Unlock()
	}
	h.mu.Lock()
	for id := range h.status {
		if !live[id] {
			delete(h.status, id)
			delete(h.remote, id)
			delete(h.pushed, id)
		}
	}
	h.mu.Unlock()
	h.distributeIPs(nodes)
}

// distributeIPs 把"其他机器上的在线 IP"下发给每台机器(含主机自身)。
func (h *Hub) distributeIPs(nodes []model.Node) {
	h.mu.Lock()
	remote := map[uint]map[string][]string{}
	for id, m := range h.remote {
		remote[id] = m
	}
	h.mu.Unlock()
	var users []model.User
	h.d.DB.Where("device_limit > 0").Find(&users)
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
	// 每台副机:外部 IP = 主机本机 + 其他副机
	for _, n := range nodes {
		if n.ApiUrl == "" || n.Token == "" {
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
		_ = h.request(n, "POST", "external-ips", ext, nil)
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
	if ok {
		st.LastSeen = time.Now().Unix()
	}
	if rep != nil {
		st.Version, st.Hostname, st.CoreRunning, st.Uptime, st.Revision, st.CertDays = rep.Version, rep.Hostname, rep.CoreRunning, rep.Uptime, rep.Revision, rep.CertDays
		st.Synced = rep.Revision == h.revision
		st.OnlineUsers = len(rep.Onlines)
		st.conns = rep.Conns
	}
	if !ok && !alerted && st.LastSeen > 0 && time.Now().Unix()-st.LastSeen > 60 {
		st.alerted = true
		h.mu.Unlock()
		if h.d.Notify != nil {
			h.d.Notify("tgOnCore", "🔴 <b>副机失联</b>:"+n.Name+"\n"+errStr)
		}
		return
	}
	if ok && alerted {
		st.alerted = false
		h.mu.Unlock()
		if h.d.Notify != nil {
			h.d.Notify("tgOnCore", "🟢 <b>副机恢复</b>:"+n.Name)
		}
		return
	}
	_ = wasOK
	h.mu.Unlock()
}

func (h *Hub) push(n model.Node, snap Snapshot) error {
	snap.SelfNodeId = n.Id
	var out struct {
		OK       string `json:"ok"`
		Revision string `json:"revision"`
	}
	if err := h.request(n, "POST", "apply", snap, &out); err != nil {
		return err
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
	snap, err := BuildSnapshot(h.d.DB, h.d.Setting)
	if err != nil {
		return err
	}
	return h.push(n, snap)
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
func (h *Hub) RemoteIPs(user string) []string {
	h.mu.Lock()
	defer h.mu.Unlock()
	set := map[string]bool{}
	for _, m := range h.remote {
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
func (h *Hub) RemoteIPLines(user string) map[string][]string {
	h.mu.Lock()
	defer h.mu.Unlock()
	out := map[string][]string{}
	for id, m := range h.remoteLines {
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
	h.mu.Lock()
	defer h.mu.Unlock()
	set := map[string]bool{}
	for _, m := range h.remote {
		for u, ips := range m {
			if len(ips) > 0 {
				set[u] = true
			}
		}
	}
	return keys(set)
}

func (h *Hub) request(n model.Node, method, path string, body interface{}, out interface{}) error {
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
	ctx, cancel := context.WithTimeout(context.Background(), 25*time.Second)
	defer cancel()
	req, err := http.NewRequestWithContext(ctx, method, base+"/agent/"+path, rd)
	if err != nil {
		return err
	}
	req.Header.Set("X-Agent-Token", n.Token)
	req.Header.Set("Content-Type", "application/json")
	resp, err := h.clients[n.Insecure].Do(req)
	if err != nil {
		return err
	}
	defer resp.Body.Close()
	b, _ := io.ReadAll(io.LimitReader(resp.Body, 64<<20))
	if resp.StatusCode != 200 {
		var e struct{ Error string }
		json.Unmarshal(b, &e)
		if e.Error == "" {
			e.Error = strings.TrimSpace(string(b))
		}
		return fmt.Errorf("HTTP %d: %s", resp.StatusCode, e.Error)
	}
	if out != nil {
		if err := json.Unmarshal(b, out); err != nil {
			return errors.New("响应不是 JSON(API 地址是否指向面板路径,如 https://tw:2053/ad/?)")
		}
	}
	return nil
}
