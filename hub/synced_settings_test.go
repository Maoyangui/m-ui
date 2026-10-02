package hub

import (
	"os"
	"path/filepath"
	"regexp"
	"testing"
)

// 订阅口在副机上也跑:订阅包读的展示类设置(落地页文案、选购 / 续费地址、Powered by、clash 扩展……)
// 都得随快照同步,不然副机订阅口返回的落地页和响应头跟主机不一样 —— 0.6.x 漏了选购地址与 Powered by(审计 M066)。
// 每台机器自己的监听、证书、路径、地址不同步。以后新增展示类设置时这条测试会提醒加进 SyncedSettings。
func TestSubDisplaySettingsAreSynced(t *testing.T) {
	local := map[string]bool{
		"subPath": true, "subListen": true, "subCertFile": true, "subKeyFile": true, "subInsecure": true, "subServerAddr": true,
	}
	synced := map[string]bool{}
	for _, k := range SyncedSettings {
		synced[k] = true
	}
	files, err := filepath.Glob(filepath.Join("..", "sub", "*.go"))
	if err != nil || len(files) == 0 {
		t.Fatalf("找不到订阅包源码: %v", err)
	}
	re := regexp.MustCompile(`setting\("(sub[A-Za-z]+)"\)`)
	seen := 0
	for _, f := range files {
		if filepath.Ext(f) != ".go" || regexp.MustCompile(`_test\.go$`).MatchString(f) {
			continue
		}
		b, err := os.ReadFile(f)
		if err != nil {
			t.Fatal(err)
		}
		for _, m := range re.FindAllStringSubmatch(string(b), -1) {
			seen++
			if !local[m[1]] && !synced[m[1]] {
				t.Errorf("订阅包读了 %s(%s),但它不在 SyncedSettings 里:副机订阅口会拿不到主机的值", m[1], filepath.Base(f))
			}
		}
	}
	if seen == 0 {
		t.Fatal("一个设置键都没扫到:订阅包读设置的写法变了,这条测试得跟着改")
	}
}
