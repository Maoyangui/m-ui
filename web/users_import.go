package web

import (
	"errors"
	"io"
	"net/http"
	"os"

	"github.com/Maoyangui/m-ui/importer"
)

// handleUsersImport POST /api/users/import(multipart:file=旧面板 .db,assign=true|false)
// 只导入用户:同名更新用量/配额/到期,新用户创建并可分配全部现有线路;线路、上游、设置不动。
func (s *Server) handleUsersImport(w http.ResponseWriter, r *http.Request) {
	if r.Method != http.MethodPost {
		writeJSON(w, http.StatusMethodNotAllowed, map[string]string{"error": "方法不允许"})
		return
	}
	if err := r.ParseMultipartForm(64 << 20); err != nil {
		badRequest(w, err)
		return
	}
	f, hdr, err := r.FormFile("file")
	if err != nil {
		badRequest(w, errors.New("请选择旧面板数据库文件"))
		return
	}
	defer f.Close()
	tmp, err := os.CreateTemp("", "m-ui-import-*.db")
	if err != nil {
		badRequest(w, err)
		return
	}
	tmpName := tmp.Name()
	defer func() {
		for _, suffix := range []string{"", "-wal", "-shm"} {
			os.Remove(tmpName + suffix)
		}
	}()
	// 上传文件落盘也要有上限;超限要明说,而不是悄悄截断后报一个"不是 SQLite 文件"
	const maxUpload = 512 << 20
	n, err := io.Copy(tmp, io.LimitReader(f, maxUpload+1))
	tmp.Close()
	if err != nil {
		badRequest(w, err)
		return
	}
	if n > maxUpload {
		badRequest(w, errors.New("文件超过 512 MB 上限"))
		return
	}

	assign := r.FormValue("assign") != "false"
	sum, err := importer.ImportUsersOnly(tmpName, s.db, assign)
	if err != nil {
		badRequest(w, err)
		return
	}
	s.audit(r, "user", "import:"+hdr.Filename, sum)
	s.reloadUsers("导入旧面板用户")
	writeJSON(w, http.StatusOK, sum)
}
