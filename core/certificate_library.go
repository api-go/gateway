package gateway

import (
	"apigo.cc/go/cast"
	"apigo.cc/go/safe"
	"context"
	"crypto/tls"
	"crypto/x509"
	"encoding/pem"
	"fmt"
	"net"
	"net/url"
	"sort"
	"strings"
	"time"
)

type ACMESettings struct {
	Email       string
	Directory   string
	AcceptTerms bool
}
type CertificateRecord struct {
	ID          string
	Name        string
	Mode        string
	Revision    int64
	Domains     []string
	Certificate string
	SealedKey   string
	Info        CertificateInfo
}
type DomainTLSStatus struct {
	Domain          string
	State           string
	CertificateID   string
	CertificateName string
	Expires         string
}
type pooledCertificate struct {
	automatic bool

	id          string
	name        string
	certificate *tls.Certificate
	leaf        *x509.Certificate
}
type certificatePool struct {
	exact     map[string][]*pooledCertificate
	wildcard  map[string][]*pooledCertificate
	automatic map[string]bool
}

func (m *Manager) acmeSettingsLocked() ACMESettings {
	if m.state.ACME != nil {
		return *m.state.ACME
	}
	return ACMESettings{Email: m.config.ACMEEmail, Directory: m.config.ACMEDirectory, AcceptTerms: m.config.ACMEAcceptTerms}
}
func parseLibraryCertificate(public string, key []byte) (*tls.Certificate, *x509.Certificate, error) {
	publicBlock, _ := pem.Decode([]byte(public))
	if publicBlock == nil || publicBlock.Type != "CERTIFICATE" {
		return nil, nil, fmt.Errorf("证书文件需要 PEM CERTIFICATE 内容，请勿上传私钥")
	}
	keyBlock, remaining := pem.Decode(key)
	if keyBlock != nil && keyBlock.Type == "EC PARAMETERS" {
		keyBlock, _ = pem.Decode(remaining)
	}
	if keyBlock == nil || (keyBlock.Type != "PRIVATE KEY" && keyBlock.Type != "RSA PRIVATE KEY" && keyBlock.Type != "EC PRIVATE KEY") {
		return nil, nil, fmt.Errorf("私钥文件需要 PEM PRIVATE KEY 内容（通常为 .key），请勿上传证书；加密私钥需先解密")
	}
	return parseLibraryCertificateBytes([]byte(public), key)
}
func parseLibraryCertificateBytes(public, key []byte) (*tls.Certificate, *x509.Certificate, error) {
	pair, err := tls.X509KeyPair(public, key)
	if err != nil {
		return nil, nil, err
	}
	leaf, err := x509.ParseCertificate(pair.Certificate[0])
	if err != nil {
		return nil, nil, err
	}
	pair.Leaf = leaf
	return &pair, leaf, nil
}
func (p *certificatePool) add(id, name string, pair *tls.Certificate, leaf *x509.Certificate) {
	entry := &pooledCertificate{id: id, name: name, certificate: pair, leaf: leaf}
	names := append([]string(nil), leaf.DNSNames...)
	for _, ip := range leaf.IPAddresses {
		names = append(names, ip.String())
	}
	for _, domain := range names {
		domain = strings.ToLower(domain)
		if strings.HasPrefix(domain, "*.") {
			p.wildcard[domain] = append(p.wildcard[domain], entry)
		} else {
			p.exact[domain] = append(p.exact[domain], entry)
		}
	}
}
func (m *Manager) compileCertificatePoolLocked() (*certificatePool, error) {
	p := &certificatePool{exact: map[string][]*pooledCertificate{}, wildcard: map[string][]*pooledCertificate{}, automatic: map[string]bool{}}
	for id, entry := range m.state.Certificates {
		if entry.Mode == "auto" {
			for _, domain := range entry.Domains {
				if m.acmeSettingsLocked().AcceptTerms {
					p.automatic[domain] = true
				}
				for _, key := range []string{domain, domain + "+rsa"} {
					data, err := (encryptedCertificateCache{owner: m}).Get(context.Background(), key)
					if err != nil {
						continue
					}
					pair, leaf, err := parseLibraryCertificateBytes(data, data)
					safe.ZeroMemory(data)
					if err == nil {
						p.add(id, strings.Join(entry.Domains, ", "), pair, leaf)
						for _, cert := range p.exact[domain] {
							if cert.id == id {
								cert.automatic = true
							}
						}
						break
					}
				}
			}
			continue
		}
		key, err := m.open(entry.SealedKey)
		if err != nil {
			return nil, fmt.Errorf("certificate %s: %w", id, err)
		}
		pair, leaf, err := parseLibraryCertificate(entry.Certificate, key)
		safe.ZeroMemory(key)
		if err != nil {
			return nil, fmt.Errorf("certificate %s: %w", id, err)
		}
		p.add(id, strings.Join(entry.Domains, ", "), pair, leaf)
	}
	for _, entries := range []map[string][]*pooledCertificate{p.exact, p.wildcard} {
		for _, list := range entries {
			sort.Slice(list, func(i, j int) bool {
				if list[i].leaf.NotAfter.Equal(list[j].leaf.NotAfter) {
					return list[i].id < list[j].id
				}
				return list[i].leaf.NotAfter.After(list[j].leaf.NotAfter)
			})
		}
	}
	return p, nil
}
func (m *Manager) refreshCertificatePool() {
	m.mu.Lock()
	defer m.mu.Unlock()
	if pool, err := m.compileCertificatePoolLocked(); err == nil {
		m.certificates.Store(pool)
	}
}
func (m *Manager) matchCertificate(domain string) *pooledCertificate {
	p := m.certificates.Load()
	if p == nil {
		return nil
	}
	domain = normalizedHost(domain)
	now := time.Now()
	selectEntry := func(list []*pooledCertificate) *pooledCertificate {
		for _, entry := range list {
			if now.Before(entry.leaf.NotAfter) && !now.Before(entry.leaf.NotBefore) {
				return entry
			}
		}
		return nil
	}
	if entry := selectEntry(p.exact[domain]); entry != nil {
		return entry
	}
	wild := domain
	if !strings.HasPrefix(domain, "*.") {
		if index := strings.IndexByte(domain, '.'); index >= 0 {
			wild = "*" + domain[index:]
		} else {
			return nil
		}
	}
	return selectEntry(p.wildcard[wild])
}
func (m *Manager) managedHTTPSMode(domain string) (string, bool, bool) {
	active := m.active.Load()
	if active == nil {
		return "", false, false
	}
	domain = normalizedHost(domain)
	best := -1
	mode := ""
	enabled := false
	for _, record := range active.hosts {
		host := record.Current
		if host == nil {
			continue
		}
		for _, name := range host.Domains {
			score := -1
			if name == domain {
				score = 10000
			} else if name == "*" {
				score = 0
			} else if strings.HasPrefix(name, "*.") && strings.HasSuffix(domain, name[1:]) {
				score = len(name)
			}
			if score > best {
				best = score
				mode = host.HTTPS.Mode
				enabled = host.Enabled
			}
		}
	}
	return mode, best >= 0, enabled
}
func (m *Manager) adaptiveDomain(domain string) bool {
	mode, known, enabled := m.managedHTTPSMode(domain)
	return known && enabled && (mode == "" || mode == "adaptive")
}
func (m *Manager) httpsTarget(host string) (string, bool) {
	port := m.config.HTTPSPort
	if port == 0 {
		for _, listener := range strings.Split(m.server.Config.Listen, "|") {
			parts := strings.Split(listener, ",")
			if len(parts) > 1 && parts[1] == "https" {
				_, number, err := net.SplitHostPort(parts[0])
				if err == nil {
					port = cast.Int(number)
					break
				}
			}
		}
	}
	if port <= 0 || port > 65535 {
		return "", false
	}
	domain := normalizedHost(host)
	if port == 443 {
		if strings.Contains(domain, ":") {
			domain = "[" + domain + "]"
		}
		return domain, true
	}
	return net.JoinHostPort(domain, cast.String(port)), true
}
func (m *Manager) domainTLSLocked(record HostRecord) []DomainTLSStatus {
	host := record.Current
	if host == nil {
		host = &record.Draft
	}
	result := []DomainTLSStatus{}
	for _, domain := range host.Domains {
		item := DomainTLSStatus{Domain: domain, State: "http"}
		if host.HTTPS.Mode == "adaptive" || host.HTTPS.Mode == "" {
			if entry := m.matchCertificate(domain); entry != nil {
				item.State = "https"
				item.CertificateID = entry.id
				item.CertificateName = entry.name
				item.Expires = entry.leaf.NotAfter.UTC().Format(time.RFC3339)
			}
		}
		result = append(result, item)
	}
	return result
}
func libraryInfo(leaf *x509.Certificate) CertificateInfo {
	state := "valid"
	if !time.Now().Before(leaf.NotAfter) {
		state = "expired"
	} else if time.Now().Before(leaf.NotBefore) {
		state = "not yet valid"
	}
	domains := append([]string(nil), leaf.DNSNames...)
	for _, ip := range leaf.IPAddresses {
		domains = append(domains, ip.String())
	}
	return CertificateInfo{State: state, Issuer: leaf.Issuer.String(), Domains: domains, Expires: leaf.NotAfter.UTC().Format(time.RFC3339)}
}
func (m *Manager) Certificates() []CertificateRecord {
	m.mu.Lock()
	defer m.mu.Unlock()
	result := []CertificateRecord{}
	for _, entry := range m.state.Certificates {
		entry.Domains = append([]string(nil), entry.Domains...)
		entry.Name = strings.Join(entry.Domains, ", ")
		entry.SealedKey = ""
		entry.Certificate = ""
		entry.Info = CertificateInfo{State: "pending", Domains: entry.Domains}
		pool := m.certificates.Load()
		if pool != nil {
			for _, entries := range []map[string][]*pooledCertificate{pool.exact, pool.wildcard} {
				for _, list := range entries {
					for _, cert := range list {
						if cert.id == entry.ID {
							entry.Info = libraryInfo(cert.leaf)
						}
					}
				}
			}
		}
		result = append(result, entry)
	}
	sort.Slice(result, func(i, j int) bool { return result[i].Name < result[j].Name })
	return result
}
func (m *Manager) SaveCertificate(entry CertificateRecord, key []byte, expected int64, actor string) (CertificateRecord, error) {
	defer safe.ZeroMemory(key)
	m.mu.Lock()
	defer m.mu.Unlock()
	if err := m.loadLocked(); err != nil {
		return CertificateRecord{}, err
	}
	old, exists := m.state.Certificates[entry.ID]
	if exists && old.Revision != expected || !exists && expected != 0 {
		return CertificateRecord{}, ErrConflict
	}
	if entry.ID == "" || len(entry.ID) > 80 || strings.ContainsAny(entry.ID, "/:* \t\r\n") {
		return CertificateRecord{}, fmt.Errorf("invalid certificate ID")
	}
	if entry.Mode == "upload" {
		if entry.Certificate == "" && exists && old.Mode == "upload" {
			entry.Certificate = old.Certificate
		}
		if len(entry.Certificate) > 256<<10 || len(key) > 64<<10 {
			return CertificateRecord{}, fmt.Errorf("certificate too large")
		}
		if len(key) == 0 && exists {
			var err error
			key, err = m.open(old.SealedKey)
			if err != nil {
				return CertificateRecord{}, err
			}
			defer safe.ZeroMemory(key)
		}
		_, leaf, err := parseLibraryCertificate(entry.Certificate, key)
		if err != nil {
			return CertificateRecord{}, err
		}
		entry.Info = libraryInfo(leaf)
		entry.Domains = entry.Info.Domains
		if len(entry.Domains) == 0 {
			return CertificateRecord{}, fmt.Errorf("certificate requires subject alternative names")
		}
		entry.SealedKey, err = m.seal(key)
		if err != nil {
			return CertificateRecord{}, err
		}
	} else if entry.Mode == "auto" {
		settings := m.acmeSettingsLocked()
		if m.rd != nil {
			return CertificateRecord{}, fmt.Errorf("HTTP-01 automatic certificates currently require single-node deployment")
		}
		if !settings.AcceptTerms {
			return CertificateRecord{}, fmt.Errorf("accept ACME terms in certificate settings first")
		}
		if len(entry.Domains) == 0 || len(entry.Domains) > 50 {
			return CertificateRecord{}, fmt.Errorf("automatic certificate requires 1–50 domains")
		}
		for _, domain := range entry.Domains {
			if !validDomain(domain) || !strings.Contains(domain, ".") || strings.Contains(domain, "*") || net.ParseIP(domain) != nil || domain != strings.ToLower(strings.TrimSpace(domain)) {
				return CertificateRecord{}, fmt.Errorf("HTTP-01 requires exact DNS domains")
			}
		}
		entry.Certificate = ""
		entry.SealedKey = ""
	} else {
		return CertificateRecord{}, fmt.Errorf("invalid certificate mode")
	}
	entry.Name = strings.Join(entry.Domains, ", ")
	entry.Revision = old.Revision + 1
	next := cloneState(m.state)
	if next.Certificates == nil {
		next.Certificates = map[string]CertificateRecord{}
	}
	next.Certificates[entry.ID] = entry
	next.Events = append(next.Events, Event{Time: time.Now(), Type: "certificate", Action: "save", Actor: actor, Message: entry.Name})
	if len(next.Events) > 1000 {
		next.Events = next.Events[len(next.Events)-1000:]
	}
	if err := m.saveLocked(next); err != nil {
		return CertificateRecord{}, err
	}
	if err := m.applyLocked(); err != nil {
		return CertificateRecord{}, err
	}
	m.ackLocked()
	entry.SealedKey = ""
	entry.Certificate = ""
	return entry, nil
}
func (m *Manager) DeleteCertificate(id string, expected int64, actor string) error {
	m.mu.Lock()
	defer m.mu.Unlock()
	if err := m.loadLocked(); err != nil {
		return err
	}
	entry, exists := m.state.Certificates[id]
	if !exists || entry.Revision != expected {
		return ErrConflict
	}
	next := cloneState(m.state)
	delete(next.Certificates, id)
	next.Events = append(next.Events, Event{Time: time.Now(), Type: "certificate", Action: "delete", Actor: actor, Message: entry.Name})
	if len(next.Events) > 1000 {
		next.Events = next.Events[len(next.Events)-1000:]
	}
	if err := m.saveLocked(next); err != nil {
		return err
	}
	if err := m.applyLocked(); err != nil {
		return err
	}
	m.ackLocked()
	return nil
}
func (m *Manager) ACMESettings() (ACMESettings, int64) {
	m.mu.Lock()
	defer m.mu.Unlock()
	return m.acmeSettingsLocked(), m.state.Revision
}
func (m *Manager) SetACMESettings(settings ACMESettings, expected int64) error {
	m.mu.Lock()
	defer m.mu.Unlock()
	if err := m.loadLocked(); err != nil {
		return err
	}
	if m.state.Revision != expected {
		return ErrConflict
	}
	if settings.Directory != "" {
		u, err := url.Parse(settings.Directory)
		if err != nil || u.Host == "" || (u.Scheme != "https" && u.Scheme != "http") {
			return fmt.Errorf("invalid ACME directory URL")
		}
	}
	if !settings.AcceptTerms {
		for _, record := range m.state.Hosts {
			if record.Current != nil && record.Current.HTTPS.Mode == "auto" {
				return fmt.Errorf("disable legacy automatic Hosts before revoking ACME acceptance")
			}
		}
	}
	next := cloneState(m.state)
	next.ACME = &settings
	next.Events = append(next.Events, Event{Time: time.Now(), Type: "certificate", Action: "settings", Actor: "admin"})
	if len(next.Events) > 1000 {
		next.Events = next.Events[len(next.Events)-1000:]
	}
	if err := m.saveLocked(next); err != nil {
		return err
	}
	if err := m.applyLocked(); err != nil {
		return err
	}
	m.ackLocked()
	return nil
}
