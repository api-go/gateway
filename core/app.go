package gateway

import (
	"github.com/api-go/cast"
	"github.com/api-go/config"
	"github.com/api-go/discover"
	"github.com/api-go/log"
	"github.com/api-go/redis"
	"github.com/api-go/service"
	"github.com/api-go/task"
	"context"
	"fmt"
	redisDriver "github.com/gomodule/redigo/redis"
	"net"
	"net/http"
	"sort"
	"strings"
	"sync"
	"time"
)

// GatewayApp implements starter lifecycle and hosts the optional management server.
type GatewayApp struct {
	manager       *Manager
	admin         *http.Server
	adminListener net.Listener
	scheduler     *task.Scheduler
	legacyMu      sync.Mutex
	legacy        map[string]*service.PreparedHostPolicy
}

func NewGatewayApp() *GatewayApp        { return &GatewayApp{} }
func (g *GatewayApp) Manager() *Manager { return g.manager }
func (g *GatewayApp) Start(ctx context.Context, logger *log.Logger) error {
	if logger == nil {
		logger = log.DefaultLogger
	}
	manager, err := NewManager(GatewayConf, service.DefaultServer)
	if err != nil {
		return err
	}
	g.manager = manager
	if !manager.config.AdminDisabled {
		listener, err := net.Listen("tcp", manager.config.AdminListen)
		if err != nil {
			manager.Close()
			return err
		}
		g.adminListener = listener
		g.admin = &http.Server{Handler: manager.AdminHandler(), ReadHeaderTimeout: 5 * time.Second, ReadTimeout: 15 * time.Second, WriteTimeout: 30 * time.Second, IdleTimeout: 60 * time.Second, MaxHeaderBytes: 32 << 10}
		go func() {
			if err := g.admin.Serve(listener); err != nil && err != http.ErrServerClosed {
				logger.Error("gateway administration stopped", "error", err.Error())
			}
		}()
	}
	var syncAddress net.Addr
	if g.adminListener != nil {
		syncAddress = g.adminListener.Addr()
	}
	if err := manager.startAffinitySync(syncAddress); err != nil {
		return err
	}
	if err := service.DefaultServer.Start(ctx, logger); err != nil {
		if g.admin != nil {
			g.admin.Close()
		}
		manager.Close()
		return err
	}
	// Gateway is an ingress and discovery client, not a business upstream.
	// Use Discover's lifecycle to remove Service's own registration while retaining client subscriptions.
	if discover.Config.Registry != "" {
		discover.Stop()
		if !discover.Start("") {
			logger.Warning("gateway discovery client subscriptions unavailable")
		}
	}
	manager.mu.Lock()
	manager.registryName = service.DefaultServer.Config.Register
	if manager.registryName == "" {
		manager.registryName = discover.Config.Registry
	}
	manager.captureRegistryLocked()
	manager.mu.Unlock()
	if manager.rd != nil {
		manager.rd.Subscribe(manager.config.Prefix+":manage", nil, func(data []byte) {
			if err := manager.Reconcile(); err != nil {
				logger.Error("gateway reconciliation failed", "error", err.Error())
			}
		})
		manager.rd.Subscribe(manager.config.Prefix+":channel", nil, func(data []byte) {
			host := strings.TrimSpace(strings.Trim(string(data), "\""))
			if host != "" {
				if err := g.loadLegacyHost(host); err != nil {
					logger.Error("legacy gateway configuration rejected", "host", host, "error", err.Error())
				}
			}
		})
		manager.rd.Start()
		if err := g.loadLegacy(); err != nil {
			logger.Warning("legacy gateway configuration rejected", "error", err.Error())
		}
	}
	g.scheduler = task.NewScheduler()
	g.scheduler.Add("gateway-reconcile", "@every 5s", func(ctx context.Context) error {
		if err := manager.Reconcile(); err != nil {
			return err
		}
		return g.loadLegacy()
	}, task.Config{Policy: task.PolicySkip, Timeout: 10 * time.Second, SuppressSuccessLog: true})
	g.scheduler.Add("gateway-monitor", "@every 5s", manager.MonitorTick, task.Config{Policy: task.PolicySkip, Timeout: 30 * time.Second, SuppressSuccessLog: true})
	g.scheduler.Add("gateway-log-retention", "@every 24h", func(ctx context.Context) error {
		if err := ctx.Err(); err != nil {
			return err
		}
		var conf log.Config
		if err := config.Load(&conf, "log"); err != nil {
			return err
		}
		return PruneGatewayLogs(conf, manager.config.LogRetentionDays, time.Now())
	}, task.Config{Policy: task.PolicySkip, Timeout: 30 * time.Second, SuppressSuccessLog: true})
	g.scheduler.Start(context.Background(), logger)
	g.scheduler.Run("gateway-log-retention")
	// Prime startup immediately; later ticks continue fairly in batches of four.
	g.scheduler.Run("gateway-monitor")
	if err := manager.Reconcile(); err != nil {
		logger.Warning("gateway started with retained configuration", "error", err.Error())
	}
	return nil
}
func (g *GatewayApp) AdminAddr() string {
	if g.adminListener == nil {
		return ""
	}
	return g.adminListener.Addr().String()
}
func (g *GatewayApp) Stop(ctx context.Context) error {
	var first error
	if g.scheduler != nil {
		first = g.scheduler.Stop(ctx)
	}
	if g.manager != nil {
		g.manager.stopAffinitySync()
	}
	if g.manager != nil && g.manager.rd != nil {
		g.manager.rd.Unsubscribe(g.manager.config.Prefix + ":manage")
		g.manager.rd.Unsubscribe(g.manager.config.Prefix + ":channel")
		g.manager.rd.Stop()
	}
	if g.admin != nil {
		if err := g.admin.Shutdown(ctx); err != nil && first == nil {
			first = err
		}
	}
	if err := service.DefaultServer.Stop(ctx); err != nil && first == nil {
		first = err
	}
	if g.manager != nil {
		g.manager.Close()
	}
	return first
}
func (g *GatewayApp) Reload() error {
	if err := service.DefaultServer.Reload(); err != nil {
		return err
	}
	if discover.Config.Registry != "" {
		discover.Stop()
		discover.Start("")
	}
	if g.manager == nil {
		return nil
	}
	g.manager.mu.Lock()
	g.manager.registryName = service.DefaultServer.Config.Register
	if g.manager.registryName == "" {
		g.manager.registryName = discover.Config.Registry
	}
	g.manager.captureRegistryLocked()
	g.manager.mu.Unlock()
	if err := g.manager.server.SetClientIPPolicy(g.manager.config.ClientIP); err != nil {
		return err
	}
	if err := g.manager.server.SetExternalAddressPolicy(g.manager.config.ExternalAddress); err != nil {
		return err
	}
	if err := g.manager.Reconcile(); err != nil {
		return err
	}
	return g.loadLegacy()
}
func (g *GatewayApp) Status() (string, error) {
	address, err := service.DefaultServer.Status()
	if err != nil {
		return "", err
	}
	count := 0
	if g.manager != nil {
		count = len(g.manager.Hosts())
	}
	return fmt.Sprintf("%s, managed_hosts: %d, admin: %s", address, count, g.AdminAddr()), nil
}
func scanKeys(rd *redis.Redis, pattern string) ([]string, error) {
	connection, err := rd.GetConnection()
	if err != nil {
		return nil, err
	}
	defer connection.Close()
	keys := []string{}
	cursor := "0"
	for count := 0; count < 1000; count++ {
		values, err := redisDriver.Values(connection.Do("SCAN", cursor, "MATCH", pattern, "COUNT", 500))
		if err != nil {
			return nil, err
		}
		if len(values) != 2 {
			return nil, fmt.Errorf("invalid SCAN reply")
		}
		cursor, err = redisDriver.String(values[0], nil)
		if err != nil {
			return nil, err
		}
		page, err := redisDriver.Strings(values[1], nil)
		if err != nil {
			return nil, err
		}
		keys = append(keys, page...)
		if cursor == "0" {
			return keys, nil
		}
	}
	return nil, fmt.Errorf("configuration key scan exceeded bound")
}
func (g *GatewayApp) loadLegacy() error {
	if g.manager == nil || g.manager.rd == nil {
		return nil
	}
	keys, err := scanKeys(g.manager.rd, g.manager.config.Prefix+":host:*")
	if err != nil {
		return err
	}
	found := map[string]bool{}
	for _, key := range keys {
		host := strings.TrimPrefix(key, g.manager.config.Prefix+":host:")
		found[host] = true
		if err := g.loadLegacyHost(host); err != nil {
			return err
		}
	}
	g.legacyMu.Lock()
	defer g.legacyMu.Unlock()
	// Remove disappeared legacy domains using the normal dynamic replacement APIs.
	for host := range g.legacy {
		if !found[host] {
			service.ReplaceProxies(host, nil)
			service.ReplaceRewrites(host, nil)
			service.ReplaceStatics(host, nil)
			delete(g.legacy, host)
		}
	}
	return nil
}
func decodeLegacyRules[T any](value string) ([]T, error) {
	if value == "" {
		return nil, nil
	}
	if strings.HasPrefix(strings.TrimSpace(value), "[") {
		return cast.FromJSON[[]T](value)
	}
	entries, err := cast.FromJSON[map[string]any](value)
	if err != nil {
		return nil, err
	}
	paths := make([]string, 0, len(entries))
	for path := range entries {
		paths = append(paths, path)
	}
	sortStrings(paths)
	rules := make([]T, 0, len(paths))
	for _, path := range paths {
		rule := map[string]any{"path": path}
		switch target := entries[path].(type) {
		case string:
			rule["to"] = target
		case map[string]any:
			for key, value := range target {
				rule[key] = value
			}
		default:
			return nil, fmt.Errorf("invalid legacy target")
		}
		data, _ := cast.ToJSONBytes(rule)
		typed, err := cast.FromJSON[T](data)
		if err != nil {
			return nil, err
		}
		rules = append(rules, typed)
	}
	return rules, nil
}
func sortStrings(values []string) {
	sort.Slice(values, func(i, j int) bool { return values[i] < values[j] })
}
func (g *GatewayApp) loadLegacyHost(host string) error {
	g.legacyMu.Lock()
	defer g.legacyMu.Unlock()
	if g.legacy == nil {
		g.legacy = map[string]*service.PreparedHostPolicy{}
	}
	result := g.manager.rd.Do("HGETALL", g.manager.config.Prefix+":host:"+host)
	if result.Error != nil {
		return result.Error
	}
	values := result.StringMap()
	proxies, err := decodeLegacyRules[service.ProxyRule](values["proxies"])
	if err != nil {
		return err
	}
	rewrites, err := decodeLegacyRules[service.RewriteRule](values["rewrites"])
	if err != nil {
		return err
	}
	var statics map[string]string
	if values["statics"] != "" {
		if err := cast.UnmarshalJSON(values["statics"], &statics); err != nil {
			return err
		}
	}
	prepared, err := service.PrepareHostPolicy(service.HostPolicy{Proxies: proxies, Rewrites: rewrites, Statics: statics})
	if err != nil {
		return err
	}
	service.ReplaceProxies(host, proxies)
	service.ReplaceRewrites(host, rewrites)
	service.ReplaceStatics(host, statics)
	g.legacy[host] = prepared
	return nil
}
