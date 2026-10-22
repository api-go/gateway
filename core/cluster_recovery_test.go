package gateway_test

import (
	"apigo.cc/go/cast"
	"apigo.cc/go/discover"
	gateway "apigo.cc/go/gateway/core"
	"apigo.cc/go/log"
	"apigo.cc/go/redis"
	"apigo.cc/go/service"
	"context"
	"fmt"
	"io"
	"net/http"
	"net/http/httptest"
	"os"
	"strings"
	"testing"
	"time"
)

func TestClusterSnapshotAndKeyRecovery(t *testing.T) {
	if os.Getenv("GATEWAY_REDIS_INTEGRATION") != "1" {
		t.Skip("set GATEWAY_REDIS_INTEGRATION=1")
	}
	rd := redis.GetRedis("127.0.0.1:6379", log.DefaultLogger)
	prefix := fmt.Sprintf("gateway-recovery-%d", time.Now().UnixNano())
	t.Cleanup(func() {
		for _, key := range rd.Do("KEYS", prefix+":*").Strings() {
			rd.Do("DEL", key)
		}
	})
	config := gateway.Config{Redis: "127.0.0.1:6379", Prefix: prefix, DataDir: t.TempDir(), AdminDisabled: true}
	first, err := gateway.NewManager(config, service.NewWebServer())
	if err != nil {
		t.Fatal(err)
	}
	host := baseHost()
	host.Rewrites = []service.RewriteRule{{Path: "/", To: "https://restored.test/"}}
	record := savePublish(t, first, host, 0)
	// Back up a draft without publishing; recovery must retain unpublished work too.
	host.Name = "retained draft"
	record, err = first.SaveDraft(host, record.Revision, "test")
	if err != nil {
		t.Fatal(err)
	}
	cert, key := certificate(t, "example.test")
	if _, err := first.SaveCertificate(gateway.CertificateRecord{ID: "recover-cert", Name: "Recovered certificate", Mode: "upload", Certificate: string(cert)}, key, 0, "test"); err != nil {
		t.Fatal(err)
	}
	originalKey := rd.Do("GET", prefix+":certificate-key").String()
	first.Close()
	rd.Do("DEL", prefix+":state", prefix+":certificate-key")
	restarted, err := gateway.NewManager(config, service.NewWebServer())
	if err != nil {
		t.Fatal(err)
	}
	defer restarted.Close()
	if rd.Do("GET", prefix+":certificate-key").String() != originalKey {
		t.Fatal("encryption key changed")
	}
	if restarted.Hosts()[0].Draft.Name != "retained draft" {
		t.Fatal("unpublished draft lost")
	}
	// A fresh instance joins using only the registry and the same namespace.
	server := service.NewWebServer()
	server.Config.Register = config.Redis
	join := config
	join.Redis = ""
	join.DataDir = t.TempDir()
	second, err := gateway.NewManager(join, server)
	if err != nil {
		t.Fatal(err)
	}
	defer second.Close()
	for _, m := range []*gateway.Manager{restarted, second} {
		r := httptest.NewRecorder()
		m.Handler().ServeHTTP(r, httptest.NewRequest("GET", "http://example.test/", nil))
		if r.Header().Get("Location") != "https://restored.test/" {
			t.Fatal("published route lost")
		}
	}
	// Live recovery is atomic and another node cannot overwrite the winner.
	rd.Do("DEL", prefix+":state", prefix+":certificate-key")
	if err := second.Reconcile(); err != nil {
		t.Fatal(err)
	}
	if err := restarted.Reconcile(); err != nil {
		t.Fatal(err)
	}
	if _, err := second.SaveDraft(host, record.Revision, "test"); err != nil {
		t.Fatal(err)
	}
	rd.Do("DEL", prefix+":state")
	rd.Do("SET", prefix+":certificate-key", string(make([]byte, 32)))
	if err := second.Reconcile(); err == nil {
		t.Fatal("conflicting key accepted")
	}
	if rd.Do("EXISTS", prefix+":state").Int() != 0 {
		t.Fatal("snapshot restored using incompatible key")
	}
}
func TestRedisAffinitySharedMapping(t *testing.T) {
	if os.Getenv("GATEWAY_REDIS_INTEGRATION") != "1" {
		t.Skip("set GATEWAY_REDIS_INTEGRATION=1")
	}
	rd := redis.GetRedis("127.0.0.1:6379", log.DefaultLogger)
	prefix := fmt.Sprintf("gateway-affinity-map-%d", time.Now().UnixNano())
	t.Cleanup(func() {
		for _, key := range rd.Do("KEYS", prefix+":*").Strings() {
			rd.Do("DEL", key)
		}
	})
	peer := httptest.NewUnstartedServer(http.NotFoundHandler())
	config := gateway.Config{Redis: "127.0.0.1:6379", Prefix: prefix, DataDir: t.TempDir(), AffinityAdvertise: "http://" + peer.Listener.Addr().String()}
	server1 := service.NewWebServer()
	server2 := service.NewWebServer()
	one, err := gateway.NewManager(config, server1)
	if err != nil {
		t.Fatal(err)
	}
	defer one.Close()
	peer.Config.Handler = one.AdminHandler()
	peer.Start()
	defer peer.Close()
	config.DataDir = t.TempDir()
	config.AffinityAdvertise = ""
	two, err := gateway.NewManager(config, server2)
	if err != nil {
		t.Fatal(err)
	}
	defer two.Close()
	if err := one.SyncAffinity(context.Background()); err != nil {
		t.Fatal(err)
	}
	startAffinityServer(t, server1)
	startAffinityServer(t, server2)
	app := affinityUpstreams(t)
	host := baseHost()
	host.Proxies = []service.ProxyRule{{Path: "/*", To: app + "/"}}
	host.Affinity = gateway.AffinityConfig{Mode: "auto", Source: "header", Name: "sessionid", TTLSeconds: 60}
	savePublish(t, one, host, 0)
	if err := two.Reconcile(); err != nil {
		t.Fatal(err)
	}
	first := affinityRequestFor(t, one, "/", "header", "client-secret")
	if err := one.SyncAffinity(context.Background()); err != nil {
		t.Fatal(err)
	}
	if err := two.SyncAffinity(context.Background()); err != nil {
		t.Fatal(err)
	}
	for i := 0; i < 20; i++ {
		response := affinityRequestFor(t, two, "/", "header", "client-secret")
		if first.Code != 200 || response.Body.String() != first.Body.String() {
			t.Fatal("mapping mismatch")
		}
	}
	if keys := rd.Do("KEYS", prefix+":affinity:*").Strings(); len(keys) != 0 {
		t.Fatalf("unexpected full Redis mapping table: %v", keys)
	}
	if ttl := rd.Do("TTL", prefix+":affinity-journal").Int(); ttl <= 0 || ttl > 300 {
		t.Fatalf("journal TTL %d", ttl)
	}
	if count := cast.Int(two.AffinityStatus()["entries"]); count != 1 {
		t.Fatalf("peer entries %d", count)
	}
	// Deliberate first-allocation conflict: different node views, first local result wins.
	original := affinityRequestFor(t, one, "/", "header", "simultaneous")
	home := ""
	for address := range discover.GetAppNodes(app) {
		res, err := http.Get("http://" + address)
		if err != nil {
			t.Fatal(err)
		}
		data, _ := io.ReadAll(res.Body)
		res.Body.Close()
		if string(data) == original.Body.String() {
			home = address
			break
		}
	}
	if home == "" {
		t.Fatal("original node missing")
	}
	discover.SetNode(app, home, 0)
	alternate := affinityRequestFor(t, two, "/", "header", "simultaneous")
	discover.SetNode(app, home, 100)
	if alternate.Code != 200 || alternate.Body.String() == original.Body.String() {
		t.Fatal("failed to establish conflicting first allocations")
	}
	for _, m := range []*gateway.Manager{one, two, one} {
		if err := m.SyncAffinity(context.Background()); err != nil {
			t.Fatal(err)
		}
	}
	if r := affinityRequestFor(t, one, "/", "header", "simultaneous"); r.Body.String() != original.Body.String() {
		t.Fatal("existing local mapping overwritten")
	}
	if r := affinityRequestFor(t, two, "/", "header", "simultaneous"); r.Body.String() != alternate.Body.String() {
		t.Fatal("conflicting local mapping overwritten")
	}
	if cast.Uint64(two.AffinityStatus()["conflicts"]) == 0 {
		t.Fatal("conflict not observable")
	}
}

func TestExplicitKeyRecoveryStaysExternal(t *testing.T) {
	if os.Getenv("GATEWAY_REDIS_INTEGRATION") != "1" {
		t.Skip("set GATEWAY_REDIS_INTEGRATION=1")
	}
	rd := redis.GetRedis("127.0.0.1:6379", log.DefaultLogger)
	prefix := fmt.Sprintf("gateway-external-recovery-%d", time.Now().UnixNano())
	t.Cleanup(func() {
		for _, key := range rd.Do("KEYS", prefix+":*").Strings() {
			rd.Do("DEL", key)
		}
	})
	config := gateway.Config{Redis: "127.0.0.1:6379", Prefix: prefix, DataDir: t.TempDir(), AdminDisabled: true, CertificateKey: strings.Repeat("12", 32)}
	m, err := gateway.NewManager(config, service.NewWebServer())
	if err != nil {
		t.Fatal(err)
	}
	defer m.Close()
	cert, key := certificate(t, "example.test")
	if _, err := m.SaveCertificate(gateway.CertificateRecord{ID: "external", Mode: "upload", Certificate: string(cert)}, key, 0, "test"); err != nil {
		t.Fatal(err)
	}
	rd.Do("DEL", prefix+":state")
	if err := m.Reconcile(); err != nil {
		t.Fatal(err)
	}
	if rd.Do("EXISTS", prefix+":certificate-key").Int() != 0 {
		t.Fatal("external encryption key uploaded to Redis")
	}
	if len(m.Certificates()) != 1 {
		t.Fatal("encrypted certificate not recovered")
	}
}
