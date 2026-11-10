package gateway_test

import (
	"github.com/api-go/cast"
	gateway "github.com/api-go/gateway/core"
	"github.com/api-go/log"
	"github.com/api-go/redis"
	"github.com/api-go/service"
	"context"
	"fmt"
	"net/http"
	"net/http/httptest"
	"os"
	"testing"
	"time"
)

func TestAffinitySnapshotReplayAndPeerAuthentication(t *testing.T) {
	if os.Getenv("GATEWAY_REDIS_INTEGRATION") != "1" {
		t.Skip("requires Redis")
	}
	rd := redis.GetRedis("127.0.0.1:6379", log.DefaultLogger)
	sessions, replay := 600, 10
	if os.Getenv("GATEWAY_AFFINITY_RELIABILITY") == "1" {
		sessions, replay = 20000, 1000
	}
	prefix := fmt.Sprintf("gateway-sync-%d", time.Now().UnixNano())
	t.Cleanup(func() {
		for _, key := range rd.Do("KEYS", prefix+":*").Strings() {
			rd.Do("DEL", key)
		}
	})
	peer := httptest.NewUnstartedServer(http.NotFoundHandler())
	config := gateway.Config{Redis: "127.0.0.1:6379", Prefix: prefix, NodeID: "owner", DataDir: t.TempDir(), AffinityAdvertise: "http://" + peer.Listener.Addr().String()}
	server := service.NewWebServer()
	owner, err := gateway.NewManager(config, server)
	if err != nil {
		t.Fatal(err)
	}
	defer owner.Close()
	peer.Config.Handler = owner.AdminHandler()
	peer.Start()
	defer peer.Close()
	startAffinityServer(t, server)
	joinerServer := service.NewWebServer()
	startAffinityServer(t, joinerServer)
	app := affinityUpstreams(t)
	if err := owner.SyncAffinity(context.Background()); err != nil {
		t.Fatal(err)
	}
	host := baseHost()
	host.Proxies = []service.ProxyRule{{Path: "/*", To: app + "/"}}
	host.Affinity = gateway.AffinityConfig{Mode: "auto", Source: "header", Name: "sessionid"}
	savePublish(t, owner, host, 0)
	// More than one snapshot page, without one Redis request on each new session.
	expected := make([]string, sessions)
	for i := range sessions {
		response := affinityRequestFor(t, owner, "/", "header", fmt.Sprintf("session-%d", i))
		if response.Code != 200 {
			t.Fatal(response.Code)
		}
		expected[i] = response.Body.String()
	}
	if rd.Do("EXISTS", prefix+":affinity-journal").Int() != 0 {
		t.Fatal("proxy request wrote Redis")
	}
	for {
		if err := owner.SyncAffinity(context.Background()); err != nil {
			t.Fatal(err)
		}
		if cast.Int(owner.AffinityStatus()["pending"]) == 0 {
			break
		}
	}
	config.DataDir = t.TempDir()
	config.NodeID = "joiner"
	config.AffinityAdvertise = ""
	joiner, err := gateway.NewManager(config, joinerServer)
	if err != nil {
		t.Fatal(err)
	}
	defer joiner.Close()
	if err := joiner.SyncAffinity(context.Background()); err != nil {
		t.Fatal(err)
	}
	if cast.Int(joiner.AffinityStatus()["entries"]) != sessions {
		t.Fatal("paginated peer snapshot incomplete")
	}
	for i, node := range expected {
		response := affinityRequestFor(t, joiner, "/", "header", fmt.Sprintf("session-%d", i))
		if response.Code != 200 || response.Body.String() != node {
			t.Fatalf("snapshot route mismatch session=%d status=%d node=%s expected=%s", i, response.Code, response.Body, node)
		}
	}
	// The joining node has no subscription. A background cursor still catches new data.
	for i := sessions; i < sessions+replay; i++ {
		affinityRequestFor(t, owner, "/", "header", fmt.Sprintf("session-%d", i))
	}
	if err := owner.SyncAffinity(context.Background()); err != nil {
		t.Fatal(err)
	}
	if err := joiner.SyncAffinity(context.Background()); err != nil {
		t.Fatal(err)
	}
	if cast.Int(joiner.AffinityStatus()["entries"]) != sessions+replay {
		t.Fatal("missed notification was not replayed")
	}
	// Force the recreated stream to reuse exactly the previous cursor ID. Ordering
	// alone must not conceal new data in that generation.
	oldCursor := rd.Do("EVAL", `local r=redis.call('XREVRANGE',KEYS[1],'+','-','COUNT',1); if #r==0 then return '' end; return r[1][1]`, 1, prefix+":affinity-journal").String()
	if oldCursor == "" {
		t.Fatal("expected an existing cursor row")
	}
	// A reader beyond the retained log window recovers from the existing node API.
	rd.Do("DEL", prefix+":affinity-journal")
	if err := owner.SyncAffinity(context.Background()); err != nil {
		t.Fatal(err)
	}
	affinityRequestFor(t, owner, "/", "header", "after-log-gap")
	if err := owner.SyncAffinity(context.Background()); err != nil {
		t.Fatal(err)
	}
	newData := rd.Do("EVAL", `local r=redis.call('XREVRANGE',KEYS[1],'+','-','COUNT',1); if #r==0 then return '' end; return r[1][2][2]`, 1, prefix+":affinity-journal").String()
	if newData == "" {
		t.Fatal("expected replacement journal data")
	}
	rd.Do("DEL", prefix+":affinity-journal")
	if result := rd.Do("XADD", prefix+":affinity-journal", oldCursor, "data", newData); result.Error != nil {
		t.Fatal(result.Error)
	}
	rd.Do("EXPIRE", prefix+":affinity-journal", 300)
	if err := joiner.SyncAffinity(context.Background()); err != nil {
		t.Fatal(err)
	}
	if cast.Int(joiner.AffinityStatus()["entries"]) != sessions+replay+1 {
		t.Fatal("recreated journal with reused cursor ID not recovered")
	}
	if err := joiner.SyncAffinity(context.Background()); err != nil {
		t.Fatal(err)
	}
	if err := owner.SyncAffinity(context.Background()); err != nil {
		t.Fatal(err)
	}
	response, err := http.Get(peer.URL + "/api/affinity/peer")
	if err != nil {
		t.Fatal(err)
	}
	response.Body.Close()
	if response.StatusCode != 401 {
		t.Fatal("snapshot exposed without peer authentication")
	}
	t.Logf("SYNC RESULT snapshot=%d replay=%d gapRecovery=%d journalBytes=%v", sessions, replay, sessions+replay+1, rd.Do("MEMORY", "USAGE", prefix+":affinity-journal").Int())
	// Redis contains only an expiring bounded journal, not one key per live session.
	if keys := rd.Do("KEYS", prefix+":affinity:*").Strings(); len(keys) != 0 {
		t.Fatal(keys)
	}
	if ttl := rd.Do("TTL", prefix+":affinity-journal").Int(); ttl < 1 || ttl > 300 {
		t.Fatal("journal not temporary")
	}
}
