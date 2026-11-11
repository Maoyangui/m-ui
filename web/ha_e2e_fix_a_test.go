//go:build !race

// 和 ha_e2e_test.go 一样:一个进程里起好几台 m-ui,数据面的进程级全局变量在 -race 下必然报竞争,只在普通 go test 里跑。

package web

import (
	"bytes"
	"crypto/ed25519"
	"crypto/rand"
	"encoding/base64"
	"encoding/json"
	"net/http"
	"os"
	"path/filepath"
	"strconv"
	"testing"
	"time"

	"github.com/Maoyangui/m-ui/ha"
)

// haCluster3 一台开着容灾(先确认再接任)的主机 + 两台副机,第一台是备用(standbys 台)。
func haUp(t *testing.T, hk *haInst, standbys int, mode string) {
	t.Helper()
	hk.sync()
	if err := hk.HA().SaveConfig(ha.ConfigInput{Config: ha.Config{Mode: mode, Standbys: standbys, ReplicaMinutes: 5, ConfirmMinutes: 3,
		AutoMinutes: 5, RankStepMinutes: 3}}); err != nil {
		t.Fatal(err)
	}
	hk.sync()
	hk.sync()
	if err := hk.HA().ReplicateNow(); err != nil {
		t.Fatal(err)
	}
	hk.sync()
}

// postAgent 直接调一台的 agent 接口(带令牌与纪元头),返回状态码与正文。
func postAgent(t *testing.T, in *haInst, action, token string, epoch uint64, master uint, body []byte) (int, string) {
	t.Helper()
	req, _ := http.NewRequest(http.MethodPost, in.url()+"api/agent/"+action, bytes.NewReader(body))
	req.Header.Set("X-Agent-Token", token)
	ha.SetHeader(req.Header, epoch, master)
	resp, err := in.ts.Client().Do(req)
	if err != nil {
		t.Fatal(err)
	}
	defer resp.Body.Close()
	var buf bytes.Buffer
	_, _ = buf.ReadFrom(resp.Body)
	return resp.StatusCode, buf.String()
}

// signStateWith 用一把私钥签一份容灾状态(线上格式)。
func signStateWith(k ed25519.PrivateKey, st ha.State) []byte {
	body, _ := json.Marshal(st)
	sig := ed25519.Sign(k, append([]byte("m-ui/ha/state/v2\n"), body...))
	b, _ := json.Marshal(ha.SignedState{V: ha.StateV, Body: body, Sig: base64.StdEncoding.EncodeToString(sig)})
	return b
}

// E5:被攻破的备用拿着副本里的副机令牌,给别的副机推伪造的交接标记、再以更高纪元冒充主机 → 副机全部拒绝,主机不降级。
func TestHAForgedHandoverRejectedE2E(t *testing.T) {
	clk := &haClock{}
	hk := newHAInst(t, "HK", false, clk)
	tw := newHAInst(t, "TW", true, clk)
	jp := newHAInst(t, "JP", true, clk)
	seedHAMaster(t, hk)
	twId := addNode(t, hk, tw, 2)
	jpId := addNode(t, hk, jp, 3)
	hkId := hk.localId()
	haUp(t, hk, 1, ha.ModeConfirm)
	_, evil, _ := ed25519.GenerateKey(rand.Reader)
	forged := ha.State{Epoch: 1, MasterId: hkId, MasterPub: base64.StdEncoding.EncodeToString(evil.Public().(ed25519.PublicKey)),
		Self: jpId, Mode: ha.ModeConfirm, Standbys: []uint{},
		Handover: &ha.Grant{Epoch: 2, To: twId, From: hkId, FromEpoch: 1, Expires: time.Now().Add(time.Hour).Unix()}}
	if code, body := postAgent(t, jp, "ha/state", jp.token, 1, hkId, signStateWith(evil, forged)); code != http.StatusBadRequest {
		t.Fatalf("伪造的状态(不是钉住的主机签的)要拒: %d %s", code, body)
	}
	if jp.setting(ha.KeyGrant) != "" {
		t.Fatal("伪造的交接标记不能留下")
	}
	if code, body := postAgent(t, jp, "ha/state", jp.token, 2, twId, signStateWith(evil, forged)); code != http.StatusConflict {
		t.Fatalf("以更高纪元冒充主机:还联系得上主机,不换主: %d %s", code, body)
	}
	if code, _ := postAgent(t, jp, "apply", jp.token, 2, twId, []byte(`{}`)); code != http.StatusConflict {
		t.Fatal("冒充的主机推配置也被拒")
	}
	hk.sync()
	if hk.isNode() || jp.setting(ha.KeyEpoch) != "1" || jp.setting(ha.KeyMasterId) != strconv.Itoa(int(hkId)) {
		t.Fatalf("主机不降级,副机还跟着主机: node=%v jp=%s/%s", hk.isNode(), jp.setting(ha.KeyEpoch), jp.setting(ha.KeyMasterId))
	}
}

// E6:一台副机前面挂了乱报纪元的 409(纪元 2^64-1、不存在的主机)→ 主机纪元不变、照常管别的副机,这台的回复记成 bogus。
func TestHABogusEpochReplyE2E(t *testing.T) {
	clk := &haClock{}
	hk := newHAInst(t, "HK", false, clk)
	tw := newHAInst(t, "TW", true, clk)
	jp := newHAInst(t, "JP", true, clk)
	seedHAMaster(t, hk)
	twId := addNode(t, hk, tw, 2)
	jpId := addNode(t, hk, jp, 3)
	haUp(t, hk, 1, ha.ModeConfirm)
	jp.setFake409(`{"error":"x","code":"stale_epoch","epoch":18446744073709551615,"masterId":99,"leaseAgo":999}`)
	for i := 0; i < 3; i++ {
		hk.sync()
	}
	if hk.isNode() || hk.setting(ha.KeyEpoch) != "1" {
		t.Fatalf("主机纪元不变、不降级: node=%v epoch=%s", hk.isNode(), hk.setting(ha.KeyEpoch))
	}
	if st, ok := hk.R().Hub().StatusOf(twId); !ok || !st.Synced {
		t.Fatalf("别的副机照常同步: %+v", st)
	}
	if v := hk.HA().View(false); v.Replies[strconv.Itoa(int(jpId))].Kind != "bogus" {
		t.Fatalf("乱报纪元的回复记成 bogus: %+v", v.Replies)
	}
}

// E7:接任时组新库失败 → 纪元、角色都不变;原主机回来照常当主机(不会把那台备用当成"已接任的新主机"、降级给自己);
// 备用 2 不会被"更高纪元"挡住;去掉故障后再接任成功。
func TestHATakeoverBuildFailRetryE2E(t *testing.T) {
	clk := &haClock{}
	hk := newHAInst(t, "HK", false, clk)
	tw := newHAInst(t, "TW", true, clk)
	jp := newHAInst(t, "JP", true, clk)
	seedHAMaster(t, hk)
	addNode(t, hk, tw, 2)
	addNode(t, hk, jp, 3)
	haUp(t, hk, 2, ha.ModeConfirm)
	hk.setDown(true)
	clk.add(4 * time.Minute)
	tw.HA().Tick()
	block := tw.path + ".promote.tmp"
	if err := os.MkdirAll(filepath.Join(block, "x"), 0o700); err != nil {
		t.Fatal(err)
	}
	if err := tw.HA().Takeover(ha.TakeoverOpts{By: "test"}); err == nil {
		t.Fatal("组新库失败要报错")
	}
	if tw.setting(ha.KeyEpoch) != "1" || tw.setting(ha.KeyRole) != "" || tw.restarted() {
		t.Fatalf("失败后纪元、角色不变,不重启: epoch=%s role=%s", tw.setting(ha.KeyEpoch), tw.setting(ha.KeyRole))
	}
	clk.add(4 * time.Minute)
	jp.HA().Tick()
	if v := jp.HA().View(false); v.Node.Evidence != nil && v.Node.Evidence.Reason == "higherEpoch" {
		t.Fatal("备用 2 不能被接任失败的备用 1 挡住")
	}
	// 原主机回来:副机都还在纪元 1,照常认它
	hk.setDown(false)
	hk.sync()
	hk.sync()
	if hk.isNode() {
		t.Fatal("原主机回来不能降级")
	}
	if tw.setting(ha.KeyEpoch) != "1" || tw.setting(ha.KeyMasterId) != strconv.Itoa(int(hk.localId())) {
		t.Fatal("备用照常认原主机")
	}
	// 主机又不在了:去掉故障后再接任成功
	_ = os.RemoveAll(block)
	hk.setDown(true)
	clk.add(4 * time.Minute)
	tw.HA().Tick()
	if err := tw.HA().Takeover(ha.TakeoverOpts{By: "test"}); err != nil {
		t.Fatal(err)
	}
	tw.restart()
	if tw.isNode() || tw.setting(ha.KeyEpoch) != "2" {
		t.Fatal("再接任成功:纪元 2")
	}
}

// E8:换机还原围栏。① 原主机停着,它的备份在新机器上还原并启动 → 新机器以更高纪元接管;原主机再起来 → 变为副机(restoredElsewhere);
// ② 原主机还活着时新机器起来 → 新机器接管、原主机变为副机;③ 没开过容灾(纪元 0)的集群,两种顺序都是副机只认新机器。
func TestHARestoreFenceE2E(t *testing.T) {
	type pair struct {
		hk, tw *haInst
		twId   uint
	}
	setup := func(t *testing.T, enable bool) (pair, *haClock) {
		clk := &haClock{}
		hk := newHAInst(t, "HK", false, clk)
		tw := newHAInst(t, "TW", true, clk)
		seedHAMaster(t, hk)
		twId := addNode(t, hk, tw, 2)
		if enable {
			haUp(t, hk, 1, ha.ModeConfirm)
		} else {
			hk.sync()
			hk.sync()
		}
		return pair{hk, tw, twId}, clk
	}
	demotedAs := func(t *testing.T, in *haInst, reason string) {
		t.Helper()
		if !in.isNode() {
			t.Fatalf("%s 要变为副机", in.name)
		}
		var d ha.Demoted
		if err := json.Unmarshal([]byte(in.setting(ha.KeyDemoted)), &d); err != nil || d.Reason != reason {
			t.Fatalf("%s 变为副机的原因要是 %s: %+v", in.name, reason, d)
		}
	}
	for _, c := range []struct {
		name         string
		enable, stop bool
	}{{"开过容灾,原主机停着", true, true}, {"开过容灾,原主机活着", true, false}, {"纪元 0,原主机停着", false, true}, {"纪元 0,原主机活着", false, false}} {
		t.Run(c.name, func(t *testing.T) {
			p, clk := setup(t, c.enable)
			hkId := p.hk.localId()
			from := p.hk.setting(ha.KeyEpoch)
			if c.stop {
				p.hk.stop()
			}
			nw := newHAInst(t, "NW", false, clk)
			restoreInto(t, nw, p.hk)
			if !nw.boot.Fence || nw.boot.Reason != "restoredElsewhere" {
				t.Fatalf("换了机器的主机库要围栏: %+v", nw.boot)
			}
			if nw.startupCheck() {
				t.Fatal("新机器不能变为副机")
			}
			nw.sync()
			nw.sync()
			want, _ := strconv.Atoi(from)
			want++
			if p.tw.setting(ha.KeyEpoch) != strconv.Itoa(want) || p.tw.setting(ha.KeyMasterId) != strconv.Itoa(int(hkId)) {
				t.Fatalf("副机要跟新机器(纪元 %d): %s/%s", want, p.tw.setting(ha.KeyEpoch), p.tw.setting(ha.KeyMasterId))
			}
			if c.stop {
				p.hk.start()
				if !p.hk.startupCheck() {
					t.Fatal("原主机回来:启动自检就变为副机")
				}
			} else {
				p.hk.sync()
			}
			demotedAs(t, p.hk, "restoredElsewhere")
			if p.tw.setting(ha.KeyEpoch) != strconv.Itoa(want) {
				t.Fatal("原主机的推送不能把副机拉回去")
			}
			nw.sync()
			if st, ok := nw.R().Hub().StatusOf(p.twId); !ok || !st.Synced {
				t.Fatalf("副机在新机器那里照常同步: %+v", st)
			}
		})
	}
}

// E9:纪元 3 的主机还原了纪元 1 时的备份,或者容灾设置被清空(重置) → 不降级,纪元回到 3 以上,副机照常认它。
func TestHARollbackSelfHealE2E(t *testing.T) {
	clk := &haClock{}
	hk := newHAInst(t, "HK", false, clk)
	tw := newHAInst(t, "TW", true, clk)
	seedHAMaster(t, hk)
	twId := addNode(t, hk, tw, 2)
	haUp(t, hk, 1, ha.ModeConfirm)
	old := snapshotDB(t, hk) // 纪元 1 时的一份备份
	for _, want := range []string{"2", "3"} {
		if _, err := hk.HA().ReclaimNow(); err != nil {
			t.Fatal(err)
		}
		hk.sync()
		if tw.setting(ha.KeyEpoch) != want {
			t.Fatalf("副机跟着升到纪元 %s: %s", want, tw.setting(ha.KeyEpoch))
		}
	}
	restoreFile(t, hk, old) // 在主机上还原纪元 1 的备份(同一台机器)
	if !hk.boot.Fence || hk.boot.Reason != "rolledBack" || hk.boot.From != 3 || hk.boot.To != 4 {
		t.Fatalf("库纪元回退了:抬回 3、围栏到 4: %+v", hk.boot)
	}
	if hk.startupCheck() {
		t.Fatal("不能降级")
	}
	hk.sync()
	if hk.isNode() || hk.setting(ha.KeyEpoch) != "4" || tw.setting(ha.KeyEpoch) != "4" {
		t.Fatalf("纪元恢复,副机照常认它: hk=%s tw=%s", hk.setting(ha.KeyEpoch), tw.setting(ha.KeyEpoch))
	}
	// 容灾设置被清空(签名密钥也没了):照样抬回;副机在租约到期后认新的签名公钥
	hk.DB().Exec("DELETE FROM settings WHERE key LIKE 'ha%'")
	hk.restart()
	if !hk.boot.Fence || hk.boot.From != 4 || hk.boot.To != 5 {
		t.Fatalf("清空后重启:抬回 4、围栏到 5: %+v", hk.boot)
	}
	if hk.startupCheck() {
		t.Fatal("不能降级")
	}
	hk.sync()
	clk.add(3 * time.Minute)
	hk.sync()
	hk.sync()
	if hk.isNode() || tw.setting(ha.KeyEpoch) != "5" {
		t.Fatalf("副机在租约到期后跟上: hk node=%v tw=%s", hk.isNode(), tw.setting(ha.KeyEpoch))
	}
	if st, ok := hk.R().Hub().StatusOf(twId); !ok || !st.Synced {
		t.Fatalf("副机照常同步: %+v", st)
	}
}

// E17:只断了备用与主机(别的副机还连得上主机)→ 面板上不能接任;主机真的停了 → 确认提示出现后才能接任。
func TestHAPanelTakeoverNeedsPromptE2E(t *testing.T) {
	hkClk, twClk, jpClk := &haClock{}, &haClock{}, &haClock{}
	hk := newHAInst(t, "HK", false, hkClk)
	tw := newHAInst(t, "TW", true, twClk)
	jp := newHAInst(t, "JP", true, jpClk)
	seedHAMaster(t, hk)
	addNode(t, hk, tw, 2)
	addNode(t, hk, jp, 3)
	haUp(t, hk, 1, ha.ModeConfirm)
	twClk.add(4 * time.Minute)
	tw.HA().Tick()
	if err := tw.HA().Takeover(ha.TakeoverOpts{By: "panel"}); err == nil {
		t.Fatal("别的副机还联系得上主机:面板上不能接任")
	}
	if v := tw.HA().View(false); v.Node.Prompt != nil || v.Node.Evidence == nil || v.Node.Evidence.Reason != "peerSeesMaster" {
		t.Fatalf("不发确认、写明原因: %+v", v.Node.Evidence)
	}
	hk.setDown(true)
	jpClk.add(4 * time.Minute)
	twClk.add(time.Minute)
	tw.HA().Tick()
	if v := tw.HA().View(false); v.Node.Prompt == nil {
		t.Fatalf("主机真的停了:发确认: %+v", v.Node.Evidence)
	}
	if err := tw.HA().Takeover(ha.TakeoverOpts{By: "panel"}); err != nil {
		t.Fatal(err)
	}
	if !tw.restarted() {
		t.Fatal("接任要重启")
	}
}

// E18:两台备用都(强制)接任、落在同一纪元 → 最后只剩一台主机(先当上的;同时的比 id),另一台变为副机,副机都跟留下的那台。
func TestHASameEpochDoubleTakeoverE2E(t *testing.T) {
	clk := &haClock{}
	hk := newHAInst(t, "HK", false, clk)
	tw := newHAInst(t, "TW", true, clk)
	jp := newHAInst(t, "JP", true, clk)
	kr := newHAInst(t, "KR", true, clk)
	seedHAMaster(t, hk)
	twId := addNode(t, hk, tw, 2)
	addNode(t, hk, jp, 3)
	addNode(t, hk, kr, 4)
	haUp(t, hk, 2, ha.ModeConfirm)
	hk.setDown(true)
	clk.add(5 * time.Minute)
	for _, in := range []*haInst{tw, jp} {
		if err := in.HA().Takeover(ha.TakeoverOpts{Force: true, By: "cli"}); err != nil {
			t.Fatal(err)
		}
		in.restart()
		if in.isNode() || in.setting(ha.KeyEpoch) != "2" {
			t.Fatalf("%s 以纪元 2 当主机", in.name)
		}
	}
	for i := 0; i < 3; i++ {
		tw.sync()
		jp.sync()
	}
	if tw.isNode() || !jp.isNode() {
		t.Fatalf("同纪元两台主机:先当上的(同时则 id 小的 TW)留下: tw node=%v jp node=%v", tw.isNode(), jp.isNode())
	}
	var d ha.Demoted
	if err := json.Unmarshal([]byte(jp.setting(ha.KeyDemoted)), &d); err != nil || d.Reason != "sameEpoch" || d.MasterId != twId {
		t.Fatalf("JP 记下是同纪元让给 TW: %+v", d)
	}
	jp.restart()
	tw.sync()
	tw.sync()
	for _, in := range []*haInst{jp, kr} {
		if in.setting(ha.KeyEpoch) != "2" || in.setting(ha.KeyMasterId) != strconv.Itoa(int(twId)) {
			t.Fatalf("%s 要跟 TW: %s/%s", in.name, in.setting(ha.KeyEpoch), in.setting(ha.KeyMasterId))
		}
		if st, ok := tw.R().Hub().StatusOf(in.localId()); !ok || !st.Synced {
			t.Fatalf("%s 在 TW 那里同步上: %+v", in.name, st)
		}
	}
	if jp.setting(ha.KeyRole) != "" {
		t.Fatal("让出的 JP 被 TW 接上后是普通副机")
	}
}

// E19:主机挂了、备用接任,新主机不久也挂了,原主机这时回来 → 原主机不降级去跟那台不在的主机,而是以更高纪元把副机收回来。
func TestHAOriginalReturnsAfterNewMasterDiedE2E(t *testing.T) {
	clk := &haClock{}
	hk := newHAInst(t, "HK", false, clk)
	tw := newHAInst(t, "TW", true, clk)
	jp := newHAInst(t, "JP", true, clk)
	seedHAMaster(t, hk)
	addNode(t, hk, tw, 2)
	jpId := addNode(t, hk, jp, 3)
	hkId := hk.localId()
	haUp(t, hk, 1, ha.ModeConfirm)
	hk.setDown(true)
	clk.add(4 * time.Minute)
	tw.HA().Tick()
	if err := tw.HA().Takeover(ha.TakeoverOpts{By: "test"}); err != nil {
		t.Fatal(err)
	}
	tw.restart()
	tw.sync()
	tw.sync()
	if jp.setting(ha.KeyEpoch) != "2" {
		t.Fatal("前提:JP 跟着新主机")
	}
	// 新主机也挂了;过了一阵原主机回来
	tw.setDown(true)
	clk.add(3 * time.Minute)
	hk.stop()
	hk.setDown(false)
	hk.start()
	if hk.startupCheck() {
		t.Fatal("新主机已经不在了:原主机不能降级去跟它")
	}
	clk.add(3 * time.Minute)
	hk.sync()
	if hk.isNode() || hk.setting(ha.KeyEpoch) != "3" {
		t.Fatalf("以高于新主机的纪元收回: node=%v epoch=%s", hk.isNode(), hk.setting(ha.KeyEpoch))
	}
	hk.sync()
	if jp.setting(ha.KeyEpoch) != "3" || jp.setting(ha.KeyMasterId) != strconv.Itoa(int(hkId)) {
		t.Fatalf("JP 回到原主机: %s/%s", jp.setting(ha.KeyEpoch), jp.setting(ha.KeyMasterId))
	}
	if st, ok := hk.R().Hub().StatusOf(jpId); !ok || !st.Synced {
		t.Fatalf("JP 照常同步: %+v", st)
	}
}
