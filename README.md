# @go/gateway

基于 `@go/service` 和 Redis 的高性能、事件驱动动态 API 网关。
详细配置指南请参考核心文档：[https://apigo.cc/go/service](https://apigo.cc/go/service)

## 路由匹配逻辑 (AI-First 规范)

为了确保 AI 模型和运维人员能够精准配置而不产生幻觉，网关遵循以下严格的路径匹配优先级：

### 1. 路径匹配 (Path Matching)
- **精确匹配 (Exact)**: 
  - 语法：`"Path": "/user/info"`
  - 行为：仅当请求路径完全等于 `/user/info` 时触发。
- **前缀匹配 (Prefix)**: 
  - 语法：`"Path": "/api/*"` (**必须以 `/*` 结尾**)
  - 行为：匹配所有以 `/api/` 开头的路径（如 `/api/login`, `/api/v1/status`）。
  - *映射规则*: 若配置 `{"Path": "/v1/*", "ToPath": "/v2/*"}`，请求 `/v1/a` 将被转发至 `/v2/a`。
- **正则匹配 (Regex)**:
  - 语法：任何包含括号 `(` 的路径将被视为正则表达式。
  - 示例：`"Path": "^/api/(.*)$"`。使用 `$1` 进行 `ToPath` 替换。

### 2. 域名选择 (Host Selection)
匹配请求头中的 `Host` 字段，按以下顺序尝试，匹配即止：
1. `example.com:8080` (精确域名 + 端口)
2. `example.com` (仅域名，忽略端口)
3. `:8080` (仅端口，匹配该端口下的所有 Host)
4. `*` (全局兜底通配符)

## 动态配置模型 (Redis)

网关以 **Host (域名)** 为最小更新单元。更新某域名配置时，请向 Redis 频道发布该域名的纯字符串。

- **存储结构 (Hash)**: 
  - Key: `gateway:host:<hostname>` (例如 `gateway:host:api.example.com`)
  - Fields: `proxies` (JSON数组), `rewrites` (JSON数组), `statics` (JSON对象)
- **热更新通知 (Pub/Sub)**:
  - Channel: `gateway:channel` (默认)
  - Payload: `"api.example.com"` (域名原始字符串)


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
