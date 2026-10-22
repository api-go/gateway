package gateway

import (
	"apigo.cc/go/cast"
	"apigo.cc/go/encoding"
	"apigo.cc/go/file"
	"crypto/sha256"
	"fmt"
	"path/filepath"
	"sync"
	"sync/atomic"
	"time"
)

type affinityBinding struct {
	Nodes   []string
	Expires int64
}
type affinityEvent struct {
	Key     string
	Binding affinityBinding
	Table   *affinityShardTable
	Load    *affinityShardLoad
}
type affinityCursor struct{ ID, Marker string }
type affinitySnapshot struct {
	Marker  string
	Cursor  string
	Entries []affinityEvent
	Token   string
	Next    int
	Expires int64
}
type affinityStore struct {
	shardMu         sync.Mutex
	shards          map[string]*affinityShardRuntime
	shardRegistry   atomic.Pointer[affinityShardRegistry]
	shardLeader     atomic.Value
	shardSynced     time.Time
	mu              sync.RWMutex
	entries         map[[32]byte]affinityBinding
	pending         map[[32]byte]affinityBinding
	capacity        int
	ready           atomic.Bool
	conflicts       atomic.Uint64
	rejected        atomic.Uint64
	lastError       atomic.Value
	syncMu          sync.Mutex
	snapshotMu      sync.Mutex
	publishedCursor atomic.Pointer[affinityCursor]
	lastReplay      time.Time
	cursor          string
	cursorMarker    string
	lastCheckpoint  time.Time
	lastSweep       time.Time
	snapshots       map[string]affinitySnapshot
	lifecycleMu     sync.Mutex
	stopOnce        sync.Once
}

func newAffinityStore(capacity int) *affinityStore {
	s := &affinityStore{entries: map[[32]byte]affinityBinding{}, pending: map[[32]byte]affinityBinding{}, capacity: capacity, snapshots: map[string]affinitySnapshot{}}
	s.shards = map[string]*affinityShardRuntime{}
	s.shardLeader.Store("")
	s.ready.Store(true)
	s.lastError.Store("")
	s.publishedCursor.Store(&affinityCursor{})
	return s
}
func affinityConfigScope(config AffinityConfig) string {
	// Preserve the pre-removal routing digest. The retired budget fields were
	// always zeroed for gradual scopes; omitting them would orphan live mappings
	// and shard tables when upgrading an existing development cluster.
	legacy := struct {
		Mode, Source, Name                 string
		TTLSeconds                         int
		Missing, Transform, HashStrategy   string
		RebalanceSeconds, RebalancePercent int
		Mappings                           []AffinityMapping
	}{Mode: config.Mode, Source: config.Source, Name: config.Name,
		TTLSeconds: config.TTLSeconds, Missing: config.Missing,
		Transform: config.Transform, HashStrategy: config.HashStrategy, Mappings: config.Mappings}
	raw, _ := cast.ToJSON(legacy)
	return raw
}
func (s *affinityStore) lookup(key [32]byte, now int64) ([]string, bool) {
	s.mu.RLock()
	entry, ok := s.entries[key]
	s.mu.RUnlock()
	return entry.Nodes, ok && entry.Expires > now
}
func (s *affinityStore) insert(key [32]byte, entry affinityBinding, local bool, now int64) ([]string, bool) {
	s.mu.Lock()
	defer s.mu.Unlock()
	if old, ok := s.entries[key]; ok && old.Expires > now {
		if len(old.Nodes) > 0 && len(entry.Nodes) > 0 && old.Nodes[0] != entry.Nodes[0] {
			s.conflicts.Add(1)
		}
		return old.Nodes, true
	}
	if entry.Expires <= now || len(entry.Nodes) == 0 || len(entry.Nodes) > 3 {
		return nil, false
	}
	if _, exists := s.entries[key]; !exists && len(s.entries) >= s.capacity {
		s.rejected.Add(1)
		return nil, false
	}
	entry.Nodes = append([]string(nil), entry.Nodes...)
	s.entries[key] = entry
	if local {
		s.pending[key] = entry
	}
	return entry.Nodes, true
}
func (s *affinityStore) apply(events []affinityEvent) error {
	now := time.Now().Unix()
	for _, event := range events {
		if event.Table != nil {
			if err := s.applyShardTable(event.Table); err != nil {
				return err
			}
			continue
		}
		if event.Load != nil {
			if err := s.applyShardLoad(event.Load); err != nil {
				return err
			}
			continue
		}
		raw, err := encoding.UnHex(event.Key)
		if err != nil || len(raw) != 32 {
			return fmt.Errorf("invalid affinity digest")
		}
		if event.Binding.Expires > now+604800+300 || len(event.Binding.Nodes) > 3 {
			return fmt.Errorf("invalid affinity entry")
		}
		var key [32]byte
		copy(key[:], raw)
		if event.Binding.Expires > now {
			if _, accepted := s.insert(key, event.Binding, false, now); !accepted {
				return fmt.Errorf("affinity capacity exceeded")
			}
		}
	}
	return nil
}
func (s *affinityStore) exportLocked() []affinityEvent {
	s.mu.RLock()
	defer s.mu.RUnlock()
	entries := make([]affinityEvent, 0, len(s.entries))
	now := time.Now().Unix()
	s.shardMu.Lock()
	for _, r := range s.shards {
		if table := r.table.Load(); table != nil {
			entries = append(entries, affinityEvent{Table: table})
		}
	}
	s.shardMu.Unlock()
	for key, entry := range s.entries {
		if entry.Expires > now {
			entries = append(entries, affinityEvent{Key: encoding.Hex(key[:]), Binding: entry})
		}
	}
	return entries
}
func (s *affinityStore) sweep() {
	now := time.Now().Unix()
	s.mu.Lock()
	defer s.mu.Unlock()
	for key, entry := range s.entries {
		if entry.Expires <= now {
			delete(s.entries, key)
			delete(s.pending, key)
		}
	}
}
func (m *Manager) affinityCheckpointPath() string {
	scope := sha256.Sum256([]byte(m.config.Prefix))
	return filepath.Join(m.config.DataDir, "affinity-"+encoding.Hex(scope[:8])+".json")
}
func (m *Manager) loadAffinityCheckpoint() error {
	name := m.affinityCheckpointPath()
	if !file.Exists(name) {
		return nil
	}
	raw, err := file.ReadBytes(name)
	if err != nil {
		return err
	}
	if len(raw) > 512<<20 {
		return fmt.Errorf("affinity checkpoint exceeds size limit")
	}
	checkpoint, err := cast.FromJSON[affinitySnapshot](raw)
	if err != nil {
		return err
	}
	if err = m.affinity.apply(checkpoint.Entries); err != nil {
		return err
	}
	m.affinity.cursor = checkpoint.Cursor
	m.affinity.cursorMarker = checkpoint.Marker
	m.affinity.publishedCursor.Store(&affinityCursor{ID: checkpoint.Cursor, Marker: checkpoint.Marker})
	return nil
}
func (m *Manager) saveAffinityCheckpoint() error {
	entries := m.affinity.exportLocked()
	raw, err := cast.ToJSONBytes(affinitySnapshot{Cursor: m.affinity.cursor, Marker: m.affinity.cursorMarker, Entries: entries})
	if err != nil {
		return err
	}
	if err = privateWrite(m.affinityCheckpointPath(), raw); err == nil {
		m.affinity.lastCheckpoint = time.Now()
	}
	return err
}

// AffinityStatus contains no identifiers or backend credentials.
func (m *Manager) AffinityStatus() map[string]any {
	s := m.affinity
	s.mu.RLock()
	count, pending := len(s.entries), len(s.pending)
	s.mu.RUnlock()
	shardTables, shardReady, shardMoved := 0, true, int64(0)
	shardVersions := map[string]int64{}
	if registry := s.shardRegistry.Load(); registry != nil {
		for scope, r := range registry.tables {
			shardTables++
			if table := r.table.Load(); table != nil {
				shardVersions[scope] = table.Version
				shardMoved += table.Moved
			} else {
				shardReady = false
			}
		}
	}
	return map[string]any{"shardTables": shardTables, "shardReady": shardReady, "shardMoved": shardMoved, "shardVersions": shardVersions, "shardLeader": s.shardLeader.Load(), "error": s.lastError.Load(), "entries": count, "pending": pending, "ready": s.ready.Load(), "conflicts": s.conflicts.Load(), "capacityRejected": s.rejected.Load(), "capacity": s.capacity}
}
