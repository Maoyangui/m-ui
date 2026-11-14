package ha

import (
	"errors"
	"fmt"
	"sort"
	"time"

	"github.com/Maoyangui/m-ui/logger"
	"github.com/Maoyangui/m-ui/notify"
)

// 两台同时当主机(演练里真机遇到的网络分区):备用接任后联系不上别的副机(比如副机的防火墙只放行原主机的 IP),
// 原主机回来以后多数副机还跟着它 —— 原主机不会变为副机(多数副机没跟新主机),新主机也不会让位(联系不上副机,
// 拿不到「它们还跟着原主机」的证据)。两台就一直都当着主机:订阅解析到新主机,副机的流量和管理改动进原主机,数据分叉。
// 这里让两边都看得见、收得到告警,并给纪元更高的那台一个「让回主机」:它变为副机、本任期自动改过的 DNS 改回去,
// 原主机随后以更高的纪元把它收回去(见 reclaimOrphans)。

// rivalAfter 另一台持续自称主机多久才提示、告警(正常换主机时原主机回来到变为副机之间会短暂出现)。
const rivalAfter = LeaseExpire + time.Minute

// Rival 另一台也在当主机的服务器。
type Rival struct {
	Id        uint   `json:"id"`
	Name      string `json:"name"`
	Epoch     uint64 `json:"epoch"`
	Since     int64  `json:"since"`     // 从什么时候起一直自称主机
	Higher    bool   `json:"higher"`    // 它的纪元比本机高(本机是原主机那一边)
	Followers int    `json:"followers"` // 认本机的副机台数(不算它)
	Others    int    `json:"others"`    // 除了它以外启用的副机台数
	CanYield  bool   `json:"canYield"`  // 本机可以把主机让回给它(本机纪元更高)
}

// noteRival NoteReply 收到「回话的这台自己就是主机」时调用:记下从什么时候起,持续超过 rivalAfter 告警一次(每台、每个纪元一次)。
func (s *Service) noteRival(id uint, epoch uint64, now time.Time) {
	c := s.core()
	c.mu.Lock()
	since, ok := c.rivalSince[id]
	if !ok {
		c.rivalSince[id] = now
		since = now
	}
	key := fmt.Sprintf("%d:%d", id, epoch)
	alert := now.Sub(since) >= rivalAfter && !c.rivalAlerted[key]
	if alert {
		c.rivalAlerted[key] = true
	}
	c.mu.Unlock()
	if !alert {
		return
	}
	name, L := s.nodeName(id), s.Epoch()
	logger.Warning("容灾:", name, " 也在当主机(纪元 ", epoch, ",本机纪元 ", L, "),两台同时当主机,订阅和改动会分叉")
	var how string
	if epoch < L {
		how = "多数副机还跟着它,本机联系不上它们。要它继续当主机:在本机面板点「让回主机」(或执行 m-ui ha yield);要本机当主机:让本机联系得上其它副机(放开它们的防火墙),它会自动变为副机。"
	} else {
		how = "它联系不上多数副机,多数副机还跟着本机。要本机继续当主机:到它的面板点「让回主机」(或在它上面执行 m-ui ha yield);要它当主机:让它联系得上其它副机,本机会自动变为副机。"
	}
	s.notify(fmt.Sprintf("🔴 <b>两台同时当主机</b>:「%s」(纪元 %d)和本机(纪元 %d)都在当主机,订阅和改动会分叉。\n%s", notify.Esc(name), epoch, L, how))
}

// forgetRival 这台不再自称主机了。
func (s *Service) forgetRival(id uint) {
	c := s.core()
	c.mu.Lock()
	delete(c.rivalSince, id)
	c.mu.Unlock()
}

// Rivals 持续超过 rivalAfter 还在自称主机的服务器(本机是副机时没有)。
func (s *Service) Rivals() []Rival {
	if s.IsNode() {
		return nil
	}
	now, L := s.now(), s.Epoch()
	t := s.tally()
	c := s.core()
	c.mu.Lock() // 两把锁分开拿(不嵌套):先抄下各台从什么时候起自称主机,再看最近的回复
	since := make(map[uint]time.Time, len(c.rivalSince))
	for id, at := range c.rivalSince {
		since[id] = at
	}
	c.mu.Unlock()
	var out []Rival
	s.mu.Lock()
	for id, r := range s.replies {
		at, ok := since[id]
		if r.kind != replyRival || now.Sub(r.at) > freshReplyLimit || !ok || now.Sub(at) < rivalAfter {
			continue
		}
		out = append(out, Rival{Id: id, Epoch: r.epoch, Since: at.Unix(), Higher: r.epoch > L, CanYield: r.epoch < L})
	}
	s.mu.Unlock()
	remote := map[uint]bool{}
	for _, n := range s.remoteNodes() {
		remote[n.Id] = true
	}
	for i := range out {
		out[i].Name = s.nodeName(out[i].Id)
		out[i].Followers = t.ok
		out[i].Others = t.total
		if remote[out[i].Id] { // 它自己也在本机的副机名单里(原主机回来后接任方把它那一行停用了,就不在)
			out[i].Others--
		}
	}
	sort.Slice(out, func(i, j int) bool { return out[i].Id < out[j].Id })
	return out
}

// YieldTo 管理员手动把主机让回给另一台还在当主机、纪元更低的服务器:本机变为副机,本任期自动改过的 DNS 改回去,
// 那台随后以更高的纪元把本机收回去。只认最近还在回话说自己是主机的那台(id 为 0 时:正好只有一台就让给它)。
func (s *Service) YieldTo(id uint, by string) error {
	if s.IsNode() {
		return errors.New("本机已经是副机")
	}
	rs := s.Rivals()
	if id == 0 && len(rs) == 1 {
		id = rs[0].Id
	}
	var rv *Rival
	for i := range rs {
		if rs[i].Id == id {
			rv = &rs[i]
		}
	}
	if rv == nil {
		if id == 0 && len(rs) > 1 {
			return errors.New("有不止一台在当主机:指明让给哪一台")
		}
		return errors.New("没有别的服务器一直在当主机:不用让")
	}
	if !rv.CanYield {
		return errors.New("那台的纪元比本机高:该让的是它,到它的面板上点「让回主机」")
	}
	L := s.Epoch()
	d := Demoted{Epoch: rv.Epoch, MasterId: rv.Id, MasterName: rv.Name, OldEpoch: L, At: s.now().Unix(), Yield: true, Reason: "yielded"}
	var b Became
	if s.getJSON(KeyBecame, &b) && b.Epoch == L {
		d.SinceChange = b.MaxChangeId
	}
	s.audit("ha", "yield-manual", map[string]interface{}{"to": rv.Name, "epoch": rv.Epoch, "from": L, "by": by})
	return s.Demote(d)
}
