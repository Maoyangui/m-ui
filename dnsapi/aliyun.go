package dnsapi

import (
	"context"
	"crypto/hmac"
	"crypto/rand"
	"crypto/sha1"
	"encoding/base64"
	"encoding/hex"
	"encoding/json"
	"fmt"
	"net/http"
	"net/url"
	"sort"
	"strconv"
	"strings"
	"time"
)

// 阿里云云解析(alidns,RPC 风格接口 2015-01-09,签名方式 HMAC-SHA1 / SignatureVersion 1.0)。
// 查记录用 DescribeSubDomainRecords(给完整域名就行,不用先找主域名),改值用 UpdateDomainRecord(要带原来的 RR、类型、线路、TTL),
// 加 TXT 先用 GetMainDomainName 拆出主域名和主机记录,再 AddDomainRecord;删用 DeleteDomainRecord。

type aliyun struct {
	id, secret, base string
	now              func() time.Time // 测试固定时间
}

// aliyunEncode 阿里云要的百分号编码(RFC 3986:除字母数字和 -_.~ 都编码,空格是 %20)。
func aliyunEncode(s string) string {
	return strings.NewReplacer("+", "%20", "*", "%2A", "%7E", "~").Replace(url.QueryEscape(s))
}

// aliyunSign 按 RPC 签名规则算签名:参数按名字排序、编码后拼起来,StringToSign = GET&%2F&编码(拼好的串),
// HMAC-SHA1(AccessKeySecret + "&"),再 base64。
func aliyunSign(method string, params map[string]string, secret string) string {
	keys := make([]string, 0, len(params))
	for k := range params {
		keys = append(keys, k)
	}
	sort.Strings(keys)
	parts := make([]string, 0, len(keys))
	for _, k := range keys {
		parts = append(parts, aliyunEncode(k)+"="+aliyunEncode(params[k]))
	}
	toSign := method + "&" + aliyunEncode("/") + "&" + aliyunEncode(strings.Join(parts, "&"))
	mac := hmac.New(sha1.New, []byte(secret+"&"))
	mac.Write([]byte(toSign))
	return base64.StdEncoding.EncodeToString(mac.Sum(nil))
}

func (a *aliyun) call(ctx context.Context, action string, args map[string]string, out interface{}) error {
	now := time.Now
	if a.now != nil {
		now = a.now
	}
	nonce := make([]byte, 12)
	_, _ = rand.Read(nonce)
	params := map[string]string{
		"Format": "JSON", "Version": "2015-01-09", "AccessKeyId": a.id, "SignatureMethod": "HMAC-SHA1",
		"Timestamp": now().UTC().Format("2006-01-02T15:04:05Z"), "SignatureVersion": "1.0", "SignatureNonce": hex.EncodeToString(nonce),
		"Action": action,
	}
	for k, v := range args {
		params[k] = v
	}
	params["Signature"] = aliyunSign(http.MethodGet, params, a.secret)
	keys := make([]string, 0, len(params))
	for k := range params {
		keys = append(keys, k)
	}
	sort.Strings(keys)
	parts := make([]string, 0, len(keys))
	for _, k := range keys { // 发出去的查询串和签名时用的同一种编码
		parts = append(parts, aliyunEncode(k)+"="+aliyunEncode(params[k]))
	}
	req, err := http.NewRequestWithContext(ctx, http.MethodGet, a.base+"?"+strings.Join(parts, "&"), nil)
	if err != nil {
		return err
	}
	resp, err := httpClient.Do(req)
	if err != nil {
		return fmt.Errorf("阿里云: %w", err)
	}
	defer resp.Body.Close()
	body, err := readBody(resp)
	if err != nil {
		return fmt.Errorf("阿里云: %w", err)
	}
	if resp.StatusCode != http.StatusOK {
		var e struct{ Code, Message string }
		if json.Unmarshal(body, &e) == nil && e.Code != "" {
			return fmt.Errorf("阿里云: %s %s", e.Code, e.Message)
		}
		return fmt.Errorf("阿里云: HTTP %d %s", resp.StatusCode, brief(body))
	}
	if out != nil {
		if err := json.Unmarshal(body, out); err != nil {
			return fmt.Errorf("阿里云: 回复看不懂(%s)", brief(body))
		}
	}
	return nil
}

type aliyunRecord struct {
	RecordId, RR, Type, Value, Line, DomainName string
	TTL                                         int
}

func (a *aliyun) Records(ctx context.Context, name, typ string) ([]Record, error) {
	var out struct {
		DomainRecords struct{ Record []aliyunRecord }
	}
	args := map[string]string{"SubDomain": name, "PageSize": "500"}
	if typ != "" {
		args["Type"] = typ
	}
	if err := a.call(ctx, "DescribeSubDomainRecords", args, &out); err != nil {
		return nil, err
	}
	recs := make([]Record, 0, len(out.DomainRecords.Record))
	for _, r := range out.DomainRecords.Record {
		if typ != "" && !strings.EqualFold(r.Type, typ) {
			continue
		}
		recs = append(recs, Record{ID: r.RecordId, Name: name, Type: r.Type, Content: r.Value, zone: r.DomainName, rr: r.RR, line: r.Line, ttl: r.TTL})
	}
	return recs, nil
}

func (a *aliyun) SetContent(ctx context.Context, r Record, content string) error {
	args := map[string]string{"RecordId": r.ID, "RR": r.rr, "Type": r.Type, "Value": content}
	if r.line != "" {
		args["Line"] = r.line
	}
	if r.ttl > 0 {
		args["TTL"] = strconv.Itoa(r.ttl)
	}
	return a.call(ctx, "UpdateDomainRecord", args, nil)
}

func (a *aliyun) AddTXT(ctx context.Context, name, value string) (Record, error) {
	var main struct{ DomainName, RR string }
	if err := a.call(ctx, "GetMainDomainName", map[string]string{"InputString": name}, &main); err != nil {
		return Record{}, err
	}
	if main.DomainName == "" {
		return Record{}, fmt.Errorf("阿里云: 找不到 %s 所属的域名", name)
	}
	rr := main.RR
	if rr == "" {
		rr = "@"
	}
	var out struct{ RecordId string }
	if err := a.call(ctx, "AddDomainRecord", map[string]string{"DomainName": main.DomainName, "RR": rr, "Type": "TXT", "Value": value, "TTL": "600"}, &out); err != nil {
		return Record{}, err
	}
	return Record{ID: out.RecordId, Name: name, Type: "TXT", Content: value, zone: main.DomainName, rr: rr}, nil
}

func (a *aliyun) Delete(ctx context.Context, r Record) error {
	return a.call(ctx, "DeleteDomainRecord", map[string]string{"RecordId": r.ID}, nil)
}
