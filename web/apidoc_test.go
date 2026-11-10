package web

import (
	"io/fs"
	"os"
	"regexp"
	"strings"
	"testing"

	"github.com/Maoyangui/m-ui/database/model"
)

// 外部 API 的接口一览只有一份(assets/js/apidoc.js,管理员页与代理「我的账号」页共用):
// 它要和 docs/API.md 的「接口一览」逐行一致,而且每个路径在外部 API 下都有处理(不是「接口不存在」)。
func TestAPIDocMatchesDocsAndRoutes(t *testing.T) {
	js, err := fs.ReadFile(assets, "assets/js/apidoc.js")
	if err != nil {
		t.Fatal(err)
	}
	var fromJS []string
	for _, m := range regexp.MustCompile(`\{ m: '([^']+)', p: '([^']+)'`).FindAllStringSubmatch(string(js), -1) {
		fromJS = append(fromJS, m[1]+" "+m[2])
	}
	if len(fromJS) < 15 {
		t.Fatalf("没从 apidoc.js 解析出接口(%d 个),正则需要更新", len(fromJS))
	}

	md, err := os.ReadFile("../docs/API.md")
	if err != nil {
		t.Fatal(err)
	}
	doc := string(md)
	i := strings.Index(doc, "## 3. 接口一览")
	if i < 0 {
		t.Fatal("docs/API.md 里找不到「## 3. 接口一览」")
	}
	sec := doc[i+1:]
	if j := strings.Index(sec, "\n## "); j >= 0 {
		sec = sec[:j]
	}
	var fromDoc []string
	for _, m := range regexp.MustCompile("(?m)^\\| ([A-Z][A-Z /]*[A-Z]) \\| `([^`]+)` \\|").FindAllStringSubmatch(sec, -1) {
		fromDoc = append(fromDoc, strings.TrimSpace(m[1])+" "+strings.ReplaceAll(m[2], `\|`, "|"))
	}
	if strings.Join(fromJS, "\n") != strings.Join(fromDoc, "\n") {
		t.Fatalf("apidoc.js 与 docs/API.md 的接口一览不一致:\napidoc.js:\n  %s\ndocs/API.md:\n  %s", strings.Join(fromJS, "\n  "), strings.Join(fromDoc, "\n  "))
	}

	// 每个接口都能路由到处理函数:用一个真实存在的用户,只看是不是「接口不存在」/ 方法不允许
	e := newPCEnv(t)
	p := e.mkPlan(model.Plan{Name: "月付", VolumeGB: 10, Days: 30})
	for n, entry := range fromJS {
		cut := strings.LastIndex(entry, " ") // 路径里没有空格,方法里可能有("PATCH / PUT")
		method, path := entry[:cut], entry[cut+1:]
		if strings.Contains(method, "/") {
			method = strings.TrimSpace(strings.SplitN(method, "/", 2)[0])
		}
		user := "u" + string(rune('a'+n))
		e.mkUser(model.User{Name: user, PlanId: p.Id, Volume: 10 << 30})
		if method == "DELETE" {
			user = "nobody" // 删除要踢线(测试里没有数据面):用不存在的用户,确认走到了用户接口
		}
		path = strings.TrimPrefix(strings.ReplaceAll(path, "{name|id}", user), "/")
		body := `{"dryRun":true}`
		w := e.api(method, path, body)
		if strings.Contains(w.Body.String(), "接口不存在") || w.Code == 405 {
			t.Errorf("%s 没有对应的处理: %d %s", entry, w.Code, w.Body.String())
		}
		if method == "DELETE" && !strings.Contains(w.Body.String(), "用户不存在") {
			t.Errorf("%s 应走到用户接口: %d %s", entry, w.Code, w.Body.String())
		}
	}
}
