package runner

import (
	"context"
	"encoding/json"
	"errors"
	"net/url"
	"os"
	"strings"
	"time"

	"github.com/Maoyangui/m-ui/database/model"
	"github.com/Maoyangui/m-ui/ha"
	"github.com/Maoyangui/m-ui/logger"

	"gorm.io/gorm"
)

// haWitness 主机容灾的境外见证:从境外 Globalping 测点 TCP 连主机的公网 IP。
// 至少 2/3 的测点连得上 = up(主机还活着,是网络分区,不接任);最多 1/3 = down;其余或接口出错 = unknown。
func (r *Runner) haWitness(ctx context.Context, host string, ports []int) (ha.WitnessResult, error) {
	ok, total, err := r.reach.Witness(ctx, host, ports)
	if err != nil {
		return ha.WitnessResult{Verdict: "unknown", OK: ok, Total: total, Error: err.Error()}, nil
	}
	return ha.WitnessResult{Verdict: ha.WitnessVerdict(ok, total), OK: ok, Total: total}, nil
}

// TestCFAPIEnv 只供测试的 Cloudflare 接口地址覆盖项:真机演练时在 systemd 单元里设(Environment=…,只有 root 改得了),
// 主机容灾的改 DNS、测试令牌、让出时改回都打到这个地址(例如测试机上的假 Cloudflare)。只读进程环境,不进库,
// 面板、外部 API 都改不了 —— 做成设置的话,面板管理员就能把已保存的令牌发到任意地址。证书签发不受影响。
const TestCFAPIEnv = "M_UI_TEST_CF_API"

// testCFAPI 读并校验覆盖项(http / https、有主机名、不带用户信息);设了就在日志与操作审计里各记一条。无效的不用。
func testCFAPI(db *gorm.DB) string {
	raw := strings.TrimSpace(os.Getenv(TestCFAPIEnv))
	if raw == "" {
		return ""
	}
	u, err := checkTestCFAPI(raw)
	if err != nil {
		logger.Warning("环境变量 ", TestCFAPIEnv, " 无效,不用它: ", err)
		return ""
	}
	logger.Warning("主机容灾改 DNS 用的是测试地址 ", u, "(环境变量 ", TestCFAPIEnv, ",只供测试;证书签发不受影响)")
	b, _ := json.Marshal(map[string]string{"url": u})
	db.Create(&model.Change{DateTime: time.Now().Unix(), Actor: "system", Key: "ha", Action: "cf-api-override", Obj: b})
	return u
}

func checkTestCFAPI(raw string) (string, error) {
	u, err := url.Parse(raw)
	switch {
	case err != nil:
		return "", err
	case u.Scheme != "http" && u.Scheme != "https":
		return "", errors.New("只能是 http:// 或 https:// 地址")
	case u.Host == "" || u.Hostname() == "":
		return "", errors.New("缺主机名")
	case u.User != nil:
		return "", errors.New("地址里不能带用户名或密码")
	case u.RawQuery != "" || u.Fragment != "":
		return "", errors.New("地址里不能带查询参数")
	}
	return strings.TrimRight(u.String(), "/"), nil
}
