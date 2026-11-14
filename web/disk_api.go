package web

import (
	"encoding/json"
	"errors"
	"net/http"

	"github.com/Maoyangui/m-ui/database/model"
	"github.com/Maoyangui/m-ui/hub"
	"github.com/Maoyangui/m-ui/logger"
	"github.com/Maoyangui/m-ui/ops"
)

// diskCleanState 「清理磁盘」的进度(服务器页的进度框轮询它):这台的运维任务状态 + 磁盘占用。
// 运维任务一台同时只跑一个,别的任务在跑时 Current 是那个任务的名字。
type diskCleanState struct {
	Running bool            `json:"running"`
	Current string          `json:"current,omitempty"`
	Log     []string        `json:"log"`
	Last    interface{}     `json:"last,omitempty"`
	Disk    *model.DiskStat `json:"disk,omitempty"`
}

func (s *Server) localDiskCleanState() diskCleanState {
	st := s.ops.Status()
	return diskCleanState{Running: st.Running, Current: st.Current, Log: st.Log, Last: st.Last, Disk: s.run.DiskStat()}
}

// startDiskClean 在本机开始清理磁盘(运维页、服务器页本机那一行、主机派给副机的都走这里)。
// opsAgent:一并停用谷歌云 Ops Agent(管理员勾了才停)。
func (s *Server) startDiskClean(opsAgent bool) error {
	p := ops.Params{JournalMB: s.settingInt("opsJournalMb", ops.DefaultJournalMB), OpsAgent: opsAgent}
	return s.ops.Start("disk-clean", p, func(ok bool, err error) {
		invalidateOpsInfo()
		s.run.ForgetDiskStat() // 清完马上按新的占用显示、判断告警
		if ok {
			logger.Info("运维任务完成: disk-clean")
		} else {
			logger.Warning("运维任务失败: disk-clean ", err)
		}
		s.run.Notifier().Event("tgOnCore", opsResultText("disk-clean", ok, err))
	})
}

type diskCleanReq struct {
	OpsAgent bool `json:"opsAgent"`
}

// handleAgentDiskClean 副机:GET agent/disk-clean 看进度;POST {opsAgent} 开始清理(主机服务器页派过来)。
func (s *Server) handleAgentDiskClean(w http.ResponseWriter, r *http.Request) {
	switch r.Method {
	case http.MethodGet:
		writeJSON(w, http.StatusOK, s.localDiskCleanState())
	case http.MethodPost:
		var req diskCleanReq
		_ = json.NewDecoder(r.Body).Decode(&req)
		if err := s.startDiskClean(req.OpsAgent); err != nil {
			badRequest(w, err)
			return
		}
		writeJSON(w, http.StatusOK, s.localDiskCleanState())
	default:
		writeJSON(w, http.StatusMethodNotAllowed, map[string]string{"error": "方法不允许"})
	}
}

// nodeDiskClean 主机服务器页:GET / POST api/nodes/{id}/disk-clean。本机就地做,副机经 agent/disk-clean 转过去。
func (s *Server) nodeDiskClean(w http.ResponseWriter, r *http.Request, node model.Node) {
	start := r.Method == http.MethodPost
	if !start && r.Method != http.MethodGet {
		writeJSON(w, http.StatusMethodNotAllowed, map[string]string{"error": "方法不允许"})
		return
	}
	var req diskCleanReq
	if start {
		_ = json.NewDecoder(r.Body).Decode(&req)
	}
	if node.IsLocal {
		if start {
			if err := s.startDiskClean(req.OpsAgent); err != nil {
				badRequest(w, err)
				return
			}
			s.audit(r, "node", "disk-clean", map[string]interface{}{"name": node.Name, "opsAgent": req.OpsAgent})
		}
		writeJSON(w, http.StatusOK, s.localDiskCleanState())
		return
	}
	if !node.Enabled {
		badRequest(w, errors.New("这台服务器已停用"))
		return
	}
	out, err := s.run.Hub().DiskClean(node, start, req.OpsAgent)
	if err != nil {
		if hub.IsNotFound(err) {
			err = errors.New("这台副机的版本太旧,不支持远程清理磁盘:先升级它,或登录它的面板在运维页清理")
		}
		badRequest(w, err)
		return
	}
	if start {
		s.audit(r, "node", "disk-clean", map[string]interface{}{"name": node.Name, "opsAgent": req.OpsAgent})
	}
	w.Header().Set("Content-Type", "application/json; charset=utf-8")
	_, _ = w.Write(out)
}
