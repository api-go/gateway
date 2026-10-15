package gateway_test

import (
	"apigo.cc/go/cast"
	"apigo.cc/go/discover"
	"apigo.cc/go/file"
	gateway "apigo.cc/go/gateway/core"
	"apigo.cc/go/log"
	"apigo.cc/go/redis"
	"apigo.cc/go/service"
	"apigo.cc/go/timer"
	"context"
	"fmt"
	"io"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"sync/atomic"
	"testing"
	"time"
)

func TestDefaultMonitorAllDomainsAndHourlyHistory(t *testing.T) {
	m, _, config := manager(t)
	var first, second atomic.Int32
	upstream := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.Host == "first.test" {
			first.Add(1)
		} else if r.Host == "second.test" {
			second.Add(1)
			w.WriteHeader(503)
		} else {
			t.Error("wrong domain", r.Host)
		}
	}))
	defer upstream.Close()
	host := baseHost()
	host.Domains = []string{"first.test", "second.test"}
	host.Proxies = []service.ProxyRule{{Path: "/*", To: upstream.URL}}
	record := savePublish(t, m, host, 0)
	if err := m.MonitorTick(context.Background()); err != nil {
		t.Fatal(err)
	}
	if first.Load() != 1 || second.Load() != 1 {
		t.Fatal("not every domain checked")
	}
	history, err := m.MonitorHistory(host.ID)
	if err != nil || len(history) != 1 || history[0].OK || len(history[0].Domains) != 2 || history[0].Domains[1].Status != 503 {
		t.Fatal("domain failure not aggregated", history, err)
	}
	rows, err := m.MonitorMatrix()
	if err != nil || len(rows) != 1 || !rows[0].Enabled || len(rows[0].Buckets) != 1 || rows[0].Buckets[0].Checks != 1 || rows[0].Buckets[0].Successes != 0 {
		t.Fatal("matrix", rows, err)
	}
	restarted, err := gateway.NewManager(config, service.NewWebServer())
	if err != nil {
		t.Fatal(err)
	}
	defer restarted.Close()
	rows, err = restarted.MonitorMatrix()
	if err != nil || len(rows[0].Buckets) != 1 {
		t.Fatal("hourly history lost", err)
	}
	host = record.Draft
	host.Monitor.Configured = true
	host.Monitor.Enabled = false
	record = savePublish(t, m, host, record.Revision)
	if err := m.MonitorTick(context.Background()); err != nil {
		t.Fatal(err)
	}
	if first.Load() != 1 {
		t.Fatal("disabled monitor still ran")
	}
	host = record.Draft
	host.Monitor.Enabled = true
	host.Monitor.Domain = "first.test"
	record = savePublish(t, m, host, record.Revision)
	sample, err := m.RunMonitorCheck(context.Background(), host.ID)
	if err != nil || !sample.OK || len(sample.Domains) != 1 || second.Load() != 1 {
		t.Fatal("explicit domain ignored", sample, err)
	}
	// Active compact storage has distinct daily and hourly data, not 720 hourly bins.
	rows, err = m.MonitorMatrix()
	if err != nil || len(rows[0].Days) != 1 || len(rows[0].Buckets) != 1 {
		t.Fatal("compact day/hour statistics missing", rows, err)
	}
	files, err := file.ReadDir(filepath.Join(config.DataDir, "monitor-compact"))
	if err != nil || len(files) != 1 {
		t.Fatal(err)
	}
	raw, err := file.ReadBytes(files[0].FullName)
	if err != nil {
		t.Fatal(err)
	}
	fields, err := cast.FromJSON[map[string]string](raw)
	if err != nil || len(fields) > 64 {
		t.Fatal("compact field bound", err)
	}

}
func TestGatewayDiscoveryClientAndImmediateMonitor(t *testing.T) {
	if os.Getenv("GATEWAY_REDIS_INTEGRATION") != "1" {
		t.Skip("set GATEWAY_REDIS_INTEGRATION=1")
	}
	prefix := fmt.Sprintf("gateway-client-test-%d", time.Now().UnixNano())
	registry := "redis://127.0.0.1:6379/15" // same registry as the native service integration suite; isolate by unique keys
	rd := redis.GetRedis(registry, log.DefaultLogger)
	previousServer, previousGateway, previousDiscover := service.DefaultServer, gateway.GatewayConf, discover.Config
	t.Cleanup(func() {
		discover.Stop()
		service.DefaultServer = previousServer
		gateway.GatewayConf = previousGateway
		discover.Config = previousDiscover
		for _, key := range rd.Do("KEYS", prefix+"*").Strings() {
			rd.Do("DEL", key)
		}
	})
	var calls atomic.Int32
	upstream := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) { calls.Add(1); io.WriteString(w, "OK") }))
	defer upstream.Close()
	upstreamName := prefix + "-upstream"
	address := upstream.Listener.Addr().String()
	rd.Do("HSET", upstreamName, address, 100)
	rd.Do("SET", upstreamName+"_"+address, "1", "EX", 60)
	config := gateway.Config{DataDir: t.TempDir(), Prefix: prefix, AdminDisabled: true}
	seed, err := gateway.NewManager(config, service.NewWebServer())
	if err != nil {
		t.Fatal(err)
	}
	host := baseHost()
	host.Proxies = []service.ProxyRule{{Path: "/*", To: upstreamName}}
	savePublish(t, seed, host, 0)
	seed.Close()
	service.DefaultServer = service.NewWebServer()
	service.DefaultServer.Config.Listen = "127.0.0.1:0,http"
	service.DefaultServer.Config.App = prefix + "-self"
	service.DefaultServer.Config.Register = registry
	service.DefaultServer.Config.Calls = map[string]service.CallConfig{upstreamName: {Http2: false}}
	gateway.GatewayConf = config
	app := gateway.NewGatewayApp()
	if err := app.Start(context.Background(), log.DefaultLogger); err != nil {
		t.Fatal(err)
	}
	defer app.Stop(context.Background())
	if rd.Do("HLEN", service.DefaultServer.Config.App).Int() != 0 {
		t.Fatal("gateway registered itself")
	}
	if err := timer.Retry(func() error {
		if calls.Load() > 0 {
			return nil
		}
		return fmt.Errorf("not started")
	}, timer.WithMaxRetries(100), timer.WithBackoff(10*time.Millisecond, 1)); err != nil {
		t.Fatal("startup monitor did not call discovered HTTP upstream immediately")
	}
	if err := timer.Retry(func() error {
		history, _ := app.Manager().MonitorHistory(host.ID)
		if len(history) > 0 && history[0].OK {
			return nil
		}
		return fmt.Errorf("not recorded")
	}, timer.WithMaxRetries(100), timer.WithBackoff(10*time.Millisecond, 1)); err != nil {
		t.Fatal("startup history missing")
	}
	entries, err := app.Manager().RegistryServices()
	if err != nil {
		t.Fatal(err)
	}
	found := false
	for _, entry := range entries {
		if entry.Name == upstreamName {
			found = true
			if entry.Protocol != "HTTP" {
				t.Fatal("HTTP protocol misreported", entry.Protocol)
			}
		}
		if entry.Name == service.DefaultServer.Config.App {
			t.Fatal("self visible in registry")
		}
	}
	if !found {
		t.Fatal("upstream discovery lost")
	}
	missing := baseHost()
	missing.ID = "missing-service-host"
	missing.Domains = []string{"missing.test"}
	missing.Proxies = []service.ProxyRule{{Path: "/*", To: prefix + "-missing"}}
	savePublish(t, app.Manager(), missing, 0)
	failed, err := app.Manager().RunMonitorCheck(context.Background(), missing.ID)
	if err != nil || failed.Failure != "service_unregistered" {
		t.Fatal("missing registration not classified", failed, err)
	}

}
