package main

import (
	"apigo.cc/go/log"
	"apigo.cc/go/redis"
	"apigo.cc/go/service"
	"context"
	"encoding/json"
	"io"
	"net"
	gohttp "net/http"
	"os"
	"testing"
	"time"
)

func TestGateway(t *testing.T) {
	if os.Getenv("GATEWAY_REDIS_INTEGRATION") != "1" {
		t.Skip("set GATEWAY_REDIS_INTEGRATION=1 to run the Redis-backed gateway integration test")
	}
	t.Setenv("SERVICE_IP", "127.0.0.1")
	// 1. 设置服务发现和 Redis 依赖
	registry := "127.0.0.1:6379"
	rd := redis.GetRedis(registry, log.DefaultLogger)
	if rd.Error != nil {
		t.Skip("Redis is not available, skipping gateway integration tests")
		return
	}

	// 清理测试数据
	rd.Do("DEL", "gateway:host:gw.test", "gateway:hosts")

	// 2. 启动一个独立的后端测试服务 test-backend (不使用全局 DefaultServer)
	backend := service.NewWebServer()
	backend.Config.App = "test-backend"
	backend.Config.Listen = ":0"
	backend.Config.Register = registry

	backend.Host("*").GET("/hello", func(req *service.Request) string {
		return "hello from backend, path: " + req.RequestURI
	})
	backend.Host("*").GET("/hello/world", func(req *service.Request) string {
		return "hello from backend, path: " + req.RequestURI
	})

	backendCtx, backendCancel := context.WithCancel(context.Background())
	defer backendCancel()
	go backend.Start(backendCtx, log.DefaultLogger)
	defer backend.Stop(backendCtx)

	time.Sleep(500 * time.Millisecond) // 等待后端启动和注册

	// 3. 配置 Gateway (使用全局 service)
	GatewayConf.Redis = registry
	GatewayConf.Prefix = "gateway"

	// 初始化网关的配置到 Redis
	proxyRules := []service.ProxyRule{
		{Path: "^/api/(.*)$", ToApp: "test-backend", ToPath: "/$1"},
		{Path: "/direct", ToApp: "test-backend", ToPath: "/hello"},
		{Path: "/v2/*", ToApp: "test-backend", ToPath: "/hello/*"},
	}
	rewriteRules := []service.RewriteRule{
		{Path: "^/old-api/(.*)$", ToPath: "/api/$1"},
	}

	proxyJson, _ := json.Marshal(proxyRules)
	rewriteJson, _ := json.Marshal(rewriteRules)

	rd.Do("HSET", "gateway:host:gw.test", "proxies", string(proxyJson))
	rd.Do("HSET", "gateway:host:gw.test", "rewrites", string(rewriteJson))

	// 创建临时静态目录
	tmpDir, _ := os.MkdirTemp("", "gateway-static")
	defer os.RemoveAll(tmpDir)
	_ = os.WriteFile(tmpDir+"/index.html", []byte("static content"), 0644)

	staticConfig := map[string]string{
		"/ui": tmpDir,
	}
	staticJson, _ := json.Marshal(staticConfig)
	rd.Do("HSET", "gateway:host:gw.test", "statics", string(staticJson))

	// 4. 启动 Gateway 应用
	app := NewGatewayApp()

	// 设置独立的端口给 Gateway 的 WebServer
	service.Config.App = "gateway"
	service.Config.Listen = ":0"
	service.Config.Register = registry
	service.Config.Calls = map[string]service.CallConfig{
		"test-backend": {Timeout: 1000},
	}

	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()

	// 这里假设 GatewayApp.Start 内部会调用 service.Start()，
	// 而 service.Start() 在新版本中如果是阻塞的，我们需要在 goroutine 中运行
	go app.Start(ctx, log.DefaultLogger)
	defer app.Stop(ctx)

	time.Sleep(500 * time.Millisecond)

	gwAddr := service.DefaultServer.Addr
	if gwAddr == "" {
		t.Fatalf("Gateway address is empty")
	}
	_, gwPort, _ := net.SplitHostPort(gwAddr)

	client := &gohttp.Client{Timeout: 5 * time.Second}

	// ============================================
	// 测试 1: 直接代理匹配 (Discover)
	// ============================================
	req, _ := gohttp.NewRequest("GET", "http://127.0.0.1:"+gwPort+"/direct", nil)
	req.Host = "gw.test"
	res, err := client.Do(req)
	var body string
	if err == nil && res != nil {
		b, _ := io.ReadAll(res.Body)
		res.Body.Close()
		body = string(b)
	}
	if err != nil || body != "hello from backend, path: /hello" {
		t.Fatalf("Proxy /direct failed, got: %s, err: %v", body, err)
	}

	// ============================================
	// 测试 2: 正则代理匹配 (Discover)
	// ============================================
	req, _ = gohttp.NewRequest("GET", "http://127.0.0.1:"+gwPort+"/api/hello?a=1", nil)
	req.Host = "gw.test"
	res, err = client.Do(req)
	body = ""
	if err == nil && res != nil {
		b, _ := io.ReadAll(res.Body)
		res.Body.Close()
		body = string(b)
	}
	if err != nil || body != "hello from backend, path: /hello?a=1" {
		t.Fatalf("Proxy /api/ failed, got: %s", body)
	}

	// ============================================
	// 测试 2.1: 极速通配符前缀匹配 (Discover)
	// ============================================
	req, _ = gohttp.NewRequest("GET", "http://127.0.0.1:"+gwPort+"/v2/world?x=2", nil)
	req.Host = "gw.test"
	res, err = client.Do(req)
	body = ""
	if err == nil && res != nil {
		b, _ := io.ReadAll(res.Body)
		res.Body.Close()
		body = string(b)
	}
	if err != nil || body != "hello from backend, path: /hello/world?x=2" {
		t.Fatalf("Proxy /v2/* failed, got: %s", body)
	}

	// ============================================
	// 测试 3: Rewrite 后再 Proxy
	// ============================================
	req, _ = gohttp.NewRequest("GET", "http://127.0.0.1:"+gwPort+"/old-api/hello", nil)
	req.Host = "gw.test"
	res, err = client.Do(req)
	body = ""
	if err == nil && res != nil {
		b, _ := io.ReadAll(res.Body)
		res.Body.Close()
		body = string(b)
	}
	if err != nil || body != "hello from backend, path: /hello" {
		t.Fatalf("Rewrite + Proxy failed, got: %s", body)
	}

	// ============================================
	// 测试 4: 静态文件服务
	// ============================================
	req, _ = gohttp.NewRequest("GET", "http://127.0.0.1:"+gwPort+"/ui/index.html", nil)
	req.Host = "gw.test"
	res, err = client.Do(req)
	body = ""
	if err == nil && res != nil {
		b, _ := io.ReadAll(res.Body)
		res.Body.Close()
		body = string(b)
	}
	if err != nil || body != "static content" {
		t.Fatalf("Static service failed, got: %s", body)
	}

	// ============================================
	// 测试 5: 通过 Redis Pub/Sub 动态更新路由
	// ============================================
	// 新增一个新代理规则，删除老的
	newProxyRules := []service.ProxyRule{
		{Path: "/new-direct", ToApp: "test-backend", ToPath: "/hello"},
	}
	newProxyJson, _ := json.Marshal(newProxyRules)
	rd.Do("HSET", "gateway:host:gw.test", "proxies", string(newProxyJson))

	// 发布更新消息
	rd.Do("PUBLISH", "gateway:channel", `"gw.test"`)

	// 等待接收和更新
	time.Sleep(300 * time.Millisecond)

	// 测试新路由应该生效
	req, _ = gohttp.NewRequest("GET", "http://127.0.0.1:"+gwPort+"/new-direct", nil)
	req.Host = "gw.test"
	res, err = client.Do(req)
	body = ""
	if err == nil && res != nil {
		b, _ := io.ReadAll(res.Body)
		res.Body.Close()
		body = string(b)
	}
	if err != nil || body != "hello from backend, path: /hello" {
		t.Fatalf("Pub/Sub dynamic proxy failed, got: %s", body)
	}

	// 测试老路由应该 404 (原子替换)
	req, _ = gohttp.NewRequest("GET", "http://127.0.0.1:"+gwPort+"/direct", nil)
	req.Host = "gw.test"
	res, err = client.Do(req)
	if err != nil {
		t.Fatalf("Request failed: %v", err)
	}
	if res.StatusCode != 404 {
		t.Fatalf("Atomic update failed, old route still exists")
	}
}

var benchmarkGatewayApp *GatewayApp

func BenchmarkNewGatewayApp(b *testing.B) {
	b.ReportAllocs()
	for b.Loop() {
		benchmarkGatewayApp = NewGatewayApp()
	}
}
