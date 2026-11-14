package web

import (
	"encoding/json"
	"io/fs"
	"os"
	"regexp"
	"strings"
	"testing"

	"github.com/Maoyangui/m-ui/database/model"
)

// checkRenewExamples 接口一览的 curl 示例要和文档的续费建议一致:示例里的套餐是月付(周期重置),
// 提前一次续多期要用 extend(保留本期用量);默认的 renew 会截断当前周期,多给流量。
func checkRenewExamples(t *testing.T, js string) {
	t.Helper()
	ms := regexp.MustCompile(`-d '(\{[^']*\})' \$\{base\}/users/\$\{user\}/(?:renew|plan)\b`).FindAllStringSubmatch(js, -1)
	if len(ms) == 0 {
		t.Fatal("apidoc.js 里找不到续费的 curl 示例,正则需要更新")
	}
	for _, m := range ms {
		var body struct {
			Periods int    `json:"periods"`
			Mode    string `json:"mode"`
		}
		if err := json.Unmarshal([]byte(m[1]), &body); err != nil {
			t.Fatalf("示例的请求体不是 JSON: %s", m[1])
		}
		if body.Periods > 1 && body.Mode != "extend" {
			t.Errorf("示例一次续 %d 期却没用 extend(周期重置套餐提前续费要用 extend): %s", body.Periods, m[1])
		}
	}
}

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

	checkRenewExamples(t, string(js))

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
