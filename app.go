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
	Redis   string // Redis 注册中心/配置中心 URL
	Prefix  string // Redis Key 的前缀
	Channel string // Redis Pub/Sub 通道名称
}

var GatewayConf = Config{
	Prefix:  "gateway",
	Channel: "gateway:channel",
}

// GatewayApp 定义网关应用
type GatewayApp struct {
	rd             *redis.Redis
	pubsubChannel  string
	cancelPubSub   context.CancelFunc
}

// NewGatewayApp 创建 Gateway
func NewGatewayApp() *GatewayApp {
	g := &GatewayApp{}
	g.pubsubChannel = GatewayConf.Channel
	return g
}

// Init 初始化 Gateway，从 Redis 加载并订阅更新
func (g *GatewayApp) Init() error {
	if GatewayConf.Redis != "" {
		g.rd = redis.GetRedis(GatewayConf.Redis, log.DefaultLogger)
	}

	if g.rd != nil && g.rd.Error != nil {
		log.DefaultLogger.Error("gateway redis connection failed", "error", g.rd.Error.Error())
		g.rd = nil
	}

	// 初始全量加载
	g.loadAll()

	// 注册 Reload 钩子 (响应 SIGHUP 或 kill 命令)，兜底机制全量加载
	service.OnReload(func() error {
		g.loadAll()
		return nil
	})

	// 开启 Redis 订阅，处理针对特定 Host 的局部更新
	if g.rd != nil {
		ctx, cancel := context.WithCancel(context.Background())
		g.cancelPubSub = cancel
		go g.subscribe(ctx)
	}

	return nil
}

// loadAll 初始化阶段从 Redis 扫描所有网关配置并加载
func (g *GatewayApp) loadAll() {
	if g.rd == nil {
		return
	}

	log.DefaultLogger.Info("gateway loading full configuration")

	// 为了简化，由于 Redis KEYS 命令在线上不推荐，更好的方式是 Gateway 启动时只加载本地固定配置，
	// 动态配置按需拉取？不，网关启动必须拉取全量。
	// 但如果我们改成以 host 为 Key，就需要维护一个 Set 存放所有的 host，或者直接遍历 KEYS gateway:*
	// 为了最佳实践，我们在 redis 维护一个 set: `gateway:hosts` 存放所有的域名
	hosts := g.rd.Do("SMEMBERS", GatewayConf.Prefix+":hosts").Strings()
	for _, host := range hosts {
		g.loadHost(host)
	}
}

// loadHost 加载指定 Host 下的所有类型配置
func (g *GatewayApp) loadHost(host string) {
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

	log.DefaultLogger.Info("gateway host configuration updated via pub/sub", "host", host, 
		"proxies", len(proxyRules), "rewrites", len(rewriteRules), "statics", len(staticConfig))
}

func (g *GatewayApp) subscribe(ctx context.Context) {
	log.DefaultLogger.Info("gateway subscribed to redis channel", "channel", g.pubsubChannel)
	g.rd.Subscribe(g.pubsubChannel, nil, func(data []byte) {
		host := string(data)
		host = strings.TrimSpace(strings.Trim(host, "\"")) // 兼容裸字符串和 JSON 字符串
		
		if host != "" {
			// 触发对应 Host 的局部刷新
			g.loadHost(host)
		}
	})
	
	<-ctx.Done()
}

// Stop 停止应用
func (g *GatewayApp) Stop() {
	if g.cancelPubSub != nil {
		g.cancelPubSub()
	}
	if g.rd != nil {
		g.rd.Unsubscribe(g.pubsubChannel)
	}
}
