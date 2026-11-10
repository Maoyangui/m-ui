package ha

import (
	"context"
	"encoding/json"
	"os"
	"path/filepath"
	"runtime/debug"
	"strconv"
	"strings"
	"sync"
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
	// Notify 用本机的通知设置发一条(主机、临时主机、已降级的原主机都用它);副机一般没配通知,候选另有通知块
	Notify func(toggle, text string)
	// Restart 过一会儿重启本进程(接管、降级、迁回都要重启:代理面板、Telegram 机器人只在启动时按角色拉起)
	Restart func(delay time.Duration)
	// Backup 在备份目录生成一份带前缀的备份,返回文件名
	Backup func(prefix string) (string, error)
	// PanelURL / ResellerURL 本机面板、代理面板的对外地址(猜的,发通知与显示用)
	PanelURL    func() string
	ResellerURL func() string
	// CertPaths 本机设置与证书库引用的证书、私钥文件
	CertPaths func() []string
	// Witness 外部见证(Globalping):从境外测点连主机的公网 IP;nil = 不做见证
	Witness func(ctx context.Context, host string, ports []int) (WitnessResult, error)
	// TelegramAPI 测试里替换 Telegram 接口地址
	TelegramAPI string
	Now         func() time.Time
}

// Service 一台机器上的容灾服务:主机侧推状态、推副本、判断要不要降级;副机侧按纪元规则接受请求、收副本、
// 判断主机失联、接管;临时主机侧迁回。
type Service struct {
	d Deps

	mu          sync.Mutex
	lastContact time.Time         // 副机:最近一次收到当前纪元主机的请求(进程启动时记成现在,重启不会马上触发接管)
	replies     map[uint]reply    // 主机:各副机最近一次回复里的纪元判定
	detached    map[uint]bool     // 主机:已经告过"已脱离"的副机
	fenced      bool              // 主机:已经决定降级(只做一次)
	stateRev    map[uint]string   // 主机:各副机的状态修订(只做显示)
	sealCache   map[string]string // 主机:通知块按(候选公钥 + 明文哈希)缓存的密文:每次重新密封会让状态修订号每轮都变
	base        *State            // 主机:这一轮发给所有副机的基础状态(见 baseCached)
	baseAt      time.Time
	storeMu     sync.Mutex // 候选:副本只有一个写入方
	handoverMu  sync.Mutex // 临时主机:同一时间只做一次迁回导出 / 提交
	promoteMu   sync.Mutex
	rep         *replicator
	eng         *engine

	stop chan struct{}
	wg   sync.WaitGroup
}

// New 建容灾服务。
func New(d Deps) *Service {
	if d.Now == nil {
		d.Now = time.Now
	}
	s := &Service{d: d, replies: map[uint]reply{}, detached: map[uint]bool{}, stateRev: map[uint]string{}, sealCache: map[string]string{},
		stop: make(chan struct{})}
	s.lastContact = d.Now()
	s.rep = newReplicator(s)
	s.eng = newEngine(s)
	return s
}

func (s *Service) now() time.Time { return s.d.Now() }

// Start 拉起后台循环(副本、失联判定、待提交的迁回)。
func (s *Service) Start() {
	for _, l := range []struct {
		name  string
		every time.Duration
		fn    func()
	}{
		{"容灾副本", 10 * time.Second, s.rep.tick},
		{"容灾判定", 10 * time.Second, s.eng.tick},
		{"迁回提交", 15 * time.Second, s.commitTick},
		{"副本自检", time.Hour, s.selfCheckTick},
	} {
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
					func() {
						defer func() {
							if v := recover(); v != nil {
								logger.Warning("后台任务 ", name, " 异常: ", v, " | ", string(debug.Stack()))
							}
						}()
						fn()
					}()
				}
			}
		}(l.name, l.every, l.fn)
	}
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

// Epoch 本机纪元(0 = 没开过容灾)。
func (s *Service) Epoch() uint64 {
	e, _ := strconv.ParseUint(strings.TrimSpace(s.setting(KeyEpoch)), 10, 64)
	return e
}

// IsNode 本机是不是副机(判定一律看 nodeMode)。
func (s *Service) IsNode() bool { return strings.EqualFold(s.setting("nodeMode"), "true") }

// Role 显示用的角色:master / temp / node / fenced / promoting。
func (s *Service) Role() string {
	r := s.setting(KeyRole)
	if s.IsNode() {
		if r == RoleFenced || r == RolePromoting {
			return r
		}
		return "node"
	}
	if r == RoleTemp {
		return RoleTemp
	}
	return "master"
}

// Mode 容灾模式(没开 = off)。
func (s *Service) Mode() string {
	switch m := strings.TrimSpace(s.setting(KeyMode)); m {
	case ModeManual, ModeConfirm, ModeAuto:
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

// Dir 容灾文件目录 <数据目录>/ha(0700):候选私钥、副本、原主机证书、迁回暂存。不进库,也不进本机备份。
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
