package gateway_test

import (
	"apigo.cc/go/cast"
	goCrypto "apigo.cc/go/crypto"
	"apigo.cc/go/file"
	gateway "apigo.cc/go/gateway/core"
	"apigo.cc/go/log"
	"apigo.cc/go/redis"
	"apigo.cc/go/safe"
	"apigo.cc/go/service"
	"bytes"
	"fmt"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"
)

func machineRequest(t testing.TB, h http.Handler, path, token string, body any) *httptest.ResponseRecorder {
	t.Helper()
	method := "GET"
	var data []byte
	if body != nil {
		method = "POST"
		data, _ = cast.ToJSONBytes(body)
	}
	r := httptest.NewRequest(method, path, bytes.NewReader(data))
	r.Header.Set("Authorization", "Bearer "+token)
	r.Header.Set("Content-Type", "application/json")
	w := httptest.NewRecorder()
	h.ServeHTTP(w, r)
	return w
}
func TestAPIKeyLifecycleAndHostPermissions(t *testing.T) {
	m, _, config := manager(t)
	h := m.AdminHandler()
	cookie, csrf := login(t, h, "admin", "gateway-test-password")
	create := func(permissions []string) map[string]any {
		listing := adminRequest(t, h, "/api/keys", nil, cookie, csrf)
		state, _ := cast.FromJSON[map[string]any](listing.Body.Bytes())
		body := map[string]any{"name": "automation", "permissions": permissions, "hostIDs": []string{"test-host"}, "revision": state["revision"]}
		if w := adminRequest(t, h, "/api/keys/create", body, cookie, ""); w.Code != 403 {
			t.Fatal("missing CSRF accepted")
		}
		w := adminRequest(t, h, "/api/keys/create", body, cookie, csrf)
		if w.Code != 200 {
			t.Fatalf("create %d %s", w.Code, w.Body.String())
		}
		result, err := cast.FromJSON[map[string]any](w.Body.Bytes())
		if err != nil {
			t.Fatal(err)
		}
		return result
	}
	created := create([]string{"read", "draft", "publish", "delete"})
	token := cast.String(created["token"])
	key, _ := cast.ToMap[string, any](created["key"])
	if token == "" || cast.String(key["hash"]) != "" {
		t.Fatal("secret response")
	}
	w := adminRequest(t, h, "/api/keys", nil, cookie, csrf)
	if strings.Contains(w.Body.String(), token) {
		t.Fatal("token leaked")
	}
	if w := machineRequest(t, h, "/api/hosts", token, nil); w.Code != 401 {
		t.Fatal("Key accessed admin API")
	}
	if w := machineRequest(t, h, "/api/v1/hosts", token+"x", nil); w.Code != 401 {
		t.Fatal("bad Key accepted")
	}
	other := baseHost()
	other.ID = "other"
	if w := machineRequest(t, h, "/api/v1/draft", token, map[string]any{"host": other}); w.Code != 403 {
		t.Fatal("Host scope bypass")
	}
	draft := machineRequest(t, h, "/api/v1/draft", token, map[string]any{"host": baseHost(), "revision": 0})
	if draft.Code != 200 {
		t.Fatal(draft.Body.String())
	}
	record, _ := cast.FromJSON[gateway.HostRecord](draft.Body.Bytes())
	if w := machineRequest(t, h, "/api/v1/draft", token, map[string]any{"host": baseHost(), "revision": 0}); w.Code != 409 {
		t.Fatal("revision bypass")
	}
	if w := machineRequest(t, h, "/api/v1/preview", token, gateway.PreviewInput{Host: baseHost(), Domain: "example.test", Path: "/"}); w.Code != 200 {
		t.Fatal(w.Body.String())
	}
	if w := machineRequest(t, h, "/api/v1/publish", token, map[string]any{"host": map[string]string{"id": "test-host"}, "revision": record.Revision}); w.Code != 200 {
		t.Fatal(w.Body.String())
	}
	if w := machineRequest(t, h, "/api/v1/nodes", token, nil); w.Code != 200 {
		t.Fatal(w.Body.String())
	}
	readonly := create([]string{"read"})
	if w := machineRequest(t, h, "/api/v1/delete", cast.String(readonly["token"]), map[string]any{"host": map[string]string{"id": "test-host"}}); w.Code != 403 {
		t.Fatal("permission bypass")
	}
	restarted, err := gateway.NewManager(config, service.NewWebServer())
	if err != nil {
		t.Fatal(err)
	}
	defer restarted.Close()
	if w := machineRequest(t, restarted.AdminHandler(), "/api/v1/hosts", token, nil); w.Code != 200 {
		t.Fatal("Key not persisted")
	}
	listing := adminRequest(t, h, "/api/keys", nil, cookie, csrf)
	state, _ := cast.FromJSON[map[string]any](listing.Body.Bytes())
	if w := adminRequest(t, h, "/api/keys/revoke", map[string]any{"id": key["id"], "revision": state["revision"]}, cookie, csrf); w.Code != 200 {
		t.Fatal(w.Body.String())
	}
	if w := machineRequest(t, restarted.AdminHandler(), "/api/v1/hosts", token, nil); w.Code != 401 {
		t.Fatal("revoked Key accepted")
	}
	guide := machineRequest(t, h, "/api-guide.txt", "", nil)
	if guide.Code != 200 || !strings.Contains(guide.Body.String(), "/api/v1/preview") {
		t.Fatal("guide missing")
	}
}
func TestAPIKeyExpiration(t *testing.T) {
	m, _, _ := manager(t)
	h := m.AdminHandler()
	cookie, csrf := login(t, h, "admin", "gateway-test-password")
	w := adminRequest(t, h, "/api/keys/create", map[string]any{"name": "expired", "permissions": []string{"read"}, "hostIDs": []string{"*"}, "expires": time.Now().Add(-time.Second), "revision": 0}, cookie, csrf)
	if w.Code != 400 {
		t.Fatal("past expiry accepted")
	}
}

func TestAPIKeyExpiredAuthentication(t *testing.T) {
	m, _, config := manager(t)
	h := m.AdminHandler()
	cookie, csrf := login(t, h, "admin", "gateway-test-password")
	w := adminRequest(t, h, "/api/keys/create", map[string]any{"name": "expiring", "permissions": []string{"read"}, "hostIDs": []string{"*"}, "revision": 0}, cookie, csrf)
	if w.Code != 200 {
		t.Fatal(w.Body.String())
	}
	result, _ := cast.FromJSON[map[string]any](w.Body.Bytes())
	token := cast.String(result["token"])
	path := filepath.Join(config.DataDir, "state.json")
	raw, err := file.ReadBytes(path)
	if err != nil {
		t.Fatal(err)
	}
	state, err := cast.FromJSON[gateway.State](raw)
	if err != nil {
		t.Fatal(err)
	}
	for id, key := range state.APIKeys {
		key.Expires = time.Now().Add(-time.Second)
		state.APIKeys[id] = key
	}
	raw, _ = cast.ToJSONBytes(state)
	if err := file.WriteBytes(path, raw); err != nil {
		t.Fatal(err)
	}
	if w := machineRequest(t, h, "/api/v1/hosts", token, nil); w.Code != 401 {
		t.Fatal("expired Key accepted")
	}
}
func TestAPIKeyClusterRevocation(t *testing.T) {
	if os.Getenv("GATEWAY_REDIS_INTEGRATION") != "1" {
		t.Skip("set GATEWAY_REDIS_INTEGRATION=1")
	}
	prefix := fmt.Sprintf("gateway-api-key-test-%d", time.Now().UnixNano())
	rd := redis.GetRedis("127.0.0.1:6379", log.DefaultLogger)
	t.Cleanup(func() {
		for _, key := range rd.Do("KEYS", prefix+":*").Strings() {
			rd.Do("DEL", key)
		}
	})
	config := gateway.Config{DataDir: t.TempDir(), Redis: "127.0.0.1:6379", Prefix: prefix, AdminDisabled: true}
	password := safe.NewSafeBufAndErase([]byte("gateway-test-password"))
	defer password.Close()
	if err := gateway.SetAdminPassword(config, password); err != nil {
		t.Fatal(err)
	}
	one, err := gateway.NewManager(config, service.NewWebServer())
	if err != nil {
		t.Fatal(err)
	}
	defer one.Close()
	config.DataDir = t.TempDir()
	two, err := gateway.NewManager(config, service.NewWebServer())
	if err != nil {
		t.Fatal(err)
	}
	defer two.Close()
	h := one.AdminHandler()
	cookie, csrf := login(t, h, "admin", "gateway-test-password")
	if w := adminRequest(t, two.AdminHandler(), "/api/session", nil, cookie, csrf); w.Code != 200 {
		t.Fatal("cluster session not shared", w.Code)
	}

	w := adminRequest(t, h, "/api/keys/create", map[string]any{"name": "cluster", "permissions": []string{"read"}, "hostIDs": []string{"*"}, "revision": 0}, cookie, csrf)
	if w.Code != 200 {
		t.Fatal(w.Body.String())
	}
	created, _ := cast.FromJSON[map[string]any](w.Body.Bytes())
	token := cast.String(created["token"])
	key, _ := cast.ToMap[string, any](created["key"])
	if w := machineRequest(t, two.AdminHandler(), "/api/v1/hosts", token, nil); w.Code != 200 {
		t.Fatal(w.Body.String())
	}
	w = adminRequest(t, h, "/api/keys/revoke", map[string]any{"id": key["id"], "revision": created["revision"]}, cookie, csrf)
	if w.Code != 200 {
		t.Fatal(w.Body.String())
	}
	if w := machineRequest(t, two.AdminHandler(), "/api/v1/hosts", token, nil); w.Code != 401 {
		t.Fatal("cluster revocation lag")
	}
	if w := adminRequest(t, two.AdminHandler(), "/api/logout", map[string]any{}, cookie, csrf); w.Code != 200 {
		t.Fatal("cluster logout failed", w.Code)
	}
	if w := adminRequest(t, h, "/api/session", nil, cookie, csrf); w.Code != 401 {
		t.Fatal("cluster logout did not revoke", w.Code)
	}

}
func BenchmarkAPIKeyRead(b *testing.B) {
	config := gateway.Config{DataDir: b.TempDir(), AdminDisabled: true}
	m, err := gateway.NewManager(config, service.NewWebServer())
	if err != nil {
		b.Fatal(err)
	}
	defer m.Close()
	// Seed persisted state using the public schema; token is a fixture, never a deployment credential.
	state := gateway.State{Format: 1, Hosts: map[string]gateway.HostRecord{}, APIKeys: map[string]gateway.APIKey{"benchmark": {ID: "benchmark", Permissions: []string{"read"}, HostIDs: []string{"*"}, Hash: ""}}}
	token := "gw_benchmark_fixture"
	key := state.APIKeys["benchmark"]
	key.Hash = goCrypto.SHA256ToHex([]byte(token))
	state.APIKeys["benchmark"] = key
	raw, _ := cast.ToJSONBytes(state)
	if err := file.WriteBytes(filepath.Join(config.DataDir, "state.json"), raw); err != nil {
		b.Fatal(err)
	}
	h := m.AdminHandler()
	b.ResetTimer()
	for b.Loop() {
		if w := machineRequest(b, h, "/api/v1/hosts", token, nil); w.Code != 200 {
			b.Fatal(w.Body.String())
		}
	}
}
