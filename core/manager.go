package gateway

import (
	"apigo.cc/go/cast"
	goCrypto "apigo.cc/go/crypto"
	"apigo.cc/go/discover"
	"apigo.cc/go/encoding"
	"apigo.cc/go/file"
	gohttp "apigo.cc/go/http"
	"apigo.cc/go/log"
	"apigo.cc/go/redis"
	"apigo.cc/go/safe"
	"apigo.cc/go/service"
	"context"
	"crypto/tls"
	"crypto/x509"
	"encoding/pem"
	"errors"
	"fmt"
	"net/http"
	"os"
	"path/filepath"
	"sort"
	"strings"
	"sync"
	"sync/atomic"
	"time"
)

var ErrConflict = errors.New("configuration changed; reload before saving")

const maxStateBytes = 8 << 20
const stateCAS = `local current=redis.call('GET',KEYS[1]) or ''; if current~=ARGV[1] then return 0 end; redis.call('SET',KEYS[1],ARGV[2]); return 1`

type activeHosts struct{ hosts map[string]HostRecord }
type Manager struct {
	adminExternalAddress *gohttp.ExternalAddressResolver
	affinity             *affinityStore
	affinityStop         context.CancelFunc
	affinityDone         chan struct{}
	monitorAttempts      map[string]monitorAttempt
	monitorLatest        map[string]MonitorSample
	registryName         string
	registryCalls        map[string]string
	registrySelf         string
	registrySelfProtocol string
	registryProtocols    map[string]string
	monitorMu            sync.Mutex
	monitorCursor        int
	manualChecks         map[string]MonitorSample
	monitorJobs          map[string]bool
	certificateJobs      map[string]bool

	mu              sync.Mutex
	eventMu         sync.Mutex
	eventCursor     int
	config          Config
	server          *service.WebServer
	rd              *redis.Redis
	state           State
	raw             string
	applied         int64
	status          NodeStatus
	active          atomic.Pointer[activeHosts]
	limiter         *Limiter
	keyMu           sync.RWMutex
	closed          bool
	key             *safe.SafeBuf
	transientEvents []Event
	sharedKey       bool
	automaticKey    bool
	keyUsage        map[string]time.Time
	adminSessionMu  sync.Mutex
	loginLimiter    *Limiter
	acme            atomic.Pointer[certificateManager]
	certificates    atomic.Pointer[certificatePool]
}

func NewManager(config Config, server *service.WebServer) (*Manager, error) {
	if config.HTTPSPort < 0 || config.HTTPSPort > 65535 {
		return nil, fmt.Errorf("invalid HTTPSPort")
	}
	if err := resolveAdminConfig(&config); err != nil {
		return nil, err
	}
	if server == nil {
		server = service.NewWebServer()
	}
	if config.Prefix == "" {
		config.Prefix = "gateway"
	}
	if config.LogRetentionDays == 0 {
		config.LogRetentionDays = 30
	}
	if config.LogRetentionDays < 1 || config.LogRetentionDays > 3650 {
		return nil, fmt.Errorf("LogRetentionDays requires 1–3650")
	}
	if config.DataDir == "" {
		config.DataDir = "data/gateway"
	}
	if config.NodeID == "" {
		hostname, _ := os.Hostname()
		config.NodeID = hostname + "-" + encoding.Hex(safe.MakeSafeToken(6))
	}
	if config.ClientIP.Mode == "" {
		config.ClientIP.Mode = "direct"
	}
	if err := server.SetClientIPPolicy(config.ClientIP); err != nil {
		return nil, err
	}
	// Gateway is the public boundary; internal Service/Admin handlers use the
	// Request-ID relay default, while this boundary always regenerates by default.
	if config.ExternalAddress.Mode == "" && len(config.ExternalAddress.TrustedProxies) == 0 {
		config.ExternalAddress.Mode = "direct"
	}
	adminResolver, err := gohttp.NewExternalAddressResolver(config.AdminExternalAddress)
	if err != nil {
		return nil, err
	}
	if err := server.SetExternalAddressPolicy(config.ExternalAddress); err != nil {
		return nil, err
	}
	if err := file.Mkdir(config.DataDir); err != nil {
		return nil, err
	}
	if err := os.Chmod(config.DataDir, 0700); err != nil {
		return nil, err
	}
	if config.Redis == "" {
		config.Redis = server.Config.Register
	}
	installAffinityBalancer()
	m := &Manager{adminExternalAddress: adminResolver, config: config, server: server, state: State{Format: 1, Hosts: map[string]HostRecord{}}, applied: -1, status: NodeStatus{ID: config.NodeID, Versions: map[string]int64{}}}
	if config.Redis != "" {
		m.rd = redis.GetRedis(config.Redis, log.DefaultLogger)

	}
	if config.MaxAffinityEntries == 0 {
		m.config.MaxAffinityEntries = 1000000
	}
	if m.config.MaxAffinityEntries < 1 {
		return nil, fmt.Errorf("invalid MaxAffinityEntries")
	}
	m.affinity = newAffinityStore(m.config.MaxAffinityEntries)
	if m.rd != nil {
		m.affinity.ready.Store(false)
	}
	m.status.AffinityURL = m.config.AffinityAdvertise
	m.captureRegistryLocked()
	m.registryCalls = map[string]string{}
	for name, call := range discover.Config.Calls {
		m.registryCalls[name] = call
	}
	m.registryName = server.Config.Register
	if m.registryName == "" {
		m.registryName = discover.Config.Registry
	}
	m.limiter = NewLimiter(config.MaxRateKeys, m.rd, config.Prefix)
	m.loginLimiter = NewLimiter(10000, nil, "login")
	keyValue := config.CertificateKey
	if value := os.Getenv("GATEWAY_CERTIFICATE_KEY"); value != "" {
		keyValue = value
	}
	m.config.CertificateKey = ""
	m.sharedKey = keyValue != ""
	m.automaticKey = keyValue == ""
	var rawKey []byte
	if keyValue != "" {
		decoded, err := encoding.UnHex(keyValue)
		if err != nil || len(decoded) != 32 {
			return nil, fmt.Errorf("certificate key must be 64 hex characters")
		}
		rawKey = decoded
	} else {
		var err error
		rawKey, err = m.automaticCertificateKey()
		if err != nil {
			return nil, err
		}
	}
	m.sharedKey = m.rd != nil || keyValue != ""

	if len(rawKey) != 32 {
		return nil, fmt.Errorf("invalid persisted certificate key")
	}
	m.key = safe.NewSafeBufAndErase(rawKey)
	if err := m.loadAffinityCheckpoint(); err != nil {
		m.key.Close()
		return nil, err
	}
	m.mu.Lock()
	err = m.loadLocked()
	if err != nil && m.rd != nil {
		cacheName := filepath.Join(m.config.DataDir, "state.json")
		if cached, cacheErr := file.ReadBytes(cacheName); cacheErr == nil && len(cached) <= maxStateBytes {
			if retained, decodeErr := cast.FromJSON[State](cached); decodeErr == nil && retained.Format == 1 {
				if retained.Hosts == nil {
					retained.Hosts = map[string]HostRecord{}
				}
				m.state, m.raw = retained, string(cached)
				m.status.Error = "Redis unavailable; using retained configuration"
				err = nil
			}
		}
	}
	if err == nil {
		plain := m.key.Open()
		err = validateCertificateStateKey([]byte(m.raw), plain.Data)
		plain.Close()
	}
	if err == nil {
		err = m.applyLocked()
	}
	m.mu.Unlock()
	if err != nil {
		m.key.Close()
		return nil, err
	}
	m.acme.Store(newCertificateManager(m, m.acmeSettingsLocked()))
	server.SetCertificateProvider(func(info *tls.ClientHelloInfo) (*tls.Certificate, error) { return m.acme.Load().getCertificate(info) })
	server.SetRequestGate(func(request *service.Request, response *service.Response) bool {
		if m.acme.Load().handleChallenge(request, response) {
			return true
		}
		service.RelayClientIP(request)
		// The HTTP client supports an explicit Host header; retain the public host
		// when the discovery target is an internal node address.
		address := request.ExternalAddress()
		if err := address.RelayHeaders(request.Request.Header); err != nil {
			response.WriteHeader(http.StatusBadRequest)
			return true
		}
		request.Request.Header.Set("Host", address.Host)
		request.Request.Header.Del("X-User-Id")
		request.Request.Header.Del("X-Discover-From-App")
		request.Request.Header.Del("X-Discover-From-Node")
		return false
	})
	return m, nil
}
func privateWrite(name string, data []byte) error {
	file.EnsureParentDir(name)
	if err := os.Chmod(filepath.Dir(name), 0700); err != nil {
		return err
	}
	temp := name + ".gateway-" + encoding.Hex(safe.MakeSafeToken(8))
	if err := file.WriteBytes(temp, data); err != nil {
		return err
	}
	defer file.Remove(temp)
	if err := os.Chmod(temp, 0600); err != nil {
		return err
	}
	opened, err := file.Open(temp)
	if err != nil {
		return err
	}
	syncErr := opened.Sync()
	closeErr := opened.Close()
	if syncErr != nil {
		return syncErr
	}
	if closeErr != nil {
		return closeErr
	}
	return file.Move(temp, name)
}
func cloneState(state State) State {
	data, _ := cast.ToJSONBytes(state)
	copied, _ := cast.FromJSON[State](data)
	if copied.Hosts == nil {
		copied.Hosts = map[string]HostRecord{}
	}
	return copied
}
func (m *Manager) stateKey() string { return m.config.Prefix + ":state" }
func (m *Manager) loadLocked() error {
	if m.rd != nil {
		result := m.rd.Do("GET", m.stateKey())
		if result.Error != nil {
			return result.Error
		}
		raw := result.String()
		if raw == "" {
			var err error
			raw, err = m.recoverMissingState()
			if err != nil {
				return err
			}
			if raw == "" {
				return nil
			}
		}
		if raw == m.raw {
			return nil
		}
		if len(raw) > maxStateBytes {
			return fmt.Errorf("state exceeds size limit")
		}
		state, err := cast.FromJSON[State](raw)
		if err != nil {
			return err
		}
		if state.Format != 1 {
			return fmt.Errorf("unsupported persisted state format")
		}
		if state.Hosts == nil {
			state.Hosts = map[string]HostRecord{}
		}
		m.state, m.raw = state, raw
		return nil
	}
	name := filepath.Join(m.config.DataDir, "state.json")
	if !file.Exists(name) {
		return nil
	}
	data, err := file.ReadBytes(name)
	if err != nil {
		return err
	}
	if len(data) > maxStateBytes {
		return fmt.Errorf("state exceeds size limit")
	}
	state, err := cast.FromJSON[State](data)
	if err != nil {
		return err
	}
	if state.Format != 1 {
		return fmt.Errorf("unsupported persisted state format")
	}
	if state.Hosts == nil {
		state.Hosts = map[string]HostRecord{}
	}
	m.state = state
	m.raw = string(data)
	return nil
}
func (m *Manager) saveLocked(next State) error {
	next.Revision = m.state.Revision + 1
	data, err := cast.ToJSONBytes(next)
	if err != nil {
		return err
	}
	if len(data) > maxStateBytes {
		return fmt.Errorf("configuration history exceeds %d bytes", maxStateBytes)
	}
	if m.rd != nil {
		result := m.rd.Do("EVAL", stateCAS, 1, m.stateKey(), m.raw, string(data))
		if result.Error != nil {
			return result.Error
		}
		if result.Int() != 1 {
			return ErrConflict
		}
	} else {
		if err := privateWrite(filepath.Join(m.config.DataDir, "state.json"), data); err != nil {
			return err
		}
	}
	m.state, m.raw = next, string(data)
	if m.rd != nil {
		if err := privateWrite(filepath.Join(m.config.DataDir, "state.json"), data); err != nil {
			return fmt.Errorf("saved to Redis but local backup failed: %w", err)
		}
		m.rd.Do("PUBLISH", m.config.Prefix+":manage", cast.String(next.Revision))
	}
	return nil
}
func (m *Manager) applyLocked() error {
	if m.applied == m.state.Revision {
		return nil
	}
	pool, err := m.compileCertificatePoolLocked()
	if err != nil {
		return err
	}
	registry := map[string]*service.PreparedHostPolicy{}
	active := &activeHosts{hosts: map[string]HostRecord{}}
	versions := map[string]int64{}
	for id, record := range m.state.Hosts {
		if record.Current == nil {
			continue
		}
		host := *record.Current
		if err := validateHost(host); err != nil {
			return fmt.Errorf("Host %s: %w", id, err)
		}
		prepared, err := m.prepareHost(host, record.Version)
		if err != nil {
			return fmt.Errorf("Host %s: %w", id, err)
		}
		for _, domain := range host.Domains {
			if registry[domain] != nil {
				return fmt.Errorf("domain %s belongs to multiple Hosts", domain)
			}
			registry[domain] = prepared
		}
		active.hosts[id] = record
		versions[id] = record.Version
	}
	if m.rd != nil && m.raw != "" {
		if err := privateWrite(filepath.Join(m.config.DataDir, "state.json"), []byte(m.raw)); err != nil {
			return err
		}
	}
	if err := m.configureAffinityShards(active); err != nil {
		return err
	}
	if err := m.server.ReplaceHostPolicies(registry); err != nil {
		return err
	}
	m.certificates.Store(pool)
	if previous := m.acme.Load(); previous != nil && previous.settings != m.acmeSettingsLocked() {
		m.acme.Store(newCertificateManager(m, m.acmeSettingsLocked()))
	}
	m.active.Store(active)
	m.applied = m.state.Revision
	m.status.Revision = m.applied
	m.status.Versions = versions
	m.status.Error = ""
	m.status.LastSeen = time.Now()
	return nil
}
func (m *Manager) prepareHost(host Host, version int64) (*service.PreparedHostPolicy, error) {
	candidates := &activeHosts{hosts: map[string]HostRecord{}}
	if active := m.active.Load(); active != nil {
		for id, record := range active.hosts {
			candidates.hosts[id] = record
		}
	}
	candidates.hosts[host.ID] = HostRecord{Current: &host}
	specs, err := affinityShardSpecs(candidates)
	if err != nil {
		return nil, err
	}
	if len(specs) > 0 && m.rd == nil {
		return nil, fmt.Errorf("gradual affinity shards require Redis registry")
	}

	if !host.Enabled {
		return service.PrepareHostPolicy(service.HostPolicy{Gate: func(r *service.Request, w *service.Response) bool { w.WriteHeader(503); return true }})
	}

	policy := service.HostPolicy{Proxies: host.Proxies, Rewrites: host.Rewrites, Statics: host.Statics, ForceHTTPS: host.HTTPS.Force}
	adaptive := host.HTTPS.Mode == "" || host.HTTPS.Mode == "adaptive"
	if adaptive {
		policy.ForceHTTPS = false
	}
	if host.HTTPS.Mode == "auto" {
		if !m.acmeSettingsLocked().AcceptTerms {
			return nil, fmt.Errorf("automatic certificates require ACMEAcceptTerms")
		}
		if m.rd != nil {
			return nil, fmt.Errorf("HTTP ACME is currently single-node only; upload certificates for clustered Hosts")
		}
	}
	if host.HTTPS.Mode == "upload" {
		rawKey, err := m.open(host.HTTPS.SealedKey)
		if err != nil {
			return nil, err
		}
		defer safe.ZeroMemory(rawKey)
		certificate, err := tls.X509KeyPair([]byte(host.HTTPS.Certificate), rawKey)
		if err != nil {
			return nil, err
		}
		leaf, err := x509.ParseCertificate(certificate.Certificate[0])
		if err != nil {
			return nil, err
		}
		if !host.Enabled {
			return service.PrepareHostPolicy(service.HostPolicy{Gate: func(r *service.Request, w *service.Response) bool { w.WriteHeader(503); return true }})
		}

		for _, domain := range host.Domains {
			if domain == "*" || strings.HasPrefix(domain, ":") {
				return nil, fmt.Errorf("certificate requires explicit DNS/IP names")
			}
			if strings.HasPrefix(domain, "*.") {
				found := false
				for _, name := range leaf.DNSNames {
					if strings.EqualFold(name, domain) {
						found = true
					}
				}
				if !found {
					return nil, fmt.Errorf("certificate does not cover %s", domain)
				}
			} else if err := leaf.VerifyHostname(domain); err != nil {
				return nil, err
			}
		}
		policy.Certificate = &certificate
	}
	acl, err := compileACL(host.ACL)
	if err != nil {
		return nil, err
	}
	limits := make([]*compiledLimit, 0, len(host.Limits))
	for _, rule := range host.Limits {
		compiled, err := compileLimit(rule)
		if err != nil {
			return nil, err
		}
		if rule.Enabled && compiled.rule.Scope == "cluster" && m.rd == nil {
			return nil, fmt.Errorf("cluster limit requires Redis")
		}
		limits = append(limits, compiled)
	}
	affinityScopes := compileAffinityScopes(host)
	policy.Gate = func(request *service.Request, response *service.Response) bool {
		if !host.Enabled {
			response.WriteHeader(http.StatusServiceUnavailable)
			response.WriteString("Host disabled")
			return true
		}
		decision := acl.evaluate(request, func(decision ACLDecision) {
			m.runtimeEvent(Event{Time: time.Now(), Type: "acl", HostID: host.ID, RuleID: decision.RuleID, Action: "observe", Version: version, Message: decision.Error})
		})
		if !decision.Allowed {
			m.runtimeEvent(Event{Time: time.Now(), Type: "acl", HostID: host.ID, RuleID: decision.RuleID, Action: "reject", Version: version, Message: decision.Error})
			response.WriteHeader(http.StatusForbidden)
			response.WriteString("access denied")
			return true
		}
		for _, decision := range m.limiter.evaluate(host.ID, limits, request, time.Now()) {
			if decision.Allowed {
				continue
			}
			action := "reject"
			if decision.Observed {
				action = "observe"
			}
			m.runtimeEvent(Event{Time: time.Now(), Type: "limit", HostID: host.ID, RuleID: decision.RuleID, Action: action, Version: version, Message: decision.Error})
			if !decision.Observed {
				writeLimitResponse(response, decision)
				return true
			}
		}
		if adaptive && host.HTTPS.Redirect != "off" && !request.ExternalAddress().IsHTTPS() && m.matchCertificate(normalizedHost(request.Host)) != nil {
			if target, available := m.httpsTarget(request.Host); available {
				http.Redirect(response.Writer, request.Request, "https://"+target+request.URL.RequestURI(), http.StatusPermanentRedirect)
				return true
			}
		}
		return m.prepareAffinityRequest(host, request, response, affinityScopes)
	}
	return service.PrepareHostPolicy(policy)
}
func (m *Manager) runtimeEvent(event Event) {
	m.eventMu.Lock()
	defer m.eventMu.Unlock()
	if event.Time.IsZero() {
		event.Time = time.Now()
	}
	if len(m.transientEvents) < 1000 {
		m.transientEvents = append(m.transientEvents, event)
	} else {
		m.transientEvents[m.eventCursor] = event
		m.eventCursor = (m.eventCursor + 1) % 1000
	}
}
func addAudit(state *State, event Event) {
	event.Time = time.Now()
	state.Events = append(state.Events, event)
	if len(state.Events) > 1000 {
		state.Events = state.Events[len(state.Events)-1000:]
	}
}
func (m *Manager) SaveDraft(host Host, expected int64, actor string) (HostRecord, error) {
	data, _ := cast.ToJSONBytes(host)
	host, _ = cast.FromJSON[Host](data)
	m.mu.Lock()
	defer m.mu.Unlock()
	if err := m.loadLocked(); err != nil {
		return HostRecord{}, err
	}
	if _, deleted := m.state.Trash[host.ID]; deleted {
		return HostRecord{}, fmt.Errorf("Host is in trash; restore it before editing")
	}
	record := m.state.Hosts[host.ID]
	if record.Revision != expected {
		return HostRecord{}, ErrConflict
	}
	if host.HTTPS.SealedKey == "" {
		host.HTTPS.SealedKey = record.Draft.HTTPS.SealedKey
	}
	if err := m.prepareMonitorSecrets(&host, record.Draft); err != nil {
		return HostRecord{}, err
	}
	for index := range host.ACL {
		if host.ACL[index].ID == "" {
			host.ACL[index].ID = "acl-" + encoding.Hex(safe.MakeSafeToken(12))
		}
	}
	for index := range host.Limits {
		if host.Limits[index].ID == "" {
			host.Limits[index].ID = "limit-" + encoding.Hex(safe.MakeSafeToken(12))
		}
	}
	if err := validateHost(host); err != nil {
		return HostRecord{}, err
	}
	next := cloneState(m.state)
	if record.CreatedAt.IsZero() {
		record.CreatedAt = hostAddedAt(record, m.state.Events)
		if record.CreatedAt.IsZero() {
			record.CreatedAt = time.Now()
		}
	}
	record.Revision++
	record.Draft = host
	next.Hosts[host.ID] = record
	addAudit(&next, Event{Type: "draft", HostID: host.ID, Actor: actor, Action: "save"})
	if err := m.saveLocked(next); err != nil {
		return HostRecord{}, err
	}
	return redactRecord(record), nil
}
func (m *Manager) Publish(id string, expected int64, actor string) (HostRecord, error) {
	m.mu.Lock()
	defer m.mu.Unlock()
	if err := m.loadLocked(); err != nil {
		return HostRecord{}, err
	}
	record, exists := m.state.Hosts[id]
	if !exists {
		return HostRecord{}, fmt.Errorf("Host not found")
	}
	if record.Revision != expected {
		return HostRecord{}, ErrConflict
	}
	if err := m.validatePublicationLocked(record.Draft, record.Version+1); err != nil {
		return HostRecord{}, err
	}

	next := cloneState(m.state)
	record.Revision++
	record.Version++
	current := record.Draft
	record.Current = &current
	record.History = append(record.History, HostVersion{Version: record.Version, Host: current, Time: time.Now(), Actor: actor})
	if len(record.History) > 20 {
		record.History = record.History[len(record.History)-20:]
	}
	next.Hosts[id] = record
	addAudit(&next, Event{Type: "publish", HostID: id, Actor: actor, Action: "publish", Version: record.Version})
	if err := m.saveLocked(next); err != nil {
		return HostRecord{}, err
	}
	if err := m.applyLocked(); err != nil {
		m.status.Error = err.Error()
		return redactRecord(record), fmt.Errorf("saved but not applied: %w", err)
	}
	m.ackLocked()
	return redactRecord(record), nil
}
func (m *Manager) Rollback(id string, version, expected int64, actor string) (HostRecord, error) {
	m.mu.Lock()
	if err := m.loadLocked(); err != nil {
		m.mu.Unlock()
		return HostRecord{}, err
	}
	record := m.state.Hosts[id]
	if record.Revision != expected {
		m.mu.Unlock()
		return HostRecord{}, ErrConflict
	}
	var previous *Host
	for _, item := range record.History {
		if item.Version == version {
			host := item.Host
			previous = &host
			break
		}
	}
	m.mu.Unlock()
	if previous == nil {
		return HostRecord{}, fmt.Errorf("version unavailable")
	}
	saved, err := m.SaveDraft(*previous, expected, actor)
	if err != nil {
		return HostRecord{}, err
	}
	return m.Publish(id, saved.Revision, actor)
}
func (m *Manager) Delete(id string, expected int64, actor string) error {
	m.mu.Lock()
	defer m.mu.Unlock()
	if err := m.loadLocked(); err != nil {
		return err
	}
	record, exists := m.state.Hosts[id]
	if !exists {
		return fmt.Errorf("Host not found")
	}
	if record.Revision != expected {
		return ErrConflict
	}
	next := cloneState(m.state)
	if next.Trash == nil {
		next.Trash = map[string]DeletedHost{}
	}
	record.Revision++
	next.Trash[id] = DeletedHost{Record: record, DeletedAt: time.Now(), DeletedBy: actor}
	delete(next.Hosts, id)
	addAudit(&next, Event{Type: "delete", HostID: id, Actor: actor, Action: "trash"})
	if err := m.saveLocked(next); err != nil {
		return err
	}
	if err := m.applyLocked(); err != nil {
		return err
	}
	m.ackLocked()
	return nil
}
func redactHost(host Host) Host {
	host.HTTPS.SealedKey = ""
	host.Monitor.HasHeaders = host.Monitor.SealedHeaders != ""
	host.Monitor.SealedHeaders = ""
	host.Monitor.Headers = nil
	host.Monitor.ClearHeaders = false
	return host
}
func redactRecord(record HostRecord) HostRecord {
	data, _ := cast.ToJSONBytes(record)
	record, _ = cast.FromJSON[HostRecord](data)
	record.Draft = redactHost(record.Draft)
	if record.Current != nil {
		current := redactHost(*record.Current)
		record.Current = &current
	}
	record.History = append([]HostVersion(nil), record.History...)
	for index := range record.History {
		record.History[index].Host = redactHost(record.History[index].Host)
	}
	return record
}
func (m *Manager) Hosts() []HostRecord {
	m.mu.Lock()
	defer m.mu.Unlock()
	records := make([]HostRecord, 0, len(m.state.Hosts))
	for _, record := range m.state.Hosts {
		view := redactRecord(record)
		view.CreatedAt = hostAddedAt(record, m.state.Events)
		view.CertificateInfo = m.certificateInfoLocked(record)
		view.DomainTLS = m.domainTLSLocked(record)
		records = append(records, view)
	}
	sort.Slice(records, func(i, j int) bool {
		if !records[i].CreatedAt.Equal(records[j].CreatedAt) {
			return records[i].CreatedAt.Before(records[j].CreatedAt)
		}
		return records[i].Draft.ID < records[j].Draft.ID
	})
	return records
}
func (m *Manager) Events() []Event {
	m.mu.Lock()
	events := append([]Event(nil), m.state.Events...)
	m.mu.Unlock()
	m.eventMu.Lock()
	events = append(events, m.transientEvents...)
	m.eventMu.Unlock()
	sort.Slice(events, func(i, j int) bool { return events[i].Time.After(events[j].Time) })
	return events
}
func (m *Manager) ackLocked() {
	m.status.LastSeen = time.Now()
	if m.affinity != nil {
		m.status.AffinityReady = m.affinity.ready.Load()
		m.affinity.mu.RLock()
		m.status.AffinityEntries = len(m.affinity.entries)
		m.affinity.mu.RUnlock()
	}
	if m.rd != nil {
		raw, _ := cast.ToJSON(m.status)
		m.rd.Do("SET", m.config.Prefix+":node:"+m.config.NodeID, raw, "EX", 30)
	}
}
func (m *Manager) Reconcile() error {
	m.mu.Lock()
	defer m.mu.Unlock()
	err := m.loadLocked()
	if err == nil {
		err = m.applyLocked()
	}
	if err != nil {
		m.status.Error = err.Error()
	}
	m.ackLocked()
	m.limiter.Sweep(time.Now())
	m.loginLimiter.Sweep(time.Now())
	return err
}
func (m *Manager) Nodes() []NodeStatus {
	m.mu.Lock()
	defer m.mu.Unlock()
	copyStatus := func(status NodeStatus) NodeStatus {
		raw, _ := cast.ToJSONBytes(status)
		copied, _ := cast.FromJSON[NodeStatus](raw)
		return copied
	}
	if m.rd == nil {
		return []NodeStatus{copyStatus(m.status)}
	}
	keys, err := scanKeys(m.rd, m.config.Prefix+":node:*")
	if err != nil {
		status := copyStatus(m.status)
		status.Error = "node inventory unavailable"
		return []NodeStatus{status}
	}
	nodes := []NodeStatus{}
	for _, key := range keys {
		entry := m.rd.Do("GET", key)
		if entry.Error == nil && entry.String() != "" {
			node, err := cast.FromJSON[NodeStatus](entry.String())
			if err == nil {
				nodes = append(nodes, node)
			}
		}
	}
	return nodes
}
func (m *Manager) Close() {
	m.stopAffinitySync()
	m.keyMu.Lock()
	defer m.keyMu.Unlock()
	if !m.closed {
		m.closed = true
		m.key.Close()
	}
}
func (m *Manager) seal(raw []byte) (string, error) {
	m.keyMu.RLock()
	defer m.keyMu.RUnlock()
	if m.closed {
		return "", fmt.Errorf("manager closed")
	}
	nonce := safe.MakeSafeToken(12)
	defer safe.ZeroMemory(nonce)
	key := m.key.Open()
	defer key.Close()
	encrypted, err := goCrypto.AESGCM.Encrypt(raw, key.Data, nonce)
	if err != nil {
		return "", err
	}
	return encoding.Base64(append(append([]byte(nil), nonce...), encrypted...)), nil
}
func (m *Manager) open(value string) ([]byte, error) {
	m.keyMu.RLock()
	defer m.keyMu.RUnlock()
	if m.closed {
		return nil, fmt.Errorf("manager closed")
	}
	encrypted, err := encoding.UnBase64(value)
	if err != nil || len(encrypted) < 28 {
		return nil, fmt.Errorf("invalid encrypted certificate key")
	}
	key := m.key.Open()
	defer key.Close()
	return goCrypto.AESGCM.Decrypt(encrypted[12:], key.Data, encrypted[:12])
}
func (m *Manager) SetCertificate(id string, expected int64, certificate string, key []byte, actor string) (HostRecord, error) {
	defer safe.ZeroMemory(key)
	if len(certificate) > 256<<10 || len(key) > 64<<10 {
		return HostRecord{}, fmt.Errorf("certificate too large")
	}
	if _, err := tls.X509KeyPair([]byte(certificate), key); err != nil {
		return HostRecord{}, err
	}
	m.mu.Lock()
	record, exists := m.state.Hosts[id]
	m.mu.Unlock()
	if !exists {
		return HostRecord{}, fmt.Errorf("Host not found")
	}
	sealed, err := m.seal(key)
	if err != nil {
		return HostRecord{}, err
	}
	host := record.Draft
	host.HTTPS.Mode = "upload"
	host.HTTPS.Certificate = certificate
	host.HTTPS.SealedKey = sealed
	return m.SaveDraft(host, expected, actor)
}
func (m *Manager) Settings() map[string]any {
	settings, _ := m.ACMESettings()
	return map[string]any{"affinity": m.AffinityStatus(), "logRetentionDays": m.config.LogRetentionDays, "nodeID": m.config.NodeID, "clientIP": m.config.ClientIP, "cluster": m.rd != nil, "sharedCertificateKey": m.sharedKey, "adminListen": m.config.AdminListen, "adminDisabled": m.config.AdminDisabled, "acmeEmail": settings.Email, "acmeAcceptTerms": settings.AcceptTerms, "acmeDirectory": settings.Directory, "rateAlgorithm": "first-request fixed window", "listen": m.server.Config.Listen}
}
func (m *Manager) Handler() http.Handler { return m.server.Handler() }

// CheckHost validates an editable draft against the same publication checks, without saving or contacting upstreams.
func (m *Manager) CheckHost(host Host) error {
	m.mu.Lock()
	defer m.mu.Unlock()
	if err := m.loadLocked(); err != nil {
		return err
	}
	record := m.state.Hosts[host.ID]
	if host.HTTPS.SealedKey == "" {
		host.HTTPS.SealedKey = record.Draft.HTTPS.SealedKey
	}
	if host.Monitor.SealedHeaders == "" && host.Monitor.Headers == nil && !host.Monitor.ClearHeaders {
		host.Monitor.SealedHeaders = record.Draft.Monitor.SealedHeaders
	}
	if err := validateHost(host); err != nil {
		return err
	}
	return m.validatePublicationLocked(host, record.Version+1)
}
func (m *Manager) validatePublicationLocked(host Host, version int64) error {
	if host.Monitor.Enabled && host.Monitor.Configured {
		if input, err := m.monitorInput(host); err != nil {
			return fmt.Errorf("监控设置：%w", err)
		} else if host.Monitor.URL == "" && host.Enabled {
			preview, err := m.preview(input, false)
			if err != nil || preview.Type != "proxy" {
				return fmt.Errorf("监控路径未匹配代理，请填写监控地址或调整路径")
			}
		}
	}
	if host.Enabled && host.HTTPS.Mode == "upload" {
		block, _ := pem.Decode([]byte(host.HTTPS.Certificate))
		if block == nil {
			return fmt.Errorf("invalid certificate PEM")
		}
		leaf, err := x509.ParseCertificate(block.Bytes)
		if err != nil {
			return err
		}
		if !time.Now().Before(leaf.NotAfter) || time.Now().Before(leaf.NotBefore) {
			return fmt.Errorf("certificate is not currently valid")
		}
	}
	if _, err := m.prepareHost(host, version); err != nil {
		return err
	}
	for otherID, other := range m.state.Hosts {
		if otherID == host.ID || other.Current == nil {
			continue
		}
		for _, domain := range other.Current.Domains {
			for _, newDomain := range host.Domains {
				if domain == newDomain {
					return fmt.Errorf("域名 %s 已由 Host %s（ID: %s）发布；请编辑该已发布 Host，或先从其配置中移除此域名", domain, other.Current.Name, otherID)
				}
			}
		}
	}
	return nil
}

func hostAddedAt(record HostRecord, events []Event) time.Time {
	stamp := record.CreatedAt
	if !stamp.IsZero() {
		return stamp
	}
	for _, version := range record.History {
		if stamp.IsZero() || version.Time.Before(stamp) {
			stamp = version.Time
		}
	}
	for _, event := range events {
		if event.HostID == record.Draft.ID && (stamp.IsZero() || event.Time.Before(stamp)) {
			stamp = event.Time
		}
	}
	return stamp
}
