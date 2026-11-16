package runner

import (
	"encoding/json"
	"regexp"
	"sort"
	"time"

	"github.com/Maoyangui/m-ui/database/model"
	"github.com/Maoyangui/m-ui/logger"
	"github.com/Maoyangui/m-ui/util/portprobe"
)

// 端口被这台机器上别的程序占着的入站:只摘掉它,别的照常服务。
//
// 以前一份配置里只要有一个入站监听不上(典型:中转入口的端口上已经跑着 realm),整份配置启动失败 → 回滚到上一份 →
// 冷却 10 分钟再试,期间这台服务器上线路的任何改动都生效不了,面板上只有一句原始的 bind 报错。
// 现在:启动报「address already in use」、点名的是一个新出现的入站(上一份运行中的配置里没有这个 tag)时,
// 把它摘掉再起(最多 maxBusySkip 个),记进名单。已有的入站(线路改端口改到了被占的端口)不摘 —— 摘了这条线路就停了;
// 那种情况照旧整份回滚,线路继续在原来的端口上服务。
// 之后进来的配置先摘掉名单里的再比对 / 应用(不然每次推送都当成"入站变了"去重启);
// 每分钟试一次监听,端口空出来就把入站热加回去(不重启数据面);管理员改了端口 / 删了,名单里那条自己消失。
// 名单随报告上报给主机(BlockedInbounds),主机在服务器页、概况、线路详情里点名。

// maxBusySkip 一次启动最多摘掉几个端口被占的入站;再多说明这台机器不对劲,按原来的失败处理。
const maxBusySkip = 8

type blockedInbound struct {
	def  json.RawMessage // 入站定义(只用来对端口;加回去时用新渲染的那份)
	info model.BlockedInbound
}

// busyRe sing-box 启动报错里「哪个入站监听不上」那一段:
// start inbound/direct[US中转 ⇄13024]: listen tcp 0.0.0.0:13024: bind: address already in use
var busyRe = regexp.MustCompile(`inbound/[^\[\s]+\[(.*)\]: listen (tcp|udp)\b.*(?:address already in use|Only one usage of each socket address)`)

// busyInboundOf 报错点名的那个端口被占的入站(tag,以及没监听上的是 tcp 还是 udp);不是这类错误返回空。
func busyInboundOf(err error) (tag, network string) {
	if err == nil {
		return "", ""
	}
	m := busyRe.FindStringSubmatch(err.Error())
	if m == nil {
		return "", ""
	}
	return m[1], m[2]
}

// inboundMeta 入站定义里的 tag 与监听端口。
func inboundMeta(def []byte) (tag string, port int) {
	var m struct {
		Tag  string `json:"tag"`
		Port int    `json:"listen_port"`
	}
	_ = json.Unmarshal(def, &m)
	return m.Tag, m.Port
}

// withoutInbound 配置里去掉 tag 这个入站:返回新配置、被去掉的入站定义;没有这个入站(或解析不了)返回 nil。
// 路由规则不动:规则里按入站 tag 匹配,入站不在了那条规则只是匹配不到,等入站加回来照旧生效。
func withoutInbound(raw []byte, tag string) (out []byte, def json.RawMessage) {
	var cfg map[string]json.RawMessage
	if json.Unmarshal(raw, &cfg) != nil {
		return nil, nil
	}
	var inbounds []json.RawMessage
	if json.Unmarshal(cfg["inbounds"], &inbounds) != nil {
		return nil, nil
	}
	kept := make([]json.RawMessage, 0, len(inbounds))
	for _, ib := range inbounds {
		if t, _ := inboundMeta(ib); def == nil && t == tag {
			def = ib
			continue
		}
		kept = append(kept, ib)
	}
	if def == nil {
		return nil, nil
	}
	b, err := json.Marshal(kept)
	if err != nil {
		return nil, nil
	}
	cfg["inbounds"] = b
	if out, err = json.Marshal(cfg); err != nil {
		return nil, nil
	}
	return out, def
}

// dropBlocked 进来的配置先摘掉名单里的入站(tag 和端口都对得上才摘)。配置里已经没有的 —— 管理员改了端口、删了入口、
// 改成了「已有转发」—— 从名单里去掉。名单为空时原样返回(不重新序列化)。
func (r *Runner) dropBlocked(raw []byte) []byte {
	r.blockedMu.Lock()
	defer r.blockedMu.Unlock()
	if len(r.blocked) == 0 {
		return raw
	}
	var cfg struct {
		Inbounds []json.RawMessage `json:"inbounds"`
	}
	if json.Unmarshal(raw, &cfg) != nil {
		return raw
	}
	present := map[string]int{}
	for _, ib := range cfg.Inbounds {
		if t, p := inboundMeta(ib); t != "" {
			present[t] = p
		}
	}
	for tag, b := range r.blocked {
		if p, ok := present[tag]; !ok || p != b.info.Port {
			delete(r.blocked, tag)
			continue
		}
		if out, _ := withoutInbound(raw, tag); out != nil {
			raw = out
		}
	}
	return raw
}

// noteBlocked 记下一个端口被占的入站(已经在名单里的保留原来的起始时间)。
func (r *Runner) noteBlocked(def json.RawMessage, network string) {
	tag, port := inboundMeta(def)
	if tag == "" {
		return
	}
	by := portprobe.Holder(port, network)
	r.blockedMu.Lock()
	defer r.blockedMu.Unlock()
	if r.blocked == nil {
		r.blocked = map[string]blockedInbound{}
	}
	since := time.Now().Unix()
	if old, ok := r.blocked[tag]; ok && old.info.Port == port {
		since = old.info.Since
	}
	r.blocked[tag] = blockedInbound{def: def, info: model.BlockedInbound{Tag: tag, Port: port, Net: network, By: by, Since: since}}
	who := ""
	if by != "" {
		who = "(" + by + ")"
	}
	logger.Warning("入站 ", tag, " 没有开:端口 ", port, " 被本机别的程序占着", who, ";其余入站照常服务,端口空出来后自动补上")
}

// startSkippingBusy 启动报「端口被占」时,把点名的那个入站摘掉再起,最多摘 maxBusySkip 个;prev 是上一份运行中的配置
// (没有为 nil),里面已有的入站不摘。起来了返回实际在跑的那份配置;不是这类错误、点名的是已有的入站、摘了还是起不来,
// 返回 nil 和最后一次的错误(调用方照旧回滚)。调用方持有 r.mu,并且数据面此刻是停着的(刚才那次 Start 失败了)。
func (r *Runner) startSkippingBusy(raw []byte, err error, prev []byte) ([]byte, error) {
	if r.noBusySkip {
		return nil, err
	}
	had := map[string]bool{}
	if prev != nil {
		var p struct {
			Inbounds []json.RawMessage `json:"inbounds"`
		}
		if json.Unmarshal(prev, &p) == nil {
			for _, ib := range p.Inbounds {
				if t, _ := inboundMeta(ib); t != "" {
					had[t] = true
				}
			}
		}
	}
	cur := raw
	for i := 0; i < maxBusySkip; i++ {
		tag, network := busyInboundOf(err)
		if tag == "" || had[tag] {
			return nil, err
		}
		next, def := withoutInbound(cur, tag)
		if next == nil {
			return nil, err
		}
		r.noteBlocked(def, network)
		cur = next
		if err = r.core.Start(cur); err == nil {
			return cur, nil
		}
	}
	return nil, err
}

// SetBusySkip 开 / 关「摘掉端口被占的新入站」(默认开)。关掉后这类配置照旧整份启动失败、回滚、进冷却。
// 只给别的包里的回归测试用(它们拿占端口来造"起不来的配置")。
func (r *Runner) SetBusySkip(on bool) {
	r.mu.Lock()
	r.noBusySkip = !on
	r.mu.Unlock()
}

// BlockedInbounds 现在因为端口被占没开的入站(按起始时间、tag 排序)。
func (r *Runner) BlockedInbounds() []model.BlockedInbound {
	r.blockedMu.Lock()
	defer r.blockedMu.Unlock()
	if len(r.blocked) == 0 {
		return nil
	}
	out := make([]model.BlockedInbound, 0, len(r.blocked))
	for _, b := range r.blocked {
		out = append(out, b.info)
	}
	sort.Slice(out, func(i, j int) bool {
		if out[i].Since != out[j].Since {
			return out[i].Since < out[j].Since
		}
		return out[i].Tag < out[j].Tag
	})
	return out
}

// addedInbounds next 比 prev 只多了几个入站、别的(抹掉用户表之后)一模一样时,返回多出来的入站定义;否则返回 nil。
// 之前因为端口被占摘掉的入站要加回来时就是这种差别:路由规则本来就在,把入站热加上就行,不用重启数据面。
// 新建线路、新增中转入口不会走到这里 —— 它们还会改路由规则。
func addedInbounds(prev, next []byte) []json.RawMessage {
	var p, n map[string]json.RawMessage
	if json.Unmarshal(prev, &p) != nil || json.Unmarshal(next, &n) != nil {
		return nil
	}
	var pin, nin []json.RawMessage
	if json.Unmarshal(p["inbounds"], &pin) != nil || json.Unmarshal(n["inbounds"], &nin) != nil || len(nin) <= len(pin) {
		return nil
	}
	have := map[string]bool{}
	for _, ib := range pin {
		t, _ := inboundMeta(ib)
		have[t] = true
	}
	var added []json.RawMessage
	rest := make([]json.RawMessage, 0, len(pin))
	for _, ib := range nin {
		if t, _ := inboundMeta(ib); t == "" || have[t] {
			rest = append(rest, ib)
		} else {
			added = append(added, ib)
		}
	}
	if len(added) == 0 || len(rest) != len(pin) {
		return nil
	}
	b, err := json.Marshal(rest)
	if err != nil {
		return nil
	}
	n["inbounds"] = b
	trimmed, err := json.Marshal(n)
	if err != nil {
		return nil
	}
	x, y := stripForCompare(prev, false), stripForCompare(trimmed, false)
	if x == nil || y == nil || string(x) != string(y) {
		return nil
	}
	return added
}

// hotAddInbounds 把 added 这几个入站加到运行中的数据面上。监听不上(端口又被占了)的记回名单、从 raw 里摘掉;
// 返回实际在跑的配置(raw 去掉没加上的)。别的错误原样返回,调用方走整机重载。调用方持有 r.mu。
func (r *Runner) hotAddInbounds(raw []byte, added []json.RawMessage) ([]byte, error) {
	for _, def := range added {
		err := r.core.AddInbound(def)
		if err == nil {
			tag, port := inboundMeta(def)
			logger.Info("入站 ", tag, " 已补上(端口 ", port, " 空出来了),数据面未重启")
			continue
		}
		if !bindBusyRe.MatchString(err.Error()) {
			return nil, err
		}
		network := "tcp"
		if m := listenNetRe.FindStringSubmatch(err.Error()); m != nil {
			network = m[1]
		}
		r.noteBlocked(def, network)
		tag, _ := inboundMeta(def)
		if out, _ := withoutInbound(raw, tag); out != nil {
			raw = out
		}
	}
	return raw, nil
}

var (
	bindBusyRe  = regexp.MustCompile(`address already in use|Only one usage of each socket address`)
	listenNetRe = regexp.MustCompile(`listen (tcp|udp)\b`)
)

// blockedRetryEvery 多久看一次被占的端口空出来没有。
const blockedRetryEvery = time.Minute

// blockedRetryLoop 每分钟:名单里的入站有端口空出来了(那个程序停了),就从名单里拿掉并重载 —— 重载时只多出这几个入站,
// 走热加(addedInbounds),不重启数据面。
func (r *Runner) blockedRetryLoop(stop <-chan struct{}) {
	t := time.NewTicker(blockedRetryEvery)
	defer t.Stop()
	for {
		select {
		case <-stop:
			return
		case <-t.C:
			if r.releaseFreedPorts() {
				if err := r.ReloadAll(); err != nil {
					logger.Warning("端口空出来后补开入站没成功: ", err)
				}
			}
		}
	}
}

// releaseFreedPorts 名单里端口已经能监听的拿掉;有拿掉的返回 true。
func (r *Runner) releaseFreedPorts() bool {
	r.blockedMu.Lock()
	defer r.blockedMu.Unlock()
	freed := false
	for tag, b := range r.blocked {
		if portprobe.Free(b.info.Port, b.info.Net) {
			delete(r.blocked, tag)
			freed = true
		}
	}
	return freed
}
