package dnsapi

import (
	"context"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"io"
	"net/http"
	"net/http/httptest"
	"net/url"
	"strconv"
	"strings"
	"sync"
	"testing"
	"time"
)

// 阿里云 RPC 签名:官方文档「签名机制」里的示例(AccessKeySecret = testsecret)算出来必须一字不差。
func TestAliyunSignatureDocExample(t *testing.T) {
	p := map[string]string{"AccessKeyId": "testid", "Action": "DescribeDomainRecords", "DomainName": "example.com", "Format": "XML",
		"SignatureMethod": "HMAC-SHA1", "SignatureNonce": "f59ed6a9-83fc-473b-9cc6-99c95df3856e", "SignatureVersion": "1.0",
		"Timestamp": "2016-03-24T16:41:54Z", "Version": "2015-01-09"}
	if got := aliyunSign("GET", p, "testsecret"); got != "uRpHwaSEt3J+6KQD//svCh/x+pI=" {
		t.Fatalf("签名和文档示例不一致: %s", got)
	}
}

// 腾讯云 TC3 规范请求:官方文档「签名方法 v3」的示例(请求体哈希、规范请求哈希)必须一致。
func TestTC3CanonicalDocExample(t *testing.T) {
	payload := []byte(`{"Limit": 1, "Filters": [{"Values": ["unnamed"], "Name": "instance-name"}]}`)
	sum := sha256.Sum256(payload)
	if hex.EncodeToString(sum[:]) != "99d58dfbc6745f6747f36bfca17dee5e6881dc0428a0a36f96199342bc5b4907" {
		t.Fatalf("前提:请求体和文档的不一样")
	}
	c := sha256.Sum256([]byte(tc3Canonical("cvm.tencentcloudapi.com", payload)))
	if got := hex.EncodeToString(c[:]); got != "2815843035062fffda5fd6f2a44ea8a34818b0dc46f024b8b3786976a3adda7a" {
		t.Fatalf("规范请求和文档示例不一致: %s", got)
	}
	auth := tc3Sign("AKIDtest", "secret", "dnspod", "dnspod.tencentcloudapi.com", 1551113065, payload)
	if !strings.HasPrefix(auth, "TC3-HMAC-SHA256 Credential=AKIDtest/2019-02-25/dnspod/tc3_request, SignedHeaders=content-type;host, Signature=") {
		t.Fatalf("Authorization 头格式不对: %s", auth)
	}
}

// 阿里云:查、改(带原来的 RR、线路、TTL)、加 TXT(先拆主域名)、删;每个请求的签名都要验得过;出错带上错误码。
func TestAliyunFlow(t *testing.T) {
	var mu sync.Mutex
	var calls []url.Values
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		q := r.URL.Query()
		params := map[string]string{}
		for k := range q {
			if k != "Signature" {
				params[k] = q.Get(k)
			}
		}
		if q.Get("AccessKeyId") != "AK" || aliyunSign("GET", params, "SK") != q.Get("Signature") {
			w.WriteHeader(400)
			w.Write([]byte(`{"Code":"SignatureDoesNotMatch","Message":"bad signature"}`))
			return
		}
		mu.Lock()
		calls = append(calls, q)
		mu.Unlock()
		switch q.Get("Action") {
		case "DescribeSubDomainRecords":
			w.Write([]byte(`{"TotalCount":2,"DomainRecords":{"Record":[{"RR":"sub","Line":"telecom","Type":"A","DomainName":"example.com","Value":"203.0.113.1","RecordId":"111","TTL":600},{"RR":"sub","Line":"default","Type":"A","DomainName":"example.com","Value":"203.0.113.9","RecordId":"112","TTL":600}]}}`))
		case "UpdateDomainRecord":
			w.Write([]byte(`{"RecordId":"111"}`))
		case "GetMainDomainName":
			w.Write([]byte(`{"DomainName":"example.com","RR":"_acme-challenge.sub","DomainLevel":3}`))
		case "AddDomainRecord":
			w.Write([]byte(`{"RecordId":"999"}`))
		case "DeleteDomainRecord":
			w.Write([]byte(`{"RecordId":"999"}`))
		default:
			w.WriteHeader(400)
			w.Write([]byte(`{"Code":"InvalidAction","Message":"no"}`))
		}
	}))
	defer srv.Close()
	p, err := New(Aliyun, " AK , SK ", srv.URL+"/")
	if err != nil {
		t.Fatal(err)
	}
	ctx := context.Background()
	recs, err := p.Records(ctx, "sub.example.com", "A")
	if err != nil || len(recs) != 2 || recs[0].Content != "203.0.113.1" {
		t.Fatalf("查记录: %v %+v", err, recs)
	}
	if err := p.SetContent(ctx, recs[0], "198.51.100.7"); err != nil {
		t.Fatal(err)
	}
	up := calls[len(calls)-1]
	if up.Get("Action") != "UpdateDomainRecord" || up.Get("RecordId") != "111" || up.Get("RR") != "sub" || up.Get("Type") != "A" ||
		up.Get("Value") != "198.51.100.7" || up.Get("Line") != "telecom" || up.Get("TTL") != "600" {
		t.Fatalf("改值要带原来的 RR、线路、TTL: %v", up)
	}
	r, err := p.AddTXT(ctx, "_acme-challenge.sub.example.com", "tok-en")
	if err != nil || r.ID != "999" {
		t.Fatalf("加 TXT: %v %+v", err, r)
	}
	add := calls[len(calls)-1]
	if add.Get("Action") != "AddDomainRecord" || add.Get("DomainName") != "example.com" || add.Get("RR") != "_acme-challenge.sub" || add.Get("Type") != "TXT" {
		t.Fatalf("加 TXT 要按主域名 + 主机记录: %v", add)
	}
	if err := p.Delete(ctx, r); err != nil || calls[len(calls)-1].Get("RecordId") != "999" {
		t.Fatalf("删: %v", err)
	}
	bad, _ := New(Aliyun, "AK,wrong", srv.URL+"/")
	if _, err := bad.Records(ctx, "sub.example.com", "A"); err == nil || !strings.Contains(err.Error(), "SignatureDoesNotMatch") {
		t.Fatalf("签名不对要报阿里云的错误码: %v", err)
	}
}

// 腾讯云 DNSPod:按最长的域名拆主机记录;没记录是空列表;改值带原来的线路、TTL;加、删 TXT;签名验得过;错误带上错误码。
func TestDNSPodFlow(t *testing.T) {
	var mu sync.Mutex
	type call struct {
		action string
		body   map[string]interface{}
	}
	var calls []call
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		payload, _ := io.ReadAll(r.Body)
		ts, _ := strconv.ParseInt(r.Header.Get("X-TC-Timestamp"), 10, 64)
		u, _ := url.Parse("http://" + r.Host)
		if r.Header.Get("Authorization") != tc3Sign("SID", "SKEY", "dnspod", u.Host, ts, payload) || r.Header.Get("X-TC-Version") != dnspodVersion {
			w.Write([]byte(`{"Response":{"Error":{"Code":"AuthFailure.SignatureFailure","Message":"bad"},"RequestId":"x"}}`))
			return
		}
		var body map[string]interface{}
		json.Unmarshal(payload, &body)
		action := r.Header.Get("X-TC-Action")
		mu.Lock()
		calls = append(calls, call{action, body})
		mu.Unlock()
		switch action {
		case "DescribeDomainList":
			w.Write([]byte(`{"Response":{"DomainList":[{"Name":"example.com"},{"Name":"sub.example.com"},{"Name":"other.net"}],"RequestId":"x"}}`))
		case "DescribeRecordList":
			if body["Subdomain"] == "none" {
				w.Write([]byte(`{"Response":{"Error":{"Code":"ResourceNotFound.NoDataOfRecord","Message":"none"},"RequestId":"x"}}`))
				return
			}
			w.Write([]byte(`{"Response":{"RecordList":[{"RecordId":4321,"Name":"a","Type":"A","Value":"203.0.113.1","Line":"电信","LineId":"10=0","TTL":600}],"RequestId":"x"}}`))
		case "ModifyRecord", "DeleteRecord":
			w.Write([]byte(`{"Response":{"RequestId":"x"}}`))
		case "CreateRecord":
			w.Write([]byte(`{"Response":{"RecordId":777,"RequestId":"x"}}`))
		}
	}))
	defer srv.Close()
	p, err := New(DNSPod, "SID\nSKEY", srv.URL+"/")
	if err != nil {
		t.Fatal(err)
	}
	ctx := context.Background()
	recs, err := p.Records(ctx, "a.sub.example.com", "A")
	if err != nil || len(recs) != 1 || recs[0].ID != "4321" {
		t.Fatalf("查记录: %v %+v", err, recs)
	}
	q := calls[len(calls)-1]
	if q.body["Domain"] != "sub.example.com" || q.body["Subdomain"] != "a" {
		t.Fatalf("要按最长的域名拆主机记录: %v", q.body)
	}
	if err := p.SetContent(ctx, recs[0], "198.51.100.7"); err != nil {
		t.Fatal(err)
	}
	m := calls[len(calls)-1]
	if m.action != "ModifyRecord" || m.body["RecordLine"] != "电信" || m.body["RecordLineId"] != "10=0" || m.body["Value"] != "198.51.100.7" ||
		m.body["RecordId"] != float64(4321) || m.body["SubDomain"] != "a" || m.body["TTL"] != float64(600) {
		t.Fatalf("改值要带原来的线路、TTL: %v", m.body)
	}
	if recs, err := p.Records(ctx, "none.example.com", "A"); err != nil || len(recs) != 0 {
		t.Fatalf("没有记录应是空列表: %v %v", err, recs)
	}
	r, err := p.AddTXT(ctx, "_acme-challenge.example.com", "v")
	if err != nil || r.ID != "777" || calls[len(calls)-1].body["SubDomain"] != "_acme-challenge" || calls[len(calls)-1].body["Domain"] != "example.com" {
		t.Fatalf("加 TXT: %v %+v %v", err, r, calls[len(calls)-1].body)
	}
	if err := p.Delete(ctx, r); err != nil || calls[len(calls)-1].action != "DeleteRecord" {
		t.Fatalf("删: %v", err)
	}
	if _, err := p.Records(ctx, "x.unknown.org", "A"); err == nil || !strings.Contains(err.Error(), "没有") {
		t.Fatalf("账号下没有的域名要说清: %v", err)
	}
	bad, _ := New(DNSPod, "SID,wrong", srv.URL+"/")
	if _, err := bad.Records(ctx, "a.example.com", "A"); err == nil || !strings.Contains(err.Error(), "AuthFailure") {
		t.Fatalf("签名不对要报腾讯云的错误码: %v", err)
	}
}

// Cloudflare:找区域、查、改(PATCH 只改内容)、加删 TXT。
func TestCloudflareFlow(t *testing.T) {
	var patched, posted, deleted string
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.Header.Get("Authorization") != "Bearer TOK" {
			w.Write([]byte(`{"success":false,"errors":[{"message":"Invalid API Token"}]}`))
			return
		}
		ok := func(v string) { w.Write([]byte(`{"success":true,"result":` + v + `}`)) }
		switch {
		case r.Method == "GET" && strings.HasPrefix(r.URL.Path, "/zones") && !strings.Contains(r.URL.Path, "dns_records"):
			if r.URL.Query().Get("name") == "example.com" {
				ok(`[{"id":"Z1","name":"example.com"}]`)
			} else {
				ok(`[]`)
			}
		case r.Method == "GET":
			ok(`[{"id":"R1","type":"A","name":"sub.example.com","content":"203.0.113.1","proxied":true,"ttl":1}]`)
		case r.Method == "PATCH":
			b, _ := io.ReadAll(r.Body)
			patched = r.URL.Path + " " + string(b)
			ok(`{}`)
		case r.Method == "POST":
			b, _ := io.ReadAll(r.Body)
			posted = string(b)
			ok(`{"id":"T1"}`)
		case r.Method == "DELETE":
			deleted = r.URL.Path
			ok(`{}`)
		}
	}))
	defer srv.Close()
	p, err := New("", "TOK", srv.URL)
	if err != nil {
		t.Fatal(err)
	}
	ctx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
	defer cancel()
	recs, err := p.Records(ctx, "sub.example.com", "A")
	if err != nil || len(recs) != 1 || !recs[0].Proxied {
		t.Fatalf("查记录: %v %+v", err, recs)
	}
	if err := p.SetContent(ctx, recs[0], "198.51.100.7"); err != nil || !strings.Contains(patched, "/zones/Z1/dns_records/R1") || !strings.Contains(patched, `"content":"198.51.100.7"`) {
		t.Fatalf("改值: %v %s", err, patched)
	}
	r, err := p.AddTXT(ctx, "_acme-challenge.sub.example.com", "v")
	if err != nil || r.ID != "T1" || !strings.Contains(posted, `"type":"TXT"`) {
		t.Fatalf("加 TXT: %v %s", err, posted)
	}
	if err := p.Delete(ctx, r); err != nil || deleted != "/zones/Z1/dns_records/T1" {
		t.Fatalf("删: %v %s", err, deleted)
	}
}

// 凭据格式:缺一半、不支持的服务商都要说清。
func TestNewCredentials(t *testing.T) {
	for _, c := range []struct{ kind, cred, want string }{
		{Cloudflare, "", "Cloudflare"}, {Aliyun, "onlyid", "AccessKey"}, {DNSPod, "id,", "SecretId"}, {"godaddy", "x", "不支持"},
	} {
		if _, err := New(c.kind, c.cred, ""); err == nil || !strings.Contains(err.Error(), c.want) {
			t.Fatalf("%s %q 应报含 %q 的错: %v", c.kind, c.cred, c.want, err)
		}
	}
	if !Valid("") || !Valid(Aliyun) || Valid("godaddy") {
		t.Fatal("Valid 判断不对")
	}
}
