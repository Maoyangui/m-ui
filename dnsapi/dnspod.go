package dnsapi

import (
	"bytes"
	"context"
	"crypto/hmac"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"fmt"
	"net/http"
	"net/url"
	"strconv"
	"strings"
	"sync"
	"time"
)

// 腾讯云 DNSPod(云 API 3.0,版本 2021-03-23,签名方式 TC3-HMAC-SHA256)。
// 先用 DescribeDomainList 找出名字属于哪个域名(取最长的),再按「主机记录」查(DescribeRecordList)、改(ModifyRecord,
// 线路、TTL 照原来的)、加 TXT(CreateRecord,默认线路)、删(DeleteRecord)。

type dnspod struct {
	id, secret, base string
	now              func() time.Time // 测试固定时间

	mu    sync.Mutex
	zones []string // 账号下的域名(一次取全,这个 Provider 用完就丢)
}

const dnspodVersion = "2021-03-23"

// tc3Canonical 规范请求:POST /,只签 content-type 与 host 两个头,最后一行是请求体的 SHA-256(和官方文档的示例一致,见测试)。
func tc3Canonical(host string, payload []byte) string {
	sum := sha256.Sum256(payload)
	return "POST\n/\n\ncontent-type:application/json; charset=utf-8\nhost:" + host + "\n\ncontent-type;host\n" + hex.EncodeToString(sum[:])
}

// tc3Sign 腾讯云 API 3.0 的签名(TC3-HMAC-SHA256)。派生密钥的顺序和官方 SDK 一样:
// HMAC("TC3"+SecretKey, 日期) → HMAC(·, 服务名) → HMAC(·, "tc3_request") → HMAC(·, 待签串)。返回 Authorization 头。
func tc3Sign(secretID, secretKey, service, host string, ts int64, payload []byte) string {
	date := time.Unix(ts, 0).UTC().Format("2006-01-02")
	creq := sha256.Sum256([]byte(tc3Canonical(host, payload)))
	scope := date + "/" + service + "/tc3_request"
	toSign := "TC3-HMAC-SHA256\n" + strconv.FormatInt(ts, 10) + "\n" + scope + "\n" + hex.EncodeToString(creq[:])
	h := func(key []byte, msg string) []byte {
		m := hmac.New(sha256.New, key)
		m.Write([]byte(msg))
		return m.Sum(nil)
	}
	k := h(h(h([]byte("TC3"+secretKey), date), service), "tc3_request")
	sig := hex.EncodeToString(h(k, toSign))
	return "TC3-HMAC-SHA256 Credential=" + secretID + "/" + scope + ", SignedHeaders=content-type;host, Signature=" + sig
}

func (d *dnspod) call(ctx context.Context, action string, args map[string]interface{}, out interface{}) error {
	u, err := url.Parse(d.base)
	if err != nil {
		return err
	}
	payload, err := json.Marshal(args)
	if err != nil {
		return err
	}
	now := time.Now
	if d.now != nil {
		now = d.now
	}
	ts := now().Unix()
	req, err := http.NewRequestWithContext(ctx, http.MethodPost, d.base, bytes.NewReader(payload))
	if err != nil {
		return err
	}
	req.Header.Set("Content-Type", "application/json; charset=utf-8")
	req.Header.Set("X-TC-Action", action)
	req.Header.Set("X-TC-Version", dnspodVersion)
	req.Header.Set("X-TC-Timestamp", strconv.FormatInt(ts, 10))
	req.Header.Set("Authorization", tc3Sign(d.id, d.secret, "dnspod", u.Host, ts, payload))
	resp, err := httpClient.Do(req)
	if err != nil {
		return fmt.Errorf("腾讯云 DNSPod: %w", err)
	}
	defer resp.Body.Close()
	body, err := readBody(resp)
	if err != nil {
		return fmt.Errorf("腾讯云 DNSPod: %w", err)
	}
	var env struct {
		Response json.RawMessage
	}
	if json.Unmarshal(body, &env) != nil || len(env.Response) == 0 {
		return fmt.Errorf("腾讯云 DNSPod: HTTP %d %s", resp.StatusCode, brief(body))
	}
	var e struct {
		Error *struct{ Code, Message string }
	}
	if json.Unmarshal(env.Response, &e) == nil && e.Error != nil && e.Error.Code != "" {
		return &dnspodError{e.Error.Code, e.Error.Message}
	}
	if out != nil {
		if err := json.Unmarshal(env.Response, out); err != nil {
			return fmt.Errorf("腾讯云 DNSPod: 回复看不懂(%s)", brief(body))
		}
	}
	return nil
}

type dnspodError struct{ Code, Message string }

func (e *dnspodError) Error() string { return "腾讯云 DNSPod: " + e.Code + " " + e.Message }

// zoneOf 名字属于账号下哪个域名(最长的那个),和主机记录(域名本身是 @)。
func (d *dnspod) zoneOf(ctx context.Context, name string) (string, string, error) {
	d.mu.Lock()
	zones := d.zones
	d.mu.Unlock()
	if zones == nil {
		var out struct {
			DomainList []struct{ Name string }
		}
		if err := d.call(ctx, "DescribeDomainList", map[string]interface{}{"Type": "ALL", "Offset": 0, "Limit": 3000}, &out); err != nil {
			return "", "", err
		}
		for _, z := range out.DomainList {
			zones = append(zones, strings.ToLower(strings.TrimSuffix(z.Name, ".")))
		}
		d.mu.Lock()
		d.zones = zones
		d.mu.Unlock()
	}
	name = strings.ToLower(strings.TrimSuffix(name, "."))
	best := ""
	for _, z := range zones {
		if (name == z || strings.HasSuffix(name, "."+z)) && len(z) > len(best) {
			best = z
		}
	}
	if best == "" {
		return "", "", fmt.Errorf("腾讯云 DNSPod: 这个密钥下没有 %s 所属的域名", name)
	}
	sub := strings.TrimSuffix(strings.TrimSuffix(name, best), ".")
	if sub == "" {
		sub = "@"
	}
	return best, sub, nil
}

func (d *dnspod) Records(ctx context.Context, name, typ string) ([]Record, error) {
	zone, sub, err := d.zoneOf(ctx, name)
	if err != nil {
		return nil, err
	}
	args := map[string]interface{}{"Domain": zone, "Subdomain": sub, "Limit": 3000}
	if typ != "" {
		args["RecordType"] = typ
	}
	var out struct {
		RecordList []struct {
			RecordId          uint64
			Name, Type, Value string
			Line, LineId      string
			TTL               int
		}
	}
	if err := d.call(ctx, "DescribeRecordList", args, &out); err != nil {
		if de, ok := err.(*dnspodError); ok && de.Code == "ResourceNotFound.NoDataOfRecord" {
			return nil, nil // 没有这样的记录
		}
		return nil, err
	}
	recs := make([]Record, 0, len(out.RecordList))
	for _, r := range out.RecordList {
		if typ != "" && !strings.EqualFold(r.Type, typ) {
			continue
		}
		recs = append(recs, Record{ID: strconv.FormatUint(r.RecordId, 10), Name: name, Type: r.Type, Content: r.Value,
			zone: zone, rr: sub, line: r.Line, lineID: r.LineId, ttl: r.TTL})
	}
	return recs, nil
}

func (d *dnspod) SetContent(ctx context.Context, r Record, content string) error {
	id, err := strconv.ParseUint(r.ID, 10, 64)
	if err != nil {
		return fmt.Errorf("腾讯云 DNSPod: 记录编号不对 %q", r.ID)
	}
	line := r.line
	if line == "" {
		line = "默认"
	}
	args := map[string]interface{}{"Domain": r.zone, "SubDomain": r.rr, "RecordType": r.Type, "RecordLine": line, "Value": content, "RecordId": id}
	if r.lineID != "" {
		args["RecordLineId"] = r.lineID
	}
	if r.ttl > 0 {
		args["TTL"] = r.ttl
	}
	return d.call(ctx, "ModifyRecord", args, nil)
}

func (d *dnspod) AddTXT(ctx context.Context, name, value string) (Record, error) {
	zone, sub, err := d.zoneOf(ctx, name)
	if err != nil {
		return Record{}, err
	}
	var out struct{ RecordId uint64 }
	if err := d.call(ctx, "CreateRecord", map[string]interface{}{"Domain": zone, "SubDomain": sub, "RecordType": "TXT", "RecordLine": "默认", "Value": value, "TTL": 600}, &out); err != nil {
		return Record{}, err
	}
	return Record{ID: strconv.FormatUint(out.RecordId, 10), Name: name, Type: "TXT", Content: value, zone: zone, rr: sub}, nil
}

func (d *dnspod) Delete(ctx context.Context, r Record) error {
	id, err := strconv.ParseUint(r.ID, 10, 64)
	if err != nil {
		return fmt.Errorf("腾讯云 DNSPod: 记录编号不对 %q", r.ID)
	}
	return d.call(ctx, "DeleteRecord", map[string]interface{}{"Domain": r.zone, "RecordId": id}, nil)
}
