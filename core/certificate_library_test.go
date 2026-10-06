package gateway_test

import (
	"apigo.cc/go/cast"
	"apigo.cc/go/file"
	"apigo.cc/go/gateway/core"
	"apigo.cc/go/log"
	"apigo.cc/go/redis"
	"apigo.cc/go/service"
	"context"
	"crypto/ecdsa"
	"crypto/elliptic"
	crand "crypto/rand"
	"crypto/tls"
	"crypto/x509"
	"encoding/pem"
	"fmt"
	"math/big"
	"net/http/httptest"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"
)

func expiredCertificate(t *testing.T, domain string) ([]byte, []byte) {
	t.Helper()
	key, err := ecdsa.GenerateKey(elliptic.P256(), crand.Reader)
	if err != nil {
		t.Fatal(err)
	}
	leaf := &x509.Certificate{SerialNumber: big.NewInt(90), DNSNames: []string{domain}, NotBefore: time.Now().Add(-48 * time.Hour), NotAfter: time.Now().Add(-time.Hour), KeyUsage: x509.KeyUsageDigitalSignature, ExtKeyUsage: []x509.ExtKeyUsage{x509.ExtKeyUsageServerAuth}}
	der, err := x509.CreateCertificate(crand.Reader, leaf, leaf, &key.PublicKey, key)
	if err != nil {
		t.Fatal(err)
	}
	raw, err := x509.MarshalPKCS8PrivateKey(key)
	if err != nil {
		t.Fatal(err)
	}
	return pem.EncodeToMemory(&pem.Block{Type: "CERTIFICATE", Bytes: der}), pem.EncodeToMemory(&pem.Block{Type: "PRIVATE KEY", Bytes: raw})
}
func TestCertificateLibraryAdaptiveMatchingAndFallback(t *testing.T) {
	m, server, config := manager(t)
	server.Config.Listen = "127.0.0.1:0,https"
	config.HTTPSPort = 10443
	// Use a real TLS listener and an explicit public port for HTTP redirects.
	other := service.NewWebServer()
	other.Config.NoLogGets = true
	other.Config.Listen = "127.0.0.1:0,https"
	m.Close()
	var err error
	m, err = gateway.NewManager(config, other)
	if err != nil {
		t.Fatal(err)
	}
	defer m.Close()
	server = other
	host := baseHost()
	host.HTTPS = gateway.HTTPSConfig{}
	host.Domains = []string{"api.example.test", "www.example.test", "example.test", "deep.api.example.test"}
	host.Rewrites = []service.RewriteRule{{Path: "/", To: "https://upstream.test/"}}
	savePublish(t, m, host, 0)
	request := func(domain string) *httptest.ResponseRecorder {
		w := httptest.NewRecorder()
		server.Handler().ServeHTTP(w, httptest.NewRequest("GET", "http://"+domain+"/", nil))
		return w
	}
	if request(host.Domains[0]).Header().Get("Location") != "https://upstream.test/" {
		t.Fatal("HTTP fallback did not serve routes")
	}
	wildcard, key := certificate(t, "*.example.test")
	if _, err := m.SaveCertificate(gateway.CertificateRecord{ID: "wild", Name: "Wildcard", Mode: "upload", Certificate: string(wildcard)}, key, 0, "test"); err != nil {
		t.Fatal(err)
	}
	exact, key := certificate(t, "api.example.test")
	if _, err := m.SaveCertificate(gateway.CertificateRecord{ID: "exact", Name: "Exact", Mode: "upload", Certificate: string(exact)}, key, 0, "test"); err != nil {
		t.Fatal(err)
	}
	statuses := m.Hosts()[0].DomainTLS
	if statuses[0].CertificateID != "exact" || statuses[1].CertificateID != "wild" || statuses[2].State != "http" || statuses[3].State != "http" {
		t.Fatalf("wrong matching: %+v", statuses)
	}
	if got := request(host.Domains[0]); got.Code != 308 || got.Header().Get("Location") != "https://api.example.test:10443/" {
		t.Fatalf("redirect %d %s", got.Code, got.Header().Get("Location"))
	}
	if err := server.Start(context.Background(), log.DefaultLogger); err != nil {
		t.Fatal(err)
	}
	defer server.Stop(context.Background())
	for domain, want := range map[string]string{"api.example.test": "api.example.test", "www.example.test": "*.example.test"} {
		conn, err := tls.Dial("tcp", server.Addrs[0], &tls.Config{InsecureSkipVerify: true, ServerName: domain})
		if err != nil {
			t.Fatal(err)
		}
		if got := conn.ConnectionState().PeerCertificates[0].DNSNames[0]; got != want {
			t.Fatalf("%s served %s", domain, got)
		}
		conn.Close()
	}

	active := m.Hosts()[0]
	off := active.Draft
	off.HTTPS.Mode = "off"
	offRecord := savePublish(t, m, off, active.Revision)
	if connection, err := tls.Dial("tcp", server.Addrs[0], &tls.Config{InsecureSkipVerify: true, ServerName: host.Domains[0]}); err == nil {
		connection.Close()
		t.Fatal("HTTP-only Host served library certificate")
	}
	host.HTTPS.Mode = "adaptive"
	savePublish(t, m, host, offRecord.Revision)
	if err := m.DeleteCertificate("exact", 1, "test"); err != nil {
		t.Fatal(err)
	}
	if m.Hosts()[0].DomainTLS[0].CertificateID != "wild" {
		t.Fatal("did not fallback to wildcard")
	}
	if err := m.DeleteCertificate("wild", 1, "test"); err != nil {
		t.Fatal(err)
	}
	if request(host.Domains[0]).Header().Get("Location") != "https://upstream.test/" {
		t.Fatal("delete did not restore HTTP")
	}
	expired, key := expiredCertificate(t, host.Domains[0])
	if _, err := m.SaveCertificate(gateway.CertificateRecord{ID: "expired", Mode: "upload", Certificate: string(expired)}, key, 0, "test"); err != nil {
		t.Fatal(err)
	}
	if m.Hosts()[0].DomainTLS[0].State != "http" || m.Certificates()[0].Info.State != "expired" {
		t.Fatal("expired certificate enabled HTTPS")
	}
	raw, err := file.Read(filepath.Join(config.DataDir, "state.json"))
	if err != nil || strings.Contains(raw, "PRIVATE KEY") {
		t.Fatal("private key persisted unencrypted", err)
	}
	listed := m.Certificates()
	listed[0].Domains[0] = "changed"
	if m.Certificates()[0].Domains[0] == "changed" {
		t.Fatal("certificate read mutated state")
	}
	bytes, _ := cast.ToJSONBytes(m.Certificates())
	if strings.Contains(string(bytes), "PRIVATE KEY") || strings.Contains(string(bytes), "BEGIN CERTIFICATE") {
		t.Fatal("library API leaked PEM")
	}
}
func TestCertificateLibrarySettingsAndAdminAPI(t *testing.T) {
	m, _, config := manager(t)
	handler := m.AdminHandler()
	cookie, csrf := login(t, handler, "admin", "gateway-test-password")
	if r := adminRequest(t, handler, "/api/acme", nil, cookie, ""); r.Code != 200 {
		t.Fatal(r.Code)
	}
	_, revision := m.ACMESettings()
	settings := gateway.ACMESettings{Email: "admin@example.test", AcceptTerms: true, Directory: "http://127.0.0.1:1089/directory"}
	if err := m.SetACMESettings(settings, revision); err != nil {
		t.Fatal(err)
	}
	if err := m.SetACMESettings(settings, revision); err == nil {
		t.Fatal("stale revision accepted")
	}
	cert, key := certificate(t, "example.test")
	body := map[string]any{"certificate": gateway.CertificateRecord{ID: "shared", Name: "Central", Mode: "upload", Certificate: string(cert)}, "key": string(key), "revision": 0}
	if r := adminRequest(t, handler, "/api/certificates/save", body, cookie, ""); r.Code != 403 {
		t.Fatal("missing CSRF accepted")
	}
	if r := adminRequest(t, handler, "/api/certificates/save", body, cookie, csrf); r.Code != 200 {
		t.Fatal(r.Code, r.Body.String())
	}
	if r := adminRequest(t, handler, "/api/certificates", nil, cookie, ""); r.Code != 200 || strings.Contains(r.Body.String(), "PRIVATE KEY") {
		t.Fatal(r.Code, r.Body.String())
	}
	if _, err := m.SaveCertificate(gateway.CertificateRecord{ID: "shared", Mode: "auto", Domains: []string{"*.example.test"}}, nil, 1, "test"); err == nil {
		t.Fatal("HTTP01 wildcard accepted")
	}
	m.Close()
	other, err := gateway.NewManager(config, service.NewWebServer())
	if err != nil {
		t.Fatal(err)
	}
	defer other.Close()
	restored, _ := other.ACMESettings()
	if restored != settings || len(other.Certificates()) != 1 {
		t.Fatal("certificate/settings not restored")
	}
}

func TestCertificateLibraryClusterConvergence(t *testing.T) {
	if os.Getenv("GATEWAY_REDIS_INTEGRATION") != "1" {
		t.Skip("set GATEWAY_REDIS_INTEGRATION=1")
	}
	t.Setenv("GATEWAY_CERTIFICATE_KEY", "")
	prefix := fmt.Sprintf("gateway-certificate-test-%d", time.Now().UnixNano())
	rd := redis.GetRedis("127.0.0.1:6379", log.DefaultLogger)
	t.Cleanup(func() {
		for _, key := range rd.Do("KEYS", prefix+":*").Strings() {
			rd.Do("DEL", key)
		}
	})
	config := gateway.Config{DataDir: t.TempDir(), Redis: "127.0.0.1:6379", Prefix: prefix, AdminDisabled: true}
	unshared, err := gateway.NewManager(config, service.NewWebServer())
	if err != nil {
		t.Fatal(err)
	}
	cert, key := certificate(t, "example.test")
	if _, err := unshared.SaveCertificate(gateway.CertificateRecord{ID: "central", Mode: "upload", Certificate: string(cert)}, key, 0, "test"); err == nil {
		t.Fatal("cluster upload accepted without shared key")
	}
	unshared.Close()
	config.CertificateKey = strings.Repeat("12", 32)
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
	host := baseHost()
	host.HTTPS = gateway.HTTPSConfig{Mode: "adaptive"}
	savePublish(t, one, host, 0)
	cert, key = certificate(t, "example.test")
	if _, err := one.SaveCertificate(gateway.CertificateRecord{ID: "central", Mode: "upload", Certificate: string(cert)}, key, 0, "test"); err != nil {
		t.Fatal(err)
	}
	if err := two.Reconcile(); err != nil {
		t.Fatal(err)
	}
	if two.Hosts()[0].DomainTLS[0].CertificateID != "central" {
		t.Fatal("certificate not available on second node")
	}
	if err := one.DeleteCertificate("central", 1, "test"); err != nil {
		t.Fatal(err)
	}
	if err := two.Reconcile(); err != nil {
		t.Fatal(err)
	}
	if two.Hosts()[0].DomainTLS[0].State != "http" {
		t.Fatal("certificate deletion did not converge")
	}
}
