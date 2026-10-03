package web

import (
	"strings"
	"testing"
	"time"

	"github.com/Maoyangui/m-ui/database/model"
)

// 外部节点测速:失联的副机直接记「离线没测」,不去等 25 秒超时;同一条外部节点同时只跑一个任务(审计 MB31)。
func TestExtTestSkipsOfflineAndSingleJob(t *testing.T) {
	s := extNodesServer(t)
	s.db.Model(&model.Node{}).Where("is_local = ?", true).Update("enabled", false) // 只留副机,不碰本机数据面
	remote := model.Node{Name: "台湾", ApiUrl: "http://127.0.0.1:1/ad/", Enabled: true}
	s.db.Create(&remote)
	var e model.ExtNode
	s.db.First(&e, 1)

	id, err := s.startExtTestWith(e, nil, map[uint]bool{remote.Id: true})
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
		if c := row[remote.Id]; c.State != "fail" || !strings.Contains(c.Error, "离线") {
			t.Fatalf("离线的服务器应直接记失败: %+v", c)
		}
	}

	extJobs.Lock()
	extJobs.m["running"] = &extTestJob{extId: e.Id, started: time.Now()}
	extJobs.Unlock()
	defer func() { extJobs.Lock(); delete(extJobs.m, "running"); extJobs.Unlock() }()
	if _, err := s.startExtTestWith(e, nil, nil); err == nil || !strings.Contains(err.Error(), "正在测速") {
		t.Fatalf("同一条外部节点已有任务在跑时应拒绝: %v", err)
	}
}
