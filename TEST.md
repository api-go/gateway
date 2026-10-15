# Gateway 验证报告

## v1.5.17 发布验证（2026-10-15）

- 依赖审计：go.mod 所有 apigo.cc/go 直接和间接版本与已发布索引一致；GOWORK=off、无 replace，go mod tidy -diff 无变化。
- 发布全量：GATEWAY_REDIS_INTEGRATION=1 go test -count=1 -v ./... 通过，61 项顶层测试通过；使用现有本机 Redis 与隔离前缀，未启动额外常驻服务。
- 本轮最终实现的 Redis 全量 race 通过（54.659s）；go vet、前端 JS 语法与 git diff --check 通过。
- 发布基准（Apple M3 Max）：APIKeyRead 39776 ns/op；NewGatewayApp 20.88 ns/op；LimiterIPWindow 1435 ns/op；PublishedHostQuota 5662 ns/op；MonitorCheck 5966744 ns/op。
- 周期代表性响应：单机验证一次失败后多次成功仍保留失败正文和准确失败数；全成功保留最后正文；Redis 环写入 15 次保留失败数与连续失败数 15。
- 开发环境浏览器验证小时/天周期详情、检查数/平均耗时/失败数、旧数据缺失标注、Host 顺序一致及无重复频率副标题；配置前后相同，无浏览器运行错误。
- ai/tools/getExports 不可用，采用 GOWORK=off go doc -all ./core 与导出源码交叉核对，更新 README.md / README.en.md 的完整 API 清单。
- OA 签名、业务回执及低代码钉钉示例由本地接收端覆盖，未向真实机器人发送消息；公网 ACME 不在本轮验证范围。

## v1.5.17 API 接入验证（2026-10-14）

- GOWORK=off + GATEWAY_REDIS_INTEGRATION=1：go test -v ./...、go test -bench=. ./...、go test -race ./... 均通过，使用现有本机 Redis 与隔离前缀。
- API Key 覆盖：管理员 Cookie/CSRF 边界、摘要不回显、错误 token、Host / 操作权限隔离、保存/预览/发布、revision 冲突、重启持久化、到期拒绝、单机和 Redis 跨节点撤销、公开 TXT 指引。
- Chrome / Playwright 全流程验收通过，无页面运行错误；包含创建 Key、通过 Bearer 保存及发布、越权拒绝、读取 TXT 与撤销。桌面界面截图检查通过。
- Apple M3 Max：APIKeyRead 40880 ns/op（含本地权威状态读取及 HTTP Handler）；NewGatewayApp 22.95 ns/op；LimiterIPWindow 1430 ns/op；PublishedHostQuota 5406 ns/op。
- 导出检查使用 GOWORK=off go doc -all ./core 与源码交叉核对（替代 ai/tools/getExports）；新增 APIKey / State.APIKeys。JS 语法和 git diff --check 通过。

## v1.5.16 发布验证（2026-10-14）

- GOWORK=off 使用正式依赖；启用 Redis 的全量、基准与 race 检查通过。
- TestGateway 断言动态测试前缀的 manage / channel 通道已有活跃订阅，验证通知更新与停止。
- go doc -all 与源码核对，公共 API 保持一致。
- Apple M3 Max：NewGatewayApp 18.59 ns/op、LimiterIPWindow 1305 ns/op、PublishedHostQuota 4900 ns/op。

## v1.5.15 发布验证
- 2026-10-14 补充：`GOWORK=off GATEWAY_REDIS_PROCESS_INTEGRATION=1 go test -v -run '^TestRedisOutageRetainsTrafficAndRecovers$' -count=1 -timeout=60s ./...` 通过（场景 0.24s）。使用随机端口的独立临时 Redis，验证中断期间保留路由、拒绝依赖 Redis 的操作及重启后恢复；测试进程已退出，现有 Redis 返回 PONG。
- 2026-10-14 本次 Open 联动：GOWORK=off 全量（GATEWAY_REDIS_INTEGRATION=1）、基准和发布代理专项 race（2 次）通过；Redis 进程故障模拟已单独启用并通过（见补充记录）。Apple M3 Max：NewGatewayApp 20.29 ns/op，LimiterIPWindow 1424 ns/op，PublishedHostQuota 5425 ns/op。
- `TestPublishedProxySharesHTTP2AndWebSocketTLSListener` 验证发布后路由、同一 TLS 监听的 HTTP2/WS、公开 Host、Origin、Cookie、子协议、二进制和关闭码；`TestPublishedProxyRetainsWebSocketRejection` 验证拒绝状态/正文/Cookie。
- `GOWORK=off` 构建依赖已发布 Service v1.5.45、Discover v1.5.17、HTTP v1.5.12；go.mod 无 replace。导出 API 使用 go doc -all 与源码核对。
- 2026-10-14：`GOWORK=off go test -v ./...`、`go test -bench=. ./...` 通过；需显式环境开关的 Redis 集成场景仍跳过。Apple M3 Max：NewGatewayApp 19.35 ns/op，LimiterIPWindow 1270 ns/op，PublishedHostQuota 4790 ns/op。

## v1.5.14 发布验证（2026-10-10）

- 依赖已发布 Service v1.5.44 与 Discover v1.5.16，GOWORK=off；go.mod 无 replace。
- 公开 Host 回归覆盖 HTTP/HTTPS、协议转发头与伪造头覆盖；Redis 发现代理验证公开 Host。
- 证书名称从 SAN 自动读取；证书/私钥传错栏拒绝，证书配对验证与泛域名匹配通过。
- Redis 自动密钥覆盖并发初始化、多节点解密、重启复用、元数据丢失拒绝覆盖和本地缓存恢复。
- 普通测试、基准及 race 使用已发布依赖；启用 Redis 集成及隔离临时 Redis 的停止/恢复场景。
- API 核对采用 GOWORK=off go doc -all 与导出源码交叉检查，公开 Go API 未改变。
- Playwright 覆盖 22 项后台交互：上传证书无需名称、发布关闭编辑器、路由配置与密码更新等。
- 本次 Apple M3 Max 基准：NewGatewayApp 20.68 ns/op、64 B/op、1 alloc/op；LimiterIPWindow 1425 ns/op、1305 B/op、21 allocs/op；PublishedHostQuota 5418 ns/op、3713 B/op、87 allocs/op。

## v1.5.12 后台路由配置验证（2026-10-10）

- `GOWORK=off go test -v ./...` 和 `GOWORK=off go test -bench=. ./...` 通过。
- JS 语法检查、Node VM 四类标签页说明渲染及正则反斜杠输出检查通过；后续本轮编辑交互改动已完成 Chrome/Playwright 全流程验收，22 项全部通过且无页面脚本异常。
- 对照已发布 Service v1.5.42 确认路径匹配、查询参数与静态目录解析。
- Chrome/Playwright 验证发布成功关闭编辑窗口及版本提示、域名自动名称、新规则无鉴权输入且 AuthLevel=0；Node VM 验证既有非零鉴权保留并显示兼容提示。
- 证书页面补充 SAN 自动读取、名称备注、泛域名覆盖和 PEM 私钥说明后，22 项 Chrome/Playwright 验收再次通过；Node VM 检查 Redis 共享密钥已配置/缺失提示通过。Settings 只新增 sharedCertificateKey 布尔状态，不返回密钥值。
- TestPreviewProxyTargetWithoutPath 覆盖 llm、llm/、无路径 HTTP/HTTPS 地址的整站目标预览及查询参数保留；代理应用名、协议要求与空目标说明的 Node VM 渲染检查通过。
- 依赖审计与 `go mod tidy` 后复测通过：Cast v1.5.8、Log v1.5.17、Redis v1.5.19、Starter v1.5.12、Task v1.5.13；未使用本地 replace。Redis 外部集成未启用，相应测试按设计跳过。
- `ai/tools/getExports` 在本工作区不可用，使用 `GOWORK=off go doc -all apigo.cc/go/gateway/core` 与导出源码交叉核对 README 的 API 表；本次未新增 Go API。
- Apple M3 Max Benchmark：NewGatewayApp 20.73 ns/op、64 B/op、1 alloc/op；LimiterIPWindow 1382 ns/op、1305 B/op、21 allocs/op；PublishedHostQuota 5063 ns/op、3633 B/op、82 allocs/op。

## v1.5.11 发布验证（2026-10-06）

运行 `go test -v ./...`、`go test -bench=. ./...`、`go test -race ./...` 与 `GOWORK=off go test ./...`。Redis 集成通过 `GATEWAY_REDIS_INTEGRATION=1` 显式启用。

覆盖：

- 草稿不影响流量，发布/回滚/重启恢复，非法规则保持原路由，域名冲突、过期 revision、读对象隔离。
- Host/Path/IP/UA/Header/Session/Cookie 联合计数，缺失维度处理、周期过期、观察模式、高基数容量；100 个并发请求严格限制允许次数。
- 实际代理正常传输 Body；超限在读取请求体、访问上游之前拒绝。原 Service 代理/重写/静态/Redis 通知兼容。
- Redis CAS、多节点配置与删除收敛、共享额度、节点心跳；主状态丢失后持续转发、缓存重启与拒绝失去权威状态的写入。
- 自建临时 Redis 真实停止/重启：无集群额度的路由继续服务、集群额度返回 503、缓存可重启、管理写入及登录拒绝，恢复后配置写入与跨节点额度恢复；不会停止用户 Redis。通过 GATEWAY_REDIS_PROCESS_INTEGRATION=1 启用，需要 redis-server。
- 单密码登录、未初始化锁定、CSRF、来源检查、登录限速、单机及集群密码修改撤销会话、移除账号 API；私有地址选择、配置生成与不覆盖已有文件。
- 独立证书库的精确/单层通配匹配、同 Host 多域名真实 SNI 握手、证书即时添加/删除/过期的 HTTPS 与 HTTP 回退、10443 端口跳转、证书库共享密钥要求与跨节点上传/删除收敛、读对象隔离、CSRF、revision 冲突、签发设置重启恢复。
- 上传证书私钥加密和 API 脱敏、加密缓存往返、管理器关闭与后台缓存写入的并发安全、上传证书与已缓存自动证书的真实 TLS 握手。
- 本地 ACME 目录服务驱动官方 autocert 完整 HTTP-01 申请，验证请求实际进入 Gateway，签发证书用于真实 TLS 握手及加密缓存。测试 CA 不验证 JWS 签名，不能代替公网 CA 的部署验证；自动续期由 autocert 提供，未等待真实续期周期。
- Chrome/Playwright 可重复验收脚本覆盖密码初始化前锁定、登录、Host 新建、四类路由及实际请求、七维联合限流、观察模式、预览、发布、历史回滚、证书库上传、编辑、删除与即时匹配、页面保存签发设置、自适应 HTTPS 跳转及关闭跳转、独立路由标签页与固定高度弹窗、导入导出、事件、节点、设置、错误配置、并发冲突、停用/启用、删除、改密撤销会话、移动布局及退出；测试夹具位于 testdata/browser。

Apple M3 Max 示例 benchmark（包含测试请求/响应对象构造，不等同于网络吞吐）：

| 场景 | ns/op | B/op | allocs/op |
| --- | ---: | ---: | ---: |
| NewGatewayApp | 20.52 | 64 | 1 |
| LimiterIPWindow（公开帮助 API，含规则编译） | 1380 | 1305 | 21 |
| PublishedHostQuota（实际已编译 Host 请求链） | 4888 | 3584 | 79 |

正常业务请求不执行配置编译/数据库配置读取，但限流计数使用锁或 Redis；不能将旧文档的“完全零锁、零抖动”描述用于当前完整请求链。集群同步采用发布通知加周期校对，不能保证所有节点瞬时切换。

完整 Redis 集成 race 检查通过；发布前以 GOWORK=off、已发布 Service v1.5.42 / Discover v1.5.15、无本地 replace 验证独立模块与浏览器流程。

命令行实际验证覆盖首次生成配置、终端不回显、两次确认和不一致拒绝，标准输入密码路径、启动/状态/停止、运行中改密及旧会话撤销。本轮按用户确认仅做本地验收：公网 ACME staging 不作为待办或发布阻塞项；真实续期周期和跨浏览器兼容性未验收。

## 验收范围

- 浏览器：Chrome / Playwright 的 22 组主流程及异常流程全部通过，无页面脚本错误；包含移动视口，不代表所有移动设备或其他浏览器兼容性已验证。
- 后端：TLS / ACME、可信代理 IP、多节点 CAS/限流、Redis 故障恢复、并发缓存关闭等通过 Go 测试及 race 验证。这些结果不归为浏览器验收。
- 文档：Gateway、Service、Discover 的 README / TEST / CHANGELOG 及模块索引已同步；版本及依赖已同步到发布版本，不含本地 replace。

## 历史验证

v1.5.10：Redis 集成测试改为显式启用；基础测试与 benchmark 通过。

## v1.5.12 发布验证（2026-10-10）

- GOWORK=off go test -v ./... 与 go test -bench=. ./... 通过，依赖均为已发布 tag。
- API 以 GOWORK=off go doc 与导出源码交叉核对（getExports 工具不可用）。
- GOWORK=off go test -race ./... 通过。

```text
BenchmarkNewGatewayApp-16         	61678689	        20.05 ns/op	      64 B/op	       1 allocs/op
BenchmarkLimiterIPWindow-16       	  874876	      1410 ns/op	    1305 B/op	      21 allocs/op
BenchmarkPublishedHostQuota-16    	  232284	      5161 ns/op	    3633 B/op	      82 allocs/op
```

## v1.5.13 发布验证（2026-10-10）

- GOWORK=off go test -v ./... 与 go test -bench=. ./... 通过，依赖均为已发布 tag。
- API 以 GOWORK=off go doc 与导出源码交叉核对（getExports 工具不可用）。
- GOWORK=off go test -race ./... 通过。

```text
BenchmarkNewGatewayApp-16         	57398643	        20.62 ns/op	      64 B/op	       1 allocs/op
BenchmarkLimiterIPWindow-16       	  856179	      1432 ns/op	    1305 B/op	      21 allocs/op
BenchmarkPublishedHostQuota-16    	  221820	      5252 ns/op	    3633 B/op	      82 allocs/op
```

## 开发服务器代理验收（2026-10-10）

- Service 外部回归覆盖 200、204、302、303、401、403、503、响应正文、Location 和两个 Set-Cookie。
- LLM/QBank 直连与 HTTPS Gateway 对比：登录 303，未登录 API 401；浏览器实际到达 Account 登录页。
- 正式 Linux amd64 构建使用已发布 Service v1.5.44，不包含本地依赖替换；部署完成后核对远端 SHA256 和容器健康状态。

## 2026-10-15 开发服务器验证

- 在开发服务器固定 Go 镜像、独立临时源码与测试数据下执行 go test -v ./...、go test -bench=. ./...、go vet ./... 与 go test -race ./...，均通过；没有写入共用业务数据。
- 上游测试覆盖 GET 固定方法、域名/路径与显式业务 Header、管理员 Cookie/CSRF 不自动转发、302 不跟随、4096 字节截断、401 业务响应、服务发现、取消请求、未匹配路径和非法路径。
- 外部开发验收脚本在实际后台验证一键规则测试、knowbase /api/health 上游 HTTP 200、重复域名失败在弹窗内可见及无成功提示；拦截成功响应验证窗口关闭与节点状态刷新。前后实际 Host 状态完全一致，无页面运行/CSP 错误。
- 开发服务器 i5-12400F：APIKeyRead 25551 ns/op；NewGatewayApp 25.97 ns/op；LimiterIPWindow 1971 ns/op；PublishedHostQuota 7409 ns/op。

## 配置与业务测试标签页验证（2026-10-15）

- CheckHost 与 Publish 共用发布校验；覆盖非法配置、重复域名、管理员认证/CSRF 边界，并断言校验不访问上游、不保存配置。
- 实际后台验证默认配置页绿色通过且没有业务请求；业务页首次自动 GET、参数修改重测、绿色 HTTP 200 与红色 HTTP 401、错误配置提示及 Host 参数恢复。Header/Cookie 不写入 LocalStorage。
- 固定 Go 镜像中执行全量测试、基准、go vet 与 race 均通过。i5-12400F：APIKeyRead 25687 ns/op、NewGatewayApp 25.80 ns/op、LimiterIPWindow 2089 ns/op、PublishedHostQuota 7505 ns/op。

## 服务发现与监控验证（2026-10-15）

- GOWORK=off 全量、基准、go vet 与 race 通过；全量与 race 启用隔离前缀的 Redis 集成测试。
- 只读注册中心覆盖未引用服务、多个权重节点、过期心跳，并断言不删除注册数据。
- 监控覆盖定时调度/停止、间隔跳过、直接 URL/查询参数、加密请求 Header、低代码 JSON 检查、历史重启保留、配置变化后旧结果失效、历史损坏隔离、原加密密钥缺失保护。
- Redis 模式覆盖同一 Host 的跨节点检查租约和共享历史。脚本取消测试验证无限循环及时中断。
- 独立 HTTP 模拟接收端验证钉钉/飞书 HMAC 签名及消息格式、企微业务回执、HTTP 200 但业务失败、连续失败阈值、故障去重和恢复通知；高级发送验证 HTTP 实际调用与自定义回执判断。没有向真实 OA 发送测试消息。
- 实际后台浏览器验证服务发现全量四个注册服务、Host 名称入口与三列状态、监控表单及通道绑定、签名选项、凭据清空、历史颜色/最近样本通过率与小屏导航；写入动作拦截，实际 Host 状态前后相同，无页面运行错误。桌面截图检查通过。
- Apple M3 Max：APIKeyRead 39255 ns/op；NewGatewayApp 21.52 ns/op；LimiterIPWindow 1437 ns/op；PublishedHostQuota 5857 ns/op；MonitorCheck 8512716 ns/op（包括网络及最多 200 条历史的文件持久化）。
- 导出使用 go doc -all ./core 与源码核对；新增监控、注册中心和报警入口。依赖对齐已发布 API v1.5.19、JS v1.5.17、Service v1.5.46。

## 会话与报警交互修订（2026-10-15）

- 新测试验证登录跨 Manager 重建、会话密文持久化、伪造 Cookie 拒绝、退出和改密码撤销。
- 钉钉高级示例直接从界面源码提取执行，模拟接收端验证时间戳、HMAC 签名和消息；高级模式无表单 Webhook/密钥也可保存。
- 勿扰支持跨午夜；使用 UTC 当前时间段验证不发送、解除勿扰发送、60 秒内不重复及历史重启后的故障重复提醒。

- 本轮全量测试、隔离 Redis 集成、race、基准、go vet 与 JS 语法检查通过；基准 MonitorCheck 7548585 ns/op，APIKeyRead 51462 ns/op。
- 开发环境浏览器验证 10 列 Host、域名/目标整体入口、网关自身及调用协议、报警弹窗/条件签名/高级钉钉示例/频率勿扰、移动端、管理日志命名；真实容器重启与页面刷新后 Cookie/CSRF 保留，退出撤销。模拟写入后关闭弹窗并正确回填；Host 前后数据一致，无运行或 CSP 错误。

## 监控矩阵与发现客户端（2026-10-15）

- 默认未配置的已发布代理自动检查所有域名；验证一域名 200、另一域名 503 时整轮失败及逐域名记录，显式关闭不请求、指定域名仅请求该域名。
- 小时统计跨 Manager 重建保留；填入 300 小时统计验证不受 200 条详情上限限制。
- 独立 Redis 测试前缀验证 Gateway 启动即检查已发现 HTTP 服务、解除自身注册后发现及调用仍正常；测试使用原生 Service 集成套件同一注册 DB，避免跨 DB 全局客户端混用。

- 本轮全量、Redis 原生服务集成、race、基准、vet、JS 语法和 diff 检查通过；MonitorCheck 基准 11161445 ns/op（包括多域名调度、原始历史和小时统计写入）。
- 实际开发浏览器验证 7 个 Host 自动产生记录、24 小时/30 天矩阵、最新列默认可见、时间格筛选、逐域名详情、空时间段、保存/取消间距、解除 Gateway 自身注册、移动端与侧栏导航；Host 配置前后相同，无 CSP/运行错误。覆盖主内容横向滚动及粘性列与侧栏的层级边界。

## 结果分类与存储边界（2026-10-15）

- 覆盖默认 302 通过、指定 200 时 302 不通过、302 内容检查不绕过；连接失败及 401/403/404/500 分类、原生发现未注册分类。
- 日志清理验证 30 天边界及保留正在写、其他应用、备份和无切割单文件。探测专用日志器使用原生 CLOSE 级别。

- Redis 小时统计升级为按小时 Hash 增量计数，验证旧 JSON 统计无损迁移、旧快照保留、重复写不重复迁移及 30 天 TTL。

- 本轮全量、Redis 集成、race、基准、vet、JS 语法及 diff 检查通过。实际开发浏览器验证 LLM 最新 302 为通过、grading 显示未注册服务、成功格仅平均 ms/失败仅 ×、60 秒检查及设置页 30 天保留；Host 配置前后相同。
- 验证时 7 个 Host 共 183 条监控详情与小时统计的 JSON 正文约 94874 字节（不含 Redis 自身开销/持久化文件），普通日志目录约 22 MiB。旧结果保留其检查时的判定，不回写伪造历史。

## 64 槽监控结构（2026-10-15）

- 验证 720 小时旧记录折算日统计、30 天/24 小时裁剪、单 Hash 字段上限 64、最近检查环 10 条、total/times 累加及 error 不被成功清空。
- Redis 验证旧值迁移一次、旧键提交后清理、原子计数与 30 天 TTL；单机相同结构，旧文件迁移后清理。
- 持久提醒测试改为修改新槽结构，重启后的提醒仍通过；原有故障阈值、恢复、勿扰、监控生命周期及发现客户端回归通过。

- 本轮全量、Redis 集成、race、基准、vet、JS/diff 均通过；环写入 15 次仍保留连续失败数 15，告警重启/勿扰回归通过。监控写入基准约 5975001 ns/op。
- 开发浏览器验证 7 Host 迁移后近期详情合计 70 条、小时最多 24 桶及日桶最多 30 个、302 判定、失败分类、ms/× 显示和 60 秒周期，实际 Host 配置未改变。

监控周期详情：每周期累计检查次数、耗时、失败次数，仅保存一份响应（存在失败保留最近失败，否则最后成功），正文上限 4096 字节。旧周期未记录的失败次数/响应明确标注；近期 10 槽用于连续失败和报警状态。Hosts 与监控按添加时间升序，旧 Host 从历史/管理日志推导，缺失时以 ID 稳定排序。
