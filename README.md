# @go/gateway

基于 `@go/service` 和 Redis 的高性能、事件驱动的动态 API 网关。

`gateway` 专注于充当配置下发的搬运工，将路由匹配、转发等所有繁重工作剥离给底层的 `service` 核心。支持通过 Redis 进行 0 延迟、0 丢包的路由级全量/局部热更新。

## 功能特性

- **极致性能**：基于 `service` 包的 Copy-on-Write (写时复制) 原语，无锁构建，转发阶段无锁等待。
- **事件驱动热更新**：摒弃轮询，监听 Redis Pub/Sub，精准下发变更配置，旧连接不受影响。
- **服务发现原生集成**：与 `@go/discover` 无缝集成，支持基于服务名的直连与负载均衡。
- **生命周期接管**：由 `@go/starter` 接管，支持通过 `kill -SIGHUP` 触发本地全量重新同步。

## 配置模型 (Redis Hash)

Gateway 将配置存储在 Redis 的 Hash 结构中，并按照 **Host (域名)** 进行字段隔离，提升拉取效率和管理粒度。

- **`gateway:proxies`** 代理规则
- **`gateway:rewrites`** 重写规则
- **`gateway:statics`** 静态目录服务

### 结构示例

假设配置的 Host 为 `api.example.com`:

```json
// HSET gateway:proxies "api.example.com"
[
  { "Path": "^/user/(.*)$", "AuthLevel": 0, "ToApp": "user-service", "ToPath": "/$1" },
  { "Path": "/direct", "AuthLevel": 0, "ToApp": "http://10.0.0.1:8080", "ToPath": "/hello" }
]

// HSET gateway:rewrites "api.example.com"
[
  { "Path": "^/old-api/(.*)$", "ToPath": "/api/$1" }
]

// HSET gateway:statics "www.example.com"
{
  "/ui": "/var/www/html"
}
```

## 动态热更新 API (Pub/Sub)

无需重启进程，向 Redis 的 `gateway:channel` 频道发布 JSON 消息，网关会在收到消息后仅拉取变化的那一部分（局部全量更新）：

```json
{
  "action": "update",
  "type": "proxy", // 可选: "proxy", "rewrite", "static"
  "host": "api.example.com"
}
```

## 启动与管理

```bash
# 以后台进程启动
./gateway start

# 查看服务状态 (通过 IPC)
./gateway status

# 平滑重启 / 重载兜底
./gateway restart
./gateway reload # 触发底层 service 的 OnReload，全量同步 Redis
```

## 环境变量配置

支持通过环境变量或 `env.yml` 覆盖。

```yaml
gateway:
  Redis: "127.0.0.1:6379"
  Prefix: "gateway"
  Channel: "gateway:channel"
```
