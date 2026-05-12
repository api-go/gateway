package main

import (
	"apigo.cc/go/config"
	"apigo.cc/go/log"
	"apigo.cc/go/service"
	"apigo.cc/go/starter"
	"fmt"
	"os"
	"time"
)

func main() {
	// 加载默认配置
	_ = config.Load(&GatewayConf, "gateway")

	app := NewGatewayApp()
	if err := app.Init(); err != nil {
		fmt.Printf("Gateway init error: %v\n", err)
		os.Exit(1)
	}

	starter.SetAppInfo("gateway", "2.0.0")

	// 注册 Gateway 服务核心: service.WebServer
	webServer := service.NewWebServer()
	starter.Register("gateway-web", webServer, 100, 5*time.Second, 10*time.Second)

	// 运行
	starter.Run()
	
	app.Stop()
	log.DefaultLogger.Info("gateway exited")
}
