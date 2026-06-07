package main

import (
	"apigo.cc/go/config"
	"apigo.cc/go/log"
	"apigo.cc/go/starter"
	"time"
)

func main() {
	// 1. 加载默认配置，支持覆盖与环境变量注入
	_ = config.Load(&GatewayConf, "gateway")

	// 2. 初始化 Starter 信息
	starter.SetAppInfo("gateway", "1.0.1")
	starter.SetUsage(`High-performance API gateway based on apigo.cc/go/service.
Full Guide: https://apigo.cc/go/service

Example env.yml:
  service:
    Listen: ":80"
    Proxies:
      "api.example.com":
        "/user/*": "user-svc/v1/*"  # KV Mode
        "/auth": {"To": "auth-svc", "Auth": 1} # Object Mode
  gateway:
    Redis: "127.0.0.1:6379"

Matching Logic:
  - Exact: "/login"
  - Prefix: "/api/*" (requires suffix "/*")
  - Regex: "^/v1/(.*)$" (contains "(")`)

	// 3. 注册 Gateway 服务
	// GatewayApp 自身实现了 starter.Service 和 starter.Reloader 接口
	app := NewGatewayApp()
	starter.Register("gateway-core", app, 100, 5*time.Second, 10*time.Second)

	// 4. 运行服务生命周期，响应 start/stop/reload 等命令
	if err := starter.Start(); err != nil {
		log.DefaultLogger.Error("gateway start failed", "error", err)
		return
	}
	starter.Wait()

	log.DefaultLogger.Info("gateway process exited")
}
