// Package dnsapi 几家域名解析服务商的接口:主机容灾接任后把要跟着主机走的域名指到新主机(查 A / AAAA、改值),
// 签证书走 DNS 验证时加、删 TXT。只做这几个动作,不碰别的记录。
//
// 支持:Cloudflare(API 令牌,权限 Zone.DNS:Edit)、阿里云云解析(AccessKey,权限 AliyunDNSFullAccess)、
// 腾讯云 DNSPod(API 密钥 SecretId / SecretKey,权限 QcloudDNSPodFullAccess)。
package dnsapi

import (
	"context"
	"errors"
	"fmt"
	"io"
	"net/http"
	"strings"
	"time"

	"github.com/Maoyangui/m-ui/acme"
)

// Record 一条解析记录。改值、删除要用的服务商内部字段不导出,原样带回去就行。
type Record struct {
	ID      string `json:"id"`
	Name    string `json:"name"`
	Type    string `json:"type"`
	Content string `json:"content"`
	Proxied bool   `json:"proxied,omitempty"` // Cloudflare 的橙云

	zone, rr, line, lineID string
	ttl                    int
}

// Provider 一家服务商。
type Provider interface {
	// Records 名字正好是 name、类型是 typ(A / AAAA / TXT)的记录。
	Records(ctx context.Context, name, typ string) ([]Record, error)
	// SetContent 只改一条记录的值:类型、线路、TTL、橙云都不动。
	SetContent(ctx context.Context, r Record, content string) error
	// AddTXT 加一条 TXT 记录(签证书的 DNS 验证用)。
	AddTXT(ctx context.Context, name, value string) (Record, error)
	// Delete 删掉一条记录(AddTXT 加的那条)。
	Delete(ctx context.Context, r Record) error
}

// 服务商代码。
const (
	Cloudflare = "cloudflare"
	Aliyun     = "aliyun"
	DNSPod     = "dnspod"
)

// Kinds 支持的服务商(界面按这个顺序列)。
var Kinds = []string{Cloudflare, Aliyun, DNSPod}

// Valid 是不是支持的服务商(空 = Cloudflare,老设置没有这一项)。
func Valid(kind string) bool {
	if kind == "" {
		return true
	}
	for _, k := range Kinds {
		if k == kind {
			return true
		}
	}
	return false
}

// Label 服务商的显示名(日志、通知用)。
func Label(kind string) string {
	switch kind {
	case Aliyun:
		return "阿里云"
	case DNSPod:
		return "腾讯云 DNSPod"
	}
	return "Cloudflare"
}

// New 按服务商和凭据建一个接口。cred:Cloudflare 是 API 令牌;阿里云是 "AccessKeyId,AccessKeySecret";
// 腾讯云 DNSPod 是 "SecretId,SecretKey"(逗号或换行隔开)。base 为空用官方地址(测试换成 httptest)。
func New(kind, cred, base string) (Provider, error) {
	cred = strings.TrimSpace(cred)
	switch kind {
	case "", Cloudflare:
		if cred == "" {
			return nil, errors.New("没填 Cloudflare API 令牌")
		}
		return &cloudflare{cf: &acme.Cloudflare{Token: cred, BaseURL: base}}, nil
	case Aliyun:
		id, secret, ok := splitPair(cred)
		if !ok {
			return nil, errors.New("阿里云要填 AccessKey ID 和 AccessKey Secret")
		}
		if base == "" {
			base = "https://alidns.aliyuncs.com/"
		}
		return &aliyun{id: id, secret: secret, base: base}, nil
	case DNSPod:
		id, secret, ok := splitPair(cred)
		if !ok {
			return nil, errors.New("腾讯云 DNSPod 要填 SecretId 和 SecretKey")
		}
		if base == "" {
			base = "https://dnspod.tencentcloudapi.com/"
		}
		return &dnspod{id: id, secret: secret, base: base}, nil
	}
	return nil, fmt.Errorf("不支持的 DNS 服务商 %q", kind)
}

// splitPair 拆 "id,secret"(逗号或换行隔开,两边去空白)。
func splitPair(s string) (string, string, bool) {
	i := strings.IndexAny(s, ",\n")
	if i < 0 {
		return "", "", false
	}
	a, b := strings.TrimSpace(s[:i]), strings.TrimSpace(s[i+1:])
	return a, b, a != "" && b != ""
}

// httpClient 各家接口共用:单次请求 20 秒为限(调用方的 ctx 管总时长)。
var httpClient = &http.Client{Timeout: 20 * time.Second}

// readBody 读回复(最多 1MB)。
func readBody(resp *http.Response) ([]byte, error) {
	return io.ReadAll(io.LimitReader(resp.Body, 1<<20))
}

// brief 截短的原文(回复不是预期的 JSON 时放进错误里)。
func brief(b []byte) string {
	s := strings.TrimSpace(string(b))
	if r := []rune(s); len(r) > 200 {
		s = string(r[:200]) + "…"
	}
	return s
}

// ---- Cloudflare:用 acme 包里现成的客户端 ----

type cloudflare struct{ cf *acme.Cloudflare }

func (c *cloudflare) Records(ctx context.Context, name, typ string) ([]Record, error) {
	zone, _, err := c.cf.ZoneFor(ctx, name)
	if err != nil {
		return nil, err
	}
	list, err := c.cf.ListRecords(ctx, zone, name, typ)
	if err != nil {
		return nil, err
	}
	out := make([]Record, 0, len(list))
	for _, r := range list {
		out = append(out, Record{ID: r.ID, Name: r.Name, Type: r.Type, Content: r.Content, Proxied: r.Proxied, zone: zone, ttl: r.TTL})
	}
	return out, nil
}

func (c *cloudflare) SetContent(ctx context.Context, r Record, content string) error {
	return c.cf.UpdateContent(ctx, r.zone, r.ID, content)
}

func (c *cloudflare) AddTXT(ctx context.Context, name, value string) (Record, error) {
	zone, _, err := c.cf.ZoneFor(ctx, name)
	if err != nil {
		return Record{}, err
	}
	id, err := c.cf.CreateTXT(ctx, zone, name, value)
	return Record{ID: id, Name: name, Type: "TXT", Content: value, zone: zone}, err
}

func (c *cloudflare) Delete(ctx context.Context, r Record) error {
	return c.cf.DeleteRecord(ctx, r.zone, r.ID)
}

// ACME 把 Provider 包成签证书 DNS 验证要的样子(见 acme.Config.DNS)。
func ACME(p Provider) acme.DNSProvider { return acmeAdapter{p} }

type acmeAdapter struct{ p Provider }

func (a acmeAdapter) AddTXT(ctx context.Context, name, value string) (func(context.Context) error, error) {
	r, err := a.p.AddTXT(ctx, name, value)
	if err != nil {
		return nil, err
	}
	return func(c context.Context) error { return a.p.Delete(c, r) }, nil
}
