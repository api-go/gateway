package gateway

import (
	"github.com/api-go/safe"
	"crypto/tls"
	"errors"
	"fmt"
	"net/http"
)

type certificateInput struct {
	Certificate CertificateRecord
	Key         string
	Revision    int64
	Settings    ACMESettings
}

func (m *Manager) handleCertificateLibrary(w http.ResponseWriter, r *http.Request, actor string) {
	if r.Method == "GET" && (r.URL.Path == "/api/certificates" || r.URL.Path == "/api/acme") {
		if err := m.Reconcile(); err != nil {
			http.Error(w, "configuration store unavailable", 503)
			return
		}
		if r.URL.Path == "/api/acme" {
			settings, revision := m.ACMESettings()
			writeJSON(w, 200, map[string]any{"settings": settings, "revision": revision})
		} else {
			writeJSON(w, 200, m.Certificates())
		}
		return
	}
	if r.Method != "POST" {
		http.Error(w, "method not allowed", 405)
		return
	}
	var input certificateInput
	if err := readJSON(w, r, &input); err != nil {
		http.Error(w, err.Error(), 400)
		return
	}
	var result any = map[string]bool{"ok": true}
	var err error
	switch r.URL.Path {
	case "/api/certificates/save":
		key := []byte(input.Key)
		defer safe.ZeroMemory(key)
		result, err = m.SaveCertificate(input.Certificate, key, input.Revision, actor)
	case "/api/certificates/delete":
		err = m.DeleteCertificate(input.Certificate.ID, input.Revision, actor)
	case "/api/acme":
		err = m.SetACMESettings(input.Settings, input.Revision)
	case "/api/certificates/issue":
		err = m.IssueCertificate(input.Certificate.ID)
	default:
		http.NotFound(w, r)
		return
	}
	if err != nil {
		status := 400
		if errors.Is(err, ErrConflict) {
			status = 409
		}
		writeJSON(w, status, map[string]string{"error": err.Error()})
		return
	}
	writeJSON(w, 200, result)
}

// IssueCertificate starts a bounded background HTTP-01 job. The ACME manager owns renewal.
func (m *Manager) IssueCertificate(id string) error {
	m.mu.Lock()
	if err := m.loadLocked(); err != nil {
		m.mu.Unlock()
		return err
	}
	entry, exists := m.state.Certificates[id]
	if !exists || entry.Mode != "auto" {
		m.mu.Unlock()
		return fmt.Errorf("automatic certificate not found")
	}
	if !m.acmeSettingsLocked().AcceptTerms {
		m.mu.Unlock()
		return fmt.Errorf("ACME terms not accepted")
	}
	if m.certificateJobs == nil {
		m.certificateJobs = map[string]bool{}
	}
	if m.certificateJobs[id] {
		m.mu.Unlock()
		return nil
	}
	if len(m.certificateJobs) >= 4 {
		m.mu.Unlock()
		return fmt.Errorf("certificate jobs busy")
	}
	m.certificateJobs[id] = true
	domains := append([]string(nil), entry.Domains...)
	manager := m.acme.Load()
	m.mu.Unlock()
	go func() {
		defer func() { m.mu.Lock(); delete(m.certificateJobs, id); m.mu.Unlock() }()
		for _, domain := range domains {
			_, err := manager.manager.GetCertificate(&tls.ClientHelloInfo{ServerName: domain, SignatureSchemes: []tls.SignatureScheme{tls.ECDSAWithP256AndSHA256, tls.PSSWithSHA256, tls.PKCS1WithSHA256}, SupportedVersions: []uint16{tls.VersionTLS13, tls.VersionTLS12}, CipherSuites: []uint16{tls.TLS_ECDHE_ECDSA_WITH_AES_128_GCM_SHA256, tls.TLS_ECDHE_RSA_WITH_AES_128_GCM_SHA256}})
			action := "issued"
			message := domain
			if err != nil {
				action = "error"
				message += ": " + err.Error()
			}
			m.runtimeEvent(Event{Type: "certificate", Action: action, Message: message})
		}
	}()
	return nil
}
