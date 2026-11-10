package gateway

import (
	"github.com/api-go/cast"
	goCrypto "github.com/api-go/crypto"
	"github.com/api-go/file"
	"github.com/api-go/safe"
	"fmt"
	"path/filepath"
	"strings"
	"time"
)

// Store encrypted sessions separately from Host state. Tokens themselves are never persisted.
func (m *Manager) sessionName(token string) (string, error) {
	if len(token) != 64 || strings.Trim(token, "0123456789abcdef") != "" {
		return "", fmt.Errorf("invalid session token")
	}
	return goCrypto.SHA256ToHex([]byte(token)), nil
}
func (m *Manager) storeAdminSession(token string, session adminSession) error {
	name, err := m.sessionName(token)
	if err != nil {
		return err
	}
	raw, err := cast.ToJSONBytes(session)
	if err != nil {
		return err
	}
	defer safe.ZeroMemory(raw)
	sealed, err := m.seal(raw)
	if err != nil {
		return err
	}
	if m.rd != nil {
		return m.rd.Do("SET", m.config.Prefix+":admin-session:"+name, sealed, "EX", 28800).Error
	}
	m.adminSessionMu.Lock()
	defer m.adminSessionMu.Unlock()
	dir := filepath.Join(m.config.DataDir, "admin-sessions")
	if err := file.Mkdir(dir); err != nil {
		return err
	}
	entries, err := file.ReadDir(dir)
	if err != nil {
		return err
	}
	count := 0
	for _, entry := range entries {
		if entry.IsDir {
			continue
		}
		if len(entry.Name) != 64 || strings.Trim(entry.Name, "0123456789abcdef") != "" {
			continue
		}
		if time.Since(time.Unix(entry.ModTime, 0)) >= 8*time.Hour {
			if err := file.Remove(entry.FullName); err != nil {
				return err
			}
		} else {
			count++
		}
	}
	if count >= 1000 {
		return fmt.Errorf("session capacity exceeded")
	}
	return privateWrite(filepath.Join(dir, name), []byte(sealed))
}
func (m *Manager) readAdminSession(token string) (adminSession, error) {
	name, err := m.sessionName(token)
	if err != nil {
		return adminSession{}, err
	}
	var sealed string
	if m.rd != nil {
		value := m.rd.Do("GET", m.config.Prefix+":admin-session:"+name)
		if value.Error != nil {
			return adminSession{}, value.Error
		}
		sealed = value.String()
	} else {
		raw, err := file.ReadBytes(filepath.Join(m.config.DataDir, "admin-sessions", name))
		if err != nil {
			return adminSession{}, err
		}
		sealed = string(raw)
	}
	raw, err := m.open(sealed)
	if err != nil {
		return adminSession{}, err
	}
	defer safe.ZeroMemory(raw)
	session, err := cast.FromJSON[adminSession](raw)
	if err == nil && (session.User != "admin" || session.CSRF == "" || session.Hash == "" || session.Expires.IsZero() || session.Expires.After(time.Now().Add(8*time.Hour+time.Minute))) {
		err = fmt.Errorf("invalid session")
	}
	return session, err
}
func (m *Manager) deleteAdminSession(token string) error {
	name, err := m.sessionName(token)
	if err != nil {
		return err
	}
	if m.rd != nil {
		return m.rd.Do("DEL", m.config.Prefix+":admin-session:"+name).Error
	}
	path := filepath.Join(m.config.DataDir, "admin-sessions", name)
	if !file.Exists(path) {
		return nil
	}
	return file.Remove(path)
}
