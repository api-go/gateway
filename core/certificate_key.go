package gateway

import (
	"apigo.cc/go/cast"
	goCrypto "apigo.cc/go/crypto"
	"apigo.cc/go/encoding"
	"apigo.cc/go/file"
	"apigo.cc/go/safe"
	"fmt"
	"path/filepath"
)

// The state comparison prevents publishing a new key over concurrently saved certificates.
const initializeCertificateKey = `local key=redis.call('GET',KEYS[1]); if key then return key end; local state=redis.call('GET',KEYS[2]) or ''; if state~=ARGV[1] then return false end; redis.call('SET',KEYS[1],ARGV[2]); return ARGV[2]`

func (m *Manager) automaticCertificateKey() ([]byte, error) {
	name := filepath.Join(m.config.DataDir, "certificate.key")
	var local []byte
	if file.Exists(name) {
		var err error
		local, err = file.ReadBytes(name)
		if err != nil {
			return nil, err
		}
		if len(local) != 32 {
			safe.ZeroMemory(local)
			return nil, fmt.Errorf("invalid persisted certificate key")
		}
	}
	defer func() { safe.ZeroMemory(local) }()
	if m.rd == nil {
		if local == nil {
			local = safe.MakeSafeToken(32)
			if file.Exists(filepath.Join(m.config.DataDir, "state.json")) {
				raw, err := file.ReadBytes(filepath.Join(m.config.DataDir, "state.json"))
				if err != nil {
					return nil, err
				}
				if err := validateCertificateStateKey(raw, local); err != nil {
					return nil, err
				}
			}
			if err := privateWrite(name, local); err != nil {
				return nil, err
			}
		}
		return append([]byte(nil), local...), nil
	}
	keyName := m.config.Prefix + ":certificate-key"
	result := m.rd.Do("GET", keyName)
	if result.Error != nil {
		if local != nil && file.Exists(filepath.Join(m.config.DataDir, "state.json")) {
			return append([]byte(nil), local...), nil
		}
		return nil, fmt.Errorf("automatic certificate key store unavailable: %w", result.Error)
	}
	raw := result.Bytes()
	if len(raw) == 0 {
		snapshot := m.rd.Do("GET", m.stateKey())
		if snapshot.Error != nil {
			return nil, snapshot.Error
		}
		if local == nil {
			local = safe.MakeSafeToken(32)
		}
		if err := validateCertificateStateKey(snapshot.Bytes(), local); err != nil {
			return nil, err
		}
		created := m.rd.Do("EVAL", initializeCertificateKey, 2, keyName, m.stateKey(), snapshot.Bytes(), local)
		if created.Error != nil {
			return nil, created.Error
		}
		raw = created.Bytes()
		if len(raw) == 0 {
			return nil, fmt.Errorf("certificate state changed during key initialization; retry startup")
		}
	}
	if len(raw) != 32 {
		safe.ZeroMemory(raw)
		return nil, fmt.Errorf("invalid shared certificate key")
	}
	snapshot := m.rd.Do("GET", m.stateKey())
	if snapshot.Error != nil {
		safe.ZeroMemory(raw)
		return nil, snapshot.Error
	}
	if err := validateCertificateStateKey(snapshot.Bytes(), raw); err != nil {
		safe.ZeroMemory(raw)
		return nil, err
	}
	if err := privateWrite(name, raw); err != nil {
		safe.ZeroMemory(raw)
		return nil, err
	}
	return raw, nil
}

// Legacy encrypted state must be readable before adopting a local key as the shared key.
func validateCertificateStateKey(raw, key []byte) error {
	if len(raw) == 0 {
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
	check := func(sealed string) error {
		if sealed == "" {
			return nil
		}
		encrypted, err := encoding.UnBase64(sealed)
		if err != nil || len(encrypted) < 28 {
			return fmt.Errorf("invalid encrypted certificate key")
		}
		plain, err := goCrypto.AESGCM.Decrypt(encrypted[12:], key, encrypted[:12])
		safe.ZeroMemory(plain)
		if err != nil {
			return fmt.Errorf("existing encrypted credentials require their original encryption key; retain the previous CertificateKey or GATEWAY_CERTIFICATE_KEY")
		}
		return nil
	}
	for _, channel := range state.AlertChannels {
		if err := check(channel.SealedCredentials); err != nil {
			return err
		}
	}
	for _, entry := range state.Certificates {
		if err := check(entry.SealedKey); err != nil {
			return err
		}
	}
	for _, record := range state.Hosts {
		if err := check(record.Draft.Monitor.SealedHeaders); err != nil {
			return err
		}
		if err := check(record.Draft.HTTPS.SealedKey); err != nil {
			return err
		}
		if record.Current != nil {
			if err := check(record.Current.Monitor.SealedHeaders); err != nil {
				return err
			}
			if err := check(record.Current.HTTPS.SealedKey); err != nil {
				return err
			}
		}
		for _, version := range record.History {
			if err := check(version.Host.Monitor.SealedHeaders); err != nil {
				return err
			}
			if err := check(version.Host.HTTPS.SealedKey); err != nil {
				return err
			}
		}
	}
	return nil
}
