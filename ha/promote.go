package ha

import (
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
// 服务器表里本机那行标成"本机"、原主机那行填上"作为副机时怎么连",纪元加 1;写成 <库>.promote,重启时换上。
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
	Id    uint   `json:"id"`
	Name  string `json:"name"`
	Since int64  `json:"since"`
}

// promoteFile 待换上的新库。
func promoteFile(dbPath string) string { return dbPath + ".promote" }

// TakeoverOpts 接任的参数。
type TakeoverOpts struct {
	Force bool   // 不管主机最近是不是还联系过本机、是不是暂停中(命令行 --force)
	By    string // panel | telegram | cli | auto
}

// errTakeoverBusy 已经在接任 / 设为主机了。
var errTakeoverBusy = errors.New("正在接任或换主机,稍等")

// Takeover 本机(备用)接任为主机:用手里的数据副本组新库、纪元加 1、重启后以主机身份启动。
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
	}
	ri, ok := s.replicaInfo()
	if !ok {
		return errors.New("本机没有数据副本:主机还没把副本推过来,或本机不是备用")
	}
	if err := s.replicaUsable(ri); err != nil {
		return err
	}
	dir, err := os.MkdirTemp(s.Dir(), "takeover-")
	if err != nil {
		return err
	}
	defer os.RemoveAll(dir)
	meta, env, which, err := s.openReplica(dir)
	if err != nil {
		return err
	}
	E := s.Epoch()
	if env.Header.Epoch < E {
		return fmt.Errorf("副本来自旧纪元 %d(本机在纪元 %d):等主机推新的一份", env.Header.Epoch, E)
	}
	next := max(E, env.Header.Epoch) + 1
	b := Became{How: HowTakeover, Epoch: next, FormerId: meta.MasterId, FormerName: meta.MasterName, At: s.now().Unix(),
		ReplicaAt: ri.AsOf, MaxChangeId: meta.MaxChangeId, By: o.By, FormerIP: meta.Identity.PublicIP, FormerIP6: meta.Identity.PublicIP6}
	if b.ReplicaAt < meta.CreatedAt {
		b.ReplicaAt = meta.CreatedAt
	}
	// 先把纪元升上去、记下"正在接任":中途崩溃也不会退回去再听原主机的
	if err := s.set(map[string]string{KeyRole: RolePromoting, KeyEpoch: strconv.FormatUint(next, 10)}); err != nil {
		return err
	}
	if err := s.buildPromote(dir, meta, b, false); err != nil {
		_ = s.set(map[string]string{KeyRole: ""}) // 纪元留着(只升不降):本机以后照样能接任,只是原主机来请求时回 stale
		return err
	}
	logger.Warning("容灾:本机用副本(", which, ",数据截至 ", time.Unix(b.ReplicaAt, 0).Format("2006-01-02 15:04:05"),
		")接任为主机,纪元 ", next, ";马上重启")
	s.restart(2 * time.Second)
	return nil
}

// replicaUsable 本机的副本能不能拿来接任(副本的版本不比本机新)。
func (s *Service) replicaUsable(ri ReplicaInfo) error {
	if selfupdate.Newer(ri.Version, s.d.Version) {
		return fmt.Errorf("副本来自 v%s,比本机的 v%s 新:先把本机升级", ri.Version, s.d.Version)
	}
	return nil
}

// buildPromote 用解开的数据(dir 下 replica.db、identity/*)组新库,写成 <库>.promote(staged 为真时写成 .promote.staged,
// 等提交时再改名)。b 写进新库的 haBecame。
func (s *Service) buildPromote(dir string, meta ReplicaMeta, b Became, staged bool) error {
	src := filepath.Join(dir, "replica.db")
	tmp := s.d.DBPath + ".promote.tmp"
	for _, p := range []string{tmp, tmp + "-wal", tmp + "-shm"} {
		_ = os.Remove(p)
	}
	if err := copyFile(src, tmp); err != nil {
		return fmt.Errorf("准备新库: %w", err)
	}
	ndb, err := database.Open(tmp) // 迁移到本机版本的表结构
	if err != nil {
		return fmt.Errorf("打开新库: %w", err)
	}
	selfId := s.SelfId()
	if selfId == 0 {
		database.Close(ndb)
		return errors.New("本机还没收到过主机的配置,不知道自己在服务器表里是哪一台")
	}
	var self model.Node
	if err := ndb.Where("id = ?", selfId).First(&self).Error; err != nil {
		database.Close(ndb)
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
		if err := s.applyLocal(tx, b.Epoch, selfId); err != nil {
			return err
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
			up := map[string]interface{}{"is_local": false}
			if l := meta.Identity.Self; l.ApiUrl != "" && l.Token != "" {
				up["api_url"], up["token"], up["cert_fp"] = l.ApiUrl, l.Token, l.CertFP
				up["insecure"] = strings.HasPrefix(strings.ToLower(l.ApiUrl), "https://")
			}
			if b.How == HowTakeover {
				up["enabled"] = false // 失联期间不进订阅;它回来自动变为副机时再启用
			}
			if err := tx.Model(&model.Node{}).Where("id = ?", meta.MasterId).Updates(up).Error; err != nil {
				return err
			}
			if b.How == HowTakeover {
				if err := upsert(tx, map[string]string{KeyFormer: mustJSON(Former{Id: meta.MasterId, Name: meta.MasterName, Since: s.now().Unix()})}); err != nil {
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
		_ = os.Remove(tmp)
		return fmt.Errorf("组新库: %w", err)
	}
	for _, p := range []string{tmp + "-wal", tmp + "-shm"} {
		_ = os.Remove(p)
	}
	dst := promoteFile(s.d.DBPath)
	if staged {
		dst += ".staged"
	}
	if err := os.Rename(tmp, dst); err != nil {
		return err
	}
	_ = os.Chmod(dst, 0o600)
	syncDir(filepath.Dir(dst))
	return nil
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

// applyLocal 新库里的本机设置换成本机自己的(原主机的本机设置不进数据,这里只是保险再删一遍),角色改成主机。
func (s *Service) applyLocal(tx *gorm.DB, epoch uint64, selfId uint) error {
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

// ApplyPending 进程启动、还没打开库时调用:有待换上的新库就换上(当前库连同 -wal / -shm 改名留着)。
// 返回是否换了。
func ApplyPending(dbPath string) (bool, error) {
	p := promoteFile(dbPath)
	if _, err := os.Stat(p); err != nil {
		return false, nil
	}
	stamp := time.Now().Format("20060102-150405")
	old := dbPath + ".pre-promote-" + stamp
	if _, err := os.Stat(dbPath); err == nil {
		if err := os.Rename(dbPath, old); err != nil {
			return false, fmt.Errorf("留旧库: %w", err)
		}
		for _, suf := range []string{"-wal", "-shm"} {
			if _, err := os.Stat(dbPath + suf); err == nil {
				_ = os.Rename(dbPath+suf, old+suf)
			}
		}
	}
	if err := os.Rename(p, dbPath); err != nil {
		_ = os.Rename(old, dbPath)
		return false, fmt.Errorf("换上新库: %w", err)
	}
	// 本机成了主机:备用手里的旧副本不要了(里面是原主机的全部数据)
	dir := HADir(filepath.Dir(dbPath))
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
