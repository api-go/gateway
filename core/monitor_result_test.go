package gateway_test

import (
	"apigo.cc/go/cast"
	"apigo.cc/go/file"
	gateway "apigo.cc/go/gateway/core"
	"apigo.cc/go/log"
	"apigo.cc/go/service"
	"context"
	"net/http"
	"net/http/httptest"
	"path/filepath"
	"testing"
	"time"
)

func TestRedirectAvailableAndExplicitStatusStrict(t *testing.T) {
	m, _, _ := manager(t)
	upstream := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Location", "https://example.invalid/login")
		w.WriteHeader(302)
	}))
	defer upstream.Close()
	host := baseHost()
	host.Proxies = []service.ProxyRule{{Path: "/*", To: upstream.URL}}
	record := savePublish(t, m, host, 0)
	sample, err := m.RunMonitorCheck(context.Background(), host.ID)
	if err != nil || !sample.OK || sample.Status != 302 {
		t.Fatal("redirect misclassified", sample, err)
	}
	host = record.Draft
	host.Monitor.ExpectedStatus = 200
	record = savePublish(t, m, host, record.Revision)
	sample, err = m.RunMonitorCheck(context.Background(), host.ID)
	if err != nil || sample.OK || sample.Failure != "http_status" {
		t.Fatal("explicit exact status ignored", sample, err)
	}
	host = record.Draft
	host.Monitor.ExpectedStatus = 0
	host.Monitor.BodyContains = "must-exist"
	savePublish(t, m, host, record.Revision)
	sample, err = m.RunMonitorCheck(context.Background(), host.ID)
	if err != nil || sample.OK || sample.Failure != "response_mismatch" {
		t.Fatal("response check bypassed", sample, err)
	}
}
func TestUpstreamConnectionFailureCategory(t *testing.T) {
	m, _, _ := manager(t)
	server := httptest.NewServer(http.HandlerFunc(func(http.ResponseWriter, *http.Request) {}))
	target := server.URL
	server.Close()
	host := baseHost()
	host.Proxies = []service.ProxyRule{{Path: "/*", To: target}}
	savePublish(t, m, host, 0)
	sample, err := m.RunMonitorCheck(context.Background(), host.ID)
	if err != nil || sample.OK || sample.Failure != "connection_failed" || sample.Domains[0].Failure != "connection_failed" {
		t.Fatal(sample, err)
	}
}
func TestGatewayLogRetentionPreservesActiveAndUnrelated(t *testing.T) {
	directory := t.TempDir()
	now := time.Date(2026, 10, 15, 12, 0, 0, 0, time.UTC)
	names := []string{"gateway.log.2026-09-15", "gateway.log.2026-09-16", "gateway.log.2026-10-15", "other.log.2020-01-01", "gateway.log.2020-01-01.backup", "gateway.log"}
	for _, name := range names {
		if err := file.Write(filepath.Join(directory, name), "fixture"); err != nil {
			t.Fatal(err)
		}
	}
	if err := gateway.PruneGatewayLogs(log.Config{File: filepath.Join(directory, "gateway.log"), SplitTag: "2006-01-02"}, 30, now); err != nil {
		t.Fatal(err)
	}
	if file.Exists(filepath.Join(directory, names[0])) {
		t.Fatal("expired log retained")
	}
	for _, name := range names[1:] {
		if !file.Exists(filepath.Join(directory, name)) {
			t.Fatal("deleted protected log", name)
		}
	}
	if err := gateway.PruneGatewayLogs(log.Config{File: filepath.Join(directory, "gateway.log")}, 30, now); err != nil {
		t.Fatal(err)
	}
}
func TestMonitorFailureHTTPClassification(t *testing.T) {
	for _, status := range []int{401, 403, 404, 500} {
		t.Run(cast.String(status), func(t *testing.T) {
			m, _, _ := manager(t)
			server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) { w.WriteHeader(status) }))
			defer server.Close()
			host := baseHost()
			host.Proxies = []service.ProxyRule{{Path: "/*", To: server.URL}}
			savePublish(t, m, host, 0)
			sample, err := m.RunMonitorCheck(context.Background(), host.ID)
			want := map[int]string{401: "unauthorized", 403: "forbidden", 404: "http_status", 500: "server_error"}[status]
			if err != nil || sample.OK || sample.Failure != want {
				t.Fatal(sample, err)
			}
		})
	}
}
