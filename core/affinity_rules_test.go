package gateway_test

import (
	"apigo.cc/go/discover"
	gateway "apigo.cc/go/gateway/core"
	"apigo.cc/go/service"
	"context"
	"fmt"
	"io"
	"net/http"
	"net/http/httptest"
	"os"
	"sort"
	"strings"
	"testing"
)

func TestManualAffinityPathsMethodsAndBackups(t *testing.T) {
	if os.Getenv("GATEWAY_REDIS_INTEGRATION") != "1" {
		t.Skip("requires Service discovery lifecycle")
	}
	m, server, _ := manager(t)
	startAffinityServer(t, server)
	app := affinityUpstreams(t)
	addresses := []string{}
	for address := range discover.GetAppNodes(app) {
		addresses = append(addresses, address)
	}
	sort.Strings(addresses)
	body := func(address string) string {
		response, err := http.Get("http://" + address)
		if err != nil {
			t.Fatal(err)
		}
		defer response.Body.Close()
		data, _ := io.ReadAll(response.Body)
		return string(data)
	}
	primary, backup := body(addresses[0]), body(addresses[1])
	host := baseHost()
	host.Proxies = []service.ProxyRule{{Path: "/*", To: app + "/"}}
	host.AffinityRules = []gateway.AffinityRule{
		{ID: "excluded", Path: "/lesson/private", Affinity: gateway.AffinityConfig{Mode: "off"}},
		{ID: "lessons", Path: "/lesson/*", Methods: []string{"GET"}, Affinity: gateway.AffinityConfig{Mode: "manual", Source: "query", Name: "lessonid", Transform: "lastDigit", Missing: "reject", Mappings: []gateway.AffinityMapping{{Values: []string{"0", "1"}, Nodes: addresses[:2]}, {Values: []string{"2", "3"}, Nodes: addresses[2:]}}}},
	}
	savePublish(t, m, host, 0)
	for _, id := range []string{"0", "10", "101", "10001"} {
		response := affinityRequestFor(t, m, "/lesson/get?lessonid="+id, "", "")
		if response.Code != 200 || response.Body.String() != primary {
			t.Fatalf("lesson %s: %d %s", id, response.Code, response.Body)
		}
	}
	for _, path := range []string{"/lesson/get", "/lesson/get?lessonid=abc"} {
		if response := affinityRequestFor(t, m, path, "", ""); response.Code != 400 {
			t.Fatalf("missing or invalid lesson: %d", response.Code)
		}
	}
	for _, path := range []string{"/other", "/lesson/private"} {
		if response := affinityRequestFor(t, m, path, "", ""); response.Code != 200 {
			t.Fatal("unmatched/off rule applied")
		}
	}
	response := httptest.NewRecorder()
	m.Handler().ServeHTTP(response, httptest.NewRequest("POST", "http://example.test/lesson/get", nil))
	if response.Code != 200 {
		t.Fatal("GET condition applied to POST")
	}
	if response := affinityRequestFor(t, m, "/lesson/get?lessonid=9", "", ""); response.Code != 502 {
		t.Fatal("unmapped manual value was arbitrarily assigned")
	}
	affinityFixtureNode(t, app, addresses[0], 0)
	for range 5 {
		response := affinityRequestFor(t, m, "/lesson/get?lessonid=101", "", "")
		if response.Code != 200 || response.Body.String() != backup {
			t.Fatal("manual backup not stable")
		}
	}
}
func TestAutomaticAffinityCheckpointAndCapacity(t *testing.T) {
	if os.Getenv("GATEWAY_REDIS_INTEGRATION") != "1" {
		t.Skip("requires Service discovery lifecycle")
	}
	config := gateway.Config{DataDir: t.TempDir(), AdminDisabled: true, MaxAffinityEntries: 1}
	server := service.NewWebServer()
	m, err := gateway.NewManager(config, server)
	if err != nil {
		t.Fatal(err)
	}
	defer m.Close()
	startAffinityServer(t, server)
	app := affinityUpstreams(t)
	host := baseHost()
	host.Proxies = []service.ProxyRule{{Path: "/*", To: app + "/"}}
	host.Affinity = gateway.AffinityConfig{Mode: "auto", Source: "header", Name: "sessionid"}
	savePublish(t, m, host, 0)
	first := affinityRequestFor(t, m, "/", "header", "first")
	if first.Code != 200 {
		t.Fatal(first.Code)
	}
	if second := affinityRequestFor(t, m, "/", "header", "second"); second.Code != 502 {
		t.Fatal("live mapping silently evicted at capacity")
	}
	if again := affinityRequestFor(t, m, "/", "header", "first"); again.Body.String() != first.Body.String() {
		t.Fatal("first mapping lost")
	}
	if err := m.SyncAffinity(context.Background()); err != nil {
		t.Fatal(err)
	}
	restarted, err := gateway.NewManager(config, service.NewWebServer())
	if err != nil {
		t.Fatal(err)
	}
	defer restarted.Close()
	if restarted.AffinityStatus()["entries"] != 1 {
		t.Fatal("checkpoint not restored")
	}
}

func TestAffinityExpansionAndHashMovement(t *testing.T) {
	if os.Getenv("GATEWAY_REDIS_INTEGRATION") != "1" {
		t.Skip("requires Service discovery lifecycle")
	}
	for _, mode := range []string{"auto", "hash"} {
		t.Run(mode, func(t *testing.T) {
			m, server, _ := manager(t)
			startAffinityServer(t, server)
			app := affinityUpstreams(t)
			host := baseHost()
			host.Proxies = []service.ProxyRule{{Path: "/*", To: app + "/"}}
			host.Affinity = gateway.AffinityConfig{Mode: mode, Source: "header", Name: "sessionid"}
			savePublish(t, m, host, 0)
			before := map[string]string{}
			for i := range 80 {
				key := fmt.Sprintf("expansion-%d", i)
				response := affinityRequestFor(t, m, "/", "header", key)
				if response.Code != 200 {
					t.Fatal(response.Code)
				}
				before[key] = response.Body.String()
			}
			added := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) { io.WriteString(w, "four") }))
			defer added.Close()
			address := strings.TrimPrefix(added.URL, "http://")
			affinityFixtureNode(t, app, address, 100)
			defer affinityFixtureNode(t, app, address, 0)
			moved := 0
			for key, old := range before {
				response := affinityRequestFor(t, m, "/", "header", key)
				if response.Body.String() != old {
					moved++
					if response.Body.String() != "four" {
						t.Fatal("unrelated old nodes remapped")
					}
				}
			}
			if mode == "auto" && moved != 0 {
				t.Fatalf("automatic mappings moved on expansion: %d", moved)
			}
			if mode == "hash" && (moved == 0 || moved == len(before)) {
				t.Fatalf("unexpected hash movement %d", moved)
			}
			affinityFixtureNode(t, app, address, 0)
			for key, old := range before {
				if response := affinityRequestFor(t, m, "/", "header", key); response.Body.String() != old {
					t.Fatal("unaffected nodes changed on removal")
				}
			}
		})
	}
}
