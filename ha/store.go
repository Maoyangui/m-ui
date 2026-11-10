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
//	ha/replica.json    最新一份的信息(序号、时间、自检结果……)

// MaxReplica 副本信封的上限:主机侧超过 64 MB 就告警,这里留足余量。
const MaxReplica int64 = 256 << 20

const (
	fileKey     = "standby.key"
	fileCur     = "replica.cur"
	filePrev    = "replica.prev"
	fileTmp     = "replica.tmp"
	fileInfo    = "replica.json"
	selfCheckDT = 24 * time.Hour
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
	VerifiedAt  int64  `json:"verifiedAt,omitempty"`
	VerifyErr   string `json:"verifyErr,omitempty"`
	Sum         string `json:"sum"`
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
	if hd.To != Fingerprint(key.PubString()) {
		return fail(errors.New("副本不是封给本机的(主机记住的本机公钥和现在的不一样)"))
	}
	if cur, ok := s.replicaInfo(); ok && cur.Epoch == hd.Epoch && hd.Seq <= cur.Seq {
		return fail(fmt.Errorf("副本序号 %d 不比本机已有的 %d 新", hd.Seq, cur.Seq))
	}
	cur, prev := filepath.Join(s.Dir(), fileCur), filepath.Join(s.Dir(), filePrev)
	old, hadOld := s.replicaInfo()
	if _, err := os.Stat(cur); err == nil {
		_ = os.Remove(prev)
		if err := os.Rename(cur, prev); err != nil {
			return fail(err)
		}
	}
	if err := os.Rename(tmp, cur); err != nil {
		return fail(err)
	}
	syncDir(s.Dir())
	ri := ReplicaInfo{Epoch: hd.Epoch, Seq: hd.Seq, CreatedAt: hd.CreatedAt, Size: hd.Size, MasterId: hd.MasterId, MasterName: hd.MasterName,
		Version: hd.Version, MaxChangeId: hd.MaxChangeId, ReceivedAt: s.now().Unix(), AsOf: hd.CreatedAt,
		Sum: hex.EncodeToString(env.CtSum[:])}
	if err := s.saveReplicaInfo(ri); err != nil {
		return 0, err
	}
	if !hadOld || old.Epoch != hd.Epoch { // 新纪元的第一份当场自检一次(在后台,不拖住主机的请求)
		go func() {
			if err := s.SelfCheck(); err != nil {
				logger.Warning("容灾:副本自检失败: ", err)
			}
		}()
	}
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
	had := false
	for _, n := range []string{fileCur, filePrev, fileTmp, fileInfo} {
		p := filepath.Join(s.Dir(), n)
		if _, err := os.Stat(p); err == nil {
			had = true
			if err := os.Remove(p); err != nil {
				return err
			}
		}
	}
	if had {
		logger.Info("容灾:本机已不是备用,删掉了手里的副本")
	}
	return nil
}

// openReplica 解开本机的副本(cur 坏了用 prev)到 dir,返回 meta 与用的是哪一份。
func (s *Service) openReplica(dir string) (ReplicaMeta, *Envelope, string, error) {
	var lastErr error
	for _, name := range []string{fileCur, filePrev} {
		p := filepath.Join(s.Dir(), name)
		if _, err := os.Stat(p); err != nil {
			continue
		}
		meta, env, err := s.decryptReplica(p, dir)
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

// decryptReplica 验一份落了盘的副本(哈希、签名)、解密、解压到 dir。
func (s *Service) decryptReplica(p, dir string) (ReplicaMeta, *Envelope, error) {
	env, err := OpenEnvelopeFile(p)
	if err != nil {
		return ReplicaMeta{}, nil, err
	}
	if pub, err := ParseSignPub(s.setting(KeyMasterPub)); err == nil {
		if err := env.VerifySig(pub); err != nil {
			return ReplicaMeta{}, env, err
		}
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
	err := s.selfCheck(ri)
	s.storeMu.Lock()
	defer s.storeMu.Unlock()
	cur, ok := s.replicaInfo()
	if !ok || cur.Seq != ri.Seq || cur.Epoch != ri.Epoch {
		return err // 自检期间又收到了新的一份:结果作废,下一份自己再检
	}
	cur.VerifiedAt, cur.VerifyErr = s.now().Unix(), ""
	if err != nil {
		cur.VerifyErr = err.Error()
	}
	_ = s.saveReplicaInfo(cur)
	return err
}

func (s *Service) selfCheck(ri ReplicaInfo) error {
	dir, err := os.MkdirTemp(s.Dir(), "check-")
	if err != nil {
		return err
	}
	defer os.RemoveAll(dir)
	s.storeMu.Lock() // 解密期间别让新的一份把 cur 换掉
	meta, _, err := s.decryptReplica(filepath.Join(s.Dir(), fileCur), dir)
	s.storeMu.Unlock()
	if err != nil {
		return err
	}
	db, err := database.OpenReadOnly(filepath.Join(dir, "replica.db"))
	if err != nil {
		return fmt.Errorf("副本里的库打不开: %w", err)
	}
	defer closeDB(db)
	var res string
	if err := db.Raw("PRAGMA integrity_check").Scan(&res).Error; err != nil {
		return err
	}
	if res != "ok" {
		return fmt.Errorf("副本库完整性检查没过: %s", res)
	}
	for _, t := range []string{"users", "nodes", "settings", "admins"} {
		var n int64
		if err := db.Raw("SELECT COUNT(*) FROM " + t).Scan(&n).Error; err != nil {
			return fmt.Errorf("副本库读不了 %s: %w", t, err)
		}
		if want, ok := meta.Counts[t]; ok && want != n {
			return fmt.Errorf("副本库 %s 有 %d 行,和 meta 记的 %d 对不上", t, n, want)
		}
	}
	return nil
}

// selfCheckTick 每天自检一次。
func (s *Service) selfCheckTick() {
	if !s.IsNode() {
		return
	}
	ri, ok := s.replicaInfo()
	if !ok || s.now().Sub(time.Unix(ri.VerifiedAt, 0)) < selfCheckDT {
		return
	}
	if err := s.SelfCheck(); err != nil {
		logger.Warning("容灾:副本自检失败: ", err)
	}
}
