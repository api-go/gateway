package gateway

import (
	gohttp "apigo.cc/go/http"
	"apigo.cc/go/service"
	"fmt"
	"net"
	"net/url"
	"regexp"
	"strings"
	"time"
)

// Config configures the gateway and its optional separate management listener.
type Config struct {
	ExternalAddress      gohttp.ExternalAddressPolicy // Business ingress only; empty trusts no proxies.
	AdminExternalAddress gohttp.ExternalAddressPolicy // Independent management-listener trust.
	MaxAffinityEntries   int                          // Default 1,000,000; automatic maps never silently evict live sessions.
	AffinityAdvertise    string                       // Optional reachable management base URL for peer snapshots.
	Redis                string
	Prefix               string
	DataDir              string
	NodeID               string
	AdminListen          string
	AdminDisabled        bool // Management is enabled by default; true closes its listener.
	ClientIP             service.ClientIPPolicy
	ACMEEmail            string
	ACMEDirectory        string // Empty selects Let's Encrypt production; tests should use staging.
	ACMEAcceptTerms      bool
	CertificateKey       string // Optional explicit encryption key; automatic persistent management is the default.
	HTTPSPort            int    // Public HTTPS redirect port; zero derives it from the TLS listener.
	MaxRateKeys          int
	LogRetentionDays     int // 0 defaults to 30; application dated log files only.
}

var GatewayConf = Config{Prefix: "gateway", DataDir: "data/gateway", ClientIP: service.ClientIPPolicy{Mode: "direct"}, MaxRateKeys: 100000}

type AffinityConfig struct {
	Mode         string // off (default), auto, hash, manual; redis is a legacy alias of auto
	Source       string // header, query, cookie, session, ip
	Name         string
	TTLSeconds   int    // Local mapping lifetime; default 3600, maximum 604800.
	Missing      string // balance (default), reject
	Transform    string // full (default), last, lastDigit
	HashStrategy string // direct (default) or gradual (shared 4096-shard table)
	Mappings     []AffinityMapping
}

type AffinityMapping struct {
	Values []string
	Nodes  []string // Ordered primary / backup discovery addresses.
}
type AffinityRule struct {
	ID       string
	Path     string   // Incoming path: exact or trailing /* prefix.
	Methods  []string // Empty matches all methods.
	Affinity AffinityConfig
}

type Host struct {
	AffinityRules []AffinityRule
	Affinity      AffinityConfig
	Monitor       MonitorConfig
	ID            string
	Name          string
	Domains       []string
	Enabled       bool
	HTTPS         HTTPSConfig
	Proxies       []service.ProxyRule
	Rewrites      []service.RewriteRule
	Statics       map[string]string
	Limits        []LimitRule
}
type HTTPSConfig struct {
	Mode        string // adaptive (default), off, upload (legacy), auto (legacy ACME)
	Redirect    string // adaptive: auto (default) or off
	Force       bool
	Certificate string // Public PEM
	SealedKey   string // AES-GCM encrypted private PEM; never returned by management API.
}
type MatchCondition struct {
	Source   string // path, ip, ua, method, header, cookie, session, host
	Name     string // Required for header/cookie; optional Session cookie/header name.
	Operator string // equals, prefix, contains, regex, cidr, exists
	Value    string
}
type GroupDimension struct {
	Source string
	Name   string
}
type LimitRule struct {
	ID            string
	Name          string
	Enabled       bool
	Conditions    []MatchCondition // All conditions must match.
	GroupBy       []GroupDimension // Composite grouping key, not independent limits.
	PeriodSeconds int
	Requests      int
	Scope         string // node or cluster
	Mode          string // enforce or observe
	Missing       string // shared (default), ip, reject
}
type HostRecord struct {
	CreatedAt       time.Time
	DomainTLS       []DomainTLSStatus
	CertificateInfo CertificateInfo
	Revision        int64
	Draft           Host
	Current         *Host
	Version         int64
	History         []HostVersion
}
type HostVersion struct {
	Version int64
	Host    Host
	Time    time.Time
	Actor   string
}
type Event struct {
	Time    time.Time
	Type    string
	HostID  string
	RuleID  string
	Action  string
	Message string
	Actor   string
	Version int64
}
type State struct {
	AlertChannels map[string]AlertChannel
	APIKeys       map[string]APIKey
	Format        int
	Revision      int64
	Certificates  map[string]CertificateRecord
	ACME          *ACMESettings
	Hosts         map[string]HostRecord
	Events        []Event
}
type NodeStatus struct {
	ID              string
	AffinityURL     string
	AffinityReady   bool
	AffinityEntries int
	Revision        int64
	Versions        map[string]int64
	Error           string
	LastSeen        time.Time
}

func validDomain(domain string) bool {
	if domain == "*" {
		return true
	}
	if strings.HasPrefix(domain, ":") {
		port := strings.TrimPrefix(domain, ":")
		return port != "" && strings.Trim(port, "0123456789") == ""
	}
	value := strings.TrimPrefix(domain, "*.")
	if net.ParseIP(value) != nil {
		return true
	}
	if len(value) > 253 || strings.ContainsAny(value, "/:@ \t\n\r") {
		return false
	}
	for _, label := range strings.Split(value, ".") {
		if len(label) == 0 || len(label) > 63 || label[0] == '-' || label[len(label)-1] == '-' {
			return false
		}
		for _, char := range label {
			if !(char >= 'a' && char <= 'z' || char >= '0' && char <= '9' || char == '-') {
				return false
			}
		}
	}
	return true
}
func validateHost(host Host) error {
	if err := validateAffinityRules(host.AffinityRules); err != nil {
		return err
	}
	if err := validateAffinity(host.Affinity); err != nil {
		return err
	}
	if err := validateMonitor(host.Monitor); err != nil {
		return err
	}
	if host.ID == "" || len(host.ID) > 80 || strings.ContainsAny(host.ID, "/:* \t\r\n") {
		return fmt.Errorf("invalid Host ID")
	}
	if len(host.Domains) == 0 || len(host.Domains) > 50 {
		return fmt.Errorf("Host requires 1–50 domains")
	}
	seen := map[string]bool{}
	for _, domain := range host.Domains {
		if domain != strings.ToLower(strings.TrimSpace(domain)) || !validDomain(domain) || seen[domain] {
			return fmt.Errorf("invalid or duplicate domain %q", domain)
		}
		seen[domain] = true
	}
	if len(host.Proxies)+len(host.Rewrites)+len(host.Statics) > 1000 || len(host.Limits) > 100 {
		return fmt.Errorf("too many rules")
	}
	if host.HTTPS.Mode == "" {
		host.HTTPS.Mode = "adaptive"
	}
	if host.HTTPS.Mode != "adaptive" && host.HTTPS.Mode != "off" && host.HTTPS.Mode != "upload" && host.HTTPS.Mode != "auto" {
		return fmt.Errorf("invalid HTTPS mode")
	}
	if host.HTTPS.Redirect != "" && host.HTTPS.Redirect != "auto" && host.HTTPS.Redirect != "off" {
		return fmt.Errorf("invalid HTTPS redirect mode")
	}
	if host.HTTPS.Force && host.HTTPS.Mode == "off" {
		return fmt.Errorf("HTTPS redirect requires a certificate mode")
	}
	if host.HTTPS.Mode == "auto" {
		for _, domain := range host.Domains {
			if strings.Contains(domain, "*") || strings.HasPrefix(domain, ":") || net.ParseIP(domain) != nil || !strings.Contains(domain, ".") {
				return fmt.Errorf("HTTP ACME requires explicit public DNS names")
			}
		}
	}
	_, err := service.PrepareHostPolicy(service.HostPolicy{Proxies: host.Proxies, Rewrites: host.Rewrites, Statics: host.Statics})
	if err != nil {
		return err
	}
	ids := map[string]bool{}
	for _, rule := range host.Limits {
		if ids[rule.ID] {
			return fmt.Errorf("duplicate limit ID")
		}
		ids[rule.ID] = true
		if _, err := compileLimit(rule); err != nil {
			return err
		}
	}
	return nil
}
func validateSource(source, name string) error {
	switch source {
	case "host", "path", "ip", "ua", "method", "session":
	case "header", "cookie":
		if name == "" || len(name) > 128 || strings.ContainsAny(name, "\r\n :") {
			return fmt.Errorf("invalid header/cookie name")
		}
	default:
		return fmt.Errorf("unsupported source %q", source)
	}
	return nil
}
func validateCondition(condition MatchCondition) (*regexp.Regexp, error) {
	if err := validateSource(condition.Source, condition.Name); err != nil {
		return nil, err
	}
	if len(condition.Value) > 4096 {
		return nil, fmt.Errorf("condition too long")
	}
	switch condition.Operator {
	case "equals", "prefix", "contains", "exists":
		return nil, nil
	case "regex":
		return regexp.Compile(condition.Value)
	case "cidr":
		if condition.Source != "ip" {
			return nil, fmt.Errorf("CIDR only applies to IP")
		}
		_, _, err := net.ParseCIDR(condition.Value)
		return nil, err
	default:
		return nil, fmt.Errorf("unsupported operator %q", condition.Operator)
	}
}

// ValidateHost performs structural validation without publishing or accessing an upstream.
func ValidateHost(host Host) error { return validateHost(host) }
func normalizedHost(host string) string {
	if value, _, err := net.SplitHostPort(host); err == nil {
		return strings.ToLower(value)
	}
	return strings.ToLower(strings.TrimSuffix(host, "."))
}
func validPreviewURL(path string) bool {
	value, err := url.ParseRequestURI(path)
	return err == nil && strings.HasPrefix(path, "/") && !strings.HasPrefix(path, "//") && value.Host == ""
}

// CertificateInfo contains public status only. Auto mode remains pending until the first HTTPS handshake.
type CertificateInfo struct {
	State   string
	Issuer  string
	Domains []string
	Expires string
}
