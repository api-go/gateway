package gateway

import (
	"fmt"
	"sort"
	"time"
)

// Trash returns redacted, isolated records, newest deletion first.
func (m *Manager) Trash() []DeletedHost {
	m.mu.Lock()
	defer m.mu.Unlock()
	items := make([]DeletedHost, 0, len(m.state.Trash))
	for _, item := range m.state.Trash {
		item.Record = redactRecord(item.Record)
		items = append(items, item)
	}
	sort.Slice(items, func(i, j int) bool {
		if !items[i].DeletedAt.Equal(items[j].DeletedAt) {
			return items[i].DeletedAt.After(items[j].DeletedAt)
		}
		return items[i].Record.Draft.ID < items[j].Record.Draft.ID
	})
	return items
}

// Restore reactivates the previously published configuration, retaining its
// separate draft and history. Conflicts leave the item in trash unchanged.
func (m *Manager) Restore(id string, expected int64, actor string) (HostRecord, error) {
	m.mu.Lock()
	defer m.mu.Unlock()
	if err := m.loadLocked(); err != nil {
		return HostRecord{}, err
	}
	item, exists := m.state.Trash[id]
	if !exists {
		return HostRecord{}, fmt.Errorf("Host not found in trash")
	}
	if item.Record.Revision != expected {
		return HostRecord{}, ErrConflict
	}
	if _, exists := m.state.Hosts[id]; exists {
		return HostRecord{}, fmt.Errorf("Host ID already exists")
	}
	record := item.Record
	if record.Current != nil {
		if err := validateHost(*record.Current); err != nil {
			return HostRecord{}, err
		}
		if err := m.validatePublicationLocked(*record.Current, record.Version+1); err != nil {
			return HostRecord{}, err
		}
	}
	next := cloneState(m.state)
	// Use the clone so mutation cannot change the retained item before CAS succeeds.
	record = next.Trash[id].Record
	record.Revision++
	if record.Current != nil {
		record.Version++
		record.History = append(record.History, HostVersion{Version: record.Version, Host: *record.Current, Time: time.Now(), Actor: actor})
		if len(record.History) > 20 {
			record.History = record.History[len(record.History)-20:]
		}
	}
	next.Hosts[id] = record
	delete(next.Trash, id)
	addAudit(&next, Event{Type: "restore", HostID: id, Actor: actor, Action: "restore", Version: record.Version})
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

// Purge removes a trashed Host from managed configuration. It cannot delete an
// active Host; audit history and independently retained backups are unaffected.
func (m *Manager) Purge(id string, expected int64, actor string) error {
	m.mu.Lock()
	defer m.mu.Unlock()
	if err := m.loadLocked(); err != nil {
		return err
	}
	item, exists := m.state.Trash[id]
	if !exists {
		return fmt.Errorf("Host not found in trash")
	}
	if item.Record.Revision != expected {
		return ErrConflict
	}
	next := cloneState(m.state)
	delete(next.Trash, id)
	addAudit(&next, Event{Type: "purge", HostID: id, Actor: actor, Action: "purge"})
	if err := m.saveLocked(next); err != nil {
		return err
	}
	if err := m.applyLocked(); err != nil {
		return err
	}
	m.ackLocked()
	return nil
}
