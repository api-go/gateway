# Gateway 验证报告

## v1.5.12 未发布后台路由配置验证（2026-10-10）

- `GOWORK=off go test -v ./...` 和 `GOWORK=off go test -bench=. ./...` 通过。
- JS 语法检查、Node VM 四类标签页说明渲染及正则反斜杠输出检查通过；后续本轮编辑交互改动已完成 Chrome/Playwright 全流程验收，22 项全部通过且无页面脚本异常。
- 对照已发布 Service v1.5.42 确认路径匹配、查询参数与静态目录解析。
- Chrome/Playwright 验证发布成功关闭编辑窗口及版本提示、域名自动名称、新规则无鉴权输入且 AuthLevel=0；Node VM 验证既有非零鉴权保留并显示兼容提示。
- TestPreviewProxyTargetWithoutPath 覆盖 llm、llm/、无路径 HTTP/HTTPS 地址的整站目标预览及查询参数保留；代理应用名、协议要求与空目标说明的 Node VM 渲染检查通过。
- 依赖审计与 `go mod tidy` 后复测通过：Cast v1.5.8、Log v1.5.17、Redis v1.5.19、Starter v1.5.12、Task v1.5.13；未使用本地 replace。Redis 外部集成未启用，相应测试按设计跳过。
- `ai/tools/getExports` 在本工作区不可用，使用 `GOWORK=off go doc -all apigo.cc/go/gateway/core` 与导出源码交叉核对 README 的 API 表；本次未新增 Go API。
- Apple M3 Max Benchmark：NewGatewayApp 20.44 ns/op、64 B/op、1 alloc/op；LimiterIPWindow 1410 ns/op、1305 B/op、21 allocs/op；PublishedHostQuota 4907 ns/op、3584 B/op、79 allocs/op。

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
