package core

import (
	"context"
	"crypto/tls"
	"encoding/json"
	"errors"
	"fmt"
	"net"
	"net/http"
	"net/url"
	"time"

	"github.com/sagernet/sing-box/adapter"
	urltest "github.com/sagernet/sing-box/common/urltest"
	"github.com/sagernet/sing-box/option"
	M "github.com/sagernet/sing/common/metadata"
	N "github.com/sagernet/sing/common/network"
)

const checkTimeout = 10 * time.Second

// retestTimeout 测通之后再测一次取较小延迟时的时限。
const retestTimeout = 5 * time.Second

type CheckOutboundResult struct {
	OK    bool
	Delay uint16
	Error string
}

// startIsolated 起一个只含该出站(加 direct)的临时 sing-box 实例:没有入站、不监听端口。
// 只有建实例、启动这一段在 validateMu 里(NewBox 会改写包级日志工厂,干跑也走它,要串行并在之后恢复);
// 拿实例去连网络的部分在锁外做 —— 以前锁一直持有到测完(单个最长 10 秒),外部节点测速只能一个接一个,
// 期间面板保存的干跑、副机应用快照的干跑都得排队,事务里还占着数据库写锁。
func startIsolated(outbound json.RawMessage) (*Box, adapter.Outbound, error) {
	var meta struct {
		Tag string `json:"tag"`
	}
	if json.Unmarshal(outbound, &meta) != nil || meta.Tag == "" {
		return nil, nil, errors.New("出站缺少 tag")
	}
	cfg, _ := json.Marshal(map[string]interface{}{
		"log":       map[string]interface{}{"level": "error"},
		"outbounds": []json.RawMessage{json.RawMessage(`{"type":"direct","tag":"direct"}`), outbound},
	})
	ctx := validationCtx()
	var opt option.Options
	if err := opt.UnmarshalJSONContext(ctx, cfg); err != nil {
		return nil, nil, err
	}
	var box *Box
	err := func() error {
		validateMu.Lock()
		savedFactory := currentFactory()
		defer func() {
			setFactory(savedFactory)
			validateMu.Unlock()
		}()
		b, err := NewBox(Options{Context: ctx, Options: opt})
		if err != nil {
			return err
		}
		if err := b.Start(); err != nil {
			_ = b.Close()
			return err
		}
		box = b
		return nil
	}()
	if err != nil {
		return nil, nil, err
	}
	ob, ok := box.Outbound().Outbound(meta.Tag)
	if !ok {
		_ = box.Close()
		return nil, nil, errors.New("outbound not found")
	}
	return box, ob, nil
}

// CheckOutboundIsolated 用一个只含该出站的临时实例真实测一次:给"本机没有线路用它、所以不在数据面里"的上游,以及数据面没起来时用。
func CheckOutboundIsolated(outbound json.RawMessage, link string) (result CheckOutboundResult) {
	box, ob, err := startIsolated(outbound)
	if err != nil {
		result.Error = err.Error()
		return result
	}
	defer box.Close()
	tctx, cancel := context.WithTimeout(context.Background(), checkTimeout)
	defer cancel()
	delay, err := urltest.URLTest(tctx, link, ob)
	if err != nil {
		result.Error = err.Error()
		return result
	}
	result.OK, result.Delay = true, delay
	return result
}

// OutboundTest 一次隔离测速的结果。
type OutboundTest struct {
	OK      bool
	DelayMs int
	Config  bool // 出站本身建不起来(参数 sing-box 不认):测不了,不等于不通
	Error   string
}

// TestOutboundIsolated 外部节点测速:临时实例只含这个出站,经它向 testURL 发 HEAD。
// 第一次通了再测一次取较小的延迟(第一次常含域名解析、握手);代理自己回 5xx 算不通。ctx 取消立即返回(面板上点了停止)。
// 网络部分不持锁,多个可以并发。
func TestOutboundIsolated(ctx context.Context, outbound json.RawMessage, testURL string, samples int) (res OutboundTest) {
	box, ob, err := startIsolated(outbound)
	if err != nil {
		res.Config, res.Error = true, err.Error()
		return res
	}
	defer box.Close()
	if samples < 1 {
		samples = 1
	}
	best := -1
	for i := 0; i < samples; i++ {
		limit := checkTimeout
		if i > 0 {
			limit = retestTimeout // 已经测通了,再测只为取个更准的延迟,别让一次慢响应拖到主机那边的请求超时
		}
		tctx, cancel := context.WithTimeout(ctx, limit)
		ms, err := probeHTTP(tctx, ob, testURL)
		cancel()
		if err != nil {
			if best < 0 {
				res.Error = err.Error()
			}
			break // 不通就别再等一轮超时;第二次没测成就用第一次的
		}
		if best < 0 || ms < best {
			best = ms
		}
	}
	if best >= 0 {
		res.OK, res.DelayMs, res.Error = true, best, ""
	}
	return res
}

// probeHTTP 经 detour 向 link 发一次 HEAD,返回从发出请求到收到响应头的毫秒数(与 sing-box 的 URLTest 同一算法,另外看状态码)。
func probeHTTP(ctx context.Context, detour N.Dialer, link string) (int, error) {
	if link == "" {
		link = "https://www.gstatic.com/generate_204"
	}
	u, err := url.Parse(link)
	if err != nil {
		return 0, err
	}
	port := u.Port()
	if port == "" {
		port = "80"
		if u.Scheme == "https" {
			port = "443"
		}
	}
	start := time.Now()
	conn, err := detour.DialContext(ctx, N.NetworkTCP, M.ParseSocksaddrHostPortStr(u.Hostname(), port))
	if err != nil {
		return 0, err
	}
	defer conn.Close()
	if N.NeedHandshakeForWrite(conn) {
		start = time.Now()
	}
	req, err := http.NewRequestWithContext(ctx, http.MethodHead, link, nil)
	if err != nil {
		return 0, err
	}
	client := http.Client{
		Transport: &http.Transport{
			DialContext:     func(context.Context, string, string) (net.Conn, error) { return conn, nil },
			TLSClientConfig: &tls.Config{},
		},
		CheckRedirect: func(*http.Request, []*http.Request) error { return http.ErrUseLastResponse },
	}
	defer client.CloseIdleConnections()
	resp, err := client.Do(req)
	if err != nil {
		return 0, err
	}
	resp.Body.Close()
	if resp.StatusCode >= 500 {
		return 0, fmt.Errorf("HTTP %d", resp.StatusCode)
	}
	return int(time.Since(start) / time.Millisecond), nil
}

// OutboundNow 上游组现在走的是哪一条;不是组、没在跑、还没测出结果时返回空。
func OutboundNow(tag string) string {
	if outbound_manager == nil {
		return ""
	}
	ob, ok := outbound_manager.Outbound(tag)
	if !ok {
		return ""
	}
	if g, ok := ob.(adapter.OutboundGroup); ok {
		return g.Now()
	}
	return ""
}

func CheckOutbound(ctx context.Context, tag string, link string) (result CheckOutboundResult) {
	if outbound_manager == nil {
		result.Error = "core not running"
		return result
	}
	ob, ok := outbound_manager.Outbound(tag)
	if !ok {
		result.Error = "outbound not found"
		return result
	}

	ctx, cancel := context.WithTimeout(ctx, checkTimeout)
	defer cancel()

	delay, err := urltest.URLTest(ctx, link, ob)
	if err != nil {
		result.Error = err.Error()
		return result
	}
	result.OK = true
	result.Delay = delay
	return result
}
