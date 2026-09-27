// Package reach 大陆连通性检测:借 Globalping 公共测试网络,从大陆移动、电信、联通的家庭宽带测点
// 去连每台服务器,同时用香港测点作对照,判断服务器的 IP 或端口是不是被墙了。
//
// 只在主机上运行:测点在大陆,请求由哪台机器发起都一样,各台副机的地址主机都知道。
// 服务器地址会发给 Globalping(设置页写明了),机房测点一律不算(用户要求)。
package reach

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"net/http"
	"runtime/debug"
	"sort"
	"strconv"
	"strings"
	"sync"
	"time"

	"github.com/Maoyangui/m-ui/database/model"
	"github.com/Maoyangui/m-ui/logger"
	"github.com/Maoyangui/m-ui/notify"

	"gorm.io/gorm"
)

const (
	DefaultMinutes = 90   // 默认每 90 分钟巡检一轮(用户定的)
	MinMinutes     = 30   // 免登录每小时约 250 个测点次,再密就不够几台服务器用了
	MaxMinutes     = 1440 // 一天一次
	perCarrier     = 4    // 每家运营商取几个测点
	keepHistory    = 24   // 界面上每台显示最近几次
	keepRows       = 48   // 库里每台留几条
	probeListTTL   = 6 * time.Hour
	parallelChecks = 3
)

// ErrBusy 已经有一轮在跑。
var ErrBusy = errors.New("正在检测,请等这一轮结束")

// Target 一台要测的服务器。
type Target struct {
	NodeId   uint
	Name     string
	Host     string // IPv4
	Port     int    // 0 = 没有可连的 TCP 端口,只 ping
	PortFrom string // line / sub / api
	PortLine string // PortFrom = line 时的线路名
	Err      string // 测不了的原因(没有地址等)
}

type Deps struct {
	DB      *gorm.DB
	Setting func(string) string
	IsNode  func() bool
	Targets func(ctx context.Context) []Target
	Notify  func(toggle, text string)
	UA      string

	// 以下只给测试替换
	API          string
	HTTP         *http.Client
	Now          func() time.Time
	Poll         time.Duration
	RecheckDelay time.Duration
	FirstDelay   time.Duration
}

// Point 历史上的一次结果(界面上一格)。
type Point struct {
	At      int64  `json:"at"`
	Verdict string `json:"verdict"`
	Auto    bool   `json:"auto"`
}

// View 给面板的整体状态。
type View struct {
	Auto       bool             `json:"auto"`
	Minutes    int              `json:"minutes"`
	LastRun    int64            `json:"lastRun"`
	NextRun    int64            `json:"nextRun"`
	Running    bool             `json:"running"`
	RunningIds []uint           `json:"runningIds"`
	Quota      *Quota           `json:"quota,omitempty"`
	TokenSet   bool             `json:"tokenSet"`
	PerCheck   int              `json:"perCheck"` // 每台服务器测一次大约用几个测点次
	Results    []Result         `json:"results"`
	History    map[uint][]Point `json:"history"`
	Error      string           `json:"error,omitempty"`
}

type Service struct {
	d Deps
	c *client

	mu        sync.Mutex
	busy      bool
	running   map[uint]bool
	latest    map[uint]Result // 最近一次(可能没测成)
	good      map[uint]Result // 最近一次测成的
	history   map[uint][]Point
	alerted   map[uint]string // 已经告过警的状态类别,防止同一件事反复发
	lastRun   int64           // 最近一次全部服务器都测过的时间(定时与手动都算)
	lastErr   string
	perCheck  int
	probeList []probe
	probeAt   time.Time

	ctx    context.Context
	cancel context.CancelFunc
	wg     sync.WaitGroup
}

func New(d Deps) *Service {
	if d.API == "" {
		d.API = DefaultAPI
	}
	if d.HTTP == nil {
		d.HTTP = &http.Client{Timeout: 30 * time.Second, Transport: &http.Transport{Proxy: http.ProxyFromEnvironment, TLSHandshakeTimeout: 10 * time.Second, IdleConnTimeout: 30 * time.Second}}
	}
	if d.Now == nil {
		d.Now = time.Now
	}
	if d.Poll == 0 {
		d.Poll = 1500 * time.Millisecond
	}
	if d.RecheckDelay == 0 {
		d.RecheckDelay = 2 * time.Minute
	}
	if d.FirstDelay == 0 {
		d.FirstDelay = 2 * time.Minute
	}
	s := &Service{d: d, running: map[uint]bool{}, latest: map[uint]Result{}, good: map[uint]Result{}, history: map[uint][]Point{}, alerted: map[uint]string{}, perCheck: 3*perCarrier + 3}
	s.c = &client{base: d.API, http: d.HTTP, ua: d.UA, now: d.Now, token: func() string { return d.Setting("reachToken") }}
	s.ctx, s.cancel = context.WithCancel(context.Background())
	s.load()
	return s
}

// Auto 定时巡检开着没有(默认开)。
func (s *Service) Auto() bool {
	return !strings.EqualFold(strings.TrimSpace(s.d.Setting("reachAuto")), "false")
}

// Minutes 巡检间隔,夹在 [30, 1440] 分钟。
func (s *Service) Minutes() int { return ClampMinutes(s.d.Setting("reachMinutes")) }

// ClampMinutes 设置值 → 实际间隔。空或不是数字按默认 90。
func ClampMinutes(v string) int {
	n, err := strconv.Atoi(strings.TrimSpace(v))
	if err != nil || n <= 0 {
		return DefaultMinutes
	}
	if n < MinMinutes {
		return MinMinutes
	}
	if n > MaxMinutes {
		return MaxMinutes
	}
	return n
}

// load 从库里恢复每台最近的结果与历史,以及告警基线(重启后不重复告同一件事)。只在 New 里调一次。
func (s *Service) load() {
	s.mu.Lock()
	defer s.mu.Unlock()
	if s.d.DB == nil {
		return
	}
	var rows []model.ReachCheck
	s.d.DB.Order("id asc").Find(&rows)
	for _, r := range rows {
		s.history[r.NodeId] = appendPoint(s.history[r.NodeId], Point{At: r.At, Verdict: r.Verdict, Auto: r.Auto})
		var res Result
		if json.Unmarshal([]byte(r.Detail), &res) == nil {
			s.latest[r.NodeId] = res
			if measured(res.Verdict) {
				s.good[r.NodeId] = res
			}
			if c := class(res.Verdict); c != "" {
				s.alerted[r.NodeId] = c
			}
		}
		if r.At > s.lastRun {
			s.lastRun = r.At
		}
	}
}

func appendPoint(h []Point, p Point) []Point {
	h = append(h, p)
	if len(h) > keepHistory {
		h = append([]Point(nil), h[len(h)-keepHistory:]...)
	}
	return h
}

// class 把结论归成告警关心的三类;检测没做成、香港也不通这些不算状态变化。
func class(v string) string {
	switch {
	case IsBlocked(v):
		return "blocked"
	case v == VerdictPartial:
		return "partial"
	case v == VerdictOK:
		return "ok"
	}
	return ""
}

// Start 启动定时巡检。副机上照样起循环,但每一轮都先看自己是不是主机。
func (s *Service) Start() {
	s.wg.Add(1)
	go func() {
		defer s.wg.Done()
		select { // 刚启动时公网 IP、副机状态都还没就绪,稍等再测
		case <-time.After(s.d.FirstDelay):
		case <-s.ctx.Done():
			return
		}
		t := time.NewTicker(time.Minute)
		defer t.Stop()
		for {
			s.tick()
			select {
			case <-t.C:
			case <-s.ctx.Done():
				return
			}
		}
	}()
}

func (s *Service) Stop() {
	s.cancel()
	s.wg.Wait()
}

func (s *Service) tick() {
	defer func() {
		if v := recover(); v != nil {
			logger.Warning("大陆连通巡检异常: ", v, " | ", string(debug.Stack()))
		}
	}()
	if s.d.IsNode() || !s.Auto() {
		return
	}
	s.mu.Lock()
	due := s.d.Now().Unix()-s.lastRun >= int64(s.Minutes())*60
	s.mu.Unlock()
	if !due {
		return
	}
	if err := s.begin(nil); err != nil {
		return // 手动那轮正在跑,下一分钟再看
	}
	s.runAll(s.ctx, nil, true)
}

// Run 手动检测:ids 为空 = 全部服务器。立即返回,结果在 View 里看。
func (s *Service) Run(ids []uint) error {
	if s.d.IsNode() {
		return errors.New("副服务器不做大陆连通检测,请在主机的服务器页操作")
	}
	if err := s.begin(ids); err != nil {
		return err
	}
	s.wg.Add(1)
	go func() {
		defer s.wg.Done()
		defer func() {
			if v := recover(); v != nil {
				logger.Warning("大陆连通检测异常: ", v, " | ", string(debug.Stack()))
			}
		}()
		s.runAll(s.ctx, ids, false)
	}()
	return nil
}

func (s *Service) begin(ids []uint) error {
	s.mu.Lock()
	defer s.mu.Unlock()
	if s.busy {
		return ErrBusy
	}
	s.busy = true
	s.running = map[uint]bool{}
	for _, id := range ids {
		s.running[id] = true
	}
	return nil
}

func (s *Service) finish(full bool) {
	s.mu.Lock()
	defer s.mu.Unlock()
	s.busy = false
	s.running = map[uint]bool{}
	if full {
		s.lastRun = s.d.Now().Unix()
	}
}

func (s *Service) runAll(ctx context.Context, ids []uint, auto bool) {
	full := len(ids) == 0
	defer s.finish(full)

	all := s.d.Targets(ctx)
	if full {
		s.forgetMissing(all)
	}
	var targets []Target
	want := map[uint]bool{}
	for _, id := range ids {
		want[id] = true
	}
	for _, t := range all {
		if full || want[t.NodeId] {
			targets = append(targets, t)
		}
	}
	s.mu.Lock()
	s.running = map[uint]bool{}
	for _, t := range targets {
		s.running[t.NodeId] = true
	}
	s.mu.Unlock()

	locs, cost, err := s.locations(ctx)
	s.mu.Lock()
	s.lastErr = ""
	if err != nil {
		s.lastErr = err.Error()
	}
	s.mu.Unlock()

	sem := make(chan struct{}, parallelChecks)
	var wg sync.WaitGroup
	for _, t := range targets {
		t := t
		wg.Add(1)
		sem <- struct{}{}
		go func() {
			defer wg.Done()
			defer func() { <-sem }()
			defer func() {
				if v := recover(); v != nil {
					logger.Warning("大陆连通检测异常: ", t.Name, " ", v, " | ", string(debug.Stack()))
				}
				s.mu.Lock()
				delete(s.running, t.NodeId)
				s.mu.Unlock()
			}()
			var res Result
			if err != nil {
				res = s.failed(t, auto, err.Error())
			} else {
				res = s.check(ctx, t, locs, cost, auto)
			}
			if auto && s.needsRecheck(res) {
				select {
				case <-time.After(s.d.RecheckDelay):
					again := s.check(ctx, t, locs, cost, auto)
					if again.Verdict != VerdictError { // 复测没做成就以第一次为准
						again.Rechecked = true
						res = again
					}
				case <-ctx.Done():
				}
			}
			if ctx.Err() != nil && res.Verdict == VerdictError {
				return // 面板正在退出:别把"被取消"记成一次失败
			}
			s.store(res)
		}()
	}
	wg.Wait()
}

// locations 选好这一轮用的测点(列表缓存 6 小时),返回每台服务器一次测量要用的测点次数。
func (s *Service) locations(ctx context.Context) ([]location, int, error) {
	s.mu.Lock()
	list, at := s.probeList, s.probeAt
	s.mu.Unlock()
	if list == nil || s.d.Now().Sub(at) > probeListTTL {
		c, cancel := context.WithTimeout(ctx, 40*time.Second)
		fresh, err := s.c.probes(c)
		cancel()
		switch {
		case err == nil:
			list = fresh
			s.mu.Lock()
			s.probeList, s.probeAt = fresh, s.d.Now()
			s.mu.Unlock()
		case list == nil:
			return nil, 0, fmt.Errorf("拿不到 Globalping 测点列表: %v", err)
		}
	}
	cn, planned := pickLocations(list, perCarrier)
	n := 0
	for _, v := range planned {
		n += v
	}
	if n == 0 {
		return nil, 0, errors.New("Globalping 现在没有在线的大陆家宽测点,稍后再试")
	}
	locs := append(cn, abroadLocations...)
	cost := 0
	for _, l := range locs {
		cost += l.Limit
	}
	s.mu.Lock()
	s.perCheck = cost
	s.mu.Unlock()
	return locs, cost, nil
}

func (s *Service) failed(t Target, auto bool, msg string) Result {
	return Result{NodeId: t.NodeId, Name: t.Name, Target: t.Host, Port: t.Port, PortFrom: t.PortFrom, PortLine: t.PortLine,
		At: s.d.Now().Unix(), Auto: auto, Verdict: VerdictError, Error: msg, Groups: orderedGroups(summarize(nil))}
}

// check 测一台:先从大陆三网 + 香港连端口;大陆全不通时再用同一批测点 ping 一次,分清 IP 被墙还是端口被封。
func (s *Service) check(ctx context.Context, t Target, locs []location, cost int, auto bool) Result {
	if t.Err != "" {
		return s.failed(t, auto, t.Err)
	}
	if t.Host == "" {
		return s.failed(t, auto, "没有可测的地址")
	}
	if q := s.c.Quota(); q.At > 0 && q.Remaining < cost && s.d.Now().Unix() < q.ResetAt {
		return s.failed(t, auto, (&QuotaError{Wait: time.Unix(q.ResetAt, 0).Sub(s.d.Now())}).Error())
	}
	res := Result{NodeId: t.NodeId, Name: t.Name, Target: t.Host, Port: t.Port, PortFrom: t.PortFrom, PortLine: t.PortLine, At: s.d.Now().Unix(), Auto: auto}
	opts := pingOptions{Packets: 3}
	if t.Port > 0 {
		opts.Protocol, opts.Port = "TCP", t.Port
	}
	c, cancel := context.WithTimeout(ctx, 90*time.Second)
	defer cancel()
	m, err := s.c.measure(c, measureRequest{Type: "ping", Target: t.Host, Locs: locs, Options: opts}, s.d.Poll)
	if err != nil {
		return s.failed(t, auto, err.Error())
	}
	res.Measurements = []string{m.Id}
	gs := summarize(m)
	var icmp *ICMPCheck
	if t.Port > 0 && allCarriersDown(gs) && gs[GroupAbroad].OK > 0 {
		if m2, err := s.c.measure(c, measureRequest{Type: "ping", Target: t.Host, Locs: m.Id, Options: pingOptions{Packets: 3}}, s.d.Poll); err == nil {
			res.Measurements = append(res.Measurements, m2.Id)
			g2 := summarize(m2)
			icmp = &ICMPCheck{HKOK: g2[GroupAbroad].OK, HKTotal: g2[GroupAbroad].Total}
			for _, k := range carrierGroups {
				icmp.CNOK += g2[k].OK
				icmp.CNTotal += g2[k].Total
			}
		} else {
			res.ICMPError = err.Error()
		}
	}
	res.Verdict = verdict(gs, icmp, t.Port == 0)
	res.Groups = orderedGroups(gs)
	res.ICMP = icmp
	return res
}

func allCarriersDown(gs map[string]*Group) bool {
	measured := 0
	for _, k := range carrierGroups {
		switch gs[k].State {
		case StateNone:
		case StateDown:
			measured++
		default:
			return false
		}
	}
	return measured > 0
}

// needsRecheck 定时巡检刚发现异常(和上一次不同)时,隔两分钟再测一次再下结论,免得测点抽风就报警。
func (s *Service) needsRecheck(res Result) bool {
	c := class(res.Verdict)
	if c != "blocked" && c != "partial" {
		return false
	}
	s.mu.Lock()
	prev := s.good[res.NodeId] // 中间没测成的那几次不算"变化"
	s.mu.Unlock()
	return class(prev.Verdict) != c
}

func (s *Service) store(res Result) {
	s.mu.Lock()
	s.latest[res.NodeId] = res
	if measured(res.Verdict) {
		s.good[res.NodeId] = res
	}
	s.history[res.NodeId] = appendPoint(s.history[res.NodeId], Point{At: res.At, Verdict: res.Verdict, Auto: res.Auto})
	prevAlert := s.alerted[res.NodeId]
	c := class(res.Verdict)
	if c != "" {
		s.alerted[res.NodeId] = c
	}
	s.mu.Unlock()

	if s.d.DB != nil {
		detail, _ := json.Marshal(res)
		if err := s.d.DB.Create(&model.ReachCheck{NodeId: res.NodeId, At: res.At, Verdict: res.Verdict, Auto: res.Auto, Detail: string(detail)}).Error; err != nil {
			logger.Warning("保存大陆连通检测结果失败: ", err)
		}
		s.d.DB.Exec("DELETE FROM reach_checks WHERE node_id = ? AND id NOT IN (SELECT id FROM reach_checks WHERE node_id = ? ORDER BY id DESC LIMIT ?)", res.NodeId, res.NodeId, keepRows)
	}
	logger.Info("大陆连通检测 ", res.Name, " ", res.Target, ": ", res.Verdict, summaryLine(res))

	// 只有定时巡检发告警;手动检测的结果你已经在页面上看到了,但同样更新基线,之后不重复告同一件事
	if !res.Auto || c == "" || c == prevAlert || s.d.Notify == nil {
		return
	}
	if c == "ok" && prevAlert == "" {
		return // 第一次测就正常,没什么可说的
	}
	s.d.Notify("tgOnReach", alertText(res))
}

// forgetMissing 服务器删掉了,它的记录也不留。
func (s *Service) forgetMissing(targets []Target) {
	if len(targets) == 0 {
		return // 本机那条记录总在;一台都没拿到只能是读库出错,别据此清空历史
	}
	keep := map[uint]bool{}
	ids := []uint{}
	for _, t := range targets {
		keep[t.NodeId] = true
		ids = append(ids, t.NodeId)
	}
	s.mu.Lock()
	for id := range s.latest {
		if !keep[id] {
			delete(s.latest, id)
			delete(s.good, id)
			delete(s.alerted, id)
		}
	}
	for id := range s.history {
		if !keep[id] {
			delete(s.history, id)
		}
	}
	s.mu.Unlock()
	if s.d.DB != nil {
		s.d.DB.Where("node_id NOT IN ?", ids).Delete(&model.ReachCheck{})
	}
}

// Forget 服务器删掉了:忘掉它的结果与告警基线(库里的记录由删服务器的事务一起删)。
func (s *Service) Forget(id uint) {
	s.mu.Lock()
	defer s.mu.Unlock()
	delete(s.latest, id)
	delete(s.good, id)
	delete(s.history, id)
	delete(s.alerted, id)
}

// View 当前状态。
func (s *Service) View() View {
	s.mu.Lock()
	defer s.mu.Unlock()
	v := View{Auto: s.Auto(), Minutes: s.Minutes(), LastRun: s.lastRun, Running: s.busy, RunningIds: []uint{},
		TokenSet: strings.TrimSpace(s.d.Setting("reachToken")) != "", PerCheck: s.perCheck, Error: s.lastErr,
		Results: []Result{}, History: map[uint][]Point{}}
	if v.Auto {
		if s.lastRun > 0 {
			v.NextRun = s.lastRun + int64(v.Minutes)*60
		} else {
			v.NextRun = s.d.Now().Unix() + int64(s.d.FirstDelay/time.Second)
		}
	}
	for id := range s.running {
		v.RunningIds = append(v.RunningIds, id)
	}
	sort.Slice(v.RunningIds, func(i, j int) bool { return v.RunningIds[i] < v.RunningIds[j] })
	if q := s.c.Quota(); q.At > 0 {
		v.Quota = &q
	}
	for id, last := range s.latest {
		r := last
		if g, ok := s.good[id]; ok && !measured(last.Verdict) {
			// 最近一次没测成:显示再上一次测成的结果,注明最近那次的失败原因
			r = g
			r.Attempt = &Attempt{At: last.At, Verdict: last.Verdict, Error: last.Error}
		}
		v.Results = append(v.Results, r)
		v.History[id] = append([]Point(nil), s.history[id]...)
	}
	sort.Slice(v.Results, func(i, j int) bool { return v.Results[i].NodeId < v.Results[j].NodeId })
	return v
}

// Bad 当前有异常的服务器(概览页顶部提示用)。
func (s *Service) Bad() []Result {
	s.mu.Lock()
	defer s.mu.Unlock()
	var out []Result
	for _, r := range s.good { // 没测成的那次不能把"被墙"的提示抹掉
		if IsBlocked(r.Verdict) || r.Verdict == VerdictPartial {
			out = append(out, Result{NodeId: r.NodeId, Name: r.Name, Target: r.Target, Verdict: r.Verdict, At: r.At, Groups: r.Groups})
		}
	}
	sort.Slice(out, func(i, j int) bool { return out[i].NodeId < out[j].NodeId })
	return out
}

var groupNames = map[string]string{GroupMobile: "移动", GroupTelecom: "电信", GroupUnicom: "联通", GroupAbroad: "香港"}

func summaryLine(r Result) string {
	var parts []string
	for _, g := range r.Groups {
		if g.Total == 0 {
			continue
		}
		parts = append(parts, fmt.Sprintf("%s %d/%d", groupNames[g.Key], g.OK, g.Total))
	}
	if r.Error != "" {
		parts = append(parts, r.Error)
	}
	if len(parts) == 0 {
		return ""
	}
	return "(" + strings.Join(parts, " · ") + ")"
}

func alertText(r Result) string {
	addr := r.Target
	if r.Port > 0 {
		addr = fmt.Sprintf("%s:%d", r.Target, r.Port)
	}
	var head, why string
	switch r.Verdict {
	case VerdictIPBlocked:
		head, why = "🚫 <b>IP 疑似被墙</b>", "大陆三网连端口和 ping 都不通,香港正常。换 IP 才能恢复。"
	case VerdictPortBlocked:
		head, why = "🚫 <b>端口疑似被封</b>", "大陆三网 ping 得通、端口连不上,香港正常。换个端口即可。"
	case VerdictBlocked:
		head, why = "🚫 <b>疑似被墙</b>", "大陆三网都连不上,香港正常。"
	case VerdictPartial:
		var down []string
		for _, g := range r.Groups {
			if g.Key != GroupAbroad && g.State == StateDown {
				down = append(down, groupNames[g.Key])
			}
		}
		head, why = "⚠️ <b>部分运营商不通</b>", strings.Join(down, "、")+"连不上,其它运营商正常。"
	case VerdictOK:
		head = "✅ <b>大陆连通已恢复</b>"
	}
	var lines []string
	for _, g := range r.Groups {
		if g.Total == 0 {
			continue
		}
		s := fmt.Sprintf("%s %d/%d", groupNames[g.Key], g.OK, g.Total)
		if g.OK > 0 && g.AvgMs > 0 {
			s += fmt.Sprintf(" %.0fms", g.AvgMs)
		}
		lines = append(lines, s)
	}
	text := fmt.Sprintf("%s:%s(%s)\n%s", head, notify.Esc(r.Name), notify.Esc(addr), notify.Esc(strings.Join(lines, " · ")))
	if why != "" {
		text += "\n" + why
	}
	return text
}
