package gateway_test

import (
	"apigo.cc/go/cast"
	"apigo.cc/go/discover"
	gateway "apigo.cc/go/gateway/core"
	"apigo.cc/go/service"
	"context"
	"net/http"
	"net/http/httptest"
	"strings"
	"sync/atomic"
	"testing"
)

func TestUpstreamDraftGETAndNoRedirect(t *testing.T) {
	m, _, _ := manager(t)
	var calls atomic.Int32
	backend := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		calls.Add(1)
		if r.Method != "GET" || r.Host != "example.test" || r.URL.RequestURI() != "/health?x=1" {
			t.Errorf("request %s %s %s", r.Method, r.Host, r.URL.RequestURI())
		}
		if r.Header.Get("Authorization") != "Bearer business-test" || r.Header.Get("X-CSRF-Token") != "" || strings.Contains(r.Header.Get("Cookie"), "gateway-admin") {
			t.Errorf("unexpected credential relay")
		}
		w.Header().Set("Location", "/do-not-follow")
		w.WriteHeader(302)
		w.Write([]byte(strings.Repeat("a", 5000)))
	}))
	defer backend.Close()
	host := baseHost()
	host.Proxies = []service.ProxyRule{{Path: "/*", To: backend.URL}}
	input := gateway.PreviewInput{Host: host, Domain: "example.test", Path: "/health?x=1", Method: "DELETE", Headers: map[string]string{"Authorization": "Bearer business-test", "X-CSRF-Token": "must-not-relay"}}
	result, err := m.TestUpstream(context.Background(), input)
	if err != nil {
		t.Fatal(err)
	}
	if result.Status != 302 || !result.Reachable || !result.Truncated || len(result.Body) != 4096 || result.Location != "/do-not-follow" || calls.Load() != 1 {
		t.Fatalf("result %+v calls %d", result, calls.Load())
	}
	if len(m.Hosts()) != 0 {
		t.Fatal("test modified state")
	}
}
func TestUpstreamAdminBoundaryAndBusinessResponse(t *testing.T) {
	m, _, _ := manager(t)
	h := m.AdminHandler()
	cookie, csrf := login(t, h, "admin", "gateway-test-password")
	backend := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.Header.Get("Cookie") != "" || r.Header.Get("X-CSRF-Token") != "" {
			t.Error("admin credential leaked")
		}
		w.WriteHeader(401)
		w.Write([]byte("business login required"))
	}))
	defer backend.Close()
	host := baseHost()
	host.Proxies = []service.ProxyRule{{Path: "/*", To: backend.URL}}
	input := gateway.PreviewInput{Host: host, Domain: "example.test", Path: "/"}
	if w := adminRequest(t, h, "/api/upstream-test", input, nil, ""); w.Code != 401 {
		t.Fatal("anonymous probe allowed")
	}
	if w := adminRequest(t, h, "/api/upstream-test", input, cookie, ""); w.Code != 403 {
		t.Fatal("CSRF bypass")
	}
	w := adminRequest(t, h, "/api/upstream-test", input, cookie, csrf)
	if w.Code != 200 {
		t.Fatal(w.Body.String())
	}
	result, err := cast.FromJSON[gateway.UpstreamTestResult](w.Body.Bytes())
	if err != nil || !result.Reachable || result.Status != 401 {
		t.Fatalf("business response %+v %v", result, err)
	}
}
func TestUpstreamDiscoveryAndCancellation(t *testing.T) {
	m, _, _ := manager(t)
	backend := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) { w.Write([]byte("discovered")) }))
	defer backend.Close()
	discover.AddExternalApp("gateway-upstream-test", "http")
	discover.SetNode("gateway-upstream-test", strings.TrimPrefix(backend.URL, "http://"), 1)
	t.Cleanup(func() { discover.SetNode("gateway-upstream-test", strings.TrimPrefix(backend.URL, "http://"), 0) })
	host := baseHost()
	host.Proxies = []service.ProxyRule{{Path: "/*", To: "gateway-upstream-test"}}
	input := gateway.PreviewInput{Host: host, Domain: "example.test", Path: "/health"}
	result, err := m.TestUpstream(context.Background(), input)
	if err != nil || result.Status != 200 || result.Node == "" || result.Body != "discovered" {
		t.Fatalf("discovery %+v %v", result, err)
	}
	host.Proxies[0].To = backend.URL
	input.Host = host
	ctx, cancel := context.WithCancel(context.Background())
	cancel()
	result, err = m.TestUpstream(ctx, input)
	if err != nil || result.Reachable || result.Error == "" {
		t.Fatalf("cancellation %+v %v", result, err)
	}
	input.Path = "//evil.test/"
	if _, err := m.TestUpstream(context.Background(), input); err == nil {
		t.Fatal("invalid path")
	}
	input.Path = "/"
	input.Host.Proxies = nil
	if _, err := m.TestUpstream(context.Background(), input); err == nil {
		t.Fatal("unmatched route")
	}
}
