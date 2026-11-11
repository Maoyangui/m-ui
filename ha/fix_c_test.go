package ha

// 修复者 C(副本与数据、容灾设置、命令行与状态视图)的测试。

import (
	"bytes"
	"context"
	"crypto/rand"
	"encoding/base64"
	"encoding/json"
	"io"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"strconv"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/Maoyangui/m-ui/database/model"
)

func init() {
	// 收副本后的后台自检和测试结束时删临时目录抢文件:包里的测试默认关掉,要测它的自己打开
	selfCheckAsync = false
}

func notesWith(cl *haCluster, sub string) int {
	n := 0
	for _, x := range *cl.notes {
		if strings.Contains(x, sub) {
			n++
		}
	}
	return n
}

func infoOf(t *testing.T, infos []StandbyInfo, id uint) StandbyInfo {
	t.Helper()
	for _, c := range infos {
		if c.Id == id {
			return c
		}
	}
	t.Fatalf("没有 #%d 的备用信息", id)
	return StandbyInfo{}
}

// #24 #47:先升了主机,版本比主机旧(仍认得容灾)的备用保留名次与手里的旧副本,不发新的,告警一次;升级后恢复。
func TestStandbyVersionLagKeepsReplica(t *testing.T) {
	cl := newCluster(t, 2)
	m := cl.m
	cl.h.RunOnce()
	if err := m.ReplicateNow(); err != nil {
		t.Fatal(err)
	}
	r1, _ := cl.cands[0].replicaInfo()
	before := cl.agents[0].count("ha/replica")
	m.d.Version = "0.7.18" // 主机先升级
	cl.h.RunOnce()
	if err := m.ReplicateNow(); err == nil {
		t.Fatal("版本落后的备用不该再收新副本")
	}
	infos, ids := m.rep.compute(m.Config())
	if len(ids) != 2 || ids[0] != cl.ids[0] || ids[1] != cl.ids[1] {
		t.Fatalf("版本落后的备用要保留名次: %v", ids)
	}
	if c := infoOf(t, infos, cl.ids[0]); !c.Held || len(c.Reasons) != 1 || c.Reasons[0] != reasonVerKeep {
		t.Fatalf("要标成保留(versionKeep): %+v", c)
	}
	cl.h.RunOnce()
	m.rep.tick()
	cl.h.RunOnce()
	if ri, ok := cl.cands[0].replicaInfo(); !ok || ri.Seq != r1.Seq {
		t.Fatal("版本落后的备用手里的旧副本不能删(照样能拿来接任)")
	}
	if cl.agents[0].count("ha/replica") != before {
		t.Fatal("版本落后的备用不发新副本")
	}
	if cl.agents[0].count("ha/drop") != 0 {
		t.Fatal("不能催保留名次的备用删副本")
	}
	m.rep.tick()
	if n := notesWith(cl, "合格的备用不够"); n != 1 {
		t.Fatalf("合格备用不够要告警一次,实际 %d", n)
	}
	ready := m.View(true).Ready
	found := false
	for _, ch := range ready {
		if ch.Code == "versionsKeep" && len(ch.Names) == 2 {
			found = true
		}
	}
	if !found {
		t.Fatalf("就绪检查要列出保留名次的备用: %+v", ready)
	}
	// 副机升级后恢复发副本
	for _, c := range cl.cands {
		c.d.Version = "0.7.18"
	}
	cl.h.RunOnce()
	m.d.DB.Model(&model.User{}).Where("name = ?", "alice").Update("up", 4242) // 内容变了才会重发
	if err := m.ReplicateNow(); err != nil {
		t.Fatalf("升级后恢复: %v", err)
	}
	if cl.agents[0].count("ha/replica") == before {
		t.Fatal("升级后要发新副本")
	}
}

// #26:改动触发的补发也按耗时放宽(max(60 秒, 4×上次耗时));定时任务、容灾自己的审计不触发补发。
func TestReplicaAuditTriggerAdaptive(t *testing.T) {
	cl := newCluster(t, 1)
	m := cl.m
	r := m.rep
	g := replicaMinGap
	replicaMinGap = time.Minute
	t.Cleanup(func() { replicaMinGap = g })
	cfg := m.Config()
	var maxId uint64
	m.d.DB.Raw("SELECT COALESCE(MAX(id), 0) FROM changes").Scan(&maxId)
	r.mu.Lock()
	r.lastBuild, r.lastCost, r.lastMax, r.lastEpoch = cl.clk.now(), 2*time.Minute, maxId, m.Epoch()
	r.mu.Unlock()
	// 定时任务、容灾自己的记录:不触发
	m.d.DB.Create(&model.Change{DateTime: 1, Actor: "ResetJob", Key: "user", Action: "reset"})
	m.d.DB.Create(&model.Change{DateTime: 1, Actor: "ha", Key: "ha", Action: "x"})
	m.d.DB.Create(&model.Change{DateTime: 1, Actor: "RuleJob", Key: "rule", Action: "x"})
	if r.due(cfg, false) {
		t.Fatal("刚生成过")
	}
	cl.clk.add(2 * time.Minute)
	if r.due(cfg, false) {
		t.Fatal("定时任务、容灾自己的审计不该触发补发")
	}
	// 管理员的改动:30 秒防抖,但离上一份还不到 4×耗时(8 分钟)也不发
	m.d.DB.Create(&model.Change{DateTime: 1, Actor: "admin", Key: "user", Action: "update"})
	r.due(cfg, false)
	cl.clk.add(time.Minute)
	if r.due(cfg, false) {
		t.Fatal("一份要做 2 分钟的库,改动触发的补发也要隔 8 分钟")
	}
	cl.clk.add(5 * time.Minute)
	if !r.due(cfg, false) {
		t.Fatal("过了 4×耗时就该补发")
	}
}

// #27:备用的磁盘门槛按接任实际要的(库 + 新库 + 压缩包 + 两代副本)算,手里副本占的算回可用;进名单多要 10% 余量。
func TestStandbyDiskThresholdHysteresis(t *testing.T) {
	cl := newCluster(t, 2)
	m := cl.m
	cl.h.RunOnce()
	if err := m.ReplicateNow(); err != nil {
		t.Fatal(err)
	}
	cl.h.RunOnce()
	ns, ok := m.d.Hub.StatusOf(cl.ids[0])
	if !ok || ns.HA == nil || ns.HA.DiskFree <= 0 {
		t.Skip("读不到磁盘剩余")
	}
	if ns.HA.ReplicaBytes <= 0 {
		t.Fatal("备用要报手里副本占的磁盘")
	}
	avail := ns.HA.DiskFree + ns.HA.ReplicaBytes
	z := int64(1 << 20)
	setNeed := func(need int64) {
		db := (need - 3*z - 64<<20) / 2
		m.rep.mu.Lock()
		m.rep.lastSize, m.rep.lastDB = z, db
		m.rep.mu.Unlock()
		if got := standbyNeed(z, db); got-need > 1 || need-got > 1 {
			t.Fatalf("门槛算法: %d vs %d", got, need)
		}
	}
	// 门槛是可用的 95%:已在名单里的照常合格;不在名单里的(第二台挪出去再挪回来)要多 10% 余量,进不来
	setNeed(avail * 95 / 100)
	infos, _ := m.rep.compute(m.Config())
	if c := infoOf(t, infos, cl.ids[0]); !c.Eligible {
		t.Fatalf("在名单里、够门槛的要合格: %+v", c)
	}
	m.rep.setList(infos, []uint{cl.ids[0]}, true) // 第二台不在名单里
	infos, _ = m.rep.compute(m.Config())
	if c := infoOf(t, infos, cl.ids[1]); c.Eligible || !hasReason(c.Reasons, reasonDisk) {
		t.Fatalf("不在名单里的进名单要多 10%% 余量: %+v", c)
	}
	// 门槛超过可用:名单里的那台暂时不合格,但保留名次(不删副本)
	setNeed(avail * 105 / 100)
	infos, ids := m.rep.compute(m.Config())
	if c := infoOf(t, infos, cl.ids[0]); c.Eligible || !c.Held || c.Rank != 1 {
		t.Fatalf("磁盘暂时不够:保留名次: %+v %v", c, ids)
	}
	// TakeoverNeed:不知道库多大时按压缩包 4 倍估
	if TakeoverNeed(z, 0) != z+8*z+64<<20 {
		t.Fatal("TakeoverNeed 估算")
	}
}

// #53 #30:名单里的备用暂时离线保留名次 30 分钟(不立刻顺延给后排);到时顺延并通知新面孔;
// 全部副机同时离线(主机自己断网)时名单整个冻结,恢复后的第一轮状态不会让备用删副本。
func TestMasterOutageKeepsList(t *testing.T) {
	cl := newCluster(t, 3)
	m := cl.m
	cl.h.RunOnce()
	if err := m.ReplicateNow(); err != nil {
		t.Fatal(err)
	}
	cl.h.RunOnce()
	want := []uint{cl.ids[0], cl.ids[1]}
	of := replicaOnlineFor
	t.Cleanup(func() { replicaOnlineFor = of })
	// 全部离线
	replicaOnlineFor = -1
	for i := 0; i < 2; i++ {
		_, ids := m.rep.compute(m.Config())
		if idsKey(ids) != idsKey(want) {
			t.Fatalf("全部副机同时离线时名单要冻结: %v", ids)
		}
		cl.clk.add(40 * time.Minute) // 冻结不受 30 分钟限制
	}
	infos, ids := m.rep.compute(m.Config())
	m.rep.setList(infos, ids, true)
	replicaOnlineFor = of
	cl.h.RunOnce() // 恢复后的第一轮先推状态
	for i := 0; i < 2; i++ {
		if _, ok := cl.cands[i].replicaInfo(); !ok {
			t.Fatalf("主机断网恢复后备用 %d 不能删副本", i+1)
		}
	}
	if notesWith(cl, "新的备用") != 0 {
		t.Fatal("名单没变不该提示新面孔")
	}
	// 只有第一台离线:保留名次 30 分钟
	cl.agents[0].setDown(true)
	replicaOnlineFor = 1
	time.Sleep(2500 * time.Millisecond)
	cl.h.RunOnce()
	infos, ids = m.rep.compute(m.Config())
	if idsKey(ids) != idsKey(want) {
		t.Fatalf("暂时离线的备用保留名次: %v", ids)
	}
	if c := infoOf(t, infos, cl.ids[0]); !c.Held || !hasReason(c.Reasons, reasonHolding) {
		t.Fatalf("要标成保留中: %+v", c)
	}
	m.rep.setList(infos, ids, true)
	cl.clk.add(31 * time.Minute)
	cl.h.RunOnce()
	infos, ids = m.rep.compute(m.Config())
	if idsKey(ids) != idsKey([]uint{cl.ids[1], cl.ids[2]}) {
		t.Fatalf("离线超过 30 分钟才顺延: %v", ids)
	}
	m.rep.setList(infos, ids, true)
	if notesWith(cl, "新的备用") != 1 {
		t.Fatalf("名单出现新面孔要通知一次: %v", *cl.notes)
	}
	found := false
	for _, n := range m.Notices() {
		if n.Kind == "standbyNew" && n.Name == "TW3" && n.N == 2 {
			found = true
		}
	}
	if !found {
		t.Fatalf("面板顶部要提示新面孔: %+v", m.Notices())
	}
}

// #32:还没开容灾(纪元 0)时副机本来就不报备用公钥:不算不合格,就绪检查给 info 而不是 ✗。
func TestReadinessEpochZero(t *testing.T) {
	cl := newCluster(t, 1)
	m := cl.m
	m.set(map[string]string{KeyEpoch: "0", KeyMode: ModeOff})
	cl.h.RunOnce()
	v := m.View(true)
	var standbys, pubLater *Check
	for i := range v.Ready {
		switch v.Ready[i].Code {
		case "standbys":
			standbys = &v.Ready[i]
		case "pubLater":
			pubLater = &v.Ready[i]
		}
	}
	if standbys == nil || standbys.Level == "bad" {
		t.Fatalf("纪元 0 时在线的副机算合格: %+v", v.Ready)
	}
	if pubLater == nil || pubLater.Level != "info" || len(pubLater.Names) != 1 {
		t.Fatalf("纪元 0 时提示开启后自动报公钥: %+v", v.Ready)
	}
	for _, c := range v.Standbys {
		if hasReason(c.Reasons, reasonNoPub) {
			t.Fatalf("纪元 0 不该有 noPub: %+v", c)
		}
	}
}

// #29 #50 #58 C6:决定订阅内容的三项、失联的原主机、明文备用开关归全局(随副本走);待删副本的副机归本机。
func TestSettingsClassificationSubKeys(t *testing.T) {
	for _, k := range []string{"subServerAddr", "subInsecure", "subRateLimit", KeyFormer, KeyAllowPlain} {
		if IsLocal(k) || !IsGlobal(k) {
			t.Fatalf("%s 要归全局", k)
		}
	}
	if !IsLocal(KeyDropPending) || IsGlobal(KeyDropPending) {
		t.Fatal("haDropPending 归本机")
	}
	if !Hidden(KeyAllowPlain) || !Hidden(KeyDropPending) {
		t.Fatal("ha 开头的键不发给浏览器")
	}
	s := testService(t, false, map[string]string{KeyEpoch: "1", "subServerAddr": "domain", "subInsecure": "true", "subRateLimit": "30",
		KeyFormer: `{"id":7,"name":"old"}`})
	seedMaster(t, s)
	db, _ := openReplicaDB(t, mustZip(t, s))
	for k, want := range map[string]string{"subServerAddr": "domain", "subInsecure": "true", "subRateLimit": "30"} {
		var v string
		db.Raw("SELECT value FROM settings WHERE key = ?", k).Scan(&v)
		if v != want {
			t.Fatalf("副本要带 %s=%s,实际 %q", k, want, v)
		}
	}
}

func mustZip(t *testing.T, s *Service) string {
	t.Helper()
	zp, _, err := s.buildReplica(t.TempDir())
	if err != nil {
		t.Fatal(err)
	}
	return zp
}

// #57:停用 / 删除的备用当时连不上,删副本(删除时再离开集群)的请求记下来,每轮重试直到它确认。
func TestDropPendingRetries(t *testing.T) {
	cl := newCluster(t, 2)
	m := cl.m
	cl.h.RunOnce()
	if err := m.ReplicateNow(); err != nil {
		t.Fatal(err)
	}
	var n0, n1 model.Node
	m.d.DB.First(&n0, cl.ids[0])
	m.d.DB.First(&n1, cl.ids[1])
	// 等 QueueDrop 当场那一次试完(连不上:记下一次失败)
	waitDrops := func() {
		for i := 0; i < 500; i++ {
			done := true
			for _, p := range m.DropPending() {
				if p.Tries == 0 {
					done = false
				}
			}
			if done {
				m.RetryDrops() // 等在跑的那一轮收尾
				return
			}
			time.Sleep(10 * time.Millisecond)
		}
		t.Fatalf("当场那一次没试完: %+v", m.DropPending())
	}
	// 第一台停用、第二台删除,当时都连不上
	cl.agents[0].setDown(true)
	cl.agents[1].setDown(true)
	m.d.DB.Model(&model.Node{}).Where("id = ?", n0.Id).Update("enabled", false)
	m.d.DB.Delete(&model.Node{}, n1.Id)
	m.QueueDrop(n0, false)
	waitDrops()
	m.QueueDrop(n1, true)
	waitDrops()
	if p := m.DropPending(); len(p) != 2 {
		t.Fatalf("连不上时要记下待办: %+v", p)
	}
	if v := m.View(false); len(v.DropPend) != 2 {
		t.Fatal("面板要能看到待删副本的副机")
	}
	if strings.Contains(m.setting(KeyDropPending), "tok") == false {
		t.Fatal("待办里要存连它用的令牌(本机设置,不进副本)")
	}
	// 还没到下次重试:不发
	calls := cl.agents[0].count("ha/drop")
	m.RetryDrops()
	if cl.agents[0].count("ha/drop") != calls {
		t.Fatal("没到下次重试时间不该发")
	}
	// 回来了:重试成功,副本删掉,待办清空;删除的那台还要离开集群
	cl.agents[0].setDown(false)
	cl.agents[1].setDown(false)
	cl.clk.add(5 * time.Minute)
	m.RetryDrops()
	if p := m.DropPending(); len(p) != 0 {
		t.Fatalf("确认后清掉待办: %+v", p)
	}
	for i := 0; i < 2; i++ {
		if _, ok := cl.cands[i].replicaInfo(); ok {
			t.Fatalf("副机 %d 回来后要删掉副本", i+1)
		}
	}
	if cl.agents[1].count("ha/leave") != 1 || cl.agents[0].count("ha/leave") != 0 {
		t.Fatal("只有删除的那台要离开集群")
	}
	// 停用后又启用:不再催它删
	cl.agents[0].setDown(true)
	m.QueueDrop(n0, false)
	waitDrops()
	if len(m.DropPending()) != 1 {
		t.Fatal("连不上时记下待办")
	}
	m.d.DB.Model(&model.Node{}).Where("id = ?", n0.Id).Update("enabled", true)
	cl.clk.add(time.Hour)
	m.RetryDrops()
	if len(m.DropPending()) != 0 {
		t.Fatal("重新启用的副机不再催它删副本")
	}
}

// #19 #48:明文 http 链接的副机默认不当备用;打开开关才算(就绪检查给提示)。
func TestPlainStandbyOptIn(t *testing.T) {
	cl := newCluster(t, 1)
	m := cl.m
	cl.h.RunOnce()
	m.d.DB.Model(&model.Node{}).Where("id = ?", cl.ids[0]).Update("api_url", strings.Replace(cl.agents[0].url(), "https://", "http://", 1))
	infos, ids := m.rep.compute(m.Config())
	if c := infoOf(t, infos, cl.ids[0]); c.Eligible || !hasReason(c.Reasons, reasonPlain) || len(ids) != 0 {
		t.Fatalf("明文链接默认不当备用: %+v %v", c, ids)
	}
	found := false
	for _, ch := range m.readiness(m.Config(), infos) {
		if ch.Code == "plainOptIn" && len(ch.Names) == 1 {
			found = true
		}
	}
	if !found {
		t.Fatal("就绪检查要说明明文链接的副机为什么不当备用")
	}
	c := m.Config()
	c.AllowPlain = true
	if err := m.SaveConfig(ConfigInput{Config: c}); err != nil {
		t.Fatal(err)
	}
	if !m.Config().AllowPlain {
		t.Fatal("开关要保存")
	}
	infos, ids = m.rep.compute(m.Config())
	if c := infoOf(t, infos, cl.ids[0]); !c.Eligible || len(ids) != 1 {
		t.Fatalf("打开开关后明文链接的副机可以当备用: %+v", c)
	}
}

// #52:同伴的响应签名绑定请求:旧的回复回放给另一次请求、时间戳过期、投票回复的纪元 / 候选人对不上,一律不算。
func TestPeerReplayRejected(t *testing.T) {
	a := testService(t, true, map[string]string{})
	b := testService(t, true, map[string]string{})
	key := make([]byte, 32)
	rand.Read(key)
	pk := base64.StdEncoding.EncodeToString(key)
	a.set(map[string]string{KeyPeerKey: pk})
	b.set(map[string]string{KeyPeerKey: pk})
	a.d.DB.Create(&model.Node{Id: 2, Name: "A", IsLocal: true, Enabled: true})
	var mu sync.Mutex
	var recorded *http.Response
	var recBody []byte
	mode := "record"
	var forged []byte
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		body, _ := io.ReadAll(r.Body)
		action := r.URL.Path[strings.LastIndex(r.URL.Path, "/")+1:]
		mu.Lock()
		defer mu.Unlock()
		switch mode {
		case "record":
			rep, err := b.ServePeer(action, r.Header, body)
			if err != nil {
				w.WriteHeader(401)
				return
			}
			recorded = &http.Response{Header: rep.Header}
			recBody = rep.Body
			for k, vs := range rep.Header {
				for _, v := range vs {
					w.Header().Add(k, v)
				}
			}
			w.Write(rep.Body)
		case "replay":
			for k, vs := range recorded.Header {
				for _, v := range vs {
					w.Header().Add(k, v)
				}
			}
			w.Write(recBody)
		case "forge": // 签名对得上这一次请求,但内容是给别的纪元 / 候选人的
			rts := strconv.FormatInt(time.Now().Unix(), 10)
			w.Header().Set(peerHeaderTs, rts)
			w.Header().Set(peerHeaderSig, peerMAC(key, "resp", action, rts, r.Header.Get(peerHeaderTs), strings.ToLower(r.Header.Get(peerHeaderSig)), bodySum(forged)))
			w.Write(forged)
		case "stale":
			rts := strconv.FormatInt(time.Now().Add(-time.Hour).Unix(), 10)
			b, _ := json.Marshal(PeerStatus{Id: 3})
			w.Header().Set(peerHeaderTs, rts)
			w.Header().Set(peerHeaderSig, peerMAC(key, "resp", action, rts, r.Header.Get(peerHeaderTs), strings.ToLower(r.Header.Get(peerHeaderSig)), bodySum(b)))
			w.Write(b)
		}
	}))
	defer srv.Close()
	p := peer{node: model.Node{Id: 3, Name: "B", ApiUrl: srv.URL}}
	var st PeerStatus
	if err := a.callPeer(context.Background(), p, "status", nil, &st); err != nil {
		t.Fatalf("正常请求: %v", err)
	}
	mu.Lock()
	mode = "replay"
	mu.Unlock()
	time.Sleep(1100 * time.Millisecond) // 下一次请求的时间戳不同
	if err := a.callPeer(context.Background(), p, "status", nil, &st); err == nil {
		t.Fatal("回放旧的回复要被拒")
	}
	mu.Lock()
	mode = "stale"
	mu.Unlock()
	if err := a.callPeer(context.Background(), p, "status", nil, &st); err == nil {
		t.Fatal("回复时间戳过期要被拒")
	}
	// 投票回复对不上这一次:不算票
	a.set(map[string]string{KeyState: mustJSON(State{Epoch: 1, Self: 2})})
	a.d.DB.Create(&model.Node{Id: 3, Name: "B", ApiUrl: srv.URL, Enabled: true})
	mu.Lock()
	mode = "forge"
	forged, _ = json.Marshal(VoteReply{Granted: true, Epoch: 7, Candidate: 2})
	mu.Unlock()
	n, err := a.requestVotes(context.Background(), 2)
	if err != nil || n != 0 {
		t.Fatalf("纪元对不上的同意不算: %d %v", n, err)
	}
	mu.Lock()
	forged, _ = json.Marshal(VoteReply{Granted: true, Epoch: 2, Candidate: 9})
	mu.Unlock()
	if n, _ := a.requestVotes(context.Background(), 2); n != 0 {
		t.Fatal("候选人对不上的同意不算")
	}
	mu.Lock()
	forged, _ = json.Marshal(VoteReply{Granted: true, Epoch: 2, Candidate: 2})
	mu.Unlock()
	if n, _ := a.requestVotes(context.Background(), 2); n != 1 {
		t.Fatal("对得上的同意要算")
	}
	var v voteRec
	if !a.getJSON(KeyVote, &v) || v.CandPub == "" {
		t.Fatal("自己的票要带当主机后的签名公钥")
	}
}

// #21:票 30 分钟后作废:一次没成功的选举不能永远挡住别的候选。
func TestVoteExpiresForOtherCandidate(t *testing.T) {
	p := newP2(t, 3, ModeAuto)
	b := p.cands[2]                  // 第三台不是备用,也投票
	p.clocks[2].add(2 * time.Minute) // 它也联系不上主机
	first := VoteRequest{Epoch: 2, Candidate: p.ids[0]}
	if r := b.grantVote(first); !r.Granted || r.Candidate != p.ids[0] {
		t.Fatalf("先给第一个候选: %+v", r)
	}
	other := VoteRequest{Epoch: 2, Candidate: p.ids[1]}
	if r := b.grantVote(other); r.Granted || r.Reason != "voted" {
		t.Fatalf("同一纪元有效期内不再给别人: %+v", r)
	}
	p.clocks[2].add(31 * time.Minute)
	if r := b.grantVote(other); !r.Granted {
		t.Fatalf("票过期后可以给别的候选: %+v", r)
	}
	if ps := b.peerStatus(); ps.Vote == nil || ps.Vote.Candidate != p.ids[1] {
		t.Fatalf("同伴看到的是有效的那张票: %+v", ps.Vote)
	}
}

// #33:每收下一份新副本都在后台自检(限频);最近一次自检的结果不随新副本清零。
func TestSelfCheckEveryReplica(t *testing.T) {
	selfCheckAsync = true
	gap := selfCheckGap
	selfCheckGap = 200 * time.Millisecond
	t.Cleanup(func() { selfCheckAsync, selfCheckGap = false, gap })
	m, c := standbyPair(t)
	waitSeq := func(seq uint64) ReplicaInfo {
		t.Helper()
		for i := 0; i < 300; i++ {
			c.rep.checkMu.Lock()
			pending := c.rep.checkPending
			c.rep.checkMu.Unlock()
			if ri, ok := c.replicaInfo(); ok && ri.VerifiedSeq == seq && !pending {
				return ri
			}
			time.Sleep(20 * time.Millisecond)
		}
		ri, _ := c.replicaInfo()
		t.Fatalf("序号 %d 没自检: %+v", seq, ri)
		return ri
	}
	if _, err := c.ReceiveReplica(bytes.NewReader(replicaFor(t, m, c, 10))); err != nil {
		t.Fatal(err)
	}
	ri := waitSeq(10)
	if ri.VerifiedAt == 0 || ri.VerifyErr != "" || ri.DBSize <= 0 {
		t.Fatalf("自检结果与库大小: %+v", ri)
	}
	m.d.DB.Model(&model.User{}).Where("1 = 1").Update("up", 12345)
	if _, err := c.ReceiveReplica(bytes.NewReader(replicaFor(t, m, c, 11))); err != nil {
		t.Fatal(err)
	}
	if ri, _ := c.replicaInfo(); ri.VerifiedAt == 0 {
		t.Fatal("新副本收下时,上一次自检的结果要留着")
	}
	ri = waitSeq(11)
	if ri.VerifyErr != "" {
		t.Fatal(ri.VerifyErr)
	}
}

// #15:副本用收下时记的主机公钥验签(之后跟了别的主机也能验);不知道公钥的一律不用,不跳过验签。
func TestReplicaVerifiedWithStoredPub(t *testing.T) {
	m, c := standbyPair(t)
	pub := c.setting(KeyMasterPub)
	if _, err := c.ReceiveReplica(bytes.NewReader(replicaFor(t, m, c, 10))); err != nil {
		t.Fatal(err)
	}
	if ri, _ := c.replicaInfo(); ri.MasterPub != pub {
		t.Fatal("收下时要记验签用的公钥")
	}
	if _, err := c.ReceiveReplica(bytes.NewReader(replicaFor(t, m, c, 11))); err != nil {
		t.Fatal(err)
	}
	// 本机后来钉了别的主机公钥:两代副本照样能用各自记下的公钥验
	other, _ := SignKey(NewSignKey())
	c.set(map[string]string{KeyMasterPub: PubOf(other)})
	for _, name := range []string{fileCur, filePrev} {
		if _, _, err := c.decryptReplica(filepath.Join(c.Dir(), name), t.TempDir(), c.replicaPubFor(name)); err != nil {
			t.Fatalf("%s 要用记下的公钥验: %v", name, err)
		}
	}
	// 别的主机的公钥验不过
	if _, _, err := c.decryptReplica(filepath.Join(c.Dir(), fileCur), t.TempDir(), PubOf(other)); err == nil {
		t.Fatal("公钥不对要验不过")
	}
	// 没有记下的公钥、也没钉主机公钥:不用(以前会跳过验签)
	var ri ReplicaInfo
	b, _ := os.ReadFile(filepath.Join(c.Dir(), fileInfo))
	json.Unmarshal(b, &ri)
	ri.MasterPub = ""
	c.saveReplicaInfo(ri)
	os.Remove(filepath.Join(c.Dir(), filePrevInfo))
	c.set(map[string]string{KeyMasterPub: ""})
	if _, _, _, err := c.openReplica(t.TempDir()); err == nil {
		t.Fatal("不知道公钥的副本不能拿来用")
	}
}

// #27:备用收副本时按接任口径查磁盘(放不下就拒收并说原因)。
func TestReceiveReplicaDiskCheck(t *testing.T) {
	m, c := standbyPair(t)
	free := c.diskFree()
	if free < 0 {
		t.Skip("读不到磁盘剩余")
	}
	// 让"上一份库多大"大到放不下:按比例估出的库超过剩余
	if _, err := c.ReceiveReplica(bytes.NewReader(replicaFor(t, m, c, 10))); err != nil {
		t.Fatal(err)
	}
	ri, _ := c.replicaInfo()
	ri.DBSize = free * ri.Size // 比例 = 剩余 / 1 字节压缩包,估出的库一定放不下
	if ri.DBSize/ri.Size != free {
		t.Skip("溢出")
	}
	c.saveReplicaInfo(ri)
	_, err := c.ReceiveReplica(bytes.NewReader(replicaFor(t, m, c, 11)))
	if err == nil || !strings.Contains(err.Error(), "磁盘剩余空间不够") {
		t.Fatalf("放不下接任要拒收并说原因: %v", err)
	}
	if ri2, _ := c.replicaInfo(); ri2.Seq != 10 {
		t.Fatal("拒收时旧的不动")
	}
}

// C7:报告带接任后会用的端口、代理面板地址、副本占用、签名指纹;canTakeover 走证据判定。
func TestReportFieldsAndSummary(t *testing.T) {
	_, c := standbyPair(t)
	c.set(map[string]string{"webPort": "8443", "subPort": "8444"})
	rep := c.Report()
	if rep.WebPort != 8443 || rep.SubPort != 8444 || rep.ResellerPort == 0 {
		t.Fatalf("报告要带端口: %+v", rep)
	}
	if rep.SignFP != "" {
		t.Fatal("副机没有签名密钥时不报、也不生成")
	}
	if c.setting(KeySignKey) != "" {
		t.Fatal("报告不能顺手生成签名密钥")
	}
}
