package gateway_test

import (
	gateway "apigo.cc/go/gateway/core"
	"apigo.cc/go/service"
	"net/http"
	"net/http/httptest"
	"sync/atomic"
	"testing"
)

func TestConfigurationCheckDoesNotContactOrSave(t *testing.T) {
	m, _, _ := manager(t)
	var calls atomic.Int32
	upstream := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) { calls.Add(1) }))
	defer upstream.Close()
	host := baseHost()
	host.Proxies = []service.ProxyRule{{Path: "/*", To: upstream.URL}}
	host.Limits = []gateway.LimitRule{{ID: "quota", Enabled: true, Requests: 1, PeriodSeconds: 60}}
	if err := m.CheckHost(host); err != nil {
		t.Fatal(err)
	}
	if calls.Load() != 0 || len(m.Hosts()) != 0 {
		t.Fatal("check contacted upstream or saved")
	}
	savePublish(t, m, host, 0)
	other := host
	other.ID = "conflicting"
	if err := m.CheckHost(other); err == nil {
		t.Fatal("duplicate domain passed")
	}
	host.Proxies[0].Path = "/("
	if err := m.CheckHost(host); err == nil {
		t.Fatal("invalid route passed")
	}
}
func TestConfigurationCheckAdminBoundary(t *testing.T) {
	m, _, _ := manager(t)
	h := m.AdminHandler()
	body := map[string]any{"host": baseHost()}
	if w := adminRequest(t, h, "/api/check-host", body, nil, ""); w.Code != 401 {
		t.Fatal("anonymous check")
	}
	cookie, csrf := login(t, h, "admin", "gateway-test-password")
	if w := adminRequest(t, h, "/api/check-host", body, cookie, ""); w.Code != 403 {
		t.Fatal("CSRF missing")
	}
	if w := adminRequest(t, h, "/api/check-host", body, cookie, csrf); w.Code != 200 {
		t.Fatal(w.Body.String())
	}
	host := baseHost()
	host.Domains = []string{"invalid domain"}
	if w := adminRequest(t, h, "/api/check-host", map[string]any{"host": host}, cookie, csrf); w.Code != 400 {
		t.Fatal("invalid configuration passed")
	}
}
