package gateway_test

import (
	"apigo.cc/go/file"
	"apigo.cc/go/gateway/core"
	"apigo.cc/go/safe"
	"apigo.cc/go/service"
	"net"
	"net/http/httptest"
	"os"
	"path/filepath"
	"strings"
	"testing"
)

func TestUninitializedAdminLockedAndCommandPasswordSurvivesRestart(t *testing.T) {
	config := gateway.Config{DataDir: t.TempDir(), AdminListen: "127.0.0.1:0"}
	first, err := gateway.NewManager(config, service.NewWebServer())
	if err != nil {
		t.Fatal(err)
	}
	defer first.Close()
	handler := first.AdminHandler()
	response := httptest.NewRecorder()
	handler.ServeHTTP(response, httptest.NewRequest("GET", "http://localhost/", nil))
	if response.Code != 200 {
		t.Fatal("locked page unavailable")
	}
	if response := adminRequest(t, handler, "/api/login", map[string]string{"password": "any-test-password"}, nil, ""); response.Code != 423 {
		t.Fatalf("uninitialized login %d", response.Code)
	}
	if response := adminRequest(t, handler, "/api/settings", nil, nil, ""); response.Code != 401 {
		t.Fatal("uninitialized API was accessible")
	}
	secret := safe.NewSafeBufAndErase([]byte("command-set-password"))
	defer secret.Close()
	if err := gateway.SetAdminPassword(config, secret); err != nil {
		t.Fatal(err)
	}
	login(t, handler, "admin", "command-set-password")
	data, err := file.Read(filepath.Join(config.DataDir, "admin.password"))
	if err != nil {
		t.Fatal(err)
	}
	if strings.Contains(data, "command-set-password") || !strings.HasPrefix(data, "$2a$") {
		t.Fatal("credential is not a password hash")
	}
	metadata, err := os.Stat(filepath.Join(config.DataDir, "admin.password"))
	if err != nil || metadata.Mode().Perm() != 0600 {
		t.Fatal("credential permissions")
	}
	second, err := gateway.NewManager(config, service.NewWebServer())
	if err != nil {
		t.Fatal(err)
	}
	defer second.Close()
	login(t, second.AdminHandler(), "admin", "command-set-password")
	short := safe.NewSafeBufAndErase([]byte("short"))
	defer short.Close()
	if err := gateway.SetAdminPassword(config, short); err == nil {
		t.Fatal("short password accepted")
	}
	login(t, second.AdminHandler(), "admin", "command-set-password")
}

func TestGeneratedAdminConfigPreservesExistingAndDefaultIsPrivate(t *testing.T) {
	listen, err := gateway.DefaultAdminListen()
	if err != nil {
		t.Fatal(err)
	}
	host, port, err := net.SplitHostPort(listen)
	if err != nil || port != "81" {
		t.Fatalf("default %q", listen)
	}
	ip := net.ParseIP(host)
	if ip == nil || (!ip.IsPrivate() && !ip.IsLoopback()) {
		t.Fatalf("public default %q", listen)
	}
	name := filepath.Join(t.TempDir(), "gateway.yaml")
	if err := gateway.WriteDefaultAdminConfig(name, gateway.Config{AdminListen: "192.168.20.2:81"}); err != nil {
		t.Fatal(err)
	}
	original, err := file.Read(name)
	if err != nil || !strings.Contains(original, "192.168.20.2:81") {
		t.Fatal("template")
	}
	if err := gateway.WriteDefaultAdminConfig(name, gateway.Config{AdminDisabled: true}); err != nil {
		t.Fatal(err)
	}
	retained, _ := file.Read(name)
	if original != retained {
		t.Fatal("existing file overwritten")
	}
}

func TestAdminLoginBudget(t *testing.T) {
	config := gateway.Config{DataDir: t.TempDir(), AdminDisabled: true}
	m, err := gateway.NewManager(config, service.NewWebServer())
	if err != nil {
		t.Fatal(err)
	}
	defer m.Close()
	handler := m.AdminHandler()
	for i := 0; i < 20; i++ {
		if response := adminRequest(t, handler, "/api/login", map[string]string{"password": "not-initialized"}, nil, ""); response.Code != 423 {
			t.Fatal(response.Code)
		}
	}
	if response := adminRequest(t, handler, "/api/login", map[string]string{"password": "not-initialized"}, nil, ""); response.Code != 429 {
		t.Fatal("unbounded login attempts")
	}
}
