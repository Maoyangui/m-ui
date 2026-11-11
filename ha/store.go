package ha

import (
	"encoding/base64"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"os"
	"path/filepath"
	"time"

	"github.com/Maoyangui/m-ui/database"
	"github.com/Maoyangui/m-ui/logger"

	"github.com/shirou/gopsutil/v4/disk"
)

// 备用侧:副本只有一个写入方(storeMu),写成 <数据目录>/ha/ 下的独立文件,完全不碰 m-ui.db ——
// 不和配置应用、记账抢 SQLite 的写锁。保留两代:最新一份打不开时用上一份。
//
//	ha/standby.key     备用的 X25519 密钥(0600):主机把副本的数据密钥封在它的公钥下
//	ha/replica.cur     最新一份副本(信封,0600)
//	ha/replica.prev    上一份
//	ha/replica.tmp     正在收的
//	ha/replica.json    最新一份的信息(序号、时间、验签用的主机公钥、自检结果……)
//	ha/replica.prev.json  上一份的信息(打开上一份时用它记的主机公钥验签)

// MaxReplica 副本信封的上限:主机侧超过 64 MB 就告警,这里留足余量。
const MaxReplica int64 = 256 << 20

const (
	fileKey      = "standby.key"
	fileCur      = "replica.cur"
	filePrev     = "replica.prev"
	fileTmp      = "replica.tmp"
	fileInfo     = "replica.json"
	filePrevInfo = "replica.prev.json"
	selfCheckDT  = 24 * time.Hour
)

// selfCheckGap 收到新副本后在后台自检,两次之间至少隔这么久(副本内容常变,不能每份都解一遍)。
// selfCheckAsync 测试里关掉(后台自检和测试结束时删临时目录抢文件),要测它的测试自己打开并等它做完。
var (
	selfCheckGap   = 10 * time.Minute
	selfCheckAsync = true
)

// ReplicaInfo 本机手里那份副本的信息(随报告上报,面板显示)。
type ReplicaInfo struct {
	Epoch       uint64 `json:"epoch"`
	Seq         uint64 `json:"seq"`
	CreatedAt   int64  `json:"createdAt"`
	Size        int64  `json:"size"`
	MasterId    uint   `json:"masterId"`
	MasterName  string `json:"masterName"`
	Version     string `json:"version"`
	MaxChangeId uint64 `json:"maxChangeId"`
	ReceivedAt  int64  `json:"receivedAt"`
	AsOf        int64  `json:"asOf"` // 主机确认这份到什么时候为止仍是最新的
	// 最近一次自检(不随新副本清零:内容常变、每份都重发,清零的话面板几乎一直显示"没自检")
	VerifiedAt  int64  `json:"verifiedAt,omitempty"`
	VerifyErr   string `json:"verifyErr,omitempty"`
	VerifiedSeq uint64 `json:"verifiedSeq,omitempty"` // 自检的是哪一份
	Sum         string `json:"sum"`
	// MasterPub 收下时验签用的主机签名公钥:之后本机跟了别的主机,也照样能验这份
	MasterPub string `json:"masterPub,omitempty"`
	// DBSize 解开后的库多大(自检时量的;接任时磁盘要放得下)
	DBSize int64 `json:"dbSize,omitempty"`
}

// standbyKey 本机的备用密钥:没有就生成。
func (s *Service) standbyKey() (BoxKey, error) {
	p := filepath.Join(s.Dir(), fileKey)
	if b, err := os.ReadFile(p); err == nil {
		var raw struct{ Pub, Priv string }
		if json.Unmarshal(b, &raw) == nil {
			pub, e1 := ParsePub(raw.Pub)
			priv, e2 := ParsePub(raw.Priv)
			if e1 == nil && e2 == nil {
				return BoxKey{Pub: pub, Priv: priv}, nil
			}
		}
		logger.Warning("容灾:备用密钥文件坏了,重新生成(主机会把它当成公钥变了,要在主机的服务器列表里对本机「重新信任」)")
	}
	if err := ensureDir(s.Dir()); err != nil {
		return BoxKey{}, err
	}
	k, err := NewBoxKey()
	if err != nil {
		return k, err
	}
	b, _ := json.Marshal(map[string]string{"pub": k.PubString(), "priv": base64.StdEncoding.EncodeToString(k.Priv[:])})
	if err := writeFileAtomic(p, b); err != nil {
		return k, err
	}
	return k, nil
}

// writeFileAtomic 写临时文件、落盘、改名:断电时要么是旧的、要么是新的。
func writeFileAtomic(p string, b []byte) error {
	tmp := p + ".w"
	f, err := os.OpenFile(tmp, os.O_CREATE|os.O_TRUNC|os.O_WRONLY, 0o600)
	if err != nil {
		return err
	}
	if _, err := f.Write(b); err != nil {
		f.Close()
		return err
	}
	if err := f.Sync(); err != nil {
		f.Close()
		return err
	}
	if err := f.Close(); err != nil {
		return err
	}
	if err := os.Rename(tmp, p); err != nil {
		return err
	}
	syncDir(filepath.Dir(p))
	return nil
}

// syncDir 改名之后把目录项也落盘(Windows 上打不开目录,忽略)。
func syncDir(dir string) {
	if d, err := os.Open(dir); err == nil {
		_ = d.Sync()
		d.Close()
	}
}

// replicaInfo 本机副本的信息(没有副本时 ok=false)。
func (s *Service) replicaInfo() (ReplicaInfo, bool) {
	var ri ReplicaInfo
	b, err := os.ReadFile(filepath.Join(s.Dir(), fileInfo))
	if err != nil || json.Unmarshal(b, &ri) != nil {
		return ri, false
	}
	if _, err := os.Stat(filepath.Join(s.Dir(), fileCur)); err != nil {
		return ri, false
	}
	return ri, true
}

// replicaPrevInfo 上一份副本的信息。
func (s *Service) replicaPrevInfo() (ReplicaInfo, bool) {
	var ri ReplicaInfo
	b, err := os.ReadFile(filepath.Join(s.Dir(), filePrevInfo))
	if err != nil || json.Unmarshal(b, &ri) != nil {
		return ri, false
	}
	return ri, true
}

// replicaBytes 本机副本文件(两代)占了多少磁盘:主机算磁盘够不够时把它算回去(它们本来就是给容灾用的)。
func (s *Service) replicaBytes() int64 {
	var n int64
	for _, f := range []string{fileCur, filePrev} {
		if st, err := os.Stat(filepath.Join(s.Dir(), f)); err == nil {
			n += st.Size()
		}
	}
	return n
}

func (s *Service) saveReplicaInfo(ri ReplicaInfo) error {
	b, _ := json.MarshalIndent(ri, "", "  ")
	return writeFileAtomic(filepath.Join(s.Dir(), fileInfo), b)
}

// diskFree 数据目录所在盘的剩余空间(读不到为 -1)。
func (s *Service) diskFree() int64 {
	u, err := disk.Usage(s.DataDir())
	if err != nil {
		return -1
	}
	return int64(u.Free)
}

// ReceiveReplica 收下主机推来的一份副本(agent/ha/replica):边收边写临时文件、边算哈希,验签后原子换入。
// 纪元已经在 agent 鉴权里对齐过。返回收下的序号。
func (s *Service) ReceiveReplica(body io.Reader) (uint64, error) {
	s.storeMu.Lock()
	defer s.storeMu.Unlock()
	L := s.Epoch()
	mid, _ := s.masterId()
	pubB64 := s.setting(KeyMasterPub)
	if L == 0 || mid == 0 || pubB64 == "" {
		return 0, errors.New("本机还没收到主机的容灾状态,暂不收副本")
	}
	st, ok := s.NodeState()
	if !ok || st.rank() == 0 || st.Mode == ModeOff {
		return 0, errors.New("本机不在备用名单里,不收副本")
	}
	pub, err := ParseSignPub(pubB64)
	if err != nil {
		return 0, err
	}
	key, err := s.standbyKey()
	if err != nil {
		return 0, err
	}
	if err := ensureDir(s.Dir()); err != nil {
		return 0, err
	}
	if free := s.diskFree(); free >= 0 && free < 64<<20 {
		return 0, fmt.Errorf("磁盘剩余空间不足(只剩 %d MB),不收副本", free>>20)
	}
	tmp := filepath.Join(s.Dir(), fileTmp)
	f, err := os.OpenFile(tmp, os.O_CREATE|os.O_TRUNC|os.O_WRONLY, 0o600)
	if err != nil {
		return 0, err
	}
	env, err := ReceiveEnvelope(body, f, MaxReplica)
	if err == nil {
		err = f.Sync()
	}
	if cerr := f.Close(); err == nil {
		err = cerr
	}
	fail := func(e error) (uint64, error) {
		_ = os.Remove(tmp)
		return 0, e
	}
	if err != nil {
		return fail(err)
	}
	hd := env.Header
	switch {
	case hd.Kind != KindReplica:
		return fail(errors.New("这不是副本"))
	case hd.Epoch < L:
		return fail(fmt.Errorf("副本来自旧纪元 %d(本机在纪元 %d)", hd.Epoch, L))
	case hd.Epoch > L:
		return fail(fmt.Errorf("副本来自纪元 %d,本机还在纪元 %d", hd.Epoch, L))
	case hd.MasterId != mid:
		return fail(fmt.Errorf("副本来自服务器 #%d,不是本机认的主机 #%d", hd.MasterId, mid))
	}
	if err := env.VerifySig(pub); err != nil {
		return fail(err)
	}
	old, hadOld := s.replicaInfo()
	// 接任时要放得下:解开的库(至少和压缩包一样大)、新库一份、压缩包本身。上一份收下后会删,它占的算回来
	dbHint := hd.Size
	if hadOld && old.MasterId == hd.MasterId && old.DBSize > 0 && old.Size > 0 {
		dbHint = max(dbHint, old.DBSize*hd.Size/old.Size)
	}
	if free := s.diskFree(); free >= 0 {
		var prevSize int64
		if st, err := os.Stat(filepath.Join(s.Dir(), filePrev)); err == nil {
			prevSize = st.Size()
		}
		if need := TakeoverNeed(hd.Size, dbHint); free+prevSize < need {
			return fail(fmt.Errorf("磁盘剩余空间不够:接任时要约 %d MB,只剩 %d MB,不收副本", need>>20, (free+prevSize)>>20))
		}
	}
	if hd.To != Fingerprint(key.PubString()) {
		return fail(errors.New("副本不是封给本机的(主机记住的本机公钥和现在的不一样)"))
	}
	if cur, ok := s.replicaInfo(); ok && cur.Epoch == hd.Epoch && hd.Seq <= cur.Seq {
		return fail(fmt.Errorf("副本序号 %d 不比本机已有的 %d 新", hd.Seq, cur.Seq))
	}
	cur, prev := filepath.Join(s.Dir(), fileCur), filepath.Join(s.Dir(), filePrev)
	if _, err := os.Stat(cur); err == nil {
		_ = os.Remove(prev)
		_ = os.Remove(filepath.Join(s.Dir(), filePrevInfo))
		if err := os.Rename(cur, prev); err != nil {
			return fail(err)
		}
		if hadOld {
			b, _ := json.MarshalIndent(old, "", "  ")
			_ = writeFileAtomic(filepath.Join(s.Dir(), filePrevInfo), b)
		}
	}
	if err := os.Rename(tmp, cur); err != nil {
		return fail(err)
	}
	syncDir(s.Dir())
	ri := ReplicaInfo{Epoch: hd.Epoch, Seq: hd.Seq, CreatedAt: hd.CreatedAt, Size: hd.Size, MasterId: hd.MasterId, MasterName: hd.MasterName,
		Version: hd.Version, MaxChangeId: hd.MaxChangeId, ReceivedAt: s.now().Unix(), AsOf: hd.CreatedAt,
		Sum: hex.EncodeToString(env.CtSum[:]), MasterPub: pubB64}
	if hadOld && old.MasterId == hd.MasterId { // 最近一次自检的结果留着,等这一份自己检完再换
		ri.VerifiedAt, ri.VerifyErr, ri.VerifiedSeq, ri.DBSize = old.VerifiedAt, old.VerifyErr, old.VerifiedSeq, old.DBSize
	}
	if err := s.saveReplicaInfo(ri); err != nil {
		return 0, err
	}
	s.selfCheckSoon() // 每份都在后台自检(限频),不拖住主机的请求
	return hd.Seq, nil
}

// noteReplicaMark 主机确认本机序号为 Seq 的副本到 At 为止仍是最新的。
func (s *Service) noteReplicaMark(m ReplicaMark) {
	s.storeMu.Lock()
	defer s.storeMu.Unlock()
	ri, ok := s.replicaInfo()
	if !ok || ri.Seq != m.Seq || m.At <= ri.AsOf {
		return
	}
	ri.AsOf = m.At
	_ = s.saveReplicaInfo(ri)
}

// DropReplica 删掉本机保存的副本(不再是备用、主机关了容灾、本机被停用、本机成了主机)。备用密钥留着:公钥不变,以后再当备用不用重新信任。
func (s *Service) DropReplica() error {
	s.storeMu.Lock()
	defer s.storeMu.Unlock()
	had, err := dropStandbyFilesIn(s.Dir(), false)
	if had {
		logger.Info("容灾:本机已不是备用,删掉了手里的副本")
	}
	return err
}

// DropStandbyAll 删掉副本连同备用密钥(本机被主机删除 / 离开集群、重置容灾时;A 的 LeaveCluster 调它)。
func (s *Service) DropStandbyAll() error {
	s.storeMu.Lock()
	defer s.storeMu.Unlock()
	had, err := dropStandbyFilesIn(s.Dir(), true)
	if had {
		logger.Info("容灾:删掉了手里的副本与备用密钥")
	}
	return err
}

// dropStandbyFiles 删掉容灾目录里的副本文件;withKey 时连备用密钥一起删(服务不在跑时也能用,如 ResetLocal)。
func dropStandbyFiles(dir string, withKey bool) error {
	_, err := dropStandbyFilesIn(dir, withKey)
	return err
}

func dropStandbyFilesIn(dir string, withKey bool) (bool, error) {
	names := []string{fileCur, filePrev, fileTmp, fileInfo, filePrevInfo}
	if withKey {
		names = append(names, fileKey, fileKey+".w")
	}
	had := false
	for _, n := range names {
		p := filepath.Join(dir, n)
		if _, err := os.Stat(p); err == nil {
			had = true
			if err := os.Remove(p); err != nil {
				return had, err
			}
		}
	}
	return had, nil
}

// openReplica 解开本机的副本(cur 坏了用 prev)到 dir,返回 meta 与用的是哪一份。
func (s *Service) openReplica(dir string) (ReplicaMeta, *Envelope, string, error) {
	var lastErr error
	for _, name := range []string{fileCur, filePrev} {
		p := filepath.Join(s.Dir(), name)
		if _, err := os.Stat(p); err != nil {
			continue
		}
		meta, env, err := s.decryptReplica(p, dir, s.replicaPubFor(name))
		if err == nil {
			return meta, env, name, nil
		}
		lastErr = fmt.Errorf("%s: %w", name, err)
		logger.Warning("容灾:副本 ", name, " 打不开: ", err)
	}
	if lastErr == nil {
		lastErr = errors.New("本机没有数据副本:主机还没把副本推过来,或本机不是备用")
	}
	return ReplicaMeta{}, nil, "", lastErr
}

// replicaPubFor 验某一份副本用的主机公钥:收下时记的那把;老格式没记的,用本机现在钉住的。
func (s *Service) replicaPubFor(name string) string {
	var ri ReplicaInfo
	var ok bool
	if name == filePrev {
		ri, ok = s.replicaPrevInfo()
	} else {
		ri, ok = s.replicaInfo()
	}
	if ok && ri.MasterPub != "" {
		return ri.MasterPub
	}
	return s.setting(KeyMasterPub)
}

// decryptReplica 验一份落了盘的副本(哈希、签名)、解密、解压到 dir。没有验签用的公钥一律不用(不跳过验签)。
func (s *Service) decryptReplica(p, dir, pubB64 string) (ReplicaMeta, *Envelope, error) {
	env, err := OpenEnvelopeFile(p)
	if err != nil {
		return ReplicaMeta{}, nil, err
	}
	pub, err := ParseSignPub(pubB64)
	if err != nil {
		return ReplicaMeta{}, env, errors.New("不知道签发这份副本的主机公钥,不能验签,不用它")
	}
	if err := env.VerifySig(pub); err != nil {
		return ReplicaMeta{}, env, err
	}
	key, err := s.standbyKey()
	if err != nil {
		return ReplicaMeta{}, env, err
	}
	dataKey, err := OpenWith(env.Header.Wrap, key)
	if err != nil {
		return ReplicaMeta{}, env, err
	}
	if err := os.MkdirAll(dir, 0o700); err != nil {
		return ReplicaMeta{}, env, err
	}
	zp := filepath.Join(dir, "replica.zip")
	zf, err := os.OpenFile(zp, os.O_CREATE|os.O_TRUNC|os.O_WRONLY, 0o600)
	if err != nil {
		return ReplicaMeta{}, env, err
	}
	err = env.DecryptFile(p, dataKey, zf)
	if cerr := zf.Close(); err == nil {
		err = cerr
	}
	if err != nil {
		return ReplicaMeta{}, env, err
	}
	meta, err := unzipReplica(zp, dir)
	_ = os.Remove(zp)
	return meta, env, err
}

// SelfCheck 自检本机的副本:解开、跑 integrity_check、数一遍行数,结果随报告上报。
func (s *Service) SelfCheck() error {
	s.storeMu.Lock()
	ri, ok := s.replicaInfo()
	s.storeMu.Unlock()
	if !ok {
		return nil
	}
	s.rep.noteCheck(s.now())
	dbSize, err := s.selfCheck(ri)
	s.storeMu.Lock()
	defer s.storeMu.Unlock()
	cur, ok := s.replicaInfo()
	if !ok || cur.Seq != ri.Seq || cur.Epoch != ri.Epoch {
		return err // 自检期间又收到了新的一份:结果作废,新的一份另有一次自检
	}
	cur.VerifiedAt, cur.VerifyErr, cur.VerifiedSeq = s.now().Unix(), "", cur.Seq
	if dbSize > 0 {
		cur.DBSize = dbSize
	}
	if err != nil {
		cur.VerifyErr = err.Error()
	}
	_ = s.saveReplicaInfo(cur)
	return err
}

// selfCheckSoon 收到新副本后:离上次自检够久就马上在后台检,否则到点再检(同一时间最多排一次)。
func (s *Service) selfCheckSoon() {
	if !selfCheckAsync {
		return
	}
	wait, ok := s.rep.scheduleCheck(s.now())
	if !ok {
		return
	}
	run := func() {
		defer s.rep.checkDone()
		if s.stopping() {
			return
		}
		if err := s.SelfCheck(); err != nil {
			logger.Warning("容灾:副本自检失败: ", err)
		}
	}
	if wait <= 0 {
		go run()
		return
	}
	time.AfterFunc(wait, run)
}

func (s *Service) selfCheck(ri ReplicaInfo) (int64, error) {
	dir, err := os.MkdirTemp(s.Dir(), "check-")
	if err != nil {
		return 0, err
	}
	defer os.RemoveAll(dir)
	s.storeMu.Lock() // 解密期间别让新的一份把 cur 换掉
	meta, _, err := s.decryptReplica(filepath.Join(s.Dir(), fileCur), dir, s.replicaPubFor(fileCur))
	s.storeMu.Unlock()
	if err != nil {
		return 0, err
	}
	dbPath := filepath.Join(dir, "replica.db")
	var dbSize int64
	if st, err := os.Stat(dbPath); err == nil {
		dbSize = st.Size()
	}
	db, err := database.OpenReadOnly(dbPath)
	if err != nil {
		return dbSize, fmt.Errorf("副本里的库打不开: %w", err)
	}
	defer closeDB(db)
	var res string
	if err := db.Raw("PRAGMA integrity_check").Scan(&res).Error; err != nil {
		return dbSize, err
	}
	if res != "ok" {
		return dbSize, fmt.Errorf("副本库完整性检查没过: %s", res)
	}
	for _, t := range []string{"users", "nodes", "settings", "admins"} {
		var n int64
		if err := db.Raw("SELECT COUNT(*) FROM " + t).Scan(&n).Error; err != nil {
			return dbSize, fmt.Errorf("副本库读不了 %s: %w", t, err)
		}
		if want, ok := meta.Counts[t]; ok && want != n {
			return dbSize, fmt.Errorf("副本库 %s 有 %d 行,和 meta 记的 %d 对不上", t, n, want)
		}
	}
	return dbSize, nil
}

// selfCheckTick 兜底:最新一份还没检过(收下时排的那次被重启打断了)就检;检过的每天再检一次。
func (s *Service) selfCheckTick() {
	if !s.IsNode() {
		return
	}
	ri, ok := s.replicaInfo()
	if !ok {
		return
	}
	if ri.VerifiedSeq != ri.Seq {
		s.selfCheckSoon()
		return
	}
	if s.now().Sub(time.Unix(ri.VerifiedAt, 0)) < selfCheckDT {
		return
	}
	if err := s.SelfCheck(); err != nil {
		logger.Warning("容灾:副本自检失败: ", err)
	}
}
