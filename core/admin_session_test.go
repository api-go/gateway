package gateway_test

import (
	"apigo.cc/go/file"
	gateway "apigo.cc/go/gateway/core"
	"apigo.cc/go/safe"
	"apigo.cc/go/service"
	"net/http"
	"os"
	"path/filepath"
	"strings"
	"testing"
)

func TestAdminSessionSurvivesRestartAndRevokes(t *testing.T) {
	m, _, config := manager(t)
	cookie, csrf := login(t, m.AdminHandler(), "admin", "gateway-test-password")
	files, err := os.ReadDir(filepath.Join(config.DataDir, "admin-sessions"))
	if err != nil || len(files) != 1 {
		t.Fatal(files, err)
	}
	raw, err := file.ReadBytes(filepath.Join(config.DataDir, "admin-sessions", files[0].Name()))
	if err != nil {
		t.Fatal(err)
	}
	if strings.Contains(string(raw), cookie.Value) || strings.Contains(string(raw), csrf) {
		t.Fatal("session persisted plaintext")
	}
	m.Close()
	restarted, err := gateway.NewManager(config, service.NewWebServer())
	if err != nil {
		t.Fatal(err)
	}
	defer restarted.Close()
	handler := restarted.AdminHandler()
	if w := adminRequest(t, handler, "/api/session", nil, cookie, csrf); w.Code != 200 || !strings.Contains(w.Body.String(), csrf) {
		t.Fatal("lost session", w.Code, w.Body.String())
	}
	forged := *cookie
	forged.Value = strings.Repeat("a", 64)
	if w := adminRequest(t, handler, "/api/session", nil, &forged, csrf); w.Code != 401 {
		t.Fatal("forgery accepted")
	}
	if w := adminRequest(t, handler, "/api/logout", map[string]any{}, cookie, csrf); w.Code != 200 {
		t.Fatal(w.Code)
	}
	if w := adminRequest(t, handler, "/api/session", nil, cookie, csrf); w.Code != 401 {
		t.Fatal("logout did not revoke")
	}
	cookie, csrf = login(t, handler, "admin", "gateway-test-password")
	password := safe.NewSafeBufAndErase([]byte("replacement-password"))
	defer password.Close()
	if err := gateway.SetAdminPassword(config, password); err != nil {
		t.Fatal(err)
	}
	if w := adminRequest(t, handler, "/api/session", nil, cookie, csrf); w.Code != http.StatusUnauthorized {
		t.Fatal("password change did not revoke")
	}
}
