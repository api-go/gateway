package gateway_test

import (
	"fmt"
	"io"
	"net/http"
	"net/http/httptest"
	"os"
	"sort"
	"strings"
	"sync"
	"testing"
	"time"

	"apigo.cc/go/discover"
	gateway "apigo.cc/go/gateway/core"
	"apigo.cc/go/service"
)

// Opt-in end-to-upstream workload. Every phase checks every session, including
// sessions unaffected by membership changes; request failures are never retries.
func TestAffinityReliability(t *testing.T) {
	if os.Getenv("GATEWAY_AFFINITY_RELIABILITY") != "1" {
		t.Skip("opt-in volume test with isolated Redis")
	}
	const sessions = 10000
	const concurrency = 64
	for _, mode := range []string{"off", "auto", "hash", "manual"} {
		t.Run(mode, func(t *testing.T) {
			m, server, _ := manager(t)
			startAffinityServer(t, server)
			app := affinityUpstreams(t)
			addresses := []string{}
			labels := map[string]string{}
			for address := range discover.GetAppNodes(app) {
				addresses = append(addresses, address)
			}
			sort.Strings(addresses)
			for _, address := range addresses {
				r, err := http.Get("http://" + address)
				if err != nil {
					t.Fatal(err)
				}
				b, _ := io.ReadAll(r.Body)
				r.Body.Close()
				labels[address] = string(b)
			}
			host := baseHost()
			host.Proxies = []service.ProxyRule{{Path: "/*", To: app + "/"}}
			affinity := gateway.AffinityConfig{Mode: mode, Source: "query", Name: "sessionid", Missing: "reject"}
			if mode == "manual" {
				affinity.Transform = "lastDigit"
				for digit := 0; digit < 10; digit++ {
					p := digit % 3
					affinity.Mappings = append(affinity.Mappings, gateway.AffinityMapping{Values: []string{fmt.Sprint(digit)}, Nodes: []string{addresses[p], addresses[(p+1)%3], addresses[(p+2)%3]}})
				}
			}
			host.AffinityRules = []gateway.AffinityRule{{ID: "volume", Path: "/sticky/*", Affinity: affinity}}
			savePublish(t, m, host, 0)
			phase := func(name string, repeats int) []string {
				result := make([]string, sessions)
				latency := make([]time.Duration, sessions*repeats)
				jobs := make(chan int, concurrency)
				var wg sync.WaitGroup
				started := time.Now()
				for range concurrency {
					wg.Add(1)
					go func() {
						defer wg.Done()
						for index := range jobs {
							id := index % sessions
							begin := time.Now()
							r := affinityRequestFor(t, m, fmt.Sprintf("/sticky/get?sessionid=%d", id), "", "")
							latency[index] = time.Since(begin)
							if r.Code != 200 {
								t.Errorf("%s request %d: status %d", name, index, r.Code)
								continue
							}
							// Each session is handled by one job per repeat, phases are sequential.
							if index < sessions {
								result[id] = r.Body.String()
							}
						}
					}()
				}
				for i := 0; i < sessions*repeats; i++ {
					jobs <- i
				}
				close(jobs)
				wg.Wait()
				elapsed := time.Since(started)
				sort.Slice(latency, func(i, j int) bool { return latency[i] < latency[j] })
				counts := map[string]int{}
				for _, v := range result {
					counts[v]++
				}
				t.Logf("RESULT mode=%s phase=%s requests=%d concurrency=%d elapsed=%s qps=%.0f p50=%s p95=%s p99=%s distribution=%v entries=%v", mode, name, len(latency), concurrency, elapsed, float64(len(latency))/elapsed.Seconds(), latency[len(latency)/2], latency[len(latency)*95/100], latency[len(latency)*99/100], counts, m.AffinityStatus()["entries"])
				return result
			}
			first := phase("initial", 1)
			if mode == "manual" {
				for id, node := range first {
					if node != labels[addresses[(id%10)%3]] {
						t.Fatalf("manual primary mismatch for %d", id)
					}
				}
			}
			if mode == "off" {
				counts := map[string]int{}
				for _, node := range first {
					counts[node]++
				}
				if len(counts) != 3 {
					t.Fatal("off mode did not balance across all nodes")
				}
				if m.AffinityStatus()["entries"] != 0 {
					t.Fatal("off mode allocated mappings")
				}
			}
			repeat := phase("repeat", 1)
			check := func(name string, a, b []string, allowed func(int, string, string) bool) int {
				moved := 0
				for i := range a {
					if a[i] != b[i] {
						moved++
						if !allowed(i, a[i], b[i]) {
							t.Fatalf("%s invalid movement session=%d %s -> %s", name, i, a[i], b[i])
						}
					}
				}
				t.Logf("MOVEMENT mode=%s phase=%s moved=%d/%d", mode, name, moved, sessions)
				return moved
			}
			check("repeat", first, repeat, func(_ int, _, _ string) bool { return mode == "off" })
			added := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) { io.WriteString(w, "four") }))
			defer added.Close()
			addedAddr := strings.TrimPrefix(added.URL, "http://")
			affinityFixtureNode(app, addedAddr, 100)
			expanded := phase("expand-3-to-4", 1)
			moved := check("expansion", first, expanded, func(_ int, _, new string) bool { return mode == "off" || mode == "hash" && new == "four" })
			if mode == "hash" && (moved < 2000 || moved > 3000) {
				t.Fatalf("hash expansion movement outside 20-30%%: %d", moved)
			}
			affinityFixtureNode(app, addresses[0], 0)
			failed := phase("primary-removed", 1)
			check("removal", expanded, failed, func(_ int, old, new string) bool { return mode == "off" || old == labels[addresses[0]] && new != old })
			again := phase("failure-repeat", 1)
			check("stable-backup", failed, again, func(_ int, _, _ string) bool { return mode == "off" })
			if mode == "manual" {
				for i, v := range failed {
					p := (i % 10) % 3
					if p == 0 {
						p = 1
					}
					if v != labels[addresses[p]] {
						t.Fatalf("wrong manual backup session %d", i)
					}
				}
			}
			affinityFixtureNode(app, addresses[0], 100)
			affinityFixtureNode(app, addedAddr, 0)
			restored := phase("membership-restored", 1)
			check("restore", first, restored, func(_ int, _, _ string) bool { return mode == "off" })
			if mode == "off" && len(discover.GetAppNodes(app)) < 3 {
				t.Fatal("off workload lost upstreams")
			}
		})
	}
}
