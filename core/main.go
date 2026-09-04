package core

import (
	"context"
	"sync"

	"github.com/Maoyangui/m-ui/logger"

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
	return c.instance
}

func (c *Core) Start(sbConfig []byte) error {
	var opt option.Options
	err := opt.UnmarshalJSONContext(globalCtx, sbConfig)
	if err != nil {
		logger.Error("Unmarshal config err:", err.Error())
	}

	c.instance, err = NewBox(Options{
		Context: globalCtx,
		Options: opt,
	})
	if err != nil {
		return err
	}

	err = c.instance.Start()
	if err != nil {
		_ = c.instance.Close()
		c.instance = nil
		return err
	}

	globalCtx = service.ContextWith(globalCtx, c)
	inbound_manager = service.FromContext[adapter.InboundManager](globalCtx)
	outbound_manager = service.FromContext[adapter.OutboundManager](globalCtx)
	service_manager = service.FromContext[adapter.ServiceManager](globalCtx)
	endpoint_manager = service.FromContext[adapter.EndpointManager](globalCtx)
	router = service.FromContext[adapter.Router](globalCtx)

	c.isRunning = true
	return nil
}

func (c *Core) Stop() error {
	c.isRunning = false
	if c.instance == nil {
		return nil
	}
	err := c.instance.Close()
	c.instance = nil
	return err
}

func (c *Core) IsRunning() bool {
	return c.isRunning
}
