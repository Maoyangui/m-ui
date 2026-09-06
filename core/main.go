package core

import (
	"context"
	"fmt"
	"sync"

	sb "github.com/sagernet/sing-box"
	"github.com/sagernet/sing-box/adapter"
	_ "github.com/sagernet/sing-box/experimental/clashapi"
	_ "github.com/sagernet/sing-box/experimental/v2rayapi"
	"github.com/sagernet/sing-box/log"
	"github.com/sagernet/sing-box/option"
	_ "github.com/sagernet/sing-box/transport/v2rayquic"
	"github.com/sagernet/sing/service"
)

var (
	globalCtx        context.Context
	inbound_manager  adapter.InboundManager
	outbound_manager adapter.OutboundManager
	service_manager  adapter.ServiceManager
	endpoint_manager adapter.EndpointManager
	router           adapter.Router

	// factory 是包级日志工厂:NewBox 会改写它,而"保存前干跑校验"也走 NewBox。
	// 干跑发生在 HTTP 处理协程,和运行中数据面创建入站/出站是并发的,所以要加锁;
	// validateMu 再把并发干跑串起来,免得两次干跑的保存/恢复交叉,把死实例的工厂留在进程里。
	factoryMu  sync.RWMutex
	factory    log.Factory
	validateMu sync.Mutex
)

func setFactory(f log.Factory) {
	factoryMu.Lock()
	factory = f
	factoryMu.Unlock()
}

func currentFactory() log.Factory {
	factoryMu.RLock()
	defer factoryMu.RUnlock()
	return factory
}

type Core struct {
	mu        sync.RWMutex // isRunning / instance 会被 HTTP 处理、定时任务、重载协程同时读写
	isRunning bool
	instance  *Box
}

func NewCore() *Core {
	globalCtx = context.Background()
	globalCtx = sb.Context(globalCtx, InboundRegistry(), OutboundRegistry(), EndpointRegistry(), DNSTransportRegistry(), ServiceRegistry(), CertificateProviderRegistry())
	return &Core{
		isRunning: false,
		instance:  nil,
	}
}

func (c *Core) GetCtx() context.Context {
	return globalCtx
}

func (c *Core) GetInstance() *Box {
	c.mu.RLock()
	defer c.mu.RUnlock()
	return c.instance
}

func (c *Core) Start(sbConfig []byte) error {
	var opt option.Options
	err := opt.UnmarshalJSONContext(globalCtx, sbConfig)
	if err != nil {
		// 以前只打日志就继续用一份空配置起数据面:零入站却显示"运行中",线路全停而面板毫无察觉。
		// 解析不过就是起不来,交给调用方回滚或报错。
		return fmt.Errorf("解析 sing-box 配置: %w", err)
	}

	box, err := NewBox(Options{
		Context: globalCtx,
		Options: opt,
	})
	if err != nil {
		return err
	}

	if err = box.Start(); err != nil {
		_ = box.Close()
		return err
	}

	globalCtx = service.ContextWith(globalCtx, c)
	inbound_manager = service.FromContext[adapter.InboundManager](globalCtx)
	outbound_manager = service.FromContext[adapter.OutboundManager](globalCtx)
	service_manager = service.FromContext[adapter.ServiceManager](globalCtx)
	endpoint_manager = service.FromContext[adapter.EndpointManager](globalCtx)
	router = service.FromContext[adapter.Router](globalCtx)

	c.mu.Lock()
	c.instance = box
	c.isRunning = true
	c.mu.Unlock()
	return nil
}

func (c *Core) Stop() error {
	c.mu.Lock()
	c.isRunning = false
	box := c.instance
	c.instance = nil
	c.mu.Unlock()
	if box == nil {
		return nil
	}
	return box.Close()
}

func (c *Core) IsRunning() bool {
	c.mu.RLock()
	defer c.mu.RUnlock()
	return c.isRunning
}

// SetLogEnabled 运行时调整数据面日志级别:关 = 只留 panic 级(什么都不会打),开 = info。
// 配置文本里的日志段保持不变,这样开关日志不会因"配置变了"而触发全量重启。
func (c *Core) SetLogEnabled(on bool) {
	box := c.GetInstance()
	if box == nil || box.logFactory == nil {
		return
	}
	if on {
		box.logFactory.SetLevel(log.LevelInfo)
	} else {
		box.logFactory.SetLevel(log.LevelPanic)
	}
}
