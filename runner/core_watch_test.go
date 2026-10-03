package runner

import (
	"path/filepath"
	"testing"

	"github.com/Maoyangui/m-ui/database"
)

// 数据面没在跑(开机那一下起不来、回滚也失败)要能自己重新拉起,不能一直停到有人改配置;进程在停 / 在重启时不拉。
func TestReviveCoreRestartsStoppedDataPlane(t *testing.T) {
	r, err := New(filepath.Join(t.TempDir(), "m.db"))
	if err != nil {
		t.Fatal(err)
	}
	defer database.Close(r.DB())
	defer r.core.Stop()
	if r.CoreRunning() {
		t.Fatal("前提:还没启动")
	}
	tried, err := r.reviveCore()
	if !tried || err != nil || !r.CoreRunning() {
		t.Fatalf("没在跑就该拉起来: tried=%v err=%v running=%v", tried, err, r.CoreRunning())
	}
	if tried, _ := r.reviveCore(); tried {
		t.Fatal("在跑时不该再拉")
	}
	r.Stop()
	if tried, _ := r.reviveCore(); tried || r.CoreRunning() {
		t.Fatal("进程在停时不该再拉起数据面")
	}
}
