package gateway

import (
	"github.com/api-go/cast"
	"github.com/api-go/log"
	"github.com/api-go/redis"
	"fmt"
	"net"
	"sort"
	"strings"
)

type RegistryNode struct {
	Address          string
	Weight           int
	Alive            bool
	HeartbeatSeconds int
}
type RegisteredService struct {
	Name               string
	Self               bool
	Protocol           string // Actual gateway call protocol; not advertised by registration.
	ProtocolConfigured bool
	Nodes              []RegistryNode
}

// RegistryServices reads authoritative registration hashes and heartbeat keys; it never changes registrations.
func (m *Manager) RegistryServices() ([]RegisteredService, error) {
	m.mu.Lock()
	name := m.registryName
	m.mu.Unlock()
	if name == "" {
		return []RegisteredService{}, nil
	}
	entries, err := ReadRegistryServices(redis.GetRedis(name, log.DefaultLogger))
	if err != nil {
		return nil, err
	}
	m.mu.Lock()
	defer m.mu.Unlock()
	for i := range entries {
		entries[i].Self = entries[i].Name == m.registrySelf
		entries[i].Protocol = "H2C"
		// Service initializes Discover's native Calls using HTTP/HTTPS/H2C prefixes.
		if protocol, ok := m.registryProtocols[entries[i].Name]; ok {
			entries[i].Protocol = protocol
			entries[i].ProtocolConfigured = true
		} else if call := m.registryCalls[entries[i].Name]; call != "" {
			for _, part := range strings.Split(strings.ToLower(call), ":") {
				switch part {
				case "http", "1":
					entries[i].Protocol = "HTTP"
				case "h2c", "2":
					entries[i].Protocol = "H2C"
				case "https", "s":
					entries[i].Protocol = "HTTPS"
				}
			}
			entries[i].ProtocolConfigured = true
		}
		if entries[i].Self {
			entries[i].Protocol = m.registrySelfProtocol
			entries[i].ProtocolConfigured = true
		}
	}
	return entries, nil
}

// ReadRegistryServices enumerates the native Discover Redis schema, including expired hash members.
func ReadRegistryServices(rd *redis.Redis) ([]RegisteredService, error) {
	if rd == nil {
		return nil, fmt.Errorf("registry is not configured")
	}
	keys, err := scanKeys(rd, "*")
	if err != nil {
		return nil, err
	}
	if len(keys) > 10000 {
		return nil, fmt.Errorf("registry exceeds 10000 keys")
	}
	result := []RegisteredService{}
	for _, name := range keys {
		kind := rd.Do("TYPE", name)
		if kind.Error != nil {
			return nil, kind.Error
		}
		if kind.String() != "hash" {
			continue
		}
		members := rd.Do("HGETALL", name)
		if members.Error != nil {
			return nil, members.Error
		}
		values := members.StringMap()
		if len(values) > 1000 {
			return nil, fmt.Errorf("registry node limit exceeded")
		}
		entry := RegisteredService{Name: name, Nodes: []RegistryNode{}}
		for address, weight := range values {
			host, port, err := net.SplitHostPort(address)
			if err != nil || host == "" || cast.Int(port) <= 0 || cast.Int(port) > 65535 || cast.Int(weight) <= 0 {
				continue
			}
			ttl := rd.Do("TTL", name+"_"+address)
			if ttl.Error != nil {
				return nil, ttl.Error
			}
			entry.Nodes = append(entry.Nodes, RegistryNode{Address: address, Weight: cast.Int(weight), Alive: ttl.Int() >= -1, HeartbeatSeconds: ttl.Int()})
		}
		if len(entry.Nodes) > 0 {
			sort.Slice(entry.Nodes, func(i, j int) bool { return entry.Nodes[i].Address < entry.Nodes[j].Address })
			result = append(result, entry)
		}
	}
	sort.Slice(result, func(i, j int) bool { return result[i].Name < result[j].Name })
	return result, nil
}

// Capture caller configuration at initialization/reload; request handlers do not read mutable Service globals.
func (m *Manager) captureRegistryLocked() {
	m.registrySelf = m.server.Config.App
	m.registrySelfProtocol = "HTTP / HTTPS"
	if strings.Contains(m.server.Config.Listen, "h2c") {
		m.registrySelfProtocol = "H2C"
	} else if m.server.Config.Listen != "" && !strings.Contains(m.server.Config.Listen, "https") {
		m.registrySelfProtocol = "HTTP"
	}
	m.registryProtocols = map[string]string{}
	for name, call := range m.server.Config.Calls {
		protocol := "HTTP"
		if call.SSL {
			protocol = "HTTPS"
		} else if call.Http2 {
			protocol = "H2C"
		}
		m.registryProtocols[name] = protocol
	}
}
