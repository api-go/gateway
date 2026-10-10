# Gateway

基于 `apigo.cc/go/service` 的动态网关。**v1.5.11** 提供管理后台、独立限流与集中证书管理，依赖 Service v1.5.42 和 Discover v1.5.15。此处以实际实现为准。

## 启动

```sh
go build -o gateway .
./gateway admin-password   # 交互输入两次，终端不回显
./gateway start
./gateway status
./gateway reload
./gateway stop
```

`env.yml` 示例：

```yaml
service:
  Listen: "127.0.0.1:8080,http|127.0.0.1:8443,https"
gateway:
  AdminListen: "192.168.1.10:81" # 改为本机实际局域网地址
  AdminDisabled: false
  DataDir: "data/gateway"
  ClientIP:
    Mode: direct
  ACMEEmail: "ops@example.com"
  ACMEAcceptTerms: true
```

管理后台默认开启，默认端口 **81**。未指定 AdminListen 时优先选启用网卡上的私有 IPv4，跳过已知隧道/虚拟网卡；其次选择私有 IPv6，无合适地址时回退 `127.0.0.1:81`。地址属于私有网段并不证明它是用户期望的局域网，尤其在多网卡和容器中；应检查启动状态中的实际地址，并在配置中指定需要的地址。

首次运行 `start` 或 `admin-password` 会在当前目录生成最小 `gateway.yaml`，固定选出的监听地址。已有 gateway.yaml/yml/json 不覆盖；不在配置中保存密码。该文件是 Gateway 的独立配置，字段直接位于顶层；env.yml 的 Gateway 设置则放在 gateway 节点下。运行 `status` 查看实际管理地址。

只有一个管理员，登录页只输入密码。通过 `./gateway admin-password` 设置或修改密码，12–72 字节；也支持从标准输入读取一行，密码不会放入命令参数。命令可在服务运行或停止时执行，修改立即生效，旧会话在下次 API 请求时被撤销。未设置密码时页面可访问，登录返回 423、管理 API 返回 401，没有网页初始化接口或默认密码。密码只保存 bcrypt 哈希；单机文件为 DataDir/admin.password，集群存储为 Prefix:admin-password，各节点共用。

`AdminDisabled: true` 关闭后台监听；AdminListen 可覆盖地址或端口，包括 `127.0.0.1:81`。修改管理监听或开关后重启服务。独立管理监听使用 HTTP，可按需要经具有 HTTPS 的受控入口访问。低端口绑定要求取决于系统权限，无法绑定会报错，不会静默换端口。旧工作区的多账号数据不再用于登录，需要运行上述命令设置单一密码。

发布版使用已发布的 Service v1.5.42 和 Discover v1.5.15，不含本地 replace；可在不保留相邻模块 checkout 的情况下独立编译。根目录 CLI 保持兼容，可复用的 Go API 位于 `apigo.cc/go/gateway/core`。

## 后台与发布

导航为 **Hosts、证书、事件、设置**。Host 列表显示域名、证书、规则、限流、草稿/版本状态；固定高度编辑窗口内部滚动，标签页为配置、代理、重写、重定向、静态文件、限流、发布记录：

- 一个 Host 可以绑定多个域名，默认自动匹配证书；每个域名可使用不同证书。无有效证书时使用 HTTP，证书可用后自动支持 HTTPS，并默认将 HTTP 跳转到 HTTPS；可关闭跳转或选择仅 HTTP。
- 证书库独立管理上传、替换、删除、自动签发；不需要在 Host 上手动选证书。证书变更立即作用于已发布的自动匹配 Host，无需重新发布。
- 配置代理、重写、重定向、静态目录；代理可配置 Session 鉴权等级，已引用的发现服务提供选择提示。
- 保存草稿不会影响流量；发布前显示变化，校验整份配置后切换。回滚将历史配置作为一个新版本发布。
- 规则预览显示匹配和路由结果，不转发真实请求、不消耗限流次数。配置导入只创建/更新草稿；导出不包含私钥。
- 设置展示运行参数、节点状态、管理员密码设置说明；自动签发邮箱、服务地址、条款确认在证书页保存并即时生效；Gateway 的全局监听、Redis、可信代理等通过 YAML 配置及重启生效；reload 重载 Service 自身配置并重新校对管理/旧 Redis 路由。

管理 API 使用登录会话、HttpOnly/SameSite Cookie、CSRF、来源校验及登录限速；不提供账号/角色管理 API。每个 Host 的并发修改带 revision 校验，过期编辑返回冲突。后台与业务端口分离。

新管理配置以 Host 为单位完整替换，不与该 Host 的旧路由混合。未纳管域名仍使用原 Service/YAML/Redis 路由；删除管理 Host 后也恢复该域名的原有处理。一次请求固定使用进入时的配置快照。

当前本地改动为 **v1.5.12（未发布）**：以下默认值和填写指南已在后台实现；已发布版本仍为 v1.5.11。

## 路由填写指南

后台静态文件新增规则默认 `/` → `www/`。`www`、`www/`、`./www`、`./www/` 都支持且等价，相对于 **Gateway 进程工作目录**，不相对于配置文件；也支持绝对目录。URL 使用正斜杠 `/`，无需反斜杠。静态挂载按路径前缀匹配；子目录挂载建议 `/assets/` → `www/`，请求 `/assets/logo.png` 对应 `www/logo.png`；静态挂载不使用通配符或正则。

代理、重写、重定向新增规则默认 `/*`，不启用正则；`/` 仅精确匹配首页。为保持现有 Service 路由兼容，不把 `/` 改成整站匹配，也不另加匹配模式开关；后台每个标签页提供说明和示例。

| 类型 | 匹配路径 | 目标 | 效果 |
| --- | --- | --- | --- |
| 整站代理 | `/*` | `https://upstream.example.com/` | `/news` 转发到上游 `/news` |
| 子目录去前缀代理 | `/api/*` | `http://127.0.0.1:8080/` | `/api/users` 转发到上游 `/users` |
| 子目录保留前缀代理 | `/api/*` | `user-svc/api/` | `/api/users` 转发到发现服务 `/api/users` |
| 精确代理 | `/health` | `user-svc/status` | 仅 `/health` 转发到 `/status` |
| 内部重写 | `/old/*` | `/new/` | `/old/a` 内部改为 `/new/a`，地址栏不变 |
| 首页重写 | `/` | `/index.html` | 只重写首页 |
| 整站迁移 | `/*` | `https://new.example.com/` | HTTP 302 跳转，保留路径 |
| 子目录重定向 | `/old/*` | `https://new.example.com/` | `/old/a` 跳转到新站 `/a` |

末尾 `/*` 为前缀匹配，将后缀追加到目标；目标目录建议以 `/` 或 `/*` 结尾。`/api/*` 匹配 `/api/` 及其下路径，不匹配 `/api`；需要时另加 `/api` 精确规则。普通规则保留查询参数；重写/重定向目标自带 `?` 时替换原查询参数。子目录代理只处理请求路径，不会自动改写上游 HTML 中的绝对资源地址、Cookie Path 或 Location，上游应支持对应的公开访问前缀。

**正则不是由 `*` 触发**：匹配路径包含 `(` 时才按 Go regexp（RE2）编译，两端自动添加 `^`、`$`；不支持前后查找或反向引用。目标用 `$1` 等引用捕获组，`$0` 为完整匹配；建议最多 9 个捕获组。例如代理 `/user/([0-9]+)` → `user-svc/profile/$1`。代理正则只匹配路径，查询参数自动保留；重写/重定向正则匹配完整 URI，含查询参数。兼容查询参数的重写示例：`/user/([0-9]+)(\?.*)?` → `/profile/$1$2`。

重写与重定向共用一个阶段，按保存顺序命中第一条后结束该阶段；内部重写后继续代理、静态处理，外部跳转返回 302。两者由目标是否包含 `://` 区分，本地路径为内部重写，完整 HTTP(S) URL 为重定向。代理同样首条匹配优先，应把具体规则放在 `/*` 之前；静态挂载按更长路径优先。重写和重定向虽分两个标签页，仍共用保存顺序；编辑已有匹配路径不会改变顺序，新增规则追加在后，若已有整站规则需重新安排后保存。整站代理会先于静态处理，可能覆盖该 Host 的静态挂载。

## 限流（独立功能）

默认只需填写适用条件、周期和允许次数，计数方式为 **所有匹配请求合计**（GroupBy 为空）。条件全部满足才计数。需要每个 IP / Session 分别拥有额度时，选择对应计数方式；Header、Cookie、多字段身份组合在高级设置中配置。多个 GroupBy 维度组成一个联合键。支持 Host、Path、IP、UA、HTTP Header、Session、Cookie，条件还支持 Method，以及精确、前缀、包含、正则、CIDR、存在判断。

例如：Host 下 `/login`，按 IP 分组，每 60 秒允许 20 次；或 `/api/*` 按 `IP + Cookie(account)` 分组，每分钟 100 次。这与分别按 IP、Cookie 各限 100 次不同，后者需要两条规则。

- 使用从该键首次请求起算的固定周期，不是滑动窗口或令牌桶；多个适用规则均会计数。
- enforce 超额返回 429 与 Retry-After，任意一条适用的 enforce 规则超额都会拒绝；observe 只记录事件。不同规则没有跨规则共用额度。“匿名请求合计”仅指同一条规则内缺少某个身份字段的请求使用该缺失值；其他身份维度仍参与组合。选“按请求 IP 分别计数”时，只把缺少的身份字段替换成请求 IP；也可以拒绝缺少身份的请求。
- Session 默认读取客户端传入的 `Session-ID` Header/Cookie，不使用本次新生成的 Session 值。
- node 是各节点各自额度；cluster 使用 Redis 原子计数与过期时间。集群 Redis 失败返回 503，不悄悄降级为节点额度。
- 单机计数键上限默认 100000，耗尽返回 503；单维度输入最多 4096 字节，周期 1–86400 秒。高基数 Header/UA/Cookie 应谨慎选择。

此轮不含攻击识别、请求体检查、WAF 或 JS 策略。限流在读取请求体之前执行，正常代理仍保留流式传输。

## 真实 IP 与信任边界

Gateway 默认 `direct`：以 TCP 对端为客户端地址，不信任客户端提供的转发头或请求 ID，并清理外部内部身份/发现标记。若前面有负载均衡器，配置明确的可信网段：

```yaml
gateway:
  ClientIP:
    Mode: trusted
    TrustedProxies:
      - "10.20.0.0/24"
    Header: "X-Forwarded-For"
```

只有 TCP 对端落入可信网段时才读取头；X-Forwarded-For 从右往左跨越可信代理，遇到首个不可信或非法跳点停止。边缘代理必须正确追加/重建该头。也可指定 X-Real-IP/X-Client-IP；只有单地址头时，其真实性完全取决于可信对端。

Service 的默认 legacy 模式保留内部兼容逻辑；其中请求 ID 是约定标记，并非代理身份认证。公网入口应使用 direct/trusted。Gateway 转发时重建客户端 IP 头，保留普通业务 Header 并去除 hop-by-hop Header。

## HTTPS 证书

证书库验证 PEM 证书/私钥匹配并从 SAN 提取域名。私钥使用 AES-GCM 加密落盘，管理 API 不返回私钥、密文或 PEM；证书信息展示来源、覆盖域名、状态和有效期。可先上传证书，也可先发布 Host；两者自动衔接。

新 Host 默认 HTTPS.Mode=adaptive，HTTPS.Redirect=auto（off 关闭跳转）。按 SNI / 请求域名精确匹配有效证书；精确匹配失败才尝试单层通配符，例如 *.example.com 覆盖 api.example.com，不覆盖 example.com 或 a.api.example.com。相同匹配级别先选到期更晚的证书，时间相同按 ID 确定顺序。未生效或过期证书不用于 TLS / 跳转。HTTP 回退只保证后续 HTTP 请求可以服务，不能把失败的 HTTPS 握手变成 HTTP，也不会撤销浏览器已缓存的 HSTS。

HTTP 跳转需要配置 TLS 监听；默认从 service.Listen 中的 https 项推导端口，本地 10443 会保留在跳转地址。公网端口映射与本地不同可显式配置 Gateway HTTPSPort（例如 443）。监听配置仍需 YAML；后台不会自动绑定或修改业务端口。

自动证书内置官方 `golang.org/x/crypto/acme/autocert`，无需额外组件。证书页面保存邮箱、ACME 服务地址和条款接受状态；已有 YAML 的 ACMEEmail / ACMEDirectory / ACMEAcceptTerms 作为尚未保存页面设置时的默认值。页面设置持久化优先，不必重启。用户须显式接受所选服务条款，不自动代为接受。

自动条目保存后在后台启动申请；可点击“申请”重试，事件展示签发结果，完成后刷新列表。每个明确域名独立签发，有效证书自动进入匹配库；首次 TLS 握手亦可触发申请，续期由 autocert 管理。首次申请可能等待或失败。后台同时最多 4 个申请任务，同一条目重复请求合并。

HTTP-01 要求域名解析到本网关且**公网 80 端口**能转发到 HTTP 监听（本机可为 1080）。443 是常规公网 HTTPS 端口，并不是 HTTP-01 验证本身的端口要求。验证路径先于 HTTPS 跳转/限流处理。生产默认 Let's Encrypt；测试可选择 staging 或自建 ACME 地址。自动签发暂只支持单节点、明确 DNS 域名，暂不支持通配符、DNS-01 与多节点挑战协调；上传通配证书不受此限制。本轮仅做本地验收，没有公网签发验证。

旧 Host HTTPS.Mode=upload/auto 及 SetCertificate API 保留兼容，但新建 Host 统一推荐 adaptive 和集中证书库。

上传证书支持集群，但各节点必须配置同一个 32 字节十六进制 `GATEWAY_CERTIFICATE_KEY`。自动缓存与上传私钥均加密；单节点未配置共享密钥时生成本地 `certificate.key`。

## 持久化与集群

不配置 Redis 时保存到 DataDir；有 Redis 时使用 `Prefix:state` 的 CAS 更新，Pub/Sub 加快同步，并每 5 秒校对，节点心跳有效期 30 秒。配置通过验证且本地最后可用副本保存成功后才应用。异常配置保留先前版本；Redis 不可用时可用本地缓存启动/转发，管理写入不会假装成功。

每个 Host 保留最近 20 个历史版本，管理审计最近 1000 条，运行事件环形队列最近 1000 条，状态最大 8 MiB。后台发布表示控制状态提交成功，应同时检查各节点的应用 revision；未实现跨节点同时切换。

备份须保存整个 DataDir（包括 state.json、admin.password、certificate.key、ACME 缓存），集群还需备份 Redis 状态、Prefix:admin-password 及共享密钥。后台导出为脱敏配置迁移文件，不能替代完整备份；导入时需重新上传证书私钥。

## 旧 Redis 配置兼容

保留 `gateway:host:<domain>` Hash 与 `gateway:channel` 通知，路由支持数组和 KV 对象。示例：

```text
HSET gateway:host:api.example.com proxies '{"/user/*":"user-svc/v1/*"}'
PUBLISH gateway:channel "api.example.com"
```

扫描使用 SCAN；旧配置更新先验证后替换，但旧三类路由更新并非一个 Host 原子事务，建议迁移至后台发布。精确 `/login`、前缀 `/api/*`、正则 `^/user/(.*)$` 路由继续遵循 Service 实现。改写阶段先于代理，静态挂载之后；后台列表不能改变不同阶段的执行顺序。

## 开发验证

```sh
go test -v ./...
go test -bench=. ./...
go test -race ./...
GATEWAY_REDIS_INTEGRATION=1 go test -v ./...
GATEWAY_REDIS_PROCESS_INTEGRATION=1 go test -race -run TestRedisOutage ./core
```

集成测试需本地 Redis。证书测试使用本地 ACME 测试服务，覆盖完整 HTTP-01/TLS 流程，不申请公网生产证书。本轮验收限定为本地环境，公网 ACME 验收不作为待办或发布阻塞项。详见 TEST.md。

浏览器全流程验收夹具位于 testdata/browser，独立于生产二进制。运行 `go run ./testdata/browser`，将输出地址设为 GATEWAY_BROWSER_ADMIN / GATEWAY_BROWSER_FIXTURES 后运行 `node testdata/browser/acceptance.cjs`；Playwright 仅为测试依赖，可通过 GATEWAY_PLAYWRIGHT 指定已有安装，GATEWAY_BROWSER_CHROME 指定浏览器。夹具使用临时数据并在退出时清理。

## Go API（core 子包）

| 入口 | 用途 |
| --- | --- |
| Config / GatewayConf、Run、NewGatewayApp | CLI 全局配置与 starter 生命周期；GatewayApp 提供 Start / Stop / Reload / Status / Manager / AdminAddr |
| NewManager(config, server)、Manager.Close | 组合管理器与 Service；停用 HTTP 处理器后关闭管理器；关闭后拒绝加密缓存读写 |
| Manager.Handler / AdminHandler | 业务请求与管理请求的独立 HTTP Handler；NewManager 不自行启动监听 |
| Manager.SaveDraft / Publish / Rollback / Delete | 修改 Host；expected 为当前草稿 revision，冲突返回 ErrConflict；发布后形成新版本 |
| Manager.Hosts / Events / Nodes / Services / Settings | 读取脱敏 Host、事件、节点、已引用服务及运行参数 |
| Manager.Reconcile | 从权威存储校对并应用完整配置，失败保留旧请求快照 |
| Manager.Preview、ValidateHost | 路由与限流适用性预览，以及结构校验；不消耗额度 |
| Manager.Certificates / SaveCertificate / DeleteCertificate | 独立证书库；expected 校验证书 revision，私钥加密且擦除传入切片 |
| Manager.ACMESettings / SetACMESettings / IssueCertificate | 持久化签发设置（expected 为全局 revision）及有界后台申请 |
| Manager.SetCertificate / CertificateCache | 加密保存上传证书和提供 autocert.Cache；SetCertificate 擦除传入私钥切片 |
| SetAdminPassword(config, safeBuf) | 独立设置单管理员密码；不修改 Host 状态，调用者负责关闭 SafeBuf |
| DefaultAdminListen / WriteDefaultAdminConfig | 选择私有地址的 81 端口；创建默认配置且不覆盖已有目标文件 |
| NewLimiter、Limiter.Evaluate / Sweep | 单独使用限流；Evaluate 包含编译成本，实际发布 Host 的热路径使用预编译规则 |
