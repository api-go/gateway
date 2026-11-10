package gateway_test

import (
	"context"
	"fmt"
	"io"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"slices"
	"sync/atomic"
	"testing"
	"time"

	"github.com/api-go/cast"
	goCrypto "github.com/api-go/crypto"
	"github.com/api-go/file"
	gateway "github.com/api-go/gateway/core"
	"github.com/api-go/log"
	"github.com/api-go/redis"
	"github.com/api-go/safe"
	"github.com/api-go/service"
)

func updateAlert(t *testing.T, m *gateway.Manager, channel gateway.AlertChannel) gateway.AlertChannel {
	t.Helper()
	h := m.AdminHandler()
	cookie, csrf := login(t, h, "admin", "gateway-test-password")
	listing := adminRequest(t, h, "/api/alerts", nil, cookie, csrf)
	state, _ := cast.FromJSON[map[string]any](listing.Body.Bytes())
	response := adminRequest(t, h, "/api/alerts/save", map[string]any{"revision": state["revision"], "channel": channel}, cookie, csrf)
	if response.Code != 200 {
		t.Fatal(response.Body.String())
	}
	result, _ := cast.FromJSON[struct{ Channel gateway.AlertChannel }](response.Body.Bytes())
	return result.Channel
}

func TestAlertUsesMonitorStreakAcrossPolicyChanges(t *testing.T) {
	m, _, config := manager(t)
	var calls atomic.Int32
	robot := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) { calls.Add(1); io.WriteString(w, `{"errcode":0}`) }))
	defer robot.Close()
	channel := saveAlert(t, m, "wecom", robot.URL, "", "", false, func(c *gateway.AlertChannel) { c.Enabled = false })
	status := 503
	upstream := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) { w.WriteHeader(status) }))
	defer upstream.Close()
	host := baseHost()
	host.Proxies = []service.ProxyRule{{Path: "/*", To: upstream.URL}}
	record := savePublish(t, m, host, 0)
	check := func(want int) gateway.MonitorSample {
		t.Helper()
		sample, err := m.RunMonitorCheck(context.Background(), host.ID)
		if err != nil || sample.Failures != want {
			t.Fatalf("streak=%d want=%d error=%v", sample.Failures, want, err)
		}
		return sample
	}
	check(1)
	check(2)
	// Editing only alert selection, threshold and cadence must not change the probe.
	host = record.Draft
	host.Monitor.Configured = true
	host.Monitor.ChannelIDs = []string{channel.ID}
	host.Monitor.FailureThreshold = 20
	host.Monitor.IntervalSeconds = 120
	record = savePublish(t, m, host, record.Revision)
	check(3)
	if calls.Load() != 0 {
		t.Fatal("disabled channel sent")
	}
	channel.Enabled = true
	channel = updateAlert(t, m, channel)
	sample := check(4)
	if calls.Load() != 1 {
		t.Fatal("enabling a channel restarted the monitor streak")
	}
	check(5)
	if calls.Load() != 1 {
		t.Fatal("duplicate fault notification")
	}
	rows, err := m.MonitorMatrix()
	if err != nil {
		t.Fatal(err)
	}
	if !slices.Equal(rows[0].Buckets[0].Recent, []bool{false, false, false, false, false}) {
		t.Fatal("dots differ from alarm samples")
	}
	// Simulate a persisted sample produced by the previous fingerprint algorithm.
	legacy := *m.Hosts()[0].Current
	legacy.Name = ""
	legacy.HTTPS.SealedKey = ""
	legacy.Monitor.Headers = nil
	legacy.Monitor.ClearHeaders = false
	legacy.Monitor.HasHeaders = false
	legacyRaw, _ := cast.ToJSONBytes(legacy)
	sample.Fingerprint = goCrypto.SHA256ToHex(legacyRaw)
	// The disabled delivery state is irrelevant here: assert the streak survives upgrade.
	path := filepath.Join(config.DataDir, "monitor-compact", goCrypto.SHA256ToHex([]byte(host.ID))+".json")
	raw, err := file.ReadBytes(path)
	if err != nil {
		t.Fatal(err)
	}
	fields, err := cast.FromJSON[map[string]string](raw)
	if err != nil {
		t.Fatal(err)
	}
	latestField := ""
	var at int64
	for field, raw := range fields {
		if len(field) > 2 && field[:2] == "r:" {
			entry, _ := cast.FromJSON[struct{ At int64 }](raw)
			if entry.At >= at {
				at = entry.At
				latestField = field
			}
		}
	}
	sample.Failures = 5
	entry, _ := cast.ToJSON(map[string]any{"at": at, "sample": sample})
	fields[latestField] = entry
	raw, _ = cast.ToJSONBytes(fields)
	if err := file.WriteBytes(path, raw); err != nil {
		t.Fatal(err)
	}
	check(6)
	// Changing the actual probe resets both the streak and recent per-period dots.
	host.Monitor.Path = "/changed"
	savePublish(t, m, host, record.Revision)
	check(1)
	rows, err = m.MonitorMatrix()
	if err != nil {
		t.Fatal(err)
	}
	for _, bucket := range []gateway.MonitorBucket{rows[0].Buckets[0], rows[0].Days[0]} {
		if !slices.Equal(bucket.Recent, []bool{false}) || bucket.Checks != 7 {
			t.Fatalf("probe change retained old dots or lost totals: %+v", bucket)
		}
	}
	if calls.Load() != 1 {
		t.Fatal("new probe alerted before threshold")
	}
	status = 200
	check(0)

}

func TestAlertHostScopeDefaultsAndSelection(t *testing.T) {
	m, _, _ := manager(t)
	var calls atomic.Int32
	robot := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) { calls.Add(1); io.WriteString(w, `{"errcode":0}`) }))
	defer robot.Close()
	// This channel predates both Hosts; an omitted scope includes future Hosts.
	channel := saveAlert(t, m, "wecom", robot.URL, "", "", false)
	if channel.HostMode != "all" || channel.FailureThreshold != 3 {
		t.Fatal("wrong defaults", channel)
	}
	upstream := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) { w.WriteHeader(503) }))
	defer upstream.Close()
	for i := range 2 {
		host := baseHost()
		host.ID = fmt.Sprintf("scoped-%d", i)
		host.Domains = []string{host.ID + ".test"}
		host.Proxies = []service.ProxyRule{{Path: "/*", To: upstream.URL}}
		savePublish(t, m, host, 0)
		for n := range 3 {
			sample, err := m.RunMonitorCheck(context.Background(), host.ID)
			if err != nil || sample.Failures != n+1 {
				t.Fatal(sample, err)
			}
		}
	}
	if calls.Load() != 2 {
		t.Fatal("default channel did not cover both Hosts", calls.Load())
	}
	channel.HostMode = "selected"
	channel.HostIDs = []string{"scoped-0"}
	channel = updateAlert(t, m, channel)
	for _, id := range []string{"scoped-0", "scoped-1"} {
		sample, err := m.RunMonitorCheck(context.Background(), id)
		if err != nil || sample.Failures != 4 {
			t.Fatal(sample, err)
		}
	}
	if calls.Load() != 3 {
		t.Fatal("selected scope not enforced", calls.Load())
	}
	channel.HostIDs = nil
	updateAlert(t, m, channel)
	for _, id := range []string{"scoped-0", "scoped-1"} {
		if _, err := m.RunMonitorCheck(context.Background(), id); err != nil {
			t.Fatal(err)
		}
	}
	if calls.Load() != 3 {
		t.Fatal("empty selected scope silently enabled all Hosts")
	}
}

func TestRedisAlertUsesThirdSharedSample(t *testing.T) {
	if os.Getenv("GATEWAY_REDIS_INTEGRATION") != "1" {
		t.Skip("requires isolated Redis")
	}
	config := gateway.Config{DataDir: t.TempDir(), Redis: "127.0.0.1:6379", Prefix: fmt.Sprintf("gateway-alert-streak-%d", time.Now().UnixNano()), AdminDisabled: true, NodeID: "one"}
	rd := redis.GetRedis(config.Redis, log.DefaultLogger)
	t.Cleanup(func() {
		for _, key := range rd.Do("KEYS", config.Prefix+":*").Strings() {
			rd.Do("DEL", key)
		}
	})
	secret := safe.NewSafeBufAndErase([]byte("gateway-test-password"))
	defer secret.Close()
	if err := gateway.SetAdminPassword(config, secret); err != nil {
		t.Fatal(err)
	}
	first, err := gateway.NewManager(config, service.NewWebServer())
	if err != nil {
		t.Fatal(err)
	}
	defer first.Close()
	config.DataDir = t.TempDir()
	config.NodeID = "two"
	second, err := gateway.NewManager(config, service.NewWebServer())
	if err != nil {
		t.Fatal(err)
	}
	defer second.Close()
	var calls atomic.Int32
	robot := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) { calls.Add(1); io.WriteString(w, `{"errcode":0}`) }))
	defer robot.Close()
	channel := saveAlert(t, first, "wecom", robot.URL, "", "", false)
	upstream := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) { w.WriteHeader(503) }))
	defer upstream.Close()
	host := baseHost()
	host.Proxies = []service.ProxyRule{{Path: "/*", To: upstream.URL}}
	record := savePublish(t, first, host, 0)
	for i, m := range []*gateway.Manager{first, second, first, second} {
		if i == 2 {
			channel.HostMode = "selected"
			channel.HostIDs = []string{host.ID}
			updateAlert(t, second, channel)
			host = record.Draft
			host.Monitor.IntervalSeconds = 120
			host.Monitor.Configured = true
			savePublish(t, first, host, record.Revision)
		}
		rd.Do("DEL", config.Prefix+":monitor-lease:"+host.ID)
		sample, err := m.RunMonitorCheck(context.Background(), host.ID)
		if err != nil || sample.Failures != i+1 {
			t.Fatal(sample, err)
		}
		want := int32(0)
		if i >= 2 {
			want = 1
		}
		if calls.Load() != want {
			t.Fatalf("check %d: deliveries=%d want=%d", i+1, calls.Load(), want)
		}
	}
	for _, m := range []*gateway.Manager{first, second} {
		rows, err := m.MonitorMatrix()
		if err != nil {
			t.Fatal(err)
		}
		if !slices.Equal(rows[0].Buckets[0].Recent, []bool{false, false, false, false}) || rows[0].Last.Failures != 4 {
			t.Fatal("matrix and alert streak disagree", rows)
		}
	}
}
