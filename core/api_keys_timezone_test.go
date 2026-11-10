package gateway_test

import (
	"github.com/api-go/file"
	"os"
	"os/exec"
	"path/filepath"
	"testing"

	"github.com/api-go/cast"
	gateway "github.com/api-go/gateway/core"
	"github.com/api-go/service"
)

func TestPermanentAPIKeySurvivesShanghaiPersistence(t *testing.T) {
	if os.Getenv("GATEWAY_EXPIRY_TZ_CHILD") != "1" {
		cmd := exec.Command(os.Args[0], "-test.run=^TestPermanentAPIKeySurvivesShanghaiPersistence$")
		cmd.Env = append(os.Environ(), "TZ=Asia/Shanghai", "GATEWAY_EXPIRY_TZ_CHILD=1")
		if output, err := cmd.CombinedOutput(); err != nil {
			t.Fatalf("Shanghai child: %v\n%s", err, output)
		}
		return
	}
	m, _, config := manager(t)
	h := m.AdminHandler()
	cookie, csrf := login(t, h, "admin", "gateway-test-password")
	w := adminRequest(t, h, "/api/keys/create", map[string]any{"name": "permanent", "permissions": []string{"read"}, "hostIDs": []string{"*"}, "revision": 0}, cookie, csrf)
	if w.Code != 200 {
		t.Fatal(w.Body.String())
	}
	result, err := cast.FromJSON[map[string]any](w.Body.Bytes())
	if err != nil {
		t.Fatal(err)
	}
	token := cast.String(result["token"])
	// Reproduce snapshots created by the prior serializer, which wrote a naive
	// zero timestamp rather than omitting the unset expiry.
	path := filepath.Join(config.DataDir, "state.json")
	raw, err := file.ReadBytes(path)
	if err != nil {
		t.Fatal(err)
	}
	legacy, err := cast.FromJSON[map[string]any](raw)
	if err != nil {
		t.Fatal(err)
	}
	keys := cast.As(cast.ToMap[string, any](legacy["apiKeys"]))
	for id, value := range keys {
		key := cast.As(cast.ToMap[string, any](value))
		key["expires"] = "0001-01-01 00:00:00.000"
		keys[id] = key
	}
	legacy["apiKeys"] = keys
	raw, err = cast.ToJSONBytes(legacy)
	if err != nil {
		t.Fatal(err)
	}
	if err = file.WriteBytes(path, raw); err != nil {
		t.Fatal(err)
	}

	restarted, err := gateway.NewManager(config, service.NewWebServer())
	if err != nil {
		t.Fatal(err)
	}
	defer restarted.Close()
	if w := machineRequest(t, restarted.AdminHandler(), "/api/v1/hosts", token, nil); w.Code != 200 {
		t.Fatalf("permanent key rejected after persistence: %d %s", w.Code, w.Body.String())
	}
}
