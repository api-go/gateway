package gateway

import (
	"github.com/api-go/discover"
	"sort"
	"strings"
)

type ServiceStatus struct {
	Name  string
	Nodes []string
}

// Services lists named upstreams referenced by configuration, with discovered node addresses.
func (m *Manager) Services() []ServiceStatus {
	m.mu.Lock()
	names := map[string]bool{}
	for _, record := range m.state.Hosts {
		hosts := []Host{record.Draft}
		if record.Current != nil {
			hosts = append(hosts, *record.Current)
		}
		for _, host := range hosts {
			for _, rule := range host.Proxies {
				name := rule.ToApp
				if rule.To != "" && !strings.Contains(rule.To, "://") {
					name, _, _ = strings.Cut(rule.To, "/")
				}
				if name != "" && !strings.Contains(name, "://") {
					names[name] = true
				}
			}
		}
	}
	m.mu.Unlock()
	result := make([]ServiceStatus, 0, len(names))
	for name := range names {
		nodes := discover.GetAppNodes(name)
		addresses := make([]string, 0, len(nodes))
		for address := range nodes {
			addresses = append(addresses, address)
		}
		sort.Strings(addresses)
		result = append(result, ServiceStatus{Name: name, Nodes: addresses})
	}
	sort.Slice(result, func(i, j int) bool { return result[i].Name < result[j].Name })
	return result
}
