package gateway

import (
	"github.com/api-go/cast"
	"github.com/api-go/file"
	"fmt"
	redisDriver "github.com/gomodule/redigo/redis"
	"path/filepath"
)

// Recovery never overwrites an existing state or a different encryption key.
const restoreClusterState = `local state=redis.call('GET',KEYS[1]); if state then return state end; if ARGV[3]=='1' then local key=redis.call('GET',KEYS[2]); if key and key~=ARGV[2] then return redis.error_reply('recovery encryption key mismatch') end; redis.call('SET',KEYS[2],ARGV[2]); end; redis.call('SET',KEYS[1],ARGV[1]); return ARGV[1]`

func (m *Manager) restoreSnapshot(raw string, key []byte) (string, error) {
	if len(raw) == 0 || len(raw) > maxStateBytes {
		return "", fmt.Errorf("invalid recovery snapshot size")
	}
	if err := validateCertificateStateKey([]byte(raw), key); err != nil {
		return "", err
	}
	state, err := cast.FromJSON[State](raw)
	if err != nil || state.Format != 1 {
		return "", fmt.Errorf("invalid recovery snapshot")
	}
	for _, record := range state.Hosts {
		if record.Current != nil {
			if err := validateHost(*record.Current); err != nil {
				return "", err
			}
		}
	}
	connection, err := m.rd.GetConnection()
	if err != nil {
		return "", err
	}
	defer connection.Close()
	// Do not send recovery key material through the command logger.
	storeKey := 0
	var recoveryKey []byte
	if m.automaticKey {
		storeKey = 1
		recoveryKey = key
	}
	return redisDriver.String(connection.Do("EVAL", restoreClusterState, 2, m.stateKey(), m.config.Prefix+":certificate-key", raw, recoveryKey, storeKey))
}
func (m *Manager) recoverMissingState() (string, error) {
	raw := m.raw
	if raw == "" {
		name := filepath.Join(m.config.DataDir, "state.json")
		if !file.Exists(name) {
			return "", nil
		}
		data, err := file.ReadBytes(name)
		if err != nil {
			return "", err
		}
		raw = string(data)
	}
	plain := m.key.Open()
	defer plain.Close()
	return m.restoreSnapshot(raw, plain.Data)
}
