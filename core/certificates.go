package gateway

import (
	"github.com/api-go/crypto"
	"github.com/api-go/file"
	"github.com/api-go/safe"
	"github.com/api-go/service"
	"context"
	"crypto/tls"
	"crypto/x509"
	"encoding/pem"
	"fmt"
	"golang.org/x/crypto/acme"
	"golang.org/x/crypto/acme/autocert"
	"net/http"
	"path/filepath"
	"strings"
	"time"
)

type certificateManager struct {
	settings    ACMESettings
	owner       *Manager
	manager     *autocert.Manager
	httpHandler http.Handler
}
type encryptedCertificateCache struct{ owner *Manager }

func (c encryptedCertificateCache) path(key string) string {
	return filepath.Join(c.owner.config.DataDir, "acme", crypto.SHA256ToHex([]byte(key)))
}
func (c encryptedCertificateCache) Get(ctx context.Context, key string) ([]byte, error) {
	if err := ctx.Err(); err != nil {
		return nil, err
	}
	path := c.path(key)
	if !file.Exists(path) {
		return nil, autocert.ErrCacheMiss
	}
	sealed, err := file.Read(path)
	if err != nil {
		return nil, err
	}
	return c.owner.open(sealed)
}
func (c encryptedCertificateCache) Put(ctx context.Context, key string, data []byte) error {
	if err := ctx.Err(); err != nil {
		return err
	}
	sealed, err := c.owner.seal(data)
	if err != nil {
		return err
	}
	if err := privateWrite(c.path(key), []byte(sealed)); err != nil {
		return err
	}
	c.owner.refreshCertificatePool()
	return nil
}
func (c encryptedCertificateCache) Delete(ctx context.Context, key string) error {
	if err := ctx.Err(); err != nil {
		return err
	}
	path := c.path(key)
	if !file.Exists(path) {
		return nil
	}
	return file.Remove(path)
}
func newCertificateManager(owner *Manager, settings ACMESettings) *certificateManager {
	manager := &autocert.Manager{Cache: encryptedCertificateCache{owner: owner}, Email: settings.Email, HostPolicy: func(ctx context.Context, host string) error {
		if owner.autoDomain(host) {
			return nil
		}
		return fmt.Errorf("domain is not enabled for automatic certificates")
	}, Prompt: func(terms string) bool { return settings.AcceptTerms }}
	if settings.Directory != "" {
		manager.Client = &acme.Client{DirectoryURL: settings.Directory}
	}
	return &certificateManager{settings: settings, owner: owner, manager: manager, httpHandler: manager.HTTPHandler(http.NotFoundHandler())}
}
func (m *Manager) autoDomain(domain string) bool {
	domain = normalizedHost(domain)
	if pool := m.certificates.Load(); pool != nil && pool.automatic[domain] {
		return true
	}
	active := m.active.Load()
	if active == nil {
		return false
	}
	domain = normalizedHost(domain)
	for _, record := range active.hosts {
		host := record.Current
		if host == nil || !host.Enabled || host.HTTPS.Mode != "auto" {
			continue
		}
		for _, name := range host.Domains {
			if name == domain {
				return true
			}
		}
	}
	return false
}
func (c *certificateManager) getCertificate(info *tls.ClientHelloInfo) (*tls.Certificate, error) {
	if mode, known, enabled := c.owner.managedHTTPSMode(info.ServerName); known && (!enabled || mode == "off") {
		return nil, fmt.Errorf("HTTPS is disabled for domain")
	}
	if c.owner.adaptiveDomain(info.ServerName) {
		if selected := c.owner.matchCertificate(info.ServerName); selected != nil && !selected.automatic {
			return selected.certificate, nil
		}
	}

	if !c.owner.autoDomain(info.ServerName) {
		if !c.owner.adaptiveDomain(info.ServerName) {
			return nil, nil
		}
		if selected := c.owner.matchCertificate(info.ServerName); selected != nil {
			return selected.certificate, nil
		}
		return nil, fmt.Errorf("no valid certificate for domain")
	}
	certificate, err := c.manager.GetCertificate(info)
	if err != nil {
		c.owner.runtimeEvent(Event{Type: "certificate", Action: "error", Message: info.ServerName + ": " + err.Error()})
	}
	return certificate, err
}
func (c *certificateManager) handleChallenge(request *service.Request, response *service.Response) bool {
	if request.Method != http.MethodGet || !strings.HasPrefix(request.URL.Path, "/.well-known/acme-challenge/") || !c.owner.autoDomain(normalizedHost(request.Host)) {
		return false
	}
	c.httpHandler.ServeHTTP(response.Writer, request.Request)
	return true
}

// CertificateCache exposes the encrypted ACME cache for backup inspection/testing without PEM disclosure on disk.
func (m *Manager) CertificateCache() autocert.Cache { return encryptedCertificateCache{owner: m} }

func (m *Manager) certificateInfoLocked(record HostRecord) CertificateInfo {
	host := record.Current
	if host == nil {
		host = &record.Draft
	}
	if host.HTTPS.Mode == "adaptive" || host.HTTPS.Mode == "" {
		result := CertificateInfo{State: "http"}
		for _, domain := range host.Domains {
			if entry := m.matchCertificate(domain); entry != nil {
				result.State = "valid"
				result.Domains = append(result.Domains, domain)
				result.Expires = entry.leaf.NotAfter.UTC().Format(time.RFC3339)
			}
		}
		return result
	}
	info := CertificateInfo{State: "off"}
	var data []byte
	if host.HTTPS.Mode == "upload" {
		data = []byte(host.HTTPS.Certificate)
		info.State = "unavailable"
	}
	if host.HTTPS.Mode == "auto" {
		info.State = "pending first HTTPS handshake"
		if len(host.Domains) > 0 {
			var err error
			data, err = (encryptedCertificateCache{owner: m}).Get(context.Background(), host.Domains[0])
			if err != nil {
				data, err = (encryptedCertificateCache{owner: m}).Get(context.Background(), host.Domains[0]+"+rsa")
			}
			if err != nil {
				return info
			}
			defer safe.ZeroMemory(data)
		}
	}
	for len(data) > 0 {
		block, rest := pem.Decode(data)
		data = rest
		if block == nil {
			break
		}
		if block.Type != "CERTIFICATE" {
			continue
		}
		leaf, err := x509.ParseCertificate(block.Bytes)
		if err != nil {
			break
		}
		info.State = "valid"
		if !time.Now().Before(leaf.NotAfter) {
			info.State = "expired"
		}
		info.Issuer = leaf.Issuer.String()
		info.Domains = append([]string(nil), leaf.DNSNames...)
		info.Expires = leaf.NotAfter.UTC().Format(time.RFC3339)
		return info
	}
	return info
}
