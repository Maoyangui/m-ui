package ha

import (
	"context"
	"crypto/rand"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"encoding/pem"
	"os"
	"path/filepath"
	"runtime/debug"
	"strconv"
	"strings"
	"sync"
	"sync/atomic"
	"time"

	"github.com/Maoyangui/m-ui/database/model"
	"github.com/Maoyangui/m-ui/hub"
	"github.com/Maoyangui/m-ui/logger"
	"github.com/Maoyangui/m-ui/render"

	"gorm.io/gorm"
	"gorm.io/gorm/clause"
)

// Deps runner 交给容灾服务的依赖(ha 不引用 runner / web,避免成环)。
type Deps struct {
	DB      *gorm.DB
	DBPath  string
	Version string
	Hub     *hub.Hub
	// Notify 用本机的通知设置发一条(主机用;副机一般没配通知,备用另有加密的通知块)
	Notify func(toggle, text string)
	// Restart 过一会儿重启本进程(接任、设为主机、变为副机都要重启:代理面板、Telegram 机器人只在启动时按角色拉起)
	Restart func(delay time.Duration)
	// Backup 在备份目录生成一份带前缀的备份,返回文件名
	Backup func(prefix string) (string, error)
	// PanelURL / ResellerURL 本机面板、代理面板的对外地址(按设置拼的,发通知与显示用)
	PanelURL    func() string
	ResellerURL func() string
	// Witness 境外测点见证(Globalping):从境外连主机的公网 IP;nil = 不做见证
	Witness func(ctx context.Context, host string, ports []int) (WitnessResult, error)
	// DNSAPI 测试里替换 Cloudflare 接口地址
	DNSAPI string
	Now    func() time.Time
}

// Service 一台机器上的容灾服务。主机侧:推容灾状态、推副本、判断要不要变为副机、设为主机;
// 副机侧:按纪元规则接受请求、备用收副本、判断主机失联、接任;被设为主机的副机:收数据、接手。
type Service struct {
	d Deps

	mu          sync.Mutex
	lastContact time.Time         // 副机:最近一次收到当前纪元主机的请求(进程启动时记成现在,重启不会马上触发接任)
	replies     map[uint]reply    // 主机:各副机最近一次回复里的纪元判定
	detached    map[uint]bool     // 主机:已经告过"已脱离"的副机
	demoting    bool              // 主机:已经决定变为副机(只做一次)
	yieldSince  time.Time         // 主机:多数副机从什么时候起一直还跟着另一台主机(见 evaluateYield)
	reclaimAt   time.Time         // 主机:上次收回脱离的副机
	grantOut    *Grant            // 主机:收回副机时随容灾状态推出去的交接标记
	sealCache   map[string]string // 主机:通知块按(备用公钥 + 明文哈希)缓存的密文:每次重新密封会让状态修订号每轮都变
	base        *State            // 主机:这一轮发给所有副机的基础状态(见 baseCached)
	baseAt      time.Time
	storeMu     sync.Mutex // 备用:副本只有一个写入方
	promoteMu   sync.Mutex // 接任 / 设为主机:同一时间只做一件
	rep         *replicator
	eng         *engine
	sw          *switcher
	inc         *incoming

	clock atomic.Value // 测试换掉的钟(func() time.Time);没换用 d.Now

	stop chan struct{}
	wg   sync.WaitGroup
}

// New 建容灾服务。
func New(d Deps) *Service {
	if d.Now == nil {
		d.Now = time.Now
	}
	s := &Service{d: d, replies: map[uint]reply{}, detached: map[uint]bool{}, sealCache: map[string]string{},
		stop: make(chan struct{})}
	s.lastContact = d.Now()
	s.rep = newReplicator(s)
	s.eng = newEngine(s)
	s.sw = &switcher{s: s}
	s.inc = &incoming{s: s}
	return s
}

func (s *Service) now() time.Time {
	if f, ok := s.clock.Load().(func() time.Time); ok {
		return f()
	}
	return s.d.Now()
}

// loops 后台循环:副本、失联判定、原主机回来、命令行请求、副本自检。
func (s *Service) loops() []struct {
	name  string
	every time.Duration
	fn    func()
} {
	return []struct {
		name  string
		every time.Duration
		fn    func()
	}{
		{"容灾副本", 10 * time.Second, s.rep.tick},
		{"容灾判定", 10 * time.Second, s.eng.tick},
		{"原主机回来", 30 * time.Second, s.formerTick},
		{"容灾杂项", 5 * time.Second, s.miscTick},
		{"副本自检", time.Hour, s.selfCheckTick},
	}
}

// Start 拉起后台循环。
func (s *Service) Start() {
	s.startupTasks()
	for _, l := range s.loops() {
		s.wg.Add(1)
		go func(name string, every time.Duration, fn func()) {
			defer s.wg.Done()
			t := time.NewTicker(every)
			defer t.Stop()
			for {
				select {
				case <-s.stop:
					return
				case <-t.C:
					s.safe(name, fn)
				}
			}
		}(l.name, l.every, l.fn)
	}
}

func (s *Service) safe(name string, fn func()) {
	defer func() {
		if v := recover(); v != nil {
			logger.Warning("后台任务 ", name, " 异常: ", v, " | ", string(debug.Stack()))
		}
	}()
	fn()
}

// Stop 停后台循环。
func (s *Service) Stop() {
	select {
	case <-s.stop:
	default:
		close(s.stop)
	}
	s.wg.Wait()
}

func (s *Service) stopping() bool {
	select {
	case <-s.stop:
		return true
	default:
		return false
	}
}

// miscTick 每 5 秒:命令行请求、设为主机的超时解冻、暂存数据过期。
func (s *Service) miscTick() {
	s.cmdTick()
	s.sw.tick()
	s.inc.tick()
}

// ---- 设置读写 ----

func (s *Service) setting(key string) string {
	var v string
	s.d.DB.Raw("SELECT value FROM settings WHERE key = ?", key).Scan(&v)
	return v
}

func (s *Service) settingInt(key string, def, lo, hi int) int {
	n, err := strconv.Atoi(strings.TrimSpace(s.setting(key)))
	if err != nil {
		return def
	}
	return min(max(n, lo), hi)
}

func (s *Service) settingBool(key string, def bool) bool {
	switch strings.ToLower(strings.TrimSpace(s.setting(key))) {
	case "true":
		return true
	case "false":
		return false
	}
	return def
}

func upsert(tx *gorm.DB, values map[string]string) error {
	for k, v := range values {
		if err := tx.Clauses(clause.OnConflict{
			Columns:   []clause.Column{{Name: "key"}},
			DoUpdates: clause.AssignmentColumns([]string{"value"}),
		}).Create(&model.Setting{Key: k, Value: v}).Error; err != nil {
			return err
		}
	}
	return nil
}

func (s *Service) set(values map[string]string) error {
	return s.d.DB.Transaction(func(tx *gorm.DB) error { return upsert(tx, values) })
}

func (s *Service) getJSON(key string, out interface{}) bool {
	raw := strings.TrimSpace(s.setting(key))
	return raw != "" && json.Unmarshal([]byte(raw), out) == nil
}

func mustJSON(v interface{}) string {
	b, _ := json.Marshal(v)
	return string(b)
}

// ---- 角色 ----

// Epoch 本机纪元(0 = 没开过容灾、没换过主机)。
func (s *Service) Epoch() uint64 {
	e, _ := strconv.ParseUint(strings.TrimSpace(s.setting(KeyEpoch)), 10, 64)
	return e
}

// IsNode 本机是不是副机(判定一律看 nodeMode)。
func (s *Service) IsNode() bool { return strings.EqualFold(s.setting("nodeMode"), "true") }

// Role 显示用的角色:master / node / demoted(原主机回来后变成的副机,等新主机来接)/ promoting(正在接任)。
func (s *Service) Role() string {
	r := s.setting(KeyRole)
	if s.IsNode() {
		if r == RoleDemoted || r == RolePromoting {
			return r
		}
		return "node"
	}
	return "master"
}

// Mode 主机容灾的模式(没开 = off)。
func (s *Service) Mode() string {
	switch m := strings.TrimSpace(s.setting(KeyMode)); m {
	case ModeConfirm, ModeAuto:
		return m
	}
	return ModeOff
}

// SelfId 本机在服务器表里的 id(副机还没收到过配置时为 0)。
func (s *Service) SelfId() uint { return render.LocalNodeID(s.d.DB) }

// selfName 本机在服务器表里的名字。
func (s *Service) selfName() string {
	var n model.Node
	if s.d.DB.Where("is_local = ?", true).Limit(1).Find(&n).Error == nil && n.Name != "" {
		return n.Name
	}
	h, _ := os.Hostname()
	return h
}

func (s *Service) nodeName(id uint) string {
	var n model.Node
	if id != 0 && s.d.DB.Select("id, name").Where("id = ?", id).Limit(1).Find(&n).Error == nil && n.Name != "" {
		return n.Name
	}
	if id == 0 {
		return "?"
	}
	return "#" + strconv.FormatUint(uint64(id), 10)
}

// DataDir 数据目录。
func (s *Service) DataDir() string {
	abs, err := filepath.Abs(s.d.DBPath)
	if err != nil {
		return filepath.Dir(s.d.DBPath)
	}
	return filepath.Dir(abs)
}

// Dir 容灾文件目录 <数据目录>/ha(0700):备用私钥、副本、设为主机时的暂存。不进库,也不进本机备份。
func (s *Service) Dir() string { return HADir(s.DataDir()) }

// HADir 数据目录下的容灾文件目录。
func HADir(dataDir string) string { return filepath.Join(dataDir, "ha") }

func ensureDir(p string) error {
	if err := os.MkdirAll(p, 0o700); err != nil {
		return err
	}
	return os.Chmod(p, 0o700)
}

// Header 主机发往副机的请求头(给 hub 用):本机纪元为 0 或本机是副机时不加。
func (s *Service) Header() (uint64, uint) {
	e := s.Epoch()
	if e == 0 || s.IsNode() {
		return 0, 0
	}
	return e, s.SelfId()
}

// notify 用本机通知发一条。
func (s *Service) notify(text string) {
	if s.d.Notify != nil {
		s.d.Notify("tgOnCore", text)
	}
}

// restart 安排重启。
func (s *Service) restart(delay time.Duration) {
	if s.d.Restart != nil {
		s.d.Restart(delay)
	}
}

// SetHub 接上主副机同步器(runner 先建容灾服务、再建 hub,两边互相引用)。
func (s *Service) SetHub(h *hub.Hub) { s.d.Hub = h }

func (s *Service) panelURL() string {
	if s.d.PanelURL != nil {
		return s.d.PanelURL()
	}
	return ""
}

func (s *Service) resellerURL() string {
	if s.d.ResellerURL != nil {
		return s.d.ResellerURL()
	}
	return ""
}

// nodeToken 本机的副机令牌:没有就生成一个(只在空时写,不覆盖面板那边已经生成的)。
// 主机也要有:变为副机后新主机凭它来管本机(主机身上它什么也打不开:agent 接口在主机上一律 403 / 409)。
func (s *Service) nodeToken() string {
	if tok := strings.TrimSpace(s.setting("nodeToken")); tok != "" {
		return tok
	}
	b := make([]byte, 24)
	if _, err := rand.Read(b); err != nil {
		return ""
	}
	s.d.DB.Clauses(clause.OnConflict{Columns: []clause.Column{{Name: "key"}}, DoNothing: true}).Create(&model.Setting{Key: "nodeToken", Value: hex.EncodeToString(b)})
	s.d.DB.Model(&model.Setting{}).Where("key = ? AND value = ''", "nodeToken").Update("value", hex.EncodeToString(b))
	return strings.TrimSpace(s.setting("nodeToken"))
}

// panelCertFP 本机面板证书的指纹(和 hub 钉的一样:叶子证书 DER 的 sha256);面板走 http 或读不到证书时为空。
func (s *Service) panelCertFP() string { return certFileFP(s.setting("webCertFile")) }

func certFileFP(path string) string {
	path = strings.TrimSpace(path)
	if path == "" {
		return ""
	}
	b, err := os.ReadFile(path)
	if err != nil {
		return ""
	}
	for {
		var blk *pem.Block
		blk, b = pem.Decode(b)
		if blk == nil {
			return ""
		}
		if blk.Type == "CERTIFICATE" {
			sum := sha256.Sum256(blk.Bytes)
			return hex.EncodeToString(sum[:])
		}
	}
}

// Link 连到一台机器的面板接口要的东西:API 地址、副机令牌、证书指纹(和服务器页"加副机"填的一样)。
type Link struct {
	ApiUrl string `json:"apiUrl"`
	Token  string `json:"token,omitempty"`
	CertFP string `json:"certFp,omitempty"`
}

// selfLink 本机作为副机时别人该怎么连:接任 / 设为主机后,新主机凭它把本机当副机管起来。
func (s *Service) selfLink() Link {
	l := Link{ApiUrl: s.panelURL(), Token: s.nodeToken(), CertFP: s.panelCertFP()}
	if strings.Contains(l.ApiUrl, "<") { // 没有域名也没探测到公网 IP:拼不出能用的地址
		l.ApiUrl = ""
	}
	return l
}

// startupTasks 进程起来后要做一次的事:刚接任 / 刚成为主机的发通知、改 DNS;刚变为副机的清掉手里的旧副本。
func (s *Service) startupTasks() {
	go s.safe("容灾启动", func() {
		s.afterBecame()
	})
}

// SetNow 换掉钟(测试里拨快时间:租约过期、暂停到期、冻结超时)。
func (s *Service) SetNow(now func() time.Time) { s.clock.Store(now) }

// Tick 同步跑一轮刚成为主机的收尾、失联判定、原主机回来、命令行请求、超时收尾(测试用;副本另有 ReplicateNow)。
func (s *Service) Tick() {
	s.afterBecame()
	s.miscTick()
	s.formerTick()
	s.eng.tick()
}

// Switch 设为主机的进度与结果。
func (s *Service) Switch() SwitchState { return s.switchState() }

// auditTo 记一条操作审计(面板「操作审计」里看得到,key 为 ha):接任、成为主机、变为副机、原主机回来、命令行的操作……
func auditTo(db *gorm.DB, at int64, actor, action string, obj interface{}) {
	b, _ := json.Marshal(obj)
	db.Create(&model.Change{DateTime: at, Actor: actor, Key: "ha", Action: action, Obj: b})
}

func (s *Service) audit(actor, action string, obj interface{}) {
	auditTo(s.d.DB, s.now().Unix(), actor, action, obj)
}
