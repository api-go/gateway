package gateway

import (
	"apigo.cc/go/cast"
	"apigo.cc/go/encoding"
	"apigo.cc/go/timer"
	"context"
	"crypto/sha256"
	"fmt"
	redisDriver "github.com/gomodule/redigo/redis"
	"net"
	"strings"
	"time"
)

const affinityJournalTTL = 300
const affinityAppend = `local id=redis.call('XADD',KEYS[1],'MAXLEN',512,'*','data',ARGV[1]); redis.call('EXPIRE',KEYS[1],300); local t=redis.call('TIME'); local cutoff=tonumber(t[1])*1000+math.floor(tonumber(t[2])/1000)-300000; redis.call('XTRIM',KEYS[1],'MINID',tostring(cutoff)..'-0'); redis.call('PUBLISH',KEYS[2],ARGV[1]); return id`

func (m *Manager) affinityJournal() string { return m.config.Prefix + ":affinity-journal" }
func (m *Manager) affinityChannel() string { return m.config.Prefix + ":affinity-events" }
func (m *Manager) flushAffinity(connection redisDriver.Conn) error {
	for range 16 {
		pending := map[[32]byte]affinityBinding{}
		events := []affinityEvent{}
		m.affinity.mu.RLock()
		for key, entry := range m.affinity.pending {
			pending[key] = entry
			events = append(events, affinityEvent{Key: encoding.Hex(key[:]), Binding: entry})
			if len(events) == 256 {
				break
			}
		}
		m.affinity.mu.RUnlock()
		if len(events) == 0 {
			return nil
		}
		data, err := cast.ToJSONBytes(events)
		if err != nil {
			return err
		}
		if _, err = connection.Do("EVAL", affinityAppend, 2, m.affinityJournal(), m.affinityChannel(), data); err != nil {
			return err
		}
		m.affinity.mu.Lock()
		for key, entry := range pending {
			if current, ok := m.affinity.pending[key]; ok && current.Expires == entry.Expires {
				delete(m.affinity.pending, key)
			}
		}
		m.affinity.mu.Unlock()
	}
	return nil
}
func affinityCursorMarker(data string) string {
	sum := sha256.Sum256([]byte(data))
	return encoding.Hex(sum[:])
}

// Stream IDs alone are not a generation identifier after DEL / recreation.
func affinityValidCursor(connection redisDriver.Conn, journal, id, marker string) (bool, error) {
	if marker == "" {
		return false, nil
	}
	rows, err := affinityStreamRows(connection, "XRANGE", journal, id, id, "COUNT", 1)
	if err != nil {
		return false, err
	}
	if len(rows) == 0 {
		return false, nil
	}
	parts, err := redisDriver.Values(rows[0], nil)
	if err != nil || len(parts) != 2 {
		return false, fmt.Errorf("invalid affinity cursor")
	}
	fields, err := redisDriver.StringMap(parts[1], nil)
	if err != nil {
		return false, err
	}
	return affinityCursorMarker(fields["data"]) == marker, nil
}
func streamIDAfter(a, b string) bool {
	am, as, _ := strings.Cut(a, "-")
	bm, bs, _ := strings.Cut(b, "-")
	return cast.Int64(am) > cast.Int64(bm) || cast.Int64(am) == cast.Int64(bm) && cast.Int64(as) > cast.Int64(bs)
}
func affinityStreamRows(connection redisDriver.Conn, command string, args ...any) ([]any, error) {
	return redisDriver.Values(connection.Do(command, args...))
}
func affinityRowID(row any) (string, error) {
	parts, err := redisDriver.Values(row, nil)
	if err != nil || len(parts) != 2 {
		return "", fmt.Errorf("invalid affinity journal row")
	}
	return redisDriver.String(parts[0], nil)
}

// SyncAffinity flushes local discoveries and replays the bounded shared journal.
// It is a control-plane operation; proxy requests never call Redis.
func (m *Manager) SyncAffinity(ctx context.Context) (syncErr error) {
	s := m.affinity
	s.syncMu.Lock()
	defer s.syncMu.Unlock()
	defer func() {
		message := ""
		if syncErr != nil {
			message = syncErr.Error()
		}
		s.lastError.Store(message)
	}()
	if err := ctx.Err(); err != nil {
		return err
	}
	if time.Since(s.lastSweep) > 30*time.Second {
		s.sweep()
		s.lastSweep = time.Now()
	}
	if m.rd == nil {
		s.ready.Store(true)
		if time.Since(s.lastCheckpoint) > 30*time.Second {
			return m.saveAffinityCheckpoint()
		}
		return nil
	}
	connection, err := m.rd.GetConnection()
	if err != nil {
		return err
	}
	defer connection.Close()
	first, err := affinityStreamRows(connection, "XRANGE", m.affinityJournal(), "-", "+", "COUNT", 1)
	if err != nil {
		return err
	}
	gap := !s.ready.Load()
	if len(first) > 0 && s.cursor != "" {
		id, err := affinityRowID(first[0])
		if err != nil {
			return err
		}
		gap = gap || streamIDAfter(id, s.cursor)
		if !gap {
			valid, err := affinityValidCursor(connection, m.affinityJournal(), s.cursor, s.cursorMarker)
			if err != nil {
				return err
			}
			gap = !valid
		}
	}
	if len(first) == 0 && s.cursor != "" {
		if s.ready.Load() && time.Since(s.lastReplay) < affinityJournalTTL*time.Second {
			s.cursor = ""
			s.cursorMarker = ""
		} else {
			gap = true
		}
	}
	if gap {
		// Node inventory is reused from the existing Gateway/Discover management plane.
		peers := m.Nodes()
		joined := false
		others := 0
		haveReady := false
		winner := m.config.NodeID
		s.mu.RLock()
		most := len(s.entries)
		s.mu.RUnlock()
		for _, peer := range peers {
			if peer.ID == m.config.NodeID {
				continue
			}
			if peer.AffinityReady {
				haveReady = true
			}
			if peer.AffinityEntries > most || peer.AffinityEntries == most && peer.ID < winner {
				winner = peer.ID
				most = peer.AffinityEntries
			}
		}
		for _, peer := range peers {
			if peer.ID == m.config.NodeID {
				continue
			}
			others++
			if !peer.AffinityReady || peer.AffinityURL == "" {
				continue
			}
			if err = m.joinAffinityPeer(ctx, peer.AffinityURL); err == nil {
				joined = true
				break
			}
		}
		if !joined && others > 0 && (haveReady || winner != m.config.NodeID) {
			s.ready.Store(false)
			return fmt.Errorf("affinity snapshot unavailable; waiting for an existing ready peer")
		}
		// A lone node can recover its retained checkpoint; there is no other live authority.
		if !joined || len(first) == 0 {
			s.cursor = ""
			s.cursorMarker = ""
		}
		if joined && s.cursor != "" {
			valid, err := affinityValidCursor(connection, m.affinityJournal(), s.cursor, s.cursorMarker)
			if err != nil {
				return err
			}
			if !valid {
				s.cursor = ""
				s.cursorMarker = ""
			}
		}
	}
	if err = m.flushAffinity(connection); err != nil {
		return err
	}
	start := "-"
	if s.cursor != "" {
		start = "(" + s.cursor
	}
	caughtUp := false
	for range 32 {
		if err := ctx.Err(); err != nil {
			return err
		}
		rows, err := affinityStreamRows(connection, "XRANGE", m.affinityJournal(), start, "+", "COUNT", 128)
		if err != nil {
			return err
		}
		for _, row := range rows {
			parts, _ := redisDriver.Values(row, nil)
			if len(parts) != 2 {
				return fmt.Errorf("invalid affinity journal")
			}
			id, err := redisDriver.String(parts[0], nil)
			if err != nil {
				return err
			}
			fields, err := redisDriver.StringMap(parts[1], nil)
			if err != nil {
				return err
			}
			events, err := cast.FromJSON[[]affinityEvent](fields["data"])
			if err != nil {
				return err
			}
			if err = s.apply(events); err != nil {
				s.ready.Store(false)
				return err
			}
			s.cursor = id
			s.cursorMarker = affinityCursorMarker(fields["data"])
			s.publishedCursor.Store(&affinityCursor{ID: id, Marker: s.cursorMarker})
			start = "(" + id
		}
		if len(rows) < 128 {
			caughtUp = true
			break
		}
	}
	if !caughtUp {
		s.ready.Store(false)
		return fmt.Errorf("affinity replay catching up")
	}
	s.ready.Store(true)
	if err := m.syncAffinityShards(ctx, connection); err != nil {
		return err
	}
	s.lastReplay = time.Now()
	s.publishedCursor.Store(&affinityCursor{ID: s.cursor, Marker: s.cursorMarker})
	if time.Since(s.lastCheckpoint) > 30*time.Second {
		return m.saveAffinityCheckpoint()
	}
	return nil
}
func (m *Manager) startAffinitySync(address net.Addr) error {
	s := m.affinity
	s.lifecycleMu.Lock()
	defer s.lifecycleMu.Unlock()
	if m.affinityStop != nil {
		return nil
	}
	if m.rd != nil {
		s.ready.Store(false)
	}
	m.mu.Lock()
	m.status.AffinityURL = m.affinityAdvertise(address)
	m.status.AffinityReady = false
	m.ackLocked()
	m.mu.Unlock()
	ctx, cancel := context.WithCancel(context.Background())
	m.affinityStop = cancel
	m.affinityDone = make(chan struct{})
	// Subscriptions apply received entries without making Redis calls under its callback lock.
	if m.rd != nil {
		m.rd.Subscribe(m.affinityChannel(), nil, func(data []byte) {
			events, err := cast.FromJSON[[]affinityEvent](data)
			if err == nil {
				if err = s.apply(events); err != nil {
					s.ready.Store(false)
				}
			}
		})
	}
	if err := m.SyncAffinity(ctx); err != nil {
		m.runtimeEvent(Event{Type: "affinity", Action: "sync-pending", Message: err.Error()})
	}
	m.mu.Lock()
	m.ackLocked()
	m.mu.Unlock()
	wake := make(chan struct{}, 1)
	pulse := timer.NewDebouncer(100*time.Millisecond, func() {
		select {
		case <-ctx.Done():
			return
		case wake <- struct{}{}:
		default:
		}
	})
	pulse.Trigger()
	go func() {
		defer close(m.affinityDone)
		lastPoll := time.Now()
		for {
			select {
			case <-ctx.Done():
				return
			case <-wake:
				s.mu.RLock()
				pending := len(s.pending)
				s.mu.RUnlock()
				if pending == 0 && time.Since(lastPoll) < time.Second {
					pulse.Trigger()
					continue
				}
				lastPoll = time.Now()
				if err := m.SyncAffinity(ctx); err != nil && ctx.Err() == nil {
					m.runtimeEvent(Event{Type: "affinity", Action: "sync-error", Message: err.Error()})
				}
				pulse.Trigger()
			}
		}
	}()
	return nil
}
func (m *Manager) stopAffinitySync() {
	if m.affinity == nil {
		return
	}
	m.affinity.stopOnce.Do(func() {
		s := m.affinity
		s.lifecycleMu.Lock()
		cancel, done := m.affinityStop, m.affinityDone
		s.lifecycleMu.Unlock()
		if cancel != nil {
			cancel()
			<-done
		}
		if m.rd != nil && cancel != nil {
			m.rd.Unsubscribe(m.affinityChannel())
		}
		s.syncMu.Lock()
		_ = m.saveAffinityCheckpoint()
		s.syncMu.Unlock()
	})
}
