package gateway_test

import (
	"bufio"
	"context"
	"fmt"
	"io"
	"net/http"
	"net/http/httptest"
	"os"
	"os/exec"
	"strings"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	"apigo.cc/go/cast"
	"apigo.cc/go/discover"
	gateway "apigo.cc/go/gateway/core"
	"apigo.cc/go/log"
	"apigo.cc/go/rand"
	"apigo.cc/go/redis"
	"apigo.cc/go/service"
	"apigo.cc/go/timer"
)

// A separate OS process is essential: Discover and Service have process globals.
// Exercise GatewayApp's real subscriptions/workers rather than manually copying maps.
func TestAffinityLVSWorker(t *testing.T) {
	raw := os.Getenv("GATEWAY_LVS_WORKER")
	if raw == "" {
		t.Skip("subprocess helper")
	}
	conf, err := cast.FromJSON[gateway.Config](raw)
	if err != nil {
		t.Fatal(err)
	}
	appName := os.Getenv("GATEWAY_LVS_UPSTREAM")
	log.DefaultLogger.SetLevel(log.CLOSE)
	gateway.GatewayConf = conf
	service.Config.App = conf.NodeID
	service.Config.Listen = "127.0.0.1:0,http"
	service.Config.Register = "redis://127.0.0.1:6379/15"
	service.Config.NoLogGets = true
	service.Config.AutoCertDir = conf.DataDir
	service.Config.Calls = map[string]service.CallConfig{appName: {Timeout: 5000}}
	app := gateway.NewGatewayApp()
	if err := app.Start(context.Background(), log.DefaultLogger); err != nil {
		t.Fatal(err)
	}
	defer app.Stop(context.Background())
	control := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.URL.Path == "/unsubscribe" {
			redis.GetRedis(conf.Redis, log.DefaultLogger).Unsubscribe(conf.Prefix + ":affinity-events")
			io.WriteString(w, "unsubscribed")
			return
		}
		nodes := []string{}
		for address := range discover.GetAppNodes(appName) {
			nodes = append(nodes, address)
		}
		raw, err := cast.ToJSONBytes(map[string]any{"affinity": app.Manager().AffinityStatus(), "nodes": nodes})
		if err != nil {
			http.Error(w, err.Error(), 500)
			return
		}
		w.Write(raw)
	}))
	defer control.Close()
	fmt.Printf("GATEWAY_LVS_READY %s %s\n", service.DefaultServer.Addr, control.URL)
	var b [1]byte
	os.Stdin.Read(b[:])
}

type affinityLVSNode struct {
	address, control string
	stop             func()
	kill             func()
}

func startAffinityLVSNode(t *testing.T, conf gateway.Config, app string) affinityLVSNode {
	t.Helper()
	exe, err := os.Executable()
	if err != nil {
		t.Fatal(err)
	}
	raw, err := cast.ToJSON(conf)
	if err != nil {
		t.Fatal(err)
	}
	ctx, cancel := context.WithCancel(context.Background())
	cmd := exec.CommandContext(ctx, exe, "-test.run=^TestAffinityLVSWorker$", "-test.timeout=10m")
	cmd.Env = append(os.Environ(), "GATEWAY_LVS_WORKER="+raw, "GATEWAY_LVS_UPSTREAM="+app, "SERVICE_IP=127.0.0.1")
	stdout, err := cmd.StdoutPipe()
	if err != nil {
		t.Fatal(err)
	}
	stdin, err := cmd.StdinPipe()
	if err != nil {
		t.Fatal(err)
	}
	cmd.Stderr = cmd.Stdout
	if err := cmd.Start(); err != nil {
		cancel()
		t.Fatal(err)
	}
	ready := make(chan []string, 1)
	drained := make(chan struct{})
	var raceDetected atomic.Bool
	var lastLine string
	go func() {
		defer close(drained)
		scanner := bufio.NewScanner(stdout)
		for scanner.Scan() {
			lastLine = scanner.Text()
			if strings.Contains(lastLine, "WARNING: DATA RACE") {
				raceDetected.Store(true)
			}
			if line, ok := strings.CutPrefix(scanner.Text(), "GATEWAY_LVS_READY "); ok {
				ready <- strings.Fields(line)
			}
		}
	}()
	var once sync.Once
	finish := func(force bool) {
		once.Do(func() {
			defer cancel()
			stdin.Close()
			if force {
				cancel()
			}
			exited := make(chan error, 1)
			go func() { <-drained; exited <- cmd.Wait() }()
			timeout, done := context.WithTimeout(context.Background(), 15*time.Second)
			defer done()
			select {
			case err := <-exited:
				if !force && err != nil {
					t.Errorf("Gateway subprocess shutdown: %v; %s", err, lastLine)
				}
			case <-timeout.Done():
				cancel()
				<-exited
				t.Error("Gateway subprocess shutdown timed out")
			}
			if raceDetected.Load() {
				t.Error("Gateway subprocess reported a data race")
			}
		})
	}
	stop := func() { finish(false) }
	t.Cleanup(stop)
	timeout, done := context.WithTimeout(context.Background(), 30*time.Second)
	defer done()
	select {
	case fields := <-ready:
		if len(fields) != 2 {
			t.Fatal(fields)
		}
		return affinityLVSNode{address: "http://" + fields[0], control: fields[1], stop: stop, kill: func() { finish(true) }}
	case <-drained:
		t.Fatalf("Gateway subprocess exited before readiness: %s", lastLine)
	case <-timeout.Done():
		t.Fatal("Gateway subprocess startup timeout")
	}
	return affinityLVSNode{}
}

func TestAffinityRandomGatewayLVS(t *testing.T) {
	if os.Getenv("GATEWAY_AFFINITY_RELIABILITY") != "1" {
		t.Skip("opt-in independent-process cluster test")
	}
	const sessions = 10000
	const workers = 64
	for _, mode := range []string{"off", "auto", "hash", "manual"} {
		t.Run(mode, func(t *testing.T) {
			prefix := fmt.Sprintf("gateway-lvs-%s-%d", mode, time.Now().UnixNano())
			appName := prefix + "-upstream"
			rd := redis.GetRedis("redis://127.0.0.1:6379/15", log.DefaultLogger)
			t.Cleanup(func() {
				for _, key := range rd.Do("KEYS", prefix+"*").Strings() {
					rd.Do("DEL", key)
				}
			})
			addresses := []string{}
			labels := map[string]string{}
			for i := range 4 {
				label := fmt.Sprintf("backend-%d", i)
				backend := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) { io.WriteString(w, label) }))
				t.Cleanup(backend.Close)
				address := strings.TrimPrefix(backend.URL, "http://")
				addresses = append(addresses, address)
				labels[address] = label
			}
			membership := func(address string, weight int) {
				if weight > 0 {
					rd.Do("HSET", appName, address, weight)
					rd.Do("SETEX", appName+"_"+address, 600, "1")
				} else {
					rd.Do("HDEL", appName, address)
					rd.Do("DEL", appName+"_"+address)
				}
				rd.Do("PUBLISH", "CH_"+appName, fmt.Sprintf("%s %d", address, weight))
			}
			for _, address := range addresses[:3] {
				membership(address, 100)
			}
			conf := gateway.Config{Redis: "redis://127.0.0.1:6379/15", Prefix: prefix, NodeID: "seed", DataDir: t.TempDir(), AdminDisabled: true}
			seed, err := gateway.NewManager(conf, service.NewWebServer())
			if err != nil {
				t.Fatal(err)
			}
			host := baseHost()
			host.Proxies = []service.ProxyRule{{Path: "/*", To: appName + "/"}}
			affinity := gateway.AffinityConfig{Mode: mode, Source: "header", Name: "sessionid", Missing: "reject"}
			if mode == "manual" {
				affinity.Transform = "lastDigit"
				for i := range 10 {
					p := i % 3
					affinity.Mappings = append(affinity.Mappings, gateway.AffinityMapping{Values: []string{fmt.Sprint(i)}, Nodes: []string{addresses[p], addresses[(p+1)%3], addresses[(p+2)%3]}})
				}
			}
			host.AffinityRules = []gateway.AffinityRule{{ID: "lvs", Path: "/*", Affinity: affinity}}
			savePublish(t, seed, host, 0)
			seed.Close()
			rd.Do("DEL", prefix+":node:seed")
			nodes := []affinityLVSNode{}
			addNode := func() {
				c := conf
				c.NodeID = fmt.Sprintf("gw-%d", len(nodes))
				c.DataDir = t.TempDir()
				c.AdminDisabled = false
				c.AdminListen = "127.0.0.1:0"
				nodes = append(nodes, startAffinityLVSNode(t, c, appName))
			}
			for range 3 {
				addNode()
			}
			client := &http.Client{Timeout: 10 * time.Second, Transport: &http.Transport{MaxIdleConns: 256, MaxIdleConnsPerHost: 64}}
			defer client.CloseIdleConnections()
			waitView := func(entries, count int) {
				t.Helper()
				ctx, cancel := context.WithTimeout(context.Background(), 30*time.Second)
				defer cancel()
				err := timer.Retry(func() error {
					if ctx.Err() != nil {
						return ctx.Err()
					}
					ok := true
					for _, node := range nodes {
						request, _ := http.NewRequestWithContext(ctx, "GET", node.control, nil)
						r, err := client.Do(request)
						if err != nil {
							ok = false
							continue
						}
						raw, _ := io.ReadAll(r.Body)
						r.Body.Close()
						status, err := cast.FromJSON[struct {
							Affinity map[string]any
							Nodes    []string
						}](raw)
						if err != nil {
							t.Fatal(err)
						}
						if !cast.Bool(status.Affinity["ready"]) || cast.Int(status.Affinity["entries"]) < entries || len(status.Nodes) != count {
							ok = false
						}
					}
					if ok {
						return nil
					}
					return fmt.Errorf("cluster did not converge entries=%d backends=%d", entries, count)
				}, timer.WithMaxRetries(600), timer.WithBackoff(50*time.Millisecond, 1))
				if err != nil {
					t.Fatal(err)
				}
			}
			waitView(0, 3)
			phase := func(name string, expected []string, allowed func(string, string) bool) []string {
				results := make([]string, sessions)
				counts := make([]atomic.Int64, len(nodes))
				var errors, moved atomic.Int64
				jobs := make(chan int, workers)
				var wg sync.WaitGroup
				begin := time.Now()
				for range workers {
					wg.Add(1)
					go func() {
						defer wg.Done()
						for id := range jobs {
							index := rand.Int(0, len(nodes)-1)
							counts[index].Add(1)
							req, _ := http.NewRequest("GET", nodes[index].address+"/lesson", nil)
							req.Host = "example.test"
							req.Header.Set("sessionid", fmt.Sprint(id))
							r, err := client.Do(req)
							if err != nil {
								errors.Add(1)
								continue
							}
							b, err := io.ReadAll(r.Body)
							r.Body.Close()
							if err != nil || r.StatusCode != 200 {
								errors.Add(1)
								continue
							}
							results[id] = string(b)
							if expected != nil && expected[id] != results[id] {
								moved.Add(1)
								if !allowed(expected[id], results[id]) {
									errors.Add(1)
								}
							}
						}
					}()
				}
				for id := range sessions {
					jobs <- id
				}
				close(jobs)
				wg.Wait()
				hits := []int64{}
				for i := range counts {
					hits = append(hits, counts[i].Load())
					if hits[i] == 0 {
						t.Error("LVS did not exercise a Gateway")
					}
				}
				t.Logf("LVS RESULT mode=%s phase=%s requests=%d gateways=%d concurrency=%d gatewayHits=%v moved=%d errors=%d elapsed=%s", mode, name, sessions, len(nodes), workers, hits, moved.Load(), errors.Load(), time.Since(begin))
				if errors.Load() != 0 {
					t.Fatalf("%d failed requests or invalid routes", errors.Load())
				}
				return results
			}
			stable := func(_, _ string) bool { return mode == "off" }
			first := phase("initial", nil, nil)
			phase("random-gateway-repeat", first, stable)
			required := 0
			if mode == "auto" {
				required = sessions
			}
			waitView(required, 3)
			addNode()
			waitView(required, 3)
			phase("gateway-join-snapshot", first, stable)
			membership(addresses[3], 100)
			waitView(required, 4)
			expanded := phase("backend-expand", first, func(_, next string) bool { return mode == "off" || mode == "hash" && next == labels[addresses[3]] })
			membership(addresses[0], 0)
			waitView(required, 3)
			failed := phase("backend-removed", expanded, func(old, next string) bool { return mode == "off" || old == labels[addresses[0]] && next != old })
			nodes[1].kill()
			nodes = append(nodes[:1], nodes[2:]...)
			phase("gateway-killed", failed, stable)
		})
	}
}
