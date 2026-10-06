// Browser acceptance fixture. Never included in the Gateway executable.
package main

import (
	"apigo.cc/go/cast"
	"apigo.cc/go/file"
	"apigo.cc/go/gateway/core"
	"apigo.cc/go/safe"
	"apigo.cc/go/service"
	"crypto/ecdsa"
	"crypto/elliptic"
	"crypto/rand"
	"crypto/x509"
	"crypto/x509/pkix"
	"encoding/pem"
	"fmt"
	"io"
	"math/big"
	"net/http"
	"net/http/httptest"
	"os"
	"os/signal"
	"path/filepath"
	"syscall"
	"time"
)

func main() {
	directory, err := os.MkdirTemp("", "go-gateway-browser-")
	if err != nil {
		panic(err)
	}
	defer file.Remove(directory)
	config := gateway.Config{DataDir: directory, AdminListen: "127.0.0.1:0", ACMEAcceptTerms: true, HTTPSPort: 10443}
	server := service.NewWebServer()
	server.Config.NoLogGets = true
	manager, err := gateway.NewManager(config, server)
	if err != nil {
		panic(err)
	}
	defer manager.Close()
	upstream := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		body, _ := io.ReadAll(r.Body)
		fmt.Fprintf(w, "%s %s %s", r.Method, r.URL.RequestURI(), body)
	}))
	defer upstream.Close()
	statics := filepath.Join(directory, "public")
	if err := file.Write(filepath.Join(statics, "hello.txt"), "browser-static"); err != nil {
		panic(err)
	}
	business := httptest.NewServer(manager.Handler())
	defer business.Close()
	key, err := ecdsa.GenerateKey(elliptic.P256(), rand.Reader)
	if err != nil {
		panic(err)
	}
	template := &x509.Certificate{SerialNumber: big.NewInt(101), Subject: pkix.Name{CommonName: "browser.test"}, DNSNames: []string{"browser.test"}, NotBefore: time.Now().Add(-time.Hour), NotAfter: time.Now().Add(24 * time.Hour), KeyUsage: x509.KeyUsageDigitalSignature, ExtKeyUsage: []x509.ExtKeyUsage{x509.ExtKeyUsageServerAuth}}
	der, err := x509.CreateCertificate(rand.Reader, template, template, &key.PublicKey, key)
	if err != nil {
		panic(err)
	}
	keyDER, err := x509.MarshalPKCS8PrivateKey(key)
	if err != nil {
		panic(err)
	}
	fixtures := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.URL.Path == "/password" && r.Method == "POST" {
			secret := safe.NewSafeBufAndErase([]byte("browser-admin-password"))
			defer secret.Close()
			if err := gateway.SetAdminPassword(config, secret); err != nil {
				http.Error(w, err.Error(), 500)
			}
			return
		}
		if r.URL.Path == "/reset-password" && r.Method == "POST" {
			secret := safe.NewSafeBufAndErase([]byte("browser-new-password"))
			defer secret.Close()
			if err := gateway.SetAdminPassword(config, secret); err != nil {
				http.Error(w, err.Error(), 500)
			}
			return
		}
		raw, _ := cast.ToJSONBytes(map[string]any{"business": business.URL, "upstream": upstream.URL, "staticDir": statics, "certificate": string(pem.EncodeToMemory(&pem.Block{Type: "CERTIFICATE", Bytes: der})), "key": string(pem.EncodeToMemory(&pem.Block{Type: "PRIVATE KEY", Bytes: keyDER}))})
		w.Header().Set("Content-Type", "application/json")
		w.Write(raw)
	}))
	defer fixtures.Close()
	admin := httptest.NewServer(manager.AdminHandler())
	defer admin.Close()
	raw, _ := cast.ToJSONBytes(map[string]string{"admin": admin.URL, "fixtures": fixtures.URL})
	fmt.Println(string(raw))
	stop := make(chan os.Signal, 1)
	signal.Notify(stop, syscall.SIGINT, syscall.SIGTERM)
	defer signal.Stop(stop)
	<-stop
}
