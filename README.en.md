# Gateway v1.5.18

[中文文档](README.md)

Dynamic HTTP gateway built on `apigo.cc/go/service`, with Host drafts, publication and rollback, adaptive HTTPS, quotas, service discovery, monitoring and signed robot alerts.

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

All dependencies are released versions: Service v1.5.47, Discover v1.5.17, HTTP v1.5.13, API v1.5.20 and JS v1.5.17. No local replacements are required.

## Management and program access

The UI provides Hosts, certificates, read-only service discovery, monitoring, management logs, API access and settings. A Host can contain multiple domains, proxy/rewrite/redirect/static rules and quotas. Saving a draft does not affect traffic; publication validates and activates it. Concurrent edits use revision checks. Certificate secrets are encrypted using a persistent key, generated automatically unless explicitly configured.

Configuration tests run immediately without an upstream request. The business test tab performs a bounded GET without following redirects or consuming quotas. Only domain, path and User Agent preferences are stored in browser LocalStorage; request credentials are not retained there.

API access creates scoped, expiring and revocable keys. The raw token is shown once; authoritative storage keeps only its digest. `Authorization: Bearer <API_KEY>` enables `/api/v1/hosts`, `/nodes`, `/draft`, `/preview`, `/publish`, `/rollback` and `/delete`, subject to Host and operation permissions. A program key cannot manage keys, certificates, monitor scripts or alert bindings. See the [integration guide](core/web/api-guide.txt). Browser administration uses separate Cookie/CSRF authentication; encrypted sessions survive restarts for up to eight hours. Logout and password changes revoke sessions.

## Discovery, monitoring and alerts

Gateway acts as a discovery client and removes its own service registration. The registry page lists native registered services and their addresses, weights and heartbeat status. Protocol labels describe Gateway's configured calling protocol because native registration does not advertise a protocol.

Published, enabled proxy Hosts are monitored automatically at startup and every 60 seconds by default. Without an explicit target, all Host domains are checked; a failed domain fails the round. The default accepts HTTP 2xx/3xx without following redirects. An explicit expected status, response substring or low-code predicate can further constrain success. Request headers are encrypted. Monitoring samples upstream availability rather than proving public ingress TLS or every replica healthy.

The matrix shows the last 24 hourly periods or 30 daily periods. A successful period shows mean latency; a failed period shows ×. Period details show checks, average latency, failures and one representative response: the latest failure takes priority, otherwise the latest success is retained. Response bodies are capped at 4096 bytes. Missing legacy counts and outputs are marked explicitly. Hosts and monitoring share creation order; older timestamps are inferred from retained history.

Each Host uses one Redis Hash, `<Prefix>:monitor:<HostID>`, with at most 30 daily fields, 24 hourly fields and 10 recent slots for failure streaks and alert delivery state. Daily/hourly values contain total latency, checks, sticky failure reason, failure count, a legacy-count marker and representative output. Hashes expire 30 days after their last update; old buckets are pruned incrementally. Standalone mode uses the same bounded structure in DataDir. Native application date logs default to 30 days of retention; unrelated files are preserved.

Alert channels support DingTalk, WeCom and Feishu robot Webhooks, with native DingTalk/Feishu signatures. Configure failure thresholds per Host, and enable/disable channels, repeat reminders or quiet hours in channel dialogs. Advanced `(event, http)` functions control HTTP requests, signing and payloads directly; the UI supplies a DingTalk example. See the [low-code guide](core/web/lowcode-guide.txt). Scripts are trusted administrator code, not an OS sandbox. Delivery records cannot guarantee exactly-once external delivery across process or storage failures. Real robot delivery requires environment-specific verification; automated signing and receipt checks use local receivers.

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
