# Gateway v1.5.19

[中文文档](README.md)

Dynamic HTTP gateway built on `apigo.cc/go/service`, with Host drafts, publication and rollback, adaptive HTTPS, quotas, service discovery, monitoring and signed robot alerts.

## Session affinity

`Host.AffinityRules []AffinityRule` contains ordered rules with ID, incoming Path (exact or trailing /*), optional Methods and AffinityConfig. The first matching rule applies; existing Host.Affinity remains a legacy fallback. The editor has a separate affinity tab. Each mode includes a one-line scenario hint and a clickable lightbulb with use cases, benefits and costs: off for stateless services, auto for retaining established sessions during expansion, hash for rebuildable state with acceptable membership drift, and manual for explicit business partitions and backup chains. Fixed HTTP(S) targets retain their fixed address.

Modes: off, auto (local mappings with asynchronous replication), hash (Rendezvous), manual (value-to-primary/backup chains). Legacy redis means auto and no longer performs per-request Redis operations. Sources: header/query/cookie with Name, session (Header → Cookie → Query, default sessionid), ip. Missing: balance or reject; no implicit IP fallback. Transform: full, last (ASCII byte, case-sensitive), lastDigit (unsigned decimal input). TTLSeconds: fixed lifetime, default3600, maximum604800, no renewal on hits. Manual Mappings contain Values and ordered Nodes matching discovery addresses; unmatched values and exhausted chains fail with proxy502.

HashStrategy selects direct (default, stateless Rendezvous) or gradual (a shared 4096-shard table, without per-session mappings). Gradual uses local lookup and counters only on the request path. Background updates reuse the affinity Pub/Sub channel, bounded journal and encrypted peer snapshots. Replay cursors carry both an ID and a content fingerprint to detect stream recreation with reused IDs. A five-second Redis lease elects one scheduling master; CAS checks the lease and previous table before atomically storing and publishing a newer version. Versions are internal and require no client cookies or version headers. Business traffic and management requests do not pass through the master.

Gradual publishes approximately one load report per second using a 30-second decaying window. Reports have a ten-second Redis fallback; missing reports and inconsistent discovery views pause voluntary migration. Active bounded tables renew a one-hour Redis TTL and also participate in local checkpoints and peer recovery. Once topology and load reports agree, the coordinator computes and broadcasts the required low-load-first shard allocation in one update, without a migration timer or batch quota. Shard counts are allocated without forcing equal traffic; a stable topology does not continually reshuffle owners. Internal revisions reject stale synchronization data; forwarding only reads the current local table. Discovery, roughly one-second background reporting, and asynchronous propagation still take time. Failed owners use deterministic live fallback. Initial table readiness returns503; an existing ready table keeps forwarding during Redis/master outages. Up to64 active rule/application tables and1024 upstream addresses per table are supported.

Low load does not mean no live session. Gradual migration can move existing sessions and cannot identify new clients by hashing alone; choose auto when live bindings must survive expansion unchanged. NodeID must be unique and cluster protocol/configuration must match. A dead Gateway's retained management heartbeat can temporarily delay migration; simultaneous loss of all copies, Redis and checkpoints cannot restore the original table. Direct hash still needs no shared table.

An established WebSocket tunnel or in-flight HTTP/SSE stream stays on its existing upstream. Subsequent requests multiplexed over the same HTTP/2 connection use the current routing table. Removing the former migration budget settings preserves the previous development build's normalized routing digest, so existing mappings and shard scopes remain addressable; upgrade all cluster instances together to use the new allocation behavior.

Auto selects a deterministic primary and up to two backups locally, stores the first live mapping and immediately forwards. Expansion preserves recorded chains. Conflicting simultaneous first assignments are deliberately not overwritten and may differ across gateways until expiration; this is not strong consistency. Primary recovery uses the original chain order. If all recorded candidates disappear, current-node hashing provides an escape, without guaranteeing that later expansion preserves that emergency choice. Business session data is not replicated.

Proxy requests, including first assignments, never access Redis for affinity. GatewayApp flushes batches roughly every100ms, applies Pub/Sub updates and polls an independent replay cursor roughly every second when idle. Redis6.2+ Stream Prefix:affinity-journal retains at most512 batches of256 entries, trimmed to five minutes using Redis time and expiring after inactivity. It is a bounded incremental journal, not a complete per-session Redis table.

New or lagging nodes obtain encrypted, authenticated paginated snapshots from a ready Gateway management endpoint and replay subsequent increments. Config.AffinityAdvertise overrides the advertised management base URL for NAT/port mapping. Prefix, encryption key and protocol must match, and NodeID must be unique. Auto requests receive503 while initial synchronization is pending. A lone node can only recover its own checkpoint; lost exclusive state cannot be recreated.

Local maps default to one million entries (Config.MaxAffinityEntries), rejecting new sessions at capacity rather than evicting live mappings. AffinityStatus exposes readiness, entries, pending records, conflicts and capacity rejection counts. Private local checkpoints are written approximately every30seconds and on normal Close. There is no cold disk lookup tier. Simultaneous loss of all live copies can lose changes after the last checkpoint. Redis outages allow already-ready nodes to continue local processing and queue changes, but may prevent newcomers from becoming ready.

Direct Manager users call SyncAffinity(ctx) before auto traffic and periodically afterward; GatewayApp manages this lifecycle. Hash mode needs no session table or fixed logical target count; additions move keys only to the added node, removals affect its former keys. Positive-weight nodes participate equally. Discovery convergence still matters. HTTP/2 connection reuse does not guarantee session affinity across multiple connections or upstream calls.

Management Redis defaults to Service.Register. Shared Redis/Prefix with separate persistent DataDir and unique NodeID supports L4 ingress and uploaded certificates; HTTP-01 issuance remains single-node. State/key recovery retains the previously documented first-valid-snapshot policy and requires backups; affinity maps use their independent checkpoints/snapshots/journal.

## Run

```sh
go build -o gateway .
./gateway admin-password
./gateway start
./gateway status
```

The management UI is enabled by default on a private address, port 81. Set `Gateway.AdminListen` explicitly when needed; `AdminDisabled: true` disables the management listener. The sole administrator password is configured through the CLI, never command arguments. The private HTTP listener should use a controlled HTTPS ingress when accessed over untrusted networks. No default password is provided.

```yaml
service:
  Listen: "127.0.0.1:8080,http|127.0.0.1:8443,https"
gateway:
  AdminListen: "192.168.1.10:81"
  DataDir: "data/gateway"
  ClientIP:
    Mode: direct
```

All dependencies are released versions: Service v1.5.50, Discover v1.5.19, HTTP v1.5.16, API v1.5.20 and JS v1.5.17. No local replacements are required.

## Management and program access

The UI provides Hosts, certificates, read-only service discovery, monitoring, management logs, API access and settings. A Host can contain multiple domains, proxy/rewrite/redirect/static rules and quotas. Saving a draft does not affect traffic; publication validates and activates it. Concurrent edits use revision checks. Certificate secrets are encrypted using a persistent key, generated automatically unless explicitly configured.

Configuration tests run immediately without an upstream request. The business test tab performs a bounded GET without following redirects or consuming quotas. Only domain, path and User Agent preferences are stored in browser LocalStorage; request credentials are not retained there.

API access creates scoped, expiring and revocable keys. The raw token is shown once; authoritative storage keeps only its digest. `Authorization: Bearer <API_KEY>` enables `/api/v1/hosts`, `/nodes`, `/draft`, `/preview`, `/publish`, `/rollback` and `/delete`, subject to Host and operation permissions. A program key cannot manage keys, certificates, monitor scripts or alert bindings. See the [integration guide](core/web/api-guide.txt). Browser administration uses separate Cookie/CSRF authentication; encrypted sessions survive restarts for up to eight hours. Logout and password changes revoke sessions.

## Discovery, monitoring and alerts

Gateway acts as a discovery client and removes its own service registration. The registry page lists native registered services and their addresses, weights and heartbeat status. Protocol labels describe Gateway's configured calling protocol because native registration does not advertise a protocol.

Published, enabled proxy Hosts are monitored automatically at startup and every 60 seconds by default. Without an explicit target, all Host domains are checked; a failed domain fails the round. The default accepts HTTP 2xx/3xx without following redirects. An explicit expected status, response substring or low-code predicate can further constrain success. Request headers are encrypted. Monitoring samples upstream availability rather than proving public ingress TLS or every replica healthy.

The matrix shows the last 24 hourly periods or 30 daily periods. Each cell follows the latest check in its period: green mean latency after success, red successful/total checks after failure. Five dots below the number show that period's last five results, oldest to newest, within the existing cell height. Missing results leave blank space; mixed legacy periods without a known latest result stay gray. Hover text includes period totals and failures. Period details show checks, average latency, failures and one representative response: the latest failure takes priority, otherwise the latest success is retained. Response bodies are capped at 4096 bytes. Missing legacy counts and outputs are marked explicitly. Hosts and monitoring share creation order; older timestamps are inferred from retained history.

Each Host uses one Redis Hash, `<Prefix>:monitor:<HostID>`, with at most 30 daily fields, 24 hourly fields and 10 recent slots for failure streaks and alert delivery state. Daily/hourly values contain total latency, checks, sticky failure reason, failure count, a legacy-count marker, representative output and up to five recent boolean results. Recovery changes the cell status while retaining past failure counts and the representative failed response. Hashes expire 30 days after their last update; old buckets are pruned incrementally. Standalone mode uses the same bounded structure in DataDir. Native application date logs default to 30 days of retention; unrelated files are preserved.

Alert channels support DingTalk, WeCom and Feishu robot Webhooks, with native DingTalk/Feishu signatures. Configure Host scope (all by default, including future Hosts), consecutive failure threshold (default3), enablement, repeat reminders and quiet hours in channel dialogs. Selected scope with no HostIDs sends to no Hosts. Alerts consume the same samples shown by monitoring; there is no separate alarm sampling interval. Host monitoring defaults to60seconds. Changing channels, thresholds or cadence preserves the streak, including during upgrade; an eligible existing fault is handled on the next check. Actual probe changes start a new streak and recent-dot series while retaining period totals. Lease contention retries on the next scheduler tick. Deprecated MonitorConfig.ChannelIDs/FailureThreshold no longer select channels or thresholds; AlertChannel.HostMode/HostIDs/FailureThreshold control them. Advanced `(event, http)` functions control HTTP requests, signing and payloads directly; the UI supplies a DingTalk example. See the [low-code guide](core/web/lowcode-guide.txt). Scripts are trusted administrator code, not an OS sandbox. Delivery records cannot guarantee exactly-once external delivery across process or storage failures. Real robot delivery requires environment-specific verification; automated signing and receipt checks use local receivers.

Back up DataDir and Redis state together with their persistent encryption keys. Redacted configuration exports are not complete credential backups.

## Go API

The reusable package is `apigo.cc/go/gateway/core`; the module root remains the CLI.

| API | Purpose |
| --- | --- |
| `Config`, `GatewayConf`, `Run`, `NewGatewayApp` | Application configuration and lifecycle |
| `GatewayApp.Start`, `Stop`, `Reload`, `Status`, `Manager`, `AdminAddr` | Service lifecycle and embedded access |
| `NewManager`, `Manager.Close`, `Handler`, `AdminHandler` | Embed business and management handlers |
| `Manager.SaveDraft`, `Publish`, `Rollback`, `Delete` | Revision-checked Host changes |
| `Manager.Hosts`, `Events`, `Nodes`, `Services`, `Settings`, `Reconcile` | Redacted state, runtime information and reconciliation |
| `Manager.SyncAffinity`, `AffinityStatus` | Synchronize embedded affinity state and inspect readiness/capacity |
| `AffinityConfig`, `AffinityRule`, `AffinityMapping` | Modes, path/method rules and primary/backup mappings |
| `Manager.Preview`, `CheckHost`, `TestUpstream`, `ValidateHost` | Preview, publication validation and bounded upstream GET |
| `Manager.Certificates`, `SaveCertificate`, `DeleteCertificate`, `SetCertificate`, `CertificateCache` | Encrypted certificate management and legacy compatibility |
| `Manager.ACMESettings`, `SetACMESettings`, `IssueCertificate` | Single-node HTTP-01 configuration and issuance |
| `SetAdminPassword`, `DefaultAdminListen`, `WriteDefaultAdminConfig` | Administrator and initial configuration |
| `NewLimiter`, `Limiter.Evaluate`, `Sweep` | Standalone request quotas |
| `Manager.RegistryServices`, `ReadRegistryServices` | Read-only native registry |
| `Manager.MonitorStatuses`, `MonitorHistory`, `MonitorMatrix` | Current status, bounded recent state and period summaries |
| `Manager.MonitorTick`, `RunMonitorCheck`, `SendAlert` | Context-aware monitoring and alert delivery |
| `PruneGatewayLogs` | Retention cleanup for matching dated application logs |

Public state and result models include `Host`, `HostRecord`, `HostVersion`, `State`, `Event`, `HTTPSConfig`, `LimitRule`, `MatchCondition`, `GroupDimension`, `LimitDecision`, `PreviewInput`, `PreviewResult`, `NodeStatus`, `ServiceStatus`, `CertificateInfo`, `CertificateRecord`, `DomainTLSStatus`, `ACMESettings`, `APIKey`, `MonitorConfig`, `MonitorSample`, `MonitorDomainResult`, `MonitorOutput`, `MonitorBucket`, `MonitorMatrixRow`, `HostMonitorStatus`, `AlertChannel`, `AlertEvent`, `RegisteredService`, `RegistryNode` and `UpstreamTestResult`. `ErrConflict` identifies stale revisions. Raw state can contain internal credential digests/ciphertexts; use redacted management outputs for external consumers.

`GatewayApp` manages monitoring automatically. Embedded `NewManager` users must schedule `MonitorTick` and propagate cancellation themselves. Static/proxy configuration and detailed quota semantics are documented in the [Chinese guide](README.md).

## Validation

```sh
GOWORK=off GATEWAY_REDIS_INTEGRATION=1 go test -v ./...
GOWORK=off go test -bench=. ./...
GOWORK=off GATEWAY_REDIS_INTEGRATION=1 go test -race ./...
```

See [TEST.md](TEST.md) for test coverage, benchmarks and environment limits. Redis tests use isolated prefixes. Browser test fixtures are under `testdata/browser` and are not production runtime dependencies.

- Permanent API keys recognize both the zero instant and legacy timezone-free zero wall-clock after persistence and restart; finite expirations remain enforced across UTC, Asia/Shanghai and America/New_York.

Established WebSocket tunnels and in-flight HTTP/SSE streams are not moved by table updates. Later requests on the same HTTP/2 connection still select from the current table. Shared shards do not guarantee zero session movement; use automatic per-key mappings when that is required, within their TTL, capacity, and failure limits.

## HTTP/2 WebSocket (v1.5.20)

Ingress supports RFC 8441 when the process starts with GODEBUG=http2xconnect=1 (required by Go/x/net). Upstream automatically prefers H2 for WSS, falling back only when ALPN/SETTINGS lack support. Plain WS stays on H1 unless H2C is explicitly configured. No WebSocketProxy.HTTP2 switch is required. HTTP rejections, certificate failures and timeouts do not trigger fallback. Independent Gorilla proxy dialers preserve their H1 network policy. Default proxy clients share H2 pools and bounded capability caches, without a shared cookie jar; Stop cleans idle default pools.

Depends on published HTTP v1.5.14 and Service v1.5.48; standalone checks run with GOWORK=off. TLS measurements and raw results: http/testdata/websocket/20261022-tls-compare.md.

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

## AES-GCM configuration compatibility

Released dependencies select crypto v1.5.9. Shared Symmetric.Decrypt/DecryptBytes authenticate `12-byte IV || ciphertext || 16-byte tag` first, then try the configured legacy IV. Existing APIs remain unchanged. Prefer EncryptBytesAutoIV for new data and upgrade readers before enabling writers. CBC/SM4 formats are unchanged.

## v1.5.24

已升级服务发现依赖至 Discover v1.5.21、Service v1.5.52、Redis v1.5.22，以获得 Redis 长时间故障后的异步恢复。
