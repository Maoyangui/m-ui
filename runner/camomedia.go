package runner

// 页面伪装的图包(用户 11-19,见 camo/media.go):伪装站用到照片(生成时 Wanted 不空,设置页预览也算)才把用到的那几张原图取下来 ——
// 仓库的 camo-media 分支,先 raw.githubusercontent.com,取不到走 jsDelivr —— 按 camo.MediaFiles 核对 sha256,存进数据目录的
// camo-media/,再按本机种子处理成要的尺寸(camo.Process),放在内存里交给生成器;处理好一批就让伪装站重新生成。
// 原图存下来以后直接用(重启不再下载);处理好的只在内存里,最多 6 MB,超了先丢最久没用的;种子变了全部丢掉。
// 取不到时照旧画插图,隔一阵再试:10 分钟、1 小时,之后每 6 小时。设置页显示这台的状况(CamoPhotoStatus)。

import (
	"context"
	"crypto/sha256"
	"encoding/hex"
	"fmt"
	"io"
	"net/http"
	"net/url"
	"os"
	"path/filepath"
	"sort"
	"sync"
	"testing"
	"time"

	"github.com/Maoyangui/m-ui/brand"
	"github.com/Maoyangui/m-ui/camo"
	"github.com/Maoyangui/m-ui/logger"
	"github.com/Maoyangui/m-ui/util/fsutil"
)

// camoMediaURLs 取原图的地址(依次试;测试里换成本地的)
var camoMediaURLs = []string{
	"https://raw.githubusercontent.com/" + brand.RepoPath + "/" + camo.MediaBranch + "/",
	"https://cdn.jsdelivr.net/gh/" + brand.RepoPath + "@" + camo.MediaBranch + "/",
}

// camoMediaHTTP 取原图用:一张两三百 KB,给 30 秒(连接、TLS 的时限和取流量奖励账本的一样)
var camoMediaHTTP = &http.Client{Timeout: 30 * time.Second, Transport: rewardHTTP.Transport}

// camoMediaBudget 处理好的照片在内存里最多占多少
const camoMediaBudget = 6 << 20

// camoMedia 图包:原图在磁盘上,处理好的在内存里
type camoMedia struct {
	mu      sync.Mutex
	seed    uint64
	done    map[camoPhotoKey]*camoPhoto
	size    int
	running bool
	fails   int
	retryAt time.Time
	err     string
	gen     int      // 每多处理好一批加一(进伪装站的缓存键,伪装站随之重新生成)
	src     []string // 取原图的地址;空 = camoMediaURLs。测试里不给就不去取(不连外网,也不会在测试结束后还往临时目录里写)
}

type camoPhotoKey struct {
	name string
	w, h int
}

type camoPhoto struct {
	b    []byte
	used time.Time
}

// CamoPhotoStatus 设置页上「封面照片」那一行:这一站用到几张、处理好几张、是不是正在取、上次没取全的原因和下次再试的时间。
type CamoPhotoStatus struct {
	Want    int    `json:"want"`
	Have    int    `json:"have"`
	Loading bool   `json:"loading,omitempty"`
	Error   string `json:"error,omitempty"`
	Retry   int64  `json:"retry,omitempty"` // Unix 秒
	Off     bool   `json:"off,omitempty"`   // 页面伪装没开:缺的照片不去取(开启后才取,见 CamoPreview)
}

// camoPhotoView 生成器取照片用(按种子;种子换了就一张都没有)
type camoPhotoView struct {
	m    *camoMedia
	seed uint64
}

func (v camoPhotoView) Photo(name string, w, h int) []byte {
	v.m.mu.Lock()
	defer v.m.mu.Unlock()
	if v.m.seed != v.seed {
		return nil
	}
	p := v.m.done[camoPhotoKey{name, w, h}]
	if p == nil {
		return nil
	}
	p.used = time.Now()
	return p.b
}

// photos 这个种子下的照片(种子变了先把处理好的全丢掉)和现在的批次号
func (m *camoMedia) photos(seed uint64) (camo.Photos, int) {
	m.mu.Lock()
	defer m.mu.Unlock()
	if m.seed != seed {
		m.seed, m.done, m.size = seed, nil, 0
		m.gen++
	}
	return camoPhotoView{m: m, seed: seed}, m.gen
}

// put 存一张处理好的(超了预算先丢最久没用的)
func (m *camoMedia) put(seed uint64, k camoPhotoKey, b []byte) {
	m.mu.Lock()
	defer m.mu.Unlock()
	if m.seed != seed {
		return
	}
	if m.done == nil {
		m.done = map[camoPhotoKey]*camoPhoto{}
	}
	if old := m.done[k]; old != nil {
		m.size -= len(old.b)
	}
	m.done[k] = &camoPhoto{b: b, used: time.Now()}
	m.size += len(b)
	for m.size > camoMediaBudget && len(m.done) > 1 {
		var oldest camoPhotoKey
		var at time.Time
		for kk, p := range m.done {
			if kk != k && (at.IsZero() || p.used.Before(at)) {
				oldest, at = kk, p.used
			}
		}
		m.size -= len(m.done[oldest].b)
		delete(m.done, oldest)
	}
}

// camoMediaWant 生成完一个站后:有没处理好的照片就在后台去取、去处理(同时只跑一个;没取全的到点才再试)
func (r *Runner) camoMediaWant(seed uint64, reqs []camo.PhotoReq) {
	if len(reqs) == 0 {
		return
	}
	m := &r.camo.media
	m.mu.Lock()
	src := m.src
	if m.running || m.seed != seed || time.Now().Before(m.retryAt) || (src == nil && testing.Testing()) {
		m.mu.Unlock()
		return
	}
	if src == nil {
		src = camoMediaURLs
	}
	m.running = true
	m.mu.Unlock()
	go r.camoMediaRun(seed, src, append([]camo.PhotoReq(nil), reqs...))
}

func (r *Runner) camoMediaRun(seed uint64, src []string, reqs []camo.PhotoReq) {
	ctx, cancel := context.WithTimeout(context.Background(), 3*time.Minute)
	defer cancel()
	dir := filepath.Join(r.DataDir(), "camo-media")
	sizes := map[string][]camoPhotoKey{}
	for _, q := range reqs {
		k := camoPhotoKey{q.Name, q.W, q.H}
		dup := false
		for _, x := range sizes[q.Name] {
			dup = dup || x == k
		}
		if !dup {
			sizes[q.Name] = append(sizes[q.Name], k)
		}
	}
	names := make([]string, 0, len(sizes))
	for n := range sizes {
		names = append(names, n)
	}
	sort.Strings(names)
	var firstErr error
	got := 0
	for _, n := range names {
		orig, err := camoMediaFile(ctx, src, dir, n)
		if err == nil {
			for _, k := range sizes[n] {
				var b []byte
				if b, err = camo.Process(orig, seed, n, k.w, k.h); err != nil {
					break
				}
				r.camo.media.put(seed, k, b)
				got++
			}
		}
		if err != nil && firstErr == nil {
			firstErr = err
		}
	}
	m := &r.camo.media
	m.mu.Lock()
	m.running = false
	if firstErr != nil {
		m.fails++
		m.err = firstErr.Error()
		m.retryAt = time.Now().Add([]time.Duration{10 * time.Minute, time.Hour, 6 * time.Hour}[min(m.fails, 3)-1])
	} else {
		m.fails, m.err, m.retryAt = 0, "", time.Time{}
	}
	if got > 0 {
		m.gen++
	}
	m.mu.Unlock()
	if got > 0 { // 伪装站下一次回应时按新照片重新生成
		r.camo.mu.Lock()
		r.camo.at = time.Time{}
		r.camo.mu.Unlock()
	}
	if firstErr != nil {
		logger.Warning("页面伪装:图包里的照片没取全(这次处理好 ", got, " 张),没有照片的位置先用画的插图: ", firstErr)
	}
}

// camoMediaFile 一张原图:数据目录里有、核对得上就用;没有就去取(src 里的地址依次试),核对 sha256 后存下来
func camoMediaFile(ctx context.Context, src []string, dir, name string) ([]byte, error) {
	want, ok := camo.MediaFiles[name]
	if !ok {
		return nil, fmt.Errorf("photo pack has no %s", name)
	}
	p := filepath.Join(dir, name)
	if b, err := os.ReadFile(p); err == nil && sha256Hex(b) == want {
		return b, nil
	}
	var lastErr error
	for _, base := range src {
		b, err := camoFetch(ctx, base+name)
		if err == nil && sha256Hex(b) != want {
			err = fmt.Errorf("%s from %s does not match the manifest", name, hostOf(base))
		}
		if err != nil {
			lastErr = err
			continue
		}
		if err := os.MkdirAll(dir, 0o755); err != nil {
			logger.Warning("页面伪装:图包目录建不了(照片照用,下次还要再取): ", err)
		} else if err := fsutil.WriteFileAtomic(p, b, 0o644); err != nil {
			logger.Warning("页面伪装:图包里的照片存不下(照片照用,下次还要再取): ", err)
		}
		return b, nil
	}
	return nil, lastErr
}

func camoFetch(ctx context.Context, u string) ([]byte, error) {
	req, err := http.NewRequestWithContext(ctx, http.MethodGet, u, nil)
	if err != nil {
		return nil, err
	}
	req.Header.Set("User-Agent", "m-ui/"+Version)
	resp, err := camoMediaHTTP.Do(req)
	if err != nil {
		return nil, err
	}
	defer resp.Body.Close()
	if resp.StatusCode != http.StatusOK {
		return nil, fmt.Errorf("%s: HTTP %d", hostOf(u), resp.StatusCode)
	}
	b, err := io.ReadAll(io.LimitReader(resp.Body, camo.MediaMaxBytes+1))
	if err != nil {
		return nil, err
	}
	if len(b) > camo.MediaMaxBytes {
		return nil, fmt.Errorf("%s: larger than %d bytes", hostOf(u), camo.MediaMaxBytes)
	}
	return b, nil
}

func sha256Hex(b []byte) string {
	s := sha256.Sum256(b)
	return hex.EncodeToString(s[:])
}

func hostOf(u string) string {
	if x, err := url.Parse(u); err == nil && x.Host != "" {
		return x.Host
	}
	return u
}

// camoPhotoStatus 一个站的封面照片状况(没用照片的站为 nil)
func (r *Runner) camoPhotoStatus(site *camo.Site) *CamoPhotoStatus {
	names := site.PhotoNames()
	if len(names) == 0 {
		return nil
	}
	miss := map[string]bool{}
	for _, q := range site.Wanted() {
		miss[q.Name] = true
	}
	m := &r.camo.media
	m.mu.Lock()
	defer m.mu.Unlock()
	st := &CamoPhotoStatus{Want: len(names), Have: len(names) - len(miss), Loading: m.running && len(miss) > 0}
	if len(miss) > 0 && m.err != "" {
		st.Error = m.err
		if !m.retryAt.IsZero() {
			st.Retry = m.retryAt.Unix()
		}
	}
	return st
}
