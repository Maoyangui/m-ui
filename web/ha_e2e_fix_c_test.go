//go:build !race

package web

// 修复者 C 的多实例端到端测试(与 ha_e2e_test.go 同一底座)。

import (
	"net/http"
	"net/http/httptest"
	"strconv"
	"strings"
	"testing"
	"time"

	"github.com/Maoyangui/m-ui/database/model"
	"github.com/Maoyangui/m-ui/ha"
)

// E16(#57 #22):备用离线时被删除:主机记下待办、每轮重试;它回来后删掉副本(并离开集群、纪元清零),
// 之后能被别的主机重新加入。
func TestHADeleteStandbyOfflineE2E(t *testing.T) {
	clk := &haClock{}
	hk := newHAInst(t, "HK", false, clk)
	tw := newHAInst(t, "TW", true, clk)
	jp := newHAInst(t, "JP", true, clk)
	seedHAMaster(t, hk)
	twId := addNode(t, hk, tw, 2)
	addNode(t, hk, jp, 3)
	hk.sync()
	if err := hk.HA().SaveConfig(ha.ConfigInput{Config: ha.Config{Mode: ha.ModeConfirm, Standbys: 2, ReplicaMinutes: 5, ConfirmMinutes: 3,
		AutoMinutes: 10, RankStepMinutes: 3}}); err != nil {
		t.Fatal(err)
	}
	hk.sync()
	hk.sync()
	if err := hk.HA().ReplicateNow(); err != nil {
		t.Fatal(err)
	}
	if v := tw.HA().View(false); v.Node == nil || v.Node.Replica == nil {
		t.Fatal("前提:TW 是备用、手里有副本")
	}
	// TW 离线时在主机面板上删掉它
	tw.setDown(true)
	req := httptest.NewRequest(http.MethodDelete, "http://x/app/api/nodes/"+strconv.FormatUint(uint64(twId), 10), nil)
	w := httptest.NewRecorder()
	hk.srv.handleNodeItem(w, req)
	if w.Code != http.StatusOK {
		t.Fatalf("删服务器: %d %s", w.Code, w.Body.String())
	}
	deadline := time.Now().Add(10 * time.Second)
	for {
		p := hk.HA().DropPending()
		if len(p) == 1 && p[0].Tries > 0 {
			if !p[0].Leave || p[0].Name != "TW" {
				t.Fatalf("删除要记下删副本 + 离开集群: %+v", p)
			}
			break
		}
		if time.Now().After(deadline) {
			t.Fatalf("连不上时要记下待办: %+v", p)
		}
		time.Sleep(20 * time.Millisecond)
	}
	if v := hk.HA().View(false); len(v.DropPend) != 1 {
		t.Fatalf("主机面板要能看到待删副本的副机: %+v", v.DropPend)
	}
	// 主机做了别的事、过了几分钟,TW 回来了
	tw.setDown(false)
	clk.add(5 * time.Minute)
	hk.HA().RetryDrops()
	if p := hk.HA().DropPending(); len(p) != 0 {
		t.Fatalf("TW 回来后待办要清掉: %+v", p)
	}
	if v := tw.HA().View(false); v.Node != nil && v.Node.Replica != nil {
		t.Fatal("TW 回来后要删掉手里的副本")
	}
	// 离开集群:纪元清零(agent/ha/leave 归 A;没合入时节点回 404,这里只记一笔)
	if e := tw.setting(ha.KeyEpoch); e != "" && e != "0" {
		err := hk.R().Hub().Send(model.Node{Id: twId, Name: "TW", ApiUrl: tw.url(), Token: tw.token, Insecure: true}, http.MethodPost,
			"ha/leave", nil, 0, nil, 10*time.Second, nil)
		if err != nil && strings.HasPrefix(err.Error(), "HTTP 404") {
			t.Log("agent/ha/leave 还没合入(A 的接口):纪元清零这一步集成后由本测试覆盖")
			return
		}
		t.Fatalf("删除的备用回来后要离开集群(纪元清零),实际纪元 %s: %v", e, err)
	}
	// 能被别的(没开容灾的)主机重新加入:不带纪元的请求不再被当成旧主机
	sg := newHAInst(t, "SG", false, clk)
	addNode(t, sg, tw, 2)
	sg.sync()
	var row model.Node
	sg.DB().Where("name = ?", "TW").First(&row)
	if st, ok := sg.R().Hub().StatusOf(row.Id); !ok || st.LastSeen == 0 {
		t.Fatalf("离开集群的副机要能被别的主机重新加入: %+v", st)
	}
}
