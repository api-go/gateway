package gateway_test

import (
	"apigo.cc/go/cast"
	goCrypto "apigo.cc/go/crypto"
	"apigo.cc/go/file"
	gateway "apigo.cc/go/gateway/core"
	"apigo.cc/go/log"
	"apigo.cc/go/redis"
	"apigo.cc/go/service"
	"context"
	"fmt"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"
)

func TestCompactMonitorCapsAndStickyFailure(t *testing.T) {
	m, _, config := manager(t)
	status := 503
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) { w.WriteHeader(status); fmt.Fprint(w, status) }))
	defer server.Close()
	host := baseHost()
	host.Proxies = []service.ProxyRule{{Path: "/*", To: server.URL}}
	savePublish(t, m, host, 0)
	// Legacy 35-day migration must fold daily counts and keep only 30 days/24 hours.
	legacy := []gateway.MonitorBucket{}
	for i := 0; i < 35*24; i++ {
		legacy = append(legacy, gateway.MonitorBucket{Time: time.Now().Truncate(time.Hour).Add(-time.Duration(i) * time.Hour), Checks: 2, Successes: 2, DurationTotalMS: 10})
	}
	// Legacy reader supports its original 720-hour maximum.
	legacy = legacy[:720]
	raw, _ := cast.ToJSONBytes(legacy)
	path := filepath.Join(config.DataDir, "monitor-hours", goCrypto.SHA256ToHex([]byte(host.ID))+".json")
	if err := file.WriteBytes(path, raw); err != nil {
		t.Fatal(err)
	}
	for i := 0; i < 15; i++ {
		if i == 1 {
			status = 200
		}
		if _, err := m.RunMonitorCheck(context.Background(), host.ID); err != nil {
			t.Fatal(err)
		}
	}
	history, err := m.MonitorHistory(host.ID)
	if err != nil || len(history) != 10 {
		t.Fatal("recent ring not capped", len(history), err)
	}
	rows, err := m.MonitorMatrix()
	if err != nil || len(rows[0].Days) > 30 || len(rows[0].Buckets) > 24 || len(rows[0].Days) < 29 {
		t.Fatal("period retention", rows, err)
	}
	current := rows[0].Buckets[len(rows[0].Buckets)-1]
	if current.Errors != 1 || current.LegacyErrors || current.Output == nil || current.Output.Status != 503 || current.Output.Body != "503" {
		t.Fatal("failed response/count not retained", current)
	}
	if current.Error == "" || current.Checks != 17 {
		t.Fatal("success cleared prior period failure", current)
	}
	raw, err = file.ReadBytes(filepath.Join(config.DataDir, "monitor-compact", goCrypto.SHA256ToHex([]byte(host.ID))+".json"))
	if err != nil {
		t.Fatal(err)
	}
	fields, _ := cast.FromJSON[map[string]string](raw)
	if len(fields) > 64 {
		t.Fatal("too many fields", len(fields))
	}
	for field, value := range fields {
		if strings.HasPrefix(field, "d:") || strings.HasPrefix(field, "h:") {
			counter, _ := cast.FromJSON[map[string]any]([]byte(value))
			if len(counter) != 6 || counter["total"] == nil || counter["times"] == nil || counter["error"] == nil {
				t.Fatal("unexpected counter schema", counter)
			}
		}
	}
	if file.Exists(path) {
		t.Fatal("legacy hourly artifact not removed")
	}
}
func TestRedisCompactRingAndAtomicCounters(t *testing.T) {
	if os.Getenv("GATEWAY_REDIS_INTEGRATION") != "1" {
		t.Skip("set GATEWAY_REDIS_INTEGRATION=1")
	}
	prefix := fmt.Sprintf("gateway-compact-test-%d", time.Now().UnixNano())
	rd := redis.GetRedis("127.0.0.1:6379", log.DefaultLogger)
	t.Cleanup(func() {
		for _, key := range rd.Do("KEYS", prefix+":*").Strings() {
			rd.Do("DEL", key)
		}
	})
	m, err := gateway.NewManager(gateway.Config{Redis: "127.0.0.1:6379", Prefix: prefix, DataDir: t.TempDir(), AdminDisabled: true}, service.NewWebServer())
	if err != nil {
		t.Fatal(err)
	}
	defer m.Close()
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) { w.WriteHeader(500) }))
	defer server.Close()
	host := baseHost()
	host.Proxies = []service.ProxyRule{{Path: "/*", To: server.URL}}
	savePublish(t, m, host, 0)
	for i := 0; i < 15; i++ {
		rd.Do("DEL", prefix+":monitor-lease:"+host.ID)
		if _, err := m.RunMonitorCheck(context.Background(), host.ID); err != nil {
			t.Fatal(err)
		}
	}
	fields := rd.Do("HGETALL", prefix+":monitor:"+host.ID).StringMap()
	if len(fields) != 12 {
		t.Fatal("expected 1 day + 1 hour + 10 recent fields", len(fields))
	}
	history, err := m.MonitorHistory(host.ID)
	if err != nil || len(history) != 10 {
		t.Fatal(err, len(history))
	}
	rows, err := m.MonitorMatrix()
	if err != nil || rows[0].Buckets[0].Checks != 15 || rows[0].Days[0].Checks != 15 || rows[0].Days[0].Errors != 15 || rows[0].Buckets[0].Errors != 15 {
		t.Fatal("lost counter increments", rows, err)
	}
	if history[len(history)-1].Failures != 15 {
		t.Fatal("streak counter lost when ring wrapped", history[len(history)-1].Failures)
	}

}

func TestMonitorPeriodKeepsLatestSuccess(t *testing.T) {
	m, _, _ := manager(t)
	count := 0
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) { count++; fmt.Fprint(w, count) }))
	defer server.Close()
	host := baseHost()
	host.Proxies = []service.ProxyRule{{Path: "/*", To: server.URL}}
	savePublish(t, m, host, 0)
	for i := 0; i < 2; i++ {
		if _, err := m.RunMonitorCheck(context.Background(), host.ID); err != nil {
			t.Fatal(err)
		}
	}
	rows, err := m.MonitorMatrix()
	if err != nil {
		t.Fatal(err)
	}
	for _, bucket := range []gateway.MonitorBucket{rows[0].Buckets[len(rows[0].Buckets)-1], rows[0].Days[len(rows[0].Days)-1]} {
		if bucket.Checks != 2 || bucket.Errors != 0 || bucket.Error != "" || bucket.Output == nil || bucket.Output.Body != "2" {
			t.Fatal("latest success not retained", bucket)
		}
	}
}
