package gateway_test

import (
	"apigo.cc/go/cast"
	goCrypto "apigo.cc/go/crypto"
	gateway "apigo.cc/go/gateway/core"
	"apigo.cc/go/log"
	"apigo.cc/go/redis"
	"apigo.cc/go/service"
	"context"
	"fmt"
	"net/http"
	"net/http/httptest"
	"os"
	"testing"
	"time"
)

func TestRedisHourlyCountersPreserveLegacyAndIncrementOnce(t *testing.T) {
	if os.Getenv("GATEWAY_REDIS_INTEGRATION") != "1" {
		t.Skip("set GATEWAY_REDIS_INTEGRATION=1")
	}
	prefix := fmt.Sprintf("gateway-hourly-v2-test-%d", time.Now().UnixNano())
	rd := redis.GetRedis("127.0.0.1:6379", log.DefaultLogger)
	t.Cleanup(func() {
		for _, key := range rd.Do("KEYS", prefix+":*").Strings() {
			rd.Do("DEL", key)
		}
	})
	config := gateway.Config{DataDir: t.TempDir(), Redis: "127.0.0.1:6379", Prefix: prefix, AdminDisabled: true}
	m, err := gateway.NewManager(config, service.NewWebServer())
	if err != nil {
		t.Fatal(err)
	}
	defer m.Close()
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {}))
	defer server.Close()
	host := baseHost()
	host.Proxies = []service.ProxyRule{{Path: "/*", To: server.URL}}
	savePublish(t, m, host, 0)
	key := prefix + ":monitor-history:" + goCrypto.SHA256ToHex([]byte(host.ID)) + ":hours"
	raw, _ := cast.ToJSONBytes([]gateway.MonitorBucket{{Time: time.Now().UTC().Truncate(time.Hour), Checks: 10, Successes: 7, DurationTotalMS: 50}})
	if err := rd.Do("SET", key, raw, "EX", 2592000).Error; err != nil {
		t.Fatal(err)
	}
	for i := 0; i < 2; i++ {
		rd.Do("DEL", prefix+":monitor-lease:"+host.ID)
		if _, err := m.RunMonitorCheck(context.Background(), host.ID); err != nil {
			t.Fatal(err)
		}
		rows, err := m.MonitorMatrix()
		if err != nil || len(rows) != 1 || len(rows[0].Buckets) != 1 || rows[0].Buckets[0].Checks != 11+i || rows[0].Buckets[0].Error == "" {
			t.Fatal("counter migration duplicated or lost data", rows, err)
		}
	}

	compact := prefix + ":monitor:" + host.ID
	if rd.Do("TYPE", compact).String() != "hash" || rd.Do("HLEN", compact).Int() != 4 {
		t.Fatal("unexpected day/hour/recent fields")
	}
	if rd.Do("EXISTS", key).Bool() || rd.Do("EXISTS", key+":v2").Bool() {
		t.Fatal("legacy keys not cleaned after migration")
	}
	if ttl := rd.Do("TTL", compact).Int(); ttl <= 0 || ttl > 2592000 {
		t.Fatal("missing retention TTL", ttl)
	}

}
