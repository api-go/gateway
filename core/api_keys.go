package gateway

import (
	goCrypto "apigo.cc/go/crypto"
	"apigo.cc/go/encoding"
	"apigo.cc/go/safe"
	"crypto/subtle"
	"errors"
	"fmt"
	"net/http"
	"slices"
	"sort"
	"strings"
	"time"
)

// APIKey contains public authorization metadata; Hash is internal persisted data and never returned by HTTP.
type APIKey struct {
	ID          string
	Name        string
	Permissions []string
	HostIDs     []string // Explicit IDs; ["*"] grants all Hosts, including new IDs.
	Expires     time.Time
	LastUsed    time.Time // Last successful authentication on this node; not persisted.
	Created     time.Time
	Hash        string
}
type apiKeyInput struct {
	Name        string
	Permissions []string
	HostIDs     []string
	Expires     time.Time
	ID          string
	Revision    int64
}

func (m *Manager) handleAPIKeys(w http.ResponseWriter, r *http.Request, actor string) {
	m.mu.Lock()
	defer m.mu.Unlock()
	if err := m.loadLocked(); err != nil {
		http.Error(w, "credential store unavailable", 503)
		return
	}
	if r.Method == "GET" && r.URL.Path == "/api/keys" {
		entries := []APIKey{}
		for _, key := range m.state.APIKeys {
			key.LastUsed = m.keyUsage[key.ID]
			key.Hash = ""
			entries = append(entries, key)
		}
		sort.Slice(entries, func(i, j int) bool { return entries[i].Created.Before(entries[j].Created) })
		writeJSON(w, 200, map[string]any{"keys": entries, "revision": m.state.Revision})
		return
	}
	if r.Method != "POST" {
		http.Error(w, "method not allowed", 405)
		return
	}
	var input apiKeyInput
	if err := readJSON(w, r, &input); err != nil {
		http.Error(w, err.Error(), 400)
		return
	}
	if input.Revision != m.state.Revision {
		http.Error(w, ErrConflict.Error(), 409)
		return
	}
	next := cloneState(m.state)
	if next.APIKeys == nil {
		next.APIKeys = map[string]APIKey{}
	}
	var token string
	var entry APIKey
	switch r.URL.Path {
	case "/api/keys/create":
		if len(next.APIKeys) >= 100 || strings.TrimSpace(input.Name) == "" || len(input.Name) > 100 || len(input.Permissions) == 0 || len(input.Permissions) > 4 || len(input.HostIDs) == 0 || len(input.HostIDs) > 100 || (apiKeyHasExpiry(input.Expires) && !input.Expires.After(time.Now())) {
			http.Error(w, "invalid API key settings", 400)
			return
		}
		for _, permission := range input.Permissions {
			if !slices.Contains([]string{"read", "draft", "publish", "delete"}, permission) {
				http.Error(w, "invalid permission", 400)
				return
			}
		}
		for _, id := range input.HostIDs {
			if id == "" || len(id) > 80 || (id != "*" && strings.ContainsAny(id, "/:* \t\r\n")) {
				http.Error(w, "invalid Host ID", 400)
				return
			}
		}
		id := encoding.Hex(safe.MakeSafeToken(12))
		secret := safe.MakeSafeToken(32)
		defer safe.ZeroMemory(secret)
		token = "gw_" + id + "_" + encoding.Hex(secret)
		raw := []byte(token)
		defer safe.ZeroMemory(raw)
		entry = APIKey{ID: id, Name: strings.TrimSpace(input.Name), Permissions: input.Permissions, HostIDs: input.HostIDs, Expires: input.Expires, Created: time.Now(), Hash: goCrypto.SHA256ToHex(raw)}
		next.APIKeys[id] = entry
	case "/api/keys/revoke":
		var exists bool
		entry, exists = next.APIKeys[input.ID]
		if !exists {
			http.NotFound(w, r)
			return
		}
		delete(next.APIKeys, input.ID)
	default:
		http.NotFound(w, r)
		return
	}
	addAudit(&next, Event{Type: "api-key", Action: strings.TrimPrefix(r.URL.Path, "/api/keys/"), Actor: actor, Message: entry.ID + " " + entry.Name})
	if err := m.saveLocked(next); err != nil {
		status := 503
		if errors.Is(err, ErrConflict) {
			status = 409
		}
		http.Error(w, err.Error(), status)
		return
	}
	if r.URL.Path == "/api/keys/revoke" {
		delete(m.keyUsage, input.ID)
	}
	entry.Hash = ""
	writeJSON(w, 200, map[string]any{"key": entry, "token": token, "revision": m.state.Revision})
}
func keyAllows(key APIKey, permission, id string) bool {
	return slices.Contains(key.Permissions, permission) && (slices.Contains(key.HostIDs, "*") || slices.Contains(key.HostIDs, id))
}
func (m *Manager) machineKey(r *http.Request) (APIKey, error) {
	value := r.Header.Get("Authorization")
	if !strings.HasPrefix(value, "Bearer gw_") || len(value) > 200 {
		return APIKey{}, fmt.Errorf("invalid API key")
	}
	token := strings.TrimPrefix(value, "Bearer ")
	parts := strings.Split(token, "_")
	if len(parts) != 3 {
		return APIKey{}, fmt.Errorf("invalid API key")
	}
	m.mu.Lock()
	defer m.mu.Unlock()
	// Always check authoritative state: a retained cache must never resurrect a revoked key.
	if err := m.loadLocked(); err != nil {
		return APIKey{}, err
	}
	key, exists := m.state.APIKeys[parts[1]]
	raw := []byte(token)
	defer safe.ZeroMemory(raw)
	hash := goCrypto.SHA256ToHex(raw)
	if !exists || subtle.ConstantTimeCompare([]byte(hash), []byte(key.Hash)) != 1 || (apiKeyHasExpiry(key.Expires) && !time.Now().Before(key.Expires)) {
		return APIKey{}, fmt.Errorf("invalid API key")
	}
	if m.keyUsage == nil {
		m.keyUsage = map[string]time.Time{}
	}
	for id := range m.keyUsage {
		if _, exists := m.state.APIKeys[id]; !exists {
			delete(m.keyUsage, id)
		}
	}
	m.keyUsage[key.ID] = time.Now()
	return key, nil
}
func (m *Manager) handleMachineAPI(w http.ResponseWriter, r *http.Request) {
	key, err := m.machineKey(r)
	if err != nil {
		http.Error(w, "API key unavailable or invalid", 401)
		return
	}
	action := strings.TrimPrefix(r.URL.Path, "/api/v1/")
	actor := "api-key:" + key.ID
	if r.Method == "GET" && action == "hosts" {
		if !slices.Contains(key.Permissions, "read") {
			http.Error(w, "permission denied", 403)
			return
		}
		if err := m.Reconcile(); err != nil {
			http.Error(w, "configuration store unavailable", 503)
			return
		}
		records := []HostRecord{}
		for _, record := range m.Hosts() {
			if keyAllows(key, "read", record.Draft.ID) {
				records = append(records, record)
			}
		}
		writeJSON(w, 200, records)
		return
	}
	if r.Method == "GET" && action == "nodes" {
		if !slices.Contains(key.Permissions, "read") {
			http.Error(w, "permission denied", 403)
			return
		}
		nodes := m.Nodes()
		for i := range nodes {
			versions := map[string]int64{}
			for id, v := range nodes[i].Versions {
				if keyAllows(key, "read", id) {
					versions[id] = v
				}
			}
			nodes[i].Versions = versions
		}
		writeJSON(w, 200, nodes)
		return
	}
	permission := map[string]string{"draft": "draft", "preview": "draft", "publish": "publish", "rollback": "publish", "delete": "delete"}[action]
	if permission == "" {
		http.NotFound(w, r)
		return
	}
	if r.Method != "POST" {
		http.Error(w, "method not allowed", 405)
		return
	}
	var input editInput
	var preview PreviewInput
	if action == "preview" {
		err = readJSON(w, r, &preview)
		input.Host = preview.Host
	} else {
		err = readJSON(w, r, &input)
	}
	if err != nil {
		http.Error(w, err.Error(), 400)
		return
	}
	if !keyAllows(key, permission, input.Host.ID) {
		http.Error(w, "permission denied", 403)
		return
	}
	var result any
	switch action {
	case "draft":
		m.mu.Lock()
		previous := m.state.Hosts[input.Host.ID].Draft
		m.mu.Unlock()
		if input.Host.Monitor.CheckScript != previous.Monitor.CheckScript || !slices.Equal(input.Host.Monitor.ChannelIDs, previous.Monitor.ChannelIDs) {
			http.Error(w, "only administrator sessions may edit monitor scripts and alarm bindings", 403)
			return
		}
		if previous.Monitor.SealedHeaders != "" && monitorRouteFingerprint(input.Host) != monitorRouteFingerprint(previous) && input.Host.Monitor.Headers == nil && !input.Host.Monitor.ClearHeaders {
			http.Error(w, "changing routing requires explicitly clearing or replacing saved monitor credentials", 403)
			return
		}

		result, err = m.SaveDraft(input.Host, input.Revision, actor)
	case "preview":
		result, err = m.Preview(preview)
	case "publish":
		result, err = m.Publish(input.Host.ID, input.Revision, actor)
	case "rollback":
		result, err = m.Rollback(input.Host.ID, input.Version, input.Revision, actor)
	case "delete":
		err = m.Delete(input.Host.ID, input.Revision, actor)
		result = map[string]bool{"ok": err == nil}
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

// Legacy cast snapshots store zero times without an offset. Parsing in a local
// timezone gives that sentinel a historical offset, so time.Time.IsZero alone
// incorrectly expires permanent keys. Both a zero instant and the legacy exact
// zero wall-clock mean unset; genuine nonzero expiry times remain effective.
func apiKeyHasExpiry(t time.Time) bool {
	return !t.IsZero() && !(t.Year() == 1 && t.Month() == time.January && t.Day() == 1 && t.Hour() == 0 && t.Minute() == 0 && t.Second() == 0 && t.Nanosecond() == 0)
}
