package web

import (
	"encoding/json"
	"fmt"
	"regexp"
	"strconv"
	"strings"
	"time"

	"github.com/Maoyangui/m-ui/database/model"
	"github.com/Maoyangui/m-ui/ext"
	"github.com/Maoyangui/m-ui/logger"
	"github.com/Maoyangui/m-ui/notify"
	"github.com/Maoyangui/m-ui/render"
	"github.com/Maoyangui/m-ui/safe"

	"gorm.io/gorm"
)

// ---- 外部节点加成的上游:坏了才跟着订阅改 ----
//
// 把外部订阅里的一个节点「添加为上游」以后,上游里存的是那一刻的地址、端口和参数。机场改了这个节点(换端口、换密码、换域名),
// 上游就连不上了,以前只能自己发现、再手动照着改一遍。
//
// 现在的规矩(用户定的):**通着就不动**。只有这条上游在所有用它的服务器上都连续测不通,才去看订阅:
//
//	1. 先把订阅重新抓一遍(单条链接不用抓);抓不下来这次就算了,过半小时再试。
//	2. 原来那个节点(按身份指纹认)还在订阅里、一字没变 → 故障不是因为它改了配置,不动上游,记一句「订阅里没变」。
//	3. 指纹不在了 → 找它现在变成了谁,只认**唯一**对得上的:同协议里 ① 名字一模一样;② 去掉名字里会变的数字
//	   (剩余流量、天数、日期、倍率)后一样;③ 服务器地址一样(只换了端口 / 参数)。对上两个以上、或一个都对不上 → 不猜,
//	   不动上游,记「找不到对应的节点」,等人来看。
//	4. 找到了 → 新配置先过和手动保存同一套校验(参数干跑、整份配置校验),通过才换上;上游的名字、编号不变,
//	   用它的线路不用动。换完十秒后各台自动复测一次;记审计、发 Telegram。
//
// 换上去的新配置如果还是不通,不会来回折腾:它的指纹此刻就在订阅里(第 2 步),之后每次都是「订阅里没变」,直到订阅再变。
// 同一条上游两次尝试至少隔半小时。手动改过这条上游的类型或参数,就不再跟随(改名不算)。

const (
	followEvery    = 2 * time.Minute  // 多久看一遍
	followCooldown = 30 * time.Minute // 同一条上游两次尝试至少隔这么久
)

// 一次尝试的结论(存进 UpstreamSource.Note,界面按它显示)。
const (
	followOK        = "followed"  // 跟着改了
	followUnchanged = "unchanged" // 订阅里那个节点没变
	followGone      = "gone"      // 订阅里找不到对应的节点
	followAmbiguous = "ambiguous" // 对得上的不止一个,不猜
	followFetch     = "fetch"     // 订阅没抓下来
	followInvalid   = "invalid"   // 找到了,但新配置转不成本站的出站 / 校验不过
)

// allFailing 这条上游是不是在所有用它的服务器上都连续测不通:每台都是「故障」而且至少连续失败 threshold 次。
// 有一台通着、或者有一台还没测到 / 失联(不知道通不通),都不算。
func allFailing(row upRow, threshold int) bool {
	if row.Unused || len(row.Servers) == 0 {
		return false
	}
	if threshold < 2 {
		threshold = 2 // 至少连着两轮:一次抖动不值得去动配置
	}
	for _, sv := range row.Servers {
		if sv.State != "fail" || sv.Fails < threshold {
			return false
		}
	}
	return true
}

// followCand 订阅里现在的一个节点(已经转成本站出站的那些)。
type followCand struct {
	Key, Name, Type, Server string
	Port                    int
}

var dynamicName = regexp.MustCompile(`(?i)\d+(\.\d+)?\s*(GB|MB|TB|KB|G|M|T|天|日|小时|%|倍|x)|\d{4}[-/.年]\d{1,2}[-/.月]\d{1,2}日?|\d+(\.\d+)?`)

// normName 名字里会变的部分(剩余流量、天数、日期、倍率、各种数字)抹掉、去掉空白后的样子。
func normName(s string) string {
	s = dynamicName.ReplaceAllString(s, "#")
	return strings.Join(strings.Fields(strings.ToLower(s)), "")
}

// pickSuccessor 在订阅现在的节点里找「原来那个节点现在是谁」。返回下标(-1 = 没有)和结论(找到了为空)。
// 只认唯一对得上的,规则见文件头。
func pickSuccessor(src model.UpstreamSource, curType, curServer string, cands []followCand) (int, string) {
	for _, c := range cands {
		if c.Key == src.Key {
			return -1, followUnchanged
		}
	}
	same := func(match func(c followCand) bool) []int {
		var hit []int
		for i, c := range cands {
			if c.Type == curType && match(c) {
				hit = append(hit, i)
			}
		}
		return hit
	}
	sawMany := false
	for _, match := range []func(c followCand) bool{
		func(c followCand) bool { return src.Name != "" && c.Name == src.Name },
		func(c followCand) bool { return src.Name != "" && normName(c.Name) == normName(src.Name) },
		func(c followCand) bool { return curServer != "" && strings.EqualFold(c.Server, curServer) },
	} {
		hit := same(match)
		if len(hit) == 1 {
			return hit[0], ""
		}
		if len(hit) > 1 {
			sawMany = true
		}
	}
	if sawMany {
		return -1, followAmbiguous
	}
	return -1, followGone
}

// endpointOf 出站参数里的「地址:端口」,给人看跟随前后改了什么。
func endpointOf(opts json.RawMessage) (server string, port int) {
	var o struct {
		Server string      `json:"server"`
		Port   interface{} `json:"server_port"`
	}
	_ = json.Unmarshal(opts, &o)
	switch v := o.Port.(type) {
	case float64:
		port = int(v)
	case string:
		port, _ = strconv.Atoi(v)
	}
	return o.Server, port
}

// sameOutbound 两份出站参数是不是同一份(键的顺序不同不算不同)。
func sameOutbound(typeA string, a json.RawMessage, typeB string, b json.RawMessage) bool {
	if typeA != typeB {
		return false
	}
	var x, y interface{}
	if json.Unmarshal(a, &x) != nil || json.Unmarshal(b, &y) != nil {
		return false
	}
	bx, _ := json.Marshal(x)
	by, _ := json.Marshal(y)
	return string(bx) == string(by)
}

// candsOf 一份外部订阅现在的节点里,能当上游的那些。
func candsOf(e model.ExtNode) (ext.Items, []followCand, []int) {
	it := ext.ItemsOf(e)
	var cands []followCand
	var idx []int
	for i, n := range it.Nodes {
		typ, opts, err := ext.Outbound(n)
		if err != nil {
			continue
		}
		b, _ := json.Marshal(opts)
		server, port := endpointOf(b)
		cands = append(cands, followCand{Key: n.Key, Name: n.Name, Type: typ, Server: server, Port: port})
		idx = append(idx, i)
	}
	return it, cands, idx
}

// refreshExtNow 把一条外部订阅重新抓一遍(测试里换成假的)。
func (s *Server) refreshExtNow(id uint) error {
	if s.extRefreshFn != nil {
		return s.extRefreshFn(id)
	}
	if s.run == nil {
		return nil
	}
	_, err := s.run.RefreshExt(id)
	return err
}

// followLoop 主机上定时看一遍。
func (s *Server) followLoop() {
	t := time.NewTicker(followEvery)
	defer t.Stop()
	for range t.C {
		safe.Do("上游跟随", func() { s.followTick(time.Now()) }) // 外部订阅的内容不归我们管:解析、比对里出什么意外都只影响这一轮
	}
}

// followTick 看一遍:先把以前加的、没记来源的上游对上号,再给全坏的那些尝试跟随。只在主机上做。
func (s *Server) followTick(now time.Time) {
	if s.role() != "master" {
		return
	}
	s.linkUpstreamSources()
	var srcs []model.UpstreamSource
	s.db.Find(&srcs)
	if len(srcs) == 0 {
		return
	}
	rows := map[uint]upRow{}
	for _, r := range s.upstreamHealthRows() {
		rows[r.Id] = r
	}
	threshold := s.settingInt("upstreamCheckFailThreshold", 2)
	for _, src := range srcs {
		row, ok := rows[src.UpstreamId]
		if !ok { // 上游已经删了:来源记录跟着清掉
			s.db.Delete(&model.UpstreamSource{}, "upstream_id = ?", src.UpstreamId)
			continue
		}
		if src.Stop != "" { // 停止跟随了:来源留着,不动上游
			continue
		}
		if !allFailing(row, threshold) {
			continue
		}
		if src.TriedAt > 0 && now.Unix()-src.TriedAt < int64(followCooldown/time.Second) {
			continue
		}
		s.followOne(src, now.Unix())
	}
}

// followOne 给一条全坏的上游尝试跟随一次。
func (s *Server) followOne(src model.UpstreamSource, now int64) {
	var up model.Upstream
	if s.db.First(&up, src.UpstreamId).Error != nil {
		return
	}
	mark := func(note, detail string) {
		upd := map[string]interface{}{"tried_at": now, "note": note, "detail": detail}
		if note == followOK {
			upd["followed_at"] = now
		}
		s.db.Model(&model.UpstreamSource{}).Where("upstream_id = ?", up.Id).Updates(upd)
		// 同一个结论不重复吵:只有结论变了(或者这次真的改了)才通知
		if s.run != nil && note != followUnchanged && (note == followOK || note != src.Note) {
			changed := detail
			if changed == "" {
				changed = "地址没变,换了连接参数"
			}
			text := map[string]string{
				followOK:        "✅ <b>上游已跟随外部节点的变化</b>\n" + notify.Esc(up.Name) + ":" + notify.Esc(changed),
				followGone:      "⚠️ <b>上游全部测不通,订阅里也找不到对应的节点</b>\n" + notify.Esc(up.Name) + "(原节点「" + notify.Esc(src.Name) + "」):请到外部节点页重新挑一个加为上游",
				followAmbiguous: "⚠️ <b>上游全部测不通,订阅里对得上的节点不止一个</b>\n" + notify.Esc(up.Name) + "(原节点「" + notify.Esc(src.Name) + "」):没有自动改,请手动确认",
				followInvalid:   "⚠️ <b>上游全部测不通,订阅里的新配置用不了</b>\n" + notify.Esc(up.Name) + ":" + notify.Esc(detail),
			}[note]
			if text != "" {
				// 跟着改好了是一条「已经处理」的消息,单独一个开关「上游自动跟随订阅的变化」(用户 11-22:「上游跟随订阅变化那个
				// telegram 提醒不能关闭吗? 很烦人」);跟不上的(订阅里找不到 / 对不上 / 新配置用不了)要人去处理,归「上游故障 / 恢复」。
				// 以前全归在「数据面停止 / 恢复」下面:关它就连数据面出事也不报了,等于关不掉
				key := "tgOnUpstream"
				if note == followOK {
					key = "tgOnUpstreamFollow"
				}
				s.run.Notifier().Event(key, text)
			}
		}
	}
	var e model.ExtNode
	if s.db.First(&e, src.ExtId).Error != nil { // 那条外部订阅删了:没有可跟的了
		s.db.Delete(&model.UpstreamSource{}, "upstream_id = ?", up.Id)
		return
	}
	if e.Type != "link" {
		if err := s.refreshExtNow(e.Id); err != nil {
			mark(followFetch, err.Error())
			return
		}
		if s.db.First(&e, src.ExtId).Error != nil {
			return
		}
	}
	it, cands, idx := candsOf(e)
	curServer, curPort := endpointOf(up.Options)
	i, verdict := pickSuccessor(src, up.Type, curServer, cands)
	if verdict == followUnchanged {
		// 订阅里那个节点一字没变。可上游现在的配置不一定就是它:手动改过参数(于是停了跟随)、之后又把跟随打开的,
		// 两边已经不一样 —— 这时该做的正是把订阅里的那份换回来;一样才是真的「没变,不动」
		for k, c := range cands {
			if c.Key != src.Key {
				continue
			}
			if typ, opts, err := ext.Outbound(it.Nodes[idx[k]]); err == nil {
				if b, _ := json.Marshal(opts); !sameOutbound(up.Type, up.Options, typ, b) {
					i, verdict = k, ""
				}
			}
			break
		}
	}
	if verdict != "" {
		mark(verdict, "")
		return
	}
	typ, opts, err := ext.Outbound(it.Nodes[idx[i]])
	if err != nil {
		mark(followInvalid, err.Error())
		return
	}
	b, _ := json.Marshal(opts)
	next := up
	next.Type, next.Options = typ, b
	if err := s.validateUpstream(&next); err != nil {
		mark(followInvalid, err.Error())
		return
	}
	if err := s.dryRunUpstream(&next); err != nil {
		mark(followInvalid, err.Error())
		return
	}
	write := func(tx *gorm.DB) error {
		if err := tx.Model(&model.Upstream{}).Where("id = ?", up.Id).Select("type", "options").Updates(next).Error; err != nil {
			return err
		}
		return tx.Model(&model.UpstreamSource{}).Where("upstream_id = ?", up.Id).
			Updates(map[string]interface{}{"key": cands[i].Key, "name": cands[i].Name}).Error
	}
	if s.run == nil {
		err = s.db.Transaction(write)
	} else {
		err = s.commitValidated(write)
	}
	if err != nil {
		mark(followInvalid, err.Error())
		return
	}
	// 存进库的补充只写「旧地址 → 新地址」(界面两种语言都能直接显示);地址没变、只换了参数时留空,由界面自己说
	detail, human := "", "地址没变,换了连接参数"
	if curServer != cands[i].Server || curPort != cands[i].Port {
		detail = fmt.Sprintf("%s:%d → %s:%d", curServer, curPort, cands[i].Server, cands[i].Port)
		human = detail
	}
	mark(followOK, detail)
	s.auditAs("FollowJob", "upstream", "follow", up.Name+"("+human+")")
	logger.Info("上游 ", up.Name, " 全部测不通,已跟随外部订阅「", e.Name, "」里节点的变化: ", human)
	if s.run != nil {
		s.reloadUpstreams("上游 " + up.Name + " 跟随外部节点的变化")
		s.recheckUpstreamSoon(up.Id)
	}
}

// linkUpstreamSources 给以前从外部节点加的、还没记来源的上游对上号:出站参数和订阅里某个节点一模一样就记下它。
// (记来源是这一版才有的;不补这一步,升级前加的上游永远跟不了。)线路上游、上游组不是从外部节点来的,不看。
func (s *Server) linkUpstreamSources() {
	var ups []model.Upstream
	s.db.Where("from_line = 0 AND id NOT IN (SELECT upstream_id FROM upstream_sources)").Find(&ups)
	var loose []model.Upstream
	for _, u := range ups {
		if !render.IsGroup(u) {
			loose = append(loose, u)
		}
	}
	if len(loose) == 0 {
		return
	}
	var exts []model.ExtNode
	s.db.Find(&exts)
	for _, e := range exts {
		it, cands, idx := candsOf(e)
		for ci, c := range cands {
			_, opts, err := ext.Outbound(it.Nodes[idx[ci]])
			if err != nil {
				continue
			}
			b, _ := json.Marshal(opts)
			for k := 0; k < len(loose); k++ {
				if sameOutbound(loose[k].Type, loose[k].Options, c.Type, b) {
					s.db.Create(&model.UpstreamSource{UpstreamId: loose[k].Id, ExtId: e.Id, Key: c.Key, Name: c.Name})
					loose = append(loose[:k], loose[k+1:]...)
					k--
				}
			}
		}
	}
}

// upSourceView 上游列表里带给面板的来源信息。
type upSourceView struct {
	ExtId      uint   `json:"extId"`
	ExtName    string `json:"extName"`
	Node       string `json:"node"`
	Note       string `json:"note,omitempty"`
	Detail     string `json:"detail,omitempty"`
	TriedAt    int64  `json:"triedAt,omitempty"`
	FollowedAt int64  `json:"followedAt,omitempty"`
	Stop       string `json:"stop,omitempty"` // 停止跟随的原因(user / edited);空 = 跟随着
}

// upstreamSources 全部上游的来源(上游 id → 来源)。
func (s *Server) upstreamSources() map[uint]*upSourceView {
	var srcs []model.UpstreamSource
	s.db.Find(&srcs)
	out := make(map[uint]*upSourceView, len(srcs))
	if len(srcs) == 0 {
		return out
	}
	names := map[uint]string{}
	var exts []model.ExtNode
	s.db.Select("id, name").Find(&exts)
	for _, e := range exts {
		names[e.Id] = e.Name
	}
	for _, x := range srcs {
		out[x.UpstreamId] = &upSourceView{ExtId: x.ExtId, ExtName: names[x.ExtId], Node: x.Name, Note: x.Note, Detail: x.Detail, TriedAt: x.TriedAt, FollowedAt: x.FollowedAt, Stop: x.Stop}
	}
	return out
}
