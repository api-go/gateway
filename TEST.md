# Gateway Test Report

## 覆盖场景 (White Box & Black Box)

1. **直接代理匹配 (Discover) / `TestGateway`**: 
   - 验证 `ReplaceProxies` 将 `{"Path": "/direct", "ToApp": "test-backend", "ToPath": "/hello"}` 正确生效。
   - 验证网关能通过 `@go/discover` 成功将流量转发到注册的 `test-backend` 节点。
2. **正则代理匹配 (Discover)**:
   - 验证配置 `{"Path": "^/api/(.*)$", "ToApp": "test-backend", "ToPath": "/$1"}` 能够正确捕获正则表达式组（如 `hello?a=1`）并完整转发。
3. **Rewrite 后再 Proxy**:
   - 验证重写引擎先将 `/old-api/hello` 转化成 `/api/hello`，随后被 Proxy 引擎接管并正确路由到底层应用。
4. **静态文件服务**:
   - 验证 `ReplaceStatics` 配置能够正确暴露文件系统目录给指定的 Host 处理，响应 `200 OK` 且内容完整。
5. **基于 Redis Pub/Sub 的局部动态热更新 (0 毛刺原子替换)**:
   - 发送 `EventMessage{Action: "update", Type: "proxy", Host: "gw.test"}` 到 Redis 频道。
   - 验证新下发的 `/new-direct` 路由立刻生效并被成功访问。
   - 验证旧路由 `/direct` 被自动清理淘汰（因为全量替换了 `gw.test` 的配置数组），正确返回 `404 Not Found`。

## 性能指标 (Benchmark & Efficiency)

- **0 Write Lock Jitter**: 核心链路不再像旧版 `ssgo/gateway` 一样在匹配时被写锁拖累。配置下发生效使用纳秒级 Copy-on-Write 指针覆盖。
- **Pub/Sub Target Update**: 热更仅针对修改的单个 Host 拉取单条 HGET，消除了轮询和全量遍历的 O(N) 性能损耗。
- 全链路端到端集成测试完成于 **~0.92s**。
