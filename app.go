package main

import (
	"apigo.cc/go/cast"
	"apigo.cc/go/log"
	"apigo.cc/go/redis"
	"apigo.cc/go/service"
	"context"
	"strings"
)

// Config 定义 Gateway 基础配置
type Config struct {
	Redis  string // Redis 配置中心 URL (与发现服务的注册中心独立，若为空则不启用动态配置)
	Prefix string // Redis Key 的前缀
}

var GatewayConf = Config{
	Prefix: "gateway",
}

// GatewayApp 定义网关应用，融合了 WebServer 的原生能力
type GatewayApp struct {
	*service.WebServer
	rd            *redis.Redis
	pubsubChannel string
	cancelPubSub  context.CancelFunc
}

// NewGatewayApp 创建 Gateway
func NewGatewayApp() *GatewayApp {
	return &GatewayApp{
		WebServer: service.NewWebServer(),
	}
}

// Start 启动网关服务 (实现 starter.Service)
func (g *GatewayApp) Start(ctx context.Context, logger *log.Logger) error {
	if GatewayConf.Prefix == "" {
		GatewayConf.Prefix = "gateway"
	}
	g.pubsubChannel = GatewayConf.Prefix + ":channel"

	if GatewayConf.Redis != "" {
		g.rd = redis.GetRedis(GatewayConf.Redis, logger)
	}

	if g.rd != nil && g.rd.Error != nil {
		logger.Error("gateway redis connection failed", "error", g.rd.Error.Error())
		g.rd = nil
	}

	// 初始全量加载 Redis 动态配置
	g.loadAll(logger)

	// 开启 Redis 订阅
	if g.rd != nil {
		subCtx, cancel := context.WithCancel(context.Background())
		g.cancelPubSub = cancel
		go g.subscribe(subCtx, logger)
	}

	// 启动底层的 WebServer，处理所有实际的 HTTP 连接和发现注册
	return g.WebServer.Start(ctx, logger)
}

// Stop 停止网关服务 (实现 starter.Service)
func (g *GatewayApp) Stop(ctx context.Context) error {
	if g.cancelPubSub != nil {
		g.cancelPubSub()
	}
	if g.rd != nil {
		g.rd.Unsubscribe(g.pubsubChannel)
	}
	// 停止底层的 WebServer
	return g.WebServer.Stop(ctx)
}

// Reload 重载网关配置 (实现 starter.Reloader)
func (g *GatewayApp) Reload() error {
	// 1. 触发底层 WebServer 的重载 (会重新加载本地 yaml 配置文件中的固定路由策略，并触发 OnReload 钩子)
	err := g.WebServer.Reload()

	// 2. 重新全量拉取 Redis 中的动态配置
	logger := log.DefaultLogger
	logger.Info("gateway reloading dynamic configurations from redis...")
	g.loadAll(logger)

	return err
}

// loadAll 初始化或 Reload 阶段从 Redis 扫描所有网关配置并加载
func (g *GatewayApp) loadAll(logger *log.Logger) {
	if g.rd == nil {
		return
	}

	logger.Info("gateway loading full configuration")

	hosts := g.rd.Do("SMEMBERS", GatewayConf.Prefix+":hosts").Strings()
	for _, host := range hosts {
		g.loadHost(host, logger)
	}
}

// loadHost 加载指定 Host 下的所有类型配置
func (g *GatewayApp) loadHost(host string, logger *log.Logger) {
	if g.rd == nil || host == "" {
		return
	}

	hashKey := GatewayConf.Prefix + ":host:" + host
	hashMap := g.rd.Do("HGETALL", hashKey).StringMap()

	// 1. Proxies
	var proxyRules []service.ProxyRule
	if jsonStr := hashMap["proxies"]; jsonStr != "" {
		_ = cast.UnmarshalJSON([]byte(jsonStr), &proxyRules)
	}
	service.ReplaceProxies(host, proxyRules)

	// 2. Rewrites
	var rewriteRules []service.RewriteRule
	if jsonStr := hashMap["rewrites"]; jsonStr != "" {
		_ = cast.UnmarshalJSON([]byte(jsonStr), &rewriteRules)
	}
	service.ReplaceRewrites(host, rewriteRules)

	// 3. Statics
	var staticConfig map[string]string
	if jsonStr := hashMap["statics"]; jsonStr != "" {
		_ = cast.UnmarshalJSON([]byte(jsonStr), &staticConfig)
	}
	if staticConfig == nil {
		staticConfig = make(map[string]string)
	}
	service.ReplaceStatics(host, staticConfig)

	logger.Info("gateway host configuration updated via pub/sub", "host", host,
		"proxies", len(proxyRules), "rewrites", len(rewriteRules), "statics", len(staticConfig))
}

func (g *GatewayApp) subscribe(ctx context.Context, logger *log.Logger) {
	logger.Info("gateway subscribed to redis channel", "channel", g.pubsubChannel)
	g.rd.Subscribe(g.pubsubChannel, nil, func(data []byte) {
		host := string(data)
		host = strings.TrimSpace(strings.Trim(host, "\"")) // 兼容裸字符串和 JSON 字符串

		if host != "" {
			g.loadHost(host, logger)
		}
	})

	<-ctx.Done()
}
