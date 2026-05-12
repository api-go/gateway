# @go/gateway (高性能动态网关)

基于 `@go/service` 实现的微服务 API 网关。详细路由语法与高级配置参考：[https://apigo.cc/go/service](https://apigo.cc/go/service)

---

## 🚀 1. 快速开始 (1分钟)

```bash
# 1. 安装最新版
go get apigo.cc/go/gateway@latest

# 2. 配置 env.yml (KV 极简语法)
# ----------------------------------------------------------------------
service:
  Listen: ":80"                   # 自动识别 HTTP 协议
  Proxies:
    "api.example.com":            # 匹配域名 (支持: aa.com, :80, *, "")
      "/user/*": "user-svc/v1/*"  # [前缀通配] 必须以 /* 结尾映射后缀
      "/direct": "http://1.1.1.1/hello" # [直接转发] 转发至 URL
  Rewrites:
    "*":                          # 通配所有域名
      "^/old/(.*)$": "/new/$1"    # [正则匹配] 包含 ( 即视为正则
  Statics:
    "www.example.com":
      "/ui": "./dist"             # [静态目录] 映射本地路径

gateway:
  Redis: "127.0.0.1:6379"         # (可选) 动态配置中心
# ----------------------------------------------------------------------

# 3. 运行
go run . start
```

---

## 🔄 2. 动态路由热更新 (Redis)

网关自动发现 `gateway:host:*` 的配置，修改后通过 Pub/Sub 秒级推送。

### A. 配置存储 (HSET)
```bash
# 存储域名 api.example.com 的规则 (支持 KV 极简 JSON)
HSET gateway:host:api.example.com \
  proxies '{"/user/*":"user-svc/v1/*", "/auth":"auth-svc"}' \
  rewrites '{"^/api/(.*)$":"/v2/$1"}' \
  statics '{"/assets":"/var/www/data"}'
```

### B. 推送生效 (PUBLISH)
```bash
# 发布域名即可触发全集群该域名的局部热更新
PUBLISH gateway:channel "api.example.com"
```

---

## 🛠 3. 命令行常用操作

```bash
./gateway start      # 后台启动
./gateway status     # 查看状态 (地址、动态域名数、队列积压)
./gateway reload     # 重载本地 YAML 并全量刷入 Redis 动态配置
./gateway stop       # 优雅停止
```

---

## 📝 路由语法总结 (AI-First)

| 模式 | 语法示例 | 说明 |
| :--- | :--- | :--- |
| **精确匹配** | `"/login"` | 路径必须完全一致 |
| **高性能通配**| `"/api/*"` | **必须带 `/*` 结尾**，自动剥离前缀转发 |
| **正则匹配** | `"^/user/(.*)$"` | 路径包含 `(` 即视为正则，支持 `$1` 替换 |

*目标格式 (`To`)：`app/path` (服务发现) 或 `http://host/path` (直接代理)。*
