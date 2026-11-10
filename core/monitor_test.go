package gateway_test

import (
	"github.com/api-go/cast"
	goCrypto "github.com/api-go/crypto"
	"github.com/api-go/encoding"
	"github.com/api-go/file"
	gateway "github.com/api-go/gateway/core"
	"github.com/api-go/log"
	"github.com/api-go/redis"
	"github.com/api-go/safe"
	"github.com/api-go/service"
	"github.com/api-go/timer"
	"context"
	"fmt"
	"io"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"strings"
	"sync/atomic"
	"testing"
	"time"
)

func saveAlert(t *testing.T, m *gateway.Manager, provider, webhook, secret, script string, sign bool, configure ...func(*gateway.AlertChannel)) gateway.AlertChannel {
	t.Helper()
	h := m.AdminHandler()
	cookie, csrf := login(t, h, "admin", "gateway-test-password")
	listing := adminRequest(t, h, "/api/alerts", nil, cookie, csrf)
	state, _ := cast.FromJSON[map[string]any](listing.Body.Bytes())
	channel := gateway.AlertChannel{Name: provider, Provider: provider, Enabled: true, Sign: sign, Script: script}
	for _, apply := range configure {
		apply(&channel)
	}
	response := adminRequest(t, h, "/api/alerts/save", map[string]any{"revision": state["revision"], "webhook": webhook, "secret": secret, "channel": channel}, cookie, csrf)
	if response.Code != 200 {
		t.Fatal(response.Body.String())
	}
	saved, _ := cast.FromJSON[struct{ Channel gateway.AlertChannel }](response.Body.Bytes())
	if saved.Channel.SealedCredentials != "" {
		t.Fatal("credentials exposed")
	}
	return saved.Channel
}
func TestMonitorSignedRobotAcknowledgements(t *testing.T) {
	for _, provider := range []string{"dingtalk", "feishu", "wecom"} {
		t.Run(provider, func(t *testing.T) {
			m, _, config := manager(t)
			var calls atomic.Int32
			server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
				calls.Add(1)
				raw, _ := io.ReadAll(r.Body)
				payload, err := cast.FromJSON[map[string]any](raw)
				if err != nil {
					t.Error(err)
				}
				if provider == "dingtalk" {
					stamp := r.URL.Query().Get("timestamp")
					expected := encoding.Base64(goCrypto.HMACSHA256([]byte("sign-fixture"), []byte(stamp+"\nsign-fixture")))
					if r.URL.Query().Get("sign") != expected || cast.Int64(stamp) < 1000000000000 {
						t.Error("DingTalk signature invalid")
					}
					if payload["msgtype"] != "text" {
						t.Error("DingTalk payload")
					}
				}
				if provider == "feishu" {
					stamp := cast.String(payload["timestamp"])
					expected := encoding.Base64(goCrypto.HMACSHA256([]byte(stamp + "\nsign-fixture")))
					if payload["sign"] != expected || payload["msg_type"] != "text" {
						t.Error("Feishu signature invalid")
					}
				}
				w.Header().Set("Content-Type", "application/json")
				if provider == "feishu" {
					w.Write([]byte(`{"code":0}`))
				} else {
					w.Write([]byte(`{"errcode":0}`))
				}
			}))
			defer server.Close()
			public := saveAlert(t, m, provider, server.URL, "sign-fixture", "", provider != "wecom")
			raw, err := file.ReadBytes(filepath.Join(config.DataDir, "state.json"))
			if err != nil {
				t.Fatal(err)
			}
			if strings.Contains(string(raw), "sign-fixture") || strings.Contains(string(raw), server.URL) {
				t.Fatal("plaintext webhook or secret persisted")
			}
			state, _ := cast.FromJSON[gateway.State](raw)
			channel := state.AlertChannels[public.ID]
			if err := m.SendAlert(context.Background(), channel, gateway.AlertEvent{Domain: "example.test", State: "测试"}); err != nil {
				t.Fatal(err)
			}
			if calls.Load() != 1 {
				t.Fatal("unexpected sends")
			}
		})
	}
}
func TestMonitorHistoryTransitionsAndProtectedHeaders(t *testing.T) {
	m, _, config := manager(t)
	var healthy atomic.Bool
	healthy.Store(true)
	var checks, alerts atomic.Int32
	upstream := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		checks.Add(1)
		if r.Header.Get("Authorization") != "Bearer monitor-fixture" {
			t.Error("protected header missing")
		}
		w.Header().Set("Content-Type", "application/json")
		if !healthy.Load() {
			w.WriteHeader(503)
		}
		w.Write([]byte(`{"status":"ok"}`))
	}))
	defer upstream.Close()
	robot := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		alerts.Add(1)
		w.Header().Set("Content-Type", "application/json")
		w.Write([]byte(`{"errcode":0}`))
	}))
	defer robot.Close()
	saveAlert(t, m, "wecom", robot.URL, "", "", false, func(channel *gateway.AlertChannel) { channel.FailureThreshold = 2 })
	host := baseHost()
	host.Proxies = []service.ProxyRule{{Path: "/*", To: upstream.URL}}
	host.Monitor = gateway.MonitorConfig{Enabled: true, IntervalSeconds: 60, Path: "/health", ExpectedStatus: 200, Headers: map[string]string{"Authorization": "Bearer monitor-fixture"}, CheckScript: `function(response) { return JSON.parse(response.body).status === "ok"; }`}
	record := savePublish(t, m, host, 0)
	if record.Draft.Monitor.Headers != nil || record.Draft.Monitor.SealedHeaders != "" || !record.Draft.Monitor.HasHeaders {
		t.Fatal("monitor secrets exposed")
	}
	if checks.Load() != 0 {
		t.Fatal("saving configuration probed upstream")
	}
	check := func(want bool) {
		sample, err := m.RunMonitorCheck(context.Background(), host.ID)
		if err != nil {
			t.Fatal(err)
		}
		if sample.OK != want {
			t.Fatalf("sample %+v", sample)
		}
	}
	check(true)
	healthy.Store(false)
	check(false)
	if alerts.Load() != 0 {
		t.Fatal("premature alert")
	}
	check(false)
	check(false)
	if alerts.Load() != 1 {
		t.Fatal("failure alert repeated")
	}
	healthy.Store(true)
	check(true)
	check(true)
	if alerts.Load() != 2 {
		t.Fatal("recovery transition")
	}
	history, err := m.MonitorHistory(host.ID)
	if err != nil || len(history) != 6 {
		t.Fatal(history, err)
	}
	data, _ := file.ReadBytes(filepath.Join(config.DataDir, "state.json"))
	if strings.Contains(string(data), "Bearer monitor-fixture") {
		t.Fatal("monitor header persisted plaintext")
	}
	restarted, err := gateway.NewManager(config, service.NewWebServer())
	if err != nil {
		t.Fatal(err)
	}
	defer restarted.Close()
	history, err = restarted.MonitorHistory(host.ID)
	if err != nil || len(history) != 6 {
		t.Fatal("history not retained")
	}
	statuses, err := restarted.MonitorStatuses()
	if err != nil || !statuses[host.ID].ConfigurationOK || statuses[host.ID].Last == nil || !statuses[host.ID].Last.OK {
		t.Fatal("statuses", statuses, err)
	}
	host = record.Draft
	host.Monitor.Path = "/changed"
	if _, err := m.SaveDraft(host, record.Revision, "test"); err != nil {
		t.Fatal(err)
	}
	statuses, err = m.MonitorStatuses()
	if err != nil || statuses[host.ID].Last != nil {
		t.Fatal("stale result attached to changed draft")
	}
}
func TestMonitorCustomAlertAndProviderFailure(t *testing.T) {
	m, _, config := manager(t)
	var fail atomic.Bool
	backend := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "application/json")
		if fail.Load() {
			w.Write([]byte(`{"errcode":100,"errmsg":"denied"}`))
		} else {
			w.Write([]byte(`{"errcode":0}`))
		}
	}))
	defer backend.Close()
	custom := saveAlert(t, m, "custom", backend.URL, "", `function(event, channel, send) { const result = send({url:channel.webhook, payload:{text:event.state}}); if(result.data.errcode !== 0) return {ok:false}; return result; }`, false)
	stored := func(id string) gateway.AlertChannel {
		data, _ := file.ReadBytes(filepath.Join(config.DataDir, "state.json"))
		state, _ := cast.FromJSON[gateway.State](data)
		return state.AlertChannels[id]
	}
	if err := m.SendAlert(context.Background(), stored(custom.ID), gateway.AlertEvent{State: "test"}); err != nil {
		t.Fatal(err)
	}
	ordinary := saveAlert(t, m, "wecom", backend.URL, "", "", false)
	fail.Store(true)
	if err := m.SendAlert(context.Background(), stored(ordinary.ID), gateway.AlertEvent{State: "test"}); err == nil {
		t.Fatal("HTTP 200 with provider failure marked delivered")
	}
	if err := m.SendAlert(context.Background(), stored(custom.ID), gateway.AlertEvent{State: "test"}); err == nil {
		t.Fatal("custom reply check ignored")
	}
	host := baseHost()
	host.Monitor.CheckScript = `function(response) {`
	if err := gateway.ValidateHost(host); err == nil {
		t.Fatal("invalid script accepted")
	}
}
func TestRegistryReadOnlyIncludesUnreferencedAndExpiredNodes(t *testing.T) {
	if os.Getenv("GATEWAY_REDIS_INTEGRATION") != "1" {
		t.Skip("set GATEWAY_REDIS_INTEGRATION=1")
	}
	rd := redis.GetRedis("127.0.0.1:6379", log.DefaultLogger)
	name := fmt.Sprintf("gateway-registry-test-%d", time.Now().UnixNano())
	rd.Do("HSET", name, "127.0.0.1:18001", 100, "127.0.0.1:18002", 50)
	rd.Do("SETEX", name+"_127.0.0.1:18001", 60, "1")
	t.Cleanup(func() { rd.Do("DEL", name, name+"_127.0.0.1:18001") })
	services, err := gateway.ReadRegistryServices(rd)
	if err != nil {
		t.Fatal(err)
	}
	found := false
	for _, entry := range services {
		if entry.Name == name {
			found = true
			if len(entry.Nodes) != 2 || !entry.Nodes[0].Alive || entry.Nodes[1].Alive {
				t.Fatalf("nodes %+v", entry.Nodes)
			}
		}
	}
	if !found {
		t.Fatal("unreferenced service omitted")
	}
	if rd.Do("HLEN", name).Int() != 2 {
		t.Fatal("reader removed expired registration")
	}
}
func TestMonitorDirectURLAndTickScheduling(t *testing.T) {
	m, _, _ := manager(t)
	var requests atomic.Int32
	backend := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		requests.Add(1)
		if r.URL.RequestURI() != "/check?scope=test" {
			t.Errorf("request path %s", r.URL.RequestURI())
		}
		w.WriteHeader(204)
	}))
	defer backend.Close()
	host := baseHost()
	host.Monitor = gateway.MonitorConfig{Enabled: true, URL: backend.URL + "/check?scope=test", IntervalSeconds: 60, ExpectedStatus: 204}
	savePublish(t, m, host, 0)
	if err := m.MonitorTick(context.Background()); err != nil {
		t.Fatal(err)
	}
	if err := m.MonitorTick(context.Background()); err != nil {
		t.Fatal(err)
	}
	if requests.Load() != 1 {
		t.Fatal("not respecting check interval")
	}
	history, err := m.MonitorHistory(host.ID)
	if err != nil || len(history) != 1 || !history[0].OK || history[0].Status != 204 {
		t.Fatal("direct URL monitor", history, err)
	}
}
func BenchmarkMonitorCheck(b *testing.B) {
	m, err := gateway.NewManager(gateway.Config{DataDir: b.TempDir(), AdminDisabled: true}, service.NewWebServer())
	if err != nil {
		b.Fatal(err)
	}
	defer m.Close()
	upstream := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) { w.Write([]byte("OK")) }))
	defer upstream.Close()
	host := baseHost()
	host.Monitor = gateway.MonitorConfig{Enabled: true, URL: upstream.URL, IntervalSeconds: 60}
	record, err := m.SaveDraft(host, 0, "benchmark")
	if err != nil {
		b.Fatal(err)
	}
	if _, err := m.Publish(host.ID, record.Revision, "benchmark"); err != nil {
		b.Fatal(err)
	}
	b.ResetTimer()
	for b.Loop() {
		if _, err := m.RunMonitorCheck(context.Background(), host.ID); err != nil {
			b.Fatal(err)
		}
	}
}
func TestMonitorMissingEncryptionKeyDoesNotReplaceCredentials(t *testing.T) {
	m, _, config := manager(t)
	saveAlert(t, m, "wecom", "https://example.invalid/secret-fixture", "", "", false)
	m.Close()
	keyPath := filepath.Join(config.DataDir, "certificate.key")
	if err := file.Remove(keyPath); err != nil {
		t.Fatal(err)
	}
	if restarted, err := gateway.NewManager(config, service.NewWebServer()); err == nil {
		restarted.Close()
		t.Fatal("missing credential encryption key was replaced")
	}
	if file.Exists(keyPath) {
		t.Fatal("generated a replacement for missing key")
	}
}
func TestMonitorCustomScriptCancellation(t *testing.T) {
	m, _, config := manager(t)
	public := saveAlert(t, m, "custom", "https://example.invalid/fixture", "", `function(event, channel, send) { while(true) {} }`, false)
	raw, _ := file.ReadBytes(filepath.Join(config.DataDir, "state.json"))
	state, _ := cast.FromJSON[gateway.State](raw)
	ctx, cancel := context.WithCancel(context.Background())
	cancel()
	started := time.Now()
	if err := m.SendAlert(ctx, state.AlertChannels[public.ID], gateway.AlertEvent{}); err == nil {
		t.Fatal("cancelled script reported sent")
	}
	if time.Since(started) > time.Second {
		t.Fatal("cancelled script did not stop promptly")
	}
}
func TestMonitorCorruptHistoryDoesNotBlockOtherHosts(t *testing.T) {
	m, _, config := manager(t)
	var requests atomic.Int32
	backend := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) { requests.Add(1) }))
	defer backend.Close()
	first := baseHost()
	first.ID = "broken"
	first.Monitor = gateway.MonitorConfig{Enabled: true, URL: backend.URL}
	savePublish(t, m, first, 0)
	second := baseHost()
	second.ID = "healthy"
	second.Domains = []string{"healthy.test"}
	second.Monitor = first.Monitor
	savePublish(t, m, second, 0)
	path := filepath.Join(config.DataDir, "monitor", goCrypto.SHA256ToHex([]byte(first.ID))+".json")
	if err := file.Write(path, "invalid-json"); err != nil {
		t.Fatal(err)
	}
	if err := m.MonitorTick(context.Background()); err == nil {
		t.Fatal("corrupt history was silently accepted")
	}
	if requests.Load() != 1 {
		t.Fatal("healthy Host was blocked")
	}
	history, err := m.MonitorHistory(second.ID)
	if err != nil || len(history) != 1 {
		t.Fatal(history, err)
	}
	status, err := m.MonitorStatuses()
	if err != nil || !status[first.ID].HistoryError || status[second.ID].Last == nil {
		t.Fatal("history failure isolation", status, err)
	}
}

func TestMonitorClusterLeaseAndSharedHistory(t *testing.T) {
	if os.Getenv("GATEWAY_REDIS_INTEGRATION") != "1" {
		t.Skip("set GATEWAY_REDIS_INTEGRATION=1")
	}
	rd := redis.GetRedis("127.0.0.1:6379", log.DefaultLogger)
	prefix := fmt.Sprintf("gateway-monitor-test-%d", time.Now().UnixNano())
	t.Cleanup(func() {
		for _, key := range rd.Do("KEYS", prefix+":*").Strings() {
			rd.Do("DEL", key)
		}
	})
	config := gateway.Config{DataDir: t.TempDir(), Redis: "127.0.0.1:6379", Prefix: prefix, AdminDisabled: true, NodeID: "one"}
	secret := safe.NewSafeBufAndErase([]byte("gateway-test-password"))
	defer secret.Close()
	if err := gateway.SetAdminPassword(config, secret); err != nil {
		t.Fatal(err)
	}
	one, err := gateway.NewManager(config, service.NewWebServer())
	if err != nil {
		t.Fatal(err)
	}
	defer one.Close()
	config.DataDir = t.TempDir()
	config.NodeID = "two"
	two, err := gateway.NewManager(config, service.NewWebServer())
	if err != nil {
		t.Fatal(err)
	}
	defer two.Close()
	var requests atomic.Int32
	backend := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) { requests.Add(1) }))
	defer backend.Close()
	host := baseHost()
	host.Monitor = gateway.MonitorConfig{Enabled: true, URL: backend.URL}
	savePublish(t, one, host, 0)
	if _, err := one.RunMonitorCheck(context.Background(), host.ID); err != nil {
		t.Fatal(err)
	}
	if _, err := two.RunMonitorCheck(context.Background(), host.ID); err == nil {
		t.Fatal("second node bypassed lease")
	}
	history, err := two.MonitorHistory(host.ID)
	if err != nil || len(history) != 1 || requests.Load() != 1 {
		t.Fatal("shared monitoring", history, err)
	}
	busy := host
	busy.ID = "busy-monitor"
	busy.Domains = []string{"busy.test"}
	savePublish(t, one, busy, 0)
	rd.Do("SET", prefix+":monitor-lease:"+busy.ID, "other", "EX", 45)
	if err := one.MonitorTick(context.Background()); err != nil {
		t.Fatal(err)
	}
	if requests.Load() != 1 {
		t.Fatal("busy lease was bypassed")
	}
	rd.Do("DEL", prefix+":monitor-lease:"+busy.ID)
	if err := one.MonitorTick(context.Background()); err != nil {
		t.Fatal(err)
	}
	if requests.Load() != 2 {
		t.Fatal("lease contention delayed the next tick by a full interval")
	}
}
func TestMonitorGatewayAppSchedulesAndStops(t *testing.T) {
	oldServer, oldConfig := service.DefaultServer, gateway.GatewayConf
	t.Cleanup(func() { service.DefaultServer = oldServer; gateway.GatewayConf = oldConfig })
	config := gateway.Config{DataDir: t.TempDir(), AdminDisabled: true}
	server := service.NewWebServer()
	server.Config.Listen = "127.0.0.1:0,http"
	server.Config.AutoCertDir = filepath.Join(config.DataDir, "ssl")
	server.Config.NoLogGets = true
	observed := make(chan struct{}, 1)
	backend := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		select {
		case observed <- struct{}{}:
		default:
		}
	}))
	defer backend.Close()
	manager, err := gateway.NewManager(config, server)
	if err != nil {
		t.Fatal(err)
	}
	host := baseHost()
	host.Monitor = gateway.MonitorConfig{Enabled: true, URL: backend.URL}
	savePublish(t, manager, host, 0)
	manager.Close()
	service.DefaultServer = server
	gateway.GatewayConf = config
	app := gateway.NewGatewayApp()
	if err := app.Start(context.Background(), log.DefaultLogger); err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() {
		ctx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
		defer cancel()
		app.Stop(ctx)
	})
	ctx, cancel := context.WithTimeout(context.Background(), 12*time.Second)
	defer cancel()
	select {
	case <-observed:
	case <-ctx.Done():
		t.Fatal("scheduled check never ran")
	}
	if err := timer.Retry(func() error {
		history, err := app.Manager().MonitorHistory(host.ID)
		if err != nil {
			return err
		}
		if len(history) == 0 {
			return fmt.Errorf("waiting for completed check")
		}
		return nil
	}, timer.WithMaxRetries(200), timer.WithBackoff(5*time.Millisecond, 1)); err != nil {
		t.Fatal(err)
	}
	if err := app.Stop(ctx); err != nil {
		t.Fatal(err)
	}
	history, err := app.Manager().MonitorHistory(host.ID)
	if err != nil || len(history) != 1 || !history[0].OK {
		t.Fatal("scheduled history", history, err)
	}
}
