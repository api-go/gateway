package gateway_test

import (
	"apigo.cc/go/cast"
	"apigo.cc/go/gateway/core"
	"apigo.cc/go/log"
	"apigo.cc/go/redis"
	"apigo.cc/go/service"
	"apigo.cc/go/shell"
	"apigo.cc/go/timer"
	"fmt"
	"net"
	"net/http/httptest"
	"os"
	"testing"
	"time"
)

// This opt-in test starts and stops its own Redis on an ephemeral localhost port.
// It never shuts down the user's Redis instance.
func TestRedisOutageRetainsTrafficAndRecovers(t *testing.T) {
	if os.Getenv("GATEWAY_REDIS_PROCESS_INTEGRATION") != "1" {
		t.Skip("set GATEWAY_REDIS_PROCESS_INTEGRATION=1; requires redis-server")
	}
	listener, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatal(err)
	}
	address := listener.Addr().String()
	_, port, _ := net.SplitHostPort(address)
	listener.Close()
	directory := t.TempDir()
	var process *shell.Process
	start := func() {
		var err error
		process, err = shell.Start("redis-server", []string{"--bind", "127.0.0.1", "--port", port, "--save", "", "--appendonly", "no", "--dir", directory, "--dbfilename", "go-gateway-test.rdb"}, nil)
		if err != nil {
			t.Fatal(err)
		}
		if err := timer.Retry(func() error {
			connection, err := net.DialTimeout("tcp", address, 100*time.Millisecond)
			if err != nil {
				return err
			}
			return connection.Close()
		}, timer.WithMaxRetries(50), timer.WithBackoff(20*time.Millisecond, 1)); err != nil {
			t.Fatal(err)
		}
	}
	defer func() {
		if process != nil {
			process.Kill()
		}
	}()
	start()
	setting := "redis://" + address + "?connectTimeout=100ms&readTimeout=100ms&writeTimeout=100ms"
	rd := redis.GetRedis(setting, log.DefaultLogger)
	config := gateway.Config{DataDir: t.TempDir(), Redis: setting, Prefix: "gateway-outage-test", AdminDisabled: true}
	first, err := gateway.NewManager(config, service.NewWebServer())
	if err != nil {
		t.Fatal(err)
	}
	defer first.Close()
	host := baseHost()
	host.Rewrites = []service.RewriteRule{{Path: "/", To: "https://retained.test/"}}
	record := savePublish(t, first, host, 0)
	quota := host
	quota.ID = "quota"
	quota.Domains = []string{"quota.test"}
	quota.Limits = []gateway.LimitRule{{ID: "shared", Enabled: true, Scope: "cluster", Requests: 1, PeriodSeconds: 60}}
	savePublish(t, first, quota, 0)
	if result := rd.Do("SAVE"); result.Error != nil {
		t.Fatal(result.Error)
	}
	if err := process.Kill(); err != nil {
		t.Fatal(err)
	}
	process = nil
	if err := first.Reconcile(); err == nil {
		t.Fatal("offline Redis accepted")
	}
	retained, err := gateway.NewManager(config, service.NewWebServer())
	if err != nil {
		t.Fatal(err)
	}
	defer retained.Close()
	for _, manager := range []*gateway.Manager{first, retained} {
		for domain, expected := range map[string]int{"example.test": 302, "quota.test": 503} {
			response := httptest.NewRecorder()
			manager.Handler().ServeHTTP(response, httptest.NewRequest("GET", "http://"+domain+"/", nil))
			if response.Code != expected {
				t.Fatalf("%s during outage: %d", domain, response.Code)
			}
		}
		if _, err := manager.SaveDraft(host, record.Revision, "test"); err == nil {
			t.Fatal("offline write accepted")
		}
		response := adminRequest(t, manager.AdminHandler(), "/api/login", map[string]string{"password": "test-password"}, nil, "")
		if response.Code != 503 {
			t.Fatalf("offline credential check: %d", response.Code)
		}
	}
	start()
	if err := timer.Retry(func() error { return first.Reconcile() }, timer.WithMaxRetries(20), timer.WithBackoff(20*time.Millisecond, 1)); err != nil {
		t.Fatal(err)
	}
	if err := retained.Reconcile(); err != nil {
		t.Fatal(err)
	}
	after, err := first.SaveDraft(host, record.Revision, "test")
	if err != nil || after.Revision <= record.Revision {
		t.Fatal("write did not recover", err)
	}
	for index, manager := range []*gateway.Manager{first, retained} {
		response := httptest.NewRecorder()
		manager.Handler().ServeHTTP(response, httptest.NewRequest("GET", "http://quota.test/", nil))
		expected := 302
		if index == 1 {
			expected = 429
		}
		if response.Code != expected {
			t.Fatal(fmt.Sprintf("quota after recovery %s expected %s", cast.String(response.Code), cast.String(expected)))
		}
	}
}
