package ha

import (
	"archive/zip"
	"context"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"os"
	"path/filepath"
	"strconv"
	"strings"
	"sync"
	"time"

	"github.com/Maoyangui/m-ui/database/model"
	"github.com/Maoyangui/m-ui/hub"
	"github.com/Maoyangui/m-ui/logger"
	"github.com/Maoyangui/m-ui/safe"

	"gorm.io/gorm"
)

// 历史数据:流量时序(stats)、订阅访问日志(sub_logs)、审计日志(changes,最近 20 万条)。
//
// 副本每隔几分钟一份,不带这几张表(大;流量时序每分钟都在变,带上等于每份都要整份重发)。可接任后没有它们,用起来就不对了:
// 面板和订阅落地页的流量图、外部 API 的今日 / 本月 / 按天流量、每日摘要都从零开始,订阅日志和 7 天以前的审计日志也没了。
// 所以另外低频发一份(默认每小时,库大时放宽,最多 6 小时)给已经收下这一任副本的备用:和副本同一套信封 ——
// 数据密钥只封给那台备用钉住的公钥、当前纪元的主机签名;备用照副本的规矩验(纪元、主机、签名、收件人、序号只增)
// 后存成 ha/history.cur(一代就够)。接任时并进新库;哪一步不成(没有、打不开、不是同一任主机的、磁盘不够)都不挡接任,
// 只在成为主机的提示和通知里说一声:用户的已用流量、到期时间这些在副本里,不受影响。
// 设为主机推的是整库(本来就带着这些),不走这里。

const (
	fileHist     = "history.cur"
	fileHistTmp  = "history.tmp"
	fileHistInfo = "history.json"
)

// MaxHistory 历史数据信封的上限。
const MaxHistory int64 = 1 << 30

// historyChangeMax 审计日志最多带最近这么多条(副本只带 7 天、2 万条)。
const historyChangeMax = 200000

// historyTables 历史数据带的表。
var historyTables = map[string]bool{"stats": true, "sub_logs": true, "changes": true}

// 节奏(测试里改)。
var (
	historyEvery    = time.Hour        // 默认多久给每台备用一份
	historyEveryMax = 6 * time.Hour    // 库大时最多放宽到
	historyRetry    = 30 * time.Minute // 发失败、备用收不下:多久以后再试
	historyMinGap   = 10 * time.Minute // 两次生成之间至少隔这么久
)

// HistoryMeta 历史数据 zip 里的 meta.json。
type HistoryMeta struct {
	V           int              `json:"v"`
	Version     string           `json:"version"`
	CreatedAt   int64            `json:"createdAt"`
	MasterId    uint             `json:"masterId"`
	MasterName  string           `json:"masterName"`
	Epoch       uint64           `json:"epoch"`
	MaxChangeId uint64           `json:"maxChangeId"`
	Counts      map[string]int64 `json:"counts"`
	Sum         string           `json:"sum"` // 内容指纹:内容没变不重发
	DBSize      int64            `json:"dbSize"`
}

// HistoryInfo 备用手里那份历史数据的信息(ha/history.json)。
type HistoryInfo struct {
	Epoch      uint64 `json:"epoch"`
	Seq        uint64 `json:"seq"`
	CreatedAt  int64  `json:"createdAt"`
	Size       int64  `json:"size"`   // 明文(zip)字节数
	DBSize     int64  `json:"dbSize"` // 解开后的库
	MasterId   uint   `json:"masterId"`
	MasterName string `json:"masterName"`
	ReceivedAt int64  `json:"receivedAt"`
	MasterPub  string `json:"masterPub"` // 收下时验签用的主机公钥:接任时照它再验一遍
}

// HistoryNote 接任时并进新库的历史数据(记在 haBecame 里,成为主机的提示据此说明)。
type HistoryNote struct {
	At      int64 `json:"at"` // 历史数据截至
	Stats   int64 `json:"stats"`
	SubLogs int64 `json:"subLogs"`
	Changes int64 `json:"changes"`
}

// historyNeed 接任时并历史数据还要多少磁盘(信封本身已经在盘上):解开的压缩包、解出的库、并进新库的一份。
// 库多大不知道时按压缩包的 4 倍估。
func historyNeed(zipSize, dbSize int64) int64 {
	if dbSize <= 0 {
		dbSize = 4 * zipSize
	}
	return zipSize + 2*dbSize
}

// 不能带历史数据的原因(成为主机的提示里按代码翻译,见 identityWarnText)。
const (
	idwHistNone  = "historyNone"  // 本机没收到过历史数据
	idwHistOther = "historyOther" // 手里那份来自别的主机或别的纪元:{t}
	idwHistDisk  = "historyDisk"  // 磁盘不够并进来:{need, free}(MB)
	idwHistFail  = "historyFail"  // 打不开 / 并不进来:{err}
)

// ---- 主机:生成与发送 ----

// buildHistory 在 dir 下生成一份历史数据(明文 zip),返回 zip 路径与 meta。
func (s *Service) buildHistory(dir string) (string, HistoryMeta, error) {
	meta := HistoryMeta{V: 1, Version: s.d.Version, CreatedAt: s.now().Unix(), MasterId: s.SelfId(), MasterName: s.selfName(), Epoch: s.Epoch()}
	dbPath := filepath.Join(dir, "history.db")
	out, err := openScratch(dbPath)
	if err != nil {
		return "", meta, err
	}
	h := sha256.New()
	counts, maxChange, err := copyTables(s.d.DB, out, copyOpts{only: historyTables, changeMax: historyChangeMax}, h)
	closeDB(out)
	if err != nil {
		return "", meta, err
	}
	meta.Counts, meta.MaxChangeId, meta.Sum = counts, maxChange, hex.EncodeToString(h.Sum(nil))
	if st, err := os.Stat(dbPath); err == nil {
		meta.DBSize = st.Size()
	}
	zipPath := filepath.Join(dir, "history.zip")
	zf, err := os.OpenFile(zipPath, os.O_CREATE|os.O_TRUNC|os.O_WRONLY, 0o600)
	if err != nil {
		return "", meta, err
	}
	defer zf.Close()
	zw := zipWriter(zf)
	if err := zipAddFile(zw, "history.db", dbPath); err != nil {
		return "", meta, err
	}
	mb, _ := json.MarshalIndent(meta, "", "  ")
	if err := zipAddBytes(zw, "meta.json", mb); err != nil {
		return "", meta, err
	}
	if err := zw.Close(); err != nil {
		return "", meta, err
	}
	if err := zf.Sync(); err != nil {
		return "", meta, err
	}
	_ = os.Remove(dbPath)
	return zipPath, meta, nil
}

// histAck 主机这边给某台备用发历史数据的情况。
type histAck struct {
	epoch  uint64
	seq    uint64
	sum    string
	size   int64
	sentAt int64 // 最近一次送到(或核对过内容没变)
	asOf   int64 // 它手里那份到这时为止仍是最新的
	err    string
	errAt  int64
}

// HistoryView 面板上一台备用的历史数据情况。
type HistoryView struct {
	AsOf  int64  `json:"asOf,omitempty"`
	Size  int64  `json:"size,omitempty"`
	Err   string `json:"err,omitempty"`
	ErrAt int64  `json:"errAt,omitempty"`
}

// historyEveryLocked 这一轮多久给一份:库越大放得越宽(每 32 MB 多一小时),最多 6 小时;生成本身很慢的也放宽。
func (r *replicator) historyEveryLocked() time.Duration {
	every := historyEvery
	if r.histSize > 32<<20 {
		every = time.Duration((r.histSize+(32<<20)-1)/(32<<20)) * historyEvery
	}
	if c := 12 * r.histCost; c > every {
		every = c
	}
	if every > historyEveryMax {
		every = historyEveryMax
	}
	return every
}

// historyTick 副本循环每一轮:名单里已经收下这一任副本的备用,到点了就给一份历史数据(后台,同一时间只有一份在跑)。
func (r *replicator) historyTick(infos []StandbyInfo, ids []uint) {
	s := r.s
	epoch := s.Epoch()
	now := s.now()
	r.mu.Lock()
	defer r.mu.Unlock()
	if r.histRunning || len(ids) == 0 {
		return
	}
	if !r.histLast.IsZero() && now.Sub(r.histLast) < historyMinGap {
		return
	}
	due := r.historyDueLocked(infos, ids, epoch, now)
	if len(due) == 0 {
		return
	}
	r.histRunning = true
	safe.Go("ha.historyTick", func() {
		defer func() {
			r.mu.Lock()
			r.histRunning = false
			r.mu.Unlock()
		}()
		r.runHistory(due)
	})
}

// historyDueLocked ids 里这一轮该给历史数据的(调用方持有 r.mu):先收下了这一任的副本;发失败的隔 30 分钟再试;
// 上一份(主机这边记的,或者它报的手里那份 —— 主机重启后不记得发过)到了间隔。
func (r *replicator) historyDueLocked(infos []StandbyInfo, ids []uint, epoch uint64, now time.Time) []uint {
	every := int64(r.historyEveryLocked() / time.Second)
	var due []uint
	for _, id := range ids {
		if a := r.acks[id]; a == nil || a.epoch != epoch || a.seq == 0 {
			continue // 先收下这一任的副本(名单、纪元、公钥都对上了)再给历史数据
		}
		last := int64(0)
		if h := r.hist[id]; h != nil && h.epoch == epoch {
			if h.err != "" && now.Unix()-h.errAt < int64(historyRetry/time.Second) {
				continue
			}
			if h.err == "" {
				last = h.sentAt
			}
		}
		for _, in := range infos { // 主机重启过、不记得发过:看它报的手里那份
			if in.Id == id && in.History != nil && in.History.Epoch == epoch && in.History.At > last {
				last = in.History.At
			}
		}
		if last == 0 || now.Unix()-last >= every {
			due = append(due, id)
		}
	}
	return due
}

// standbyDiskFree 备用收副本、收历史数据时查的剩余空间(测试里换掉)。
var standbyDiskFree = func(s *Service) int64 { return s.diskFree() }

// HistoryNow 立刻给当前名单里的备用发一份历史数据(测试用;跳过节奏控制)。
func (s *Service) HistoryNow() error {
	if s.Epoch() == 0 || s.IsNode() {
		return errors.New("本机不是开着主机容灾的主机")
	}
	ids := sendable(s.rep.compute(s.Config()))
	if len(ids) == 0 {
		return errors.New("没有合格的备用")
	}
	return s.rep.runHistory(ids)
}

// runHistory 生成一份历史数据,加密一次,发给 ids 里的每台备用(内容没变、它手里就是上一份的不重发)。
func (r *replicator) runHistory(ids []uint) error {
	s := r.s
	start := s.now()
	epoch := s.Epoch()
	if err := ensureDir(s.Dir()); err != nil {
		return err
	}
	dir, err := os.MkdirTemp(s.Dir(), "history-")
	if err != nil {
		return err
	}
	defer os.RemoveAll(dir)
	zipPath, meta, err := s.buildHistory(dir)
	r.mu.Lock()
	r.histLast = start
	r.mu.Unlock()
	if err != nil {
		logger.Warning("容灾:生成历史数据失败: ", err)
		for _, id := range ids {
			r.noteHist(id, epoch, 0, meta, 0, err)
		}
		return err
	}
	zst, err := os.Stat(zipPath)
	if err != nil {
		return err
	}
	size := zst.Size()
	r.mu.Lock()
	r.histCost, r.histSize = s.now().Sub(start), size
	replNeed := TakeoverNeed(r.lastSize, r.lastDB)
	r.mu.Unlock()
	var send []uint
	for _, id := range ids {
		known := false // 它报过容灾信息(报了就按它报的核对手里那份是不是上一份)
		var held *hub.HAHistory
		var free, heldBytes int64 = -1, 0
		if s.d.Hub != nil {
			if st, ok := s.d.Hub.StatusOf(id); ok && st.HA != nil {
				known, held = true, st.HA.History
				free, heldBytes = st.HA.DiskFree, st.HA.HistoryBytes
			}
		}
		r.mu.Lock()
		h := r.hist[id]
		same := h != nil && h.epoch == epoch && h.err == "" && h.sum == meta.Sum &&
			(!known || (held != nil && held.Epoch == epoch && held.Seq == h.seq))
		if same { // 内容没变,它手里就是上一份:不重发,只记"到现在还是最新的"
			h.sentAt, h.asOf = start.Unix(), meta.CreatedAt
		}
		r.mu.Unlock()
		if same {
			continue
		}
		// 它的磁盘(随报告上来的剩余空间,手里那份历史数据换掉后算回来):接任时副本那一份照样要放得下
		if need := replNeed + size + historyNeed(size, meta.DBSize); free >= 0 && free+heldBytes < need {
			r.noteHist(id, epoch, 0, meta, size, fmt.Errorf("备用磁盘剩余 %d MB,收下历史数据后接任要约 %d MB:先不发", (free+heldBytes)>>20, need>>20))
			continue
		}
		send = append(send, id)
	}
	if len(send) == 0 {
		return nil
	}
	if s.Frozen() || s.IsNode() || s.Epoch() != epoch {
		return errors.New("纪元变了或正在换主机:这份历史数据不发了")
	}
	seq, err := s.nextSeq()
	if err != nil {
		return err
	}
	sign, err := s.signer()
	if err != nil {
		return err
	}
	dataKey := NewDataKey()
	ctPath := filepath.Join(dir, "history.ct")
	zf, err := os.Open(zipPath)
	if err != nil {
		return err
	}
	cf, err := os.OpenFile(ctPath, os.O_CREATE|os.O_TRUNC|os.O_WRONLY, 0o600)
	if err != nil {
		zf.Close()
		return err
	}
	sum, err := SealStream(cf, dataKey, zf, size)
	zf.Close()
	if cerr := cf.Close(); err == nil {
		err = cerr
	}
	if err != nil {
		return err
	}
	base := EnvHeader{Kind: KindHistory, Epoch: epoch, Seq: seq, CreatedAt: meta.CreatedAt, MasterId: meta.MasterId,
		MasterName: meta.MasterName, Version: meta.Version, Size: size, MaxChangeId: meta.MaxChangeId, DBSize: meta.DBSize}
	var wg sync.WaitGroup
	errs := make([]error, len(send))
	for i, id := range send {
		var n model.Node
		if s.d.DB.Where("id = ?", id).Limit(1).Find(&n).Error != nil || n.Id == 0 {
			continue
		}
		pub := s.setting(KeyPubPrefix + strconv.FormatUint(uint64(id), 10))
		k, err := ParsePub(pub)
		if err != nil {
			errs[i] = err
			r.noteHist(id, epoch, 0, meta, size, err)
			continue
		}
		hdr := base
		hdr.To = Fingerprint(pub)
		if hdr.Wrap, err = SealTo(dataKey, k); err != nil {
			errs[i] = err
			continue
		}
		gate := r.histGate(id)
		select {
		case gate <- struct{}{}:
		default:
			continue // 上一份还没送完
		}
		wg.Add(1)
		safe.Go3("ha.runHistory", i, n, hdr, func(i int, n model.Node, hdr EnvHeader) {
			defer wg.Done()
			defer func() { <-gate }()
			err := s.sendEnvelope(n, "ha/history", ctPath, hdr, sum, sign)
			errs[i] = err
			r.noteHist(n.Id, epoch, seq, meta, size, err)
			if err != nil {
				logger.Warning("容灾:给备用 ", n.Name, " 发历史数据失败: ", err)
			}
		})
	}
	wg.Wait()
	return errors.Join(errs...)
}

func (r *replicator) histGate(id uint) chan struct{} {
	r.mu.Lock()
	defer r.mu.Unlock()
	g := r.histGates[id]
	if g == nil {
		g = make(chan struct{}, 1)
		r.histGates[id] = g
	}
	return g
}

func (r *replicator) noteHist(id uint, epoch, seq uint64, meta HistoryMeta, size int64, err error) {
	r.mu.Lock()
	defer r.mu.Unlock()
	h := r.hist[id]
	if h == nil || h.epoch != epoch {
		h = &histAck{epoch: epoch}
		r.hist[id] = h
	}
	if err != nil {
		h.err, h.errAt = err.Error(), r.s.now().Unix()
		return
	}
	*h = histAck{epoch: epoch, seq: seq, sum: meta.Sum, size: size, sentAt: r.s.now().Unix(), asOf: meta.CreatedAt}
}

// historyView 面板上 id 这台备用的历史数据情况(调用方持有 r.mu)。
func (r *replicator) historyViewLocked(id uint) *HistoryView {
	h := r.hist[id]
	if h == nil || h.epoch != r.s.Epoch() {
		return nil
	}
	return &HistoryView{AsOf: h.asOf, Size: h.size, Err: h.err, ErrAt: h.errAt}
}

// ---- 备用:收下与保存 ----

// historyInfo 本机历史数据的信息(没有为 ok=false)。
func (s *Service) historyInfo() (HistoryInfo, bool) {
	var hi HistoryInfo
	b, err := os.ReadFile(filepath.Join(s.Dir(), fileHistInfo))
	if err != nil || json.Unmarshal(b, &hi) != nil {
		return hi, false
	}
	if _, err := os.Stat(filepath.Join(s.Dir(), fileHist)); err != nil {
		return hi, false
	}
	return hi, true
}

// historyBytes 本机历史数据文件占的磁盘。
func (s *Service) historyBytes() int64 {
	if st, err := os.Stat(filepath.Join(s.Dir(), fileHist)); err == nil {
		return st.Size()
	}
	return 0
}

// dropHistoryLocked 删掉本机的历史数据(调用方持有 storeMu)。
func (s *Service) dropHistoryLocked() {
	for _, n := range []string{fileHist, fileHistTmp, fileHistInfo} {
		_ = os.Remove(filepath.Join(s.Dir(), n))
	}
}

// ReceiveHistory 收下主机推来的一份历史数据(agent/ha/history):和副本一样边收边写临时文件、边算哈希,验签后原子换入。
// 要先有这一任主机的副本(没有副本也接任不了,历史数据单独留着没用)。返回收下的序号。
func (s *Service) ReceiveHistory(body io.Reader) (uint64, error) {
	s.storeMu.Lock()
	defer s.storeMu.Unlock()
	L := s.Epoch()
	mid, _ := s.masterId()
	pubB64 := s.setting(KeyMasterPub)
	if L == 0 || mid == 0 || pubB64 == "" {
		return 0, errors.New("本机还没收到主机的容灾状态,暂不收历史数据")
	}
	st, ok := s.NodeState()
	if !ok || st.rank() == 0 || st.Mode == ModeOff {
		return 0, errors.New("本机不在备用名单里,不收历史数据")
	}
	ri, ok := s.replicaInfo()
	if !ok || ri.Epoch != L || ri.MasterId != mid {
		return 0, errors.New("本机还没有这一任主机的数据副本:先收副本,再收历史数据")
	}
	pub, err := ParseSignPub(pubB64)
	if err != nil {
		return 0, err
	}
	key, err := s.standbyKey()
	if err != nil {
		return 0, err
	}
	if free := standbyDiskFree(s); free >= 0 && free < 64<<20 {
		return 0, fmt.Errorf("磁盘剩余空间不足(只剩 %d MB),不收历史数据", free>>20)
	}
	tmp := filepath.Join(s.Dir(), fileHistTmp)
	f, err := os.OpenFile(tmp, os.O_CREATE|os.O_TRUNC|os.O_WRONLY, 0o600)
	if err != nil {
		return 0, err
	}
	env, err := ReceiveEnvelope(body, f, MaxHistory)
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
	case hd.Kind != KindHistory:
		return fail(errors.New("这不是历史数据"))
	case hd.Epoch != L:
		return fail(fmt.Errorf("历史数据来自纪元 %d,本机在纪元 %d", hd.Epoch, L))
	case hd.MasterId != mid:
		return fail(fmt.Errorf("历史数据来自服务器 #%d,不是本机认的主机 #%d", hd.MasterId, mid))
	}
	if err := env.VerifySig(pub); err != nil {
		return fail(err)
	}
	if hd.To != Fingerprint(key.PubString()) {
		return fail(errors.New("历史数据不是封给本机的(主机记住的本机公钥和现在的不一样)"))
	}
	old, hadOld := s.historyInfo()
	if hadOld && old.Epoch == hd.Epoch && old.MasterId == hd.MasterId && hd.Seq <= old.Seq {
		return fail(fmt.Errorf("历史数据序号 %d 不比本机已有的 %d 新", hd.Seq, old.Seq))
	}
	// 收下后接任时要放得下:副本那一份(解开、组新库)和这一份(解开、并进新库)。换下去的上一份算回来
	if free := standbyDiskFree(s); free >= 0 {
		if need := TakeoverNeed(ri.Size, ri.DBSize) + historyNeed(hd.Size, hd.DBSize); free+s.historyBytes() < need {
			return fail(fmt.Errorf("磁盘剩余空间不够:收下历史数据后接任要约 %d MB,只剩 %d MB,不收", need>>20, (free+s.historyBytes())>>20))
		}
	}
	cur := filepath.Join(s.Dir(), fileHist)
	if err := os.Rename(tmp, cur); err != nil {
		return fail(err)
	}
	syncDir(s.Dir())
	hi := HistoryInfo{Epoch: hd.Epoch, Seq: hd.Seq, CreatedAt: hd.CreatedAt, Size: hd.Size, DBSize: hd.DBSize, MasterId: hd.MasterId,
		MasterName: hd.MasterName, ReceivedAt: s.now().Unix(), MasterPub: pubB64}
	b, _ := json.MarshalIndent(hi, "", "  ")
	if err := writeFileAtomic(filepath.Join(s.Dir(), fileHistInfo), b); err != nil {
		return 0, err
	}
	return hd.Seq, nil
}

// ---- 接任:并进新库 ----

// openedHistory 接任时解开的历史数据。
type openedHistory struct {
	Path string // 解出的 history.db
	Meta HistoryMeta
}

// openHistory 接任时解开本机的历史数据(dir 下 history/):要和接任所用的副本出自同一任主机(同一台、同一个纪元),
// 验签用收下时记的主机公钥。用不了的返回告警(不挡接任)。
func (s *Service) openHistory(dir string, rep EnvHeader, replicaDB int64) (*openedHistory, *PlanItem) {
	s.storeMu.Lock()
	defer s.storeMu.Unlock()
	warn := func(code string, kv ...string) (*openedHistory, *PlanItem) {
		it := item(code, kv...)
		return nil, &it
	}
	hi, ok := s.historyInfo()
	if !ok {
		return warn(idwHistNone)
	}
	at := time.Unix(hi.CreatedAt, 0).In(s.loc()).Format("2006-01-02 15:04")
	if hi.MasterId != rep.MasterId || hi.Epoch != rep.Epoch {
		return warn(idwHistOther, "t", at)
	}
	if free := takeoverDiskFree(s); free >= 0 {
		// 副本那份已经解开了,还要组新库(一份库);历史数据解开、并进新库
		if need := replicaDB + historyNeed(hi.Size, hi.DBSize) + 64<<20; free < need {
			return warn(idwHistDisk, "need", strconv.FormatInt(need>>20, 10), "free", strconv.FormatInt(free>>20, 10))
		}
	}
	failed := func(err error) (*openedHistory, *PlanItem) {
		logger.Warning("容灾:本机的历史数据打不开,接任时不带它: ", err)
		return warn(idwHistFail, "err", errText(err))
	}
	p := filepath.Join(s.Dir(), fileHist)
	env, err := OpenEnvelopeFile(p)
	if err != nil {
		return failed(err)
	}
	if env.Header.Kind != KindHistory || env.Header.Epoch != hi.Epoch || env.Header.MasterId != hi.MasterId || env.Header.Seq != hi.Seq {
		return failed(errors.New("历史数据和它的记录对不上"))
	}
	pub, err := ParseSignPub(hi.MasterPub)
	if err != nil {
		return failed(errors.New("不知道签发历史数据的主机公钥,不能验签"))
	}
	if err := env.VerifySig(pub); err != nil {
		return failed(err)
	}
	key, err := s.standbyKey()
	if err != nil {
		return failed(err)
	}
	dataKey, err := OpenWith(env.Header.Wrap, key)
	if err != nil {
		return failed(err)
	}
	hdir := filepath.Join(dir, "history")
	if err := os.MkdirAll(hdir, 0o700); err != nil {
		return failed(err)
	}
	zp := filepath.Join(hdir, "history.zip")
	zf, err := os.OpenFile(zp, os.O_CREATE|os.O_TRUNC|os.O_WRONLY, 0o600)
	if err != nil {
		return failed(err)
	}
	err = env.DecryptFile(p, dataKey, zf)
	if cerr := zf.Close(); err == nil {
		err = cerr
	}
	if err != nil {
		return failed(err)
	}
	meta, err := unzipHistory(zp, hdir)
	_ = os.Remove(zp)
	if err != nil {
		return failed(err)
	}
	if meta.MasterId != hi.MasterId || meta.Epoch != hi.Epoch {
		return failed(errors.New("历史数据里记的主机、纪元和信封上的对不上"))
	}
	return &openedHistory{Path: filepath.Join(hdir, "history.db"), Meta: meta}, nil
}

// unzipHistory 解出 history.db 与 meta.json(别的一概不要)。
func unzipHistory(zipPath, dir string) (HistoryMeta, error) {
	var meta HistoryMeta
	zr, err := zip.OpenReader(zipPath)
	if err != nil {
		return meta, fmt.Errorf("历史数据不是 zip: %w", err)
	}
	defer zr.Close()
	got := 0
	for _, f := range zr.File {
		name := filepath.ToSlash(f.Name)
		if name != "history.db" && name != "meta.json" {
			continue
		}
		rc, err := f.Open()
		if err != nil {
			return meta, err
		}
		out, err := os.OpenFile(filepath.Join(dir, name), os.O_CREATE|os.O_TRUNC|os.O_WRONLY, 0o600)
		if err != nil {
			rc.Close()
			return meta, err
		}
		_, err = io.Copy(out, io.LimitReader(rc, 64<<30))
		rc.Close()
		if cerr := out.Close(); err == nil {
			err = cerr
		}
		if err != nil {
			return meta, err
		}
		got++
	}
	if got != 2 {
		return meta, errors.New("历史数据不完整")
	}
	b, err := os.ReadFile(filepath.Join(dir, "meta.json"))
	if err != nil {
		return meta, err
	}
	if err := json.Unmarshal(b, &meta); err != nil {
		return meta, fmt.Errorf("历史数据的说明读不出来: %w", err)
	}
	return meta, nil
}

// importHistory 接任组库时把历史数据并进新库(ndb):
//   - 流量时序按桶累加(组库时并进去的本机还没上报的流量不丢)。用户的只并还在的、而且不早于他建号的(留一小时余量:
//     48 小时以前的分钟样本并成了整点的桶)—— 副本之后删掉的用户不留下孤儿行,删了又建的同名用户不会接上前一个人的历史;
//   - 订阅日志、审计日志按 id 去重;审计日志只要副本截止那一条及以前的(之后的改动副本里没有,记进来就对不上了)。
//
// 两边的表结构以新库为准:只并两边都有的列(历史数据出自主机,版本不比本机新)。
func importHistory(ndb *gorm.DB, path string, maxChange uint64) (HistoryNote, error) {
	var note HistoryNote
	sqlDB, err := ndb.DB()
	if err != nil {
		return note, err
	}
	ctx := context.Background()
	conn, err := sqlDB.Conn(ctx) // ATTACH 只对这一条连接有效:整个过程用同一条
	if err != nil {
		return note, err
	}
	defer conn.Close()
	if _, err := conn.ExecContext(ctx, "ATTACH DATABASE ? AS h", path); err != nil {
		return note, fmt.Errorf("打开历史数据: %w", err)
	}
	defer func() { _, _ = conn.ExecContext(ctx, "DETACH DATABASE h") }()
	colsOf := func(schema, table string) (map[string]bool, []string, error) {
		rows, err := conn.QueryContext(ctx, "SELECT name FROM pragma_table_info(?, ?)", table, schema)
		if err != nil {
			return nil, nil, err
		}
		defer rows.Close()
		set := map[string]bool{}
		var list []string
		for rows.Next() {
			var n string
			if err := rows.Scan(&n); err != nil {
				return nil, nil, err
			}
			set[n] = true
			list = append(list, n)
		}
		return set, list, rows.Err()
	}
	common := func(table string, withId bool) (string, string, error) {
		hset, _, err := colsOf("h", table)
		if err != nil {
			return "", "", err
		}
		_, mlist, err := colsOf("main", table)
		if err != nil {
			return "", "", err
		}
		var cols, sel []string
		for _, c := range mlist {
			if !hset[c] || (c == "id" && !withId) {
				continue
			}
			cols = append(cols, quoteIdent(c))
			sel = append(sel, "x."+quoteIdent(c))
		}
		if len(cols) == 0 {
			return "", "", fmt.Errorf("历史数据里的 %s 和本机的表对不上", table)
		}
		return strings.Join(cols, ","), strings.Join(sel, ","), nil
	}
	tx, err := conn.BeginTx(ctx, nil)
	if err != nil {
		return note, err
	}
	defer tx.Rollback()
	run := func(q string, args ...interface{}) (int64, error) {
		res, err := tx.ExecContext(ctx, q, args...)
		if err != nil {
			return 0, err
		}
		return res.RowsAffected()
	}
	if cols, sel, err := common("stats", false); err != nil {
		return note, err
	} else if note.Stats, err = run(`INSERT INTO main.stats (` + cols + `) SELECT ` + sel + ` FROM h.stats x
		WHERE x.resource <> 'user' OR EXISTS (SELECT 1 FROM main.users u WHERE u.name = x.tag AND x.date_time >= u.created_at - 3600)
		ON CONFLICT(resource, tag, date_time, direction) DO UPDATE SET traffic = traffic + excluded.traffic`); err != nil {
		return note, fmt.Errorf("并流量时序: %w", err)
	}
	if cols, sel, err := common("sub_logs", true); err != nil {
		return note, err
	} else if note.SubLogs, err = run(`INSERT OR IGNORE INTO main.sub_logs (` + cols + `) SELECT ` + sel + ` FROM h.sub_logs x`); err != nil {
		return note, fmt.Errorf("并订阅日志: %w", err)
	}
	if cols, sel, err := common("changes", true); err != nil {
		return note, err
	} else if note.Changes, err = run(`INSERT OR IGNORE INTO main.changes (`+cols+`) SELECT `+sel+` FROM h.changes x WHERE x.id <= ?`, maxChange); err != nil {
		return note, fmt.Errorf("并审计日志: %w", err)
	}
	return note, tx.Commit()
}
