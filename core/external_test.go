package gateway_test

import (
	"apigo.cc/go/cast"
	gateway "apigo.cc/go/gateway/core"
	gohttp "apigo.cc/go/http"
	"apigo.cc/go/safe"
	"apigo.cc/go/service"
	"io"
	"net/http"
	"net/http/httptest"
	"net/http/httputil"
	"net/url"
	"strings"
	"testing"
)

func externalManager(t *testing.T, adminTrust bool) *gateway.Manager {
	t.Helper()
	config := gateway.Config{DataDir: t.TempDir(), AdminDisabled: true, ExternalAddress: gohttp.ExternalAddressPolicy{TrustedProxies: []string{"127.0.0.1"}}}
	if adminTrust {
		config.AdminExternalAddress = gohttp.ExternalAddressPolicy{TrustedProxies: []string{"127.0.0.1"}}
	}
	password := safe.NewSafeBufAndErase([]byte("gateway-test-password"))
	defer password.Close()
	if err := gateway.SetAdminPassword(config, password); err != nil {
		t.Fatal(err)
	}
	m, err := gateway.NewManager(config, service.NewWebServer())
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(m.Close)
	return m
}
func TestAdminHTTPSForwardProxyLoginAndWrites(t *testing.T) {
	m := externalManager(t, true)
	backend := httptest.NewServer(m.AdminHandler())
	defer backend.Close()
	target, _ := url.Parse(backend.URL)
	proxy := httputil.NewSingleHostReverseProxy(target)
	director := proxy.Director
	proxy.Director = func(r *http.Request) {
		host := r.Host
		director(r)
		r.Host = target.Host
		r.Header.Del("X-Host")
		r.Header.Del("X-Scheme")
		r.Header.Set("X-Forwarded-Host", host)
		r.Header.Set("X-Forwarded-Proto", "https")
	}
	frontend := httptest.NewTLSServer(proxy)
	defer frontend.Close()
	client := frontend.Client()
	send := func(path, origin string, cookie *http.Cookie, csrf string) *http.Response {
		t.Helper()
		body := "{}"
		if path == "/api/login" {
			body = `{"password":"gateway-test-password"}`
		}
		r, _ := http.NewRequest("POST", frontend.URL+path, strings.NewReader(body))
		r.Header.Set("Content-Type", "application/json")
		r.Header.Set("Origin", origin)
		r.Header.Set("X-CSRF-Token", csrf)
		if cookie != nil {
			r.AddCookie(cookie)
		}
		response, err := client.Do(r)
		if err != nil {
			t.Fatal(err)
		}
		return response
	}
	response := send("/api/login", frontend.URL, nil, "")
	data, _ := io.ReadAll(response.Body)
	response.Body.Close()
	if response.StatusCode != 200 {
		t.Fatalf("login %d %s", response.StatusCode, data)
	}
	cookies := response.Cookies()
	if len(cookies) != 1 || !cookies[0].Secure {
		t.Fatal("missing Secure", cookies)
	}
	result, err := cast.FromJSON[map[string]any](data)
	if err != nil {
		t.Fatal(err)
	}
	csrf := cast.String(result["csrf"])
	response = send("/api/logout", "https://evil.example", cookies[0], csrf)
	response.Body.Close()
	if response.StatusCode != 403 {
		t.Fatal("cross-origin write", response.StatusCode)
	}
	response = send("/api/logout", frontend.URL, cookies[0], csrf)
	response.Body.Close()
	if response.StatusCode != 200 {
		t.Fatal("same-origin write", response.StatusCode)
	}
}
func TestAdminTrustIndependentAndMalformedHeaders(t *testing.T) {
	for _, trusted := range []bool{false, true} {
		m := externalManager(t, trusted)
		for _, tc := range []struct {
			name         string
			origin       []string
			host, scheme string
			status       int
		}{
			{"spoof", []string{"https://public.example"}, "public.example", "https", 403},
			{"cross", []string{"https://evil.example"}, "public.example", "https", 403},
			{"null", []string{"null"}, "public.example", "https", 403},
			{"duplicate origin", []string{"https://public.example", "https://public.example"}, "public.example", "https", 403},
			{"partial", []string{"http://internal"}, "", "https", 400},
			{"path origin", []string{"https://public.example/"}, "public.example", "https", 403},
		} {
			r := httptest.NewRequest("POST", "http://internal/api/login", strings.NewReader(`{"password":"gateway-test-password"}`))
			r.RemoteAddr = "127.0.0.1:1234"
			r.Header.Set("Content-Type", "application/json")
			r.Header["Origin"] = tc.origin
			if tc.host != "" {
				r.Header.Set("X-Forwarded-Host", tc.host)
			}
			r.Header.Set("X-Forwarded-Proto", tc.scheme)
			want := tc.status
			if trusted && tc.name == "spoof" {
				want = 200
			}
			if !trusted && tc.name == "partial" {
				want = 200
			}
			w := httptest.NewRecorder()
			m.AdminHandler().ServeHTTP(w, r)
			if w.Code != want {
				t.Errorf("trust %v %s: %d %s", trusted, tc.name, w.Code, w.Body.String())
			}
		}
	}
}

// Exercise the actual Gateway business handler as the TLS frontend. Neither
// management nor downstream services require any external-address configuration.
func TestDefaultGatewayHTTPSAdminWithoutTrustConfiguration(t *testing.T) {
	m, server, _ := manager(t)
	backend := httptest.NewServer(m.AdminHandler())
	defer backend.Close()
	server.Host("*").Proxy(0, "/*", backend.URL+"/*")
	frontend := httptest.NewTLSServer(server.Handler())
	defer frontend.Close()
	client := frontend.Client()
	send := func(path, origin, csrf string, cookie *http.Cookie) *http.Response {
		t.Helper()
		body := "{}"
		if path == "/api/login" {
			body = `{"password":"gateway-test-password"}`
		}
		r, _ := http.NewRequest("POST", frontend.URL+path, strings.NewReader(body))
		r.Header.Set("Content-Type", "application/json")
		r.Header.Set("Origin", origin)
		r.Header.Set("X-CSRF-Token", csrf)
		// The public boundary regenerates metadata even when a client supplies an ID.
		r.Header.Set("X-Request-ID", "client-id")
		r.Header.Set("X-Host", "ignored.example")
		r.Header.Set("X-Scheme", "http")
		r.Header.Set("X-Forwarded-Host", "other.example")
		r.Header.Set("X-Forwarded-Proto", "http")
		if cookie != nil {
			r.AddCookie(cookie)
		}
		response, err := client.Do(r)
		if err != nil {
			t.Fatal(err)
		}
		return response
	}
	response := send("/api/login", frontend.URL, "", nil)
	data, _ := io.ReadAll(response.Body)
	response.Body.Close()
	if response.StatusCode != 200 {
		t.Fatalf("default login %d %s", response.StatusCode, data)
	}
	cookies := response.Cookies()
	if len(cookies) == 0 || !cookies[0].Secure {
		t.Fatal("missing Secure Cookie")
	}
	result, err := cast.FromJSON[map[string]any](data)
	if err != nil {
		t.Fatal(err)
	}
	csrf := cast.String(result["csrf"])
	response = send("/api/logout", "https://evil.example", csrf, cookies[0])
	response.Body.Close()
	if response.StatusCode != 403 {
		t.Fatal("cross-origin accepted", response.StatusCode)
	}
	response = send("/api/logout", frontend.URL, csrf, cookies[0])
	response.Body.Close()
	if response.StatusCode != 200 {
		t.Fatal("default write", response.StatusCode)
	}
}
