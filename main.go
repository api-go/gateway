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
	starter.SetAppInfo("gateway", "1.0.0")
	starter.SetUsage(`High-performance, event-driven dynamic API gateway.
Full Documentation: https://apigo.cc/go/service

Routing Path Matching Rules:
  - Exact Match (Default): Path is treated as a literal string. 
    Example: "Path": "/api" only matches EXACTLY "/api".
  - Prefix Match (Mandatory "/*" suffix): Matches any path starting with the given prefix.
    Example: "Path": "/api/*" matches "/api/v1", "/api/login", etc.
    Note: The trailing "/*" is required to enable prefix matching logic.
  - Regex Match: Path containing "(" is treated as a Regular Expression.
    Example: "Path": "^/api/(.*)$" captures groups for replacement in ToPath.

Host Selection Mechanism:
  Requests are matched against the defined "Host" field in following order:
  1. Exact Host:Port (e.g., "aa.com:8080")
  2. Host Only (e.g., "aa.com") - matches any port for this host.
  3. Port Only (e.g., ":80") - matches any host on this port.
  4. Global Wildcard ("*" or "") - fallback for all other requests.`)

	// 3. 注册 Gateway 服务
	// GatewayApp 自身实现了 starter.Service 和 starter.Reloader 接口
	app := NewGatewayApp()
	starter.Register("gateway-core", app, 100, 5*time.Second, 10*time.Second)

	// 4. 运行服务生命周期，响应 start/stop/reload 等命令
	starter.Run()

	log.DefaultLogger.Info("gateway process exited")
}
