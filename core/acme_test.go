package gateway_test

import (
	"apigo.cc/go/cast"
	"apigo.cc/go/encoding"
	"apigo.cc/go/gateway/core"
	"apigo.cc/go/log"
	"apigo.cc/go/service"
	"apigo.cc/go/timer"
	"context"
	"crypto/ecdsa"
	"crypto/elliptic"
	crand "crypto/rand"
	"crypto/tls"
	"crypto/x509"
	"crypto/x509/pkix"
	"encoding/pem"
	"fmt"
	"io"
	"math/big"
	"net/http"
	"net/http/httptest"
	"sync"
	"sync/atomic"
	"testing"
	"time"
)

// Exercise the official ACME client over real local HTTP-01 and TLS connections.
// The fixture signs CSRs but does not implement CA-side JWS signature verification.
func TestACMEHTTP01IssuanceAndEncryptedCache(t *testing.T) {
	var mutex sync.Mutex
	accepted := false
	finalized := false
	var chain []byte
	var nonce atomic.Int64
	issuer, err := ecdsa.GenerateKey(elliptic.P256(), crand.Reader)
	if err != nil {
		t.Fatal(err)
	}
	ca := &x509.Certificate{SerialNumber: big.NewInt(1), Subject: pkix.Name{CommonName: "Gateway test CA"}, NotBefore: time.Now().Add(-time.Hour), NotAfter: time.Now().Add(365 * 24 * time.Hour), IsCA: true, BasicConstraintsValid: true, KeyUsage: x509.KeyUsageCertSign}
	caDER, err := x509.CreateCertificate(crand.Reader, ca, ca, &issuer.PublicKey, issuer)
	if err != nil {
		t.Fatal(err)
	}
	server := service.NewWebServer()
	server.Config.NoLogGets = true
	server.Config.Listen = "127.0.0.1:0,http|127.0.0.1:0,https"
	server.Config.AutoCertDir = t.TempDir()
	var base string
	respond := func(writer http.ResponseWriter, status int, value any) {
		data, _ := cast.ToJSONBytes(value)
		writer.Header().Set("Content-Type", "application/json")
		writer.WriteHeader(status)
		writer.Write(data)
	}
	order := func() map[string]any {
		status := "pending"
		if accepted {
			status = "ready"
		}
		if finalized {
			status = "valid"
		}
		return map[string]any{"status": status, "identifiers": []map[string]string{{"type": "dns", "value": "acme.example.test"}}, "authorizations": []string{base + "/authorization"}, "finalize": base + "/finalize", "certificate": base + "/certificate"}
	}
	authority := httptest.NewServer(http.HandlerFunc(func(writer http.ResponseWriter, request *http.Request) {
		writer.Header().Set("Replay-Nonce", encoding.URLBase64([]byte(fmt.Sprintf("gateway-nonce-%d", nonce.Add(1)))))
		switch request.URL.Path {
		case "/directory":
			respond(writer, 200, map[string]string{"newNonce": base + "/nonce", "newAccount": base + "/account", "newOrder": base + "/new-order", "revokeCert": base + "/revoke", "keyChange": base + "/key-change"})
		case "/nonce":
			writer.WriteHeader(204)
		case "/account":
			writer.Header().Set("Location", base+"/account/1")
			respond(writer, 201, map[string]string{"status": "valid", "orders": base + "/orders"})
		case "/new-order":
			mutex.Lock()
			value := order()
			mutex.Unlock()
			writer.Header().Set("Location", base+"/order")
			respond(writer, 201, value)
		case "/authorization":
			mutex.Lock()
			status := "pending"
			if accepted {
				status = "valid"
			}
			mutex.Unlock()
			respond(writer, 200, map[string]any{"status": status, "identifier": map[string]string{"type": "dns", "value": "acme.example.test"}, "challenges": []map[string]string{{"type": "http-01", "url": base + "/challenge", "token": "gateway-test-challenge", "status": "pending"}}})
		case "/challenge":
			probe, _ := http.NewRequest("GET", "http://"+server.Addrs[0]+"/.well-known/acme-challenge/gateway-test-challenge", nil)
			probe.Host = "acme.example.test"
			response, err := http.DefaultClient.Do(probe)
			if err != nil {
				t.Error(err)
				http.Error(writer, "challenge unavailable", 500)
				return
			}
			proof, _ := io.ReadAll(response.Body)
			response.Body.Close()
			if response.StatusCode != 200 || len(proof) < 40 {
				t.Errorf("HTTP challenge response %d %q", response.StatusCode, proof)
				http.Error(writer, "challenge failed", 500)
				return
			}
			mutex.Lock()
			accepted = true
			mutex.Unlock()
			respond(writer, 200, map[string]string{"status": "valid"})
		case "/order":
			mutex.Lock()
			value := order()
			mutex.Unlock()
			respond(writer, 200, value)
		case "/finalize":
			raw, _ := io.ReadAll(request.Body)
			jws, err := cast.FromJSON[struct{ Payload string }](raw)
			if err != nil {
				t.Error(err)
				http.Error(writer, "invalid JWS", 400)
				return
			}
			payload, err := encoding.UnURLBase64(jws.Payload)
			if err != nil {
				t.Error(err)
				return
			}
			body, err := cast.FromJSON[struct{ CSR string }](payload)
			if err != nil {
				t.Error(err)
				return
			}
			csrDER, err := encoding.UnURLBase64(body.CSR)
			if err != nil {
				t.Error(err)
				return
			}
			csr, err := x509.ParseCertificateRequest(csrDER)
			if err != nil {
				t.Error(err)
				return
			}
			leaf := &x509.Certificate{SerialNumber: big.NewInt(2), DNSNames: csr.DNSNames, Subject: csr.Subject, NotBefore: time.Now().Add(-time.Hour), NotAfter: time.Now().Add(90 * 24 * time.Hour), KeyUsage: x509.KeyUsageDigitalSignature, ExtKeyUsage: []x509.ExtKeyUsage{x509.ExtKeyUsageServerAuth}}
			der, err := x509.CreateCertificate(crand.Reader, leaf, ca, csr.PublicKey, issuer)
			if err != nil {
				t.Error(err)
				return
			}
			mutex.Lock()
			chain = append(pem.EncodeToMemory(&pem.Block{Type: "CERTIFICATE", Bytes: der}), pem.EncodeToMemory(&pem.Block{Type: "CERTIFICATE", Bytes: caDER})...)
			finalized = true
			value := order()
			mutex.Unlock()
			respond(writer, 200, value)
		case "/certificate":
			mutex.Lock()
			data := append([]byte(nil), chain...)
			mutex.Unlock()
			writer.Header().Set("Content-Type", "application/pem-certificate-chain")
			writer.Write(data)
		default:
			http.NotFound(writer, request)
		}
	}))
	defer authority.Close()
	base = authority.URL
	m, err := gateway.NewManager(gateway.Config{DataDir: t.TempDir(), ACMEAcceptTerms: true, ACMEDirectory: base + "/directory", ACMEEmail: "admin@example.test"}, server)
	if err != nil {
		t.Fatal(err)
	}
	defer m.Close()
	host := baseHost()
	host.Domains = []string{"acme.example.test"}
	host.HTTPS = gateway.HTTPSConfig{Mode: "adaptive"}
	settings, revision := m.ACMESettings()
	if err := m.SetACMESettings(settings, revision); err != nil {
		t.Fatal(err)
	}
	if _, err := m.SaveCertificate(gateway.CertificateRecord{ID: "acme-library", Name: "Local ACME", Mode: "auto", Domains: host.Domains}, nil, 0, "test"); err != nil {
		t.Fatal(err)
	}
	savePublish(t, m, host, 0)
	if err := server.Start(context.Background(), log.DefaultLogger); err != nil {
		t.Fatal(err)
	}
	defer server.Stop(context.Background())
	if err := m.IssueCertificate("acme-library"); err != nil {
		t.Fatal(err)
	}
	roots := x509.NewCertPool()
	roots.AppendCertsFromPEM(pem.EncodeToMemory(&pem.Block{Type: "CERTIFICATE", Bytes: caDER}))
	connection, err := tls.Dial("tcp", server.Addrs[1], &tls.Config{RootCAs: roots, ServerName: "acme.example.test"})
	if err != nil {
		t.Fatal(err)
	}
	connection.Close()
	mutex.Lock()
	done := accepted && finalized
	mutex.Unlock()
	if !done {
		t.Fatal("issuance did not exercise HTTP-01 and CSR finalization")
	}
	var cache []byte
	if err := timer.Retry(func() error {
		var err error
		cache, err = m.CertificateCache().Get(context.Background(), "acme.example.test")
		return err
	}, timer.WithMaxRetries(50), timer.WithBackoff(10*time.Millisecond, 1)); err != nil || len(cache) == 0 {
		t.Fatalf("issued certificate not cached: %v", err)
	}
	if err := timer.Retry(func() error {
		if m.Hosts()[0].CertificateInfo.State != "valid" {
			return fmt.Errorf("pool not yet refreshed")
		}
		return nil
	}, timer.WithMaxRetries(50), timer.WithBackoff(10*time.Millisecond, 1)); err != nil {
		t.Fatal(err)
	}
	status := m.Hosts()[0].CertificateInfo
	if status.State != "valid" || status.Expires == "" {
		t.Fatalf("certificate status %+v", status)
	}
}
