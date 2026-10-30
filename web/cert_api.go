package web

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net/http"
	"os"
	"path/filepath"
	"strings"
	"time"

	"github.com/Maoyangui/m-ui/acme"
	"github.com/Maoyangui/m-ui/runner"
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
		"info":     s.run.CertInfo(),
		"status":   s.run.CertStatus(),
		"source":   s.run.CertSource(),
		"certs":    s.run.Certs(),
		"certPath": certFile,
		// 面板 / 订阅实际引用的证书路径(可能是证书库之外手填的)
		"panelCert": s.setting("webCertFile"),
		"subCert":   s.setting("subCertFile"),
		"settings": map[string]interface{}{
			"acmeDomain": s.setting("acmeDomain"), "acmeEmail": s.setting("acmeEmail"), "acmeMethod": s.setting("acmeMethod"),
			"hasCfToken": s.setting("acmeCfToken") != "", "acmeStaging": s.setting("acmeStaging") == "true",
			"acmeAutoRenew": s.setting("acmeAutoRenew") != "false",
			"webDomain":     s.setting("webDomain"), "certFile": s.setting("certFile"), "keyFile": s.setting("keyFile"),
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
	case "cancel": // 手动 DNS 等记录时反悔了
		if r.Method != http.MethodPost {
			break
		}
		s.run.CancelIssue()
		writeJSON(w, http.StatusOK, map[string]string{"ok": "1"})
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
			Domain, Email, Method, CfToken string
			Staging, AutoRenew             bool
			ForLines, ApplyPanel, ApplySub bool
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
		if !validDomain(req.Domain) {
			badRequest(w, errors.New("域名格式不对:只能是字母、数字、点与连字符,如 hk.example.com"))
			return
		}
		if req.Method != acme.MethodCloudflare && req.Method != acme.MethodManual {
			req.Method = acme.MethodHTTP
		}
		// 在这里就拦下,别等 CA 回一个看不懂的错:泛域名没有 http-01
		if acme.IsWildcard(req.Domain) && req.Method == acme.MethodHTTP {
			badRequest(w, errors.New("泛域名只能用 DNS 验证:CA 不接受 HTTP 验证。验证方式请选 Cloudflare 或手动 DNS"))
			return
		}
		if err := s.saveSettings(map[string]string{
			"acmeDomain": req.Domain, "acmeEmail": strings.TrimSpace(req.Email), "acmeMethod": req.Method,
			"acmeStaging": boolStr(req.Staging), "acmeAutoRenew": boolStr(req.AutoRenew),
		}); err != nil {
			badRequest(w, fmt.Errorf("保存证书设置失败: %w", err))
			return
		}
		if strings.TrimSpace(req.CfToken) != "" { // 留空保留原 Token
			if err := s.saveSettings(map[string]string{"acmeCfToken": strings.TrimSpace(req.CfToken)}); err != nil {
				badRequest(w, fmt.Errorf("保存 Cloudflare Token 失败: %w", err))
				return
			}
		}
		if req.Method == acme.MethodCloudflare && s.setting("acmeCfToken") == "" {
			badRequest(w, errors.New("dns-01 需要 Cloudflare API Token"))
			return
		}
		if err := s.run.IssueCert(runner.IssueRequest{
			Domain: req.Domain, Method: req.Method, Staging: req.Staging, AutoRenew: req.AutoRenew,
			Targets: runner.CertTargets{Lines: req.ForLines, Panel: req.ApplyPanel, Sub: req.ApplySub},
		}); err != nil {
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
			Hosts                          string
			ForLines, ApplyPanel, ApplySub bool
		}
		json.NewDecoder(r.Body).Decode(&req)
		var hosts []string
		for _, h := range strings.Split(req.Hosts, ",") {
			if h = strings.TrimSpace(h); h != "" {
				hosts = append(hosts, h)
			}
		}
		if err := s.run.SelfSign(hosts, runner.CertTargets{Lines: req.ForLines, Panel: req.ApplyPanel, Sub: req.ApplySub}); err != nil {
			badRequest(w, err)
			return
		}
		s.audit(r, "cert", "selfsign", hosts)
		writeJSON(w, http.StatusOK, map[string]string{"ok": "1"})
	case "external":
		if r.Method != http.MethodPost {
			break
		}
		var req struct {
			CertFile, KeyFile              string
			ForLines, ApplyPanel, ApplySub bool
		}
		if err := json.NewDecoder(r.Body).Decode(&req); err != nil {
			badRequest(w, err)
			return
		}
		if err := s.run.UseExternalCert(req.CertFile, req.KeyFile, runner.CertTargets{Lines: req.ForLines, Panel: req.ApplyPanel, Sub: req.ApplySub}); err != nil {
			badRequest(w, err)
			return
		}
		s.audit(r, "cert", "external", req.CertFile)
		writeJSON(w, http.StatusOK, map[string]string{"ok": "1"})
	case "use": // 从证书库里选一张用到某个用途(抽屉里点的)
		if r.Method != http.MethodPost {
			break
		}
		var req struct{ Name, Target string }
		if err := json.NewDecoder(r.Body).Decode(&req); err != nil {
			badRequest(w, err)
			return
		}
		if err := s.run.UseCert(req.Name, req.Target); err != nil {
			badRequest(w, err)
			return
		}
		s.audit(r, "cert", "use", map[string]string{"name": req.Name, "target": req.Target})
		writeJSON(w, http.StatusOK, map[string]string{"ok": "1"})
	case "delete":
		if r.Method != http.MethodPost {
			break
		}
		var req struct{ Name string }
		if err := json.NewDecoder(r.Body).Decode(&req); err != nil {
			badRequest(w, err)
			return
		}
		if err := s.run.DeleteCert(req.Name); err != nil {
			badRequest(w, err)
			return
		}
		s.audit(r, "cert", "delete", req.Name)
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

func (s *Server) saveSettings(kv map[string]string) error {
	return s.run.SetSettings(kv)
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

// validDomain 只接受形如 example.com / *.example.com 的域名。
// 域名会成为证书文件名的一部分,也会进 ACME 请求,先把奇怪的输入卡住。
func validDomain(d string) bool {
	d = strings.TrimPrefix(d, "*.")
	if len(d) < 3 || len(d) > 253 || strings.HasPrefix(d, ".") || strings.HasSuffix(d, ".") {
		return false
	}
	labels := strings.Split(d, ".")
	if len(labels) < 2 {
		return false
	}
	for _, l := range labels {
		if l == "" || len(l) > 63 || strings.HasPrefix(l, "-") || strings.HasSuffix(l, "-") {
			return false
		}
		for _, c := range l {
			if !(c >= 'a' && c <= 'z') && !(c >= '0' && c <= '9') && c != '-' {
				return false
			}
		}
	}
	return true
}
