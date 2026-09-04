// Package selfupdate 负责"查最新版"与"就地替换二进制":
// 面板的更新按钮与命令行菜单共用这里,逻辑只有一份。
//
// 只换 /usr/local/bin/m-ui 这一个文件,不碰数据库、证书、备份与任何设置,
// 所以更新前后用户、线路、订阅地址完全不变;替换完由调用方重启服务。
package selfupdate

import (
	"archive/tar"
	"bufio"
	"compress/gzip"
	"context"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"os"
	"os/exec"
	"path/filepath"
	"runtime"
	"strings"
	"time"

	"github.com/Maoyangui/m-ui/brand"
)

// Info 是一次版本检查的结果。
type Info struct {
	Current   string `json:"current"`   // 当前运行的版本(不带 v)
	Latest    string `json:"latest"`    // 最新 Release 的标签(带 v)
	HasUpdate bool   `json:"hasUpdate"` // 是否有更新
	CheckedAt int64  `json:"checkedAt"` // 本次检查时间
	Err       string `json:"error,omitempty"`
}

// Check 查询最新 Release 的标签。
// 先跟随 releases/latest 的重定向(不经 API,没有匿名限流),拿不到再查一次 API。
func Check(ctx context.Context, current string) (Info, error) {
	info := Info{Current: strings.TrimPrefix(current, "v"), CheckedAt: time.Now().Unix()}
	tag, err := latestTag(ctx)
	if err != nil {
		info.Err = err.Error()
		return info, err
	}
	info.Latest = tag
	info.HasUpdate = newer(strings.TrimPrefix(tag, "v"), info.Current)
	return info, nil
}

func latestTag(ctx context.Context) (string, error) {
	// 1) 重定向:GET releases/latest 会 302 到 .../tag/vX.Y.Z
	client := &http.Client{
		Timeout:       20 * time.Second,
		CheckRedirect: func(*http.Request, []*http.Request) error { return http.ErrUseLastResponse },
	}
	req, _ := http.NewRequestWithContext(ctx, "GET", "https://github.com/"+brand.RepoPath+"/releases/latest", nil)
	if resp, err := client.Do(req); err == nil {
		loc := resp.Header.Get("Location")
		resp.Body.Close()
		if i := strings.LastIndex(loc, "/tag/"); i >= 0 {
			if tag := loc[i+len("/tag/"):]; tag != "" {
				return tag, nil
			}
		}
	}
	// 2) API 兜底
	req2, _ := http.NewRequestWithContext(ctx, "GET", "https://api.github.com/repos/"+brand.RepoPath+"/releases/latest", nil)
	req2.Header.Set("User-Agent", "m-ui")
	if tok := os.Getenv("GITHUB_TOKEN"); tok != "" {
		req2.Header.Set("Authorization", "Bearer "+tok)
	}
	resp, err := (&http.Client{Timeout: 20 * time.Second}).Do(req2)
	if err != nil {
		return "", fmt.Errorf("连接 GitHub 失败: %w", err)
	}
	defer resp.Body.Close()
	if resp.StatusCode != 200 {
		return "", fmt.Errorf("读取 Releases 失败(HTTP %d)", resp.StatusCode)
	}
	var rel struct {
		TagName string `json:"tag_name"`
	}
	if err := json.NewDecoder(resp.Body).Decode(&rel); err != nil || rel.TagName == "" {
		return "", fmt.Errorf("解析 Releases 失败")
	}
	return rel.TagName, nil
}

// newer 比较两个 x.y.z:latest 比 current 新才返回 true。
// 版本号解析不出来时保守返回 false(宁可不提示,也不要误报有更新)。
func newer(latest, current string) bool {
	l, okL := parse(latest)
	c, okC := parse(current)
	if !okL || !okC {
		return false
	}
	for i := 0; i < 3; i++ {
		if l[i] != c[i] {
			return l[i] > c[i]
		}
	}
	return false
}

func parse(v string) ([3]int, bool) {
	var out [3]int
	parts := strings.SplitN(strings.TrimSpace(v), ".", 4)
	if len(parts) < 2 {
		return out, false
	}
	for i := 0; i < 3 && i < len(parts); i++ {
		n := 0
		seen := false
		for _, c := range parts[i] {
			if c < '0' || c > '9' {
				break
			}
			n, seen = n*10+int(c-'0'), true
		}
		if !seen {
			return out, false
		}
		out[i] = n
	}
	return out, true
}

// Apply 下载指定标签的二进制并就地替换 binPath;不负责重启服务。
// 下载后先用同一个 Release 里的 SHA256SUMS 校验,再确认新二进制能执行,最后才替换。
func Apply(ctx context.Context, tag, binPath string, logf func(string, ...interface{})) error {
	if runtime.GOOS != "linux" {
		return fmt.Errorf("只支持 Linux 在线更新")
	}
	if logf == nil {
		logf = func(string, ...interface{}) {}
	}
	asset := "m-ui-linux-" + runtime.GOARCH + ".tar.gz"
	base := "https://github.com/" + brand.RepoPath + "/releases/download/" + tag + "/"

	logf("下载 %s", base+asset)
	body, err := download(ctx, base+asset)
	if err != nil {
		return err
	}

	if want, err := sha256Of(ctx, base+"SHA256SUMS", asset); err != nil {
		logf("提示:未取到 SHA256SUMS,跳过校验(%v)", err)
	} else {
		got := sha256.Sum256(body)
		if hex.EncodeToString(got[:]) != want {
			return fmt.Errorf("校验失败:下载的文件与 Release 的 SHA256 不一致,已中止")
		}
		logf("SHA256 校验通过")
	}

	bin, err := extractBinary(body)
	if err != nil {
		return err
	}
	tmp := binPath + ".new"
	if err := os.WriteFile(tmp, bin, 0o755); err != nil {
		return err
	}
	if out, err := exec.Command(tmp, "version").CombinedOutput(); err != nil {
		os.Remove(tmp)
		return fmt.Errorf("新二进制无法运行: %v %s", err, out)
	}
	if err := os.Rename(tmp, binPath); err != nil {
		os.Remove(tmp)
		return fmt.Errorf("替换 %s 失败: %w", binPath, err)
	}
	logf("已替换 %s,准备重启", binPath)
	return nil
}

func download(ctx context.Context, url string) ([]byte, error) {
	dctx, cancel := context.WithTimeout(ctx, 5*time.Minute)
	defer cancel()
	req, _ := http.NewRequestWithContext(dctx, "GET", url, nil)
	if tok := os.Getenv("GITHUB_TOKEN"); tok != "" {
		req.Header.Set("Authorization", "Bearer "+tok)
	}
	resp, err := http.DefaultClient.Do(req)
	if err != nil {
		return nil, err
	}
	defer resp.Body.Close()
	if resp.StatusCode != 200 {
		return nil, fmt.Errorf("下载失败(HTTP %d)", resp.StatusCode)
	}
	return io.ReadAll(io.LimitReader(resp.Body, 256<<20))
}

// sha256Of 从 SHA256SUMS 里取出某个文件的期望摘要。
func sha256Of(ctx context.Context, url, asset string) (string, error) {
	body, err := download(ctx, url)
	if err != nil {
		return "", err
	}
	sc := bufio.NewScanner(strings.NewReader(string(body)))
	for sc.Scan() {
		f := strings.Fields(sc.Text())
		if len(f) == 2 && strings.TrimPrefix(f[1], "*") == asset {
			return strings.ToLower(f[0]), nil
		}
	}
	return "", fmt.Errorf("SHA256SUMS 里没有 %s", asset)
}

// extractBinary 从 tar.gz 里取出名为 m-ui 的那个文件(忽略其它成员与目录结构)。
func extractBinary(targz []byte) ([]byte, error) {
	gz, err := gzip.NewReader(strings.NewReader(string(targz)))
	if err != nil {
		return nil, fmt.Errorf("解压失败: %w", err)
	}
	defer gz.Close()
	tr := tar.NewReader(gz)
	for {
		h, err := tr.Next()
		if err == io.EOF {
			break
		}
		if err != nil {
			return nil, err
		}
		if h.Typeflag == tar.TypeReg && filepath.Base(h.Name) == "m-ui" {
			return io.ReadAll(io.LimitReader(tr, 256<<20))
		}
	}
	return nil, fmt.Errorf("压缩包里没有 m-ui")
}
