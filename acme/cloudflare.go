package acme

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net/http"
	"net/url"
	"strings"
	"time"
)

// Cloudflare 是 dns-01 与主机容灾改 DNS 所需的最小 API 客户端(读区域、增删 TXT、查与改 A / AAAA)。
type Cloudflare struct {
	Token   string
	BaseURL string // 测试用,默认官方地址
	Client  *http.Client
}

func (c *Cloudflare) base() string {
	if c.BaseURL != "" {
		return strings.TrimRight(c.BaseURL, "/")
	}
	return "https://api.cloudflare.com/client/v4"
}

func (c *Cloudflare) do(ctx context.Context, method, path string, body interface{}, out interface{}) error {
	var rd io.Reader
	if body != nil {
		b, _ := json.Marshal(body)
		rd = bytes.NewReader(b)
	}
	req, err := http.NewRequestWithContext(ctx, method, c.base()+path, rd)
	if err != nil {
		return err
	}
	req.Header.Set("Authorization", "Bearer "+c.Token)
	req.Header.Set("Content-Type", "application/json")
	cl := c.Client
	if cl == nil {
		cl = &http.Client{Timeout: 20 * time.Second}
	}
	resp, err := cl.Do(req)
	if err != nil {
		return err
	}
	defer resp.Body.Close()
	var env struct {
		Success bool                       `json:"success"`
		Errors  []struct{ Message string } `json:"errors"`
		Result  json.RawMessage            `json:"result"`
	}
	raw, _ := io.ReadAll(io.LimitReader(resp.Body, 1<<20))
	if err := json.Unmarshal(raw, &env); err != nil {
		return fmt.Errorf("cloudflare %d: %s", resp.StatusCode, strings.TrimSpace(string(raw)))
	}
	if !env.Success {
		msg := "unknown"
		if len(env.Errors) > 0 {
			msg = env.Errors[0].Message
		}
		return fmt.Errorf("cloudflare: %s", msg)
	}
	if out != nil {
		return json.Unmarshal(env.Result, out)
	}
	return nil
}

// ZoneFor 找到域名所属的区域:从整个域名开始逐级去掉左侧标签查询。
func (c *Cloudflare) ZoneFor(ctx context.Context, domain string) (id, name string, err error) {
	labels := strings.Split(domain, ".")
	for i := 0; i < len(labels)-1; i++ {
		cand := strings.Join(labels[i:], ".")
		var zones []struct{ ID, Name string }
		if err := c.do(ctx, "GET", "/zones?name="+cand, nil, &zones); err != nil {
			return "", "", err
		}
		if len(zones) > 0 {
			return zones[0].ID, zones[0].Name, nil
		}
	}
	return "", "", errors.New("此 Token 下没有找到该域名所属的 Cloudflare 区域")
}

// CreateTXT 新建 TXT 记录,返回记录 ID。
func (c *Cloudflare) CreateTXT(ctx context.Context, zone, name, content string) (string, error) {
	var rec struct{ ID string }
	err := c.do(ctx, "POST", "/zones/"+zone+"/dns_records", map[string]interface{}{
		"type": "TXT", "name": name, "content": content, "ttl": 60,
	}, &rec)
	return rec.ID, err
}

// DeleteRecord 删除记录。
func (c *Cloudflare) DeleteRecord(ctx context.Context, zone, id string) error {
	return c.do(ctx, "DELETE", "/zones/"+zone+"/dns_records/"+id, nil, nil)
}

// DNSRecord 一条 DNS 记录(主机容灾改 A / AAAA 用)。
type DNSRecord struct {
	ID      string `json:"id"`
	Type    string `json:"type"`
	Name    string `json:"name"`
	Content string `json:"content"`
	Proxied bool   `json:"proxied"`
	TTL     int    `json:"ttl"`
}

// ListRecords 列出区域里名字为 name、类型为 typ 的记录。
func (c *Cloudflare) ListRecords(ctx context.Context, zone, name, typ string) ([]DNSRecord, error) {
	var out []DNSRecord
	q := "/zones/" + zone + "/dns_records?per_page=100&name=" + url.QueryEscape(name)
	if typ != "" {
		q += "&type=" + url.QueryEscape(typ)
	}
	err := c.do(ctx, "GET", q, nil, &out)
	return out, err
}

// UpdateContent 只改一条记录的内容(PATCH):类型、名字、橙云、TTL 都保持原样。
func (c *Cloudflare) UpdateContent(ctx context.Context, zone, id, content string) error {
	return c.do(ctx, "PATCH", "/zones/"+zone+"/dns_records/"+id, map[string]interface{}{"content": content}, nil)
}
