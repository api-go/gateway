# Gateway 验证报告

## v1.5.25 发布验证（2026-11-02）

- 依赖审计：全部 apigo 直接及间接依赖与 .profile 正式版本一致，无 replace；GOWORK=off。dev1 使用 Go 1.26.8、3 CPU / 3 GiB，以及独立无外网 Redis 7.4.2 网络空间。
- `go mod verify`、`go mod tidy -diff`、`go test -v ./...`（core 12.021s）、`go test -bench=. -benchmem ./...`（core 29.858s）、`go vet ./...` 和 Linux 构建通过。ACL/回收站专项 race 11.881s；增加回收站快照恢复覆盖后的专项 race 4.536s。大规模 affinity 可靠性及 Redis 进程中断项目保持其 opt-in，本轮未启用。
- 回收站覆盖：删除后停止路由、完整草稿/发布/历史保留、恢复仅重新启用原发布内容、纯草稿不自动发布、修订号和版本递增、重启、域名冲突、ID 防覆盖、过期修订拒绝、永久删除、脱敏/加密凭据与错误密钥拒绝、CSRF、API Key 按 Host 授权及恢复所需 publish 权限、Redis 双节点同步/CAS 单次恢复、非空回收站快照恢复、永久删除后快照不复活。
- `testdata/browser/acceptance.cjs` 在隔离真实后端完成全流程，覆盖四类路由、限流、证书、回滚、并发编辑、回收站操作取消/恢复/永久删除与实际流量切换、API Key、会话及窄屏，无页面错误。线上 HTTPS 资源另用浏览器隔离 API 夹具验证规则弹窗和窄屏回收站，未改动共享 Host。
- `tests/acl-ui.cjs`、全部修改的 JS 语法及 git diff --check 通过。仓库无 ai/tools/getExports，改用 `go doc -all ./core` 与导出源码交叉核对；中英文 README/API 指引已同步 Trash/Restore/Purge、DeletedHost 与 ACL。
- 基准：32 条回收站记录脱敏列表 335284ns/op、137126B/op、2724allocs/op；这是管理操作，不在转发请求路径。ACL 目录范围 49.23ns/op、16B/1alloc，加脚本 2111ns/op、2989B/44alloc。ACL 为并行摊销值，不代表单请求延迟或生产吞吐。会话保持代理基准均校验上游 204 与有效发现租约。
- dev1 运行 1.5.25，配置及 Host 状态哈希不变；管理首页 200，原 ACL 演示 403，QBank/LLM/Knowbase/Teaching 健康 200。部署和原始验证证据见 [发布摘要](testdata/acl/gateway-v1.5.25-20261102.txt)。

## 规则弹窗交互开发验证（未发布，2026-11-02）

- ACL、代理、重写、重定向、静态文件、限流、会话保持采用摘要列表和独立规则弹窗；取消、关闭及 Esc 不修改原规则。规则删除独立确认，Host 删除仅在配置标签页显示“删除整个 Host”。
- 新建 ACL 无 admin 快捷入口及路径预设；空匹配值阻止确认，整站保护通过显式移除范围条件表达。批量 IP 在结构编辑期间保留，确认时写入规则，不保存临时编辑字段。
- dev1 固定 Go 1.26.8、3 CPU / 3 GiB：`go mod verify`、`go test -v ./...`（core 7.203s）、`go vet ./...` 和 Linux 构建通过。纯前端交互修改，未重复既有 ACL 性能基准及 race 验证。
- Node 24.21.0 的 `tests/acl-ui.cjs` 通过；真实 HTTPS 域名加载线上资源的 `tests/rule-editor-browser.cjs` 通过，覆盖添加/编辑/取消、批量 IP、动态条件、删除取消/确认、七类规则保存回显、390px 布局及弹窗底部可达，无页面脚本错误。API 在浏览器内使用隔离夹具拦截，未写入共享 Host/Redis。
- 已部署 dev1，运行配置和 `data/gateway/state.json` 哈希保持一致；线上四份 JS/CSS 资源逐字节匹配。管理入口 200，既有 ACL 演示 403，QBank、LLM、Knowbase、Teaching 健康检查 200。
- [部署与验证摘要](testdata/acl/gateway-acl-dialog-dev1-20261102.txt)。无提交、Tag 或模块发布；Gateway 保持纯 L7。

## ACL 范围/放行简化验收（2026-11-02）

- 新模型覆盖：Scope 内默认拒绝、Allow OR、grant 条件 AND、重叠范围 AND、空范围/空放行、字符串目录前缀边界、IPv6 等值归一化、脚本错误与其他放行项、取消、原配置不可变、自动稳定 ID、无名称保存、API Key 声明式修改与脚本编辑边界、观察模式与限流、旧开发配置兼容。
- dev1 固定 Go 1.26.8、3 CPU / 3 GiB、独立 Redis 7.4.2 容器：全量 Redis 集成测试 11.541s、全模块基准 29.073s、ACL race 8.485s、go mod verify、go vet 均通过。专项 Node 24.21.0 表单测试验证 IP 批量输入、自动摘要、名称/排序入口移除、纯字符串 admin 模板和旧两条规则等价转换；node --check 与 git diff --check 通过。
- 3 并行执行者基准：新目录范围 + CIDR 放行 48.52ns/op，16B/1alloc；脚本放行 1985ns/op，2987B/44alloc。为并行总耗时摊销值，不代表单请求延迟或完整网关吞吐。
- 已更新 dev-gateway 且健康，替换二进制时 env.yml/state.json 哈希一致。备份 `/app/gateway/backup/gateway-acl-scope-20261102T043857Z`。演示 Host 由 version=2 的旧两条规则迁移到 version=3/revision=6 的单个 Scope/Allow，在线节点确认应用；其他 Host 记录不变。当前二进制 SHA256 为 `e1cc151d4d0fdad43f3b0317739b61efd6913f599ecd54e29146225d1e7b5da9`。
- 真实 HTTPS 演示：回环来源 200、外部来源保护路径及子路径 403、相似名称非保护路径返回上游 404；管理首页及 QBank/LLM/Knowbase/Teaching 健康均 200。线上 acl.js/app.js 与本地一致。临时源码、探测程序和测试 Redis 已清理。没有加入四层运行模式或 LVS 系统管理代码。
- [验证摘要](testdata/acl/gateway-acl-scope-dev1-20261102.txt)。未提交/Tag；如回退到首个不识别 Scope/Allow 的开发二进制，先用当前管理端把演示 Host 回滚到旧 version=2，或配套恢复配置备份。

## ACL dev1 开发部署验收（2026-11-02）

- 按 DL SOP 在 dev1 固定 Go 1.26.8 镜像构建；独立 Redis 7.4.2 容器/网络完成全量集成测试（11.438s）、全模块基准（25.690s）、ACL race（5.832s）、go mod verify 与 go vet，均通过。临时测试 Redis 已清理。
- 已部署现有 dev-gateway（健康）；替换二进制时 env.yml 与 Host 状态哈希均保持一致，证书/密钥保留。备份 `/app/gateway/backup/gateway-acl-20261102T040620Z`，二进制 SHA256 为 `66999aef37e1e287363c4da1e4a365b6f1eda27524fd45278ea29434263114a6`。本次无提交/Tag，CLI 既有硬编码版本仍为 1.5.19，以构建哈希和清单识别开发版本。
- 人工验收演示：管理 Host gateway-dev.deeler.cn 新增路径 `/__acl_demo__/admin` 的回环 IP allow + 其他 IP deny，并将该精确路径重写到管理首页；该 Host version=2/revision=4，在线节点确认应用。其余 Host 配置保持一致（仅序列化新增空 ACL 字段）。
- 真实 TLS 验证：回环来源演示 200，外部来源演示 403；管理首页及 QBank、LLM、Knowbase、Teaching 健康均 200。线上 acl.js/app.js 与本地源码逐字节一致；浏览器真实 HTTPS 入口显示 access denied。
- [完整部署摘要](testdata/acl/gateway-acl-dev1-20261102.txt)；dev1 测试/构建日志和源文件哈希清单位于 `/app/gateway/.upstream/acl-dev-20261102/`，临时源码与探测程序已清理。LVS 未实现或部署。

## ACL 开发验证（未发布，2026-11-02）

- 依赖审计：Gateway 直接/间接 apigo 依赖与 .profile 已发布版本一致；GOWORK=off，无 replace，无新增依赖。
- 覆盖有序 allow/deny、观察并继续、未匹配默认允许、IPv4/IPv6/CIDR、路径边界/编码、方法/Header/Cookie/Session、既有可信 X-Real-IP 解析结果、拒绝不访问上游或消耗限流额度、允许仍执行限流。
- 覆盖脚本 true/false、异常、非法返回、超时、请求取消、并发、请求体保留、编译配置与调用方修改隔离、无效规则/重复 ID、发布/回滚/重启、API Key 脚本编辑边界和 Redis 双节点同步/移除。
- 覆盖公开请求预览 ACL 判定，以及配置检查、监控路由解析、直接上游测试保留独立行为。Redis 测试夹具采用原子节点/心跳登记及实际快照就绪判定，符合已发布 Discover 的异步生命周期。
- 浏览器实测：独立 ACL 标签页、白名单模板、网段编辑、草稿保存/关闭/重新打开回显；布局可用，控制台无错误。临时后台已清理。
- API 检查使用 `GOWORK=off go doc -all ./core` 与导出源码交叉核对（仓库未提供 ai/tools/getExports）；中英文 README 已补齐 ACL、NewACL、ACL.Evaluate、ACLRule、ACLDecision。
- 最终验证：`GATEWAY_REDIS_INTEGRATION=1 GOWORK=off go test -v ./...` 通过（core 15.051s）；同环境 ACL 专项 race 通过（6.224s）；`GOWORK=off go test -bench=. ./...` 通过（17.910s）；go vet、两份 JS 的 node --check、git diff --check 通过。独立进程/大规模 affinity 可靠性和 Redis 进程中断项目保持其显式 opt-in，未启用。
- 16 并行执行者的 ACL 基准：无规则 0.3953ns/op、0B/0alloc；路径正则 + IPv4 CIDR 18.81ns/op、16B/1alloc；同条件加 JS 1340ns/op、3026B/43alloc。均为并行总耗时摊销值，非单请求延迟或生产网关吞吐；不含 HTTP、上游、日志及 Redis。脚本基准采用 100ms 执行预算以测量正常执行开销；超时/取消行为由独立测试验证，默认预算仍为 10ms。
- 结果摘录：[ACL 验证](testdata/acl/gateway-acl-20261102.txt)。

## v1.5.19 发布验证（2026-10-22）

- 依赖审计：全部apigo直接/间接依赖与公开已发布索引一致，GOWORK=off，无replace；tidy-diff无变化。
- dev1固定Go 1.26.8、3 CPU/3 GiB、独立无外网Redis容器：启用Redis集成及GATEWAY_AFFINITY_RELIABILITY=1，全量116.885s通过，包含四模式48万次请求、渐进集群212,000次请求、恢复/快照/连接/监控报警场景。3→4一次迁移1024/4096分片。独立Redis进程中断项未启用；其余race证据见下方已完成记录。
- 基准9.805s通过；完整代理hash203291、auto199480、gradual175914 ns/op，包含本机上游和日志，不代表生产吞吐或模式性能排名。APIKeyRead25536、LimiterIPWindow2029、PublishedHostQuota7666、MonitorCheck2194921 ns/op。
- go vet、node --check及git diff --check通过。ai/tools/getExports不可用，以GOWORK=off go doc -all ./core与导出源码交叉核对；中英文API表补齐AffinityConfig/Rule/Mapping及Manager.SyncAffinity/AffinityStatus，AlertChannel范围/阈值与MonitorBucket.Recent已说明。
- 开发部署成功，运行命令确认gateway version 1.5.19；dev-gateway健康。qbank/llm/knowbase健康、Git入口、Teaching健康及首页均200；运行env.yml及完整state.json哈希保持不变。测试Redis已清理。
- 结果摘录：[最终全量及基准](testdata/reliability/20261022-release-v1.5.19.txt)。dev1原始日志为/tmp/go-gateway-lvs-20261022/release-v1.5.19-{full,bench}.log，导出文档为同目录release-v1.5.19-api.txt。

以下为开发过程各轮验证记录；其中旧版本号、未提交/待验收文字表示当时状态，当前发布以本节为准。

## 通道集中配置及报警/监控同源（2026-10-22）

- AlertChannel新增HostMode（默认all）、HostIDs、FailureThreshold（默认3）；selected空列表不发送，all包括后续新增Host。Host页面移除报警绑定与阈值，旧MonitorConfig对应字段只兼容读取、不再决定通知。旧通道不改revision/密钥即可默认全部Host。
- 监控和通知使用相同MonitorSample及连续失败数，跨小时/日继续累计；绑定、阈值、间隔变更不影响实际探测摘要，升级识别旧摘要以保留失败数/通知状态。实际检查目标/条件变化才重置序列，小时/日recent同步重置，累计检查/失败统计保留。勿扰及重复提醒只影响发送；已有故障在通道启用后下一轮检查处理。
- Redis租约争用回归：占用租约时不产生样本；释放后立即执行第二次tick，验证可检查且计数准确。
- 缺失的近期结果不绘制占位点；实际1次就画1点、没有记录就空白，保留32px格子高度。周期详情显示当前连续失败次数与可用的发送错误。
- 新增本地通道默认全部/后续新增Host、指定范围/空范围、开关通道继承失败数、策略变更/探测变更区别、旧摘要升级测试；独立Redis两个Manager轮流检查，第三个共享失败样本发送一次，第四次不重复，矩阵点和失败数一致。机器人均为隔离本地接收端。
- 最终监控/报警相关race27.888s、全量普通9.519s、go vet、tidy-diff及JS/diff检查通过；未重复启用长时四模式/渐进可靠性和Redis进程中断，结果见此前各节。

- `./dl pub`成功，dev-gateway健康；部署前后env.yml及完整state.json SHA256一致，9个Host与报警配置未写入。qbank/llm/knowbase健康、git.deeler.cn入口、Teaching健康/首页均200。独立测试Redis已stop+rm，HEAD仍442c7c6/v1.5.18，无提交、推送或Tag。
- 浏览器验证通道默认全部/3次、指定Host保存请求、新通道默认值、Host移除绑定但保留检查间隔；保存请求在浏览器拦截，未向服务端写配置或发送人工消息。小时/日五种状态及390px窄屏通过，无JS错误；缺失点隐藏、实际格子32px。
- 部署后bak最新检查成功，连续失败0，恢复通知记录为17:40:39且无AlertError。保留用户现有通道revision2、重复提醒300秒、上海时区19:00–09:00勿扰配置；勿扰时第三次失败仍累计但不立即发送。

证据：[最终race/全量摘录](testdata/reliability/20261022-alert-scope-final.txt)，dev1原始日志 `/tmp/go-gateway-lvs-20261022/alert-scope-final.log`。bak只读诊断 `/tmp/go-gateway-alert-before.jsonl` 不含通道凭据。

## 监控最近状态及5轮走势（2026-10-22，开发部署待验收）

- 小时/日桶新增最多5个布尔结果，左旧右新；Redis Lua在原有单次原子写入内更新，本地文件同步更新。Hash仍最多64个field，原计数、失败原因和代表性失败响应保留。
- 矩阵按周期最后一次结果着色：最新成功显示绿色平均ms，最新失败显示红色成功/总次数；下方5点展示最近结果。旧混合周期若缺少最后状态则显示灰色，缺少的逐次记录在当时用灰点（现已由上节改为空白），不伪造历史。失败次数不全的旧数据仍标记成功数上界。
- `TestMonitorPeriodRecentResults`分别验证本地/Redis的7轮交替成功与失败、5轮截断、每步小时/日计数、恢复后失败响应保留、重启持久化。监控/报警/迁移/租约相关race18.276s通过；全量普通回归21.499s、vet及tidy-diff通过。长时四模式/渐进可靠性本次未重复启用，结果见上一节；独立Redis进程中断本次未启用。
- 部署后真实管理API已有新recent数据；浏览器在真实管理页注入只读响应样例，验证恢复成功、最新失败、全成功、旧记录未知及仅一次检查五种情况，小时/日一致、详情仍保留失败响应。桌面及390px宽度通过，无JS异常；实测格子32px，原32.5px，无额外高度。样例截图为浏览器内模拟状态，未写业务监控历史。
- `./dl pub`完成，dev-gateway健康；部署前后env.yml及完整state.json SHA256相同，本轮看到的9个现有Host未改。qbank/llm/knowbase健康、Git入口及Teaching健康/首页均200。隔离测试Redis已清理，仍未提交、推送、递增版本或Tag。

证据：[监控race](testdata/reliability/20261022-monitor-recent-race.txt)、[本轮全量](testdata/reliability/20261022-monitor-recent-full.txt)。dev1原始日志 `/tmp/go-gateway-lvs-20261022/monitor-recent-*.log`；本机浏览器/部署日志 `/tmp/go-gateway-monitor-{browser,deploy}.log`，截图 `/tmp/go-gateway-monitor-recent-{desktop,mobile,example}.png`。

## 一次低负载分配及连接语义（2026-10-22，工作区待验收）

本轮移除迁移间隔及分批限额。dev1 固定 Go 1.26.8、GOWORK=off，独立 Redis 7.4.2、独立网络空间，测试容器 3 CPU / 3 GiB；未向业务 Redis 写测试数据。

- 定向 race：175.284s通过，涵盖真实多进程渐进分片、连接语义、跨管理节点登录会话、20,000条自动保持快照 / 1,000条增量 / 同ID日志重建及双表快照容量。
- 渐进集群每轮212,000次HTTP请求，100,000个标识覆盖4096/4096分片；3→4后端一次迁移1024分片，表不再处于待平衡状态，拓扑变化到全部Gateway表对齐实测1.865734209s。10,000个旧标识中1071个迁移，100个热点标识不动；10,000个新标识中2468个落新节点。全部分片有历史流量，仍会移动一部分旧标识；这些比例是本轮观测，不是业务保证。
- 单张Redis分片表10,408字节，不含journal、负载报告和本地对象。Gateway逐会话entries为0；丢通知补偿、100次旧表通知拒绝、强杀主节点自动接任、调度暂停时备用与后续持久化修复一致、仅改显示名称保留原表均通过。
- 真实TLS HTTP/2：同一客户端连接上的200个后续请求在手动映射修改后使用新上游；已建立WS的201次echo及在途SSE尾部仍由旧上游提供。该测试用确定性手动映射验证连接语义，不冒充真实LVS或渐进分片的端到端长连接验证。
- 同一管理员Cookie/CSRF可在两个Manager读取会话，密码重置撤销继续通过。
- 保留移除参数前的规范化摘要中的两个零字段，避免已有保持映射/分片scope因字段删除失去寻址；业务配置及UI不再提供这些参数。所有实例仍须统一升级配置/同步协议。
- 最终源码全量 `go test -count=1 -v ./...` 117.777s通过，含上述旧摘要兼容断言、四模式24万次随机Gateway请求和四模式24万次可靠性请求；渐进集群再次一次迁移1024分片，全部节点对齐1.514821931s，旧标识迁移1440/10,000、热点不动，新标识2500/10,000到新节点。此前race的1071/2468为独立一轮，不混作同一观测。
- `TestAffinityLVSWorker`是只由子进程激活的入口，顶层跳过，实际子进程由集群测试覆盖；`TestRedisOutageRetainsTrafficAndRecovers`本轮未启用，不计通过。跨物理机分区、真实内核LVS及小时级持续压力仍未覆盖。
- 摘要兼容额外定向race及 `go test -run '^$' -bench=. ./...` 通过；3 CPU同机完整代理基准hash181925 / auto181466 / gradual185975 ns/op（含本机上游及日志，不作为生产吞吐承诺）。go vet、go mod tidy -diff、JS语法及diff检查通过。

开发部署与浏览器复核：`./dl pub`成功，dev-gateway健康；四模式说明/灯泡、去掉旧预算输入、手动映射及多规则、未保存退出通过，8个Host无写入、无JS异常。部署前后env.yml及完整state.json的SHA256一致。qbank/llm/knowbase健康及Git入口均200，Teaching健康200、首页401鉴权响应。测试Redis已清理；HEAD仍442c7c6 / v1.5.18，无提交、推送或Tag。

证据：[一次分配race](testdata/reliability/20261022-once-race.txt)、[最终全量](testdata/reliability/20261022-once-full.txt)、[摘要兼容race与基准](testdata/reliability/20261022-once-compat-bench.txt)。原始日志在dev1 `/tmp/go-gateway-lvs-20261022/finish-*.log`。

## 历史验证：30秒 / 41分片限速构建（2026-10-22，已被一次分配替代）

以下迁移数量、预算参数和界面观测仅证明旧构建，不作为当前一次分配算法的验证结果。

- 用相互独立的 OS 进程启动真实 GatewayApp（各自 Discover、订阅、后台同步、数据目录与管理端口），共享独立测试 Redis；调用端每次随机选择一个 Gateway，64 并发、同一 HTTP Client 复用，无 CookieJar、不传分片版本。模拟 LVS 分发行为，未实际安装 LVS。
- 四模式每轮 24 万次真实 HTTP 请求：3 Gateway 首次/复访、增加第4个 Gateway、后端3→4、移除后端、强制终止一个 Gateway。两轮定向 race + 引入分片后的定向 race + 全量回归累计 96 万次 LVS 请求通过。检查每阶段各 Gateway 的命中数，确保全部入口都实际参与；auto 在跨入口复访、Gateway 加入/故障和后端扩容时零漂移。
- 最终渐进分片测试每轮 **212,000 次请求**，普通构建及 race 各一轮通过（66.761s / 107.058s）。先发送10万个标识，确认 **4096/4096 分片均有历史流量**，再对100个热点标识发送32,000次请求。默认首次扩容只迁移41个低负载分片；复访1万个旧标识时仅18/14个迁移，100个热点标识均未漂移；1万个全新标识中100/97个由新增节点承接。这是本轮分布的观测，不保证所有业务分布下相同比例。
- 单张实际 Redis 分片表 MEMORY USAGE 为 **10,383 字节**（4个实际地址、4096个owner索引和元数据），各 Gateway 的逐会话映射 entries 为0。此数不包含 journal、短期负载报告、Gateway本地计数器/解码对象，不能当作整个集群的总内存。
- 第4个 Gateway 经真实启动/加密快照加入；关闭一个从节点的 affinity 订阅后仍从后台补齐共享表。持续注入100次旧表通知时，新标识复访未回退。调度主 `gw-0` 被强制终止后自动选出新主，已有路由保持稳定；通过固定租约占用15秒暂停调度，故障备用转发正常，租约恢复后持久化重分配与即时备用归属完全一致。只改迁移预算也不创建新表或漂移旧归属。
- 边界：非法策略/周期/比例拒绝；65张活动表在发布前拒绝；无注册中心拒绝启用渐进策略；草稿允许保存但无效策略不能发布。分片表事件按版本覆盖，会话映射仍保留首条，不混用更新语义；快照额外按编码字节数限页。
- 引入分片后的全量可靠性回归127.057s通过；含随机多Gateway、分片、快照、冲突和生命周期的race239.979s通过；随后对最终分片/备用/预算变更的212,000次race单独复测通过。最终短规模全量及 go vet 再次通过，go mod tidy -diff通过；Rand v1.5.4仅由间接改为直接测试依赖，没有升级依赖版本。
- 同机3 CPU限额下完整代理基准：hash245650 ns/op，auto235270，gradual240248；含本机HTTP上游与日志，非纯路由成本，也不作为生产吞吐承诺。
- 故障测试定位并补齐了“丢订阅的从节点接任主后缺少其他节点负载”问题：每份负载报告按Gateway/固定分片保留10秒，订阅为主、后台缺失补取，不增加逐请求Redis操作。失败证据保留在 dev1 `/tmp/go-gateway-lvs-20261022/shards-load-gap-failure.log`。另按实际草稿/发布契约修正了测试断言，保留 `shard-coverage.log`；未把失败运行计作通过。
- 界面每模式一句话提示，灯泡展开适用场景/收益/代价；hash可选纯哈希或渐进迁移。浏览器验证四种提示、灯泡原生展开、渐进参数采集、切换模式清理无效策略字段，未保存退出后8个Host配置不变，无页面JS错误。

收尾同步协议修复及最终验证：

- 删除并重建 Stream 时即使复用同一 ID，也以游标对应内容的 SHA256 指纹识别代际变化；ID/指纹作为同一个原子快照发布，节点分页快照及本地检查点同时携带它们。验证失败时获取节点快照并从有效日志重放，避免漏记录或反复停在无效游标。该校验仅在后台同步，客户端无 Cookie/版本要求。
- 分片表事件不占逐会话映射配额；新增 MaxAffinityEntries=1、两张分片表的新节点快照测试。最终全量回归8.365s及10轮游标/双表容量race14.133s通过，go vet通过。
- 最终协议版本再次执行100,000标识/全部4096分片/212,000请求的完整渐进调度race，并结合20,000条自动保持快照、1000条补偿和同ID日志重建、双表容量验证，145.475s全部通过。该轮首次迁移41分片，复访10,000旧标识移动20个、热点不动，10,000新标识中110个落到新节点。此前18/14个的观测仍保留，说明比例随实际负载及地址哈希变化。
- 开发环境通过真实浏览器验证四种说明、灯泡展开/收起、渐进参数、手动模式切换及未保存退出，无JS异常；部署前后env.yml和完整state.json的SHA256均一致，8个Host无写入。qbank/llm/knowbase/git入口200；Teaching入口与直连上游同为401鉴权响应。正式版本仍1.5.18，无提交、推送或Tag。

结果摘录：[随机Gateway race](testdata/reliability/20261022-lvs-race.txt)、[满分片普通构建](testdata/reliability/20261022-shards.txt)、[满分片race](testdata/reliability/20261022-shards-race.txt)、[最终协议race](testdata/reliability/20261022-final-protocol-race.txt)、[游标重复10轮](testdata/reliability/20261022-cursor-regression.txt)。另附[低负载分片分析脚本](testdata/reliability/shard-expansion.py)及[模拟结果](testdata/reliability/20261022-shard-experiment.json)；模拟不等于生产实测。尚未覆盖跨物理机网络分区、真实LVS内核分发或小时级持续压力，不宣称零旧会话漂移或瞬时强一致。

## 四种保持模式批量可靠性验证（2026-10-22，开发服务器，无发布）

在 dev1 的独立 Docker 网络命名空间与独立 Redis 中运行，未改业务配置或重启开发 Gateway。Go 1.26.8，测试容器限额 4 CPU / 3 GiB，GOWORK=off，使用已发布依赖。

每种模式 10,000 个不同数字标识、64 并发、6 个阶段：首次分配、重复请求、3→4 节点、移除一个原节点、故障后重复请求、恢复原有节点集合。每阶段 10,000 次，共每模式 60,000 次。三轮 race 加最终全量回归合计 **960,000 次四模式请求**，通过且无请求错误、无违反规则的迁移、无数据竞争。

| 模式 | 最终回归扩容漂移 | 移除原节点后的变化 | 重复/故障后重复/恢复原集合 |
| --- | --- | --- | --- |
| off | 不约束 | 普通负载均衡，三节点均收到请求，无保持映射 | 不约束固定归属 |
| auto | 0 / 10,000 | 3,297 个受影响会话使用原备用链，其他会话不动 | 均零非预期漂移 |
| hash | 2,524 / 10,000（25.24%） | 2,480 个会话迁移，全部原属被移除节点 | 均零非预期漂移 |
| manual | 0 / 10,000 | 4,000 个会话全部到指定备用节点 | 均零非预期漂移 |

三轮 race 中 hash 扩容漂移为 24.45%、24.39%、25.75%，全部只迁往新增节点；auto/manual 扩容始终零漂移。手动映射额外逐条核对数字个位数与主备配置，非仅比较稳定性。

额外验证：

- header、query、cookie、session、IP 各 2,000 个标识，每标识 5 次跨两 Gateway 请求，每轮共 50,000 次；三轮 race 与全量回归均通过。
- auto 每轮创建 20,000 条映射，分页从已有 Gateway 同步；未订阅节点补齐 1,000 条增量；删除短期日志后恢复到 21,001 条。新映射请求阶段 Redis 无 affinity journal，显式后台同步后才产生短期日志，无逐会话 Redis key。
- 最终全量回归及补充 race 在新 Gateway 上逐条发送 20,000 个原会话请求，路由结果与源 Gateway 一致；保留首次冲突、检查点重启、容量拒绝和路径/方法/缺失标识/主备等边界回归。
- 三轮定向 race：321.645s。最终全量：74 项顶层测试通过、1 项独立 Redis 进程故障测试未启用，32.537s。该进程故障项不能算作本轮通过。

最终普通构建的六阶段观测：off 17,930～19,285 req/s，auto 17,524～19,595，hash 17,096～18,616，manual 18,319～19,207；各模式阶段 p99 分别约 9.09～13.92 / 8.57～11.59 / 8.89～12.39 / 8.89～9.64 ms。入口为并发调用真实 Gateway Handler，上游为真实本机 HTTP 服务，保留代理与日志开销；不包含客户端到 Gateway 的网络/TLS，因此不能当生产网络吞吐上限或模式间性能排名。

测试夹具修正记录：首次延长 race 后，发现模拟节点仅本地 SetNode、定时注册中心刷新后被移除；登记夹具心跳时又纠正了 DB15 连接串缺少 redis:// 前缀的问题。修正后重新完整运行三轮。失败日志保留在 dev1 `/tmp/go-gateway-reliability-20261022/race-fixture-failure.log` 和 `race-fixture-dsn-failure.log`，未通过重试掩盖请求失败。另将大批量待同步队列完整排空后再开始增量补齐断言，避免把单批上限误当数据丢失。

复现：准备独立 Redis，使容器内 127.0.0.1:6379 指向测试实例，设置 `GATEWAY_REDIS_INTEGRATION=1 GATEWAY_AFFINITY_RELIABILITY=1 GOWORK=off`；执行 `go test -v ./...`，定向三轮执行 `go test -race -v ./core -run 'TestAffinityReliability|TestAffinitySnapshotReplayAndPeerAuthentication|TestAffinityAcrossManagersAndSources|TestRedisAffinitySharedMapping|TestAutomaticAffinityCheckpointAndCapacity|TestManualAffinityPathsMethodsAndBackups' -count 3`。

补充 20,000 条快照路由逐条核对的 race 通过（30.073s），随后 go vet ./... 通过。

结果摘录：[全量](testdata/reliability/20261022-full.txt)、[三轮 race](testdata/reliability/20261022-race.txt)、[快照路由 race](testdata/reliability/20261022-snapshot-race.txt)。本轮仍未覆盖百万条同时存活映射、跨物理机网络分区或小时级持续压测；不宣称自动模式在首次并发冲突下具备强一致性，也不以发现层主动移除测试代替故障检测延迟验证。

## 会话保持重构工作区验证（2026-10-22，待验收，不新增版本）

- GOWORK=off，正式依赖保持不变，无 replace；全量Redis集成与独立Redis进程中断测试通过，74项顶层测试（11.176s）。
- 全量Redis race通过（67.465s）；最终保持/生命周期定向race通过（8.307s），末次日志断档及冲突定向race通过（2.598s），并发快照/游标与生命周期定向race通过（3.131s）。
- 多条规则：路径完全/前缀、GET方法限定、首匹配off排除、缺失标识拒绝、数字个位数、指定主备、手动未匹配502。
- 自动保持：扩容不迁移已有会话；刻意制造不同节点视图的首次冲突，保留各自首条记录并统计；容量上限拒绝新会话、保留旧会话；检查点重启恢复。
- hash：增加节点只将部分键移给新增节点，删除新增节点后原归属恢复；不设置4096目标数量。
- 同步：600条映射跨页节点快照、匿名访问401、未订阅节点从游标补齐、增量日志断档后的节点快照恢复。600次新会话HTTP请求没有产生Redis增量写入，后台SyncAffinity才写有TTL的日志，不创建逐会话Redis键。
- GatewayApp管理、旧路由和affinity-events三通道生命周期；短期日志容量与过期、节点快照加密鉴权；HTTP/2/WebSocket及现有状态/密钥恢复回归通过。
- 全量基准（Apple M3 Max）：AffinityHashProxy 139565 ns/op，AffinityAutomaticProxy 142120 ns/op，均1106 allocs/op；包含完整代理、本地HTTP上游和日志，非纯查表耗时，不代表生产吞吐上限。APIKeyRead 39726 ns/op，LimiterIPWindow 1317 ns/op，PublishedHostQuota 5013 ns/op，MonitorCheck 5120970 ns/op。
- go vet、node --check、go mod tidy -diff及git diff --check通过；go doc -all ./core与源码核对AffinityRule / AffinityMapping / AffinityConfig、Manager.SyncAffinity / AffinityStatus，替代ai/tools/getExports。
- 未做百万级映射容量及长时间压力验证，不承诺每条固定50字节；内存需包含映射、主备链、待同步数据及快照副本。

## v1.5.18 发布验证（2026-10-22）

- 永久 Key：UTC、Asia/Shanghai、America/New_York 下使用当前序列化持久化及重启；旧无时区零时间兼容；真实过期、Host 范围、撤销和权限拒绝。
- 从仅含认证补丁及已发布依赖的隔离源树执行 GOWORK=off 全量、race、vet、基准；使用现有本机 Redis 的隔离测试前缀。
- 无新增导出 API；以 GOWORK=off go doc -all ./core 交叉核对，替代不可用的 ai/tools/getExports。

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
- `ai/tools/getExports` 在本工作区不可用，使用 `GOWORK=off go doc -all github.com/api-go/gateway/core` 与导出源码交叉核对 README 的 API 表；本次未新增 Go API。
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

- TestPermanentAPIKeySurvivesShanghaiPersistence：独立上海时区子进程覆盖永久Key持久化、重启和认证；同时运行有限过期与撤销回归。

## v1.5.20 HTTP/2 WebSocket 发布验证（2026-10-23）

依赖已发布 HTTP v1.5.14 与 Service v1.5.48；独立发布验证使用 GOWORK=off，不使用相邻模块源码。

真实 TLS 集成覆盖默认自动 H2→H1/H2（无需应用协议开关）、旧 TLS 后端 ALPN 回退、H1 客户端兼容、多流与普通请求复用同一 TCP、关闭单流、发布新路由后旧隧道持续工作与后续请求使用新路由。认证/证书错误不触发协议降级。Go 的入口声明仍需启动环境 GODEBUG=http2xconnect=1。

2026-10-22 / Apple M3 Max 本轮基准：APIKeyRead 35792 ns/op，PublishedHostQuota 4962 ns/op，MonitorCheck 6043671 ns/op。H1/H2 TLS 对照方法与三轮原始结果位于相邻 http/testdata/websocket/20261022-tls-compare.md；客户端性能不能直接当作网关全链路性能。

2026-10-23 独立发布检查：`GOWORK=off GATEWAY_REDIS_INTEGRATION=1`，默认和启动 `GODEBUG=http2xconnect=1` 两种模式的全量 test-v/count1、vet、race/count1、benchmark 均通过。使用现有 Redis 的隔离测试前缀，既有服务前后 PONG，未启停用户 Redis。H2 模式实际执行 `TestGatewayWebSocketHTTP2TLS/h2-h1` 与 `h2-h2`；默认模式跳过该需启动声明的专项，既有 H1 回归仍执行。独立进程可靠性大矩阵及主动 Redis 进程故障测试未启用，不计入本次覆盖。

`ai/tools/getExports` 不可用，以 `GOWORK=off go doc -all ./core` 与导出源码核对；本版本无新增或移除 Go API。Apple M3 Max 本次 APIKeyRead / PublishedHostQuota / MonitorCheck：默认 35852 / 4711 / 6446051 ns/op，H2 启动模式 39653 / 5253 / 5097200 ns/op；此基准不构成 H1/H2 网关吞吐对照。

## External addressing validation (2026-10-23)
- Real TLS frontend → HTTP management listener: login succeeds with Secure Cookie, cross-origin logout fails and same-origin CSRF logout succeeds.
- Independent admin trust (business trust does not transfer), untrusted loopback spoof, partial pairs, null/duplicate/path Origin rejection.
- Existing direct-IP management, sessions, API-key, HTTP/2, H2C and WebSocket regression suites retained.
- API review fallback: go doc and exported sources, getExports unavailable.

- DL development verification before release used temporary replace mappings with GOWORK=off and server-side Go build. Real browser https://gateway-dev.deeler.cn: login 200, Secure/HttpOnly Cookie, authenticated POST /api/check-host 200 without persistent configuration writes, cross-origin/null/path Origin logout 403, same-origin logout 200. Direct http://10.177.65.105:81 login/logout 200; network-peer spoofed X-Host/X-Scheme/X-Forwarded-*/Forwarded login 403. Management trust explicitly set to the actual in-container proxy source 127.0.0.1/32; no implicit loopback/private trust.
- Released-dependency validation uses HTTP v1.5.15 / Discover v1.5.18 / Service v1.5.49, GOWORK=off, no replace. Full tests, benchmarks and race validation.

- Released-dependency benchmarks (Apple M3 Max): API-key read 38413 ns/op; NewGatewayApp 19.78 ns/op; limiter 1418 ns/op; published Host quota 6721 ns/op; monitor check 5343776 ns/op.

## v1.5.22 default relay validation
- Actual TLS Gateway business handler → HTTP independent AdminHandler, both with no external-address configuration: login with Secure Cookie, cross-origin write rejection and same-origin CSRF logout. Client-supplied ID and conflicting address headers are overwritten at the public boundary. Optional explicit source restrictions remain covered.
- 开发验收使用 GOWORK=off 与显式 http/discover/service replace；发布复验使用正式 HTTP v1.5.16 / Discover v1.5.19 / Service v1.5.50。

- Development validation passed: GOWORK=off go test -v ./..., go test -bench=. ./..., go test -race ./... and go vet ./.... Apple M3 Max: API-key read 37136 ns/op; limiter 1352 ns/op; published Host quota 6027 ns/op; monitor check 5862705 ns/op. HTTP is tested independently; downstream modules use documented explicit local replace mappings.

- v1.5.22 发布复验：正式 HTTP v1.5.16、Discover v1.5.19、Service v1.5.50，GOWORK=off、无 replace；全量测试、benchmark 和 go vet 通过。API 用 go doc -all ./core 与导出源码核对（getExports 不可用）。

## v1.5.23 发布验证（2026-10-26）

GOWORK=off，正式依赖 crypto v1.5.9，无 replace。`go test -v ./...`、`go test -bench=. ./...`、`go vet ./...`、`go mod verify` 通过。使用 `go doc` 与源码交叉核对 API，替代 getExports。现有回归覆盖本模块业务；新旧 GCM 格式、认证失败和性能基准见 crypto v1.5.9。

跳过的外部条件场景：
- --- SKIP: TestAffinityLegacyScopeCompatibility (0.00s)
- --- SKIP: TestAffinityLiveConnectionsAcrossRoutingChange (0.00s)
- --- SKIP: TestAffinityLVSWorker (0.00s)
- --- SKIP: TestAffinityRandomGatewayLVS (0.00s)
- --- SKIP: TestAffinityReliability (0.00s)
- --- SKIP: TestManualAffinityPathsMethodsAndBackups (0.00s)
- --- SKIP: TestAutomaticAffinityCheckpointAndCapacity (0.00s)
- --- SKIP: TestAffinityExpansionAndHashMovement (0.00s)
- --- SKIP: TestGradualAffinityShardCluster (0.00s)
- --- SKIP: TestGradualSnapshotCapacityCountsSessionBindings (0.00s)
- --- SKIP: TestAffinitySnapshotReplayAndPeerAuthentication (0.00s)
- --- SKIP: TestAffinityAcrossManagersAndSources (0.00s)
- --- SKIP: TestAffinityHTTPAndWebSocket (0.00s)
- --- SKIP: TestRedisAlertUsesThirdSharedSample (0.00s)
- --- SKIP: TestAPIKeyClusterRevocation (0.00s)
- --- SKIP: TestGateway (0.00s)
- --- SKIP: TestCertificateLibraryClusterConvergence (0.00s)
- --- SKIP: TestCertificateAutomaticSharedKeyConcurrencyAndRestart (0.00s)
- --- SKIP: TestClusterSnapshotAndKeyRecovery (0.00s)
- --- SKIP: TestRedisAffinitySharedMapping (0.00s)
- --- SKIP: TestExplicitKeyRecoveryStaysExternal (0.00s)
- --- SKIP: TestClusterCASConvergenceDeletionAndSharedQuotas (0.00s)
- --- SKIP: TestClusterMissingStateRetainsCacheAcrossRestart (0.00s)
- --- SKIP: TestClusterAdministratorPasswordSharedAndReset (0.00s)
- --- SKIP: TestRedisHourlyCountersPreserveLegacyAndIncrementOnce (0.00s)
- --- SKIP: TestRedisCompactRingAndAtomicCounters (0.00s)
- --- SKIP: TestMonitorPeriodRecentResults/redis (0.00s)
- --- SKIP: TestGatewayDiscoveryClientAndImmediateMonitor (0.00s)
- --- SKIP: TestRegistryReadOnlyIncludesUnreferencedAndExpiredNodes (0.00s)
- --- SKIP: TestMonitorClusterLeaseAndSharedHistory (0.00s)
- --- SKIP: TestRedisOutageRetainsTrafficAndRecovers (0.00s)
- --- SKIP: TestGatewayWebSocketHTTP2TLS (0.00s)

基准结果：
```text
BenchmarkAPIKeyRead-16                	   32738	     35569 ns/op
BenchmarkNewGatewayApp-16             	56115848	        19.50 ns/op	      64 B/op	       1 allocs/op
BenchmarkLimiterIPWindow-16           	  900435	      1328 ns/op	    1305 B/op	      21 allocs/op
BenchmarkPublishedHostQuota-16        	  184228	      6381 ns/op	    3856 B/op	      96 allocs/op
BenchmarkMonitorCheck-16              	     201	   5964082 ns/op
```

## v1.5.24 发布验证（2026-10-31）

- GOWORK=off 使用正式依赖 Discover v1.5.21、Redis v1.5.22、Service v1.5.52；全量测试、go vet 和 Gateway core race 通过。
- Gateway core race：68.172s；无源码依赖替换进入正式 go.mod。

## GitHub migration validation (pending release)

Module paths and internal dependency versions are migrated. Development verification uses GOWORK=off and explicit sibling replacements; remove these replacements before release. See migration validation logs for actual results. Existing unrelated working changes are preserved and require separate review.

API inventory verification: `GOWORK=off go doc` succeeded; output retained in the migration validation directory. Source exports were preserved by the path migration.

## GitHub release verification

GOWORK=off go test -v ./... and go test -bench=. ./... passed using published GitHub dependencies, without local replacements. go mod verify passed.

## GitHub release verification

GOWORK=off go test -v ./... and go test -bench=. ./... passed using published GitHub dependencies, without local replacements. go mod verify passed.

## 日志集成 — 2026-11-12

- Gateway 默认启用代理精简而不启用 GET 日志抑制；历史明文和压缩/字典配对保留策略、活跃文件及备份保护。
- 全量回归/基准使用 GODEBUG=http2xconnect=1、GOWORK=off，以及 /tmp/apigo-validation-gateway.mod 显式替换本次本地模块。API 用 go doc 与源码核对。

## 日志依赖联动 — 2026-11-12

开发验证使用 GOWORK=off 与 go.mod 的显式本地 replace；执行完整测试、全量基准（benchtime=100ms），API 用 go doc 与导出源码交叉核对。结果记录在 /tmp/apigo-gateway-integration-*.txt。
