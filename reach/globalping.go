package reach

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net/http"
	"sort"
	"strconv"
	"strings"
	"sync"
	"time"
)

// DefaultAPI Globalping 公共测试网络的接口地址。免登录按发起请求的 IP 计额度(每小时约 250 个测点次),
// 填了令牌按账号计,额度更高。
const DefaultAPI = "https://api.globalping.io/v1"

// probe 是 /probes 列表里的一个测点。
type probe struct {
	Location struct {
		Country string `json:"country"`
		City    string `json:"city"`
		ASN     int    `json:"asn"`
		Network string `json:"network"`
	} `json:"location"`
	Tags []string `json:"tags"`
}

func (p probe) hasTag(t string) bool {
	for _, x := range p.Tags {
		if x == t {
			return true
		}
	}
	return false
}

// location 测量请求里的选点条件。同一对象里的各字段是"且";limit 是这个条件最多取几个测点。
type location struct {
	Country string   `json:"country,omitempty"`
	City    string   `json:"city,omitempty"`
	ASN     int      `json:"asn,omitempty"`
	Tags    []string `json:"tags,omitempty"`
	Limit   int      `json:"limit,omitempty"`
}

type pingOptions struct {
	Packets  int    `json:"packets"`
	Protocol string `json:"protocol,omitempty"` // 空 = ICMP;"TCP" = 连端口(三次握手)
	Port     int    `json:"port,omitempty"`
}

type measureRequest struct {
	Type    string      `json:"type"`
	Target  string      `json:"target"`
	Locs    interface{} `json:"locations"` // []location,或上一次测量的 id(复用同一批测点)
	Options pingOptions `json:"measurementOptions"`
}

// measurement GET /measurements/:id 的返回。只取用得到的字段。
type measurement struct {
	Id      string `json:"id"`
	Status  string `json:"status"`
	Results []struct {
		Probe struct {
			Country string   `json:"country"`
			City    string   `json:"city"`
			ASN     int      `json:"asn"`
			Network string   `json:"network"`
			Tags    []string `json:"tags"`
		} `json:"probe"`
		Result struct {
			Status string `json:"status"` // finished / failed / offline / in-progress
			Stats  *struct {
				Avg   *float64 `json:"avg"`
				Loss  float64  `json:"loss"`
				Rcv   int      `json:"rcv"`
				Total int      `json:"total"`
			} `json:"stats"`
		} `json:"result"`
	} `json:"results"`
}

// Quota 最近一次从响应头读到的额度。
type Quota struct {
	Limit     int   `json:"limit"`
	Remaining int   `json:"remaining"`
	ResetAt   int64 `json:"resetAt"` // unix 秒
	At        int64 `json:"at"`      // 读到的时间
}

// QuotaError 额度用完(HTTP 429,或剩余额度不够测一台)。Wait 是离恢复还有多久,0 = 不知道。
type QuotaError struct{ Wait time.Duration }

func (e *QuotaError) Error() string {
	switch {
	case e.Wait <= 0:
		return "测试额度已用完"
	case e.Wait < time.Minute:
		return "测试额度已用完,1 分钟内恢复"
	}
	return fmt.Sprintf("测试额度已用完,约 %d 分钟后恢复", int(e.Wait.Minutes()+0.5))
}

type client struct {
	base  string
	http  *http.Client
	token func() string
	ua    string
	now   func() time.Time

	mu    sync.Mutex
	quota Quota
}

func (c *client) Quota() Quota {
	c.mu.Lock()
	defer c.mu.Unlock()
	return c.quota
}

func (c *client) noteQuota(h http.Header) {
	lim, err1 := strconv.Atoi(h.Get("X-RateLimit-Limit"))
	rem, err2 := strconv.Atoi(h.Get("X-RateLimit-Remaining"))
	if err1 != nil || err2 != nil {
		return
	}
	q := Quota{Limit: lim, Remaining: rem, At: c.now().Unix()}
	if sec, err := strconv.Atoi(h.Get("X-RateLimit-Reset")); err == nil {
		q.ResetAt = c.now().Unix() + int64(sec)
	}
	c.mu.Lock()
	c.quota = q
	c.mu.Unlock()
}

func (c *client) do(ctx context.Context, method, path string, body, out interface{}) error {
	var rd io.Reader
	if body != nil {
		b, err := json.Marshal(body)
		if err != nil {
			return err
		}
		rd = bytes.NewReader(b)
	}
	req, err := http.NewRequestWithContext(ctx, method, strings.TrimSuffix(c.base, "/")+path, rd)
	if err != nil {
		return err
	}
	req.Header.Set("Accept", "application/json")
	if body != nil {
		req.Header.Set("Content-Type", "application/json")
	}
	if c.ua != "" {
		req.Header.Set("User-Agent", c.ua)
	}
	if tok := strings.TrimSpace(c.token()); tok != "" {
		req.Header.Set("Authorization", "Bearer "+tok)
	}
	resp, err := c.http.Do(req)
	if err != nil {
		return err
	}
	defer resp.Body.Close()
	if method == http.MethodPost {
		c.noteQuota(resp.Header)
	}
	data, err := io.ReadAll(io.LimitReader(resp.Body, 8<<20))
	if err != nil {
		return err
	}
	if resp.StatusCode == http.StatusTooManyRequests {
		qe := &QuotaError{}
		var resetAt int64
		if sec, err := strconv.Atoi(resp.Header.Get("X-RateLimit-Reset")); err == nil && sec > 0 {
			qe.Wait = time.Duration(sec) * time.Second
			resetAt = c.now().Unix() + int64(sec)
		}
		c.mu.Lock()
		c.quota.Remaining, c.quota.ResetAt, c.quota.At = 0, resetAt, c.now().Unix()
		c.mu.Unlock()
		return qe
	}
	if resp.StatusCode == http.StatusUnauthorized || resp.StatusCode == http.StatusForbidden {
		return errors.New("Globalping 令牌无效或无权限,请在设置里检查")
	}
	if resp.StatusCode/100 != 2 {
		var e struct {
			Error struct {
				Message string            `json:"message"`
				Params  map[string]string `json:"params"`
			} `json:"error"`
		}
		_ = json.Unmarshal(data, &e)
		if msg := e.Error.Params["target"]; msg != "" {
			// 最常见的是地址不是公网地址(内网、保留段):说人话
			if strings.Contains(msg, "private") {
				return errors.New("只能测公网 IP:这个地址是内网或保留地址")
			}
			return fmt.Errorf("Globalping 不接受这个地址:%s", msg)
		}
		if e.Error.Message != "" {
			detail := make([]string, 0, len(e.Error.Params))
			for k, v := range e.Error.Params {
				detail = append(detail, k+": "+v)
			}
			sort.Strings(detail)
			if len(detail) > 0 {
				return fmt.Errorf("Globalping %d: %s(%s)", resp.StatusCode, e.Error.Message, strings.Join(detail, ";"))
			}
			return fmt.Errorf("Globalping %d: %s", resp.StatusCode, e.Error.Message)
		}
		return fmt.Errorf("Globalping 返回 %d", resp.StatusCode)
	}
	if out == nil {
		return nil
	}
	return json.Unmarshal(data, out)
}

func (c *client) probes(ctx context.Context) ([]probe, error) {
	var out []probe
	if err := c.do(ctx, http.MethodGet, "/probes", nil, &out); err != nil {
		return nil, err
	}
	return out, nil
}

// measure 发起一次测量并等它结束。poll 是两次查询之间的间隔。
func (c *client) measure(ctx context.Context, req measureRequest, poll time.Duration) (*measurement, error) {
	var created struct {
		Id          string `json:"id"`
		ProbesCount int    `json:"probesCount"`
	}
	if err := c.do(ctx, http.MethodPost, "/measurements", req, &created); err != nil {
		return nil, err
	}
	if created.Id == "" {
		return nil, errors.New("Globalping 没有返回测量编号")
	}
	for {
		select {
		case <-ctx.Done():
			return nil, fmt.Errorf("等待测量结果超时: %w", ctx.Err())
		case <-time.After(poll):
		}
		var m measurement
		if err := c.do(ctx, http.MethodGet, "/measurements/"+created.Id, nil, &m); err != nil {
			return nil, err
		}
		if m.Status != "in-progress" {
			m.Id = created.Id
			return &m, nil
		}
	}
}
