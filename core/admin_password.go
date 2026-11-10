package gateway

import (
	"github.com/api-go/file"
	"github.com/api-go/log"
	"github.com/api-go/redis"
	"github.com/api-go/safe"
	"bytes"
	"fmt"
	"golang.org/x/crypto/bcrypt"
	"golang.org/x/term"
	"io"
	"os"
	"path/filepath"
)

func credentialConfig(config Config) Config {
	if config.DataDir == "" {
		config.DataDir = "data/gateway"
	}
	if config.Prefix == "" {
		config.Prefix = "gateway"
	}
	return config
}

// SetAdminPassword sets the sole administrator credential without changing Host state.
// It works while Gateway is running or stopped. Existing sessions are revoked on their next request.
func SetAdminPassword(config Config, password *safe.SafeBuf) error {
	if password == nil {
		return fmt.Errorf("password required")
	}
	plain := password.Open()
	defer plain.Close()
	if len(plain.Data) < 12 || len(plain.Data) > 72 {
		return fmt.Errorf("password requires 12–72 bytes")
	}
	hash, err := bcrypt.GenerateFromPassword(plain.Data, bcrypt.DefaultCost)
	if err != nil {
		return err
	}
	config = credentialConfig(config)
	if config.Redis != "" {
		rd := redis.GetRedis(config.Redis, log.DefaultLogger)
		if rd.Error != nil {
			return rd.Error
		}
		return rd.Do("SET", config.Prefix+":admin-password", hash).Error
	}
	if err := file.Mkdir(config.DataDir); err != nil {
		return err
	}
	if err := os.Chmod(config.DataDir, 0700); err != nil {
		return err
	}
	return privateWrite(filepath.Join(config.DataDir, "admin.password"), hash)
}

func (m *Manager) adminHash() (string, error) {
	if m.rd != nil {
		result := m.rd.Do("GET", m.config.Prefix+":admin-password")
		return result.String(), result.Error
	}
	name := filepath.Join(m.config.DataDir, "admin.password")
	if !file.Exists(name) {
		return "", nil
	}
	data, err := file.ReadBytes(name)
	return string(data), err
}

func adminPasswordCommand(config Config) error {
	var raw []byte
	var err error
	if term.IsTerminal(int(os.Stdin.Fd())) {
		fmt.Fprint(os.Stderr, "管理员密码（12–72 字节）：")
		raw, err = term.ReadPassword(int(os.Stdin.Fd()))
		fmt.Fprintln(os.Stderr)
		if err != nil {
			return err
		}
		defer safe.ZeroMemory(raw)
		fmt.Fprint(os.Stderr, "再次输入：")
		confirmation, readErr := term.ReadPassword(int(os.Stdin.Fd()))
		fmt.Fprintln(os.Stderr)
		if readErr != nil {
			return readErr
		}
		defer safe.ZeroMemory(confirmation)
		if !bytes.Equal(raw, confirmation) {
			return fmt.Errorf("passwords do not match")
		}
	} else {
		raw, err = io.ReadAll(io.LimitReader(os.Stdin, 75))
		if err != nil {
			return err
		}
		defer safe.ZeroMemory(raw)
		if len(raw) > 74 {
			return fmt.Errorf("password input too long")
		}
		raw = bytes.TrimSuffix(bytes.TrimSuffix(raw, []byte("\n")), []byte("\r"))
	}
	secret := safe.NewSafeBufAndErase(raw)
	defer secret.Close()
	if err := SetAdminPassword(config, secret); err != nil {
		return err
	}
	fmt.Println("管理员密码已设置；现有登录会话将失效。")
	return nil
}
