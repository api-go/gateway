package gateway

import (
	"apigo.cc/go/cast"
	goCrypto "apigo.cc/go/crypto"
	"apigo.cc/go/js"
	"context"
	"fmt"
	"net/url"
	"strings"
	"time"
)

type MonitorConfig struct {
	Enabled          bool
	Configured       bool // Explicit choice; old/missing settings default to enabled for proxy Hosts.
	URL              string
	IntervalSeconds  int
	Domain           string
	Path             string
	ExpectedStatus   int
	BodyContains     string
	CheckScript      string
	FailureThreshold int               // Deprecated: alert thresholds are configured on AlertChannel.
	ChannelIDs       []string          // Deprecated: AlertChannel selects Hosts centrally.
	Headers          map[string]string // Input only; encrypted before saving, never returned.
	ClearHeaders     bool              // Explicitly remove saved request credentials.
	SealedHeaders    string
	HasHeaders       bool
}
type AlertChannel struct {
	HostMode          string // all (default) or selected; selected with no IDs sends to no Hosts.
	HostIDs           []string
	FailureThreshold  int // Default 3; counts the same samples shown by monitoring.
	ID                string
	Name              string
	Provider          string // dingtalk, wecom, feishu, custom
	Enabled           bool
	Sign              bool
	Script            string // function(event, http) { return http.request({...}); }
	RepeatSeconds     int    // 0: only transitions; otherwise repeat unresolved failures.
	QuietStart        string // HH:MM in Timezone, start inclusive.
	QuietEnd          string // HH:MM, end exclusive; may cross midnight.
	Timezone          string
	Revision          int64
	SealedCredentials string
	HasCredentials    bool
}

func monitorDefaults(value MonitorConfig) MonitorConfig {
	if !value.Configured {
		value.Enabled = true
	}
	if value.IntervalSeconds == 0 {
		value.IntervalSeconds = 60
	}
	if value.Path == "" {
		value.Path = "/"
	}
	if value.FailureThreshold == 0 {
		value.FailureThreshold = 3
	}
	return value
}
func validateMonitor(value MonitorConfig) error {
	value = monitorDefaults(value)
	if value.IntervalSeconds < 60 || value.IntervalSeconds > 86400 || value.FailureThreshold < 1 || value.FailureThreshold > 20 || value.ExpectedStatus != 0 && (value.ExpectedStatus < 100 || value.ExpectedStatus > 599) || !validPreviewURL(value.Path) || len(value.Path) > 4096 || len(value.BodyContains) > 4096 || len(value.ChannelIDs) > 10 || len(value.Headers) > 50 {
		return fmt.Errorf("监控参数无效：周期 60–86400 秒，失败阈值 1–20，路径须以 / 开头")
	}
	if value.URL != "" && !validWebhook(value.URL) {
		return fmt.Errorf("监控地址必须为完整 HTTP(S) URL")
	}
	if value.Domain != "" && !validDomain(value.Domain) {
		return fmt.Errorf("监控域名无效")
	}
	for k, v := range value.Headers {
		if len(k) > 128 || len(v) > 4096 || strings.ContainsAny(k+v, "\r\n") {
			return fmt.Errorf("监控 Header 无效")
		}
	}
	return validateCode(value.CheckScript)
}
func validateCode(code string) error {
	if code == "" {
		return nil
	}
	if len(code) > 16384 {
		return fmt.Errorf("低代码最多 16384 字节")
	}
	pool := js.NewPool()
	defer pool.Stop(context.Background())
	if err := pool.Define("gatewayScript", code, 1); err != nil {
		return fmt.Errorf("低代码校验失败：%w", err)
	}
	return nil
}
func runMonitorCode(ctx context.Context, code string, args ...any) (any, error) {
	pool := js.NewPool()
	defer pool.Stop(context.Background())
	if err := pool.Define("gatewayScript", code, 1); err != nil {
		return nil, err
	}
	result, err := pool.CallContext(ctx, "gatewayScript", 5*time.Second, map[string]any{"SafeMode": true}, args...)
	if err != nil {
		return nil, err
	}
	return result, nil
}
func hostFingerprint(host Host) string {
	// Notification policy and scheduling do not change what a probe checks.
	host.Monitor = monitorDefaults(host.Monitor)
	host.Monitor.ChannelIDs = nil
	host.Monitor.FailureThreshold = 3
	host.Monitor.IntervalSeconds = 60
	host.Monitor.Configured = true
	return legacyHostFingerprint(host)
}
func monitorSampleMatches(sample MonitorSample, host Host) bool {
	// Preserve the current development build's persisted streak on upgrade.
	return sample.Fingerprint == hostFingerprint(host) || sample.Fingerprint == legacyHostFingerprint(host)
}
func legacyHostFingerprint(host Host) string {
	host.Monitor = monitorDefaults(host.Monitor)
	if len(host.Monitor.ChannelIDs) == 0 {
		host.Monitor.ChannelIDs = nil
	}
	host.HTTPS.SealedKey = ""
	host.Name = ""
	host.Monitor.Headers = nil
	host.Monitor.ClearHeaders = false
	host.Monitor.HasHeaders = false
	data, _ := cast.ToJSONBytes(host)
	return goCrypto.SHA256ToHex(data)
}
func validWebhook(raw string) bool {
	value, err := url.Parse(raw)
	return err == nil && (value.Scheme == "https" || value.Scheme == "http") && value.Host != "" && value.User == nil && value.Fragment == ""
}

func monitorRouteFingerprint(host Host) string {
	data, _ := cast.ToJSONBytes(struct {
		Domains  []string
		Proxies  any
		Rewrites any
		URL      string
		Domain   string
		Path     string
	}{host.Domains, host.Proxies, host.Rewrites, host.Monitor.URL, host.Monitor.Domain, monitorDefaults(host.Monitor).Path})
	return goCrypto.SHA256ToHex(data)
}

func monitorEnabled(host Host) bool {
	settings := monitorDefaults(host.Monitor)
	return settings.Enabled && (host.Monitor.Configured || host.Monitor.URL != "" || len(host.Proxies) > 0)
}
