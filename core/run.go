package gateway

import (
	"github.com/api-go/config"
	"github.com/api-go/log"
	"github.com/api-go/service"
	"github.com/api-go/starter"
	"fmt"
	"os"
	"time"
)

func Run() {
	// 1. 加载默认配置，支持覆盖与环境变量注入
	if err := config.Load(&GatewayConf, "gateway"); err != nil {
		log.DefaultLogger.Error("gateway configuration failed", "error", err.Error())
		return
	}

	if err := config.Load(&service.DefaultServer.Config, "service"); err != nil {
		log.DefaultLogger.Error("service configuration failed", "error", err.Error())
		return
	}
	if GatewayConf.Redis == "" {
		GatewayConf.Redis = service.DefaultServer.Config.Register
	}
	if err := resolveAdminConfig(&GatewayConf); err != nil {
		fmt.Fprintln(os.Stderr, err)
		os.Exit(1)
	}
	command := ""
	if len(os.Args) > 1 {
		command = os.Args[1]
	}
	if command == "" || command == "start" || command == "restart" || command == "admin-password" {
		if !hasGatewayConfig() {
			if err := WriteDefaultAdminConfig("gateway.yaml", GatewayConf); err != nil {
				fmt.Fprintln(os.Stderr, err)
				os.Exit(1)
			}
		}
	}
	starter.AddCommand("admin-password", "Set the sole administrator password (hidden terminal input or stdin)", func() {
		if err := adminPasswordCommand(GatewayConf); err != nil {
			fmt.Fprintln(os.Stderr, err)
			os.Exit(1)
		}
	})
	// 2. 初始化 Starter 信息
	starter.SetAppInfo("gateway", "1.5.25")
	starter.SetUsage(`High-performance API gateway based on github.com/api-go/service.
Full Guide: https://github.com/api-go/service

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
