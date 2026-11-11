package ha

import (
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
	"os"
	"path/filepath"
	"strings"
	"sync"
	"time"

	"github.com/Maoyangui/m-ui/logger"
	"github.com/Maoyangui/m-ui/selfupdate"
)

// 被设为主机的副机这一边:给出一次性公钥与本机以后的签名公钥 → 收整库数据(必须是本机钉住的主机签的)、验证、组好新库暂存
// → 提交时换上、重启。一次性私钥与新签名密钥只在内存里;原主机给的期限(冻结时长再加 5 分钟)内没提交就丢掉暂存
// (进程重启也等于丢掉:私钥没了,原主机那边会失败、解冻)。提交与丢弃在同一把锁下:丢掉了的就不可能再提交。

// MaxBundle 设为主机时整库数据的上限。
const MaxBundle int64 = 32 << 30

// incomingTTL 原主机没说期限时暂存多久没提交就丢掉;incomingTTLMax 最多留多久。
var (
	incomingTTL    = 20 * time.Minute
	incomingTTLMax = 2 * time.Hour
)

// promoteMarkTTL 提交以后多久之内重启才换上新库(过期的不再换上:原主机那时多半已经隔离了本机)。
const promoteMarkTTL = 10 * time.Minute

const (
	incPrepared  = "prepared"
	incStaged    = "staged"
	incCommitted = "committed"
)

// PrepareRequest 原主机要一次性公钥时带的东西。
type PrepareRequest struct {
	Master  Link   `json:"master"` // 原主机变为副机后别人该怎么连:本机试着连一下,连不上提前告诉它
	Version string `json:"version"`
	Size    int64  `json:"size"`
	TTL     int64  `json:"ttl,omitempty"` // 暂存多久(秒);0 = 旧默认
}

// PrepareReply 本机给的一次性公钥。
type PrepareReply struct {
	Pub         string `json:"pub"`
	Nonce       string `json:"nonce"`
	Version     string `json:"version"`
	DiskFree    int64  `json:"diskFree"`
	MasterReach string `json:"masterReach,omitempty"` // 空 = 连得上原主机的面板地址;否则是原因
	Epoch       uint64 `json:"epoch"`
	MasterPubFP string `json:"masterPubFp"`
	SignPub     string `json:"signPub"`
}

// IncomingState 暂存的进度(本机设置 haIncoming,不含任何密钥)。
type IncomingState struct {
	Stage   string `json:"stage"`
	From    uint   `json:"from"`
	Expires int64  `json:"expires"`
	Users   int64  `json:"users,omitempty"`
	Size    int64  `json:"size,omitempty"`
}

type incoming struct {
	s        *Service
	mu       sync.Mutex
	key      *BoxKey
	signSeed string // 本机成为主机后用的签名密钥(prepare 时生成,组新库时写进去)
	nonce    string
	from     uint
	expires  time.Time
	stage    string
	epoch    uint64 // 组好的新库的纪元
}

func (in *incoming) reset() {
	in.key, in.signSeed, in.nonce, in.from, in.stage, in.expires, in.epoch = nil, "", "", 0, "", time.Time{}, 0
}

func (s *Service) stagedFile() string { return promoteFile(s.d.DBPath) + ".staged" }

// masterOfRecord 本机认的主机:进过纪元的看 haMasterId,没进过的看最近一份配置是谁推的。
func (s *Service) masterOfRecord() uint {
	if id, ok := s.masterId(); ok {
		return id
	}
	return uint(atoiDef(s.setting("hubMasterId"), 0))
}

// Prepare 原主机要设本机为主机:生成一次性密钥,检查能不能接手。
func (s *Service) Prepare(req PrepareRequest) (PrepareReply, error) {
	if !s.IsNode() {
		return PrepareReply{}, errors.New("本机不是副机")
	}
	if s.setting(KeyRole) == RolePromoting {
		return PrepareReply{}, errors.New("本机正在接任为主机")
	}
	if req.Version != "" && selfupdate.Newer(req.Version, s.d.Version) {
		return PrepareReply{}, fmt.Errorf("本机版本 v%s 比主机的 v%s 旧,先把本机升级", s.d.Version, req.Version)
	}
	free := s.diskFree()
	if need := 3*req.Size + (64 << 20); free >= 0 && free < need {
		return PrepareReply{}, fmt.Errorf("本机磁盘剩余 %d MB,不够放主机的数据(至少要 %d MB)", free>>20, need>>20)
	}
	k, err := NewBoxKey()
	if err != nil {
		return PrepareReply{}, err
	}
	nb := make([]byte, 16)
	if _, err := rand.Read(nb); err != nil {
		return PrepareReply{}, err
	}
	nonce := hex.EncodeToString(nb)
	seed := NewSignKey()
	sk, err := SignKey(seed)
	if err != nil {
		return PrepareReply{}, err
	}
	ttl := incomingTTL
	if req.TTL > 0 {
		ttl = min(max(time.Duration(req.TTL)*time.Second, 5*time.Minute), incomingTTLMax)
	}
	in := s.inc
	in.mu.Lock()
	if in.stage == incCommitted || s.committedPending() {
		in.mu.Unlock()
		return PrepareReply{}, errAlreadyCommitted
	}
	in.cleanupLocked()
	in.key, in.signSeed, in.nonce, in.from, in.stage, in.expires = &k, seed, nonce, s.masterOfRecord(), incPrepared, s.now().Add(ttl)
	in.persistLocked(0, 0)
	in.mu.Unlock()
	// 本机认的纪元与主机签名公钥:原主机核对过才往下做(整库要靠它验签)
	rep := PrepareReply{Pub: k.PubString(), Nonce: nonce, Version: s.d.Version, DiskFree: free, SignPub: PubOf(sk),
		Epoch: s.Epoch(), MasterPubFP: Fingerprint(strings.TrimSpace(s.setting(KeyMasterPub)))}
	if req.Master.ApiUrl != "" {
		if err := probeLink(req.Master, 8*time.Second); err != nil {
			rep.MasterReach = err.Error()
		}
	}
	logger.Info("容灾:主机要把本机设为主机,已给出一次性公钥")
	return rep, nil
}

func (in *incoming) persistLocked(users, size int64) {
	st := IncomingState{Stage: in.stage, From: in.from, Expires: in.expires.Unix(), Users: users, Size: size}
	v := mustJSON(st)
	if in.stage == "" {
		v = ""
	}
	_ = in.s.set(map[string]string{KeyIncoming: v})
}

// cleanupLocked 丢掉暂存的数据与一次性密钥。
func (in *incoming) cleanupLocked() {
	s := in.s
	_ = os.Remove(s.stagedFile())
	_ = os.RemoveAll(filepath.Join(s.Dir(), "incoming"))
	_ = os.Remove(filepath.Join(s.Dir(), "incoming.env"))
	in.reset()
}

// ReceiveBundle 收下原主机推来的整库数据:验证、解开、组好新库暂存(还不换上)。
func (s *Service) ReceiveBundle(body io.Reader) (IncomingState, error) {
	in := s.inc
	in.mu.Lock()
	defer in.mu.Unlock()
	if in.key == nil || in.stage != incPrepared || s.now().After(in.expires) {
		return IncomingState{}, errors.New("本机没有在等主机的数据(没要过一次性公钥,或已经过期):重新发起设为主机")
	}
	if err := ensureDir(s.Dir()); err != nil {
		return IncomingState{}, err
	}
	envPath := filepath.Join(s.Dir(), "incoming.env")
	f, err := os.OpenFile(envPath, os.O_CREATE|os.O_TRUNC|os.O_WRONLY, 0o600)
	if err != nil {
		return IncomingState{}, err
	}
	env, err := ReceiveEnvelope(body, f, MaxBundle)
	if err == nil {
		err = f.Sync()
	}
	if cerr := f.Close(); err == nil {
		err = cerr
	}
	fail := func(e error) (IncomingState, error) {
		_ = os.Remove(envPath)
		return IncomingState{}, e
	}
	if err != nil {
		return fail(err)
	}
	hd := env.Header
	switch {
	case hd.Kind != KindBundle:
		return fail(errors.New("这不是主机的整库数据"))
	case hd.To != Fingerprint(in.key.PubString()) || hd.Nonce != in.nonce:
		return fail(errors.New("数据不是封给本机这一次的(一次性公钥或编号对不上)"))
	case in.from != 0 && hd.MasterId != in.from:
		return fail(fmt.Errorf("数据来自服务器 #%d,不是本机的主机 #%d", hd.MasterId, in.from))
	}
	// 只认本机钉住的主机签名公钥签的数据:没钉住就拒收(不跳过验签),纪元也要对得上
	pub, err := ParseSignPub(s.setting(KeyMasterPub))
	if err != nil {
		return fail(errNoPinnedPub)
	}
	if err := env.VerifySig(pub); err != nil {
		return fail(err)
	}
	if L := s.Epoch(); hd.Epoch != L {
		return fail(fmt.Errorf("数据来自纪元 %d,本机在纪元 %d", hd.Epoch, L))
	}
	dataKey, err := OpenWith(hd.Wrap, *in.key)
	if err != nil {
		return fail(err)
	}
	dir := filepath.Join(s.Dir(), "incoming")
	_ = os.RemoveAll(dir)
	if err := os.MkdirAll(dir, 0o700); err != nil {
		return fail(err)
	}
	zp := filepath.Join(dir, "bundle.zip")
	zf, err := os.OpenFile(zp, os.O_CREATE|os.O_TRUNC|os.O_WRONLY, 0o600)
	if err != nil {
		return fail(err)
	}
	err = env.DecryptFile(envPath, dataKey, zf)
	if cerr := zf.Close(); err == nil {
		err = cerr
	}
	_ = os.Remove(envPath)
	if err != nil {
		return fail(err)
	}
	meta, err := unzipReplica(zp, dir)
	_ = os.Remove(zp)
	if err != nil {
		return fail(err)
	}
	if !meta.Full {
		return fail(errors.New("这不是整库数据"))
	}
	if err := checkDB(filepath.Join(dir, "replica.db"), meta); err != nil {
		return fail(err)
	}
	next, err := NextEpoch(s.Epoch(), hd.Epoch)
	if err != nil {
		return fail(err)
	}
	b := Became{How: HowSetMaster, Epoch: next, FormerId: meta.MasterId, FormerName: meta.MasterName, At: s.now().Unix(),
		MaxChangeId: meta.MaxChangeId, By: "panel", FormerIP: meta.Identity.PublicIP, FormerIP6: meta.Identity.PublicIP6}
	if err := s.buildPromote(dir, meta, b, true, promoteOpts{SignSeed: in.signSeed}); err != nil {
		return fail(err)
	}
	_ = os.RemoveAll(dir)
	in.stage, in.epoch = incStaged, next
	in.persistLocked(meta.Counts["users"], hd.Size)
	logger.Info("容灾:主机的数据已收下并组好新库(", meta.Counts["users"], " 个用户),等提交")
	return IncomingState{Stage: incStaged, From: in.from, Expires: in.expires.Unix(), Users: meta.Counts["users"], Size: hd.Size}, nil
}

// checkDB 解出来的库完整、行数和 meta 对得上。
func checkDB(path string, meta ReplicaMeta) error {
	db, err := openScratch(path)
	if err != nil {
		return fmt.Errorf("数据里的库打不开: %w", err)
	}
	defer closeDB(db)
	var res string
	if err := db.Raw("PRAGMA integrity_check").Scan(&res).Error; err != nil {
		return err
	}
	if res != "ok" {
		return fmt.Errorf("数据里的库完整性检查没过: %s", res)
	}
	for _, t := range []string{"users", "nodes", "settings", "admins"} {
		var n int64
		if err := db.Raw("SELECT COUNT(*) FROM " + t).Scan(&n).Error; err != nil {
			return fmt.Errorf("数据里的库读不了 %s: %w", t, err)
		}
		if want, ok := meta.Counts[t]; ok && want != n {
			return fmt.Errorf("数据里的库 %s 有 %d 行,和记录的 %d 对不上", t, n, want)
		}
	}
	return nil
}

// Commit 原主机让本机提交:换上暂存的新库(下次启动时),马上重启,以新纪元接手。
func (s *Service) Commit(nonce string) error {
	in := s.inc
	in.mu.Lock()
	defer in.mu.Unlock()
	if in.stage != incStaged || in.nonce == "" || nonce != in.nonce {
		return errors.New("本机没有可提交的数据(编号对不上,或已经丢弃)")
	}
	if s.now().After(in.expires) {
		in.cleanupLocked()
		in.persistLocked(0, 0)
		return errors.New("暂存的数据已经过期")
	}
	// 先写时效标记再换名:提交以后 10 分钟内重启才换上新库(过期的启动时改名留档,不再生效)
	mark, _ := json.Marshal(map[string]interface{}{"epoch": in.epoch, "how": HowSetMaster, "expires": s.now().Add(promoteMarkTTL).Unix()})
	if err := writeFileAtomic(promoteMarkFile(s.d.DBPath), mark); err != nil {
		return err
	}
	if err := os.Rename(s.stagedFile(), promoteFile(s.d.DBPath)); err != nil {
		_ = os.Remove(promoteMarkFile(s.d.DBPath))
		return err
	}
	syncDir(filepath.Dir(s.d.DBPath))
	in.key, in.signSeed = nil, "" // 一次性私钥用完即弃;签名密钥已经在新库里
	in.stage = incCommitted
	in.persistLocked(0, 0)
	_ = s.set(map[string]string{KeyRole: RolePromoting})
	logger.Warning("容灾:已提交,本机马上重启并以主机身份启动")
	s.restart(2 * time.Second)
	return nil
}

// Abort 原主机说不做了:丢掉暂存。和提交同一把锁:这里回了成功,这一次就不可能再提交。
// 已经提交了的不能丢(回错误);什么都没暂存(过期丢掉了、进程重启过)也算成功:本来就不会再提交。
func (s *Service) Abort(nonce string) error {
	in := s.inc
	in.mu.Lock()
	defer in.mu.Unlock()
	if in.stage == incCommitted || s.committedPending() {
		return errAlreadyCommitted
	}
	if in.nonce == "" {
		return nil
	}
	if nonce != in.nonce {
		return errors.New("编号对不上")
	}
	in.cleanupLocked()
	in.persistLocked(0, 0)
	logger.Info("容灾:主机取消了设为主机,暂存的数据已丢弃")
	return nil
}

var (
	errAlreadyCommitted = errors.New("本机已经提交了,正在以主机身份重启")
	errNoPinnedPub      = errors.New("本机还没钉住主机的签名公钥(没收到主机的容灾状态),不收整库数据:等主机推送一轮以后再试")
)

// promoteMarkFile 提交时写下的时效标记(<库>.promote.json):过期的新库启动时不换上。
func promoteMarkFile(dbPath string) string { return promoteFile(dbPath) + ".json" }

// committedPending 上次已经提交、新库还在等着换上(进程重启过却还是副机 = 换库没成,下次启动可能再换)。
func (s *Service) committedPending() bool {
	var st IncomingState
	if !s.getJSON(KeyIncoming, &st) || st.Stage != incCommitted {
		return false
	}
	_, err := os.Stat(promoteFile(s.d.DBPath))
	return err == nil
}

// IncomingStage 暂存到了哪一步(原主机提交没回话时来问)。只说这个进程里的:提交后重启过还是副机(新库没换上),
// 说不出"已提交"—— 原主机这时让本机丢弃会被拒(新库还在,下次启动可能换上),它按结果不明处理,到期隔离本机。
func (s *Service) IncomingStage() string {
	in := s.inc
	in.mu.Lock()
	defer in.mu.Unlock()
	return in.stage
}

// tick 每 5 秒:暂存过期就丢掉;进程重启过的(一次性私钥没了)把残留清掉。
func (in *incoming) tick() {
	s := in.s
	in.mu.Lock()
	defer in.mu.Unlock()
	if in.stage != "" && in.stage != incCommitted && s.now().After(in.expires) {
		logger.Info("容灾:等主机的数据超时,丢掉暂存")
		in.cleanupLocked()
		in.persistLocked(0, 0)
		return
	}
	if in.stage == "" && s.setting(KeyIncoming) != "" {
		if s.committedPending() {
			return // 已提交,等重启换上
		}
		// 暂存随进程重启没了,或提交过的新库已经换上 / 过期没换:清掉记录(原主机来问时说没有暂存)
		in.cleanupLocked()
		in.persistLocked(0, 0)
	}
}

// probeLink 试着连一下一台机器的面板接口(GET <API 地址>/api/agent/ping,不带令牌):回了任何 HTTP 响应就算连得上。
// 带了证书指纹就只认这个指纹。
func probeLink(l Link, timeout time.Duration) error {
	u := strings.TrimRight(strings.TrimSpace(l.ApiUrl), "/")
	if !strings.HasSuffix(u, "/api") {
		u += "/api"
	}
	u += "/agent/ping"
	tr := &http.Transport{TLSClientConfig: pinnedTLS(l.CertFP)}
	defer tr.CloseIdleConnections()
	c := &http.Client{Transport: tr, Timeout: timeout}
	ctx, cancel := context.WithTimeout(context.Background(), timeout)
	defer cancel()
	req, err := http.NewRequestWithContext(ctx, http.MethodGet, u, nil)
	if err != nil {
		return err
	}
	resp, err := c.Do(req)
	if err != nil {
		return err
	}
	resp.Body.Close()
	return nil
}

// pinnedTLS 只认指定证书指纹的 TLS 设置(指纹为空时不校验证书:自签的面板也连得上,只用来判断通不通)。
func pinnedTLS(fp string) *tls.Config {
	fp = strings.ToLower(strings.TrimSpace(fp))
	return &tls.Config{
		InsecureSkipVerify: true,
		VerifyPeerCertificate: func(raw [][]byte, _ [][]*x509.Certificate) error {
			if fp == "" {
				return nil
			}
			if len(raw) == 0 {
				return errors.New("对方没有出示证书")
			}
			sum := sha256.Sum256(raw[0])
			if hex.EncodeToString(sum[:]) != fp {
				return errors.New("对方的证书指纹和记住的不一样")
			}
			return nil
		},
	}
}

// IncomingView 面板显示用。
func (s *Service) incomingView() *IncomingState {
	var st IncomingState
	if !s.getJSON(KeyIncoming, &st) || st.Stage == "" {
		return nil
	}
	return &st
}
