# Changelog

## [v2.0.0] - 2026-05-12

### ✨ 全新重构架构 (Architecture Rewrite)
- **解耦核心能力**: 彻底废弃旧版内部闭门造车的低效路由引擎，完全依托于 `@go/service` 提供的极速规则引擎 (Proxy/Rewrite)。
- **Copy-on-Write 无锁生效**: 基于新版 `service` 的原子替换能力，实现动态配置 100% 零锁耗变更，消除请求抖动 (Jitter)。
- **精准局部热更**: 引入了按照 `Host` 域名进行空间隔离的 Redis Hash 配置结构。结合 Pub/Sub，实现“指哪打哪”的配置精准下发。
- **降维打击重构**:
  - 用 `apigo.cc/go/redis` 替换臃肿的 `redigo`。
  - 用 `apigo.cc/go/service` 和 `apigo.cc/go/starter` 接管所有的生命周期 (SIGHUP 监听、PID 文件处理)。
  - 彻底废除了耗费性能且脆弱的 `time.Sleep` 轮询架构，全切为事件驱动 + 启动兜底。
  - `apigo.cc/go/discover` 进行更轻薄的原生对接。
- **现代化基建**: JSON 解析全部由原生的 `encoding/json` 与 `go/cast` 组合支撑，移除旧版冗余的反射逻辑。
