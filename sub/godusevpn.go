package sub

// 佛跳墙(本项目自家的客户端)在落地页里的下载版本。
// 别的客户端版本号是写死的常量,自家这个要"永远是最新":后台每隔几小时读一次 GitHub 的发布订阅源
// (releases.atom,不用令牌、预发布也算),把最新的 tag 缓存下来;拉不到就用编译时的兜底版本,页面照常能下载。
// 纯 IPv6 机器连不上 GitHub:直连不通时经本机 WARP 再取一次(与检查更新同一套)。

import (
	"context"
	"encoding/xml"
	"io"
	"net/http"
	"regexp"
	"strings"
	"sync"
	"time"

	"github.com/Maoyangui/m-ui/selfupdate"
)

const (
	godRepo         = "Maoyangui/godusevpn"
	godRefreshEvery = 6 * time.Hour
)

// godFallbackVer 兜底版本:拉不到发布源时落地页用它。发版构建(.github/workflows/release.yml)会取佛跳墙
// 当前最新发布、用 -ldflags "-X github.com/Maoyangui/m-ui/sub.godFallbackVer=…" 写进去,不用手改;
// 这里的值只给自己从源码编译、没有注入的情况兜底(两边版本号同步发,佛跳墙先发)。
var godFallbackVer = "0.7.13"

var godTagRe = regexp.MustCompile(`^[0-9]+\.[0-9]+\.[0-9]+[0-9A-Za-z.-]*$`)

var godLatest = struct {
	sync.RWMutex
	ver string
}{ver: godFallbackVer}

// godusevpnVersion 落地页用的版本号(不带 v 前缀)。
func godusevpnVersion() string {
	godLatest.RLock()
	defer godLatest.RUnlock()
	return godLatest.ver
}

func setGodusevpnVersion(v string) {
	v = strings.TrimPrefix(strings.TrimSpace(v), "v")
	if !godTagRe.MatchString(v) {
		return
	}
	godLatest.Lock()
	godLatest.ver = v
	godLatest.Unlock()
}

// fetchGodusevpnVersion 读发布订阅源,取最新一条的 tag。条目按时间倒序,第一条就是最新发布。
func fetchGodusevpnVersion(ctx context.Context, base string) (string, error) {
	resp, err := selfupdate.DoGitHub(ctx, &http.Client{Timeout: 20 * time.Second}, func() (*http.Request, error) {
		req, err := http.NewRequestWithContext(ctx, http.MethodGet, base+"/"+godRepo+"/releases.atom", nil)
		if err == nil {
			req.Header.Set("Accept", "application/atom+xml")
		}
		return req, err
	})
	if err != nil {
		return "", err
	}
	defer resp.Body.Close()
	var feed struct {
		Entries []struct {
			Link struct {
				Href string `xml:"href,attr"`
			} `xml:"link"`
		} `xml:"entry"`
	}
	if err := xml.NewDecoder(io.LimitReader(resp.Body, 2<<20)).Decode(&feed); err != nil {
		return "", err
	}
	for _, e := range feed.Entries {
		tag := e.Link.Href[strings.LastIndex(e.Link.Href, "/")+1:]
		if v := strings.TrimPrefix(tag, "v"); godTagRe.MatchString(v) {
			return v, nil
		}
	}
	return "", nil
}

// startGodusevpnRefresh 起一个后台协程定期刷新;服务启动时调一次,stop 关闭时退出。
func (s *Server) startGodusevpnRefresh() {
	go func() {
		t := time.NewTicker(godRefreshEvery)
		defer t.Stop()
		for {
			ctx, cancel := context.WithTimeout(context.Background(), 30*time.Second)
			v, err := fetchGodusevpnVersion(ctx, "https://github.com")
			cancel()
			if err == nil && v != "" {
				setGodusevpnVersion(v)
			}
			select {
			case <-t.C:
			case <-s.stop:
				return
			}
		}
	}()
}
