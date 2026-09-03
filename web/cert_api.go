package web

import (
	"context"
	"encoding/json"
	"errors"
	"io"
	"net/http"
	"os"
	"path/filepath"
	"strings"
	"time"

	"github.com/Maoyangui/m-ui/acme"
)

// ---- 证书 ----

// handleCert GET /cert:证书信息 + 签发设置 + 进度
func (s *Server) handleCert(w http.ResponseWriter, r *http.Request) {
	if r.Method != http.MethodGet {
		writeJSON(w, http.StatusMethodNotAllowed, map[string]string{"error": "方法不允许"})
		return
	}
	certFile, _ := s.run.DataPlaneCert()
	writeJSON(w, http.StatusOK, map[string]interface{}{
		"info":       s.run.CertInfo(),
		"status":     s.run.CertStatus(),
		"source":     s.run.CertSource(),
		"applyPanel": certFile != "" && s.setting("webCertFile") == certFile,
		"applySub":   certFile != "" && s.setting("subCertFile") == certFile,
		"certPath":   certFile,
		// 面板 / 订阅实际引用的证书路径:与数据面不同则说明设置页里手填了另一张证书
		"panelCert": s.setting("webCertFile"),
		"subCert":   s.setting("subCertFile"),
		"settings": map[string]interface{}{
			"acmeDomain": s.setting("acmeDomain"), "acmeEmail": s.setting("acmeEmail"), "acmeMethod": s.setting("acmeMethod"),
			"hasCfToken": s.setting("acmeCfToken") != "", "acmeStaging": s.setting("acmeStaging") == "true",
			"acmeAutoRenew":  s.setting("acmeAutoRenew") != "false",
			"acmeApplyPanel": s.setting("acmeApplyPanel") != "false", "acmeApplySub": s.setting("acmeApplySub") != "false",
			"webDomain": s.setting("webDomain"), "certFile": s.setting("certFile"), "keyFile": s.setting("keyFile"),
		},
		"panelTLS": s.setting("webCertFile") != "",
		"subTLS":   s.setting("subCertFile") != "",
		"dataDir":  s.run.DataDir(),
	})
}

func (s *Server) handleCertSub(w http.ResponseWriter, r *http.Request) {
	action := strings.TrimPrefix(r.URL.Path, innerBase+"api/cert/")
	switch action {
	case "status":
		writeJSON(w, http.StatusOK, s.run.CertStatus())
	case "precheck":
		var req struct{ Domain string }
		json.NewDecoder(r.Body).Decode(&req)
		req.Domain = strings.TrimSpace(strings.ToLower(req.Domain))
		if req.Domain == "" {
			badRequest(w, errors.New("请填写域名"))
			return
		}
		ctx, cancel := context.WithTimeout(r.Context(), 20*time.Second)
		defer cancel()
		writeJSON(w, http.StatusOK, acme.Precheck(ctx, req.Domain))
	case "issue":
		if r.Method != http.MethodPost {
			break
		}
		var req struct {
			Domain, Email, Method, CfToken           string
			Staging, AutoRenew, ApplyPanel, ApplySub bool
		}
		if err := json.NewDecoder(r.Body).Decode(&req); err != nil {
			badRequest(w, err)
			return
		}
		req.Domain = strings.TrimSpace(strings.ToLower(req.Domain))
		if req.Domain == "" {
			badRequest(w, errors.New("请填写域名"))
			return
		}
		if req.Method != "cloudflare" {
			req.Method = "http"
		}
		s.saveSettings(map[string]string{
			"acmeDomain": req.Domain, "acmeEmail": strings.TrimSpace(req.Email), "acmeMethod": req.Method,
			"acmeStaging": boolStr(req.Staging), "acmeAutoRenew": boolStr(req.AutoRenew),
			"acmeApplyPanel": boolStr(req.ApplyPanel), "acmeApplySub": boolStr(req.ApplySub),
		})
		if strings.TrimSpace(req.CfToken) != "" { // 留空保留原 Token
			s.saveSettings(map[string]string{"acmeCfToken": strings.TrimSpace(req.CfToken)})
		}
		if err := s.run.IssueCert(); err != nil {
			badRequest(w, err)
			return
		}
		s.audit(r, "cert", "issue", req.Domain)
		writeJSON(w, http.StatusOK, map[string]string{"ok": "1"})
	case "selfsign":
		if r.Method != http.MethodPost {
			break
		}
		var req struct {
			Hosts                string
			ApplyPanel, ApplySub bool
		}
		json.NewDecoder(r.Body).Decode(&req)
		var hosts []string
		for _, h := range strings.Split(req.Hosts, ",") {
			if h = strings.TrimSpace(h); h != "" {
				hosts = append(hosts, h)
			}
		}
		if err := s.run.SelfSign(hosts, req.ApplyPanel, req.ApplySub); err != nil {
			badRequest(w, err)
			return
		}
		s.saveSettings(map[string]string{"acmeApplyPanel": boolStr(req.ApplyPanel), "acmeApplySub": boolStr(req.ApplySub)})
		s.audit(r, "cert", "selfsign", hosts)
		writeJSON(w, http.StatusOK, map[string]string{"ok": "1"})
	case "external":
		if r.Method != http.MethodPost {
			break
		}
		var req struct {
			CertFile, KeyFile    string
			ApplyPanel, ApplySub bool
		}
		if err := json.NewDecoder(r.Body).Decode(&req); err != nil {
			badRequest(w, err)
			return
		}
		if err := s.run.UseExternalCert(req.CertFile, req.KeyFile, req.ApplyPanel, req.ApplySub); err != nil {
			badRequest(w, err)
			return
		}
		s.saveSettings(map[string]string{"acmeApplyPanel": boolStr(req.ApplyPanel), "acmeApplySub": boolStr(req.ApplySub)})
		s.audit(r, "cert", "external", req.CertFile)
		writeJSON(w, http.StatusOK, map[string]string{"ok": "1"})
	case "apply":
		if r.Method != http.MethodPost {
			break
		}
		var req struct{ ApplyPanel, ApplySub bool }
		if err := json.NewDecoder(r.Body).Decode(&req); err != nil {
			badRequest(w, err)
			return
		}
		if err := s.run.ApplyCertTargets(req.ApplyPanel, req.ApplySub); err != nil {
			badRequest(w, err)
			return
		}
		s.audit(r, "cert", "apply", map[string]bool{"panel": req.ApplyPanel, "sub": req.ApplySub})
		writeJSON(w, http.StatusOK, map[string]string{"ok": "1"})
	default:
		http.NotFound(w, r)
		return
	}
}

func boolStr(b bool) string {
	if b {
		return "true"
	}
	return "false"
}

func (s *Server) saveSettings(kv map[string]string) {
	for k, v := range kv {
		s.run.SetSetting(k, v)
	}
}

// ---- 备份 ----

// handleBackup GET /backup:直接下载一份新备份
func (s *Server) handleBackup(w http.ResponseWriter, r *http.Request) {
	if r.Method != http.MethodGet {
		writeJSON(w, http.StatusMethodNotAllowed, map[string]string{"error": "方法不允许"})
		return
	}
	name := "m-ui-" + time.Now().Format("20060102-150405") + ".zip"
	w.Header().Set("Content-Type", "application/zip")
	w.Header().Set("Content-Disposition", "attachment; filename="+name)
	if err := s.run.WriteBackup(w); err != nil {
		badRequest(w, err)
		return
	}
	s.audit(r, "backup", "download", name)
}

func (s *Server) handleBackupSub(w http.ResponseWriter, r *http.Request) {
	action := strings.TrimPrefix(r.URL.Path, innerBase+"api/backup/")
	switch action {
	case "list":
		writeJSON(w, http.StatusOK, map[string]interface{}{
			"files": s.run.ListBackups(), "dir": s.run.BackupDir(),
			"backupHour": s.setting("backupHour"), "backupKeep": s.setting("backupKeep"),
			"backupTelegram": s.setting("backupTelegram") == "true",
			"restorePending": s.run.RestorePending(),
		})
	case "run":
		if r.Method != http.MethodPost {
			break
		}
		bf, err := s.run.CreateBackupFile()
		if err != nil {
			badRequest(w, err)
			return
		}
		s.audit(r, "backup", "create", bf.Name)
		writeJSON(w, http.StatusOK, bf)
	case "file":
		p, err := s.run.BackupFilePath(r.URL.Query().Get("name"))
		if err != nil {
			badRequest(w, err)
			return
		}
		if r.Method == http.MethodDelete {
			os.Remove(p)
			s.audit(r, "backup", "delete", filepath.Base(p))
			writeJSON(w, http.StatusOK, map[string]string{"ok": "1"})
			return
		}
		w.Header().Set("Content-Type", "application/zip")
		w.Header().Set("Content-Disposition", "attachment; filename="+filepath.Base(p))
		http.ServeFile(w, r, p)
	case "inspect", "restore":
		if r.Method != http.MethodPost {
			break
		}
		src, cleanup, err := s.receiveBackup(r)
		if err != nil {
			badRequest(w, err)
			return
		}
		defer cleanup()
		if action == "inspect" {
			sum, err := s.run.InspectBackup(src)
			if err != nil {
				badRequest(w, err)
				return
			}
			writeJSON(w, http.StatusOK, sum)
			return
		}
		sum, err := s.run.StageRestore(src)
		if err != nil {
			badRequest(w, err)
			return
		}
		s.audit(r, "backup", "restore", map[string]interface{}{"users": sum.Users, "lines": sum.Lines, "from": sum.Meta.Host})
		writeJSON(w, http.StatusOK, map[string]interface{}{"ok": "1", "summary": sum, "restarting": true})
		s.run.ScheduleRestart(1500 * time.Millisecond)
	case "restart":
		if r.Method != http.MethodPost {
			break
		}
		s.audit(r, "system", "restart", nil)
		writeJSON(w, http.StatusOK, map[string]interface{}{"ok": "1", "restarting": true})
		s.run.ScheduleRestart(800 * time.Millisecond)
	default:
		http.NotFound(w, r)
	}
}

// receiveBackup 接收上传文件(multipart "file")或本地备份名(JSON {name}),落到临时文件。
func (s *Server) receiveBackup(r *http.Request) (string, func(), error) {
	noop := func() {}
	if strings.HasPrefix(r.Header.Get("Content-Type"), "multipart/form-data") {
		if err := r.ParseMultipartForm(256 << 20); err != nil {
			return "", noop, err
		}
		f, hdr, err := r.FormFile("file")
		if err != nil {
			return "", noop, errors.New("未收到文件")
		}
		defer f.Close()
		tmp, err := os.CreateTemp("", "m-ui-upload-*"+filepath.Ext(hdr.Filename))
		if err != nil {
			return "", noop, err
		}
		if _, err := io.Copy(tmp, f); err != nil {
			tmp.Close()
			os.Remove(tmp.Name())
			return "", noop, err
		}
		tmp.Close()
		return tmp.Name(), func() { os.Remove(tmp.Name()) }, nil
	}
	var req struct{ Name string }
	json.NewDecoder(r.Body).Decode(&req)
	p, err := s.run.BackupFilePath(req.Name)
	return p, noop, err
}
