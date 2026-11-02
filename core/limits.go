package gateway

import (
	"apigo.cc/go/cast"
	goCrypto "apigo.cc/go/crypto"
	"apigo.cc/go/redis"
	"apigo.cc/go/service"
	"fmt"
	"net"
	"net/http"
	"regexp"
	"strings"
	"sync"
	"time"
)

type compiledLimit struct {
	rule        LimitRule
	matchers    []*regexp.Regexp
	networks    []*net.IPNet
	fingerprint string
}

func compileLimit(rule LimitRule) (*compiledLimit, error) {
	if rule.ID == "" || len(rule.ID) > 80 || rule.PeriodSeconds < 1 || rule.PeriodSeconds > 86400 || rule.Requests < 1 || rule.Requests > 1000000000 {
		return nil, fmt.Errorf("limit requires ID, 1–86400 seconds and a positive request budget")
	}
	if rule.Scope == "" {
		rule.Scope = "node"
	}
	if rule.Mode == "" {
		rule.Mode = "enforce"
	}
	if rule.Missing == "" {
		rule.Missing = "shared"
	}
	if rule.Scope != "node" && rule.Scope != "cluster" || rule.Mode != "enforce" && rule.Mode != "observe" || rule.Missing != "shared" && rule.Missing != "ip" && rule.Missing != "reject" {
		return nil, fmt.Errorf("invalid limit scope, mode or missing-value behavior")
	}
	if len(rule.GroupBy) > 8 || len(rule.Conditions) > 20 {
		return nil, fmt.Errorf("too many grouping dimensions or conditions")
	}
	compiled := &compiledLimit{rule: rule}
	for _, dimension := range rule.GroupBy {
		if err := validateSource(dimension.Source, dimension.Name); err != nil {
			return nil, err
		}
	}
	for _, condition := range rule.Conditions {
		matcher, err := validateCondition(condition)
		if err != nil {
			return nil, err
		}
		compiled.matchers = append(compiled.matchers, matcher)
		var network *net.IPNet
		if condition.Operator == "cidr" {
			_, network, _ = net.ParseCIDR(condition.Value)
		}
		compiled.networks = append(compiled.networks, network)
	}
	encoded, _ := cast.ToJSONBytes(rule)
	compiled.fingerprint = goCrypto.SHA256ToHex(encoded)
	return compiled, nil
}
func requestValue(request *service.Request, source, name string) string {
	switch source {
	case "host":
		return normalizedHost(request.Host)
	case "path":
		return request.URL.Path
	case "ip":
		return request.ClientIp()
	case "ua":
		return request.UserAgent()
	case "method":
		return request.Method
	case "header":
		return request.Request.Header.Get(name)
	case "cookie":
		cookie, err := request.Request.Cookie(name)
		if err == nil {
			return cookie.Value
		}
	case "session":
		if name == "" {
			name = "Session-ID"
		}
		// Use incoming client identity rather than a newly generated per-request session.
		if value := request.Request.Header.Get(name); value != "" {
			return value
		}
		cookie, err := request.Request.Cookie(name)
		if err == nil {
			return cookie.Value
		}
	}
	return ""
}
func (c *compiledLimit) matches(request *service.Request) bool {
	for index, condition := range c.rule.Conditions {
		value := requestValue(request, condition.Source, condition.Name)
		matched := false
		switch condition.Operator {
		case "equals":
			matched = value == condition.Value
		case "directory":
			prefix := strings.TrimSuffix(condition.Value, "/")
			matched = value == prefix || strings.HasPrefix(value, prefix+"/")
		case "prefix":
			matched = strings.HasPrefix(value, condition.Value)
		case "contains":
			matched = strings.Contains(value, condition.Value)
		case "exists":
			matched = value != ""
		case "regex":
			matched = c.matchers[index].MatchString(value)
		case "cidr":
			matched = c.networks[index].Contains(net.ParseIP(value))
		}
		if !matched {
			return false
		}
	}
	return true
}
func (c *compiledLimit) key(hostID string, request *service.Request) (string, bool) {
	values := []string{hostID, c.fingerprint}
	for _, dimension := range c.rule.GroupBy {
		value := requestValue(request, dimension.Source, dimension.Name)
		if len(value) > 4096 {
			return "", false
		}
		if value == "" {
			switch c.rule.Missing {
			case "reject":
				return "", false
			case "ip":
				value = "ip:" + request.ClientIp()
			default:
				value = "missing:"
			}
		} else {
			value = "value:" + value
		}
		values = append(values, value)
	}
	encoded, _ := cast.ToJSONBytes(values)
	return goCrypto.SHA256ToHex(encoded), true
}

type rateCounter struct {
	window  int64
	count   int
	expires int64
}

// Limiter enforces first-request anchored fixed windows, with bounded local key cardinality.
// Cluster mode uses a Redis atomic counter and server-side TTL. No request body is consumed.
type Limiter struct {
	mu       sync.Mutex
	counters map[string]rateCounter
	maxKeys  int
	rd       *redis.Redis
	prefix   string
}

func NewLimiter(maxKeys int, rd *redis.Redis, prefix string) *Limiter {
	if maxKeys <= 0 {
		maxKeys = 100000
	}
	return &Limiter{counters: map[string]rateCounter{}, maxKeys: maxKeys, rd: rd, prefix: prefix}
}

type LimitDecision struct {
	RuleID     string
	Allowed    bool
	Observed   bool
	RetryAfter int
	Error      string
}

const rateScript = `local n=redis.call('INCR',KEYS[1]); if n==1 then redis.call('PEXPIRE',KEYS[1],ARGV[1]) end; return {n,redis.call('PTTL',KEYS[1])}`

func (l *Limiter) check(rule *compiledLimit, key string, now time.Time) (int, int, error) {
	period := int64(rule.rule.PeriodSeconds) * 1000
	if rule.rule.Scope == "cluster" {
		if l.rd == nil {
			return 0, 0, fmt.Errorf("cluster limit requires Redis")
		}
		result := l.rd.Do("EVAL", rateScript, 1, l.prefix+":limit:"+key, period)
		if result.Error != nil {
			return 0, 0, result.Error
		}
		values := result.Ints()
		if len(values) != 2 || values[1] < 0 {
			return 0, 0, fmt.Errorf("invalid Redis limit response")
		}
		return values[0], (values[1] + 999) / 1000, nil
	}
	l.mu.Lock()
	defer l.mu.Unlock()
	milliseconds := now.UnixMilli()
	counter, exists := l.counters[key]
	if !exists && len(l.counters) >= l.maxKeys {
		for name, value := range l.counters {
			if value.expires <= milliseconds {
				delete(l.counters, name)
			}
		}
		if len(l.counters) >= l.maxKeys {
			return 0, 0, fmt.Errorf("rate counter capacity exhausted")
		}
	}
	if !exists || counter.expires <= milliseconds {
		counter = rateCounter{window: milliseconds, expires: milliseconds + period}
	}
	if counter.count <= rule.rule.Requests {
		counter.count++
	}
	l.counters[key] = counter
	return counter.count, int((counter.expires - milliseconds + 999) / 1000), nil
}
func (l *Limiter) Evaluate(hostID string, rules []LimitRule, request *service.Request, now time.Time) ([]LimitDecision, error) {
	compiled := make([]*compiledLimit, 0, len(rules))
	for _, rule := range rules {
		item, err := compileLimit(rule)
		if err != nil {
			return nil, err
		}
		compiled = append(compiled, item)
	}
	return l.evaluate(hostID, compiled, request, now), nil
}
func (l *Limiter) evaluate(hostID string, rules []*compiledLimit, request *service.Request, now time.Time) []LimitDecision {
	decisions := make([]LimitDecision, 0, len(rules))
	for _, rule := range rules {
		if !rule.rule.Enabled || !rule.matches(request) {
			continue
		}
		decision := LimitDecision{RuleID: rule.rule.ID, Allowed: true, Observed: rule.rule.Mode == "observe"}
		key, ok := rule.key(hostID, request)
		if !ok {
			decision.Allowed = false
			decision.Error = "required grouping value missing"
		} else {
			count, retry, err := l.check(rule, key, now)
			decision.RetryAfter = retry
			if err != nil {
				decision.Allowed = false
				decision.Error = "rate counter unavailable"
			} else {
				decision.Allowed = count <= rule.rule.Requests
			}
		}
		decisions = append(decisions, decision)
	}
	return decisions
}
func (l *Limiter) Sweep(now time.Time) {
	l.mu.Lock()
	defer l.mu.Unlock()
	for key, counter := range l.counters {
		if counter.expires <= now.UnixMilli() {
			delete(l.counters, key)
		}
	}
}
func writeLimitResponse(response *service.Response, decision LimitDecision) {
	if decision.Error == "required grouping value missing" {
		response.WriteHeader(http.StatusBadRequest)
		response.WriteString("required request grouping value missing")
		return
	}
	if decision.Error != "" {
		response.WriteHeader(http.StatusServiceUnavailable)
		response.WriteString("request quota unavailable")
		return
	}
	response.Header().Set("Retry-After", cast.String(max(1, decision.RetryAfter)))
	response.WriteHeader(http.StatusTooManyRequests)
	response.WriteString("request quota exceeded")
}

// Summaries describe the current configuration; identity remains the stable ID.
func limitSummary(rule LimitRule) string {
	scope := "全部请求"
	for _, condition := range rule.Conditions {
		if condition.Source == "path" {
			scope = condition.Value
			break
		}
	}
	group := "合计"
	if len(rule.GroupBy) > 0 {
		group = "每个身份"
		if len(rule.GroupBy) == 1 && rule.GroupBy[0].Source == "ip" {
			group = "每个 IP"
		}
	}
	return fmt.Sprintf("%s · %s · %d 次 / %d 秒", scope, group, rule.Requests, rule.PeriodSeconds)
}
