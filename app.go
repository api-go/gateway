package main

import "github.com/api-go/gateway/core"

// Compatibility entry points for the original command package.
type Config = gateway.Config
type GatewayApp = gateway.GatewayApp

var GatewayConf = gateway.GatewayConf

func NewGatewayApp() *GatewayApp { gateway.GatewayConf = GatewayConf; return gateway.NewGatewayApp() }
