package main

import (
	"apigo.cc/go/cast"
	"apigo.cc/go/log"
	"apigo.cc/go/redis"
	"apigo.cc/go/service"
	"context"
)

// EventMessage 定义从 Redis 接收到的更新事件结构
type EventMessage struct {
	Action string `json:"action"` // "update"
	Type   string `json:"type"`   // "proxy", "rewrite", "static"
	Host   string `json:"host"`
}

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
	proxiesKey     string
	rewritesKey    string
	staticsKey     string
	pubsubChannel  string
	cancelPubSub   context.CancelFunc
}

// NewGatewayApp 创建 Gateway
func NewGatewayApp() *GatewayApp {
	g := &GatewayApp{}
	g.proxiesKey = GatewayConf.Prefix + ":proxies"
	g.rewritesKey = GatewayConf.Prefix + ":rewrites"
	g.staticsKey = GatewayConf.Prefix + ":statics"
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

// loadAll 初始化阶段从 Redis 的 Hash 表拉取全量配置并解析
func (g *GatewayApp) loadAll() {
	if g.rd == nil {
		return
	}

	log.DefaultLogger.Info("gateway loading full configuration")

	// 1. Proxies
	proxiesHash := g.rd.Do("HGETALL", g.proxiesKey).StringMap()
	for host, jsonStr := range proxiesHash {
		var rules []service.ProxyRule
		_ = cast.UnmarshalJSON([]byte(jsonStr), &rules)
		service.ReplaceProxies(host, rules)
	}

	// 2. Rewrites
	rewritesHash := g.rd.Do("HGETALL", g.rewritesKey).StringMap()
	for host, jsonStr := range rewritesHash {
		var rules []service.RewriteRule
		_ = cast.UnmarshalJSON([]byte(jsonStr), &rules)
		service.ReplaceRewrites(host, rules)
	}

	// 3. Statics
	staticsHash := g.rd.Do("HGETALL", g.staticsKey).StringMap()
	for host, jsonStr := range staticsHash {
		var config map[string]string
		_ = cast.UnmarshalJSON([]byte(jsonStr), &config)
		if config != nil {
			service.ReplaceStatics(host, config)
		}
	}
}

// loadHost 仅加载指定 Host 下的指定类型配置
func (g *GatewayApp) loadHost(typ string, host string) {
	if g.rd == nil {
		return
	}

	switch typ {
	case "proxy":
		jsonStr := g.rd.Do("HGET", g.proxiesKey, host).String()
		var rules []service.ProxyRule
		if jsonStr != "" {
			_ = cast.UnmarshalJSON([]byte(jsonStr), &rules)
		}
		service.ReplaceProxies(host, rules)
		log.DefaultLogger.Info("gateway proxy updated via pub/sub", "host", host, "count", len(rules))
	case "rewrite":
		jsonStr := g.rd.Do("HGET", g.rewritesKey, host).String()
		var rules []service.RewriteRule
		if jsonStr != "" {
			_ = cast.UnmarshalJSON([]byte(jsonStr), &rules)
		}
		service.ReplaceRewrites(host, rules)
		log.DefaultLogger.Info("gateway rewrite updated via pub/sub", "host", host, "count", len(rules))
	case "static":
		jsonStr := g.rd.Do("HGET", g.staticsKey, host).String()
		var config map[string]string
		if jsonStr != "" {
			_ = cast.UnmarshalJSON([]byte(jsonStr), &config)
		}
		if config == nil {
			config = make(map[string]string)
		}
		service.ReplaceStatics(host, config)
		log.DefaultLogger.Info("gateway static updated via pub/sub", "host", host, "count", len(config))
	}
}

func (g *GatewayApp) subscribe(ctx context.Context) {
	log.DefaultLogger.Info("gateway subscribed to redis channel", "channel", g.pubsubChannel)
	g.rd.Subscribe(g.pubsubChannel, nil, func(data []byte) {
		var msg EventMessage
		if err := cast.UnmarshalJSON(data, &msg); err == nil && msg.Action == "update" {
			// 触发对应 Host 的局部刷新
			g.loadHost(msg.Type, msg.Host)
		} else {
			log.DefaultLogger.Warning("gateway received invalid pub/sub message", "data", string(data))
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
