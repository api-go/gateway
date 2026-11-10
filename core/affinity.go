package gateway

import (
	"github.com/api-go/discover"
	"github.com/api-go/service"
	"context"
	"crypto/sha256"
	"fmt"
	"net/http"
	"strings"
	"sync"
	"time"
)

type affinityContextKey struct{}
type affinityRequest struct {
	manager    *Manager
	scope, key string
	config     AffinityConfig
}

var affinityInstall sync.Once

func installAffinityBalancer() {
	affinityInstall.Do(func() { discover.SetLoadBalancer(&affinityBalancer{}) })
}

type affinityBalancer struct{ discover.DefaultLoadBalancer }

func validateAffinityRules(rules []AffinityRule) error {
	if len(rules) > 100 {
		return fmt.Errorf("too many affinity rules")
	}
	ids := map[string]bool{}
	for _, rule := range rules {
		if rule.ID == "" || len(rule.ID) > 80 || ids[rule.ID] {
			return fmt.Errorf("invalid or duplicate affinity rule ID")
		}
		ids[rule.ID] = true
		if !strings.HasPrefix(rule.Path, "/") || strings.ContainsAny(rule.Path, "?(\r\n") || strings.Contains(strings.TrimSuffix(rule.Path, "/*"), "*") {
			return fmt.Errorf("affinity path requires exact path or trailing /*")
		}
		for _, method := range rule.Methods {
			if method == "" || method != strings.ToUpper(method) || strings.ContainsAny(method, " \r\n\t") {
				return fmt.Errorf("invalid affinity HTTP method")
			}
		}
		if err := validateAffinity(rule.Affinity); err != nil {
			return err
		}
	}
	return nil
}
func validateAffinity(config AffinityConfig) error {
	if config.Mode == "" || config.Mode == "off" {
		return nil
	}
	switch config.Mode {
	case "auto", "hash", "manual", "redis":
	default:
		return fmt.Errorf("invalid affinity mode")
	}
	if config.HashStrategy != "" && config.HashStrategy != "direct" && config.HashStrategy != "gradual" {
		return fmt.Errorf("invalid affinity hash strategy")
	}
	if config.HashStrategy == "gradual" && config.Mode != "hash" {
		return fmt.Errorf("gradual strategy requires hash mode")
	}
	switch config.Source {
	case "header", "cookie", "query":
		if config.Name == "" {
			return fmt.Errorf("affinity source requires a name")
		}
	case "session", "ip":
	default:
		return fmt.Errorf("invalid affinity source")
	}
	if len(config.Name) > 128 || strings.ContainsAny(config.Name, "\r\n\t :") {
		return fmt.Errorf("invalid affinity name")
	}
	if config.TTLSeconds < 0 || config.TTLSeconds > 604800 {
		return fmt.Errorf("affinity TTL requires 0–604800 seconds")
	}
	if config.Missing != "" && config.Missing != "balance" && config.Missing != "reject" {
		return fmt.Errorf("invalid affinity missing policy")
	}
	if config.Transform != "" && config.Transform != "full" && config.Transform != "last" && config.Transform != "lastDigit" {
		return fmt.Errorf("invalid affinity transform")
	}
	if len(config.Mappings) > 1000 {
		return fmt.Errorf("too many manual mappings")
	}
	if config.Mode == "manual" {
		if len(config.Mappings) == 0 {
			return fmt.Errorf("manual affinity requires mappings")
		}
		values := map[string]bool{}
		for _, mapping := range config.Mappings {
			if len(mapping.Values) == 0 || len(mapping.Nodes) == 0 || len(mapping.Nodes) > 32 {
				return fmt.Errorf("manual mapping requires values and primary/backup nodes")
			}
			for _, value := range mapping.Values {
				if value == "" || len(value) > 4096 || values[value] {
					return fmt.Errorf("invalid or duplicate manual value")
				}
				values[value] = true
			}
			seen := map[string]bool{}
			for _, node := range mapping.Nodes {
				if node == "" || len(node) > 512 || seen[node] || strings.ContainsAny(node, " /\r\n") {
					return fmt.Errorf("invalid manual node")
				}
				seen[node] = true
			}
		}
	}
	return nil
}
func (m *Manager) prepareAffinityRequest(host Host, request *service.Request, response *service.Response, scopes map[string]string) bool {
	config := host.Affinity
	scope := host.ID + "\x00legacy"
	for _, rule := range host.AffinityRules {
		if _, matched := matchRule(rule.Path, "", request.URL.Path); !matched {
			continue
		}
		allowed := len(rule.Methods) == 0
		for _, method := range rule.Methods {
			if method == request.Method {
				allowed = true
				break
			}
		}
		if !allowed {
			continue
		}
		config = rule.Affinity
		scope = host.ID + "\x00" + rule.ID
		break
	}
	if config.Mode == "" || config.Mode == "off" {
		return false
	}
	var key string
	switch config.Source {
	case "header":
		key = request.Request.Header.Get(config.Name)
	case "query":
		key = request.URL.Query().Get(config.Name)
	case "cookie":
		if cookie, err := request.Cookie(config.Name); err == nil {
			key = cookie.Value
		}
	case "session":
		name := config.Name
		if name == "" {
			name = "sessionid"
		}
		key = request.Request.Header.Get(name)
		if key == "" {
			if cookie, err := request.Cookie(name); err == nil {
				key = cookie.Value
			}
		}
		if key == "" {
			key = request.URL.Query().Get(name)
		}
	case "ip":
		key = request.ClientIp()
	}
	if len(key) > 4096 {
		response.WriteHeader(400)
		response.WriteString("Affinity identifier too long")
		return true
	}
	if key != "" {
		switch config.Transform {
		case "last":
			key = key[len(key)-1:]
		case "lastDigit":
			for i := range len(key) {
				if key[i] < '0' || key[i] > '9' {
					key = ""
					break
				}
			}
			if key != "" {
				key = key[len(key)-1:]
			}
		}
	}
	if key == "" {
		if config.Missing == "reject" {
			response.WriteHeader(http.StatusBadRequest)
			response.WriteString("Affinity key required")
			return true
		}
		return false
	}
	// Compiled scope is also the identity of each shared shard table.
	scope = scopes[scope]
	if (config.Mode == "auto" || config.Mode == "redis" || config.Mode == "hash" && config.HashStrategy == "gradual") && (!m.affinity.ready.Load() || config.HashStrategy == "gradual" && !m.affinityShardsReady(scope)) {
		response.WriteHeader(503)
		response.WriteString("Affinity synchronization pending")
		return true
	}
	// Compiled once at publication, never serialize configuration on a request.
	value := affinityRequest{manager: m, scope: scope, key: key, config: config}
	request.Request = request.Request.WithContext(context.WithValue(request.Context(), affinityContextKey{}, value))
	return false
}
func affinityNode(nodes []*discover.NodeInfo, key string) *discover.NodeInfo {
	var selected *discover.NodeInfo
	var best [32]byte
	for _, node := range nodes {
		weight, _, _ := node.State()
		if weight <= 0 {
			continue
		}
		score := sha256.Sum256([]byte(key + "\x00" + node.Addr))
		if selected == nil || strings.Compare(string(score[:]), string(best[:])) > 0 {
			selected = node
			best = score
		}
	}
	return selected
}
func affinityChain(nodes []*discover.NodeInfo, key string) []string {
	remaining := append([]*discover.NodeInfo(nil), nodes...)
	chain := []string{}
	for len(chain) < 3 {
		node := affinityNode(remaining, key)
		if node == nil {
			break
		}
		chain = append(chain, node.Addr)
		for i, item := range remaining {
			if item == node {
				remaining = append(remaining[:i], remaining[i+1:]...)
				break
			}
		}
	}
	return chain
}
func chooseAffinityChain(chain []string, nodes []*discover.NodeInfo) *discover.NodeInfo {
	for _, address := range chain {
		for _, node := range nodes {
			weight, _, _ := node.State()
			if address == node.Addr && weight > 0 {
				return node
			}
		}
	}
	return nil
}
func (lb *affinityBalancer) Next(app *discover.AppClient, nodes []*discover.NodeInfo, request *http.Request) *discover.NodeInfo {
	if request == nil {
		return lb.DefaultLoadBalancer.Next(app, nodes, request)
	}
	value, ok := request.Context().Value(affinityContextKey{}).(affinityRequest)
	if !ok {
		return lb.DefaultLoadBalancer.Next(app, nodes, request)
	}
	key := value.scope + "\x00" + app.App + "\x00" + value.key
	if value.config.Mode == "manual" {
		for _, mapping := range value.config.Mappings {
			for _, match := range mapping.Values {
				if value.key == match {
					return chooseAffinityChain(mapping.Nodes, nodes)
				}
			}
		}
		return nil
	}
	if value.config.Mode == "hash" {
		if value.config.HashStrategy == "gradual" {
			return value.manager.chooseAffinityShard(value.scope, app.App, key, nodes)
		}
		return affinityNode(nodes, key)
	}
	digest := sha256.Sum256([]byte(key))
	now := time.Now().Unix()
	chain, exists := value.manager.affinity.lookup(digest, now)
	if !exists {
		chain = affinityChain(nodes, key)
		if len(chain) == 0 {
			return nil
		}
		ttl := value.config.TTLSeconds
		if ttl == 0 {
			ttl = 3600
		}
		var accepted bool
		chain, accepted = value.manager.affinity.insert(digest, affinityBinding{Nodes: chain, Expires: now + int64(ttl)}, true, now)
		if !accepted {
			return nil
		}
	}
	if selected := chooseAffinityChain(chain, nodes); selected != nil {
		return selected
	}
	// All original primary/backup nodes are gone: deterministically choose a survivor.
	return affinityNode(nodes, key)
}

func compileAffinityScopes(host Host) map[string]string {
	scopes := map[string]string{}
	add := func(id string, config AffinityConfig) {
		sum := sha256.Sum256([]byte(affinityConfigScope(config)))
		scopes[id] = id + "\x00" + string(sum[:])
	}
	add(host.ID+"\x00legacy", host.Affinity)
	for _, rule := range host.AffinityRules {
		add(host.ID+"\x00"+rule.ID, rule.Affinity)
	}
	return scopes
}
