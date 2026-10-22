package gateway_test

import (
	"apigo.cc/go/discover"
	gateway "apigo.cc/go/gateway/core"
	"apigo.cc/go/log"
	"apigo.cc/go/redis"
	"apigo.cc/go/service"
	"context"
	"fmt"
	"github.com/gorilla/websocket"
	"net/http"
	"net/http/httptest"
	"os"
	"strings"
	"sync/atomic"
	"testing"
	"time"
)

var affinitySequence atomic.Uint64

func affinityUpstreams(t *testing.T) string {
	t.Helper()
	discover.Init()
	app := fmt.Sprintf("gateway-affinity-%d", affinitySequence.Add(1))
	for _, name := range []string{"one", "two", "three"} {
		server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
			if websocket.IsWebSocketUpgrade(r) {
				upgrader := websocket.Upgrader{CheckOrigin: func(*http.Request) bool { return true }}
				conn, err := upgrader.Upgrade(w, r, nil)
				if err != nil {
					return
				}
				defer conn.Close()
				conn.WriteMessage(websocket.TextMessage, []byte(name))
				return
			}
			fmt.Fprint(w, name)
		}))
		t.Cleanup(server.Close)
		discover.AddExternalApp(app, "1")
		address := strings.TrimPrefix(server.URL, "http://")
		affinityFixtureNode(app, address, 100)
		t.Cleanup(func() { affinityFixtureNode(app, address, 0) })
	}
	return app
}

// Long workloads must register fixture heartbeats too: Discover periodically
// refreshes its node view from Redis, just as it does in production.
func affinityFixtureNode(app, address string, weight int) {
	if os.Getenv("GATEWAY_AFFINITY_RELIABILITY") == "1" {
		rd := redis.GetRedis("redis://127.0.0.1:6379/15", log.DefaultLogger)
		if weight > 0 {
			rd.Do("HSET", app, address, weight)
			rd.Do("SETEX", app+"_"+address, 3600, "1")
		} else {
			rd.Do("HDEL", app, address)
			rd.Do("DEL", app+"_"+address)
		}
	}
	discover.SetNode(app, address, weight)
}

func affinityRequestFor(t *testing.T, manager *gateway.Manager, path, source, key string) *httptest.ResponseRecorder {
	t.Helper()
	request := httptest.NewRequest("GET", "http://example.test"+path, nil)
	if source == "header" || source == "session" {
		request.Header.Set("sessionid", key)
	}
	if source == "cookie" {
		request.AddCookie(&http.Cookie{Name: "sessionid", Value: key})
	}
	recorder := httptest.NewRecorder()
	manager.Handler().ServeHTTP(recorder, request)
	return recorder
}
func TestAffinityAcrossManagersAndSources(t *testing.T) {
	if os.Getenv("GATEWAY_REDIS_INTEGRATION") != "1" {
		t.Skip("requires Redis-backed Service lifecycle")
	}
	first, server1, _ := manager(t)
	second, server2, _ := manager(t)
	startAffinityServer(t, server1)
	startAffinityServer(t, server2)
	app := affinityUpstreams(t)
	for _, source := range []string{"header", "cookie", "query", "session", "ip"} {
		t.Run(source, func(t *testing.T) {
			host := baseHost()
			host.Proxies = []service.ProxyRule{{Path: "/*", To: app + "/"}}
			host.Affinity = gateway.AffinityConfig{Mode: "hash", Source: source, Name: "sessionid", Missing: "reject"}
			for _, m := range []*gateway.Manager{first, second} {
				var revision int64
				for _, r := range m.Hosts() {
					revision = r.Revision
				}
				savePublish(t, m, host, revision)
			}
			nodes := map[string]bool{}
			sessionCount := 30
			if os.Getenv("GATEWAY_AFFINITY_RELIABILITY") == "1" {
				sessionCount = 2000
			}
			for i := 0; i < sessionCount; i++ {
				key := fmt.Sprintf("client-%d", i)
				path := "/"
				if source == "query" {
					path += "?sessionid=" + key
				}
				a := affinityRequestFor(t, first, path, source, key)
				if a.Code != 200 {
					t.Fatalf("status %d: %s", a.Code, a.Body)
				}
				nodes[a.Body.String()] = true
				for j := 0; j < 4; j++ {
					b := affinityRequestFor(t, second, path, source, key)
					if b.Code != 200 || b.Body.String() != a.Body.String() {
						t.Fatalf("affinity mismatch %s / %s", a.Body, b.Body)
					}
				}
			}
			if source != "ip" && len(nodes) < 2 {
				t.Fatal("all clients stuck on one node")
			}
			if source != "ip" {
				if r := affinityRequestFor(t, first, "/", source, ""); r.Code != 400 {
					t.Fatalf("missing key: %d", r.Code)
				}
			}
		})
	}
}
func TestAffinityValidation(t *testing.T) {
	for _, config := range []gateway.AffinityConfig{
		{Mode: "other"}, {Mode: "hash", Source: "query"}, {Mode: "hash", Source: "other"},
		{Mode: "hash", Source: "ip", TTLSeconds: -1}, {Mode: "hash", Source: "ip", Missing: "other"},
	} {
		host := baseHost()
		host.Affinity = config
		if err := gateway.ValidateHost(host); err == nil {
			t.Fatalf("accepted %+v", config)
		}
	}
}
func BenchmarkAffinityHashProxy(b *testing.B)      { benchmarkAffinityProxy(b, "hash") }
func BenchmarkAffinityAutomaticProxy(b *testing.B) { benchmarkAffinityProxy(b, "auto") }
func BenchmarkAffinityGradualProxy(b *testing.B)   { benchmarkAffinityProxy(b, "gradual") }
func benchmarkAffinityProxy(b *testing.B, mode string) {
	if os.Getenv("GATEWAY_REDIS_INTEGRATION") != "1" {
		b.Skip("requires Redis-backed Service lifecycle")
	}
	discover.Init()
	app := "gateway-affinity-benchmark"
	discover.AddExternalApp(app, "1")
	upstream := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) { w.WriteHeader(204) }))
	defer upstream.Close()
	discover.SetNode(app, strings.TrimPrefix(upstream.URL, "http://"), 100)
	server := service.NewWebServer()
	config := gateway.Config{DataDir: b.TempDir(), AdminDisabled: true}
	if mode == "auto" || mode == "gradual" {
		config.Redis = "127.0.0.1:6379"
		config.Prefix = fmt.Sprintf("gateway-benchmark-%d", time.Now().UnixNano())
		b.Cleanup(func() {
			rd := redis.GetRedis(config.Redis, log.DefaultLogger)
			for _, key := range rd.Do("KEYS", config.Prefix+":*").Strings() {
				rd.Do("DEL", key)
			}
		})
	}
	m, err := gateway.NewManager(config, server)
	if err != nil {
		b.Fatal(err)
	}
	defer m.Close()
	if err := m.SyncAffinity(context.Background()); err != nil {
		b.Fatal(err)
	}
	startAffinityServer(b, server)
	discover.AddExternalApp(app, "1")
	discover.SetNode(app, strings.TrimPrefix(upstream.URL, "http://"), 100)
	host := baseHost()
	host.Proxies = []service.ProxyRule{{Path: "/*", To: app + "/"}}
	host.Affinity = gateway.AffinityConfig{Mode: mode, Source: "header", Name: "sessionid"}
	if mode == "gradual" {
		host.Affinity.Mode = "hash"
		host.Affinity.HashStrategy = "gradual"
	}
	record, err := m.SaveDraft(host, 0, "bench")
	if err != nil {
		b.Fatal(err)
	}
	if _, err = m.Publish(host.ID, record.Revision, "bench"); err != nil {
		b.Fatal(err)
	}
	if err := m.SyncAffinity(context.Background()); err != nil {
		b.Fatal(err)
	}
	b.ResetTimer()
	b.ReportAllocs()
	for b.Loop() {
		request := httptest.NewRequest("GET", "http://example.test/", nil)
		request.Header.Set("sessionid", "same")
		m.Handler().ServeHTTP(httptest.NewRecorder(), request)
	}
}

func startAffinityServer(t testing.TB, server *service.WebServer) {
	t.Helper()
	server.Config.Listen = "127.0.0.1:0,http"
	server.Config.Register = "redis://127.0.0.1:6379/15"
	server.Config.App = fmt.Sprintf("gateway-affinity-ingress-%d", affinitySequence.Add(1))
	server.Config.NoLogGets = true
	logger := log.NewLogger(log.Config{Name: "gateway-affinity-test", Level: "error"})
	logger.SetLevel(log.CLOSE)
	if err := server.Start(context.Background(), logger); err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { server.Stop(context.Background()) })
}

func TestAffinityHTTPAndWebSocket(t *testing.T) {
	if os.Getenv("GATEWAY_REDIS_INTEGRATION") != "1" {
		t.Skip("requires Redis-backed Service lifecycle")
	}
	m, server, _ := manager(t)
	startAffinityServer(t, server)
	app := affinityUpstreams(t)
	host := baseHost()
	host.Proxies = []service.ProxyRule{{Path: "/*", To: app + "/"}}
	host.Affinity = gateway.AffinityConfig{Mode: "hash", Source: "header", Name: "sessionid"}
	savePublish(t, m, host, 0)
	ordinary := affinityRequestFor(t, m, "/", "header", "websocket-client")
	ingress := httptest.NewServer(m.Handler())
	defer ingress.Close()
	conn, response, err := websocket.DefaultDialer.Dial("ws"+strings.TrimPrefix(ingress.URL, "http")+"/", http.Header{"Host": {"example.test"}, "Sessionid": {"websocket-client"}})
	if err != nil {
		t.Fatalf("websocket %v %+v", err, response)
	}
	defer conn.Close()
	_, data, err := conn.ReadMessage()
	if err != nil || string(data) != ordinary.Body.String() {
		t.Fatalf("HTTP=%s WS=%s err=%v", ordinary.Body, data, err)
	}
}
