package reach

import (
	"context"
	"strings"
	"testing"
)

// groupsOf 造一份各组结果:cn 是三家运营商各自通了几个(共 4 个),hk 是香港通了几个(共 3 个)。
func groupsOf(cn [3]int, hk int) map[string]*Group {
	gs := map[string]*Group{}
	for i, k := range carrierGroups {
		g := &Group{Key: k, Total: 4, OK: cn[i]}
		switch {
		case g.OK == 0:
			g.State = StateDown
		case g.OK*2 >= g.Total:
			g.State = StateOK
		default:
			g.State = StateWeak
		}
		gs[k] = g
	}
	gs[GroupAbroad] = &Group{Key: GroupAbroad, Total: 3, OK: hk}
	return gs
}

// 审计 I5:外部节点常见的国内中转入口(大陆三网都通、香港不通)以前判成"境外也不通";UDP 协议拿去连 TCP 端口必然全失败,
// 也判成"境外也不通"。外部节点用单独的判定:大陆过半通就是通,香港不通只作提示。服务器巡检的判定不变。
func TestVerdictExtDomesticRelay(t *testing.T) {
	relay := groupsOf([3]int{4, 4, 4}, 0)
	if v := verdict(relay, nil, false); v != VerdictAbroadDown {
		t.Fatalf("服务器巡检的判定不该变: %s", v)
	}
	if v, note := verdictExt(relay, nil, false); v != VerdictOK || note != NoteHKDown {
		t.Fatalf("国内中转入口应判通并提示香港不通: %s %q", v, note)
	}
	for _, c := range []struct {
		name     string
		cn       [3]int
		hk       int
		icmp     *ICMPCheck
		icmpOnly bool
		verdict  string
		note     string
	}{
		{"三网都通", [3]int{4, 4, 4}, 3, nil, false, VerdictOK, ""},
		{"过半通但移动整家不通", [3]int{0, 4, 4}, 3, nil, false, VerdictOK, NoteCarrierDown},
		{"不到一半通", [3]int{1, 1, 0}, 3, nil, false, VerdictPartial, ""},
		{"大陆全不通、香港通", [3]int{0, 0, 0}, 3, nil, false, VerdictBlocked, ""},
		{"大陆全不通、ping 得通", [3]int{0, 0, 0}, 3, &ICMPCheck{CNOK: 5, CNTotal: 12, HKOK: 3, HKTotal: 3}, false, VerdictPortBlocked, ""},
		{"大陆全不通、ping 也不通", [3]int{0, 0, 0}, 3, &ICMPCheck{CNOK: 0, CNTotal: 12, HKOK: 3, HKTotal: 3}, false, VerdictIPBlocked, ""},
		{"两边都不通", [3]int{0, 0, 0}, 0, nil, false, VerdictAbroadDown, NoteBothDown},
		{"UDP 只 ping:通", [3]int{4, 4, 4}, 3, nil, true, VerdictOK, NoteUDPPing},
		{"UDP 只 ping:大陆不通", [3]int{0, 0, 0}, 3, nil, true, VerdictBlocked, NoteUDPPing},
	} {
		v, note := verdictExt(groupsOf(c.cn, c.hk), c.icmp, c.icmpOnly)
		if v != c.verdict || note != c.note {
			t.Errorf("%s: 得到 %s %q,应为 %s %q", c.name, v, note, c.verdict, c.note)
		}
	}
	none := groupsOf([3]int{0, 0, 0}, 0)
	for _, k := range carrierGroups {
		none[k].State, none[k].Total = StateNone, 0
	}
	if v, _ := verdictExt(none, nil, false); v != VerdictNoProbes {
		t.Fatalf("没有大陆测点: %s", v)
	}
}

// UDP 协议(hysteria2 / tuic)只发 ping,不再拿 UDP 端口当 TCP 去连;结果里注明。TCP 协议照样连端口。
func TestCheckExtUDPPingOnly(t *testing.T) {
	h := newHarness(t)
	res := h.svc.CheckExt(context.Background(), "hy2", "203.0.113.9", 443, true)
	h.api.mu.Lock()
	last := h.api.requests[len(h.api.requests)-1]
	n := len(h.api.requests)
	h.api.mu.Unlock()
	if n != 1 || last.Options.Protocol != "" || last.Options.Port != 0 {
		t.Fatalf("UDP 协议只该 ping 一次: %d %+v", n, last.Options)
	}
	if res.Verdict != VerdictOK || !strings.Contains(res.Note, NoteUDPPing) || res.Port != 0 {
		t.Fatalf("结果应注明只 ping 了: %+v", res)
	}
	h.api.set(map[string]bool{"cm": true, "ct": true, "cu": true, "hk": false}, map[string]bool{"cm": true, "ct": true, "cu": true, "hk": false})
	res = h.svc.CheckExt(context.Background(), "relay", "203.0.113.9", 443, false)
	h.api.mu.Lock()
	last = h.api.requests[len(h.api.requests)-1]
	h.api.mu.Unlock()
	if last.Options.Protocol != "TCP" || last.Options.Port != 443 {
		t.Fatalf("TCP 协议应连端口: %+v", last.Options)
	}
	if res.Verdict != VerdictOK || res.Note != NoteHKDown {
		t.Fatalf("国内中转入口应判通并提示香港不通: %+v", res)
	}
}

// 外部节点的检测和服务器检测互斥(共用测点额度),外部节点的检测同时也只跑一个。
func TestBeginExtExclusive(t *testing.T) {
	h := newHarness(t)
	release, err := h.svc.BeginExt()
	if err != nil {
		t.Fatal(err)
	}
	if _, err := h.svc.BeginExt(); err != ErrBusy {
		t.Fatalf("第二个外部节点检测应等: %v", err)
	}
	if err := h.svc.Run(nil); err != ErrBusy {
		t.Fatalf("服务器检测也应等: %v", err)
	}
	release()
	if err := h.svc.begin(nil); err != nil {
		t.Fatalf("结束后服务器检测可以跑: %v", err)
	}
	if _, err := h.svc.BeginExt(); err != ErrBusy {
		t.Fatalf("服务器检测在跑时外部节点检测应等: %v", err)
	}
	h.svc.finish(false)
}

// 手动测外部节点要给下一轮服务器巡检留出额度,别把巡检挤到"额度用完"、漏报被墙。
func TestCanCheckExtReservesQuota(t *testing.T) {
	h := newHarness(t)
	_ = h.svc.CheckExt(context.Background(), "x", "203.0.113.9", 443, false) // 读到一次额度
	q := h.svc.c.Quota()
	h.svc.mu.Lock()
	cost := h.svc.perCheck
	servers := q.Remaining/cost + 1 // 给这么多台服务器的巡检留额度,就不够再测一个外部节点了
	for i := 1; i <= servers; i++ {
		h.svc.latest[uint(i)] = Result{NodeId: uint(i), Verdict: VerdictOK}
	}
	h.svc.mu.Unlock()
	if q.At == 0 || cost <= 0 || q.Remaining < cost {
		t.Fatalf("前置条件不对: %+v cost=%d", q, cost)
	}
	if err := h.svc.CanCheck(1); err != nil {
		t.Fatalf("不算预留时够测一个: %v", err)
	}
	if err := h.svc.CanCheckExt(1); err == nil || !strings.Contains(err.Error(), "定时巡检") {
		t.Fatalf("要给 %d 台服务器的巡检留 %d 个,剩 %d,应拒绝: %v", servers, servers*cost, q.Remaining, err)
	}
	h.mu.Lock()
	h.settings["reachAuto"] = "false"
	h.mu.Unlock()
	if err := h.svc.CanCheckExt(1); err != nil {
		t.Fatalf("定时巡检关着时不用预留: %v", err)
	}
}
