package gateway_test

import (
	"crypto/sha256"
	"fmt"
	"io"
	"net/http"
	"os"
	"strings"
	"testing"

	"github.com/api-go/cast"
	"github.com/api-go/discover"
	gateway "github.com/api-go/gateway/core"
	"github.com/api-go/service"
)

// This is the serialized scope of the previous development build, including
// retired budget fields. Dropping those zero fields must not reroute live IDs.
func legacyAffinityScope(hostID, ruleID string, config gateway.AffinityConfig) string {
	raw := fmt.Sprintf(`{"mode":%q,"source":%q,"name":%q,"ttlSeconds":%d,"missing":%q,"transform":%q,"hashStrategy":%q,"rebalanceSeconds":0,"rebalancePercent":0,"mappings":[]}`, config.Mode, config.Source, config.Name, config.TTLSeconds, config.Missing, config.Transform, config.HashStrategy)
	digest := sha256.Sum256([]byte(raw))
	return hostID + "\x00" + ruleID + "\x00" + string(digest[:])
}

func TestAffinityLegacyScopeCompatibility(t *testing.T) {
	if os.Getenv("GATEWAY_REDIS_INTEGRATION") != "1" {
		t.Skip("requires isolated registry")
	}
	m, server, _ := manager(t)
	startAffinityServer(t, server)
	app := affinityUpstreams(t)
	labels := map[string]string{}
	for address := range discover.GetAppNodes(app) {
		response, err := http.Get("http://" + address)
		if err != nil {
			t.Fatal(err)
		}
		data, err := io.ReadAll(response.Body)
		response.Body.Close()
		if err != nil {
			t.Fatal(err)
		}
		labels[address] = string(data)
	}
	var revision int64
	for _, mode := range []string{"hash", "auto"} {
		host := baseHost()
		host.Proxies = []service.ProxyRule{{Path: "/*", To: app + "/"}}
		host.Affinity = gateway.AffinityConfig{Mode: mode, Source: "header", Name: "sessionid"}
		record := savePublish(t, m, host, revision)
		revision = record.Revision
		scope := legacyAffinityScope(host.ID, "legacy", host.Affinity)
		for i := range 100 {
			id := cast.String(i)
			best, winner := "", ""
			for address := range labels {
				sum := sha256.Sum256([]byte(scope + "\x00" + app + "\x00" + id + "\x00" + address))
				if winner == "" || strings.Compare(string(sum[:]), best) > 0 {
					best, winner = string(sum[:]), address
				}
			}
			response := affinityRequestFor(t, m, "/", "header", id)
			if response.Code != 200 || response.Body.String() != labels[winner] {
				t.Fatalf("%s ID %s changed legacy owner: %d %s", mode, id, response.Code, response.Body)
			}
		}
	}
}
