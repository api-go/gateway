# Gateway

[English](README.en.md)

基于 `github.com/api-go/service` 的动态网关。**v1.5.25** 提供管理后台、ACL、Host 回收站、独立限流与集中证书管理，依赖已发布的 Service v1.5.52、Discover v1.5.21、API v1.5.21、JS v1.5.17。此处以实际实现为准。

Redis 通知订阅随 GatewayApp 启动与停止，管理配置和旧路由通知及时生效。

## Host 回收站

Host 配置页的“移入回收站”停止托管该 Host，保留其 ID、创建时间、草稿、已发布配置、历史版本及加密凭据；本地/旧 Redis 的同域名规则仍沿用既有回退行为。Host 列表和普通导出不包含回收站内容。点击 Host 页的“回收站”查看删除时间及操作人、恢复或永久删除；无自动过期或自动清空。

恢复会重新启用删除前的已发布配置，并增加发布版本及修订号；尚未发布的草稿仍独立保留，纯草稿恢复后不会自动发布。域名已被其他 Host 占用、证书失效或配置校验失败时拒绝恢复，原记录保留。回收站中的 ID 不允许直接新建覆盖。永久删除只移除回收站内的配置，不能直接作用于活动 Host；审计和独立运维备份不受影响。

API：`Manager.Trash() []DeletedHost`、`Manager.Restore(id, expected, actor) (HostRecord, error)`、`Manager.Purge(id, expected, actor) error`；`Manager.Delete` 改为软删除。`DeletedHost` 含 Record、DeletedAt、DeletedBy；返回的 Record 已脱敏。`State.Trash` 与现有 State 共用 Redis CAS、通知、周期同步及本地备份。操作使用回收站 Record.Revision，冲突返回 409；永久删除后不能通过回收站恢复。管理接口为 GET `/api/trash`、POST `/api/restore`、POST `/api/purge`；Bearer 程序接口使用对应 `/api/v1/` 路径，分别要求 read、publish、delete 权限及 Host 授权。删除仍使用 POST `/api/v1/delete`。

回收站是新增持久化字段，集群各节点应统一升级后使用，避免旧二进制写配置时丢弃该字段；不支持降级后继续写入回收站配置。

## ACL 访问控制

Host 使用 `ACL []ACLRule` 保护请求范围。每项分为 `Scope`（范围条件全部满足）和 `Allow`（放行方式，任意一项满足）。范围内默认拒绝，范围外不受该项影响；空 Scope 保护整站，空 Allow 全部拒绝。重叠范围必须全部通过，顺序不影响结果。允许之后仍执行限流。

ACL、代理、重写、重定向、静态文件、限流和会话保持使用摘要列表，添加和编辑通过独立弹窗完成；取消或 Esc 不修改原规则，确认后仍需保存草稿并发布。规则删除有独立确认；“移入回收站”仅在配置标签页显示。ACL 新建路径留空，无 admin 快捷按钮或预设。

后台不要求名称、不提供 allow/deny 排序；根据范围与参数生成摘要，IP/CIDR 支持批量输入。限流同样自动显示路径、计数维度和次数/周期。内部 ID 自动生成并随保存保留；API 编辑时保留响应 ID，Name 仅为旧配置兼容字段。

```yaml
ACL:
  - Enabled: true
    Scope:
      - {Source: path, Operator: prefix, Value: /admin/}
    Allow:
      - Conditions:
          - {Source: ip, Operator: cidr, Value: 192.168.10.0/24}
      - Conditions:
          - {Source: ip, Operator: equals, Value: 203.0.113.10}
```

前缀 `/admin/` 仅覆盖该前缀；例如手动配置 `Operator: directory, Value: /admin`，用字符串比较覆盖 `/admin` 及 `/admin/...`，不匹配 `/administrator`，无需正则。范围及放行条件复用 Host/Path/IP/UA/method/header/cookie/session 与 equals/prefix/directory/contains/regex/cidr/exists；directory 只用于 Path。Path 是进入网关时的已解码路径。IP 直接使用既有 `request.ClientIp()`，不改变信任或转发机制。

每个 `Allow` 元素是 `ACLGrant`：`Conditions` 内部 AND，多个 grant 之间 OR；高级条件和脚本无需额外名称。可选 `Script` 是 `github.com/api-go/js` 匿名布尔函数，作为该 grant 的额外条件：

```javascript
function(request) { return request.method === 'GET'; }
```

true 满足本项，false、异常、超时或非布尔返回值不构成放行；仍可由其他 grant 放行，没有任何放行则 403。原生 grant 优先于脚本执行。脚本输入副本含 host/path/method/ip/ua/session、headers（标准名称 → 字符串数组）、cookies；不读请求体。SafeMode、16384 字节脚本上限、默认 10ms/可设 1–100ms 的 TimeoutMS 及请求取消保持支持。客户端自报 Header/Cookie 不是认证凭证。

`ACLRule` 新配置字段为 ID、Enabled、Scope、Allow、Mode（enforce 默认/observe）；`ACLGrant` 为 Conditions、Script、TimeoutMS。observe 仅记录判定，不拒绝。最多 100 个范围、每个最多 100 项放行、每组最多 20 个条件。入口执行先于限流、重写、代理、静态资源和 WebSocket 握手，不追溯关闭存量连接。

`NewACL([]ACLRule) (*ACL,error)` 预编译并可并发复用，`ACL.Evaluate(*service.Request) ACLDecision` 返回最终 RuleID/Allowed/Observed/Error。无请求路径 Redis 访问；JS 使用编译执行池。规则参与草稿、发布、回滚、重启和节点同步。公开 Preview 返回 ACL 判定；配置检查、监控路由解析和直接上游测试不执行入口 ACL。API Key 可编辑声明式条件，脚本增删改沿用管理员边界。

首个开发版的 Action/Conditions/Script 有序规则保留兼容；新 Scope 策略始终先执行，旧 allow 不能绕过新保护范围。后台只对可证明等价的两条 allow→deny 模板转换为单个 Scope/Allow，其他旧配置保留兼容执行。Gateway 保持纯 L7，不引入 L4 模式或系统网络管理。

## 启动

`go.mod` 使用已发布 Service v1.5.48 与 HTTP v1.5.14 的 WebSocket 代理实现，普通请求继续使用既有 HTTP/2 / H2C 配置，WebSocket 独立协商 HTTP/1.1 或 RFC 8441 HTTP/2。

同时依赖 HTTP v1.5.13、Discover v1.5.17 的统一 Open 接口，发现代理可透传握手 Cookie 和拒绝响应。WebSocket 超时/消息限制/压缩设置在 `service.yaml` 的 `WebSocketProxy` 下；默认握手与写入 30 秒，空闲和消息大小限制关闭，压缩关闭。私有 CA / mTLS 可通过 Service 的代理客户端或独立 Dialer 配置。

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

发布版使用已发布的 Service v1.5.50 和 Discover v1.5.19，不含本地 replace；可在不保留相邻模块 checkout 的情况下独立编译。根目录 CLI 保持兼容，可复用的 Go API 位于 `github.com/api-go/gateway/core`。

## 会话保持与节点同步

后台有独立“会话保持”标签页，支持多条 `Host.AffinityRules []AffinityRule`。按顺序匹配进入 Gateway 时的路径和 HTTP 方法，首条匹配规则生效。`Path` 支持完全匹配和 `/*` 前缀，`Methods` 空表示全部。规则使用独立 ID，互不混用映射；仅影响后续 Discover 代理节点选择，不改变固定 HTTP(S) URL。旧 `Host.Affinity` 保留兼容，未匹配新规则时作为旧版全局回退。

四种模式均提供一句话场景提示及可点击灯泡展开的适用场景、收益和代价。无状态服务选 off；旧会话需在扩容后保留归属选 auto；允许节点变化时部分会话漂移、希望不存会话表选 hash；节点及业务分区明确、需要指定主备选 manual。

```json
{"affinityRules":[{"id":"lesson","path":"/lesson/*","methods":["GET"],"affinity":{"mode":"auto","source":"query","name":"lessonid","missing":"reject","ttlSeconds":3600}}]}
```

`AffinityConfig` 字段：

- `Mode`：空 / `off` 关闭；`auto` 本地自动映射；`hash` Rendezvous 确定性哈希；`manual` 手动值和主备地址映射。旧 `redis` 模式名作为 `auto` 别名，不再逐请求查询 Redis。
- `Source`：header / query / cookie（必须填 Name）、session、ip。session 默认名称 sessionid，按 Header → Cookie → Query 取首个非空值；Service 自带会话填 `Session-ID`。ip 使用已解析可信客户端地址。
- `Transform`：空 / full 使用完整值；last 取末尾 ASCII 字节（大小写敏感）；lastDigit 要求整个值为十进制数字并取个位数。
- `Missing`：空 / balance 走普通均衡，reject 返回 400；不提供隐式 IP 降级。标识最长 4096 字节。
- `TTLSeconds`：自动映射的固定有效期，0 默认 3600，最大 604800 秒；命中不续期。过期、修改规则配置或丢失全部有效副本后，标识可能重新分配。
- `Mappings`：手动模式的 `{values:[], nodes:[]}` 数组；nodes 是 Discover 中实际地址，依次为主、备用。完整值/末位等变换后匹配 values；未映射的值或所有指定节点不可用返回代理 502。

自动保持首次在本地按完整标识产生确定性的主选和最多两个备用节点，并立即转发；后续本地查询，无等待 Redis。已有有效映射不被其他节点覆盖，扩容不改变已记录的归属；故障按已保存的主备顺序切换，原主节点恢复后按原顺序选取。若原始主备全部消失，从当前可用节点按哈希重新选择，此时不保证继续扩容时备用归属不变。不会复制业务 Session 数据。

**一致性取舍**：相同上游视图一般产生相同初始归属；并发首次分配遇到不同视图时，各节点保留各自先建立的结果，并计入 conflicts。不会以稍后收到的广播覆盖，差异可能持续到有效期结束；这是高性能最终传播方案，不是强一致会话保持。新连接不等于新会话；始终按规则标识建立记录。HTTP/2 连接复用不代替跨连接及上游节点的保持。

hash 提供两种 `HashStrategy`，均不保存逐会话映射：

- 空 / `direct`：纯 Rendezvous 哈希。正权重节点等概率参与，不按权重比例分配；扩容期望移动约 1/(N+1) 的标识，且只迁到新增节点，缩容只影响原属删除节点的标识。无共享分片表，不能区分新旧会话。
- `gradual`：4096 固定分片共享表。所有 Gateway 本地查表、累计近期请求量，后台复用 affinity-events / 短期 journal / 加密快照通道，游标使用 ID 和内容指纹识别日志重建。Redis 保留有界分片表（活动时续期 1 小时）及每 Gateway 的 10 秒负载报告，无逐 Session Redis 数据。分片表、节点主租约及版本不依赖 Cookie，也不要求客户端提供版本。

渐进调度通过 5 秒 Redis 租约自动选举主 Gateway；只有当前持租约者能用 CAS 写入表并原子发布版本事件，旧主及乱序旧版本不能覆盖新表。主节点仅协调分片分配，不集中业务请求或管理入口。每个 Gateway 约每秒后台广播一次 30 秒衰减窗口负载，报告缺失时从短期键补取；负载不齐或发现视图不一致时暂停主动迁移，不把缺失报告当成空闲。失效 Gateway 的管理存活记录过期前，可能暂缓迁移；现有路由继续使用本地表。

拓扑及负载信息齐备后，一次计算并广播所需的低负载分片调整，不设迁移间隔或分批限额。优先从分片数超额的节点迁移近期请求最少的分片；新增节点获得分片份额，但不追求实际流量立即均衡。稳定拓扑不会仅因负载变化反复搬迁。内部版本只用于同步防旧数据覆盖，业务请求只读取本地当前表。服务发现、后台约每秒一次的负载汇总及异步传播仍有耗时，不承诺全链路毫秒级一致。主节点不可用时，同一失效分片确定性选择可用备用。初始表尚未就绪时返回503；已有就绪表在Redis/调度主不可用时仍可转发。单实例最多64张活动分片表，每个保持规则与Discover应用组合一张，每表最多1024个上游地址。配置及内部同步协议必须在所有Gateway一致，NodeID必须唯一。

WebSocket已建立隧道及正在执行的HTTP/SSE流不因分片更新中断；同一HTTP/2连接中的后续请求仍按当前表选路。分片模式不承诺旧会话零漂移；需按标识保持旧归属时使用自动保持模式（受TTL、容量及故障边界约束）。

删除旧迁移参数时仍保留此前开发构建的规范化路由摘要，使原有映射和分片scope可继续寻址；集群实例应统一升级，以保持新的分配行为一致。

渐进迁移降低的是受影响流量，不保证旧会话零漂移：请求少的分片可能仍有活跃会话；全部分片有流量时也会迁移其中的旧标识。它也不能仅凭哈希识别新会话。需要扩容不动有效旧映射时仍选 auto。各 Gateway 发现视图短暂不一致时，故障备用选择可能暂不一致；极端全部副本和 Redis/检查点丢失不能恢复原分片归属。

### 同步、内存与恢复

GatewayApp 自动启动后台同步。请求路径完全在内存完成，包括首次分配；新记录最多约100ms后批量写入 Redis 并 Pub/Sub 广播，接收端立即应用。每个 Gateway 维护独立日志游标，空闲时约每秒校对，不使用竞争消费的 Consumer Group。Redis `Prefix:affinity-journal` 是 Redis Stream（要求 Redis 6.2+），最多512个批次、每批最多256条，按Redis时间保留最多5分钟并整体TTL。它是短期增量，不是全量会话索引；保留上限超出后需要节点快照补齐。

新节点或游标落后超出日志窗口时，从节点心跳里的管理地址请求加密分页快照，随后重放增量。接口 `/api/affinity/peer` 使用共享存储密钥的短时身份凭据，响应加密；不接受普通 API Key 或匿名访问。管理地址自动从监听地址推导；跨 NAT、独立端口映射或代理时，通过 `Config.AffinityAdvertise` 指定同集群可达的管理 base URL。集群须使用相同 Prefix / 密钥以及唯一 NodeID。新节点须同步就绪后才处理 auto 请求，期间返回503。没有其他在线副本时从自己的检查点或空状态启动；不能凭空恢复其他已丢失节点的独有记录。

每个节点内存保存有效映射；`MaxAffinityEntries` 默认100万，达到上限拒绝新增而不静默淘汰已有会话（代理502）。配置及状态接口展示 entries / pending / conflicts / capacityRejected / ready。会话内容只以SHA-256摘要作为索引，物理地址保存主备链；实际内存还包含Go映射、切片及同步待发送数据，未承诺每条50字节。当前不做冷热磁盘查表，磁盘只作恢复检查点。

后台约30秒及正常关闭时向 DataDir 写权限0600的 `affinity-<prefix摘要>.json` 检查点；节点快照、增量和本地检查点共同恢复。进程崩溃且没有任何已收到最近记录的节点时，检查点之后尚未持久化的映射可能丢失。Redis 故障不阻塞已就绪节点的本地处理和分配，待发送记录留在本地，恢复后补发；新加入节点若无法可靠补齐则保持未就绪。不应把通知机制当作永久备份或强一致协议。

直接使用 `NewManager` 的库调用方应在开始处理自动保持前调用 `SyncAffinity(ctx)` 并在后台周期调用；GatewayApp 已管理完整生命周期。`AffinityStatus()` 返回不含原始会话标识的运行状态。规则配置的保存、发布、回滚及 API 复用现有流程。

### 四层部署与配置恢复

CLI 未指定 gateway.Redis 时使用 service.Register；显式 gateway.Redis 可以分离管理存储。所有实例使用相同Redis/Prefix、各自持久DataDir和唯一NodeID，即可同步配置和证书库。监听、信任策略及本地目录仍是节点启动配置。LVS可做四层TCP分发，TLS在Gateway终止；上传证书库共享，HTTP-01自动签发仍限单节点。

Redis可访问但Prefix:state丢失时，从有效本地快照原子恢复配置及自动加密密钥，不覆盖既有状态，密钥冲突拒绝恢复；显式CertificateKey不上传Redis。首个有效快照获胜，不保证它是所有离线节点中最新者；灾后应先启动持有最新完整DataDir的节点。完整DataDir及Redis备份仍必需。State恢复不包括已丢失的监控历史、限流计数、管理员密码、登录会话或旧路由Hash；会话保持映射采用上面的独立恢复机制。

## API Key 程序接入（v1.5.17）

后台新增 **API 接入**：创建、撤销命名 Key，指定 Host ID（`*` 为所有 Host，含未来新 ID）、到期时间和 read / draft / publish / delete 权限。密钥只在创建响应显示一次，权威状态仅保存 SHA-256 摘要；列表展示本节点最近成功认证时间，重启清空此时间。Key 管理仅允许管理员会话和 CSRF，不接受程序 Key。

使用管理监听上的 `/api/v1/` 接口，认证头为 `Authorization: Bearer <API_KEY>`；无需浏览器登录或 CSRF。提供 `GET hosts / nodes` 和 `POST draft / preview / publish / rollback / delete`，复用原 Host 校验、revision、草稿发布及审计；Actor 为 `api-key:<ID>`。Host 列表及节点 versions 按 ID 授权过滤。Key 不授予证书库、设置或其他 Key 的管理权限。Host ID 授权允许修改该 Host 的完整配置，包括域名、代理和静态目录；不是域名或文件目录的沙箱。

后台提供“复制接入说明”、查看及下载 `/api-guide.txt`，TXT 无需认证，不包含凭据，含字段、curl 示例、错误码和节点生效检查。说明使用当前浏览器管理地址，程序所在机器必须能访问该地址；密钥单独配置。跨不可信网络通过 HTTPS 受控入口访问。

Key 与现有单机 / Redis 状态一起持久化、备份，凭据变更带全局 revision。每次程序请求从权威存储校验撤销与到期；Redis 不可用时拒绝凭据认证，不使用保留缓存授权。撤销影响后续鉴权，已经通过鉴权的在途请求可能完成。现有 Cookie / CSRF 管理接口保持兼容。

完整程序接入步骤见 [TXT 指引](core/web/api-guide.txt)。新增导出结构 `APIKey` 和 `State.APIKeys` 仅用于状态模型，`Hash` 为内部摘要，不经 HTTP 返回有效值，不应对外序列化原始 State。

## 后台与发布

导航为 **Hosts、证书、服务发现、监控、管理日志、API 接入、设置**。Host 列表显示域名/目标、协议、独立规则列及生效/配置测试/上游测试状态；固定高度编辑窗口内部滚动，标签页为配置、代理、重写、重定向、静态文件、限流、监控、发布记录：

- Host 无需填写配置名称，后台以首个域名作为列表名称和编辑标题，保存时自动生成 Name；多域名仍全部参与匹配。
- 一个 Host 可以绑定多个域名，默认自动匹配证书；每个域名可使用不同证书。无有效证书时使用 HTTP，证书可用后自动支持 HTTPS，并默认将 HTTP 跳转到 HTTPS；可关闭跳转或选择仅 HTTP。
- 证书库独立管理上传、替换、删除、自动签发；不需要在 Host 上手动选证书。证书变更立即作用于已发布的自动匹配 Host，无需重新发布。
- 配置代理、重写、重定向、静态目录；新代理规则不要求网关身份认证（AuthLevel=0），业务身份认证由目标应用处理；已引用的发现服务提供选择提示。
- 保存草稿不会影响流量；发布前显示变化，校验整份配置后切换；成功关闭编辑窗口并显示版本提示，刷新列表中的节点生效状态，失败保留编辑内容。回滚将历史配置作为一个新版本发布。
- 规则预览显示匹配和路由结果，不转发真实请求、不消耗限流次数。配置导入只创建/更新草稿；导出不包含私钥。
- 设置展示运行参数、节点状态、管理员密码设置说明；自动签发邮箱、服务地址、条款确认在证书页保存并即时生效；Gateway 的全局监听、Redis、可信代理等通过 YAML 配置及重启生效；reload 重载 Service 自身配置并重新校对管理/旧 Redis 路由。

管理 API 使用登录会话、HttpOnly/SameSite Cookie、CSRF、来源校验及登录限速；不提供账号/角色管理 API。每个 Host 的并发修改带 revision 校验，过期编辑返回冲突。后台与业务端口分离。

新管理配置以 Host 为单位完整替换，不与该 Host 的旧路由混合。未纳管域名仍使用原 Service/YAML/Redis 路由；删除管理 Host 后也恢复该域名的原有处理。一次请求固定使用进入时的配置快照。

以下默认值和填写指南从 v1.5.12 起提供。

## 路由填写指南

后台静态文件新增规则默认 `/` → `www/`。`www`、`www/`、`./www`、`./www/` 都支持且等价，相对于 **Gateway 进程工作目录**，不相对于配置文件；也支持绝对目录。URL 使用正斜杠 `/`，无需反斜杠。静态挂载按路径前缀匹配；子目录挂载建议 `/assets/` → `www/`，请求 `/assets/logo.png` 对应 `www/logo.png`；静态挂载不使用通配符或正则。

代理、重写、重定向新增规则默认 `/*`，不启用正则；`/` 仅精确匹配首页。为保持现有 Service 路由兼容，不把 `/` 改成整站匹配，也不另加匹配模式开关；后台每个标签页提供说明和示例。

| 类型 | 匹配路径 | 目标 | 效果 |
| --- | --- | --- | --- |
| 整站应用代理 | `/*` | `llm` | `/v1/models` 转发到 llm 的 `/v1/models` |
| 整站 HTTP 代理（省略目标路径） | `/*` | `https://upstream.example.com` | `/news` 转发到上游 `/news` |
| 整站代理 | `/*` | `https://upstream.example.com/` | `/news` 转发到上游 `/news` |
| 子目录去前缀代理 | `/api/*` | `http://127.0.0.1:8080/` | `/api/users` 转发到上游 `/users` |
| 子目录保留前缀代理 | `/api/*` | `user-svc/api/` | `/api/users` 转发到发现服务 `/api/users` |
| 精确代理 | `/health` | `user-svc/status` | 仅 `/health` 转发到 `/status` |
| 内部重写 | `/old/*` | `/new/` | `/old/a` 内部改为 `/new/a`，地址栏不变 |
| 首页重写 | `/` | `/index.html` | 只重写首页 |
| 整站迁移 | `/*` | `https://new.example.com/` | HTTP 302 跳转，保留路径 |
| 子目录重定向 | `/old/*` | `https://new.example.com/` | `/old/a` 跳转到新站 `/a` |

代理目标可以只写应用名 `llm`（需要已启用的服务发现及可用节点）；直连 HTTP 服务必须以 `http://` 或 `https://` 开头，不自动补协议。省略目标路径时默认 `/`，所以 `llm` 与 `llm/`、`https://upstream.example.com` 与 `https://upstream.example.com/` 等价。是否整站匹配由来源 `path: "/*"` 决定；省略目标路径不会让来源 `/` 变成整站规则。后台新增代理默认匹配 `/*`、认证级别 `0`，目标实际留空，输入框仅提示 `llm 或 https://upstream.example.com`。

末尾 `/*` 为前缀匹配，将后缀追加到目标；目标目录建议以 `/` 或 `/*` 结尾。`/api/*` 匹配 `/api/` 及其下路径，不匹配 `/api`；需要时另加 `/api` 精确规则。普通规则保留查询参数；重写/重定向目标自带 `?` 时替换原查询参数。子目录代理只处理请求路径，不会自动改写上游 HTML 中的绝对资源地址、Cookie Path 或 Location，上游应支持对应的公开访问前缀。

**正则不是由 `*` 触发**：匹配路径包含 `(` 时才按 Go regexp（RE2）编译，两端自动添加 `^`、`$`；不支持前后查找或反向引用。目标用 `$1` 等引用捕获组，`$0` 为完整匹配；建议最多 9 个捕获组。例如代理 `/user/([0-9]+)` → `user-svc/profile/$1`。代理正则只匹配路径，查询参数自动保留；重写/重定向正则匹配完整 URI，含查询参数。兼容查询参数的重写示例：`/user/([0-9]+)(\?.*)?` → `/profile/$1$2`。

重写与重定向共用一个阶段，按保存顺序命中第一条后结束该阶段；内部重写后继续代理、静态处理，外部跳转返回 302。两者由目标是否包含 `://` 区分，本地路径为内部重写，完整 HTTP(S) URL 为重定向。代理同样首条匹配优先，应把具体规则放在 `/*` 之前；静态挂载按更长路径优先。重写和重定向虽分两个标签页，仍共用保存顺序；编辑已有匹配路径不会改变顺序，新增规则追加在后，若已有整站规则需重新安排后保存。整站代理会先于静态处理，可能覆盖该 Host 的静态挂载。

后台不提供业务账号、Session 权限设置或自定义鉴权配置，因此移除代理认证级别输入框，新规则固定默认 AuthLevel=0。旧版或导入的非零 AuthLevel 保留并显示提示，不会因编辑而自动取消鉴权。非零级别依赖 Service 的已有 Session 权限或自定义认证函数；没有该集成时通常返回 403。管理员后台登录会话不授予业务请求的代理权限。Go API 与旧配置的鉴权能力保持兼容。

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

入口根据实际请求 Host 和 TLS 重建 `Host`、`X-Host`、`X-Scheme`、`X-Forwarded-Host`、`X-Forwarded-Proto`，删除客户端传入的 `Forwarded`。发现节点地址用于连接，上游仍收到原公开 Host，避免应用把内部节点地址误当浏览器访问域名而触发规范域名跳转。HTTP 自动跳转到 HTTPS 仍由 Gateway 执行；应用自身的公开 URL、SSO 回调与同源校验配置需要同步使用实际的 HTTPS 域名。

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

证书名称自动使用证书覆盖的域名，无需填写。上传模式从 SAN 读取域名，不以文件名推断；没有 SAN 的证书不支持。证书与私钥为两个独立上传栏：证书通常使用 `.crt` / `.pem`，内容为 `BEGIN CERTIFICATE`；私钥通常使用 `.key`，内容为 `BEGIN PRIVATE KEY`、`RSA PRIVATE KEY` 或 `EC PRIVATE KEY`。扩展名只作选择提示，实际校验 PEM 内容和证书/私钥配对，上传错栏会明确报错。证书建议包含完整中间证书链，私钥须为未加密 PEM。私钥使用 AES-GCM 加密保存，管理 API 不返回私钥、密文或 PEM。可先上传证书，也可先发布 Host；两者自动衔接。

新 Host 默认 HTTPS.Mode=adaptive，HTTPS.Redirect=auto（off 关闭跳转）。按 SNI / 请求域名精确匹配有效证书；精确匹配失败才尝试单层通配符，例如 *.example.com 覆盖 api.example.com，不覆盖 example.com 或 a.api.example.com。相同匹配级别先选到期更晚的证书，时间相同按 ID 确定顺序。未生效或过期证书不用于 TLS / 跳转。HTTP 回退只保证后续 HTTP 请求可以服务，不能把失败的 HTTPS 握手变成 HTTP，也不会撤销浏览器已缓存的 HSTS。

HTTP 跳转需要配置 TLS 监听；默认从 service.Listen 中的 https 项推导端口，本地 10443 会保留在跳转地址。公网端口映射与本地不同可显式配置 Gateway HTTPSPort（例如 443）。监听配置仍需 YAML；后台不会自动绑定或修改业务端口。

自动证书内置官方 `golang.org/x/crypto/acme/autocert`，无需额外组件。证书页面保存邮箱、ACME 服务地址和条款接受状态；已有 YAML 的 ACMEEmail / ACMEDirectory / ACMEAcceptTerms 作为尚未保存页面设置时的默认值。页面设置持久化优先，不必重启。用户须显式接受所选服务条款，不自动代为接受。

自动条目保存后在后台启动申请；可点击“申请”重试，事件展示签发结果，完成后刷新列表。每个明确域名独立签发，有效证书自动进入匹配库；首次 TLS 握手亦可触发申请，续期由 autocert 管理。首次申请可能等待或失败。后台同时最多 4 个申请任务，同一条目重复请求合并。

HTTP-01 要求域名解析到本网关且**公网 80 端口**能转发到 HTTP 监听（本机可为 1080）。443 是常规公网 HTTPS 端口，并不是 HTTP-01 验证本身的端口要求。验证路径先于 HTTPS 跳转/限流处理。生产默认 Let's Encrypt；测试可选择 staging 或自建 ACME 地址。自动签发暂只支持单节点、明确 DNS 域名，暂不支持通配符、DNS-01 与多节点挑战协调；上传通配证书不受此限制。本轮仅做本地验收，没有公网签发验证。

旧 Host HTTPS.Mode=upload/auto 及 SetCertificate API 保留兼容，但新建 Host 统一推荐 adaptive 和集中证书库。

**证书存储加密密钥默认自动管理，无需手动配置。** TLS 私钥用于 HTTPS 握手，内部存储密钥仅用于加密保存这些私钥，两者不同。单机在 DataDir 下生成并保留 `certificate.key`；Redis 模式原子生成 `Prefix:certificate-key`，各节点自动取得同一份密钥并写入权限 0600 的本地缓存，重启不重新生成。Redis 暂不可用时，已有密钥与状态缓存的节点可按原有机制启动，管理写入仍要求 Redis 可用。

自动管理把 Redis 和 DataDir 作为受信任的运行存储；它避免私钥以明文 PEM 保存，但不能抵御同时获得密钥和证书数据的 Redis 访问者。Redis 访问权限与备份应按运行凭据保护。可选 `GATEWAY_CERTIFICATE_KEY`（64 个十六进制字符，即 32 字节）或 `CertificateKey` 继续支持已有部署及需要把密钥与 Redis 分开管理的环境，环境变量优先。显式模式不把该密钥自动写入 Redis。已有显式密钥部署应保持原配置；自动迁移会验证原加密数据，无法恢复原密钥时拒绝初始化，避免覆盖旧数据。HTTP-01 自动签发仍仅支持单节点。

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
| Manager.SyncAffinity / AffinityStatus | 嵌入式调用方管理保持状态同步；读取就绪、容量及同步统计 |
| AffinityConfig / AffinityRule / AffinityMapping | 四种保持模式、按路径/方法匹配的规则及主备映射 |
| Manager.Reconcile | 从权威存储校对并应用完整配置，失败保留旧请求快照 |
| Manager.CheckHost / TestUpstream | 配置发布校验与有界上游 GET 测试；后者接收 context 和 PreviewInput |
| Manager.RegistryServices / ReadRegistryServices | 只读原生注册中心服务与节点 |
| Manager.MonitorStatuses / MonitorHistory / MonitorMatrix | 配置状态、近期检查状态与小时/日周期汇总 |
| Manager.MonitorTick / RunMonitorCheck | 按周期批量调度或检查一个已发布 Host；嵌入时由调用方管理调度和 context |
| Manager.SendAlert | 使用配置的报警通道发送 AlertEvent |
| PruneGatewayLogs | 按配置的日志文件名及日期格式清理过期文件 |
| Manager.Preview、ValidateHost | 路由与限流适用性预览，以及结构校验；不消耗额度 |
| Manager.Certificates / SaveCertificate / DeleteCertificate | 独立证书库；expected 校验证书 revision，私钥加密且擦除传入切片 |
| Manager.ACMESettings / SetACMESettings / IssueCertificate | 持久化签发设置（expected 为全局 revision）及有界后台申请 |
| Manager.SetCertificate / CertificateCache | 加密保存上传证书和提供 autocert.Cache；SetCertificate 擦除传入私钥切片 |
| SetAdminPassword(config, safeBuf) | 独立设置单管理员密码；不修改 Host 状态，调用者负责关闭 SafeBuf |
| DefaultAdminListen / WriteDefaultAdminConfig | 选择私有地址的 81 端口；创建默认配置且不覆盖已有目标文件 |
| NewLimiter、Limiter.Evaluate / Sweep | 单独使用限流；Evaluate 包含编译成本，实际发布 Host 的热路径使用预编译规则 |

## v1.5.12 依赖基线

本版本对齐 Log v1.5.17 与发布时模块索引中的上游已发布依赖；公开 API 保持兼容。

## v1.5.13 依赖基线

本版本对齐 Log v1.5.17 与发布时模块索引中的上游已发布依赖；公开 API 保持兼容。

## v1.5.15 依赖基线

依赖已发布的 Service v1.5.45 和 Discover v1.5.17；代理保留上游状态码、Location、多条 Set-Cookie 与公开 Host。证书名称从 SAN 自动读取，存储密钥默认自动生成并持久化。正式构建无需本地 replace。

## 配置与上游业务测试（v1.5.17）

编辑 Host 点击测试，默认立即执行配置测试；校验域名、路由、限流、证书和发布冲突，用绿色通过或红色错误显示结论。不保存、不发布、不生成业务请求，也不联系上游。Go API Manager.CheckHost 与 Publish 共用发布校验。

切换“上游业务测试”首次自动发起真实 GET，显示成功（2xx）、跳转（3xx，未跟随）、认证失败（401/403）或其他错误。可调整域名、路径、User Agent、Header、Cookie 后再次测试；域名/路径/User Agent 按 Host 保存到 LocalStorage，Header/Cookie 不持久化。最长 10 秒、展示前 4096 字节，不消耗网关额度；不会自动转发管理员登录凭据。响应详情默认折叠，连通性不替代完整业务认证及限流验收。

Go API：Manager.TestUpstream(ctx, PreviewInput) 返回 UpstreamTestResult。后台 POST /api/check-host 与 /api/upstream-test 使用管理员 Cookie 和 CSRF，程序 API Key 无此权限。原 POST /api/preview 继续支持按请求参数进行规则预演。

发布只有成功后才关闭窗口并提示成功；失败在编辑窗口内显示并保留内容。重复域名显示所属已发布 Host 和 ID，不能同时发布两个相同域名的 Host。

## 服务发现与监控（v1.5.17）

**服务发现**读取 Service 当前注册中心的原生 Redis 注册 Hash 和心跳，展示所有已注册服务（包括未被代理引用的服务）、节点、权重和心跳过期状态，每 30 秒刷新。只读，不手工摘除节点或改权重；原生服务会自行心跳、重注册和执行失败摘除。Gateway 管理存储与注册中心可以不同，以 Service.Register 为准。直接 Go 使用可调用 RegistryServices / ReadRegistryServices。

**Host 列表**点击名称进入编辑；右侧依次展示生效、配置测试、上游测试。配置状态是当前草稿的原生发布校验；上游状态绑定配置指纹，修改配置后旧结果不显示为新配置通过。启用监控时以自动检查结果为准；未启用时可显示本节点最近的手动业务测试。未知状态灰色、失败红色、通过绿色，长期未更新的自动结果显示已过期。

**监控配置**位于 Host 的监控标签页，保存草稿并发布后生效，已发布的代理 Host 默认启用，可显式关闭。默认每 60 秒 GET 上游首页 /，默认 HTTP 2xx/3xx 为可访问（不跟随跳转），连续 3 次失败报警。可设置具体请求域名、路径及查询参数、加密请求 Header、完整直接监控 URL、期望状态、响应需包含的文本、低代码检查。报警通道及适用Host在监控模块集中设置。指定完整 URL 后不走 Host 代理匹配；否则按该 Host 的原生重写和代理规则检查。通配 Host 需提供具体域名或直接 URL。Header 留空保留，{} 清除；URL 与脚本作为配置可导出，认证信息应放加密 Header。

每次请求最多 10 秒，响应检查只读前 4096 字节；不跟随跳转、不消耗网关限流额度。检查是上游采样，不代表公共入口 TLS、网关身份权限或每个副本均正常。低代码检查使用 go/js 匿名函数，运行最多 5 秒，须返回 true；状态、包含文本及脚本条件均须通过。

**监控历史**使用每 Host 单 Hash，日汇总保留 30 天、小时汇总保留 24 小时、内部近期槽保留 10 轮以恢复报警状态。周期详情展示检查次数、平均耗时、失败次数及一份代表性响应：优先保留最近失败，全成功时保留最后成功，正文最多 4096 字节。旧数据缺失项明确标注；周期汇总不等同全时段 SLA。task 每 5 秒调度一批最多 4 个到期 Host，周期是目标频率，慢响应和排队可能延后。单节点避免重叠，Redis 模式以 45 秒租约避免多节点同时检查同一 Host；存储不可用时不凭本地缓存冒充共享监控成功。一个 Host 的历史异常不阻断其他 Host。

**报警通道**在监控模块以独立弹窗维护钉钉、企微、飞书及高级自定义通道。钉钉/飞书可启用原生签名，企微群机器人使用 Webhook 的 key；发送由 go/api 执行，同时校验 HTTP 与平台业务回执。Webhook、签名密钥及监控 Header 复用现有持久加密密钥加密，不回显；备份应保留 state.json / Redis 状态、certificate.key / 共享密钥及 monitor 历史。缺失原密钥时拒绝覆盖生成新密钥。

连续失败达到阈值后通知，恢复再通知，相同已确认状态不重复发送；发送失败在后续检查重试。网络断开、进程退出或存储提交失败时，外部发送与本地记录无法原子提交，不保证恰好一次送达。测试报警按钮会明确确认后发送真实消息。

高级发送函数使用 (event, http)，无需填写标准 Webhook/签名字段，通过 http.request({url, method, headers, payload}) 独立发送，界面可插入钉钉签名示例，可构造 URL、Header、请求体、多个步骤和回执判断；最多 5 次 HTTP 请求，总时间最多 8 秒，至少一次成功请求并返回 {ok:true} 才视为成功。请求和响应各最多 64 KiB。脚本由可信管理员维护，SafeMode 不等于操作系统沙箱。API Key 不可修改监控脚本或通道绑定，改变有加密 Header 的路由时须明确清除/替换凭据。详见 [低代码指引](core/web/lowcode-guide.txt)。

新增 Go API：Manager.RegistryServices、ReadRegistryServices、Manager.MonitorStatuses、Manager.MonitorHistory、Manager.MonitorTick、Manager.RunMonitorCheck、Manager.SendAlert；GatewayApp 自动管理监控调度生命周期，直接嵌入 NewManager 时由调用方调度 MonitorTick 并传播取消。状态模型新增 MonitorConfig、MonitorSample、MonitorDomainResult、MonitorOutput、MonitorBucket、MonitorMatrixRow、HostMonitorStatus、AlertChannel、AlertEvent、RegisteredService、RegistryNode。

机器人协议：[钉钉安全设置](https://open.dingtalk.com/document/robots/customize-robot-security-settings)、[飞书自定义机器人](https://open.feishu.cn/document/client-docs/bot-v3/add-custom-bot)、[企微群机器人](https://developer.work.weixin.qq.com/document/path/91770)。

后台登录使用 Gateway 自有 Cookie/CSRF 会话，非 Service 默认鉴权。会话加密持久化至 DataDir/admin-sessions 或 Redis 的 Prefix:admin-session:*，8 小时有效期内重启无需重新登录。退出删除服务端会话；改密码撤销旧会话。单机过期文件在下一次登录清理，Redis 使用 TTL；备份需保留持久加密密钥。

Gateway 默认作为发现客户端，在 Service 启动后通过 Discover 原生生命周期解除自身注册并保留上游订阅；多地址逐节点展示。注册中心原生 Hash 不包含协议或运行时长，页面「调用协议」展示实际调用配置 HTTP/H2C/HTTPS（未配置默认 H2C），心跳只显示在线/过期。Hosts 展示独立规则列和纯文字状态；新鲜自动监控通过即视为对应已测配置的上游测试通过。草稿修改后不会沿用旧配置的通过结果。

监控和报警共用同一批检查，没有单独报警采样频率；Host检查间隔默认60秒，可按需调整。连续失败从相同检查结果累计，跨小时/天不清零；调整通道、阈值、检查间隔不重置，通道启用或阈值降低后在下一轮检查处理已有故障。更换实际检查目标/判定条件才开启新序列，并清理对应周期的近期点，累计统计仍保留。旧`MonitorConfig.ChannelIDs/FailureThreshold`仅兼容读取，不再决定通道和阈值；通道新增`HostMode/HostIDs/FailureThreshold`。租约争用未产生样本时在下一次调度tick重试，不消耗整个检查间隔。

报警通道可临时停用，签名密钥仅启用签名时显示；适用Host范围（默认all，包括以后新增Host；selected仅含HostIDs，空列表表示不向任何Host发送）及连续失败阈值（默认3）在通道中设置，通道重复提醒为 0 时只通知状态变化，否则按秒数重复未恢复故障。勿扰时段使用显式时区，支持跨午夜，结束后下次检查发送仍待发送的故障/恢复；手动测试消息不受勿扰限制。高级脚本内的凭据随脚本保存并对管理员可见；既有 (event, channel, send) 脚本兼容保留。

监控首页以 Host × 时间矩阵展示所有 Host，可切换最近 24 小时 / 最近 30 天。每格汇总该小时/天的检查，以最近一次结果决定颜色：最近成功显示绿色平均 ms，最近失败显示红色成功次数/总次数，无记录或旧周期最近状态未知为灰色；数字下方5个点展示该周期最近5轮（左旧右新，绿成功/红失败，缺失不画点），保持原格子高度。延迟取每轮最慢域名耗时后计算时段平均。日统计保留 30 天、小时统计保留 24 小时，每个周期保留一份代表性响应；点击时间格查看周期汇总。

缺省监控设置现在对已发布、启用的代理 Host 自动生效：启动立即触发首批检查，每 5 秒调度、最多 4 个 Host 并发，默认每 60 秒检查。`Monitor.Configured: true` 表示显式选择，此时 `Enabled: false` 停止监控；旧配置中缺少 Configured 的默认关闭值升级为默认启用。仅静态/重定向且未指定监控 URL 的 Host 不自动探测不存在的代理。未指定 URL/Domain 时所有域名逐一检查（每 Host 最多 4 个并发）；任一域名失败则整轮失败，详情保留逐域名状态。指定 Domain 或独立 URL 时按该目标检查。通配域名须指定具体检查域名或 URL。

默认监控不再将登录跳转 302 等 3xx 判为失败：`ExpectedStatus: 0` 允许 2xx/3xx；指定非零状态则精确匹配，同时仍执行内容/脚本检查。失败保存结构化分类：未注册、离线、注册中心不可用、超时、DNS/TLS、连接失败、状态/内容/脚本错误；界面直接显示分类。矩阵最近成功显示平均 ms，最近失败显示成功次数/总次数，无记录显示 —；悬浮提示保留周期成功/总次数及失败原因，旧失败次数不全时成功数标为上界（≤）。历史保留当时检查结果，规则变更后的新检查不改写旧统计。

检查默认每 60 秒；页面 30 秒刷新、调度每 5 秒检查是否到期都不等于每 30/5 秒请求上游。一个单域名 Host 每天约 1440 次检查，7 个约 10080 次。监控详情在 Redis 使用 Hash 的近期槽（10 个近期槽、停写后 30 天 TTL），小时统计使用 Redis Hash 按小时增量更新（30 个日桶 + 24 个小时桶、30 天 TTL），每次仅更新当天、当前小时及一个近期槽，避免反复重写整月数据及放大 Redis 持久化日志；单机使用同样结构，文件读取上限 4 MiB。单个近期槽上限 40 KiB，正常单域名记录远小于此。保存状态、分类、耗时、通知结果及有界的代表性响应。探测专用日志器设为 CLOSE，避免反复写普通错误堆栈；上游自己的访问日志仍由各应用日志策略管理。

普通 Gateway 日志按配置的 SplitTag 分文件。新增 `LogRetentionDays`（默认 30，1–3650）：启动和每 24 小时只清理当前应用配置的日志基名、日期格式所匹配的到期文件，保留正在写的文件、其他应用日志及备份；不切割的单文件、远程日志输出不做清理。Docker 标准输出另按部署日志轮转策略保留。`PruneGatewayLogs` 可用于独立验证此清理边界。

## 当前监控存储布局

每个 Host 使用一个 Redis Hash：`<Prefix>:monitor:<HostID>`，例如 `gateway:monitor:abc123`。

| Field | 保留数量 | Value |
| --- | ---: | --- |
| `d:20261015` | 最近 30 天 | `{total,times,error,errors,legacyErrors,output,recent,fingerprint}` |
| `h:2026101510` | 最近 24 小时 | `{total,times,error,errors,legacyErrors,output,recent,fingerprint}` |
| `r:00` … `r:09` | 最近 10 轮 | 最近检查及连续失败数、告警/恢复发送状态 |

`total` 是累计耗时毫秒，`times` 是检查轮数，平均延迟为 `total / times`。`error` 为空表示时段全成功；只要出现失败即保留最近失败原因，不被后续成功清空。`recent`仅保存该小时/天最近5轮的布尔结果，左旧右新，缺失记录留空，界面状态取最后一轮；失败原因/次数和代表性失败响应继续保留，恢复后不再将整个格子判红。旧周期不伪造逐次结果，升级后随新检查逐步填充。日期按服务器时区（开发环境 Asia/Shanghai）划分。每个 Hash 最多 64 个 field，无元数据额外 field；Hash 停止更新 30 天后整体过期。跨节点租约是独立短期锁，不计入统计 field。

每分钟检查一次，近期槽不额外产生半分钟请求。连续失败数保存在最新检查，超过 10 次的阈值仍正确；告警去重、重复提醒、恢复、勿扰状态保留并支持重启。Lua 原子更新当日、当前小时与近期槽并删除过期桶；正常写入只发送当前样本，不发送整个月数据。单机模式使用同样 64 槽结构的文件。

旧 720 小时数据迁移时折算 30 个日桶，只留最近 24 个小时桶与最近 10 条检查；成功提交后清理旧 List/统计键或文件。旧历史失败仍保留其当时判定。每周期仅保留汇总和一份代表性响应，旧周期未保存的正文和完整失败计数明确标注。

Hosts 与监控统一按添加时间升序；旧记录从保留的发布历史和管理日志推导，缺失时以 ID 稳定排序。

- 永久 API Key 同时识别标准零时刻和旧快照的无时区零时间；UTC、Asia/Shanghai、America/New_York 下持久化和重启后仍有效，有限到期时间保持校验。

## HTTP/2 WebSocket（v1.5.20）

Gateway 代理入口支持 RFC 8441；Go/x/net 的服务端声明仍需启动环境 `GODEBUG=http2xconnect=1`。上游默认自动选择：WSS 优先 H2，ALPN/SETTINGS 不支持时兼容 H1；普通 WS 直接 H1，显式注入 NewClientH2C 可使用 H2C。认证拒绝、证书错误、超时等不触发协议降级。不再增加 WebSocketProxy.HTTP2 配置。Service 默认代理客户端共享连接池与有界能力缓存，Cookie Jar 关闭；Stop 清理其空闲池。显式 SetProxyWebSocketDialer 保持 H1 网络策略。

依赖已发布 HTTP v1.5.14 与 Service v1.5.48，独立发布验证使用 GOWORK=off。TLS 对照方法与原始数据见 http/testdata/websocket/20261022-tls-compare.md。

## External addressing / 外部地址（v1.5.22）

Business ingress `Config.ExternalAddress` and management `Config.AdminExternalAddress` are independent HTTP `ExternalAddressPolicy` values. Default behavior (v1.5.22): business ingress uses direct mode and regenerates address headers, including requests with client-provided IDs. The independent AdminHandler defaults to the existing internal Request-ID relay convention, so Gateway HTTPS → HTTP admin self-proxy works with no configuration. Optional CIDR policies can strengthen either entry. Preserve the public request Host for business routing. Each trusted relay must overwrite single-value paired headers rather than append lists.

```yaml
ExternalAddress:
  TrustedProxies: ["192.0.2.10/32"]
AdminExternalAddress:
  TrustedProxies: ["192.0.2.20/32"]
```

The normal Gateway self-proxy needs no management trust configuration. For optional explicit source restrictions choose the actual proxy source, e.g. `127.0.0.1/32` or `::1/128`. The proxy must overwrite X-Forwarded-Host and X-Forwarded-Proto from its trusted ingress state (or use X-Host/X-Scheme). Both groups, when present, must agree. Forwarded alone is unsupported. Invalid trusted metadata returns 400; untrusted forwarding headers cannot alter security decisions.

The independent AdminHandler pins the HTTP result before dispatch. Login and subsequent Cookie-authenticated writes compare Origin structurally with this result; Cookie Secure uses its external HTTPS flag. Origin is never rewritten or invented, no X-Origin or domain bypass is introduced, CSRF tokens remain required on writes. Missing Origin retains existing non-browser compatibility; empty/null, lists, duplicates, credentials and URL paths/queries/fragments are rejected. Actual request TLS is never fabricated. API-key and affinity RPC authentication remain independent of browser addressing.

Gateway self-proxy TLS termination works by default; a separate third-party frontend can use the existing internal relay contract or optionally configure its source. A direct HTTP IP:port management connection remains supported. Recreate Manager to change its management policy; business policy is restored on GatewayApp.Reload from the manager configuration.

Release dependencies: HTTP v1.5.16, Discover v1.5.19 and Service v1.5.50; GOWORK=off without local replacements.

## 默认可用 / Zero-configuration default (v1.5.22)

Controlled Gateway → Service/Discover chains preserve the first external address by the existing Request-ID convention; no per-service trusted-source configuration is required. The public Gateway business boundary defaults to direct and overwrites client address headers regardless of client ID. Its own proxy creates/relays Request-ID, and the independent management handler consumes that context by default for same-origin checks and Secure Cookie. CSRF, Origin preservation and RPC authentication remain intact.

Optional `ExternalAddress.Mode` and `AdminExternalAddress.Mode`: legacy (internal default), direct (always use connection), trusted (require explicit direct-peer addresses). An address list with omitted Mode retains the released explicit-policy behavior. Settings can be left entirely absent for the normal self-proxy topology. Released dependencies need no local replace.

## AES-GCM 配置密文兼容

正式依赖选用 crypto v1.5.9，共享 `Symmetric.Decrypt/DecryptBytes` 先认证 `[12 字节 IV][密文][16 字节 tag]`，再使用配置 IV 兼容旧格式；现有 API 不变。新数据推荐 `EncryptBytesAutoIV`，先升级接收端再启用新写入。此能力针对 AES-GCM，不改变 CBC/SM4 格式。

## v1.5.24

已升级服务发现依赖至 Discover v1.5.21、Service v1.5.52、Redis v1.5.22，以获得 Redis 长时间故障后的异步恢复。

### 日志

Gateway 的代理默认采用精简 ProxyLog，避免重复 RequestLog；静态文件请求保留独立 RequestLog。未进入代理的入口拒绝和未命中仍保留请求结果。代理失败补充有界脱敏诊断，200 后流异常也会记录，TraceID 沿用入口 RequestID。`gateway log` 默认 -tail 100 -f；支持 -cat/-head/-tail、-f、-json/-raw 和显式文件。logv 支持 stdin 和 .gz+.dict 文件。默认实时字典压缩；日志保留策略成对清理 .gz/.dict，保留当前文件和无关备份。查看日志不启动 Gateway 服务。

### 实时压缩日志（v1.5.28）

正式依赖已按顺序发布，go.mod 不保留本地开发 replace。文件日志默认采用 `.gz` + 同文件级 `.dict`，两份文件须一起管理；通过 logv 或内置 log/logv 读取，NoCompress=true 可保留明文。


### Release v1.5.28 — 2026-11-12

Released GitHub dependencies; no local development replacements. Full tests and benchmarks validated with GOWORK=off.
