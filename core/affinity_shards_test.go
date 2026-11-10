package gateway_test

import (
	"github.com/api-go/cast"
	gateway "github.com/api-go/gateway/core"
	"github.com/api-go/log"
	"github.com/api-go/rand"
	"github.com/api-go/redis"
	"github.com/api-go/service"
	"github.com/api-go/timer"
	"context"
	"crypto/sha256"
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
)

type shardTableView struct {
	Scope, App              string
	Version, Updated, Moved int64
	Nodes                   []string
	Owners                  []uint16
	Balancing               bool
}
type shardStatusView struct {
	Affinity map[string]any
	Nodes    []string
}

func TestGradualAffinityRequiresRegistry(t *testing.T) {
	m, _, _ := manager(t)
	for _, config := range []gateway.AffinityConfig{
		{Mode: "hash", Source: "header", Name: "sessionid", HashStrategy: "unknown"},
		{Mode: "auto", Source: "header", Name: "sessionid", HashStrategy: "gradual"},
	} {
		candidate := baseHost()
		candidate.Affinity = config
		if _, err := m.SaveDraft(candidate, 0, "test"); err == nil {
			t.Fatalf("invalid shard policy accepted: %+v", config)
		}
	}
	host := baseHost()
	host.Proxies = []service.ProxyRule{{Path: "/*", To: "example-app/"}}
	host.Affinity = gateway.AffinityConfig{Mode: "hash", Source: "header", Name: "sessionid", HashStrategy: "gradual"}
	many := host
	many.Affinity = gateway.AffinityConfig{}
	for i := range 65 {
		many.AffinityRules = append(many.AffinityRules, gateway.AffinityRule{ID: fmt.Sprint(i), Path: "/*", Affinity: host.Affinity})
	}
	record, err := m.SaveDraft(many, 0, "test")
	if err != nil {
		t.Fatal(err)
	}
	if _, err = m.Publish(many.ID, record.Revision, "test"); err == nil || !strings.Contains(err.Error(), "at most 64") {
		t.Fatalf("active shard table bound not enforced: %v", err)
	}
	if m.Hosts()[0].Version != 0 || m.Hosts()[0].Current != nil {
		t.Fatal("invalid shard policy was published")
	}
	record, err = m.SaveDraft(host, record.Revision, "test")
	if err != nil {
		t.Fatal(err)
	}
	if _, err = m.Publish(host.ID, record.Revision, "test"); err == nil || !strings.Contains(err.Error(), "require Redis") {
		t.Fatalf("gradual mode accepted without registry: %v", err)
	}

}

// Separate real Gateway processes, no CookieJar or route-version headers.
func TestGradualAffinityShardCluster(t *testing.T) {
	if os.Getenv("GATEWAY_AFFINITY_RELIABILITY") != "1" {
		t.Skip("opt-in shard control-plane and real HTTP cluster test")
	}
	const sessions = 10000
	const liveIDs = 100000
	prefix := fmt.Sprintf("gateway-shards-%d", time.Now().UnixNano())
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
		label := fmt.Sprintf("node-%d", i)
		server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) { io.WriteString(w, label) }))
		t.Cleanup(server.Close)
		address := strings.TrimPrefix(server.URL, "http://")
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
	host.AffinityRules = []gateway.AffinityRule{{ID: "gradual", Path: "/*", Affinity: gateway.AffinityConfig{Mode: "hash", HashStrategy: "gradual", Source: "header", Name: "sessionid", Missing: "reject"}}}
	savePublish(t, seed, host, 0)
	seed.Close()
	rd.Do("DEL", prefix+":node:seed")
	nodes := []affinityLVSNode{}
	ids := []string{}
	addNode := func() {
		c := conf
		c.NodeID = fmt.Sprintf("gw-%d", len(ids))
		ids = append(ids, c.NodeID)
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
	statuses := func() []shardStatusView {
		values := []shardStatusView{}
		for _, node := range nodes {
			response, err := client.Get(node.control)
			if err != nil {
				return nil
			}
			data, _ := io.ReadAll(response.Body)
			response.Body.Close()
			value, err := cast.FromJSON[shardStatusView](data)
			if err != nil {
				t.Fatal(err)
			}
			values = append(values, value)
		}
		return values
	}
	wait := func(name string, predicate func() bool) {
		t.Helper()
		if err := timer.Retry(func() error {
			if predicate() {
				return nil
			}
			return fmt.Errorf("waiting for %s", name)
		}, timer.WithMaxRetries(900), timer.WithBackoff(50*time.Millisecond, 1)); err != nil {
			t.Fatal(err)
		}
	}
	var key string
	readTable := func() shardTableView {
		if key == "" {
			keys := rd.Do("KEYS", prefix+":affinity-shard:*").Strings()
			if len(keys) != 1 {
				return shardTableView{}
			}
			key = keys[0]
		}
		value, err := cast.FromJSON[shardTableView](rd.Do("GET", key).String())
		if err != nil {
			return shardTableView{}
		}
		return value
	}
	aligned := func(version int64, backends int) bool {
		values := statuses()
		if len(values) != len(nodes) {
			return false
		}
		for _, status := range values {
			versions, err := cast.ToMap[string, any](status.Affinity["shardVersions"])
			if err != nil {
				return false
			}
			if !cast.Bool(status.Affinity["shardReady"]) || len(status.Nodes) != backends || len(versions) != 1 {
				return false
			}
			for _, v := range versions {
				if version > 0 && cast.Int64(v) != version {
					return false
				}
			}
		}
		return true
	}
	wait("initial tables", func() bool { return aligned(0, 3) && readTable().Version > 0 })
	initial := readTable()
	legacyScope := legacyAffinityScope(host.ID, "gradual", host.AffinityRules[0].Affinity)
	legacyDigest := sha256.Sum256([]byte(legacyScope + "\x00" + appName))
	if initial.Scope != fmt.Sprintf("%x", legacyDigest) {
		t.Fatal("existing development-build shard scope changed")
	}
	phase := func(name string, count, distinct, offset int, expected []string, check func(int, string, string) bool) []string {
		results := make([]string, distinct)
		var errors, moved atomic.Int64
		hits := make([]atomic.Int64, len(nodes))
		var wg sync.WaitGroup
		jobs := make(chan int, 64)
		begin := time.Now()
		for range 64 {
			wg.Add(1)
			go func() {
				defer wg.Done()
				for index := range jobs {
					id := index % distinct
					gw := rand.Int(0, len(nodes)-1)
					hits[gw].Add(1)
					request, _ := http.NewRequest("GET", nodes[gw].address+"/lesson", nil)
					request.Host = "example.test"
					request.Header.Set("sessionid", fmt.Sprint(id+offset))
					response, err := client.Do(request)
					if err != nil {
						errors.Add(1)
						continue
					}
					data, err := io.ReadAll(response.Body)
					response.Body.Close()
					if err != nil || response.StatusCode != 200 {
						errors.Add(1)
						continue
					}
					label := string(data)
					if index < distinct {
						results[id] = label
					}
					if expected != nil && label != expected[id] {
						moved.Add(1)
						if check == nil || !check(id, expected[id], label) {
							errors.Add(1)
						}
					}
				}
			}()
		}
		for i := range count {
			jobs <- i
		}
		close(jobs)
		wg.Wait()
		distribution := []int64{}
		for i := range hits {
			distribution = append(distribution, hits[i].Load())
			if distribution[i] == 0 {
				t.Error("Gateway not exercised")
			}
		}
		t.Logf("SHARD HTTP phase=%s requests=%d gateways=%d gatewayHits=%v moved=%d errors=%d elapsed=%s", name, count, len(nodes), distribution, moved.Load(), errors.Load(), time.Since(begin))
		if errors.Load() != 0 {
			t.Fatalf("%s: %d errors", name, errors.Load())
		}
		return results
	}
	baseline := phase("initial", liveIDs, liveIDs, 0, nil, nil)
	before := baseline[:sessions]
	// Reuse the exact same HTTP client across different IDs. It does not store cookies.
	phase("hot-IDs", 32000, 100, 0, before[:100], nil)
	addNode()
	wait("joining Gateway snapshot", func() bool { return aligned(initial.Version, 3) })
	phase("gateway-join", sessions, sessions, 0, before, nil)
	// Lose notifications on one follower: bounded table polling/journal recovery
	// must still converge. The master continues receiving all load reports.
	leader := rd.Do("GET", prefix+":affinity-shard-master").String()
	follower := 0
	if ids[follower] == leader {
		follower = 1
	}
	response, err := client.Get(nodes[follower].control + "/unsubscribe")
	if err != nil {
		t.Fatal(err)
	}
	response.Body.Close()
	covered := 0
	wait("all 4096 shards carry prior traffic", func() bool {
		seen := make([]bool, 4096)
		for _, key := range rd.Do("KEYS", prefix+":affinity-shard-load:*").Strings() {
			report, err := cast.FromJSON[struct{ Hits []uint64 }](rd.Do("GET", key).String())
			if err != nil {
				continue
			}
			for bucket, load := range report.Hits {
				if bucket < len(seen) && load > 0 {
					seen[bucket] = true
				}
			}
		}
		covered = 0
		for _, hit := range seen {
			if hit {
				covered++
			}
		}
		return covered == 4096
	})
	expansionStarted := time.Now()
	membership(addresses[3], 100)
	wait("first gradual migration", func() bool {
		table := readTable()
		return table.Version > initial.Version && table.Moved > 0 && aligned(table.Version, 4)
	})
	expanded := readTable()
	if elapsed := time.Since(expansionStarted); elapsed >= 20*time.Second {
		t.Fatalf("expansion delayed despite complete topology/load: %s", elapsed)
	} else {
		t.Logf("SHARD SCHEDULING topology-to-all-gateways=%s", elapsed)
	}
	changed := 0
	for bucket, owner := range initial.Owners {
		old := initial.Nodes[owner]
		next := expanded.Nodes[expanded.Owners[bucket]]
		if old != next {
			changed++
			if next != addresses[3] {
				t.Fatal("expansion moved a shard between old nodes")
			}
		}
	}
	if changed != 1024 || expanded.Balancing {
		t.Fatalf("expansion should finish in one table: changed=%d pending=%v", changed, expanded.Balancing)
	}
	after := phase("low-load-expansion", sessions, sessions, 0, before, func(id int, _, next string) bool { return id >= 100 && next == labels[addresses[3]] })
	t.Logf("SHARD COVERAGE liveIDs=%d occupiedShards=%d", liveIDs, covered)
	t.Logf("SHARD TABLE shards=%d changed=%d oldVersion=%d newVersion=%d redisBytes=%d", len(expanded.Owners), changed, initial.Version, expanded.Version, rd.Do("MEMORY", "USAGE", key).Int())
	oldMoved := 0
	for id, node := range after {
		if node != before[id] {
			oldMoved++
		}
	}
	if oldMoved == 0 {
		t.Fatal("fully occupied shards should move some old IDs")
	}
	t.Logf("SHARD OLD checked=%d moved=%d hotMoved=0", sessions, oldMoved)
	newIDs := phase("new-IDs", sessions, sessions, liveIDs, nil, nil)
	newNodeHits := 0
	for _, node := range newIDs {
		if node == labels[addresses[3]] {
			newNodeHits++
		}
	}
	if newNodeHits < sessions/5 || newNodeHits > sessions*3/10 {
		t.Fatalf("new IDs should use the updated allocation: %d", newNodeHits)
	}
	t.Logf("SHARD NEW IDs=%d assignedToNewNode=%d", sessions, newNodeHits)
	// Delayed duplicate of an old notification cannot roll the table back.
	event, _ := cast.ToJSONBytes([]map[string]any{{"table": initial}})
	spamDone := make(chan struct{})
	go func() {
		defer close(spamDone)
		sent := 0
		_ = timer.Retry(func() error {
			rd.Do("PUBLISH", prefix+":affinity-events", event)
			sent++
			if sent >= 100 {
				return nil
			}
			return fmt.Errorf("repeat stale event")
		}, timer.WithMaxRetries(100), timer.WithBackoff(10*time.Millisecond, 1))
	}()
	phase("stale-version-events", sessions, sessions, liveIDs, newIDs, nil)
	<-spamDone
	wait("stale update rejected", func() bool { return aligned(expanded.Version, 4) })
	oldLeader := rd.Do("GET", prefix+":affinity-shard-master").String()
	index := -1
	for i, id := range ids {
		if id == oldLeader {
			index = i
		}
	}
	if index < 0 {
		t.Fatal("master not one of the Gateway nodes")
	}
	nodes[index].kill()
	nodes = append(nodes[:index], nodes[index+1:]...)
	ids = append(ids[:index], ids[index+1:]...)
	wait("master takeover", func() bool {
		next := rd.Do("GET", prefix+":affinity-shard-master").String()
		return next != "" && next != oldLeader && aligned(expanded.Version, 4)
	})
	phase("master-killed", sessions, sessions, 0, after, nil)
	// Hold scheduling while forwarding performs immediate local failure fallback.
	// Later persisted reassignment must choose exactly the same owner.
	rd.Do("SET", prefix+":affinity-shard-master", "test-suspended-scheduler", "EX", 15)
	membership(addresses[0], 0)
	wait("failure discovery without scheduler", func() bool { return aligned(expanded.Version, 3) })
	fallback := phase("scheduler-unavailable-fallback", sessions, sessions, 0, after, func(_ int, old, next string) bool { return old == labels[addresses[0]] && next != old })
	wait("failed backend table repair", func() bool {
		table := readTable()
		return table.Version > expanded.Version && aligned(table.Version, 3)
	})
	failed := phase("backend-failure", sessions, sessions, 0, fallback, nil)
	retained := readTable()
	editConf := conf
	editConf.NodeID = "editor"
	editConf.DataDir = t.TempDir()
	editor, err := gateway.NewManager(editConf, service.NewWebServer())
	if err != nil {
		t.Fatal(err)
	}
	host.Name = "Updated display name"
	savePublish(t, editor, host, editor.Hosts()[0].Revision)
	editor.Close()
	wait("policy update retained table", func() bool {
		return len(rd.Do("KEYS", prefix+":affinity-shard:*").Strings()) == 1 && aligned(retained.Version, 3)
	})
	phase("non-routing-config-change", sessions, sessions, 0, failed, nil)
	if readTable().Scope != retained.Scope {
		t.Fatal("non-routing config change reset shard ownership")
	}
	for _, status := range statuses() {
		if cast.Int(status.Affinity["entries"]) != 0 {
			t.Fatal("gradual mode allocated per-session mappings")
		}
	}
	t.Logf("SHARD RESULT noClientCookie=true notificationGapRecovered=true staleVersionRejected=true masterBefore=%s masterAfter=%s", oldLeader, rd.Do("GET", prefix+":affinity-shard-master").String())
}

func TestGradualSnapshotCapacityCountsSessionBindings(t *testing.T) {
	if os.Getenv("GATEWAY_REDIS_INTEGRATION") != "1" {
		t.Skip("requires isolated Redis")
	}
	prefix := fmt.Sprintf("gateway-shard-capacity-%d", time.Now().UnixNano())
	rd := redis.GetRedis("127.0.0.1:6379", log.DefaultLogger)
	t.Cleanup(func() {
		for _, key := range rd.Do("KEYS", prefix+":*").Strings() {
			rd.Do("DEL", key)
		}
	})
	peer := httptest.NewUnstartedServer(http.NotFoundHandler())
	defer peer.Close()
	conf := gateway.Config{Redis: "127.0.0.1:6379", Prefix: prefix, DataDir: t.TempDir(), NodeID: "owner", MaxAffinityEntries: 1, AffinityAdvertise: "http://" + peer.Listener.Addr().String()}
	server := service.NewWebServer()
	owner, err := gateway.NewManager(conf, server)
	if err != nil {
		t.Fatal(err)
	}
	defer owner.Close()
	peer.Config.Handler = owner.AdminHandler()
	peer.Start()
	startAffinityServer(t, server)
	app := affinityUpstreams(t)
	host := baseHost()
	host.Proxies = []service.ProxyRule{{Path: "/*", To: app + "/"}}
	for i := range 2 {
		host.AffinityRules = append(host.AffinityRules, gateway.AffinityRule{ID: fmt.Sprint(i), Path: fmt.Sprintf("/%d/*", i), Affinity: gateway.AffinityConfig{Mode: "hash", Source: "header", Name: "sessionid", HashStrategy: "gradual"}})
	}
	savePublish(t, owner, host, 0)
	if err = owner.SyncAffinity(context.Background()); err != nil {
		t.Fatal(err)
	}
	if err = owner.Reconcile(); err != nil {
		t.Fatal(err)
	}
	conf.NodeID = "joiner"
	conf.DataDir = t.TempDir()
	conf.AffinityAdvertise = ""
	joiner, err := gateway.NewManager(conf, service.NewWebServer())
	if err != nil {
		t.Fatal(err)
	}
	defer joiner.Close()
	if err = joiner.SyncAffinity(context.Background()); err != nil {
		t.Fatal(err)
	}
	status := joiner.AffinityStatus()
	if cast.Int(status["entries"]) != 0 || cast.Int(status["shardTables"]) != 2 || !cast.Bool(status["shardReady"]) {
		t.Fatalf("shard snapshot incorrectly used the session capacity: %v", status)
	}
}
