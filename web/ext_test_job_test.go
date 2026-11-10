package web

import (
	"context"
	"net/http"
	"strings"
	"testing"
	"time"

	"github.com/Maoyangui/m-ui/database/model"
	"github.com/Maoyangui/m-ui/ext"
)

// 外部节点测速:失联的副机直接记「离线没测」,不去等超时;同一条外部节点同时只跑一个任务(审计 MB31)。
func TestExtTestSkipsOfflineAndSingleJob(t *testing.T) {
	s := extNodesServer(t)
	s.db.Model(&model.Node{}).Where("is_local = ?", true).Update("enabled", false) // 只留副机,不碰本机数据面
	remote := model.Node{Name: "台湾", ApiUrl: "http://127.0.0.1:1/ad/", Enabled: true}
	s.db.Create(&remote)
	var e model.ExtNode
	s.db.First(&e, 1)
	it := ext.ItemsOf(e)
	infos := ext.Details(it)

	id, err := s.startExtTestWith(e, it, infos, map[uint]bool{remote.Id: true})
	if err != nil {
		t.Fatal(err)
	}
	extJobs.Lock()
	job := extJobs.m[id]
	extJobs.Unlock()
	st := job.snapshot()
	if st["done"] != true {
		t.Fatalf("全是离线的服务器,任务应立刻结束: %v", st)
	}
	for _, row := range job.results {
		if c := row[remote.Id]; c.State != cellSkip || !strings.Contains(c.Error, "离线") {
			t.Fatalf("离线的服务器应直接记没测: %+v", c)
		}
	}

	extJobs.Lock()
	extJobs.m["running"] = &extTestJob{extId: e.Id, started: time.Now()}
	extJobs.Unlock()
	defer func() { extJobs.Lock(); delete(extJobs.m, "running"); extJobs.Unlock() }()
	if _, err := s.startExtTestWith(e, it, infos, nil); err == nil || !strings.Contains(err.Error(), "正在测速") {
		t.Fatalf("同一条外部节点已有任务在跑时应拒绝: %v", err)
	}
}

// 停止测速:还在等的格子记"已停止",任务结束;在途的结果回来不再改、不重复计数。
func TestExtTestStop(t *testing.T) {
	s := extNodesServer(t)
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	job := &extTestJob{extId: 1, started: time.Now(), total: 2, cancel: cancel, results: map[string]map[uint]*extCell{
		"k1": {1: {State: cellOK, DelayMs: 80}},
		"k2": {1: {State: cellPending}},
	}}
	job.finished = 1
	extJobs.Lock()
	extJobs.m["stopme"] = job
	extJobs.Unlock()
	defer func() { extJobs.Lock(); delete(extJobs.m, "stopme"); extJobs.Unlock() }()
	code, st := extCall(t, s, http.MethodDelete, "1/nodes/test/stopme", "")
	if code != http.StatusOK || st["done"] != true {
		t.Fatalf("停止后任务应结束: %d %v", code, st)
	}
	if ctx.Err() == nil {
		t.Fatal("停止应取消在途的测速")
	}
	res := st["results"].(map[string]interface{})
	if c := res["k2"].(map[string]interface{})["1"].(map[string]interface{}); c["state"] != cellCancelled {
		t.Fatalf("没测的格子应记已停止: %v", c)
	}
	if c := res["k1"].(map[string]interface{})["1"].(map[string]interface{}); c["state"] != cellOK {
		t.Fatalf("测完的格子不该被改: %v", c)
	}
	job.set("k2", 1, extCell{State: cellOK, DelayMs: 1})
	if job.results["k2"][1].State != cellCancelled || job.finished != 2 {
		t.Fatalf("停止后回来的结果不该再记: %+v finished=%d", job.results["k2"][1], job.finished)
	}
	// 别的外部节点拿这个任务号查不到
	s.db.Create(&model.ExtNode{Name: "机场B", Type: "sub", Value: "https://example.com/b", Enabled: true})
	if code, out := extCall(t, s, http.MethodGet, "2/nodes/test/stopme", ""); code != http.StatusNotFound || !strings.Contains(out["error"].(string), "任务") {
		t.Fatalf("别的外部节点的任务应 404: %d %v", code, out)
	}
}
