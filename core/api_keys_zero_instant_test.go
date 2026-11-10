package gateway_test

import (
	"os"
	"os/exec"
	"testing"

	"github.com/api-go/cast"
	gateway "github.com/api-go/gateway/core"
	"github.com/api-go/service"
)

func TestPermanentAPIKeyZeroInstantAcrossTimezones(t *testing.T) {
	if os.Getenv("GATEWAY_ZERO_INSTANT_TZ_CHILD") != "1" {
		for _, zone := range []string{"UTC", "Asia/Shanghai", "America/New_York"} {
			t.Run(zone, func(t *testing.T) {
				cmd := exec.Command(os.Args[0], "-test.run=^TestPermanentAPIKeyZeroInstantAcrossTimezones$")
				cmd.Env = append(os.Environ(), "TZ="+zone, "GATEWAY_ZERO_INSTANT_TZ_CHILD=1")
				if output, err := cmd.CombinedOutput(); err != nil {
					t.Fatalf("timezone child: %v\n%s", err, output)
				}
			})
		}
		return
	}
	m, _, config := manager(t)
	h := m.AdminHandler()
	cookie, csrf := login(t, h, "admin", "gateway-test-password")
	w := adminRequest(t, h, "/api/keys/create", map[string]any{"name": "permanent-zero-instant", "permissions": []string{"read"}, "hostIDs": []string{"*"}, "revision": 0}, cookie, csrf)
	if w.Code != 200 {
		t.Fatalf("create key: %d", w.Code)
	}
	created, err := cast.FromJSON[map[string]any](w.Body.Bytes())
	if err != nil {
		t.Fatal(err)
	}
	token := cast.String(created["token"])
	if w := machineRequest(t, h, "/api/v1/hosts", token, nil); w.Code != 200 {
		t.Fatalf("permanent key after current serialization: %d %s", w.Code, w.Body.String())
	}
	restarted, err := gateway.NewManager(config, service.NewWebServer())
	if err != nil {
		t.Fatal(err)
	}
	defer restarted.Close()
	if w := machineRequest(t, restarted.AdminHandler(), "/api/v1/hosts", token, nil); w.Code != 200 {
		t.Fatalf("permanent key after restart: %d %s", w.Code, w.Body.String())
	}
}
