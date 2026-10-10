package gateway_test

import (
	"apigo.cc/go/cast"
	"apigo.cc/go/file"
	"apigo.cc/go/gateway/core"
	"apigo.cc/go/log"
	"apigo.cc/go/redis"
	"apigo.cc/go/safe"
	"apigo.cc/go/service"
	"bytes"
	"context"
	"crypto/ecdsa"
	"crypto/elliptic"
	crand "crypto/rand"
	"crypto/tls"
	"crypto/x509"
	"crypto/x509/pkix"
	"encoding/pem"
	"errors"
	"fmt"
	"io"
	"math/big"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"strings"
	"sync"
	"testing"
	"time"
)

func manager(t *testing.T) (*gateway.Manager, *service.WebServer, gateway.Config) {
	t.Helper()
	config := gateway.Config{DataDir: t.TempDir(), AdminDisabled: true, ACMEAcceptTerms: true}
	secret := safe.NewSafeBufAndErase([]byte("gateway-test-password"))
	defer secret.Close()
	if err := gateway.SetAdminPassword(config, secret); err != nil {
		t.Fatal(err)
	}
	server := service.NewWebServer()
	server.Config.NoLogGets = true
	server.Config.AutoCertDir = filepath.Join(config.DataDir, "selfsigned")
	result, err := gateway.NewManager(config, server)
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(result.Close)
	return result, server, config
}
func baseHost() gateway.Host {
	return gateway.Host{ID: "test-host", Domains: []string{"example.test"}, Enabled: true, HTTPS: gateway.HTTPSConfig{Mode: "off"}, Statics: map[string]string{}}
}
func savePublish(t *testing.T, m *gateway.Manager, host gateway.Host, revision int64) gateway.HostRecord {
	t.Helper()
	record, err := m.SaveDraft(host, revision, "test")
	if err != nil {
		t.Fatal(err)
	}
	record, err = m.Publish(host.ID, record.Revision, "test")
	if err != nil {
		t.Fatal(err)
	}
	return record
}
func TestHostDraftPublishRollbackAndRestart(t *testing.T) {
	m, server, config := manager(t)
	host := baseHost()
	host.Rewrites = []service.RewriteRule{{Path: "/old", To: "https://first.test/"}}
	draft, err := m.SaveDraft(host, 0, "test")
	if err != nil {
		t.Fatal(err)
	}
	before := httptest.NewRecorder()
	server.Handler().ServeHTTP(before, httptest.NewRequest("GET", "http://example.test/old", nil))
	if before.Code != 404 {
		t.Fatal("draft affected traffic")
	}
	first, err := m.Publish(host.ID, draft.Revision, "test")
	if err != nil {
		t.Fatal(err)
	}
	after := httptest.NewRecorder()
	server.Handler().ServeHTTP(after, httptest.NewRequest("GET", "http://example.test/old", nil))
	if after.Header().Get("Location") != "https://first.test/" {
		t.Fatalf("publish did not apply: %v", after.Header())
	}
	if _, err := m.SaveDraft(host, 0, "test"); !errors.Is(err, gateway.ErrConflict) {
		t.Fatalf("stale edit: %v", err)
	}
	host.Rewrites[0].To = "https://second.test/"
	second := savePublish(t, m, host, first.Revision)
	rolled, err := m.Rollback(host.ID, first.Version, second.Revision, "test")
	if err != nil {
		t.Fatal(err)
	}
	if rolled.Version != 3 {
		t.Fatalf("rollback version %d", rolled.Version)
	}
	restartedServer := service.NewWebServer()
	restartedServer.Config.NoLogGets = true
	restarted, err := gateway.NewManager(config, restartedServer)
	if err != nil {
		t.Fatal(err)
	}
	defer restarted.Close()
	response := httptest.NewRecorder()
	restartedServer.Handler().ServeHTTP(response, httptest.NewRequest("GET", "http://example.test/old", nil))
	if response.Header().Get("Location") != "https://first.test/" {
		t.Fatal("restart lost last publication")
	}
	if err := restarted.Delete(host.ID, rolled.Revision, "test"); err != nil {
		t.Fatal(err)
	}
}
func TestInvalidPublishRetainsTrafficAndDomainOwnership(t *testing.T) {
	m, server, _ := manager(t)
	host := baseHost()
	host.Rewrites = []service.RewriteRule{{Path: "/", To: "https://working.test/"}}
	record := savePublish(t, m, host, 0)
	host.Proxies = []service.ProxyRule{{Path: "/(", To: "http://localhost/"}}
	if _, err := m.SaveDraft(host, record.Revision, "test"); err == nil {
		t.Fatal("bad regex accepted")
	}
	other := baseHost()
	other.ID = "other"
	draft, err := m.SaveDraft(other, 0, "test")
	if err != nil {
		t.Fatal(err)
	}
	if _, err := m.Publish(other.ID, draft.Revision, "test"); err == nil {
		t.Fatal("duplicate domain accepted")
	}
	response := httptest.NewRecorder()
	server.Handler().ServeHTTP(response, httptest.NewRequest("GET", "http://example.test/", nil))
	if response.Header().Get("Location") != "https://working.test/" {
		t.Fatal("invalid publication affected traffic")
	}
}
func request() *service.Request {
	raw := httptest.NewRequest("GET", "/login", nil)
	raw.Host = "example.test"
	raw.RemoteAddr = "192.0.2.10:1000"
	return service.NewRequest(raw)
}
func TestLimiterCompositeDimensionsMissingAndWindows(t *testing.T) {
	dimensions := []gateway.GroupDimension{{Source: "host"}, {Source: "path"}, {Source: "ip"}, {Source: "ua"}, {Source: "header", Name: "X-Tenant"}, {Source: "session"}, {Source: "cookie", Name: "account"}}
	limiter := gateway.NewLimiter(20, nil, "test")
	rule := gateway.LimitRule{ID: "all", Enabled: true, PeriodSeconds: 60, Requests: 2, GroupBy: dimensions}
	now := time.Unix(1000, 0)
	first := request()
	first.Header().Set("User-Agent", "browser")
	first.Header().Set("X-Tenant", "alpha")
	first.Header().Set("Session-ID", "session-one")
	first.Request.AddCookie(&http.Cookie{Name: "account", Value: "user-one"})
	for index := 0; index < 3; index++ {
		results, err := limiter.Evaluate("host", []gateway.LimitRule{rule}, first, now)
		if err != nil {
			t.Fatal(err)
		}
		if results[0].Allowed != (index < 2) {
			t.Fatalf("request %d decision %+v", index, results)
		}
	}
	second := request()
	second.Header().Set("User-Agent", "browser")
	second.Header().Set("X-Tenant", "beta")
	second.Header().Set("Session-ID", "session-one")
	second.Request.AddCookie(&http.Cookie{Name: "account", Value: "user-one"})
	results, _ := limiter.Evaluate("host", []gateway.LimitRule{rule}, second, now)
	if !results[0].Allowed {
		t.Fatal("different tenant shares quota")
	}
	results, _ = limiter.Evaluate("host", []gateway.LimitRule{rule}, first, now.Add(60*time.Second))
	if !results[0].Allowed {
		t.Fatal("window did not expire")
	}
	missingRule := gateway.LimitRule{ID: "missing", Enabled: true, PeriodSeconds: 60, Requests: 1, GroupBy: []gateway.GroupDimension{{Source: "cookie", Name: "absent"}}}
	missing := request()
	results, _ = limiter.Evaluate("host", []gateway.LimitRule{missingRule}, missing, now)
	if !results[0].Allowed {
		t.Fatal("first missing request")
	}
	results, _ = limiter.Evaluate("host", []gateway.LimitRule{missingRule}, request(), now)
	if results[0].Allowed {
		t.Fatal("missing cookie bypassed quota")
	}
	missingRule.Missing = "reject"
	results, _ = limiter.Evaluate("host", []gateway.LimitRule{missingRule}, missing, now)
	if results[0].Allowed {
		t.Fatal("required cookie not rejected")
	}
}
func TestLimiterConcurrentCapacityAndObserve(t *testing.T) {
	limiter := gateway.NewLimiter(1, nil, "test")
	rule := gateway.LimitRule{ID: "concurrent", Enabled: true, PeriodSeconds: 60, Requests: 10}
	var wg sync.WaitGroup
	allowed := 0
	var mu sync.Mutex
	for index := 0; index < 100; index++ {
		wg.Add(1)
		go func() {
			defer wg.Done()
			results, err := limiter.Evaluate("host", []gateway.LimitRule{rule}, request(), time.Unix(1000, 0))
			if err != nil {
				t.Error(err)
				return
			}
			if results[0].Allowed {
				mu.Lock()
				allowed++
				mu.Unlock()
			}
		}()
	}
	wg.Wait()
	if allowed != 10 {
		t.Fatalf("allowed %d", allowed)
	}
	rule.ID = "capacity"
	rule.Mode = "observe"
	results, _ := limiter.Evaluate("host", []gateway.LimitRule{rule}, request(), time.Unix(1000, 0))
	if results[0].Allowed || !results[0].Observed || results[0].Error == "" {
		t.Fatalf("capacity result %+v", results)
	}
}
func TestQuotaPrecedesStreamingProxyAndStatic(t *testing.T) {
	m, server, _ := manager(t)
	upstreamCalls := 0
	upstream := httptest.NewServer(http.HandlerFunc(func(writer http.ResponseWriter, req *http.Request) { upstreamCalls++; io.Copy(writer, req.Body) }))
	defer upstream.Close()
	host := baseHost()
	host.Proxies = []service.ProxyRule{{Path: "/upload", To: upstream.URL + "/"}}
	host.Limits = []gateway.LimitRule{{ID: "host-total", Enabled: true, Requests: 1, PeriodSeconds: 60}}
	savePublish(t, m, host, 0)
	body := bytes.NewBufferString("stream-body")
	response := httptest.NewRecorder()
	server.Handler().ServeHTTP(response, httptest.NewRequest("POST", "http://example.test/upload", body))
	if response.Code != 200 || response.Body.String() != "stream-body" {
		t.Fatalf("proxy: %d %s", response.Code, response.Body.String())
	}
	rejected := httptest.NewRecorder()
	server.Handler().ServeHTTP(rejected, httptest.NewRequest("POST", "http://example.test/upload", bytes.NewBufferString("other-body")))
	if rejected.Code != 429 || rejected.Header().Get("Retry-After") == "" || upstreamCalls != 1 {
		t.Fatalf("quota did not precede proxy: %d calls %d", rejected.Code, upstreamCalls)
	}
}
func adminRequest(t *testing.T, handler http.Handler, path string, body any, cookie *http.Cookie, csrf string) *httptest.ResponseRecorder {
	t.Helper()
	var reader io.Reader
	if body != nil {
		data, err := cast.ToJSONBytes(body)
		if err != nil {
			t.Fatal(err)
		}
		reader = bytes.NewReader(data)
	}
	method := "GET"
	if body != nil {
		method = "POST"
	}
	request := httptest.NewRequest(method, path, reader)
	request.Header.Set("Content-Type", "application/json")
	request.Header.Set("X-CSRF-Token", csrf)
	if cookie != nil {
		request.AddCookie(cookie)
	}
	response := httptest.NewRecorder()
	handler.ServeHTTP(response, request)
	return response
}
func login(t *testing.T, handler http.Handler, name, password string) (*http.Cookie, string) {
	t.Helper()
	response := adminRequest(t, handler, "/api/login", map[string]string{"name": name, "password": password}, nil, "")
	if response.Code != 200 {
		t.Fatalf("login %d %s", response.Code, response.Body.String())
	}
	result, err := cast.FromJSON[map[string]any](response.Body.Bytes())
	if err != nil {
		t.Fatal(err)
	}
	return response.Result().Cookies()[0], cast.String(result["csrf"])
}
func TestAdminAuthenticationAndCSRF(t *testing.T) {
	m, _, config := manager(t)
	handler := m.AdminHandler()
	if response := adminRequest(t, handler, "/api/hosts", nil, nil, ""); response.Code != 401 {
		t.Fatal("anonymous access")
	}
	cookie, csrf := login(t, handler, "admin", "gateway-test-password")
	if response := adminRequest(t, handler, "/api/draft", map[string]any{"host": baseHost(), "revision": 0}, cookie, ""); response.Code != 403 {
		t.Fatal("missing CSRF accepted")
	}
	if response := adminRequest(t, handler, "/api/users", nil, cookie, csrf); response.Code != 405 && response.Code != 404 {
		t.Fatal("user API still exists")
	}
	secret := safe.NewSafeBufAndErase([]byte("replacement-test-password"))
	defer secret.Close()
	if err := gateway.SetAdminPassword(config, secret); err != nil {
		t.Fatal(err)
	}
	if response := adminRequest(t, handler, "/api/hosts", nil, cookie, ""); response.Code != 401 {
		t.Fatal("reset failed to revoke old session")
	}
	login(t, handler, "admin", "replacement-test-password")
}
func certificate(t *testing.T, domain string) ([]byte, []byte) {
	t.Helper()
	key, err := ecdsa.GenerateKey(elliptic.P256(), crand.Reader)
	if err != nil {
		t.Fatal(err)
	}
	template := &x509.Certificate{SerialNumber: big.NewInt(17), Subject: pkix.Name{CommonName: domain}, DNSNames: []string{domain}, NotBefore: time.Now().Add(-time.Hour), NotAfter: time.Now().Add(90 * 24 * time.Hour), KeyUsage: x509.KeyUsageDigitalSignature, ExtKeyUsage: []x509.ExtKeyUsage{x509.ExtKeyUsageServerAuth}}
	der, err := x509.CreateCertificate(crand.Reader, template, template, &key.PublicKey, key)
	if err != nil {
		t.Fatal(err)
	}
	raw, err := x509.MarshalPKCS8PrivateKey(key)
	if err != nil {
		t.Fatal(err)
	}
	return pem.EncodeToMemory(&pem.Block{Type: "CERTIFICATE", Bytes: der}), pem.EncodeToMemory(&pem.Block{Type: "PRIVATE KEY", Bytes: raw})
}
func TestUploadedCertificateEncryptedAndRedacted(t *testing.T) {
	m, _, config := manager(t)
	host := baseHost()
	draft, err := m.SaveDraft(host, 0, "test")
	if err != nil {
		t.Fatal(err)
	}
	cert, key := certificate(t, "example.test")
	privateCopy := append([]byte(nil), key...)
	saved, err := m.SetCertificate(host.ID, draft.Revision, string(cert), key, "test")
	if err != nil {
		t.Fatal(err)
	}
	if bytes.Equal(key, privateCopy) {
		t.Fatal("private key not erased")
	}
	if saved.Draft.HTTPS.SealedKey != "" {
		t.Fatal("sealed key disclosed")
	}
	published, err := m.Publish(host.ID, saved.Revision, "test")
	if err != nil {
		t.Fatal(err)
	}
	data, err := file.ReadBytes(filepath.Join(config.DataDir, "state.json"))
	if err != nil {
		t.Fatal(err)
	}
	if bytes.Contains(data, privateCopy) || bytes.Contains(data, []byte("PRIVATE KEY")) {
		t.Fatal("private key stored in plaintext")
	}
	if published.Current.HTTPS.SealedKey != "" {
		t.Fatal("published key disclosed")
	}
	restarted, err := gateway.NewManager(config, service.NewWebServer())
	if err != nil {
		t.Fatal(err)
	}
	defer restarted.Close()
	cached := m.CertificateCache()
	if err := cached.Put(context.Background(), "example.test", privateCopy); err != nil {
		t.Fatal(err)
	}
	read, err := cached.Get(context.Background(), "example.test")
	if err != nil || !bytes.Equal(read, privateCopy) {
		t.Fatalf("cache roundtrip: %v", err)
	}
	files := file.Search(filepath.Join(config.DataDir, "acme"), "*")
	for _, name := range files {
		raw, _ := file.ReadBytes(name)
		if bytes.Contains(raw, []byte("PRIVATE KEY")) {
			t.Fatal("ACME cache plaintext")
		}
	}
}
func TestPreviewHasNoQuotaSideEffects(t *testing.T) {
	m, _, _ := manager(t)
	host := baseHost()
	host.Rewrites = []service.RewriteRule{{Path: "/old/*", To: "/new/*"}}
	host.Proxies = []service.ProxyRule{{Path: "/new/*", To: "user-svc/v1/*"}}
	host.Limits = []gateway.LimitRule{{ID: "login", Name: "login quota", Enabled: true, Requests: 1, PeriodSeconds: 60, Conditions: []gateway.MatchCondition{{Source: "path", Operator: "prefix", Value: "/old/"}}}}
	for index := 0; index < 3; index++ {
		result, err := m.Preview(gateway.PreviewInput{Host: host, Domain: "example.test", Path: "/old/user", IP: "192.0.2.10"})
		if err != nil {
			t.Fatal(err)
		}
		if result.Target != "user-svc/v1/user" || len(result.Limits) != 1 {
			t.Fatalf("preview %+v", result)
		}
	}
}
func TestClusterCASConvergenceDeletionAndSharedQuotas(t *testing.T) {
	if os.Getenv("GATEWAY_REDIS_INTEGRATION") != "1" {
		t.Skip("set GATEWAY_REDIS_INTEGRATION=1")
	}
	rd := redis.GetRedis("127.0.0.1:6379", log.DefaultLogger)
	if rd.Error != nil {
		t.Fatal(rd.Error)
	}
	prefix := fmt.Sprintf("gateway-test-%d", time.Now().UnixNano())
	t.Cleanup(func() {
		keys := rd.Do("KEYS", prefix+":*").Strings()
		for _, key := range keys {
			rd.Do("DEL", key)
		}
	})
	configs := []gateway.Config{{DataDir: t.TempDir(), Redis: "127.0.0.1:6379", Prefix: prefix, NodeID: "one"}, {DataDir: t.TempDir(), Redis: "127.0.0.1:6379", Prefix: prefix, NodeID: "two"}}
	one, err := gateway.NewManager(configs[0], service.NewWebServer())
	if err != nil {
		t.Fatal(err)
	}
	defer one.Close()
	two, err := gateway.NewManager(configs[1], service.NewWebServer())
	if err != nil {
		t.Fatal(err)
	}
	defer two.Close()
	host := baseHost()
	host.Rewrites = []service.RewriteRule{{Path: "/", To: "https://cluster.test/"}}
	host.Limits = []gateway.LimitRule{{ID: "shared", Enabled: true, Requests: 1, PeriodSeconds: 60, Scope: "cluster"}}
	record := savePublish(t, one, host, 0)
	if err := two.Reconcile(); err != nil {
		t.Fatal(err)
	}
	if len(two.Hosts()) != 1 {
		t.Fatal("node missed configuration")
	}
	if err := one.Reconcile(); err != nil {
		t.Fatal(err)
	}
	if len(one.Nodes()) != 2 {
		t.Fatalf("nodes %+v", one.Nodes())
	}
	first := httptest.NewRecorder()
	one.Handler().ServeHTTP(first, httptest.NewRequest("GET", "http://example.test/", nil))
	if first.Code != 302 {
		t.Fatalf("first cluster quota %d", first.Code)
	}
	second := httptest.NewRecorder()
	two.Handler().ServeHTTP(second, httptest.NewRequest("GET", "http://example.test/", nil))
	if second.Code != 429 {
		t.Fatalf("shared quota %d %s", second.Code, second.Body.String())
	}
	if _, err := two.SaveDraft(host, 0, "test"); !errors.Is(err, gateway.ErrConflict) {
		t.Fatalf("stale cluster edit: %v", err)
	}
	if err := one.Delete(host.ID, record.Revision, "test"); err != nil {
		t.Fatal(err)
	}
	if err := two.Reconcile(); err != nil {
		t.Fatal(err)
	}
	if len(two.Hosts()) != 0 {
		t.Fatal("offline-node deletion did not converge")
	}
}
func BenchmarkLimiterIPWindow(b *testing.B) {
	limiter := gateway.NewLimiter(1000, nil, "bench")
	hostRule := gateway.LimitRule{ID: "ip", Enabled: true, Requests: 1000000000, PeriodSeconds: 60, GroupBy: []gateway.GroupDimension{{Source: "ip"}}}
	req := request()
	now := time.Unix(1000, 0)
	b.ReportAllocs()
	for b.Loop() {
		limiter.Evaluate("host", []gateway.LimitRule{hostRule}, req, now)
	}
}

func TestCertificateTLSHandshakeAndCachedACME(t *testing.T) {
	m, server, _ := manager(t)
	host := baseHost()
	draft, err := m.SaveDraft(host, 0, "test")
	if err != nil {
		t.Fatal(err)
	}
	cert, key := certificate(t, "example.test")
	draft, err = m.SetCertificate(host.ID, draft.Revision, string(cert), key, "test")
	if err != nil {
		t.Fatal(err)
	}
	record, err := m.Publish(host.ID, draft.Revision, "test")
	if err != nil {
		t.Fatal(err)
	}
	server.Config.Listen = "127.0.0.1:0,https"
	if err := server.Start(context.Background(), log.DefaultLogger); err != nil {
		t.Fatal(err)
	}
	defer server.Stop(context.Background())
	connection, err := tls.Dial("tcp", server.Addrs[0], &tls.Config{InsecureSkipVerify: true, ServerName: "example.test"})
	if err != nil {
		t.Fatal(err)
	}
	if connection.ConnectionState().PeerCertificates[0].DNSNames[0] != "example.test" {
		t.Fatal("wrong uploaded certificate")
	}
	connection.Close()
	cert, key = certificate(t, "auto.example.test")
	bundle := append(append([]byte(nil), key...), cert...)
	if err := m.CertificateCache().Put(context.Background(), "auto.example.test", bundle); err != nil {
		t.Fatal(err)
	}
	auto := baseHost()
	auto.ID = "auto-host"
	auto.Domains = []string{"auto.example.test"}
	auto.HTTPS.Mode = "auto"
	savePublish(t, m, auto, 0)
	connection, err = tls.Dial("tcp", server.Addrs[0], &tls.Config{InsecureSkipVerify: true, ServerName: "auto.example.test"})
	if err != nil {
		t.Fatal(err)
	}
	if connection.ConnectionState().PeerCertificates[0].DNSNames[0] != "auto.example.test" {
		t.Fatal("ACME cached certificate not selected")
	}
	connection.Close()
	host = *record.Current
	host.HTTPS.Mode = "auto"
	host.HTTPS.Certificate = ""
	host.HTTPS.SealedKey = ""
	host.Domains = []string{"*.example.test"}
	if _, err := m.SaveDraft(host, record.Revision, "test"); err == nil {
		t.Fatal("wildcard HTTP ACME accepted")
	}
}

func TestManagerReadsAreIsolatedAndEventsBounded(t *testing.T) {
	m, _, _ := manager(t)
	host := baseHost()
	savePublish(t, m, host, 0)
	views := m.Hosts()
	views[0].Draft.Domains[0] = "mutated.test"
	if m.Hosts()[0].Draft.Domains[0] != "example.test" {
		t.Fatal("management view mutated retained configuration")
	}
}

func TestAdminRejectsForeignOriginAndAllowsPreview(t *testing.T) {
	m, _, _ := manager(t)
	handler := m.AdminHandler()
	cookie, csrf := login(t, handler, "admin", "gateway-test-password")
	request := httptest.NewRequest("POST", "http://gateway.local/api/publish", bytes.NewBufferString(`{}`))
	request.Header.Set("Content-Type", "application/json")
	request.Header.Set("Origin", "https://foreign.example")
	request.Header.Set("X-CSRF-Token", csrf)
	request.AddCookie(cookie)
	response := httptest.NewRecorder()
	handler.ServeHTTP(response, request)
	if response.Code != 403 {
		t.Fatal("foreign origin accepted")
	}
	result := adminRequest(t, handler, "/api/preview", gateway.PreviewInput{Host: baseHost(), Domain: "example.test", Path: "/"}, cookie, csrf)
	if result.Code != 200 {
		t.Fatalf("preview %d %s", result.Code, result.Body.String())
	}
}

func BenchmarkPublishedHostQuota(b *testing.B) {
	directory, err := os.MkdirTemp("", "go-gateway-bench-")
	if err != nil {
		b.Fatal(err)
	}
	defer os.RemoveAll(directory)
	server := service.NewWebServer()
	server.Config.NoLogGets = true
	m, err := gateway.NewManager(gateway.Config{DataDir: directory}, server)
	if err != nil {
		b.Fatal(err)
	}
	defer m.Close()
	server.Host("*").GET("/", func() string { return "ok" })
	host := baseHost()
	host.Limits = []gateway.LimitRule{{ID: "ip", Enabled: true, Requests: 1000000000, PeriodSeconds: 60, GroupBy: []gateway.GroupDimension{{Source: "ip"}}}}
	record, err := m.SaveDraft(host, 0, "bench")
	if err != nil {
		b.Fatal(err)
	}
	if _, err = m.Publish(host.ID, record.Revision, "bench"); err != nil {
		b.Fatal(err)
	}
	handler := server.Handler()
	req := httptest.NewRequest("GET", "http://example.test/", nil)
	b.ReportAllocs()
	b.ResetTimer()
	for b.Loop() {
		handler.ServeHTTP(httptest.NewRecorder(), req)
	}
}

func TestClusterMissingStateRetainsCacheAcrossRestart(t *testing.T) {
	if os.Getenv("GATEWAY_REDIS_INTEGRATION") != "1" {
		t.Skip("set GATEWAY_REDIS_INTEGRATION=1")
	}
	prefix := fmt.Sprintf("gateway-cache-test-%d", time.Now().UnixNano())
	rd := redis.GetRedis("127.0.0.1:6379", log.DefaultLogger)
	if rd.Error != nil {
		t.Fatal(rd.Error)
	}
	defer func() {
		for _, key := range rd.Do("KEYS", prefix+":*").Strings() {
			rd.Do("DEL", key)
		}
	}()
	config := gateway.Config{DataDir: t.TempDir(), Redis: "127.0.0.1:6379", Prefix: prefix}
	first, err := gateway.NewManager(config, service.NewWebServer())
	if err != nil {
		t.Fatal(err)
	}
	defer first.Close()
	host := baseHost()
	host.Rewrites = []service.RewriteRule{{Path: "/", To: "https://retained.test/"}}
	record := savePublish(t, first, host, 0)
	rd.Do("DEL", prefix+":state")
	if err := first.Reconcile(); err == nil {
		t.Fatal("missing authoritative state was accepted")
	}
	second, err := gateway.NewManager(config, service.NewWebServer())
	if err != nil {
		t.Fatal(err)
	}
	defer second.Close()
	for _, manager := range []*gateway.Manager{first, second} {
		response := httptest.NewRecorder()
		manager.Handler().ServeHTTP(response, httptest.NewRequest("GET", "http://example.test/", nil))
		if response.Header().Get("Location") != "https://retained.test/" {
			t.Fatalf("lost retained route: %d", response.Code)
		}
		if _, err := manager.SaveDraft(host, record.Revision, "test"); err == nil {
			t.Fatal("write accepted without authoritative state")
		}
	}
}

func TestClusterAdministratorPasswordSharedAndReset(t *testing.T) {
	if os.Getenv("GATEWAY_REDIS_INTEGRATION") != "1" {
		t.Skip("set GATEWAY_REDIS_INTEGRATION=1")
	}
	prefix := fmt.Sprintf("gateway-password-test-%d", time.Now().UnixNano())
	rd := redis.GetRedis("127.0.0.1:6379", log.DefaultLogger)
	if rd.Error != nil {
		t.Fatal(rd.Error)
	}
	defer func() {
		for _, key := range rd.Do("KEYS", prefix+":*").Strings() {
			rd.Do("DEL", key)
		}
	}()
	firstConfig := gateway.Config{DataDir: t.TempDir(), Redis: "127.0.0.1:6379", Prefix: prefix, AdminDisabled: true}
	secondConfig := firstConfig
	secondConfig.DataDir = t.TempDir()
	first, err := gateway.NewManager(firstConfig, service.NewWebServer())
	if err != nil {
		t.Fatal(err)
	}
	defer first.Close()
	second, err := gateway.NewManager(secondConfig, service.NewWebServer())
	if err != nil {
		t.Fatal(err)
	}
	defer second.Close()
	secret := safe.NewSafeBufAndErase([]byte("cluster-admin-password"))
	defer secret.Close()
	if err := gateway.SetAdminPassword(firstConfig, secret); err != nil {
		t.Fatal(err)
	}
	firstCookie, _ := login(t, first.AdminHandler(), "admin", "cluster-admin-password")
	secondCookie, _ := login(t, second.AdminHandler(), "admin", "cluster-admin-password")
	replacement := safe.NewSafeBufAndErase([]byte("cluster-new-password"))
	defer replacement.Close()
	if err := gateway.SetAdminPassword(secondConfig, replacement); err != nil {
		t.Fatal(err)
	}
	for _, entry := range []struct {
		m      *gateway.Manager
		cookie *http.Cookie
	}{{first, firstCookie}, {second, secondCookie}} {
		if response := adminRequest(t, entry.m.AdminHandler(), "/api/settings", nil, entry.cookie, ""); response.Code != 401 {
			t.Fatal("other-node old session survived")
		}
		login(t, entry.m.AdminHandler(), "admin", "cluster-new-password")
	}
	rd.Do("DEL", prefix+":admin-password")
	if response := adminRequest(t, first.AdminHandler(), "/api/login", map[string]string{"password": "cluster-new-password"}, nil, ""); response.Code != 423 {
		t.Fatal("missing shared credential was accepted")
	}
}

func TestCertificateCacheCloseDuringBackgroundWrites(t *testing.T) {
	m, _, _ := manager(t)
	cache := m.CertificateCache()
	start := make(chan struct{})
	var workers sync.WaitGroup
	for index := 0; index < 32; index++ {
		workers.Add(1)
		go func(index int) {
			defer workers.Done()
			<-start
			_ = cache.Put(context.Background(), fmt.Sprintf("close-%d", index), []byte("test-certificate-cache"))
		}(index)
	}
	close(start)
	m.Close()
	workers.Wait()
	m.Close()
	if err := cache.Put(context.Background(), "after-close", []byte("test")); err == nil {
		t.Fatal("closed manager accepted encrypted cache writes")
	}
}

func TestPreviewProxyTargetWithoutPath(t *testing.T) {
	m, _, _ := manager(t)
	for _, target := range []string{"llm", "llm/", "http://127.0.0.1:8080", "https://upstream.example.com"} {
		t.Run(target, func(t *testing.T) {
			host := baseHost()
			host.Proxies = []service.ProxyRule{{Path: "/*", To: target}}
			result, err := m.Preview(gateway.PreviewInput{Host: host, Domain: "example.test", Path: "/v1/models?x=1"})
			if err != nil {
				t.Fatal(err)
			}
			expected := strings.TrimSuffix(target, "/") + "/v1/models?x=1"
			if result.Type != "proxy" || result.Target != expected {
				t.Fatalf("preview %+v, want %s", result, expected)
			}
		})
	}
}
