package gateway

import (
	"apigo.cc/go/cast"
	"fmt"
	"net"
	"sort"
	"time"

	"apigo.cc/go/js"
	"apigo.cc/go/service"
)

// ACLRule protects Scope (AND). Inside that scope at least one Allow grant
// must match; otherwise access is denied. All overlapping scopes must pass.
// Empty Scope covers the entire Host; empty Allow denies the entire scope.
type ACLRule struct {
	ID      string
	Enabled bool
	Scope   []MatchCondition
	Allow   []ACLGrant // OR; conditions within each grant are ANDed.
	Mode    string     // enforce (default) or observe

	// Compatibility with the first unpublished ordered-rule development build.
	Name       string
	Conditions []MatchCondition
	Action     string
	Script     string
	TimeoutMS  int
}

// ACLGrant is one independent way to enter a protected scope. Script is an
// optional additional predicate; errors never grant access.
type ACLGrant struct {
	Conditions []MatchCondition
	Script     string
	TimeoutMS  int // default 10; 1–100
}

type ACLDecision struct {
	RuleID   string
	Allowed  bool
	Observed bool
	Error    string
}

// ACL is an immutable, concurrency-safe compiled access policy. Construct once
// per configuration revision and reuse across requests. No match allows access.
type ACL struct{ rules compiledACL }

// NewACL validates and compiles rules without contacting upstreams.
func NewACL(rules []ACLRule) (*ACL, error) {
	// Own condition slices so caller changes cannot affect the published matcher.
	owned := make([]ACLRule, len(rules))
	for i, rule := range rules {
		owned[i] = rule
		owned[i].Conditions = append([]MatchCondition(nil), rule.Conditions...)
		owned[i].Scope = append([]MatchCondition(nil), rule.Scope...)
		owned[i].Allow = make([]ACLGrant, len(rule.Allow))
		for j, grant := range rule.Allow {
			owned[i].Allow[j] = grant
			owned[i].Allow[j].Conditions = append([]MatchCondition(nil), grant.Conditions...)
		}
	}
	compiled, err := compileACL(owned)
	if err != nil {
		return nil, err
	}
	return &ACL{rules: compiled}, nil
}

// Evaluate returns the enforced decision; observe rules never reject traffic.
// Evaluation does not consume the request body or modify request headers.
func (acl *ACL) Evaluate(request *service.Request) ACLDecision {
	return acl.rules.evaluate(request, nil)
}

type compiledACLRule struct {
	rule    ACLRule
	matcher *compiledLimit
	pool    *js.Pool
	grants  []compiledACLGrant
}
type compiledACL []compiledACLRule

type compiledACLGrant struct {
	matcher   *compiledLimit
	pool      *js.Pool
	timeoutMS int
}

func aclMatcher(id string, conditions []MatchCondition) (*compiledLimit, error) {
	if id == "" {
		id = "acl"
	}
	normalized := append([]MatchCondition(nil), conditions...)
	for index, condition := range normalized {
		if condition.Source == "ip" && condition.Operator == "equals" {
			ip := net.ParseIP(condition.Value)
			if ip == nil {
				return nil, fmt.Errorf("invalid ACL IP address")
			}
			normalized[index].Value = ip.String()
		}
	}
	return compileLimit(LimitRule{ID: id, Conditions: normalized, Requests: 1, PeriodSeconds: 1})
}
func legacyACL(rule ACLRule) bool {
	return rule.Action != "" || len(rule.Conditions) > 0 || rule.Script != "" || rule.TimeoutMS != 0
}
func validateACL(rules []ACLRule) error {
	if len(rules) > 100 {
		return fmt.Errorf("ACL supports at most 100 scopes")
	}
	ids := map[string]bool{}
	for _, rule := range rules {
		if len(rule.ID) > 80 || rule.ID != "" && ids[rule.ID] {
			return fmt.Errorf("ACL IDs must be unique and at most 80 bytes")
		}
		ids[rule.ID] = true
		if rule.Mode != "" && rule.Mode != "enforce" && rule.Mode != "observe" {
			return fmt.Errorf("invalid ACL mode")
		}
		if legacyACL(rule) {
			if len(rule.Scope) > 0 || len(rule.Allow) > 0 {
				return fmt.Errorf("cannot mix legacy ACL actions with scope/allow")
			}
			if rule.Action != "allow" && rule.Action != "deny" {
				return fmt.Errorf("invalid legacy ACL action")
			}
			if _, err := aclMatcher(rule.ID, rule.Conditions); err != nil {
				return err
			}
			if err := validateACLGrant(ACLGrant{Script: rule.Script, TimeoutMS: rule.TimeoutMS}, true); err != nil {
				return err
			}
			continue
		}
		if _, err := aclMatcher(rule.ID, rule.Scope); err != nil {
			return err
		}
		if len(rule.Allow) > 100 {
			return fmt.Errorf("ACL supports at most 100 allow alternatives per scope")
		}
		for _, grant := range rule.Allow {
			if err := validateACLGrant(grant, false); err != nil {
				return err
			}
		}
	}
	return nil
}
func validateACLGrant(grant ACLGrant, legacy bool) error {
	if !legacy && len(grant.Conditions) == 0 && grant.Script == "" {
		return fmt.Errorf("ACL allow alternative requires conditions or script")
	}
	if grant.TimeoutMS < 0 || grant.TimeoutMS > 100 {
		return fmt.Errorf("ACL timeout must be 1–100 ms or zero for default")
	}
	if _, err := aclMatcher("acl", grant.Conditions); err != nil {
		return err
	}
	return validateCode(grant.Script)
}
func compileACL(rules []ACLRule) (compiledACL, error) {
	if err := validateACL(rules); err != nil {
		return nil, err
	}
	result := make(compiledACL, 0, len(rules))
	for _, rule := range rules {
		if !rule.Enabled {
			continue
		}
		conditions := rule.Scope
		if legacyACL(rule) {
			conditions = rule.Conditions
		}
		matcher, err := aclMatcher(rule.ID, conditions)
		if err != nil {
			return nil, err
		}
		item := compiledACLRule{rule: rule, matcher: matcher}
		if legacyACL(rule) {
			if rule.Script != "" {
				item.pool = js.NewPool()
				if err := item.pool.Define("acl", rule.Script, 1); err != nil {
					return nil, err
				}
			}
		} else {
			for _, grant := range rule.Allow {
				matcher, err := aclMatcher(rule.ID, grant.Conditions)
				if err != nil {
					return nil, err
				}
				compiled := compiledACLGrant{matcher: matcher, timeoutMS: grant.TimeoutMS}
				if grant.Script != "" {
					compiled.pool = js.NewPool()
					if err := compiled.pool.Define("acl", grant.Script, 1); err != nil {
						return nil, err
					}
				}
				item.grants = append(item.grants, compiled)
			}
		}
		// OR alternatives have no priority: try native checks before optional JS.
		sort.SliceStable(item.grants, func(i, j int) bool { return item.grants[i].pool == nil && item.grants[j].pool != nil })
		result = append(result, item)
	}
	return result, nil
}
func aclScript(request *service.Request, pool *js.Pool, timeoutMS int) (bool, string) {
	if pool == nil {
		return true, ""
	}
	if timeoutMS == 0 {
		timeoutMS = 10
	}
	input := map[string]any{"host": normalizedHost(request.Host), "path": request.URL.Path, "method": request.Method, "ip": request.ClientIp(), "ua": request.UserAgent(), "headers": request.Request.Header.Clone(), "session": requestValue(request, "session", "")}
	cookies := map[string]string{}
	for _, cookie := range request.Request.Cookies() {
		cookies[cookie.Name] = cookie.Value
	}
	input["cookies"] = cookies
	value, err := pool.CallContext(request.Context(), "acl", time.Duration(timeoutMS)*time.Millisecond, map[string]any{"SafeMode": true}, input)
	matched, ok := value.(bool)
	if err != nil || !ok {
		return false, "ACL script failed"
	}
	return matched, ""
}
func (rules compiledACL) evaluate(request *service.Request, observe func(ACLDecision)) ACLDecision {
	result := ACLDecision{Allowed: true}
	// Scope policies always run before legacy policies so a legacy allow cannot
	// bypass an overlapping protected scope.
	for _, item := range rules {
		if legacyACL(item.rule) || !item.matcher.matches(request) {
			continue
		}
		decision := ACLDecision{RuleID: item.rule.ID, Observed: item.rule.Mode == "observe"}
		for _, grant := range item.grants {
			if !grant.matcher.matches(request) {
				continue
			}
			matched, reason := aclScript(request, grant.pool, grant.timeoutMS)
			if matched {
				decision.Allowed = true
				decision.Error = ""
				break
			}
			if reason != "" {
				decision.Error = reason
			}
		}
		if decision.Observed {
			if observe != nil {
				observe(decision)
			}
			continue
		}
		if !decision.Allowed {
			return decision
		}
		result = decision
	}
	for _, item := range rules {
		if !legacyACL(item.rule) || !item.matcher.matches(request) {
			continue
		}
		decision := ACLDecision{RuleID: item.rule.ID, Allowed: item.rule.Action == "allow", Observed: item.rule.Mode == "observe"}
		matched, reason := aclScript(request, item.pool, item.rule.TimeoutMS)
		if reason != "" {
			decision.Allowed = false
			decision.Error = reason
		} else if !matched {
			continue
		}
		if decision.Observed {
			if observe != nil {
				observe(decision)
			}
			continue
		}
		return decision
	}

	return result
}

// Keep executable policy under the same administrator boundary as monitor code.
func sameACLScripts(left, right []ACLRule) bool {
	scripts := func(rules []ACLRule) map[string]string {
		result := map[string]string{}
		for _, rule := range rules {
			codes := []string{}
			if rule.Script != "" {
				codes = append(codes, rule.Script)
			}
			for _, grant := range rule.Allow {
				if grant.Script != "" {
					codes = append(codes, grant.Script)
				}
			}
			if len(codes) > 0 {
				sort.Strings(codes)
				encoded, _ := cast.ToJSON(codes)
				result[rule.ID] = encoded
			}
		}
		return result
	}
	a, b := scripts(left), scripts(right)
	if len(a) != len(b) {
		return false
	}
	for id, code := range a {
		if b[id] != code {
			return false
		}
	}
	return true
}
