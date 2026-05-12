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
	starter.SetAppInfo("gateway", "2.0.0")

	// 3. 注册 Gateway 服务
	// GatewayApp 自身实现了 starter.Service 和 starter.Reloader 接口
	app := NewGatewayApp()
	starter.Register("gateway-core", app, 100, 5*time.Second, 10*time.Second)

	// 4. 运行服务生命周期，响应 start/stop/reload 等命令
	starter.Run()

	log.DefaultLogger.Info("gateway process exited")
}
