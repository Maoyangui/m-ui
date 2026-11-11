package ha

import (
	"fmt"
	"strconv"
	"strings"
	"sync"
	"time"

	"github.com/Maoyangui/m-ui/database/model"
	"github.com/Maoyangui/m-ui/hub"
	"github.com/Maoyangui/m-ui/logger"
	"github.com/Maoyangui/m-ui/notify"
)

// 原主机回来后自动变为副机,以及主机侧对各副机回复的全部判定(设计 2.1)。
//
// 主机失联期间备用以更高的纪元接任了。原主机回来后,可达的(认得纪元的)副机过半回 stale_epoch,而且确有证据说明新主机还活着
// (它自己回话说是主机,或者有副机正被它推着)—— 它就变为副机:先留一份备份(失联期间本机可能还收了改动),记下是谁接任的,
// 写 nodeMode=true,换一本新的流量账本,发通知,然后重启。重启后它是 demoted 状态:接受任何更高纪元的主机。
//
// 副机报来的纪元、主机、租约都只是"声称":超出窗口的(EpochClaimOK)记为 bogus,不计入任何判定;
// 声称的新主机不在本机服务器表里的不降级;只有一台副机这么说、别的副机还认本机时不变 —— 被攻破的一台副机不能把主机踢下线。
//
// 判定顺序(每条回复进来时按最近的回复整体判一次;启动自检问完一圈后再判一次):
//
//	0 启动自检还没问完                                  只记下
//	1 设为主机进行中,回 stale 的跟的是目标              交给设为主机的流程(目标自己回话说已是主机 → switchTargetUp)
//	2 过半回 stale 的说主机就是本机(同一实例)          本机库的纪元被回退过:抬回去并以更高纪元继续当主机
//	3 同纪元另一台主机(两台备用同时接任)              比当上主机的先后(再比 id),输了就变为副机
//	4 过半回 stale,且新主机活着、没被反证、认识它       变为副机(新主机是本机的另一个实例 = 换机还原)
//	5 回 stale 的全是孤儿(租约已过期)、它们的主机已不在 以更高纪元收回(每个 主机+纪元 只收回一次)
//	6 回 stale 的跟着本机不认识的主机                    不降级不收回,告警
//	7 其余                                              等下一轮(新主机活着租约会刷新,死了会过期)
//	8 多数副机一直回 still_following、本机凑不够多数     本机是被误接任的:持续超过租约期再加一分钟后让出(先把改过的 DNS 改回去)

type replyKind int

const (
	replyNone replyKind = iota
	replyOK
	replyLegacy // 老副机:不认纪元,不计入
	replyStale
	replyFollowing
	replyNotAdopted
	replyDown
	replyRival // 回话的这台自己就是主机(409 里 role=master)
	replyBogus // 报的纪元超出采信窗口:可能被人动过,不计入任何判定
)

type reply struct {
	at        time.Time
	kind      replyKind
	epoch     uint64
	master    uint
	since     uint64
	leaseAgo  int64 // 回 stale 的副机距上次收到它那台主机的请求多少秒(主机已经不在了的话会一直涨)
	inst      string
	becameAt  int64
	role      string
	nodeEpoch uint64 // 回话那台响应头里带的它自己的纪元(X-HA-Node)
}

// orphanLease 副机多久没收到它那台主机的请求算"孤儿"(那台主机多半不在了);freshLease 以内算正被那台主机推着。
const (
	orphanLease = LeaseExpire
	freshLease  = 30 * time.Second
	deadFor     = 120 * time.Second // 声称的主机本机连不上多久,才算它不在了
	detachAfter = 15 * time.Second  // 同一台持续回 stale 多久才告"已脱离"(刚好碰上切换的那一两条不告)
)

// classify 把一次请求的纪元结果归类(L 是本机纪元)。
func classify(L uint64, r hub.EpochReply) replyKind {
	switch {
	case r.Down:
		return replyDown
	case r.Code != "":
		if !EpochClaimOK(L, r.Epoch) {
			return replyBogus
		}
		switch r.Code {
		case CodeStale:
			if r.Role == "master" {
				return replyRival
			}
			if r.Epoch < L {
				return replyNotAdopted // 本机纪元刚升过:它还没跟上
			}
			return replyStale
		case CodeFollowing:
			if r.Role == "master" {
				return replyRival // 纪元更低、还没下台的另一台主机
			}
			return replyFollowing
		}
		return replyNotAdopted
	case r.Aware:
		return replyOK
	}
	return replyLegacy
}

// NoteReply hub 每次请求副机后调用:记下这台的纪元判定,按 2.1 的判定顺序决定要不要变为副机、收回、让出。
// 本机纪元为 0 时也记(从开启容灾之前的备份还原回来的原主机,副机对它回 stale):这时 200 一律算认本机。
func (s *Service) NoteReply(n model.Node, r hub.EpochReply) {
	if n.Id == 0 || s.IsNode() {
		return
	}
	L := s.Epoch()
	k := classify(L, r)
	if L == 0 && k == replyLegacy && !r.Down {
		k = replyOK // 纪元 0 的请求不带纪元头,副机也不回纪元头:放行就是认本机
	}
	if k != replyOK && k != replyLegacy && !s.nodeKnown(n.Id) {
		return // 已从服务器表删掉的(删除后重试"删副本、离开集群"那几次):它说什么都不进降级、让出、告警的判定
	}
	now := s.now()
	s.mu.Lock()
	s.replies[n.Id] = reply{at: now, kind: k, epoch: r.Epoch, master: r.MasterId, since: r.SinceChange, leaseAgo: r.LeaseAgo,
		inst: r.MasterInst, becameAt: r.BecameAt, role: r.Role, nodeEpoch: r.NodeEpoch}
	s.mu.Unlock()
	c := s.core()
	c.mu.Lock()
	if k == replyDown {
		if _, ok := c.downSince[n.Id]; !ok {
			c.downSince[n.Id] = now
		}
	} else {
		delete(c.downSince, n.Id)
	}
	if k != replyStale && k != replyRival {
		delete(c.staleSince, n.Id)
	} else if _, ok := c.staleSince[n.Id]; !ok {
		c.staleSince[n.Id] = now
	}
	checking := c.checking
	c.mu.Unlock()
	switch k {
	case replyBogus:
		s.alertBogus(n, r.Epoch)
	case replyStale, replyRival:
		if !checking { // 启动自检还没问完:只记下,问完一圈再判(不然第一条 stale 就会发出"已脱离"又马上变为副机)
			s.evaluateDemote(false)
		}
	case replyFollowing:
		if !checking {
			s.evaluateYield()
		}
	}
}

// alertBogus 副机报了超出窗口的纪元:告警一次。
func (s *Service) alertBogus(n model.Node, e uint64) {
	c := s.core()
	c.mu.Lock()
	done := c.bogus[n.Id]
	c.bogus[n.Id] = true
	c.mu.Unlock()
	if done {
		return
	}
	name := s.nodeName(n.Id)
	logger.Warning("容灾:副机 ", name, " 报了异常的纪元 ", e, "(本机纪元 ", s.Epoch(), "),不采信")
	s.notify(fmt.Sprintf("🟠 <b>副机报了异常的纪元</b>:%s 说它在纪元 %d,远超本机的纪元 %d,不采信。那台可能被人动过,请检查。",
		notify.Esc(name), e, s.Epoch()))
}

// claim 一条"X 在纪元 E 当主机"的声称(来自回 stale 的副机,或回话的主机本身)。
type claim struct {
	from  uint // 回话的那台
	r     reply
	rival bool // 回话的就是主机本身
}

// demoteTally 一次判定的计数。
type demoteTally struct {
	total, aware, unknown int
	ok, following         int
	followBest            reply // 回 still_following 的里面纪元最高的那条
	claims                []claim
	rivals                map[uint]reply // 回话说自己是主机的(纪元不低于本机)
	refuted               map[uint]bool  // 回话说明它不是主机的(认本机、还跟着本机的纪元)
}

// tally 统计启用的副机最近的回复(只算 freshReplyLimit 以内的;老副机、报了异常纪元的不计)。
func (s *Service) tally() demoteTally {
	nodes := s.remoteNodes()
	now := s.now()
	L := s.Epoch()
	self := s.SelfId()
	t := demoteTally{rivals: map[uint]reply{}, refuted: map[uint]bool{}}
	s.mu.Lock()
	defer s.mu.Unlock()
	for _, n := range nodes {
		r, ok := s.replies[n.Id]
		if !ok || now.Sub(r.at) > freshReplyLimit {
			t.total++
			t.unknown++
			continue
		}
		switch r.kind {
		case replyLegacy, replyBogus:
			continue // 不认纪元的老副机、报了异常纪元的,既不算赞成也不算反对
		case replyDown:
			t.total++
			continue
		}
		t.total++
		t.aware++
		switch r.kind {
		case replyOK:
			t.ok++
			t.refuted[n.Id] = true // 它认本机:不是主机
		case replyNotAdopted:
			t.refuted[n.Id] = true // 它还在更低的纪元当副机:不是主机
		case replyFollowing:
			t.refuted[n.Id] = true
			if r.master == self && self != 0 {
				continue // 它跟的就是本机(更早的纪元,还没跟上来):不是跟着别的主机
			}
			t.following++
			if r.epoch > t.followBest.epoch {
				t.followBest = r
			}
		case replyStale:
			t.claims = append(t.claims, claim{from: n.Id, r: r})
		case replyRival:
			if r.epoch >= L {
				r.master = n.Id // 主机回 409 时报的主机就是它自己
				t.rivals[n.Id] = r
				t.claims = append(t.claims, claim{from: n.Id, r: r, rival: true})
			}
		}
	}
	return t
}

// majority 本机加上认本机的副机过不过半(按启用的服务器总数算,含本机)。
func (t demoteTally) majority(clusterSize int) bool { return 2*(t.ok+1) > clusterSize }

// enough n 台(声称同一件事的)够不够多数:占全部(认得纪元的)副机过半直接算;否则要等每台都回过话,再看是不是占可达的过半。
// 只有一台副机时它一台说了就算。
func (t demoteTally) enough(n int) bool {
	if n == 0 {
		return false
	}
	if 2*n > t.total {
		return true
	}
	if t.unknown > 0 {
		return false
	}
	return 2*n > t.aware
}

// beats 声称 (X, e) 压不压得过本机(纪元 L):纪元更高;或同纪元、X 自己回过话、它比本机先当上主机(同时再比 id 小)。
func (s *Service) beats(c claim, L uint64, t demoteTally, mine int64, self uint) bool {
	if c.r.epoch > L {
		return true
	}
	if c.r.epoch < L {
		return false
	}
	x := c.r.master
	rv, ok := t.rivals[x]
	if !ok || rv.epoch != L || !s.sameEpochRivalPlausible(L) {
		return false
	}
	return rv.becameAt < mine || (rv.becameAt == mine && x < self)
}

// sameEpochRivalPlausible 同一纪元会不会真有另一台主机:只有本机是在这个纪元接任上来的才可能(两台备用同时接任);
// 本机是开启容灾、设为主机、收回、围栏得到这个纪元的,别人不可能也在这个纪元当主机,那样说的就是假话。
func (s *Service) sameEpochRivalPlausible(L uint64) bool {
	var b Became
	return s.getJSON(KeyBecame, &b) && b.Epoch == L && b.How == HowTakeover
}

// nodeKnown 本机服务器表里有没有这台。
func (s *Service) nodeKnown(id uint) bool {
	if id == 0 {
		return false
	}
	var n int64
	s.d.DB.Model(&model.Node{}).Where("id = ?", id).Count(&n)
	return n > 0
}

// evaluateDemote 按最近的回复判定要不要变为副机、收回(见文件开头的判定顺序)。startup 为真表示启动自检:每台都已经问过一遍,
// 没回的就当不可达。返回是否变为副机了。
func (s *Service) evaluateDemote(startup bool) bool {
	if s.IsNode() {
		return false
	}
	s.mu.Lock()
	demoting := s.demoting
	s.mu.Unlock()
	if demoting {
		return false
	}
	// 同一时刻只判一次:判定里会推状态(收回、抬纪元),推出去的回复又会进来要求判定 —— 那些只记下,不重入
	c := s.core()
	if !c.evalMu.TryLock() {
		return false
	}
	defer c.evalMu.Unlock()
	t := s.tally()
	if startup {
		t.unknown = 0
	}
	if len(t.claims) == 0 {
		return false
	}
	L := s.Epoch()
	self := s.SelfId()
	inst := s.Instance()
	now := s.now()
	mine := s.becameAt()

	// 1. 设为主机进行中:跟着目标的回复交给设为主机的流程收尾,本机不按"失联被接任"降级
	if sw := s.switchState(); sw.Target != 0 && (sw.Active() || s.Switching()) {
		toTarget := false
		for _, c := range t.claims {
			if c.r.master == sw.Target {
				toTarget = true
			}
		}
		if toTarget {
			if rv, ok := t.rivals[sw.Target]; ok && rv.epoch > L {
				s.switchTargetUp(sw.Target, rv.epoch)
			}
			return false
		}
	}

	// 2. 过半回 stale 的说主机就是本机、实例号也是本机的:本机库的纪元被回退过
	var selfTop uint64
	selfN := 0
	for _, c := range t.claims {
		if !c.rival && c.r.master == self && self != 0 && c.r.inst != "" && c.r.inst == inst && c.r.epoch > L {
			selfN++
			if c.r.epoch > selfTop {
				selfTop = c.r.epoch
			}
		}
	}
	if selfN > 0 && t.enough(selfN) {
		s.healRollback(selfTop)
		return false
	}

	// 3. 同纪元的另一台主机(它自己回的话):比先后,输了就让给它
	for x, rv := range t.rivals {
		if rv.epoch == L && s.nodeKnown(x) && s.beats(claim{from: x, r: reply{epoch: L, master: x}}, L, t, mine, self) {
			var b Became
			s.getJSON(KeyBecame, &b)
			d := Demoted{Epoch: L, MasterId: x, OldEpoch: L, At: now.Unix(), Reason: "sameEpoch", SinceChange: b.MaxChangeId}
			if err := s.Demote(d); err != nil {
				logger.Warning("容灾:变为副机失败: ", err)
			}
			return true
		}
	}

	// 压得过本机的声称
	var beat []claim
	var best claim
	for _, c := range t.claims {
		if c.r.master == self && self != 0 && (c.r.inst == "" || c.r.inst == inst) {
			// 说主机就是本机的:同一实例是第 2 条(库被回退);不知道实例的(让出主机后还记着本机的)等成了孤儿按第 5 条收回
			continue
		}
		if s.beats(c, L, t, mine, self) {
			beat = append(beat, c)
			if c.r.epoch > best.r.epoch {
				best = c
			}
		}
	}

	// 4. 过半压得过本机,新主机活着、没被反证、本机认识它:变为副机
	if len(beat) > 0 && t.enough(len(beat)) {
		x := best.r.master
		foreign := x == self && best.r.inst != "" && best.r.inst != inst
		known := foreign || (x != self && s.nodeKnown(x))
		alive := false
		if rv, ok := t.rivals[x]; ok && rv.epoch >= best.r.epoch {
			alive = true
		}
		for _, c := range beat {
			if !c.rival && c.r.master == x && c.r.leaseAgo < int64(freshLease/time.Second) {
				alive = true
			}
		}
		if known && alive && !t.refuted[x] {
			reason, since := "takenOver", best.r.since
			if foreign {
				reason, since = "restoredElsewhere", 0
			} else if best.r.epoch == L {
				reason = "sameEpoch"
			}
			d := Demoted{Epoch: best.r.epoch, MasterId: x, SinceChange: since, OldEpoch: L, At: now.Unix(), Reason: reason}
			if err := s.Demote(d); err != nil {
				logger.Warning("容灾:变为副机失败: ", err)
			}
			return true
		}
	}

	// 5. 回 stale 的全是孤儿,它们跟的主机都已经不在:以更高纪元收回
	if ok, top, x := s.orphansReclaimable(t, L, self, inst, now); ok {
		s.reclaimOrphans(x, top)
		return false
	}

	// 6 / 告警:跟着本机不认识的主机的、持续回 stale 的
	s.alertDetached(t, self, inst, now)
	return false
}

// orphansReclaimable 第 5 条:回 stale 的(非主机本身)全是孤儿,它们声称的主机都认识、都已经不在了。返回要收回到的最高纪元与对应的主机。
func (s *Service) orphansReclaimable(t demoteTally, L uint64, self uint, inst string, now time.Time) (bool, uint64, uint) {
	var top uint64
	var topX uint
	n := 0
	c := s.core()
	for _, cl := range t.claims {
		if cl.rival {
			if cl.r.epoch >= L {
				return false, 0, 0 // 有主机自己回话说它还在
			}
			continue
		}
		if cl.r.leaseAgo < int64(orphanLease/time.Second) {
			return false, 0, 0
		}
		x := cl.r.master
		switch {
		case x == self && self != 0:
			if cl.r.inst != "" && cl.r.inst == inst {
				return false, 0, 0 // 第 2 条的事
			}
			// 本机的另一个实例(换机还原后又停了)、或让出主机后还记着本机的:它们的主机就是不在了
		case !s.nodeKnown(x):
			return false, 0, 0 // 第 6 条:不认识的主机
		case t.refuted[x]:
			// 它自己回话说明已经不是主机了
		default:
			c.mu.Lock()
			since, down := c.downSince[x]
			c.mu.Unlock()
			if !down || now.Sub(since) < deadFor {
				return false, 0, 0
			}
		}
		n++
		if cl.r.epoch > top {
			top, topX = cl.r.epoch, x
		}
	}
	return n > 0, top, topX
}

// alertDetached 回 stale 却不够降级 / 收回的:跟着本机不认识的主机的告警一次(第 6 条);持续回 stale 的标成"已脱离"告警一次。
func (s *Service) alertDetached(t demoteTally, self uint, inst string, now time.Time) {
	c := s.core()
	for _, cl := range t.claims {
		if cl.rival {
			continue
		}
		id, x := cl.from, cl.r.master
		if x == self {
			continue // 说主机是本机的:第 2 条 / 第 5 条的事,不算脱离
		}
		if !s.nodeKnown(x) {
			key := fmt.Sprintf("%d:%d:%d", id, x, cl.r.epoch)
			c.mu.Lock()
			done := c.unknownAlert[key]
			c.unknownAlert[key] = true
			c.mu.Unlock()
			if !done {
				name := s.nodeName(id)
				logger.Warning("容灾:副机 ", name, " 跟着本机不认识的主机 #", x, "(纪元 ", cl.r.epoch, "),不变")
				s.notify(fmt.Sprintf("🟠 <b>副机跟着本机不认识的主机</b>:%s 说它的主机是服务器 #%d(纪元 %d),本机的服务器列表里没有这台。"+
					"本机继续当主机。确认那台不该当主机后,在本机执行 m-ui ha takeover --force 把副机收回来。", notify.Esc(name), x, cl.r.epoch))
			}
			continue
		}
		c.mu.Lock()
		first, ok := c.staleSince[id]
		c.mu.Unlock()
		if !ok || now.Sub(first) < detachAfter {
			continue
		}
		s.mu.Lock()
		done := s.detached[id]
		s.detached[id] = true
		s.mu.Unlock()
		if !done {
			name := s.nodeName(id)
			logger.Warning("容灾:副机 ", name, " 已跟随别的主机,但不够让本机变为副机,本机继续当主机")
			s.notify("🟠 <b>副机已脱离</b>:" + notify.Esc(name) + " 跟随了纪元更高的另一台主机,而多数副机仍认本机。\n" +
				"本机继续当主机。那台(和跟着它的副机)在它那台主机不在之后会自动回到本机;两边一直僵持时,在不要的那台主机的设置里打开「副服务器模式」。")
		}
	}
}

// healRollback 第 2 条:副机认的"本机"纪元比库里的高(库被回退过而启动时没发现):抬回去,再以更高纪元继续当主机
// (签名密钥若也被还原成旧的,副机在租约过期后跟上)。
func (s *Service) healRollback(top uint64) {
	L := s.Epoch()
	if top <= L {
		return
	}
	self := s.SelfId()
	if err := s.setEpoch(top, self, nil); err != nil {
		logger.Warning("容灾:把纪元抬回 ", top, " 失败: ", err)
		return
	}
	next, err := NextEpoch(top)
	if err == nil {
		_, err = s.fenceTo(next, 0, s.remoteNodes())
	}
	if err != nil {
		logger.Warning("容灾:以更高纪元继续当主机失败: ", err)
		next = top
	}
	logger.Warning("容灾:副机认的本机纪元 ", top, " 比库里的 ", L, " 高(库被回退过),已抬回,以纪元 ", next, " 继续当主机")
	s.audit("ha", "epoch-healed", map[string]interface{}{"from": L, "seen": top, "epoch": next})
	s.addNotice(Notice{Kind: "fenced", Reason: "rolledBack", N: int64(next)})
	s.notify(fmt.Sprintf("🟠 <b>本机的纪元已恢复</b>:库里的纪元比副机认的低(还原了旧备份或重置过设置),已抬回并以纪元 %d 继续当主机,副机照常认本机。", next))
}

// reclaimOrphans 第 5 条:以高于 top 的纪元收回孤儿(每个 主机+纪元 只收回一次:收不回来的不会每轮再升一次纪元)。
func (s *Service) reclaimOrphans(x uint, top uint64) {
	key := fmt.Sprintf("%d:%d", x, top)
	c := s.core()
	c.mu.Lock()
	done := c.reclaimed[key]
	c.reclaimed[key] = true
	c.mu.Unlock()
	if done {
		return
	}
	L := s.Epoch()
	next, err := NextEpoch(L, top)
	if err != nil {
		logger.Warning("容灾:收回脱离的副机失败: ", err)
		return
	}
	granted, err := s.fenceTo(next, 0, s.remoteNodes())
	if err != nil {
		logger.Warning("容灾:收回脱离的副机失败: ", err)
		return
	}
	s.afterReclaim(L, next, granted, false)
}

// afterReclaim 收回之后:审计、提示、通知(要是开了自动改 DNS,提醒核对域名还指着本机)。
func (s *Service) afterReclaim(from, next uint64, granted int, force bool) {
	s.audit("ha", "reclaim", map[string]interface{}{"epoch": next, "from": from, "granted": granted, "force": force})
	logger.Warning("容灾:以纪元 ", next, " 收回跟过别的主机的副机(原纪元 ", from, ",已给 ", granted, " 台发交接标记)")
	s.addNotice(Notice{Kind: "reclaimed", N: int64(next)})
	msg := fmt.Sprintf("🟠 <b>已收回脱离的副机</b>:有副机跟过一台纪元更高、现在已经不在的主机。本机以纪元 %d 继续当主机,它们会自动回来。", next)
	if force {
		msg = fmt.Sprintf("🟠 <b>已以纪元 %d 继续当主机</b>(命令行 takeover --force):认本机的副机已收到交接标记,跟过别的主机的副机在它们的租约到期后回来。", next)
	}
	if names := splitNames(s.setting(KeyDnsNames)); len(names) > 0 && strings.TrimSpace(s.setting(KeyDnsToken)) != "" {
		msg += "\n开着自动改 DNS:请核对 " + notify.Esc(strings.Join(names, ", ")) + " 仍指向本机(另一台主机接任时可能改过)。"
	}
	s.notify(msg)
}

// ReclaimNow 命令行 m-ui ha takeover --force(在主机上):以严格高于已知全部纪元的纪元继续当主机,
// 给全部启用的副机推带自交接标记的签名状态。已知 = 各副机最近回复里采信的纪元 + 各副机报告里的纪元。
func (s *Service) ReclaimNow() (uint64, error) {
	if s.IsNode() {
		return 0, fmt.Errorf("本机不是主机")
	}
	L := s.Epoch()
	var seen []uint64
	s.mu.Lock()
	for _, r := range s.replies {
		switch r.kind {
		case replyStale, replyFollowing, replyRival, replyNotAdopted:
			seen = append(seen, r.epoch)
		}
	}
	s.mu.Unlock()
	nodes := s.remoteNodes()
	if s.d.Hub != nil {
		for _, n := range nodes {
			if st, ok := s.d.Hub.StatusOf(n.Id); ok && st.HA != nil {
				seen = append(seen, st.HA.Epoch)
			}
		}
	}
	next, err := NextEpoch(L, seen...)
	if err != nil {
		return 0, err
	}
	granted, err := s.fenceTo(next, 0, nodes)
	if err != nil {
		return 0, err
	}
	s.afterReclaim(L, next, granted, true)
	return next, nil
}

// reclaim 旧的入口(命令行):force 为真时同 ReclaimNow。
func (s *Service) reclaim(seen uint64, force bool) (uint64, error) {
	if force {
		return s.ReclaimNow()
	}
	s.reclaimOrphans(0, seen)
	return s.Epoch(), nil
}

// Demoted 原主机变为副机的记录:是谁接任的、本机原来的纪元、失联期间本机还收了哪些改动。
type Demoted struct {
	Epoch       uint64      `json:"epoch"`    // 接任方的纪元
	MasterId    uint        `json:"masterId"` // 接任方的服务器 id
	MasterName  string      `json:"masterName,omitempty"`
	OldEpoch    uint64      `json:"oldEpoch"`
	SinceChange uint64      `json:"sinceChange"` // 接任所用数据里最大的审计 id:之后的改动没进新主机(0 = 不知道)
	Divergent   int64       `json:"divergent"`   // 失联期间本机收的改动条数(-1 = 不知道)
	Backup      string      `json:"backup,omitempty"`
	At          int64       `json:"at"`
	Yield       bool        `json:"yield,omitempty"`  // 不是原主机回来,而是本机在原主机还在时被误接任、又让了出来
	Reason      string      `json:"reason,omitempty"` // takenOver | yielded | sameEpoch | restoredElsewhere
	Dns         []DnsChange `json:"dns,omitempty"`    // 让出时改回去的 DNS 记录
}

// beginDemote 进入"正在变为副机":之后不再做任何降级、收回、让出的判定。已经在变了返回 false。
// 设为主机完成时(becomeNode)也先调它,免得同一时刻副机的 stale 回复再触发一次"失联被接任"。
func (s *Service) beginDemote() bool {
	s.mu.Lock()
	defer s.mu.Unlock()
	if s.demoting {
		return false
	}
	s.demoting = true
	return true
}

// Demote 本机变为副机。只做一次;先写角色、再做备份,做完安排重启。
func (s *Service) Demote(d Demoted) error {
	if !s.beginDemote() {
		return nil
	}
	if d.Reason == "" {
		d.Reason = "takenOver"
		if d.Yield {
			d.Reason = "yielded"
		}
	}
	d.MasterName = s.nodeName(d.MasterId)
	d.Divergent = -1
	if d.SinceChange > 0 {
		s.d.DB.Model(&model.Change{}).Where("id > ?", d.SinceChange).Count(&d.Divergent)
	}
	if s.d.Backup != nil {
		// 先留一份主机身份的备份(失联期间本机收的改动在里面);已经标了"正在变为副机",备份期间再来的回复不会再触发一次
		if name, err := s.d.Backup(fmt.Sprintf("former-master-E%d-", d.OldEpoch)); err != nil {
			logger.Warning("容灾:变为副机前的备份失败(继续): ", err)
		} else {
			d.Backup = name
		}
	}
	values := map[string]string{
		"nodeMode": "true", KeyRole: RoleDemoted, KeyDemoted: mustJSON(d),
		hub.LedgerEpochKey: hub.NewLedgerEpoch(), // 本机的流量账本换一本:作为副机加入后,新主机只建基线
		KeySignKey:         "", KeyFreezeUntil: "", KeyMasterId: strconv.FormatUint(uint64(d.MasterId), 10),
	}
	if err := s.set(values); err != nil {
		s.mu.Lock()
		s.demoting = false
		s.mu.Unlock()
		return err
	}
	logger.Warning("容灾:主机是「", d.MasterName, "」(纪元 ", d.Epoch, "),本机变为副机(", d.Reason, ");备份 ", d.Backup,
		",本机还有 ", d.Divergent, " 条改动没进那台主机(-1 = 不知道)")
	s.notify(demoteText(d))
	action := "demoted"
	if d.Yield {
		action = "yielded"
	}
	s.audit("ha", action, map[string]interface{}{"master": d.MasterName, "epoch": d.Epoch, "divergent": d.Divergent, "backup": d.Backup, "reason": d.Reason})
	s.addNotice(Notice{Kind: "demoted", Reason: d.Reason, Yield: d.Yield, Name: d.MasterName, N: d.Divergent, File: d.Backup, Changes: d.Dns, At: d.At})
	s.restart(3 * time.Second)
	return nil
}

// demoteText 变为副机的 Telegram 通知。
func demoteText(d Demoted) string {
	name := notify.Esc(d.MasterName)
	var msg string
	switch d.Reason {
	case "yielded":
		msg = fmt.Sprintf("🟠 <b>本机已让出主机</b>\n多数副机一直还跟着原来的主机「%s」:它其实还在。本机变回副机,继续用自己的线路服务,那台主机会把本机收回去。", name)
	case "sameEpoch":
		msg = fmt.Sprintf("🟠 <b>本机已变为副机</b>\n「%s」和本机在同一纪元都接任了主机(两台备用同时接任),它先当上,本机让给它,继续用自己的线路服务。", name)
	case "restoredElsewhere":
		msg = "🟠 <b>本机已变为副机</b>\n同一份数据已在另一台机器上当主机:本机多半是换机前的旧主机。本机继续用自己的线路服务;不要它了就在本机执行 systemctl disable --now m-ui。"
	default:
		msg = fmt.Sprintf("🟠 <b>本机已变为副机</b>\n本机失联期间「%s」已接任主机。本机继续用自己的线路服务,不再推送配置、不再跑主机任务,新主机会把本机当副机管起来。", name)
	}
	for _, c := range d.Dns {
		msg += fmt.Sprintf("\nDNS 已改回:%s %s %s → %s", notify.Esc(c.Name), c.Type, notify.Esc(c.To), notify.Esc(c.From))
	}
	switch {
	case d.Divergent > 0:
		msg += fmt.Sprintf("\n本机还有 %d 条改动没进那台主机,已留备份:%s", d.Divergent, notify.Esc(d.Backup))
	case d.Divergent < 0 && d.Backup != "":
		msg += "\n本机的数据已留备份:" + notify.Esc(d.Backup) + "(不确定有没有改动没进那台主机)"
	}
	msg += "\n想让本机重新当主机:在新主机的服务器列表里对本机「设为主机」。"
	return msg
}

// StartupCheck 启动自检:先执行启动时定下的围栏;在跑主机任务(推送、并账、判超量……)之前,把启用的副机都问一遍,
// 按判定顺序决定要不要直接变为副机(不先跑一轮主机任务)。本机纪元为 0 也问(从开启容灾之前的备份还原回来的原主机)。返回是否变了。
func (s *Service) StartupCheck() bool {
	if s.IsNode() || s.d.Hub == nil {
		return false
	}
	s.runFence(s.takeFence())
	nodes := s.remoteNodes()
	if len(nodes) == 0 {
		return false
	}
	timeout := 10 * time.Second
	if s.Epoch() == 0 {
		timeout = 5 * time.Second
	}
	c := s.core()
	c.mu.Lock()
	c.checking = true
	c.mu.Unlock()
	var wg sync.WaitGroup
	for _, n := range nodes {
		wg.Add(1)
		go func(n model.Node) {
			defer wg.Done()
			_ = s.d.Hub.Send(n, "GET", "ping", nil, 0, nil, timeout, nil) // 结果经 NoteReply 记下
		}(n)
	}
	wg.Wait()
	c.mu.Lock()
	c.checking = false
	c.mu.Unlock()
	t := s.tally()
	if len(t.claims) > 0 || s.Epoch() > 0 {
		logger.Info("容灾启动自检:副机 ", t.total, " 台,认得纪元的 ", t.aware, " 台,其中 ", len(t.claims), " 台回了更高纪元")
	}
	return s.evaluateDemote(true)
}

// replyView 给面板看的各副机回复。
type replyView struct {
	Kind   string `json:"kind"`
	At     int64  `json:"at"`
	Epoch  uint64 `json:"epoch,omitempty"`
	Master uint   `json:"master,omitempty"`
}

func (s *Service) replyViews() map[string]replyView {
	names := map[replyKind]string{replyOK: "ok", replyLegacy: "legacy", replyStale: "stale", replyFollowing: "following",
		replyNotAdopted: "notAdopted", replyDown: "down", replyRival: "rival", replyBogus: "bogus"}
	s.mu.Lock()
	defer s.mu.Unlock()
	out := map[string]replyView{}
	for id, r := range s.replies {
		out[strconv.FormatUint(uint64(id), 10)] = replyView{Kind: names[r.kind], At: r.at.Unix(), Epoch: r.epoch, Master: r.master}
	}
	return out
}

// evaluateYield 第 8 条:本机是主机,但多数副机一直回 still_following(它们还跟着另一台纪元更低、仍然活着的主机),
// 本机自己又凑不够多数:说明是在主机其实还在的时候被误接任了。持续超过租约期再加一分钟,本机让出主机、变为副机;
// 本任期自动改过 DNS 的先改回去。那台主机随后以更高的纪元把本机和跟着本机的副机收回去(见 reclaimOrphans)。
func (s *Service) evaluateYield() {
	s.mu.Lock()
	demoting := s.demoting
	s.mu.Unlock()
	if demoting || s.IsNode() {
		return
	}
	t := s.tally()
	now := s.now()
	s.mu.Lock()
	if 2*t.following <= t.aware || t.majority(s.clusterSize()) {
		s.yieldSince = time.Time{}
		s.mu.Unlock()
		return
	}
	if s.yieldSince.IsZero() {
		s.yieldSince = now
	}
	since := s.yieldSince
	s.mu.Unlock()
	if now.Sub(since) < LeaseExpire+time.Minute {
		return
	}
	L := s.Epoch()
	d := Demoted{Epoch: t.followBest.epoch, MasterId: t.followBest.master, OldEpoch: L, At: now.Unix(), Yield: true, Reason: "yielded"}
	var b Became
	if s.getJSON(KeyBecame, &b) && b.Epoch == L {
		d.SinceChange = b.MaxChangeId
		if len(b.Dns) > 0 {
			changes, err := s.revertDNS(b)
			if err != nil {
				logger.Warning("容灾:让出时把 DNS 改回去失败: ", err)
			}
			d.Dns = changes
		}
	}
	if err := s.Demote(d); err != nil {
		logger.Warning("容灾:让出主机失败: ", err)
	}
}

// setGrantOut 这一轮随容灾状态推给副机的交接标记(收回、围栏时用;设为主机的交接标记在 switch.go)。
func (s *Service) setGrantOut(g *Grant) {
	s.mu.Lock()
	s.grantOut = g
	s.mu.Unlock()
}

func (s *Service) pendingGrant() *Grant {
	s.mu.Lock()
	defer s.mu.Unlock()
	return s.grantOut
}
