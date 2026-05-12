package main

import (
	"encoding/json"
	"apigo.cc/go/log"
	"apigo.cc/go/redis"
	"apigo.cc/go/service"
	"io"
	"net"
	gohttp "net/http"
	"os"
	"strings"
	"testing"
	"time"
)

func TestGateway(t *testing.T) {
	// 1. 设置服务发现和 Redis 依赖
	registry := "127.0.0.1:6379"
	rd := redis.GetRedis(registry, log.DefaultLogger)
	if rd.Error != nil {
		t.Skip("Redis is not available, skipping gateway integration tests")
		return
	}

	// 清理测试数据
	rd.Do("DEL", "gateway:host:gw.test", "gateway:hosts")

	// 2. 启动一个后端测试服务 test-backend
	service.Config.App = "test-backend"
	service.Config.Listen = ":0"
	service.Config.Register = registry

	service.Host("*").GET("/hello", func(req *service.Request) string {
		return "hello from backend, path: " + req.RequestURI
	})

	asBackend := service.AsyncStart()
	defer asBackend.Stop()
	time.Sleep(200 * time.Millisecond) // 等待后端启动和注册

	// 3. 配置 Gateway
	GatewayConf.Redis = registry
	GatewayConf.Prefix = "gateway"
	GatewayConf.Channel = "gateway:channel"

	// 初始化网关的配置到 Redis
	proxyRules := []service.ProxyRule{
		{Path: "^/api/(.*)$", ToApp: "test-backend", ToPath: "/$1"},
		{Path: "/direct", ToApp: "test-backend", ToPath: "/hello"},
	}
	rewriteRules := []service.RewriteRule{
		{Path: "^/old-api/(.*)$", ToPath: "/api/$1"},
	}
	
	proxyJson, _ := json.Marshal(proxyRules)
	rewriteJson, _ := json.Marshal(rewriteRules)
	
	rd.Do("SADD", "gateway:hosts", "gw.test")
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
	err := app.Init()
	if err != nil {
		t.Fatalf("Failed to init gateway: %v", err)
	}
	defer app.Stop()

	// 启动网关的 HTTP 监听
	service.Config.App = "gateway"
	service.Config.Listen = ":0"
	// 重置发现，确保网关独立
	service.GlobalDiscoverer = nil 
	// 配置网关可以通过 discover 找到 test-backend (网关也需要开启 discover)
	service.Config.Register = registry
	service.Config.Calls = map[string]service.CallConfig{
		"test-backend": {Timeout: 1000},
	}
	asGw := service.AsyncStart()
	defer asGw.Stop()
	time.Sleep(200 * time.Millisecond)
	
	_, gwPort, _ := net.SplitHostPort(asGw.Addr)

	client := &gohttp.Client{Timeout: 2 * time.Second}

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
	if err != nil || !strings.Contains(body, "hello from backend, path: /hello?a=1") {
		t.Fatalf("Proxy regexp failed, got: %s", body)
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
		t.Fatalf("Rewrite+Proxy failed, got: %s", body)
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
		t.Fatalf("Old proxy rule should be deleted (404), got status: %v", res.StatusCode)
	}
	res.Body.Close()
}
