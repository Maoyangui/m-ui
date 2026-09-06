package core

import (
	"context"
	"encoding/json"
	"time"

	urltest "github.com/sagernet/sing-box/common/urltest"
	"github.com/sagernet/sing-box/option"
)

const checkTimeout = 10 * time.Second

type CheckOutboundResult struct {
	OK    bool
	Delay uint16
	Error string
}

// CheckOutboundIsolated 用一个只含该出站(加 direct)的临时 sing-box 实例真实测一次:
// 没有入站、不监听端口,测完即关。给"本机没有线路用它、所以不在数据面里"的上游,以及数据面没起来时用。
func CheckOutboundIsolated(outbound json.RawMessage, link string) (result CheckOutboundResult) {
	var meta struct {
		Tag string `json:"tag"`
	}
	if json.Unmarshal(outbound, &meta) != nil || meta.Tag == "" {
		result.Error = "出站缺少 tag"
		return result
	}
	cfg, _ := json.Marshal(map[string]interface{}{
		"log":       map[string]interface{}{"level": "error"},
		"outbounds": []json.RawMessage{json.RawMessage(`{"type":"direct","tag":"direct"}`), outbound},
	})
	ctx := validationCtx()
	var opt option.Options
	if err := opt.UnmarshalJSONContext(ctx, cfg); err != nil {
		result.Error = err.Error()
		return result
	}
	// 和干跑一样要串行并保护包级日志工厂(NewBox 会改写它)
	validateMu.Lock()
	savedFactory := currentFactory()
	defer func() {
		setFactory(savedFactory)
		validateMu.Unlock()
	}()
	box, err := NewBox(Options{Context: ctx, Options: opt})
	if err != nil {
		result.Error = err.Error()
		return result
	}
	if err := box.Start(); err != nil {
		_ = box.Close()
		result.Error = err.Error()
		return result
	}
	defer box.Close()
	ob, ok := box.Outbound().Outbound(meta.Tag)
	if !ok {
		result.Error = "outbound not found"
		return result
	}
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
