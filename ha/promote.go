package ha

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"sort"
	"strconv"
	"strings"
	"time"

	"github.com/Maoyangui/m-ui/database"
	"github.com/Maoyangui/m-ui/database/model"
	"github.com/Maoyangui/m-ui/hub"
	"github.com/Maoyangui/m-ui/logger"
	"github.com/Maoyangui/m-ui/selfupdate"

	"gorm.io/gorm"
)

// 接任为主机(主机失联时备用接手)和被设为主机(计划内换主机)最后都落到同一件事:
// 用原主机的数据(副本 / 整库)组一个新库,换上本机自己的本机设置、证书库,套用原主机的身份(订阅地址),
// 服务器表里本机那行标成"本机"、原主机那行填上"作为副机时怎么连",新纪元(NextEpoch)写进新库;写成 <库>.promote(带时效),重启时换上。
// 新库换上之前,本机当前库里的纪元与角色一概不动。
//
// 换库发生在下次启动、还没打开库的时候(ApplyPending):当前库连同 -wal / -shm 改名为 <库>.pre-promote-<时间> 留着,
// 新库换上。之后它就是主机,承担全部主机职能(配额与到期、合账、订阅、代理面板、告警、备份、证书续期、
// Telegram 机器人、外部 API),可以长期担任,没有时限、不降级。

// 成为主机的方式。
const (
	HowTakeover  = "takeover"   // 主机失联,备用接任
	HowSetMaster = "set-master" // 原主机上点了「设为主机」
)

// Became 本机怎么成为主机的(本机设置 haBecame;面板的角色卡片、通知、改 DNS 用)。
type Became struct {
	How         string         `json:"how"`
	Epoch       uint64         `json:"epoch"`
	FormerId    uint           `json:"formerId"`
	FormerName  string         `json:"formerName"`
	At          int64          `json:"at"`
	ReplicaAt   int64          `json:"replicaAt,omitempty"` // 接任所用副本的时间:数据截至
	MaxChangeId uint64         `json:"maxChangeId,omitempty"`
	By          string         `json:"by,omitempty"` // panel | telegram | cli | auto
	Identity    identityResult `json:"identity"`
	FormerIP    string         `json:"formerIp,omitempty"`
	FormerIP6   string         `json:"formerIp6,omitempty"`
	Notified    bool           `json:"notified,omitempty"`
	Dns         []DnsChange    `json:"dns,omitempty"`
	DnsErr      string         `json:"dnsErr,omitempty"`
}

// Former 接任后的主机:失联的原主机(等它回来自动变为副机;那之前它那一行停用,订阅里不出现死节点)。
type Former struct {
	Id          uint   `json:"id"`
	Name        string `json:"name"`
	Since       int64  `json:"since"`
	SinceChange uint64 `json:"sinceChange,omitempty"` // 接任所用数据里最大的审计 id:原主机回来时据此数它失联期间多收的改动
}

// promoteFile 待换上的新库。
func promoteFile(dbPath string) string { return dbPath + ".promote" }

// TakeoverOpts 接任的参数。
type TakeoverOpts struct {
	Force bool   // 不管主机最近是不是还联系过本机、是不是暂停中、失联判定走到哪一步(命令行 --force)
	By    string // panel | telegram | cli | auto
	// Expect 发起时以为的新纪元(Telegram 确认码绑着它):纪元在这期间变了就不接任。0 = 不核对
	Expect uint64
}

// errTakeoverBusy 已经在接任 / 设为主机了。
var errTakeoverBusy = errors.New("正在接任或换主机,稍等")

// promoteTTL 写好的新库多久没换上就作废(重启迟迟没发生:那时集群多半已经变了,不能再拿它当主机)。
var promoteTTL = 10 * time.Minute

// takeoverDiskFree 接任前查的剩余空间(测试里换掉)。
var takeoverDiskFree = func(s *Service) int64 { return s.diskFree() }

// Takeover 本机(备用)接任为主机:用手里的数据副本组好新库(.promote,带时效),重启后以新纪元、主机身份启动。
// 新库落盘之前本机的纪元与角色一概不动:中途失败或崩溃,本机还是原来那台副机,以后照样能接任,别的备用也不会被"更高纪元"挡住。
func (s *Service) Takeover(o TakeoverOpts) error {
	if !s.promoteMu.TryLock() {
		return errTakeoverBusy
	}
	defer s.promoteMu.Unlock()
	if !s.IsNode() {
		return errors.New("本机已经是主机")
	}
	if s.setting(KeyRole) == RolePromoting {
		return errors.New("本机已经在接任了,等它重启")
	}
	st, _ := s.NodeState()
	E := s.Epoch()
	if !o.Force {
		if st.Mode == ModeOff {
			return errors.New("主机容灾没有开启:要接任请用命令行 m-ui ha takeover --force")
		}
		if st.rank() == 0 {
			return errors.New("本机不是备用,手里没有数据副本")
		}
		if ago := s.LeaseAgo(); ago < SuspectAfter {
			return fmt.Errorf("主机 %d 秒前还联系过本机,看起来没有失联:确实要接任请用命令行 m-ui ha takeover --force", int(ago.Seconds()))
		}
		if st.PauseUntil > s.now().Unix() {
			return errors.New("主机暂停了接任(升级、重启或手动暂停中):等暂停结束,或用命令行 m-ui ha takeover --force")
		}
		if o.By != "auto" {
			if ok, why := s.takeoverVerdict(); !ok {
				return takeoverRefused(why)
			}
		}
	}
	fail := func(err error) error {
		s.noteTakeoverErr()
		s.dropOwnVote()
		return err
	}
	ri, ok := s.replicaInfo()
	if !ok {
		return fail(errors.New("本机没有数据副本:主机还没把副本推过来,或本机不是备用"))
	}
	if err := s.replicaUsable(ri); err != nil {
		return fail(err)
	}
	need := TakeoverNeed(ri.Size, ri.DBSize)
	if free := takeoverDiskFree(s); free >= 0 && free < need {
		return fail(fmt.Errorf("本机磁盘剩余 %d MB,接任至少要 %d MB:先腾出空间再接任", free>>20, need>>20))
	}
	if !o.Force {
		if err := s.peersAllowTakeover(E); err != nil {
			return err
		}
	}
	if err := ensureDir(s.Dir()); err != nil {
		return fail(err)
	}
	dir, err := os.MkdirTemp(s.Dir(), "takeover-")
	if err != nil {
		return fail(err)
	}
	defer os.RemoveAll(dir)
	meta, env, which, err := s.openReplica(dir)
	if err != nil {
		return fail(err)
	}
	if env.Header.Epoch < E && !o.Force {
		// 本机后来跟过一台更新的主机,手里的副本却还是更早那台的:用它接任会丢掉那一任主机期间的全部改动
		return fail(fmt.Errorf("副本来自旧纪元 %d,本机已在纪元 %d:用它接任会丢掉上一任主机期间的改动。确实要接任,用命令行 m-ui ha takeover --force", env.Header.Epoch, E))
	}
	next, err := NextEpoch(E, env.Header.Epoch)
	if err != nil {
		return fail(err)
	}
	if o.Expect != 0 && next != o.Expect {
		return fmt.Errorf("发起确认之后纪元已经变了(要接任的是纪元 %d,现在会是 %d):不接任", o.Expect, next)
	}
	seed, err := s.takeoverSeed()
	if err != nil {
		return fail(err)
	}
	b := Became{How: HowTakeover, Epoch: next, FormerId: meta.MasterId, FormerName: meta.MasterName, At: s.now().Unix(),
		ReplicaAt: ri.AsOf, MaxChangeId: meta.MaxChangeId, By: o.By, FormerIP: meta.Identity.PublicIP, FormerIP6: meta.Identity.PublicIP6}
	if b.ReplicaAt < meta.CreatedAt {
		b.ReplicaAt = meta.CreatedAt
	}
	if err := s.buildPromote(dir, meta, b, false, promoteOpts{SignSeed: seed}); err != nil {
		return fail(err)
	}
	// 新库已经落盘:这时才记"正在接任"(重启前不再接受别的接任 / 设为主机)
	if err := s.set(map[string]string{KeyRole: RolePromoting}); err != nil {
		discardPromote(s.d.DBPath)
		return fail(err)
	}
	logger.Warning("容灾:本机用副本(", which, ",数据截至 ", time.Unix(b.ReplicaAt, 0).Format("2006-01-02 15:04:05"),
		")接任为主机,纪元 ", next, ";马上重启")
	s.restart(2 * time.Second)
	return nil
}

// takeoverRefused 不能手动接任的原因(代码给界面;命令行与 Telegram 看中文)。
func takeoverRefused(why string) error {
	switch why {
	case "auto":
		return errors.New("自动接任模式由系统判定,面板上不能手动接任:看失联判定的原因;确实要接任,用命令行 m-ui ha takeover --force")
	case "peerSeesMaster":
		return errors.New("别的副机还联系得上主机:多半是本机和主机之间的网络问题,不接任")
	case "witnessUp":
		return errors.New("境外测点还能连上主机:可能是网络分区,不接任")
	case "betterStandby":
		return errors.New("排在前面的备用还在,由它接任")
	case "higherEpoch":
		return errors.New("别的备用已经接任了,等它来推送")
	case "masterOnline":
		return errors.New("主机最近还联系过本机,看起来没有失联")
	}
	return errors.New("还没到确认接任的时候(失联判定还在进行,或证据不足):等确认提示出现;确实要接任,用命令行 m-ui ha takeover --force")
}

// peersAllowTakeover 接任前问一圈同伴:有同伴已经在新纪元或更高、或已经是主机 / 正在接任,就不接任(两台备用别在同一纪元都当主机)。
func (s *Service) peersAllowTakeover(E uint64) error {
	next, err := NextEpoch(E)
	if err != nil {
		return err
	}
	ctx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
	defer cancel()
	for id, o := range s.collectPeers(ctx) {
		if !o.ok {
			continue
		}
		switch {
		case o.st.Epoch >= next && EpochClaimOK(E, o.st.Epoch):
			return fmt.Errorf("同伴 %s 已经在纪元 %d:别的备用已经接任了,不再接任", s.nodeName(id), o.st.Epoch)
		case o.st.Role == "master" || o.st.Role == RolePromoting:
			return fmt.Errorf("同伴 %s 已经是主机或正在接任,不再接任", s.nodeName(id))
		}
	}
	return nil
}

// takeoverSeed 新主机的签名密钥:沿用本机的(要票时已经把它的公钥给了同伴),没有就生成。
func (s *Service) takeoverSeed() (string, error) {
	if _, err := s.signKey(); err != nil {
		return "", err
	}
	return s.setting(KeySignKey), nil
}

// dropOwnVote 接任没做成:撤掉本机投给自己的票(别的候选还能要到本机的票)。
func (s *Service) dropOwnVote() {
	var v Vote
	if s.getJSON(KeyVote, &v) && v.Candidate == s.SelfId() {
		_ = s.set(map[string]string{KeyVote: ""})
	}
}

// replicaUsable 本机的副本能不能拿来接任(副本的版本不比本机新)。
func (s *Service) replicaUsable(ri ReplicaInfo) error {
	if selfupdate.Newer(ri.Version, s.d.Version) {
		return fmt.Errorf("副本来自 v%s,比本机的 v%s 新:先把本机升级", ri.Version, s.d.Version)
	}
	return nil
}

// promoteOpts 组新库的选项。
type promoteOpts struct {
	SignSeed string // 新主机的签名密钥:候选人要票时、被设为主机时给出去的公钥对应的那把,新库里沿用它
}

// promoteMeta <库>.promote.json:待换上的新库的说明(过期的不换)。
type promoteMeta struct {
	Epoch   uint64 `json:"epoch"`
	How     string `json:"how"`
	Expires int64  `json:"expires"`
}

func promoteMetaFile(dbPath string) string { return promoteFile(dbPath) + ".json" }

// writePromoteMeta 写新库的说明(接任时组好就写;设为主机在提交时写)。
func writePromoteMeta(dbPath string, m promoteMeta) error {
	b, _ := json.Marshal(m)
	return writeFileAtomic(promoteMetaFile(dbPath), b)
}

func readPromoteMeta(dbPath string) (promoteMeta, bool) {
	var m promoteMeta
	b, err := os.ReadFile(promoteMetaFile(dbPath))
	return m, err == nil && json.Unmarshal(b, &m) == nil
}

// discardPromote 删掉写好的新库与说明(组好之后又没能记下"正在接任"时)。
func discardPromote(dbPath string) {
	_ = os.Remove(promoteFile(dbPath))
	_ = os.Remove(promoteMetaFile(dbPath))
	syncDir(filepath.Dir(dbPath))
}

// buildPromote 用解开的数据(dir 下 replica.db、identity/*)组新库:先写 <库>.promote.tmp、落盘,再改名为 <库>.promote
// 并写说明(带时效);staged 为真时改名为 .promote.staged,等提交时再改名、写说明。b 写进新库的 haBecame。
// opts 最多一个:新主机的签名密钥(不给就沿用本机的)。
func (s *Service) buildPromote(dir string, meta ReplicaMeta, b Became, staged bool, opts ...promoteOpts) error {
	var o promoteOpts
	if len(opts) > 0 {
		o = opts[0]
	}
	src := filepath.Join(dir, "replica.db")
	tmp := s.d.DBPath + ".promote.tmp"
	clean := func() {
		for _, p := range []string{tmp, tmp + "-wal", tmp + "-shm"} {
			_ = os.Remove(p)
		}
	}
	clean()
	if err := copyFile(src, tmp); err != nil {
		clean()
		return fmt.Errorf("准备新库: %w", err)
	}
	ndb, err := database.Open(tmp) // 迁移到本机版本的表结构
	if err != nil {
		clean()
		return fmt.Errorf("打开新库: %w", err)
	}
	selfId := s.SelfId()
	if selfId == 0 {
		database.Close(ndb)
		clean()
		return errors.New("本机还没收到过主机的配置,不知道自己在服务器表里是哪一台")
	}
	var self model.Node
	if err := ndb.Where("id = ?", selfId).First(&self).Error; err != nil {
		database.Close(ndb)
		clean()
		return fmt.Errorf("数据里没有本机(服务器 #%d):这份数据不是本机所在集群的", selfId)
	}
	// 本机还没上报的流量并进去:按数据里本机的游标只计增量,不多算、不漏算
	var counters []model.AgentCounter
	s.d.DB.Find(&counters)
	bucket := int64(atoiDef(s.settingFrom(ndb, "statsBucketSeconds"), 60))
	if bucket <= 0 {
		bucket = 60
	}
	if len(counters) > 0 {
		if _, err := hub.ApplyCounters(ndb, selfId, self.Name, s.setting(hub.LedgerEpochKey), counters, s.now().Unix(), bucket, self.Ratio); err != nil {
			logger.Warning("容灾:并入本机还没上报的流量失败(继续): ", err)
		}
	}
	var res identityResult
	err = ndb.Transaction(func(tx *gorm.DB) error {
		if err := s.applyLocal(tx, b.Epoch, selfId, o); err != nil {
			return err
		}
		if ids := withoutId(meta.Standbys, selfId); len(ids) > 0 {
			// 原主机当时的备用名单(去掉本机):新主机刚起来、各台报告还没拉齐时先按它发副本
			if err := upsert(tx, map[string]string{KeyStandbyList: mustJSON(ids)}); err != nil {
				return err
			}
		}
		if err := s.copyCerts(tx); err != nil {
			return err
		}
		var err error
		if res, err = applyIdentity(tx, meta.Identity, selfId, dir, s.DataDir()); err != nil {
			return err
		}
		if err := tx.Exec("UPDATE nodes SET is_local = ?", false).Error; err != nil {
			return err
		}
		if err := tx.Exec("UPDATE nodes SET is_local = ?, enabled = ? WHERE id = ?", true, true, selfId).Error; err != nil {
			return err
		}
		if meta.MasterId != 0 && meta.MasterId != selfId {
			if err := s.applyFormerRow(tx, meta.MasterId, selfId, meta.Identity, b.How); err != nil {
				return err
			}
			if b.How == HowTakeover {
				f := Former{Id: meta.MasterId, Name: meta.MasterName, Since: s.now().Unix(), SinceChange: meta.MaxChangeId}
				if err := upsert(tx, map[string]string{KeyFormer: mustJSON(f)}); err != nil {
					return err
				}
			}
		}
		for _, t := range []string{"sessions", "agent_counters"} {
			if err := tx.Exec("DELETE FROM " + t).Error; err != nil {
				return err
			}
		}
		b.Identity = res
		// 审计记进新库:旧库重启时就换下去了
		actor, action := "ha:"+nonEmpty(b.By, "panel"), "takeover"
		if b.How == HowSetMaster {
			actor, action = "ha", "became-master"
		}
		auditTo(tx, s.now().Unix(), actor, action, map[string]interface{}{"former": b.FormerName, "epoch": b.Epoch, "replicaAt": b.ReplicaAt})
		return upsert(tx, map[string]string{KeyBecame: mustJSON(b)})
	})
	if cerr := database.Close(ndb); err == nil {
		err = cerr
	}
	if err != nil {
		clean()
		return fmt.Errorf("组新库: %w", err)
	}
	for _, p := range []string{tmp + "-wal", tmp + "-shm"} {
		_ = os.Remove(p)
	}
	if err := syncFile(tmp); err != nil {
		clean()
		return fmt.Errorf("新库落盘: %w", err)
	}
	dst := promoteFile(s.d.DBPath)
	if staged {
		dst += ".staged"
	}
	if err := os.Rename(tmp, dst); err != nil {
		clean()
		return err
	}
	_ = os.Chmod(dst, 0o600)
	syncDir(filepath.Dir(dst))
	if !staged {
		if err := writePromoteMeta(s.d.DBPath, promoteMeta{Epoch: b.Epoch, How: b.How, Expires: s.now().Add(promoteTTL).Unix()}); err != nil {
			discardPromote(s.d.DBPath)
			return fmt.Errorf("记下新库的时效: %w", err)
		}
	}
	return nil
}

// syncFile 把一个文件的内容落盘。
func syncFile(p string) error {
	f, err := os.OpenFile(p, os.O_RDWR, 0)
	if err != nil {
		return err
	}
	if err := f.Sync(); err != nil {
		f.Close()
		return err
	}
	return f.Close()
}

func withoutId(ids []uint, id uint) []uint {
	var out []uint
	for _, x := range ids {
		if x != id {
			out = append(out, x)
		}
	}
	return out
}

func (s *Service) settingFrom(db *gorm.DB, key string) string {
	var v string
	db.Raw("SELECT value FROM settings WHERE key = ?", key).Scan(&v)
	return v
}

// dropOnPromote 成为主机时不带过去的本机设置:主副机同步的进度、副机才有的容灾状态。
func dropOnPromote(k string) bool {
	if strings.HasPrefix(k, "hub") {
		return true
	}
	switch k {
	case "nodeMode", KeyRole, KeyEpoch, KeyMasterId, KeyMasterPub, KeySignKey, KeyVote, KeyGrant, KeyState, KeyNotify, KeyPeerKey,
		KeyDemoted, KeyBecame, KeyFreezeUntil, KeySwitch, KeyIncoming, KeyStandbyList, KeyFormer, KeyCmd, KeyCmdResult, KeyNotices, KeyDnsChanged:
		return true
	}
	return false
}

// applyLocal 新库里的本机设置换成本机自己的(原主机的本机设置不进数据,这里只是保险再删一遍),角色改成主机;
// 纪元写进新库(换上新库时才生效),签名密钥用 o 给的那把,实例号记本机的。
func (s *Service) applyLocal(tx *gorm.DB, epoch uint64, selfId uint, o promoteOpts) error {
	var keys []string
	if err := tx.Raw("SELECT key FROM settings").Scan(&keys).Error; err != nil {
		return err
	}
	for _, k := range keys {
		if IsLocal(k) {
			if err := tx.Exec("DELETE FROM settings WHERE key = ?", k).Error; err != nil {
				return err
			}
		}
	}
	var mine []model.Setting
	if err := s.d.DB.Find(&mine).Error; err != nil {
		return err
	}
	values := map[string]string{}
	for _, row := range mine {
		if IsLocal(row.Key) && !dropOnPromote(row.Key) {
			values[row.Key] = row.Value
		}
	}
	values["nodeMode"] = "false"
	values[KeyRole] = ""
	values[KeyEpoch] = strconv.FormatUint(epoch, 10)
	values[KeyMasterId] = strconv.FormatUint(uint64(selfId), 10)
	seed := o.SignSeed
	if seed == "" {
		seed = s.setting(KeySignKey)
	}
	if _, err := SignKey(seed); err == nil {
		values[KeySignKey] = seed
	}
	values[KeyInstance] = s.Instance() // 新库由本机运行(数据里带来的是原主机的实例号;读不到本机的就清空,启动时再写)
	return upsert(tx, values)
}

// copyCerts 证书库换成本机自己的(各机各签:原主机的证书路径在本机上不存在)。
func (s *Service) copyCerts(tx *gorm.DB) error {
	if err := tx.Exec("DELETE FROM certs").Error; err != nil {
		return err
	}
	var rows []model.Cert
	if err := s.d.DB.Find(&rows).Error; err != nil {
		return err
	}
	for _, c := range rows {
		auto := c.AutoRenew
		if err := tx.Create(&c).Error; err != nil {
			return err
		}
		if !auto {
			tx.Model(&model.Cert{}).Where("id = ?", c.Id).Update("auto_renew", false)
		}
	}
	return nil
}

// ApplyPending 进程启动、还没打开库时调用:有待换上的新库就换上(当前库连同 -wal / -shm 改名留着)。返回是否换了。
func ApplyPending(dbPath string) (bool, error) { return ApplyPendingAt(dbPath, time.Now()) }

// ApplyPendingAt 同 ApplyPending,按 now 判时效:过期的新库不换,改名 .promote.expired-<时间> 留档,下次启动面板提示一次。
// 换库前先把当前库的 -wal / -shm 挪开(留在原名下会被 SQLite 当成新库的日志回放进去),挪不走就不换;每次改名后落盘目录。
func ApplyPendingAt(dbPath string, now time.Time) (bool, error) {
	p := promoteFile(dbPath)
	dataDir := filepath.Dir(dbPath)
	if _, err := os.Stat(p); err != nil {
		_ = os.Remove(promoteMetaFile(dbPath)) // 没有新库,说明也不要了
		return false, nil
	}
	stamp := now.Format("20060102-150405")
	if m, ok := readPromoteMeta(dbPath); ok && m.Expires > 0 && now.Unix() > m.Expires {
		if err := os.Rename(p, p+".expired-"+stamp); err != nil {
			return false, fmt.Errorf("作废过期的新库: %w", err)
		}
		_ = os.Remove(promoteMetaFile(dbPath))
		syncDir(dataDir)
		if b, err := json.Marshal(m); err == nil && ensureDir(HADir(dataDir)) == nil {
			_ = writeFileAtomic(filepath.Join(HADir(dataDir), fileExpired), b)
		}
		logger.Warning("容灾:待换上的新库已经过期(纪元 ", m.Epoch, "),不换,留作 ", filepath.Base(p), ".expired-", stamp)
		return false, nil
	}
	old := dbPath + ".pre-promote-" + stamp
	type move struct{ from, to string }
	var done []move
	undo := func() {
		for i := len(done) - 1; i >= 0; i-- {
			_ = os.Rename(done[i].to, done[i].from)
		}
		syncDir(dataDir)
	}
	for _, suf := range []string{"", "-wal", "-shm"} {
		if _, err := os.Stat(dbPath + suf); err != nil {
			continue
		}
		if err := os.Rename(dbPath+suf, old+suf); err != nil {
			undo()
			return false, fmt.Errorf("挪开旧库 %s 失败,不换库: %w", filepath.Base(dbPath+suf), err)
		}
		done = append(done, move{dbPath + suf, old + suf})
		syncDir(dataDir)
	}
	for _, suf := range []string{"-wal", "-shm"} {
		if _, err := os.Stat(dbPath + suf); err == nil {
			undo()
			return false, fmt.Errorf("旧库的 %s 还在原处,不换库", filepath.Base(dbPath+suf))
		}
	}
	if err := os.Rename(p, dbPath); err != nil {
		undo()
		return false, fmt.Errorf("换上新库: %w", err)
	}
	_ = os.Remove(promoteMetaFile(dbPath))
	syncDir(dataDir)
	// 本机成了主机:备用手里的旧副本不要了(里面是原主机的全部数据)
	dir := HADir(dataDir)
	for _, n := range []string{fileCur, filePrev, fileTmp, fileInfo} {
		_ = os.Remove(filepath.Join(dir, n))
	}
	prunePrePromote(dbPath, 2)
	return true, nil
}

// prunePrePromote 只留最近 keep 份换下来的旧库。
func prunePrePromote(dbPath string, keep int) {
	matches, _ := filepath.Glob(dbPath + ".pre-promote-*")
	var bases []string
	for _, m := range matches {
		if !strings.HasSuffix(m, "-wal") && !strings.HasSuffix(m, "-shm") {
			bases = append(bases, m)
		}
	}
	sort.Sort(sort.Reverse(sort.StringSlice(bases)))
	for i := keep; i < len(bases); i++ {
		for _, suf := range []string{"", "-wal", "-shm"} {
			_ = os.Remove(bases[i] + suf)
		}
	}
}

func copyFile(src, dst string) error {
	b, err := os.Open(src)
	if err != nil {
		return err
	}
	defer b.Close()
	out, err := os.OpenFile(dst, os.O_CREATE|os.O_TRUNC|os.O_WRONLY, 0o600)
	if err != nil {
		return err
	}
	if _, err := out.ReadFrom(b); err != nil {
		out.Close()
		return err
	}
	if err := out.Sync(); err != nil {
		out.Close()
		return err
	}
	return out.Close()
}

func nowUnix() int64 { return time.Now().Unix() }
