package gateway_test

import (
	"apigo.cc/go/log"
	"apigo.cc/go/redis"
	"context"
	"fmt"
	"io"
	"net/http"
	"net/http/httptest"
	"os"
	"strings"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	"apigo.cc/go/cast"
	gateway "apigo.cc/go/gateway/core"
	"apigo.cc/go/service"
)

func adminACL() []gateway.ACLRule {
	path := gateway.MatchCondition{Source: "path", Operator: "regex", Value: "^/admin(/|$)"}
	return []gateway.ACLRule{
		{ID: "office", Enabled: true, Action: "allow", Conditions: []gateway.MatchCondition{path, {Source: "ip", Operator: "cidr", Value: "192.0.2.0/24"}}},
		{ID: "private", Enabled: true, Action: "deny", Conditions: []gateway.MatchCondition{path}},
	}
}
func TestACLIngressPublishRollbackAndTrustedIP(t *testing.T) {
	m, server, _ := manager(t)
	if err := server.SetClientIPPolicy(service.ClientIPPolicy{Mode: "trusted", TrustedProxies: []string{"10.0.0.0/8"}, Header: "X-Real-IP"}); err != nil {
		t.Fatal(err)
	}
	var calls atomic.Int32
	upstream := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) { calls.Add(1); w.WriteHeader(204) }))
	defer upstream.Close()
	host := baseHost()
	host.ACL = adminACL()
	host.Proxies = []service.ProxyRule{{Path: "/*", To: upstream.URL}}
	record := savePublish(t, m, host, 0)
	check := func(path, peer, real string, want int) {
		t.Helper()
		req := httptest.NewRequest("GET", "http://example.test"+path, nil)
		req.RemoteAddr = peer
		req.Header.Set("X-Real-IP", real)
		w := httptest.NewRecorder()
		server.Handler().ServeHTTP(w, req)
		if w.Code != want {
			t.Fatalf("%s %s %s: %d, %s", path, peer, real, w.Code, w.Body.String())
		}
	}
	check("/admin", "10.0.0.1:1234", "192.0.2.20", 204)
	check("/admin/users", "10.0.0.1:1234", "198.51.100.20", 403)
	check("/admin", "198.51.100.1:1234", "192.0.2.20", 403)
	check("/administrator", "10.0.0.1:1234", "198.51.100.20", 204)
	check("/%61dmin", "10.0.0.1:1234", "198.51.100.20", 403)
	if calls.Load() != 2 {
		t.Fatal("denied request reached upstream")
	}
	invalid := host
	invalid.ACL = []gateway.ACLRule{{ID: "bad", Enabled: true, Action: "oops"}}
	if _, err := m.SaveDraft(invalid, record.Revision, "test"); err == nil {
		t.Fatal("invalid ACL saved")
	}
	check("/admin", "10.0.0.1:1234", "198.51.100.20", 403)
	host.ACL = nil
	next := savePublish(t, m, host, record.Revision)
	check("/admin", "10.0.0.1:1234", "198.51.100.20", 204)
	if _, err := m.Rollback(host.ID, record.Version, next.Revision, "test"); err != nil {
		t.Fatal(err)
	}
	check("/admin", "10.0.0.1:1234", "198.51.100.20", 403)
}
func TestACLConditionsObserveAndQuota(t *testing.T) {
	m, server, _ := manager(t)
	host := baseHost()
	host.ACL = []gateway.ACLRule{
		{ID: "observe", Enabled: true, Action: "deny", Mode: "observe"},
		{ID: "allow", Enabled: true, Action: "allow", Conditions: []gateway.MatchCondition{{Source: "method", Operator: "equals", Value: "POST"}, {Source: "header", Name: "X-Role", Operator: "equals", Value: "staff"}, {Source: "cookie", Name: "role", Operator: "equals", Value: "staff"}, {Source: "session", Operator: "exists"}}},
		{ID: "deny", Enabled: true, Action: "deny"},
	}
	host.Limits = []gateway.LimitRule{{ID: "quota", Enabled: true, Requests: 1, PeriodSeconds: 60}}
	savePublish(t, m, host, 0)
	request := func(authorized bool) int {
		req := httptest.NewRequest("POST", "http://example.test/", strings.NewReader("body"))
		if authorized {
			req.Header.Set("X-Role", "staff")
			req.Header.Set("Session-ID", "abc")
			req.AddCookie(&http.Cookie{Name: "role", Value: "staff"})
		}
		w := httptest.NewRecorder()
		server.Handler().ServeHTTP(w, req)
		return w.Code
	}
	if got := request(false); got != 403 {
		t.Fatal(got)
	}
	if got := request(true); got == 403 || got == 429 {
		t.Fatal("deny consumed quota or allow failed", got)
	}
	if got := request(true); got != 429 {
		t.Fatal("allow bypassed quota", got)
	}
	count := 0
	for _, event := range m.Events() {
		if event.Type == "acl" && event.RuleID == "observe" {
			count++
		}
	}
	if count != 3 {
		t.Fatal("observe did not continue/record", count)
	}
}
func TestACLScriptAndValidation(t *testing.T) {
	m, _, _ := manager(t)
	for _, test := range []struct {
		code   string
		denied bool
	}{
		{`function(r){return r.ip === "2001:db8::1" && r.headers["X-Role"][0] === "staff" && r.cookies.role === "staff";}`, true},
		{`function(r){return false;}`, false},
		{`function(r){throw Error("private");}`, true},
		{`function(r){return "true";}`, true},
		{`function(r){while(true){}}`, true},
	} {
		host := baseHost()
		host.ACL = []gateway.ACLRule{{ID: "script", Enabled: true, Action: "deny", Script: test.code, TimeoutMS: 2}}
		result, err := m.Preview(gateway.PreviewInput{Host: host, Domain: "example.test", Path: "/", IP: "[2001:db8::1]:1234", Headers: map[string]string{"X-Role": "staff"}, Cookies: map[string]string{"role": "staff"}})
		if err != nil {
			t.Fatal(err)
		}
		if (result.Type == "denied") != test.denied {
			t.Fatalf("%s: %+v", test.code, result)
		}
	}
	for _, rule := range []gateway.ACLRule{
		{ID: "bad", Action: "invalid"}, {ID: "bad", Action: "allow", Mode: "invalid"}, {ID: "bad", Action: "allow", TimeoutMS: 101},
		{ID: "bad", Action: "allow", Script: `function(`},
		{ID: "bad", Action: "deny", Conditions: []gateway.MatchCondition{{Source: "ip", Operator: "cidr", Value: "broken"}}},
	} {
		host := baseHost()
		host.ACL = []gateway.ACLRule{rule}
		if err := gateway.ValidateHost(host); err == nil {
			t.Fatalf("invalid ACL accepted: %+v", rule)
		}
	}
	host := baseHost()
	host.ACL = append(adminACL(), adminACL()...)
	if gateway.ValidateHost(host) == nil {
		t.Fatal("duplicate IDs accepted")
	}
}
func TestACLScriptConcurrentCancellationAndRecovery(t *testing.T) {
	m, server, _ := manager(t)
	host := baseHost()
	host.ACL = []gateway.ACLRule{{ID: "script", Enabled: true, Action: "deny", TimeoutMS: 2, Script: `function(r){if(r.path === "/loop"){while(true){}} return r.path === "/deny";}`}}
	savePublish(t, m, host, 0)
	var wg sync.WaitGroup
	for i := 0; i < 24; i++ {
		wg.Add(1)
		go func() {
			defer wg.Done()
			for _, path := range []string{"/loop", "/deny", "/ok"} {
				req := httptest.NewRequest("GET", "http://example.test"+path, nil)
				w := httptest.NewRecorder()
				server.Handler().ServeHTTP(w, req)
				if path != "/ok" && w.Code != 403 {
					t.Errorf("%s: %d", path, w.Code)
				}
			}
		}()
	}
	wg.Wait()
	ctx, cancel := context.WithCancel(context.Background())
	cancel()
	req := httptest.NewRequest("GET", "http://example.test/loop", nil).WithContext(ctx)
	w := httptest.NewRecorder()
	server.Handler().ServeHTTP(w, req)
	if w.Code != 403 {
		t.Fatal(w.Code)
	}
}
func TestACLCompiledOwnsConditionsAndPreservesRequest(t *testing.T) {
	rules := adminACL()
	acl, err := gateway.NewACL(rules)
	if err != nil {
		t.Fatal(err)
	}
	rules[1].Conditions[0].Value = "^/other"
	req := httptest.NewRequest("POST", "http://example.test/admin", strings.NewReader("original body"))
	req.RemoteAddr = "198.51.100.1:1234"
	wrapped := service.NewRequest(req)
	if decision := acl.Evaluate(wrapped); decision.Allowed {
		t.Fatal("caller changed compiled policy", decision)
	}
	data, err := io.ReadAll(req.Body)
	if err != nil || string(data) != "original body" {
		t.Fatal("ACL consumed body", err)
	}
}
func BenchmarkACL(b *testing.B) {
	for _, kind := range []string{"none", "conditions", "script"} {
		b.Run(kind, func(b *testing.B) {
			var rules []gateway.ACLRule
			if kind != "none" {
				rules = []gateway.ACLRule{{ID: "allow", Enabled: true, Action: "allow", Conditions: []gateway.MatchCondition{{Source: "path", Operator: "regex", Value: "^/admin(/|$)"}, {Source: "ip", Operator: "cidr", Value: "192.0.2.0/24"}}}}
			}
			if kind == "script" {
				rules[0].Script = `function(r){return r.method === "GET";}`
				rules[0].TimeoutMS = 100
			}
			acl, err := gateway.NewACL(rules)
			if err != nil {
				b.Fatal(err)
			}
			b.ReportAllocs()
			b.ResetTimer()
			b.RunParallel(func(pb *testing.PB) {
				req := httptest.NewRequest("GET", "http://example.test/admin", nil)
				req.RemoteAddr = "192.0.2.1:1234"
				wrapped := service.NewRequest(req)
				for pb.Next() {
					if !acl.Evaluate(wrapped).Allowed {
						b.Error("unexpected rejection")
					}
				}
			})
		})
	}
}

func TestACLRestartAndAPIKeyScriptBoundary(t *testing.T) {
	m, _, config := manager(t)
	host := baseHost()
	host.ACL = []gateway.ACLRule{{ID: "code", Enabled: true, Action: "deny", Script: `function(r){return r.path === "/admin";}`}}
	record := savePublish(t, m, host, 0)
	server := service.NewWebServer()
	restarted, err := gateway.NewManager(config, server)
	if err != nil {
		t.Fatal(err)
	}
	defer restarted.Close()
	w := httptest.NewRecorder()
	server.Handler().ServeHTTP(w, httptest.NewRequest("GET", "http://example.test/admin", nil))
	if w.Code != 403 {
		t.Fatal("restart lost ACL", w.Code)
	}
	handler := m.AdminHandler()
	cookie, csrf := login(t, handler, "admin", "gateway-test-password")
	listing := adminRequest(t, handler, "/api/keys", nil, cookie, csrf)
	state, err := cast.FromJSON[map[string]any](listing.Body.Bytes())
	if err != nil {
		t.Fatal(err)
	}
	created := adminRequest(t, handler, "/api/keys/create", map[string]any{"name": "acl-editor", "permissions": []string{"draft"}, "hostIDs": []string{host.ID}, "revision": state["revision"]}, cookie, csrf)
	if created.Code != 200 {
		t.Fatal(created.Body.String())
	}
	result, err := cast.FromJSON[map[string]any](created.Body.Bytes())
	if err != nil {
		t.Fatal(err)
	}
	token := cast.String(result["token"])
	host.ACL[0].Script = `function(r){return true;}`
	denied := machineRequest(t, handler, "/api/v1/draft", token, map[string]any{"host": host, "revision": record.Revision})
	if denied.Code != 403 {
		t.Fatal("API key edited script", denied.Code, denied.Body.String())
	}
	host.ACL = record.Draft.ACL
	host.ACL[0].Name = "rename"
	allowed := machineRequest(t, handler, "/api/v1/draft", token, map[string]any{"host": host, "revision": record.Revision})
	if allowed.Code != 200 {
		t.Fatal("API key could not edit declarative ACL", allowed.Code, allowed.Body.String())
	}
}
func TestACLIPv6AndScriptObserveFailure(t *testing.T) {
	rules := []gateway.ACLRule{
		{ID: "broken", Enabled: true, Action: "deny", Mode: "observe", Script: `function(r){throw Error("failed");}`},
		{ID: "ipv6", Enabled: true, Action: "allow", Conditions: []gateway.MatchCondition{{Source: "ip", Operator: "cidr", Value: "2001:db8::/32"}}},
		{ID: "default", Enabled: true, Action: "deny"},
	}
	acl, err := gateway.NewACL(rules)
	if err != nil {
		t.Fatal(err)
	}
	for _, test := range []struct {
		ip    string
		allow bool
	}{{"[2001:db8::1]:1234", true}, {"[2001:db9::1]:1234", false}, {"192.0.2.1:1234", false}} {
		req := httptest.NewRequest("GET", "http://example.test/", nil)
		req.RemoteAddr = test.ip
		if got := acl.Evaluate(service.NewRequest(req)); got.Allowed != test.allow {
			t.Fatal(test, got)
		}
	}
}

func TestACLDoesNotChangeMonitorAndDirectUpstreamTests(t *testing.T) {
	m, _, _ := manager(t)
	var calls atomic.Int32
	upstream := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) { calls.Add(1); w.WriteHeader(204) }))
	defer upstream.Close()
	host := baseHost()
	host.Proxies = []service.ProxyRule{{Path: "/*", To: upstream.URL}}
	host.Monitor = gateway.MonitorConfig{Configured: true, Enabled: true}
	host.ACL = []gateway.ACLRule{{ID: "deny", Enabled: true, Action: "deny", Script: `function(r){throw Error("ingress only");}`}}
	if err := m.CheckHost(host); err != nil {
		t.Fatal("monitor route validation ran ingress ACL", err)
	}
	if calls.Load() != 0 {
		t.Fatal("configuration check contacted upstream")
	}
	input := gateway.PreviewInput{Host: host, Domain: "example.test", Path: "/"}
	result, err := m.TestUpstream(context.Background(), input)
	if err != nil || result.Status != 204 {
		t.Fatal("direct upstream test changed", result, err)
	}
	preview, err := m.Preview(input)
	if err != nil || preview.Type != "denied" {
		t.Fatal("public preview skipped ACL", preview, err)
	}
}

func TestACLClusterConvergence(t *testing.T) {
	if os.Getenv("GATEWAY_REDIS_INTEGRATION") != "1" {
		t.Skip("set GATEWAY_REDIS_INTEGRATION=1")
	}
	rd := redis.GetRedis("127.0.0.1:6379", log.DefaultLogger)
	if rd.Error != nil {
		t.Fatal(rd.Error)
	}
	prefix := fmt.Sprintf("gateway-acl-test-%d", time.Now().UnixNano())
	t.Cleanup(func() {
		for _, key := range rd.Do("KEYS", prefix+":*").Strings() {
			rd.Do("DEL", key)
		}
	})
	config := gateway.Config{DataDir: t.TempDir(), Redis: "127.0.0.1:6379", Prefix: prefix, NodeID: "acl-one", AdminDisabled: true}
	one, err := gateway.NewManager(config, service.NewWebServer())
	if err != nil {
		t.Fatal(err)
	}
	defer one.Close()
	config.DataDir = t.TempDir()
	config.NodeID = "acl-two"
	server := service.NewWebServer()
	two, err := gateway.NewManager(config, server)
	if err != nil {
		t.Fatal(err)
	}
	defer two.Close()
	host := baseHost()
	host.ACL = []gateway.ACLRule{{ID: "script", Enabled: true, Action: "deny", Script: `function(r){return r.path === "/admin";}`}}
	record := savePublish(t, one, host, 0)
	if err := two.Reconcile(); err != nil {
		t.Fatal(err)
	}
	w := httptest.NewRecorder()
	server.Handler().ServeHTTP(w, httptest.NewRequest("GET", "http://example.test/admin", nil))
	if w.Code != 403 {
		t.Fatal("ACL did not propagate", w.Code)
	}
	host.ACL = nil
	savePublish(t, one, host, record.Revision)
	if err := two.Reconcile(); err != nil {
		t.Fatal(err)
	}
	w = httptest.NewRecorder()
	server.Handler().ServeHTTP(w, httptest.NewRequest("GET", "http://example.test/admin", nil))
	if w.Code == 403 {
		t.Fatal("removed ACL remained active")
	}
}
