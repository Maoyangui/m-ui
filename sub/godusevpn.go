package sub

// 佛跳墙(本项目自家的客户端)在落地页里的下载版本。
// 别的客户端版本号是写死的常量,自家这个要"永远是最新":后台每隔几小时读一次 GitHub 的发布订阅源
// (releases.atom,不用令牌、预发布也算),把最新的 tag 缓存下来;拉不到就用编译时的兜底版本,页面照常能下载。

import (
	"context"
	"encoding/xml"
	"io"
	"net/http"
	"regexp"
	"strings"
	"sync"
	"time"
)

const (
	godRepo = "Maoyangui/godusevpn"
	// godFallbackVer 兜底版本:发布新版时顺手更新一下,拉不到网络也有一份能用的链接
	godFallbackVer  = "0.6.2-mac2"
	godRefreshEvery = 6 * time.Hour
)

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
	req, err := http.NewRequestWithContext(ctx, http.MethodGet, base+"/"+godRepo+"/releases.atom", nil)
	if err != nil {
		return "", err
	}
	req.Header.Set("Accept", "application/atom+xml")
	resp, err := (&http.Client{Timeout: 20 * time.Second}).Do(req)
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
