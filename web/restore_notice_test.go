package web

import (
	"encoding/json"
	"net/http/httptest"
	"os"
	"path/filepath"
	"testing"

	"github.com/Maoyangui/m-ui/backup"
)

// 上次还原放进数据目录的证书副本(没有谁续)、还缺的文件:status 里带上 restore,面板顶部提示;
// 点「知道了」(POST backup/restore-ack)后不再带,GET 不算。
func TestStatusShowsRestoreNoticeUntilAck(t *testing.T) {
	s, run := certServer(t)
	status := func() map[string]json.RawMessage {
		t.Helper()
		w := httptest.NewRecorder()
		s.handleStatus(w, httptest.NewRequest("GET", "http://x/app/api/status", nil))
		var m map[string]json.RawMessage
		if err := json.Unmarshal(w.Body.Bytes(), &m); err != nil {
			t.Fatalf("status 不是 JSON: %v %s", err, w.Body.String())
		}
		return m
	}
	ack := func(method string) int {
		w := httptest.NewRecorder()
		s.handleBackupSub(w, httptest.NewRequest(method, "http://x/app/api/backup/restore-ack", nil))
		return w.Code
	}
	if _, ok := status()["restore"]; ok {
		t.Fatal("没还原过不该有还原提示")
	}
	notice := `{"at":1,"copies":[{"from":"/etc/letsencrypt/live/a.example.com/fullchain.pem","to":"/etc/m-ui/cert/restored/a.example.com-fullchain.pem",` +
		`"names":["a.example.com"],"notAfter":1900000000}],"missing":["/gone/reseller.pem"]}`
	if err := os.WriteFile(filepath.Join(run.DataDir(), backup.NoticeFile), []byte(notice), 0o600); err != nil {
		t.Fatal(err)
	}
	var n backup.Notice
	if err := json.Unmarshal(status()["restore"], &n); err != nil || len(n.Copies) != 1 || n.Copies[0].Names[0] != "a.example.com" ||
		n.Copies[0].NotAfter != 1900000000 || len(n.Missing) != 1 || n.Missing[0] != "/gone/reseller.pem" {
		t.Fatalf("status 里要带上还原提示: %v %+v", err, n)
	}
	ack("GET")
	if _, ok := status()["restore"]; !ok {
		t.Fatal("GET 不算「知道了」")
	}
	if code := ack("POST"); code != 200 {
		t.Fatalf("「知道了」应成功: %d", code)
	}
	if _, ok := status()["restore"]; ok {
		t.Fatal("点了「知道了」就不再提示")
	}
	if code := ack("POST"); code != 200 {
		t.Fatalf("已经没有提示时再点也不报错: %d", code)
	}
}
