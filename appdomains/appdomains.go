// Package appdomains 按应用名查它用到的域名,给线路分流规则里的"应用(查域名)"用:
// 填抖音、netflix 这样的名字,换成一组可以再改的域名规则。
//
// 数据来自 v2fly/domain-list-community —— 各家客户端 geosite 规则的源头,按应用一份列表,
// 列表之间用 include 互相引用。这里只取三种分流规则认得的写法(域名后缀、完整域名、关键字),
// 正则写法跳过并计数。查过的列表缓存一天。
package appdomains

import (
	"bufio"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net/http"
	"net/netip"
	"regexp"
	"sort"
	"strings"
	"sync"
	"time"
)

// Result 一个应用名的查询结果。
type Result struct {
	Query   string   `json:"query"`
	List    string   `json:"list,omitempty"`    // 实际用到的列表名
	Suffix  []string `json:"suffix,omitempty"`  // 域名后缀(含子域名)
	Full    []string `json:"full,omitempty"`    // 完整域名(已被后缀盖住的去掉了)
	Keyword []string `json:"keyword,omitempty"` // 域名关键字
	Skipped int      `json:"skipped,omitempty"` // 正则写法的条目:分流规则不支持,跳过
	Error   string   `json:"error,omitempty"`
	Suggest []string `json:"suggest,omitempty"` // 没找到时,名字相近的列表
}

// ErrNotFound 没有这个名字的列表。
var ErrNotFound = errors.New("没找到这个应用")

// Client 查列表的客户端;Bases 依次尝试(第一个说没有就是没有,连不上才换下一个)。
type Client struct {
	Bases []string // 数据文件所在目录,以 / 结尾
	Index string   // 全部列表名的索引(只用来推荐相近的名字)
	HTTP  *http.Client
	TTL   time.Duration

	mu      sync.Mutex
	files   map[string]cachedFile
	index   []string
	indexAt time.Time
}

type cachedFile struct {
	lines []string
	at    time.Time
}

// Default 面板用的客户端:GitHub 原始文件,连不上换 jsDelivr 镜像。
var Default = &Client{
	Bases: []string{
		"https://raw.githubusercontent.com/v2fly/domain-list-community/master/data/",
		"https://cdn.jsdelivr.net/gh/v2fly/domain-list-community@master/data/",
	},
	Index: "https://data.jsdelivr.com/v1/package/gh/v2fly/domain-list-community@master/flat",
	HTTP:  &http.Client{Timeout: 12 * time.Second},
	TTL:   24 * time.Hour,
}

const (
	maxFiles  = 64      // 一个应用展开 include 最多读这么多份列表
	maxDepth  = 8       // include 最多套这么多层
	maxFileSz = 2 << 20 // 单份列表的大小上限
)

// 中文名与常见叫法 → 列表名。没列在这里的,英文名直接当列表名试。
var aliases = map[string]string{
	"抖音": "douyin", "douyin": "douyin", "tiktok": "tiktok", "抖音海外版": "tiktok",
	"字节": "bytedance", "字节跳动": "bytedance", "今日头条": "bytedance", "头条": "bytedance",
	"飞书": "lark", "豆包": "doubao", "小红书": "xiaohongshu", "红书": "xiaohongshu",
	"chatgpt": "openai", "gpt": "openai", "sora": "openai", "claude": "anthropic",
	"gemini": "google-gemini", "谷歌": "google", "油管": "youtube", "yt": "youtube",
	"奈飞": "netflix", "网飞": "netflix", "声破天": "spotify",
	"推特": "twitter", "x": "twitter", "电报": "telegram", "tg": "telegram",
	"脸书": "facebook", "fb": "facebook", "ins": "instagram", "ig": "instagram",
	"迪士尼": "disney", "disney+": "disney", "disneyplus": "disney", "prime": "primevideo", "primevideo": "primevideo",
	"亚马逊": "amazon", "b站": "bilibili", "哔哩哔哩": "bilibili", "知乎": "zhihu",
	"腾讯": "tencent", "微信": "tencent", "qq": "tencent", "阿里": "alibaba", "阿里巴巴": "alibaba", "淘宝": "alibaba",
	"微软": "microsoft", "苹果": "apple", "必应": "bing",
	"ai": "category-ai-chat-!cn", "人工智能": "category-ai-chat-!cn",
}

var validName = regexp.MustCompile(`^[a-z0-9][a-z0-9.!-]{0,63}$`)

// ListName 应用名换成列表名;认不出(既不在别名表、也不像列表名)返回空。
func ListName(q string) string {
	k := strings.ToLower(strings.Join(strings.Fields(q), ""))
	if v, ok := aliases[k]; ok {
		return v
	}
	if validName.MatchString(k) {
		return k
	}
	return ""
}

// Lookup 查一组应用名(同时查几个,结果按输入的顺序);同名重复的只查一次。
func (c *Client) Lookup(ctx context.Context, names []string) []Result {
	var qs []string
	seen := map[string]bool{}
	for _, q := range names {
		q = strings.TrimSpace(q)
		if q == "" || seen[strings.ToLower(q)] {
			continue
		}
		seen[strings.ToLower(q)] = true
		qs = append(qs, q)
	}
	out := make([]Result, len(qs))
	sem := make(chan struct{}, 4)
	var wg sync.WaitGroup
	for i, q := range qs {
		wg.Add(1)
		go func(i int, q string) {
			defer wg.Done()
			sem <- struct{}{}
			defer func() { <-sem }()
			out[i] = c.lookupOne(ctx, q)
		}(i, q)
	}
	wg.Wait()
	return out
}

func (c *Client) lookupOne(ctx context.Context, q string) Result {
	r := Result{Query: q}
	name := ListName(q)
	if name == "" {
		r.Error = ErrNotFound.Error()
		return r
	}
	acc := newAcc()
	if err := c.resolve(ctx, name, map[string]bool{}, 0, acc); err != nil {
		if errors.Is(err, ErrNotFound) {
			r.Error = ErrNotFound.Error()
			r.Suggest = c.suggest(ctx, strings.ToLower(strings.Join(strings.Fields(q), "")))
		} else {
			r.Error = err.Error()
		}
		return r
	}
	r.List = name
	r.Suffix, r.Full, r.Keyword, r.Skipped = acc.result()
	if len(r.Suffix)+len(r.Full)+len(r.Keyword) == 0 {
		r.Error = ErrNotFound.Error()
	}
	return r
}

// resolve 读一份列表并展开 include;只有最外层那份找不到才算错,里面引用的缺一份就跳过。
func (c *Client) resolve(ctx context.Context, name string, seen map[string]bool, depth int, acc *acc) error {
	if seen[name] || depth > maxDepth || len(seen) >= maxFiles {
		return nil
	}
	seen[name] = true
	lines, err := c.file(ctx, name)
	if err != nil {
		if depth > 0 {
			return nil
		}
		return err
	}
	for _, line := range lines {
		if i := strings.IndexByte(line, '#'); i >= 0 {
			line = line[:i]
		}
		f := strings.Fields(line)
		if len(f) == 0 {
			continue
		}
		if !attrsOnly(f[1:]) { // 值后面只能跟 @cn / @ads 这样的属性(不影响分流),别的写法整行不认
			continue
		}
		v := strings.ToLower(f[0])
		switch {
		case strings.HasPrefix(v, "include:"):
			if sub := strings.TrimPrefix(v, "include:"); validName.MatchString(sub) {
				c.resolve(ctx, sub, seen, depth+1, acc)
			}
		case strings.HasPrefix(v, "regexp:"):
			acc.skipped++
		case strings.HasPrefix(v, "keyword:"):
			acc.add("k", strings.TrimPrefix(v, "keyword:"))
		case strings.HasPrefix(v, "full:"):
			acc.add("f", strings.TrimPrefix(v, "full:"))
		case strings.HasPrefix(v, "domain:"):
			acc.add("s", strings.TrimPrefix(v, "domain:"))
		case !strings.Contains(v, ":"):
			acc.add("s", v)
		}
	}
	return nil
}

// file 取一份列表(带缓存)。第一个地址回 404 就是没有;连不上才换下一个。
func (c *Client) file(ctx context.Context, name string) ([]string, error) {
	c.mu.Lock()
	if f, ok := c.files[name]; ok && time.Since(f.at) < c.TTL {
		c.mu.Unlock()
		return f.lines, nil
	}
	c.mu.Unlock()
	var lastErr error
	for _, base := range c.Bases {
		lines, err := c.fetchLines(ctx, base+name)
		if err == nil {
			c.mu.Lock()
			if c.files == nil {
				c.files = map[string]cachedFile{}
			}
			c.files[name] = cachedFile{lines: lines, at: time.Now()}
			c.mu.Unlock()
			return lines, nil
		}
		if errors.Is(err, ErrNotFound) {
			return nil, err
		}
		lastErr = err
	}
	if lastErr == nil {
		lastErr = ErrNotFound
	}
	return nil, fmt.Errorf("连不上域名库:%w", lastErr)
}

func (c *Client) fetchLines(ctx context.Context, url string) ([]string, error) {
	req, err := http.NewRequestWithContext(ctx, http.MethodGet, url, nil)
	if err != nil {
		return nil, err
	}
	resp, err := c.HTTP.Do(req)
	if err != nil {
		return nil, err
	}
	defer resp.Body.Close()
	if resp.StatusCode == http.StatusNotFound {
		return nil, ErrNotFound
	}
	if resp.StatusCode != http.StatusOK {
		return nil, fmt.Errorf("HTTP %d", resp.StatusCode)
	}
	var lines []string
	sc := bufio.NewScanner(io.LimitReader(resp.Body, maxFileSz))
	for sc.Scan() {
		lines = append(lines, sc.Text())
	}
	return lines, sc.Err()
}

// suggest 名字相近的列表(拿不到索引就不推荐)。
func (c *Client) suggest(ctx context.Context, q string) []string {
	if len(q) < 2 || c.Index == "" {
		return nil
	}
	names := c.indexNames(ctx)
	var pre, sub []string
	for _, n := range names {
		switch {
		case strings.HasPrefix(n, q):
			pre = append(pre, n)
		case strings.Contains(n, q) || (len(n) >= 4 && strings.Contains(q, n)):
			sub = append(sub, n)
		}
	}
	sort.Strings(pre)
	sort.Strings(sub)
	out := append(pre, sub...)
	if len(out) > 6 {
		out = out[:6]
	}
	return out
}

func (c *Client) indexNames(ctx context.Context) []string {
	c.mu.Lock()
	if c.index != nil && time.Since(c.indexAt) < c.TTL {
		defer c.mu.Unlock()
		return c.index
	}
	c.mu.Unlock()
	req, err := http.NewRequestWithContext(ctx, http.MethodGet, c.Index, nil)
	if err != nil {
		return nil
	}
	resp, err := c.HTTP.Do(req)
	if err != nil {
		return nil
	}
	defer resp.Body.Close()
	var idx struct {
		Files []struct {
			Name string `json:"name"`
		} `json:"files"`
	}
	if resp.StatusCode != http.StatusOK || json.NewDecoder(io.LimitReader(resp.Body, 4<<20)).Decode(&idx) != nil {
		return nil
	}
	var names []string
	for _, f := range idx.Files {
		if n := strings.TrimPrefix(f.Name, "/data/"); n != f.Name && validName.MatchString(n) {
			names = append(names, n)
		}
	}
	c.mu.Lock()
	c.index, c.indexAt = names, time.Now()
	c.mu.Unlock()
	return names
}

// acc 收集结果:保持列表里的先后,去重;完整域名已被某个后缀盖住的不要。
type acc struct {
	suffix, full, keyword []string
	seen                  map[string]bool
	skipped               int
}

func newAcc() *acc { return &acc{seen: map[string]bool{}} }

// add kind:s = 域名后缀,f = 完整域名,k = 关键字
func (a *acc) add(kind, v string) {
	v = strings.Trim(v, ".")
	if v == "" || len(v) > 253 {
		return
	}
	if kind == "k" && !validKeyword.MatchString(v) || kind != "k" && !validDomain(v) {
		return
	}
	if a.seen[kind+v] {
		return
	}
	a.seen[kind+v] = true
	switch kind {
	case "s":
		a.suffix = append(a.suffix, v)
	case "f":
		a.full = append(a.full, v)
	default:
		a.keyword = append(a.keyword, v)
	}
}

func (a *acc) result() (suffix, full, keyword []string, skipped int) {
	set := make(map[string]bool, len(a.suffix))
	for _, s := range a.suffix {
		set[s] = true
	}
	covered := func(host string) bool {
		for h := host; ; {
			if set[h] {
				return true
			}
			i := strings.IndexByte(h, '.')
			if i < 0 {
				return false
			}
			h = h[i+1:]
		}
	}
	for _, f := range a.full {
		if !covered(f) {
			full = append(full, f)
		}
	}
	return a.suffix, full, a.keyword, a.skipped
}

func attrsOnly(f []string) bool {
	for _, x := range f {
		if !strings.HasPrefix(x, "@") {
			return false
		}
	}
	return true
}

// 域名:一段段字母数字(可带 - 与 _);列表里也有整个顶级域(比如 google)。不收 IP。
var (
	domainRE     = regexp.MustCompile(`^[a-z0-9_]([a-z0-9_-]*[a-z0-9_])?(\.[a-z0-9_]([a-z0-9_-]*[a-z0-9_])?)*$`)
	validKeyword = regexp.MustCompile(`^[a-z0-9._-]+$`)
)

func validDomain(v string) bool {
	if _, err := netip.ParseAddr(v); err == nil {
		return false
	}
	return domainRE.MatchString(v)
}
