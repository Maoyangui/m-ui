package web

import (
	"context"
	"encoding/json"
	"errors"
	"net/http"
	"strconv"
	"strings"
	"sync"
	"time"

	"github.com/Maoyangui/m-ui/database/model"
	"github.com/Maoyangui/m-ui/logger"
	"github.com/Maoyangui/m-ui/ops"
)

func (s *Server) warpPort() int { return s.settingInt("warpPort", 40000) }

// 系统信息要跑 warp-cli / journalctl / timedatectl 并经 SOCKS5 外连一次 Cloudflare 验证出口,
// 页面每次刷新都来一遍太重:缓存 30 秒,点"刷新"或任务跑完才重新采集。
var (
	opsMu     sync.Mutex
	opsAt     time.Time
	opsPort   int
	opsCached ops.Info
)

func (s *Server) opsInfo(ctx context.Context, fresh bool) ops.Info {
	port := s.warpPort()
	opsMu.Lock()
	if !fresh && port == opsPort && time.Since(opsAt) < 30*time.Second {
		info := opsCached
		opsMu.Unlock()
		return info
	}
	opsMu.Unlock()
	info := ops.Collect(ctx, port, s.run.DataDir())
	opsMu.Lock()
	opsAt, opsPort, opsCached = time.Now(), port, info
	opsMu.Unlock()
	return info
}

func invalidateOpsInfo() {
	opsMu.Lock()
	opsAt = time.Time{}
	opsMu.Unlock()
}

// handleOps GET /ops:系统信息 + 任务列表 + 任务状态
func (s *Server) handleOps(w http.ResponseWriter, r *http.Request) {
	if r.Method != http.MethodGet {
		writeJSON(w, http.StatusMethodNotAllowed, map[string]string{"error": "方法不允许"})
		return
	}
	ctx, cancel := context.WithTimeout(r.Context(), 15*time.Second)
	defer cancel()
	var warpUp model.Upstream
	hasWarp := s.db.Where("name = ?", "warp").First(&warpUp).Error == nil
	sysctl := s.setting("opsSysctl")
	if strings.TrimSpace(sysctl) == "" {
		sysctl = ops.DefaultSysctl
	}
	writeJSON(w, http.StatusOK, map[string]interface{}{
		"info":         s.opsInfo(ctx, r.URL.Query().Get("fresh") == "1"),
		"tasks":        ops.Tasks,
		"status":       s.ops.Status(),
		"warpPort":     s.warpPort(),
		"warpUpstream": hasWarp,
		"params": map[string]interface{}{
			"swapGb": s.settingInt("opsSwapGb", 2), "noFile": s.settingInt("opsNoFile", ops.DefaultNoFile),
			"sysctl": sysctl, "defaultSysctl": ops.DefaultSysctl, "defaultNoFile": ops.DefaultNoFile,
			"journalMb": s.settingInt("opsJournalMb", ops.DefaultJournalMB), "journalDays": s.settingInt("opsJournalDays", ops.DefaultJournalDays),
		},
	})
}

func (s *Server) handleOpsSub(w http.ResponseWriter, r *http.Request) {
	action := strings.TrimPrefix(r.URL.Path, innerBase+"api/ops/")
	switch action {
	case "status":
		writeJSON(w, http.StatusOK, s.ops.Status())
	case "run":
		if r.Method != http.MethodPost {
			break
		}
		var req struct {
			Task        string `json:"task"`
			Port        int    `json:"port"`
			SwapGB      int    `json:"swapGb"`
			NoFile      int    `json:"noFile"`
			Sysctl      string `json:"sysctl"`
			JournalMB   int    `json:"journalMb"`
			JournalDays int    `json:"journalDays"`
		}
		if err := json.NewDecoder(r.Body).Decode(&req); err != nil {
			badRequest(w, err)
			return
		}
		if req.Port > 0 {
			s.run.SetSetting("warpPort", itoa(req.Port))
		}
		// 记住用户的参数,下次打开页面沿用
		if req.SwapGB > 0 {
			s.run.SetSetting("opsSwapGb", itoa(req.SwapGB))
		}
		if req.NoFile > 0 {
			s.run.SetSetting("opsNoFile", itoa(req.NoFile))
		}
		if req.JournalMB > 0 {
			s.run.SetSetting("opsJournalMb", itoa(req.JournalMB))
		}
		if req.JournalDays > 0 {
			s.run.SetSetting("opsJournalDays", itoa(req.JournalDays))
		}
		if strings.TrimSpace(req.Sysctl) != "" {
			if _, err := ops.ValidateSysctl(req.Sysctl); err != nil {
				badRequest(w, err)
				return
			}
			s.run.SetSetting("opsSysctl", req.Sysctl)
		}
		task := req.Task
		jm, jd := req.JournalMB, req.JournalDays
		if jm <= 0 {
			jm = ops.DefaultJournalMB
		}
		if jd <= 0 {
			jd = ops.DefaultJournalDays
		}
		err := s.ops.Start(task, ops.Params{Port: s.warpPort(), SwapGB: req.SwapGB, NoFile: req.NoFile, Sysctl: req.Sysctl,
			JournalMB: jm, JournalDays: jd}, func(ok bool, err error) {
			invalidateOpsInfo() // 任务改了系统状态,下次打开页面要重新采集
			if ok {
				logger.Info("运维任务完成: ", task)
				if task == "warp-enable" {
					s.ensureWarpUpstream()
				}
			} else {
				logger.Warning("运维任务失败: ", task, " ", err)
			}
			s.run.Notifier().Event("tgOnCore", opsResultText(task, ok, err))
		})
		if err != nil {
			badRequest(w, err)
			return
		}
		s.audit(r, "ops", "run:"+task, map[string]interface{}{"port": s.warpPort(), "swapGb": req.SwapGB})
		writeJSON(w, http.StatusOK, map[string]string{"ok": "1"})
	case "cancel":
		if r.Method != http.MethodPost {
			break
		}
		s.ops.Cancel()
		s.audit(r, "ops", "cancel", nil)
		writeJSON(w, http.StatusOK, map[string]string{"ok": "1"})
	case "warp-upstream":
		if r.Method != http.MethodPost {
			break
		}
		created, err := s.ensureWarpUpstream()
		if err != nil {
			badRequest(w, err)
			return
		}
		s.audit(r, "ops", "warp-upstream", created)
		writeJSON(w, http.StatusOK, map[string]interface{}{"ok": "1", "created": created})
	case "warp-check":
		ctx, cancel := context.WithTimeout(r.Context(), 12*time.Second)
		defer cancel()
		state, ip, loc, colo := ops.CheckExit(ctx, s.warpPort())
		writeJSON(w, http.StatusOK, map[string]string{"exit": state, "ip": ip, "loc": loc, "colo": colo})
	default:
		http.NotFound(w, r)
	}
}

// ensureWarpUpstream 确保存在名为 warp 的 socks 上游指向 127.0.0.1:<warpPort>。
func (s *Server) ensureWarpUpstream() (bool, error) {
	var up model.Upstream
	if err := s.db.Where("name = ?", "warp").First(&up).Error; err == nil {
		return false, nil
	}
	opts, _ := json.Marshal(map[string]interface{}{"server": "127.0.0.1", "server_port": s.warpPort()})
	up = model.Upstream{Name: "warp", Type: "socks", Options: opts}
	if err := s.db.Create(&up).Error; err != nil {
		return false, err
	}
	if err := s.run.ReloadAll(); err != nil {
		return true, errors.New("上游已创建,但数据面重载失败: " + err.Error())
	}
	return true, nil
}

func opsResultText(task string, ok bool, err error) string {
	if ok {
		return "🟢 <b>运维任务完成</b>:" + task
	}
	msg := ""
	if err != nil {
		msg = "\n" + err.Error()
	}
	return "🔴 <b>运维任务失败</b>:" + task + msg
}

func itoa(n int) string { return strconv.Itoa(n) }
