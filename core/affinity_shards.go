package gateway

import (
	"github.com/api-go/cast"
	"github.com/api-go/discover"
	"github.com/api-go/encoding"
	"context"
	"crypto/sha256"
	"fmt"
	redisDriver "github.com/gomodule/redigo/redis"
	"math"
	"sort"
	"strings"
	"sync"
	"sync/atomic"
	"time"
)

const affinityShardCount = 4096
const affinityShardLimit = 64

// Tables are immutable after publication. Versions belong to the control plane,
// never to a client cookie. Redis fences every writer and orders table updates.
type affinityShardTable struct {
	Scope     string
	App       string
	Version   int64
	Updated   int64
	Nodes     []string
	Owners    []uint16
	Moved     int64
	Balancing bool
}
type affinityShardLoad struct {
	Scope    string
	Node     string
	Sent     int64
	Hits     []uint64
	Topology []string
}
type affinityShardRuntime struct {
	scope, app string
	table      atomic.Pointer[affinityShardTable]
	hits       [affinityShardCount]atomic.Uint64
	previous   [affinityShardCount]uint64
	recent     [affinityShardCount]float64
	loadMu     sync.Mutex
	loads      map[string]affinityShardLoad
	sampled    time.Time
}

type affinityShardRegistry struct {
	tables map[string]*affinityShardRuntime
	rules  map[string][]*affinityShardRuntime
}

func affinityShardScope(scope, app string) string {
	digest := sha256.Sum256([]byte(scope + "\x00" + app))
	return encoding.Hex(digest[:])
}
func (s *affinityStore) shardRuntime(scope, app string) *affinityShardRuntime {
	s.shardMu.Lock()
	defer s.shardMu.Unlock()
	if r := s.shards[scope]; r != nil {
		return r
	}
	if len(s.shards) >= affinityShardLimit {
		return nil
	}
	r := &affinityShardRuntime{scope: scope, app: app, loads: map[string]affinityShardLoad{}}
	s.shards[scope] = r
	return r
}

type affinityShardSpec struct {
	rule, app string
}

func affinityShardSpecs(active *activeHosts) (map[string]affinityShardSpec, error) {
	specs := map[string]affinityShardSpec{}
	for _, record := range active.hosts {
		if record.Current == nil || !record.Current.Enabled {
			continue
		}
		host := *record.Current
		scopes := compileAffinityScopes(host)
		configs := map[string]AffinityConfig{host.ID + "\x00legacy": host.Affinity}
		for _, rule := range host.AffinityRules {
			configs[host.ID+"\x00"+rule.ID] = rule.Affinity
		}
		for id, config := range configs {
			if config.Mode != "hash" || config.HashStrategy != "gradual" {
				continue
			}
			scope := scopes[id]
			for _, proxy := range host.Proxies {
				target := proxy.To
				if target == "" {
					target = proxy.ToApp
				}
				if strings.Contains(target, "://") {
					continue
				}
				app, _, _ := strings.Cut(target, "/")
				if app == "" {
					continue
				}
				specs[affinityShardScope(scope, app)] = affinityShardSpec{rule: scope, app: app}
				if len(specs) > affinityShardLimit {
					return nil, fmt.Errorf("at most %d active gradual shard tables", affinityShardLimit)
				}
			}
		}
	}
	return specs, nil
}
func (m *Manager) configureAffinityShards(active *activeHosts) error {
	specs, err := affinityShardSpecs(active)
	if err != nil {
		return err
	}
	if len(specs) > 0 && m.rd == nil {
		return fmt.Errorf("gradual affinity shards require Redis registry")
	}
	registry := &affinityShardRegistry{tables: map[string]*affinityShardRuntime{}, rules: map[string][]*affinityShardRuntime{}}
	retained := map[string]*affinityShardRuntime{}
	m.affinity.shardMu.Lock()
	defer m.affinity.shardMu.Unlock()
	for scope, spec := range specs {
		r := m.affinity.shards[scope]
		if r == nil {
			r = &affinityShardRuntime{scope: scope, app: spec.app, loads: map[string]affinityShardLoad{}}
		}
		registry.tables[scope] = r
		registry.rules[spec.rule] = append(registry.rules[spec.rule], r)
		retained[scope] = r
	}
	m.affinity.shards = retained
	m.affinity.shardRegistry.Store(registry)
	return nil
}
func (m *Manager) affinityShardsReady(scope string) bool {
	registry := m.affinity.shardRegistry.Load()
	if registry == nil {
		return false
	}
	for _, r := range registry.rules[scope] {
		if r.table.Load() == nil {
			return false
		}
	}
	return true
}
func (m *Manager) chooseAffinityShard(scope, app, key string, nodes []*discover.NodeInfo) *discover.NodeInfo {
	registry := m.affinity.shardRegistry.Load()
	if registry == nil {
		return nil
	}
	r := registry.tables[affinityShardScope(scope, app)]
	if r == nil {
		return nil
	}
	table := r.table.Load()
	if table == nil {
		return nil
	}
	digest := sha256.Sum256([]byte(key))
	bucket := int(digest[0])<<4 | int(digest[1]>>4)
	r.hits[bucket].Add(1)
	address := table.Nodes[table.Owners[bucket]]
	for _, node := range nodes {
		weight, _, _ := node.State()
		if node.Addr == address && weight > 0 {
			return node
		}
	}
	// All IDs in a failed shard choose the same deterministic live fallback.
	return affinityNode(nodes, r.scope+"\x00shard-"+cast.String(bucket))
}
func validateAffinityShardTable(table *affinityShardTable) error {
	raw, err := encoding.UnHex(table.Scope)
	if err != nil || len(raw) != 32 || table.App == "" || len(table.App) > 256 || table.Version < 1 || len(table.Owners) != affinityShardCount || len(table.Nodes) < 1 || len(table.Nodes) > 1024 {
		return fmt.Errorf("invalid affinity shard table")
	}
	seen := map[string]bool{}
	for _, node := range table.Nodes {
		if node == "" || len(node) > 256 || seen[node] {
			return fmt.Errorf("invalid shard node")
		}
		seen[node] = true
	}
	for _, owner := range table.Owners {
		if int(owner) >= len(table.Nodes) {
			return fmt.Errorf("invalid shard owner")
		}
	}
	return nil
}
func (s *affinityStore) applyShardTable(table *affinityShardTable) error {
	if err := validateAffinityShardTable(table); err != nil {
		return err
	}
	registry := s.shardRegistry.Load()
	if registry != nil && registry.tables[table.Scope] == nil {
		return nil
	}
	r := s.shardRuntime(table.Scope, table.App)
	if r != nil && r.app != table.App {
		return fmt.Errorf("shard application mismatch")
	}
	if r == nil {
		return fmt.Errorf("affinity shard table capacity exceeded")
	}
	for {
		old := r.table.Load()
		if old != nil && old.Version >= table.Version {
			return nil
		}
		if r.table.CompareAndSwap(old, table) {
			return nil
		}
	}
}
func (s *affinityStore) applyShardLoad(load *affinityShardLoad) error {
	if len(load.Hits) != affinityShardCount || load.Node == "" || len(load.Node) > 256 || load.Sent > time.Now().UnixMilli()+30000 {
		return fmt.Errorf("invalid shard load")
	}
	registry := s.shardRegistry.Load()
	if registry == nil {
		return nil
	}
	r := registry.tables[load.Scope]
	if r == nil {
		return nil
	}
	r.loadMu.Lock()
	defer r.loadMu.Unlock()
	for node, old := range r.loads {
		if time.Now().UnixMilli()-old.Sent > 30000 {
			delete(r.loads, node)
		}
	}
	if old, ok := r.loads[load.Node]; ok && old.Sent >= load.Sent {
		return nil
	}
	if _, exists := r.loads[load.Node]; !exists && len(r.loads) >= 256 {
		return fmt.Errorf("too many shard load reporters")
	}
	r.loads[load.Node] = *load
	return nil
}

const affinityShardLoadPut = `redis.call('SET',KEYS[1],ARGV[1],'EX',10); redis.call('PUBLISH',KEYS[2],ARGV[2]); return 1`

func (r *affinityShardRuntime) recoverLoads(connection redisDriver.Conn, prefix string, peers []NodeStatus, now time.Time) error {
	args := []any{}
	r.loadMu.Lock()
	for _, peer := range peers {
		if !peer.AffinityReady {
			continue
		}
		load, ok := r.loads[peer.ID]
		if !ok || now.UnixMilli()-load.Sent > 2000 {
			args = append(args, prefix+":affinity-shard-load:"+r.scope+":"+peer.ID)
		}
	}
	r.loadMu.Unlock()
	if len(args) == 0 {
		return nil
	}
	values, err := redisDriver.Values(connection.Do("MGET", args...))
	if err != nil {
		return err
	}
	for _, value := range values {
		if value == nil {
			continue
		}
		raw, err := redisDriver.Bytes(value, nil)
		if err != nil {
			return err
		}
		load, err := cast.FromJSON[affinityShardLoad](raw)
		if err != nil {
			return err
		}
		if len(load.Hits) != affinityShardCount || load.Scope != r.scope {
			return fmt.Errorf("invalid retained shard load")
		}
		r.loadMu.Lock()
		if old, ok := r.loads[load.Node]; !ok || load.Sent > old.Sent {
			r.loads[load.Node] = load
		}
		r.loadMu.Unlock()
	}
	return nil
}

const affinityShardLease = `local old=redis.call('GET',KEYS[1]); if old==ARGV[1] then redis.call('EXPIRE',KEYS[1],5); return ARGV[1] end; if not old then redis.call('SET',KEYS[1],ARGV[1],'EX',5,'NX'); return redis.call('GET',KEYS[1]) end; return old`
const affinityShardCAS = `if redis.call('GET',KEYS[4])~=ARGV[4] then return 0 end; local old=redis.call('GET',KEYS[1]) or ''; if old~=ARGV[1] then return 0 end; redis.call('SET',KEYS[1],ARGV[2],'EX',3600); local id=redis.call('XADD',KEYS[2],'MAXLEN',512,'*','data',ARGV[3]); redis.call('EXPIRE',KEYS[2],300); local t=redis.call('TIME'); redis.call('XTRIM',KEYS[2],'MINID',tostring(tonumber(t[1])*1000+math.floor(tonumber(t[2])/1000)-300000)..'-0'); redis.call('PUBLISH',KEYS[3],ARGV[3]); return 1`

func affinityShardAddresses(app string) []string {
	addresses := []string{}
	for address, node := range discover.GetAppNodes(app) {
		weight, _, _ := node.State()
		if weight > 0 {
			addresses = append(addresses, address)
		}
	}
	sort.Strings(addresses)
	return addresses
}
func affinityShardOwner(scope string, bucket int, addresses []string) int {
	best := -1
	var score [32]byte
	key := scope + "\x00shard-" + cast.String(bucket)
	for i, address := range addresses {
		value := sha256.Sum256([]byte(key + "\x00" + address))
		if best < 0 || strings.Compare(string(value[:]), string(score[:])) > 0 {
			best = i
			score = value
		}
	}
	return best
}
func (r *affinityShardRuntime) sample(node string, addresses []string, now time.Time) affinityShardLoad {
	elapsed := now.Sub(r.sampled).Seconds()
	if r.sampled.IsZero() {
		elapsed = 1
	}
	decay := math.Exp(-elapsed / 30)
	hits := make([]uint64, affinityShardCount)
	for i := range hits {
		current := r.hits[i].Load()
		r.recent[i] = r.recent[i]*decay + float64(current-r.previous[i])
		r.previous[i] = current
		hits[i] = uint64(r.recent[i] * 1000)
	}
	r.sampled = now
	return affinityShardLoad{Scope: r.scope, Node: node, Sent: now.UnixMilli(), Hits: hits, Topology: addresses}
}
func (r *affinityShardRuntime) aggregate(peers []NodeStatus, addresses []string, now time.Time) ([]uint64, bool) {
	loads := make([]uint64, affinityShardCount)
	r.loadMu.Lock()
	defer r.loadMu.Unlock()
	for _, peer := range peers {
		if !peer.AffinityReady {
			continue
		}
		report, ok := r.loads[peer.ID]
		if !ok || now.UnixMilli()-report.Sent > 5000 || strings.Join(report.Topology, "\x00") != strings.Join(addresses, "\x00") {
			return nil, false
		}
		for i, v := range report.Hits {
			loads[i] += v
		}
	}
	return loads, true
}
func (r *affinityShardRuntime) plan(previous *affinityShardTable, addresses []string, loads []uint64, now time.Time) *affinityShardTable {
	if len(addresses) == 0 || len(addresses) > 1024 {
		return nil
	}
	table := &affinityShardTable{Scope: r.scope, App: r.app, Version: now.UnixMilli(), Updated: now.UnixMilli(), Nodes: addresses, Owners: make([]uint16, affinityShardCount)}
	if previous == nil {
		for bucket := range table.Owners {
			table.Owners[bucket] = uint16(affinityShardOwner(r.scope, bucket, addresses))
		}
		return table
	}
	table.Version = max(table.Version, previous.Version+1)
	table.Moved = previous.Moved
	table.Balancing = previous.Balancing
	oldNodes := map[string]bool{}
	for _, node := range previous.Nodes {
		oldNodes[node] = true
	}
	for _, node := range addresses {
		if !oldNodes[node] {
			table.Balancing = true
		}
	}
	counts := make([]int, len(addresses))
	indices := map[string]int{}
	for i, address := range addresses {
		indices[address] = i
	}
	changed := false
	for bucket, owner := range previous.Owners {
		index, ok := indices[previous.Nodes[owner]]
		if !ok {
			index = affinityShardOwner(r.scope, bucket, addresses)
			table.Moved++
			changed = true
		}
		table.Owners[bucket] = uint16(index)
		counts[index]++
	}
	// Mandatory failure reassignment is not rate limited. Redis fencing still
	// serializes it; forwarding can already use the deterministic local fallback.
	if changed {
		return table
	}
	// A topology change produces one complete low-load-first allocation. There
	// is no timer or per-batch quota delaying the next request's new owner.
	if !table.Balancing || loads == nil {
		return nil
	}
	buckets := make([]int, affinityShardCount)
	for i := range buckets {
		buckets[i] = i
	}
	sort.Slice(buckets, func(i, j int) bool {
		a, b := buckets[i], buckets[j]
		if loads[a] == loads[b] {
			return a < b
		}
		return loads[a] < loads[b]
	})
	target := (affinityShardCount + len(addresses) - 1) / len(addresses)
	moved := 0
	for _, bucket := range buckets {
		donor := int(table.Owners[bucket])
		if counts[donor] <= target {
			continue
		}
		recipient := 0
		for i := range counts {
			if counts[i] < counts[recipient] {
				recipient = i
			}
		}
		if counts[recipient] >= affinityShardCount/len(addresses) {
			break
		}
		table.Owners[bucket] = uint16(recipient)
		counts[donor]--
		counts[recipient]++
		moved++
	}
	table.Moved += int64(moved)
	table.Balancing = false
	return table
}

// syncAffinityShards runs only on the existing background/control-plane path.
// The bounded authoritative tables live in Redis; per-session maps still don't.
func (m *Manager) syncAffinityShards(ctx context.Context, connection redisDriver.Conn) error {
	registry := m.affinity.shardRegistry.Load()
	if registry == nil || len(registry.tables) == 0 {
		return nil
	}
	now := time.Now()
	if now.Sub(m.affinity.shardSynced) < time.Second {
		return nil
	}
	m.affinity.shardSynced = now
	if err := ctx.Err(); err != nil {
		return err
	}
	leaseKey := m.config.Prefix + ":affinity-shard-master"
	leader, err := redisDriver.String(connection.Do("EVAL", affinityShardLease, 1, leaseKey, m.config.NodeID))
	if err != nil {
		return err
	}
	m.affinity.shardLeader.Store(leader)
	peers := m.Nodes()
	for scope, r := range registry.tables {
		addresses := affinityShardAddresses(r.app)
		report := r.sample(m.config.NodeID, addresses, now)
		if err := m.affinity.applyShardLoad(&report); err != nil {
			return err
		}
		data, err := cast.ToJSONBytes([]affinityEvent{{Load: &report}})
		if err != nil {
			return err
		}
		reportRaw, err := cast.ToJSONBytes(report)
		if err != nil {
			return err
		}
		if _, err = connection.Do("EVAL", affinityShardLoadPut, 2, m.config.Prefix+":affinity-shard-load:"+scope+":"+m.config.NodeID, m.affinityChannel(), reportRaw, data); err != nil {
			return err
		}
		key := m.config.Prefix + ":affinity-shard:" + scope
		raw, err := redisDriver.String(connection.Do("GET", key))
		if err == redisDriver.ErrNil {
			raw = ""
		} else if err != nil {
			return err
		}
		var authoritative *affinityShardTable
		if raw != "" {
			value, err := cast.FromJSON[affinityShardTable](raw)
			if err != nil {
				return err
			}
			authoritative = &value
			if err = m.affinity.applyShardTable(authoritative); err != nil {
				return err
			}
		}
		if leader != m.config.NodeID {
			continue
		}
		if _, err = connection.Do("EXPIRE", key, 3600); err != nil {
			return err
		}
		loads, complete := r.aggregate(peers, addresses, now)
		if !complete {
			if err = r.recoverLoads(connection, m.config.Prefix, peers, now); err != nil {
				return err
			}
			loads, complete = r.aggregate(peers, addresses, now)
		}
		// A missing reporter is never treated as zero traffic. Keep the table until
		// all ready Gateway views agree, including on backend membership.
		if authoritative != nil && !complete {
			continue
		}
		if authoritative == nil {
			// Existing local/peer checkpoints recover bounded tables after Redis loss.
			if retained := r.table.Load(); retained != nil {
				copy := *retained
				copy.Version = max(now.UnixMilli(), retained.Version+1)
				copy.Updated = now.UnixMilli()
				authoritative = &copy
			}
		}
		var next *affinityShardTable
		if raw == "" && authoritative != nil {
			next = authoritative
		} else {
			next = r.plan(authoritative, addresses, loads, now)
		}
		if next == nil {
			continue
		}
		nextRaw, err := cast.ToJSON(next)
		if err != nil {
			return err
		}
		event, err := cast.ToJSONBytes([]affinityEvent{{Table: next}})
		if err != nil {
			return err
		}
		accepted, err := redisDriver.Int(connection.Do("EVAL", affinityShardCAS, 4, key, m.affinityJournal(), m.affinityChannel(), leaseKey, raw, nextRaw, event, m.config.NodeID))
		if err != nil {
			return err
		}
		if accepted == 1 {
			if err = m.affinity.applyShardTable(next); err != nil {
				return err
			}
		}
	}
	return nil
}
