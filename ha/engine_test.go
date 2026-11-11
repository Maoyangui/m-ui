package ha

import (
	"context"
	"encoding/json"
	"fmt"
	"net/http"
	"net/http/httptest"
	"regexp"
	"strconv"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/Maoyangui/m-ui/database/model"
)

// p2Cluster 一台开着主机容灾的主机 + n 台副机(前两台是备用);副机库里有彼此的服务器行(和快照下发的一样:
// 地址、证书校验方式,没有令牌),各有自己的钟。返回各副机的钟与"重启了几次"。
type p2 struct {
	*haCluster
	clocks   []*testClock
	restarts []*int
	mu       sync.Mutex
}

func newP2(t *testing.T, n int, mode string) *p2 {
	t.Helper()
	cl := newCluster(t, n)
	m := cl.m
	// 自动接任按最短的 5 分钟:测试里各台的钟分开拨,彼此差太多同伴接口会因为时间戳不对拒绝
	m.set(map[string]string{KeyMode: mode, KeyAutoMinutes: "5", "publicIp": "203.0.113.9"})
	p := &p2{haCluster: cl}
	for i, c := range cl.cands {
		clk := &testClock{t: time.Now()}
		c.SetNow(clk.now)
		cnt := new(int)
		c.d.Restart = func(time.Duration) {
			p.mu.Lock()
			*cnt++
			p.mu.Unlock()
		}
		p.clocks, p.restarts = append(p.clocks, clk), append(p.restarts, cnt)
		// 彼此的服务器行(快照里有的那些)
		c.d.DB.Create(&model.Node{Id: m.SelfId(), Name: "HK", Enabled: true, Sort: 1})
		for j, other := range cl.agents {
			if j == i {
				continue
			}
			c.d.DB.Create(&model.Node{Id: cl.ids[j], Name: "TW" + strconv.Itoa(j+1), ApiUrl: other.url(), Insecure: true, Enabled: true, Sort: 2 + j})
		}
	}
	m.invalidate()
	cl.h.RunOnce()
	cl.h.RunOnce()
	if err := m.ReplicateNow(); err != nil {
		t.Fatal(err)
	}
	cl.h.RunOnce() // 状态里带上集群密钥、备用名单
	for i, c := range cl.cands {
		if st, ok := c.NodeState(); !ok || st.Mode != mode || c.setting(KeyPeerKey) == "" {
			t.Fatalf("副机 %d 要收到容灾状态与集群密钥: %+v", i, st)
		}
	}
	return p
}

func (p *p2) restarted(i int) int {
	p.mu.Lock()
	defer p.mu.Unlock()
	return *p.restarts[i]
}

// 只有一台备用连不上主机(别的副机还连得上):不接任,告一次警。
func TestEnginePartitionNoTakeover(t *testing.T) {
	p := newP2(t, 3, ModeAuto)
	p.clocks[0].add(7 * time.Minute)
	c := p.cands[0]
	c.eng.tick()
	if p.restarted(0) != 0 {
		t.Fatal("别的副机还连得上主机,不能接任")
	}
	if ev := c.eng.evidenceView(); ev == nil || ev.Reason != "peerSeesMaster" || ev.PeersOK != 2 {
		t.Fatalf("证据要写明是别的副机还连得上主机: %+v", ev)
	}
}

// 主机真的不在了(所有副机都联系不上):自动接任要过半票;票够了接任,票不够(一台同伴也连不上)不接任。
func TestEngineAutoTakeoverQuorum(t *testing.T) {
	p := newP2(t, 3, ModeAuto)
	// 第三台连不上:要票只能拿到第二台的 → 2/3(集群 4 台,要 3 票)不够
	p.agents[2].setDown(true)
	for _, clk := range p.clocks {
		clk.add(7 * time.Minute)
	}
	c := p.cands[0]
	c.eng.tick()
	if p.restarted(0) != 0 {
		t.Fatal("票不够不能接任")
	}
	if ev := c.eng.evidenceView(); ev == nil || ev.Reason != "noQuorum" {
		t.Fatalf("要写明票不够: %+v", ev)
	}
	// 第三台回来:3 票过半,接任(新库带纪元 2;本机库的纪元要等换上新库才变)
	p.agents[2].setDown(false)
	p.clocks[0].add(time.Minute) // 过了证据刷新间隔
	c.eng.tick()
	if m, ok := readPromoteMeta(c.d.DBPath); p.restarted(0) != 1 || c.setting(KeyRole) != RolePromoting || c.Epoch() != 1 || !ok || m.Epoch != 2 {
		t.Fatalf("票够了应接任(新库纪元 2): restarts=%d role=%q epoch=%d meta=%+v", p.restarted(0), c.setting(KeyRole), c.Epoch(), m)
	}
	var v Vote
	if !p.cands[1].getJSON(KeyVote, &v) || v.Epoch != 2 || v.Candidate != p.ids[0] {
		t.Fatalf("同意的副机要先把票落库: %+v", v)
	}
}

// 一主一副:自动接任必须境外测点也说主机不通;测点说主机还活着就是网络分区,不接任。
func TestEngineTwoNodesNeedWitness(t *testing.T) {
	p := newP2(t, 1, ModeAuto)
	c := p.cands[0]
	verdict := "up"
	calls := 0
	c.d.Witness = func(ctx context.Context, host string, ports []int) (WitnessResult, error) {
		calls++
		if host != "203.0.113.9" || len(ports) == 0 {
			t.Errorf("见证要测主机的公网 IP 与端口: %s %v", host, ports)
		}
		if verdict == "up" {
			return WitnessResult{Verdict: "up", OK: 9, Total: 10}, nil
		}
		return WitnessResult{Verdict: "down", OK: 0, Total: 10}, nil
	}
	p.clocks[0].add(7 * time.Minute)
	c.eng.tick()
	if p.restarted(0) != 0 || calls != 1 {
		t.Fatalf("境外测点能连上主机:不接任(见证 %d 次)", calls)
	}
	if ev := c.eng.evidenceView(); ev == nil || ev.Reason != "witnessUp" {
		t.Fatalf("要写明境外测点能连上: %+v", ev)
	}
	verdict = "down"
	p.clocks[0].add(6 * time.Minute)
	c.eng.tick()
	if p.restarted(0) != 1 {
		t.Fatal("境外测点也连不上主机:接任")
	}
}

// 名次接力:备用 1 还在、手里有副本,备用 2 不动;备用 1 也不在了,备用 2 在阈值再加名次间隔之后发起(确认后接任:等确认)。
func TestEngineRankRelay(t *testing.T) {
	p := newP2(t, 2, ModeConfirm)
	for _, clk := range p.clocks {
		clk.add(4 * time.Minute)
	}
	second := p.cands[1]
	second.eng.tick()
	if second.eng.promptView() != nil {
		t.Fatal("备用 1 还在:备用 2 不发起")
	}
	if ev := second.eng.evidenceView(); ev == nil || ev.Reason != "betterStandby" {
		t.Fatalf("要写明在等备用 1: %+v", ev)
	}
	p.agents[0].setDown(true)
	p.clocks[1].add(time.Minute) // 5 分钟:还没到 3+3
	second.eng.tick()
	if second.eng.promptView() != nil {
		t.Fatal("备用 2 要多等一个名次间隔")
	}
	p.clocks[1].add(2 * time.Minute)
	second.eng.tick()
	if second.eng.promptView() == nil {
		t.Fatal("备用 1 不在、时间到了:备用 2 发确认")
	}
	if v := second.View(false); !v.Node.CanTakeover {
		t.Fatal("等确认时备用面板上能点接任")
	}
}

// 主机暂停了接任(升级前、计划内重启前):不判失联。
func TestEnginePaused(t *testing.T) {
	p := newP2(t, 1, ModeAuto)
	p.m.PauseForUpgrade()
	p.h.RunOnce()
	c := p.cands[0]
	if st, _ := c.NodeState(); st.PauseUntil <= time.Now().Unix() {
		t.Fatal("升级前暂停要推给备用")
	}
	p.clocks[0].add(12 * time.Minute)
	c.eng.tick()
	if p.restarted(0) != 0 {
		t.Fatal("暂停期间不能接任")
	}
	if ev := c.eng.evidenceView(); ev == nil || ev.Reason != "paused" {
		t.Fatalf("要写明暂停中: %+v", ev)
	}
	if err := c.Takeover(TakeoverOpts{By: "panel"}); err == nil || !strings.Contains(err.Error(), "暂停") {
		t.Fatalf("面板上点也要说明暂停中: %v", err)
	}
}

// 确认后接任:失联到时间发一条 Telegram(带确认码);别的会话、错的码都不认;对的码 → 接任。主机恢复联系就取消。
func TestEngineConfirmViaTelegram(t *testing.T) {
	var mu sync.Mutex
	var sent []string
	var replied bool
	badOffset := ""
	codeRe := regexp.MustCompile(`/takeover (\d{4})`)
	tg := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		switch {
		case strings.HasSuffix(r.URL.Path, "/sendMessage"):
			var b struct {
				Text string `json:"text"`
			}
			json.NewDecoder(r.Body).Decode(&b)
			mu.Lock()
			sent = append(sent, b.Text)
			mu.Unlock()
			w.Write([]byte(`{"ok":true}`))
		case strings.HasSuffix(r.URL.Path, "/getUpdates"):
			mu.Lock()
			code := ""
			for _, s := range sent {
				if m := codeRe.FindStringSubmatch(s); m != nil {
					code = m[1]
				}
			}
			if r.URL.Query().Get("offset") != "0" {
				badOffset = r.URL.Query().Get("offset") // 不能确认(吃掉)任何消息:主机的机器人回来照样要收得到
			}
			// 确认提示之后才来的消息:先看一眼队列(timeout=0)时还不在
			first := !replied && code != "" && r.URL.Query().Get("timeout") != "0"
			if first {
				replied = true
			}
			mu.Unlock()
			if !first {
				if r.URL.Query().Get("timeout") != "0" {
					time.Sleep(50 * time.Millisecond)
				}
				w.Write([]byte(`{"ok":true,"result":[]}`))
				return
			}
			fmt.Fprintf(w, `{"ok":true,"result":[
				{"update_id":10,"message":{"text":"/takeover %s","chat":{"id":99}}},
				{"update_id":11,"message":{"text":"/takeover 0000","chat":{"id":42}}},
				{"update_id":12,"message":{"text":"接任 %s","chat":{"id":42}}}]}`, code, code)
		default:
			http.NotFound(w, r)
		}
	}))
	defer tg.Close()
	p := newP2(t, 1, ModeConfirm)
	p.m.set(map[string]string{"tgEnabled": "true", "tgToken": "T", "tgChatId": "42", "tgApiBase": tg.URL})
	p.m.invalidate()
	p.h.RunOnce()
	c := p.cands[0]
	if c.standbyNotifySetting("tgChatId") != "42" {
		t.Fatal("备用要收到密封的通知设置")
	}
	p.clocks[0].add(4 * time.Minute)
	c.eng.tick()
	if c.eng.promptView() == nil {
		t.Fatal("失联到时间要发确认")
	}
	deadline := time.Now().Add(10 * time.Second)
	for p.restarted(0) == 0 && time.Now().Before(deadline) {
		time.Sleep(20 * time.Millisecond)
	}
	if m, ok := readPromoteMeta(c.d.DBPath); p.restarted(0) != 1 || !ok || m.Epoch != 2 {
		t.Fatalf("回复了对的码应接任: restarts=%d meta=%+v", p.restarted(0), m)
	}
	mu.Lock()
	defer mu.Unlock()
	if len(sent) == 0 || !strings.Contains(sent[0], "已失联") {
		t.Fatalf("确认消息要说清主机失联: %q", sent)
	}
	if badOffset != "" {
		t.Fatalf("等确认时不能确认任何 Telegram 消息(offset 一直是 0),实际用了 %s", badOffset)
	}
}

// 主机恢复联系:取消等确认,发一句已恢复。
func TestEngineCancelOnRecovery(t *testing.T) {
	p := newP2(t, 1, ModeConfirm)
	c := p.cands[0]
	p.clocks[0].add(4 * time.Minute)
	c.eng.tick()
	if c.eng.promptView() == nil {
		t.Fatal("前提:在等确认")
	}
	p.h.RunOnce() // 主机又来请求了
	if c.eng.promptView() != nil || c.eng.evidenceView() != nil {
		t.Fatal("主机恢复联系后要取消")
	}
}

// 同伴接口:签名错、时间戳过期都拒;投票只在本机也联系不上主机时给,同一纪元只投一个,先落库。
func TestPeerAuthAndVote(t *testing.T) {
	p := newP2(t, 2, ModeAuto)
	a, b := p.cands[0], p.cands[1]
	key := peerKey(a.setting(KeyPeerKey))
	call := func(action string, body []byte, ts int64, sig string) (PeerReply, error) {
		h := http.Header{}
		h.Set(peerHeaderNode, strconv.FormatUint(uint64(a.SelfId()), 10))
		h.Set(peerHeaderTs, strconv.FormatInt(ts, 10))
		if sig == "" {
			sig = peerMAC(key, http.MethodPost, action, strconv.FormatUint(uint64(a.SelfId()), 10), strconv.FormatInt(ts, 10), bodySum(body))
		}
		h.Set(peerHeaderSig, sig)
		return b.ServePeer(action, h, body)
	}
	now := time.Now().Unix()
	if _, err := call("status", nil, now, "deadbeef"); err == nil {
		t.Fatal("签名不对应拒")
	}
	if _, err := call("status", nil, now-3600, ""); err == nil {
		t.Fatal("时间戳过期应拒")
	}
	rep, err := call("status", nil, now, "")
	if err != nil {
		t.Fatal(err)
	}
	var st PeerStatus
	json.Unmarshal(rep.Body, &st)
	if st.Epoch != 1 || st.Rank != 2 {
		t.Fatalf("status 回本机纪元与名次: %+v", st)
	}
	vote, _ := json.Marshal(VoteRequest{Epoch: 2, Candidate: a.SelfId()})
	rep, _ = call("vote", vote, now, "")
	var vr VoteReply
	json.Unmarshal(rep.Body, &vr)
	if vr.Granted || vr.Reason != "masterOnline" {
		t.Fatalf("本机还联系得上主机:不给票: %+v", vr)
	}
	p.clocks[1].add(2 * time.Minute)
	rep, _ = call("vote", vote, now, "")
	json.Unmarshal(rep.Body, &vr)
	if !vr.Granted {
		t.Fatalf("本机也联系不上主机:给票: %+v", vr)
	}
	other, _ := json.Marshal(VoteRequest{Epoch: 2, Candidate: p.ids[1]})
	h := http.Header{}
	h.Set(peerHeaderNode, strconv.FormatUint(uint64(p.ids[1]), 10))
	h.Set(peerHeaderTs, strconv.FormatInt(now, 10))
	h.Set(peerHeaderSig, peerMAC(key, http.MethodPost, "vote", strconv.FormatUint(uint64(p.ids[1]), 10), strconv.FormatInt(now, 10), bodySum(other)))
	rep, _ = b.ServePeer("vote", h, other)
	json.Unmarshal(rep.Body, &vr)
	if vr.Granted {
		t.Fatal("同一纪元已经投给别人:不再给")
	}
	// 冒充别人要票:发起方和被投票的人对不上
	h.Set(peerHeaderNode, strconv.FormatUint(uint64(a.SelfId()), 10))
	if _, err := b.ServePeer("vote", h, other); err == nil {
		t.Fatal("只能给自己要票")
	}
}

func TestQuorumAndWitnessVerdict(t *testing.T) {
	for size, want := range map[int]int{2: 2, 3: 2, 4: 3, 5: 3, 6: 4} {
		if got := quorum(size); got != want {
			t.Errorf("集群 %d 台要 %d 票,算出 %d", size, want, got)
		}
	}
	cases := []struct {
		ok, total int
		want      string
	}{{0, 0, "unknown"}, {10, 10, "up"}, {7, 10, "up"}, {6, 10, "unknown"}, {4, 10, "unknown"}, {3, 10, "down"}, {0, 10, "down"}}
	for _, c := range cases {
		if got := WitnessVerdict(c.ok, c.total); got != c.want {
			t.Errorf("%d/%d 应是 %s,实际 %s", c.ok, c.total, c.want, got)
		}
	}
	for _, c := range []struct {
		text string
		ok   bool
	}{{"/takeover 1234", true}, {"接任 1234", true}, {"/takeover@bot 1234", true}, {"/takeover 1235", false}, {"takeover 1234", false}, {"/takeover", false}} {
		if confirmText(c.text, "1234") != c.ok {
			t.Errorf("%q 应 %v", c.text, c.ok)
		}
	}
}
