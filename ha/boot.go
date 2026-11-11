package ha

import (
	"crypto/rand"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"strconv"
	"strings"
	"time"

	"github.com/Maoyangui/m-ui/database/model"
	"github.com/Maoyangui/m-ui/logger"

	"gorm.io/gorm"
)

// 纪元只升不降(设计 2.5)。
//
// 库会被整个换掉:还原旧备份、菜单「重置全部设置」、把主机的备份还原到另一台机器上启动。库里的纪元因此可能回退,
// 或者同一份主机库在两台机器上同时当主机。所以每台机器在 <数据目录>/ha/(不进库、不进备份)另记两样:
//
//	ha/instance     本机实例号(第一次启动随机生成)
//	ha/epoch.json   本机当过 / 跟过的最高纪元及当时的主机(每次改纪元都同步写、落盘)
//
// 库里另有本机设置 haInstance = 最近一次运行这份库的实例号。启动时(Boot)对照这两样:
//   - 主机库的 haInstance 是别的实例:这份库换了机器(换机还原、手工拷库)→ 以更高纪元围栏,原机器回来会被副机拒绝、自动变为副机;
//   - 同一实例,库纪元低于记录、而记录里那个纪元的主机就是本机:库被回退了 → 抬回记录值再围栏(副机照常认本机);
//   - 记录里本机当时是跟随者,却还原出一份更旧的主机库:不抬,让它按多数副机的回复自然变为副机(数据进分叉备份);
//   - 副机库纪元低于记录:抬回记录值与记录的主机。

const (
	fileInstance = "instance"
	fileEpoch    = "epoch.json"
	fileExpired  = "promote-expired.json" // 启动时没换上的过期新库:换成一条面板提示
)

// KeyInstance 本机设置:最近一次运行这份库的实例号(启动时写)。
const KeyInstance = "haInstance"

// epochRecord ha/epoch.json:本机当过 / 跟过的最高纪元及当时的主机。
type epochRecord struct {
	Epoch  uint64 `json:"epoch"`
	Master uint   `json:"master"`
	At     int64  `json:"at"`
}

func readEpochRecord(dir string) epochRecord {
	var r epochRecord
	if b, err := os.ReadFile(filepath.Join(dir, fileEpoch)); err == nil {
		_ = json.Unmarshal(b, &r)
	}
	return r
}

func writeEpochRecord(dir string, r epochRecord) error {
	if err := ensureDir(dir); err != nil {
		return err
	}
	b, _ := json.Marshal(r)
	return writeFileAtomic(filepath.Join(dir, fileEpoch), b)
}

// raiseEpochRecord 记录不低于 (e, master):纪元更高,或同一纪元换了主机(先被接受的那台让给了另一台)时更新。
func raiseEpochRecord(dir string, e uint64, master uint, now time.Time) error {
	r := readEpochRecord(dir)
	if e < r.Epoch || (e == r.Epoch && (master == 0 || master == r.Master)) {
		return nil
	}
	return writeEpochRecord(dir, epochRecord{Epoch: e, Master: master, At: now.Unix()})
}

// instanceID 读本机实例号,没有就生成。
func instanceID(dir string) (string, error) {
	p := filepath.Join(dir, fileInstance)
	if b, err := os.ReadFile(p); err == nil {
		if id := strings.TrimSpace(string(b)); len(id) >= 16 {
			return id, nil
		}
	}
	if err := ensureDir(dir); err != nil {
		return "", err
	}
	raw := make([]byte, 12)
	if _, err := rand.Read(raw); err != nil {
		return "", err
	}
	id := hex.EncodeToString(raw)
	if err := writeFileAtomic(p, []byte(id+"\n")); err != nil {
		return "", err
	}
	return id, nil
}

// Instance 本机实例号(换机还原的判定用;读不到写不进时为空,只是少了这层判定)。
func (s *Service) Instance() string {
	c := s.core()
	c.mu.Lock()
	defer c.mu.Unlock()
	if c.inst == "" {
		id, err := instanceID(s.Dir())
		if err != nil {
			logger.Warning("容灾:读写本机实例号失败: ", err)
			return ""
		}
		c.inst = id
	}
	return c.inst
}

// setEpoch 改本机纪元的唯一入口:先把记录抬上去(落盘),再在同一事务里写纪元、主机与 more。
func (s *Service) setEpoch(e uint64, master uint, more map[string]string) error {
	if e >= MaxEpoch {
		return errEpochLimit
	}
	if err := raiseEpochRecord(s.Dir(), e, master, s.now()); err != nil {
		logger.Warning("容灾:记下本机纪元失败(继续): ", err)
	}
	values := map[string]string{KeyEpoch: strconv.FormatUint(e, 10), KeyMasterId: strconv.FormatUint(uint64(master), 10)}
	for k, v := range more {
		values[k] = v
	}
	if err := s.set(values); err != nil {
		return err
	}
	s.invalidate()
	return nil
}

// EnterEpoch 主机第一次进入纪元(0 → 1):开启容灾、纪元 0 的集群做设为主机之前。已经进过纪元的什么都不做,返回当前纪元。
func (s *Service) EnterEpoch() (uint64, error) {
	if s.IsNode() {
		return 0, errors.New("副机上不能改主机容灾:到主机面板改")
	}
	if L := s.Epoch(); L > 0 {
		return L, nil
	}
	if _, err := s.signKey(); err != nil {
		return 0, err
	}
	s.clusterKey()
	next, err := NextEpoch(0)
	if err != nil {
		return 0, err
	}
	if err := s.setEpoch(next, s.SelfId(), nil); err != nil {
		return 0, err
	}
	logger.Info("主机容灾:本机进入纪元 ", next)
	return next, nil
}

// BootResult 启动时的纪元判定:要不要以更高纪元围栏(StartupCheck 执行)。
type BootResult struct {
	Fence    bool
	From, To uint64
	Reason   string // restoredElsewhere | rolledBack | ""
}

// fencePause 围栏前暂停接任多久(随状态推给备用:本机正在以新纪元接管,别误判失联)。
const fencePause = 5 * time.Minute

// Boot 进程起来、打开库之后,在启动自检之前调用一次(restored = 这次启动刚从备份还原过库)。见文件开头。
func (s *Service) Boot(restored bool) BootResult {
	inst := s.Instance()
	dir := s.Dir()
	rec := readEpochRecord(dir)
	dbInst := strings.TrimSpace(s.setting(KeyInstance))
	L := s.Epoch()
	node := s.IsNode()
	self := s.SelfId()
	var res BootResult
	if node && s.setting(KeyRole) == RolePromoting {
		if _, err := os.Stat(promoteFile(s.d.DBPath)); err != nil {
			_ = s.set(map[string]string{KeyRole: ""})
			logger.Warning("容灾:上次接任 / 被设为主机没做完(没有待换的新库),清掉「正在接任」")
		}
	}
	switch {
	case inst != "" && !node && foreignDB(dbInst, inst, restored, L, rec):
		if next, err := NextEpoch(L); err == nil {
			res = BootResult{Fence: true, From: L, To: next, Reason: "restoredElsewhere"}
			logger.Warning("容灾:这份主机库上次在另一台机器上运行(实例 ", dbInst, "),本机以纪元 ", next, " 接管;原来那台回来后会自动变为副机")
		}
	case !node && self != 0 && rec.Master == self && rec.Epoch > L:
		if err := s.setEpoch(rec.Epoch, self, nil); err != nil {
			logger.Warning("容灾:把纪元抬回 ", rec.Epoch, " 失败: ", err)
			break
		}
		if next, err := NextEpoch(rec.Epoch); err == nil {
			res = BootResult{Fence: true, From: rec.Epoch, To: next, Reason: "rolledBack"}
		}
		logger.Warning("容灾:库里的纪元 ", L, " 比本机记下的 ", rec.Epoch, " 低(还原了旧备份或重置过设置),已抬回,并以纪元 ", res.To, " 继续当主机")
	case node && rec.Master != 0 && rec.Epoch > L:
		if err := s.setEpoch(rec.Epoch, rec.Master, map[string]string{"hubSnapshotSequence": "", KeyMasterPub: "", KeyState: "", KeyVote: "", KeyGrant: ""}); err != nil {
			logger.Warning("容灾:把纪元抬回 ", rec.Epoch, " 失败: ", err)
		} else {
			logger.Warning("容灾:库里的纪元 ", L, " 比本机记下的 ", rec.Epoch, " 低,已抬回(主机 #", rec.Master, ")")
		}
	}
	if inst != "" && dbInst != inst {
		_ = s.set(map[string]string{KeyInstance: inst})
	}
	master := self
	if s.IsNode() {
		master, _ = s.masterId()
	}
	if e := s.Epoch(); e > 0 {
		if err := raiseEpochRecord(dir, e, master, s.now()); err != nil {
			logger.Warning("容灾:记下本机纪元失败: ", err)
		}
	}
	s.noteExpiredPromote()
	if res.Fence {
		until := s.now().Add(fencePause).Unix()
		if cur, _ := strconv.ParseInt(s.setting(KeyPauseUntil), 10, 64); cur < until {
			_ = s.set(map[string]string{KeyPauseUntil: strconv.FormatInt(until, 10)})
		}
		c := s.core()
		c.mu.Lock()
		c.fence = &res
		c.mu.Unlock()
	}
	return res
}

// foreignDB 这份主机库是不是上次在别的机器上运行的。没有实例号的库(容灾以前的版本写的):
// 刚从备份还原、进过纪元、而本机从没记过纪元 → 当作别的机器的库;没进过纪元的无从判断,不围栏。
func foreignDB(dbInst, inst string, restored bool, dbEpoch uint64, rec epochRecord) bool {
	if dbInst != "" {
		return dbInst != inst
	}
	return restored && dbEpoch > 0 && rec.Epoch == 0
}

// noteExpiredPromote 上次启动没换上的过期新库:面板顶部提示一次。
func (s *Service) noteExpiredPromote() {
	p := filepath.Join(s.Dir(), fileExpired)
	b, err := os.ReadFile(p)
	if err != nil {
		return
	}
	var m promoteMeta
	_ = json.Unmarshal(b, &m)
	s.addNotice(Notice{Kind: "promoteExpired", How: m.How, N: int64(m.Epoch)})
	_ = os.Remove(p)
}

// takeFence 取走 Boot 定下的围栏(只执行一次)。
func (s *Service) takeFence() *BootResult {
	c := s.core()
	c.mu.Lock()
	defer c.mu.Unlock()
	f := c.fence
	c.fence = nil
	return f
}

// runFence 执行 Boot 定下的围栏:先在旧纪元给全部启用副机推带自交接标记的签名状态,再把纪元升上去。
func (s *Service) runFence(f *BootResult) {
	if f == nil || !f.Fence || s.IsNode() || s.Epoch() != f.From {
		return
	}
	granted, err := s.fenceTo(f.To, 0, s.remoteNodes(), nil)
	if err != nil {
		logger.Warning("容灾:以纪元 ", f.To, " 围栏失败: ", err)
		return
	}
	logger.Warning("容灾:已以纪元 ", f.To, " 继续当主机(", f.Reason, ",交接标记推给了 ", granted, " 台副机)")
	s.audit("ha", "fence", map[string]interface{}{"from": f.From, "epoch": f.To, "reason": f.Reason, "granted": granted})
	s.addNotice(Notice{Kind: "fenced", Reason: f.Reason, N: int64(f.To)})
	switch f.Reason {
	case "restoredElsewhere":
		s.notify(fmt.Sprintf("🟠 <b>本机以纪元 %d 接管主机</b>:这份数据上次在另一台机器上当主机(本机是从它的备份还原的)。"+
			"原来那台要是还在运行,它会被副机拒绝并自动变为副机;不要它了就在那台执行 systemctl disable --now m-ui。", f.To))
	default:
		s.notify(fmt.Sprintf("🟠 <b>本机的纪元已恢复</b>:库里的纪元比本机记下的低(还原了旧备份或重置过设置),已抬回并以纪元 %d 继续当主机,副机照常认本机。", f.To))
	}
}

// fenceTo 以更高的纪元 next 继续当主机:先在当前纪元给 nodes 推带自交接标记(给本机、绑本机签名公钥,10 分钟有效)
// 与撤销的签名状态,再把本机纪元升到 next。认本机的副机凭标记立刻跟上;租约已过期的副机本来就接受更高纪元。返回推成功的台数。
// check 不为空时,推完、升纪元之前先问它(参数是推成功的台数):返回错误就不升纪元,原样返回这个错误。
func (s *Service) fenceTo(next, revoke uint64, nodes []model.Node, check func(granted int) error) (int, error) {
	if s.IsNode() {
		return 0, errors.New("本机不是主机")
	}
	L := s.Epoch()
	if next <= L {
		return 0, fmt.Errorf("新纪元 %d 不比本机的 %d 高", next, L)
	}
	self := s.SelfId()
	granted := 0
	if L > 0 {
		pub, err := s.SignPub()
		if err != nil {
			return 0, err
		}
		if revoke > 0 {
			c := s.core()
			c.mu.Lock()
			if revoke > c.revoke {
				c.revoke = revoke
			}
			c.mu.Unlock()
		}
		s.setGrantOut(&Grant{Epoch: next, To: self, ToPub: pub, Expires: s.now().Add(grantTTL).Unix()})
		defer s.setGrantOut(nil)
		s.invalidate()
		granted = s.pushAll(nodes, 15*time.Second)
	}
	if check != nil {
		if err := check(granted); err != nil {
			return granted, err
		}
	}
	if s.IsNode() || s.Epoch() != L { // 推的这段时间里本机已经变为副机(或纪元已被别处改过):不再升
		return granted, errors.New("本机的角色或纪元在隔离途中变了")
	}
	if err := s.setEpoch(next, self, nil); err != nil {
		return granted, err
	}
	s.rep.kick()
	return granted, nil
}

// FenceAbandon 设为主机提交结果不明、冻结到期或管理员放弃时:先撤销给目标的交接标记(纪元 targetEpoch),
// 再以高于它的纪元继续当主机。目标之后以 targetEpoch 起来会被副机按纪元拒绝、自动变为副机,它那份数据不会生效。返回新纪元。
// 撤销只推给目标以外的副机;check 见 fenceTo(推完、升纪元前的最后一道检查:目标其实已经起来了、确认的台数不够,都不升)。
func (s *Service) FenceAbandon(target uint, targetEpoch uint64, check func(granted int) error) (uint64, error) {
	L := s.Epoch()
	next, err := NextEpoch(L, targetEpoch)
	if err != nil {
		return 0, err
	}
	var tn model.Node
	hasT := s.d.DB.Where("id = ? AND enabled = ?", target, true).Limit(1).Find(&tn).Error == nil && tn.Id != 0 && tn.ApiUrl != "" && tn.Token != ""
	granted, err := s.fenceTo(next, targetEpoch, s.nodesExcept(target), func(g int) error {
		if hasT && s.d.Hub != nil {
			_ = s.pushState(tn, 15*time.Second) // 目标若其实没接手、还是副机,也让它跟上;不计入确认台数。它若已是主机,回话就是证据
		}
		if check != nil {
			return check(g)
		}
		return nil
	})
	if err != nil {
		return 0, err
	}
	s.audit("ha", "fence-abandon", map[string]interface{}{"target": s.nodeName(target), "targetEpoch": targetEpoch, "epoch": next, "granted": granted})
	logger.Warning("容灾:设为主机结果不明,已以纪元 ", next, " 继续当主机,隔离了目标 ", s.nodeName(target), "(纪元 ", targetEpoch, ")")
	return next, nil
}

// ResetLocal 清掉本机的容灾状态(纪元、认的主机、钉住的公钥、票、交接标记、通知块、集群密钥、角色),删掉手里的副本与备用密钥,
// 纪元记录归零。服务不在跑也能用:m-ui ha reset、菜单切换角色、副机被主机删除(ha/leave)、副机重新生成令牌。
func ResetLocal(db *gorm.DB, dataDir string) error {
	values := map[string]string{}
	for _, k := range []string{KeyEpoch, KeyMasterId, KeyMasterPub, KeyState, KeyVote, KeyGrant, KeyNotify, KeyPeerKey, KeyRole} {
		values[k] = ""
	}
	if err := db.Transaction(func(tx *gorm.DB) error { return upsert(tx, values) }); err != nil {
		return err
	}
	dir := HADir(dataDir)
	if _, err := os.Stat(dir); err != nil {
		return nil
	}
	firstErr := dropStandbyFiles(dir, true) // 两代副本及其说明、备用密钥(含写到一半的)
	if err := writeEpochRecord(dir, epochRecord{At: time.Now().Unix()}); err != nil && firstErr == nil {
		firstErr = err
	}
	return firstErr
}

// LeaveCluster 运行中的副机退出集群(主机删除了本机、本机重新生成了令牌):同 ResetLocal,并清掉内存里的判定。
func (s *Service) LeaveCluster() error {
	s.storeMu.Lock()
	err := ResetLocal(s.d.DB, s.DataDir())
	s.storeMu.Unlock()
	s.mu.Lock()
	s.lastContact = s.now()
	s.core().notListed = time.Time{}
	s.mu.Unlock()
	s.eng.reset()
	s.invalidate()
	if err != nil {
		logger.Warning("容灾:退出集群时清理失败: ", err)
		return err
	}
	logger.Info("容灾:本机已退出原来的集群:纪元清零,副本与备用密钥已删")
	return nil
}
