package gateway_test

import (
	"context"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
	"time"

	"apigo.cc/go/cast"
	gateway "apigo.cc/go/gateway/core"
	"apigo.cc/go/service"
)

func scopeACL() []gateway.ACLRule {
	return []gateway.ACLRule{{ID: "admin", Enabled: true, Scope: []gateway.MatchCondition{{Source: "path", Operator: "directory", Value: "/admin"}}, Allow: []gateway.ACLGrant{
		{Conditions: []gateway.MatchCondition{{Source: "ip", Operator: "cidr", Value: "192.0.2.0/24"}}},
		{Conditions: []gateway.MatchCondition{{Source: "ip", Operator: "equals", Value: "2001:0db8:0000:0000:0000:0000:0000:0001"}}},
	}}}
}
func TestACLScopeDefaultDenyAlternativesAndDirectory(t *testing.T) {
	acl, err := gateway.NewACL(scopeACL())
	if err != nil {
		t.Fatal(err)
	}
	for _, test := range []struct {
		path, ip string
		allow    bool
	}{
		{"/admin", "192.0.2.1:80", true}, {"/admin/users", "192.0.2.2:80", true}, {"/admin", "[2001:db8::1]:80", true},
		{"/admin", "198.51.100.1:80", false}, {"/%61dmin/users", "198.51.100.1:80", false}, {"/administrator", "198.51.100.1:80", true},
	} {
		req := httptest.NewRequest("GET", "http://example.test"+test.path, nil)
		req.RemoteAddr = test.ip
		if decision := acl.Evaluate(service.NewRequest(req)); decision.Allowed != test.allow {
			t.Fatal(test, decision)
		}
	}
	rules := scopeACL()
	rules[0].Allow = nil
	acl, err = gateway.NewACL(rules)
	if err != nil {
		t.Fatal(err)
	}
	if acl.Evaluate(service.NewRequest(httptest.NewRequest("GET", "http://example.test/admin", nil))).Allowed {
		t.Fatal("empty allow opened protected scope")
	}
	if !acl.Evaluate(service.NewRequest(httptest.NewRequest("GET", "http://example.test/public", nil))).Allowed {
		t.Fatal("scope denied unrelated path")
	}
}
func TestACLScopeOverlapAndLegacyCannotBypass(t *testing.T) {
	rules := scopeACL()
	rules = append(rules, gateway.ACLRule{ID: "nested", Enabled: true, Scope: []gateway.MatchCondition{{Source: "path", Operator: "prefix", Value: "/admin/private/"}}})
	rules = append([]gateway.ACLRule{{ID: "legacy", Enabled: true, Action: "allow"}}, rules...)
	for i := 0; i < 2; i++ {
		acl, err := gateway.NewACL(rules)
		if err != nil {
			t.Fatal(err)
		}
		req := httptest.NewRequest("GET", "http://example.test/admin/private/data", nil)
		req.RemoteAddr = "192.0.2.1:80"
		if acl.Evaluate(service.NewRequest(req)).Allowed {
			t.Fatal("overlap or legacy allow bypassed narrower scope")
		}
		rules[1], rules[2] = rules[2], rules[1]
	}
}
func TestACLScopeScriptOrAndCancellation(t *testing.T) {
	for _, script := range []string{`function(r){return false;}`, `function(r){throw Error("hidden");}`, `function(r){return "true";}`, `function(r){while(true){}}`} {
		rules := scopeACL()
		rules[0].Scope = nil
		rules[0].Allow = []gateway.ACLGrant{{Script: script, TimeoutMS: 2}, {Conditions: []gateway.MatchCondition{{Source: "method", Operator: "equals", Value: "GET"}, {Source: "header", Name: "X-Role", Operator: "equals", Value: "staff"}}}}
		acl, err := gateway.NewACL(rules)
		if err != nil {
			t.Fatal(err)
		}
		req := httptest.NewRequest("GET", "http://example.test/", nil)
		req.Header.Set("X-Role", "staff")
		if decision := acl.Evaluate(service.NewRequest(req)); !decision.Allowed || decision.Error != "" {
			t.Fatal("failed alternative vetoed valid grant", decision)
		}
		req = httptest.NewRequest("POST", "http://example.test/", nil)
		req.Header.Set("X-Role", "staff")
		if acl.Evaluate(service.NewRequest(req)).Allowed {
			t.Fatal("grant conditions were not ANDed")
		}
	}
	acl, err := gateway.NewACL([]gateway.ACLRule{{Enabled: true, Allow: []gateway.ACLGrant{{Script: `function(r){while(true){}}`, TimeoutMS: 100}}}})
	if err != nil {
		t.Fatal(err)
	}
	ctx, cancel := context.WithCancel(context.Background())
	cancel()
	req := httptest.NewRequest("GET", "http://example.test/", nil).WithContext(ctx)
	start := time.Now()
	if acl.Evaluate(service.NewRequest(req)).Allowed {
		t.Fatal("cancelled script allowed")
	}
	if time.Since(start) > time.Second {
		t.Fatal("cancellation ignored")
	}
}
func TestACLScopeValidationOwnershipAndUnnamedPolicy(t *testing.T) {
	for _, rule := range []gateway.ACLRule{
		{Allow: []gateway.ACLGrant{{}}},
		{Scope: []gateway.MatchCondition{{Source: "ip", Operator: "directory", Value: "/admin"}}},
		{Allow: []gateway.ACLGrant{{Conditions: []gateway.MatchCondition{{Source: "ip", Operator: "equals", Value: "invalid"}}}}},
		{Allow: []gateway.ACLGrant{{Script: `function(`}}},
		{Action: "deny", Scope: []gateway.MatchCondition{{Source: "path", Operator: "prefix", Value: "/admin/"}}},
	} {
		if _, err := gateway.NewACL([]gateway.ACLRule{rule}); err == nil {
			t.Fatalf("invalid scope accepted: %+v", rule)
		}
	}
	rules := scopeACL()
	acl, err := gateway.NewACL(rules)
	if err != nil {
		t.Fatal(err)
	}
	rules[0].Scope[0].Value = "/public"
	rules[0].Allow[0].Conditions[0].Value = "198.51.100.0/24"
	req := httptest.NewRequest("GET", "http://example.test/admin", strings.NewReader("body"))
	req.RemoteAddr = "192.0.2.1:80"
	if !acl.Evaluate(service.NewRequest(req)).Allowed {
		t.Fatal("caller mutated compiled policy")
	}
	m, _, _ := manager(t)
	host := baseHost()
	host.ACL = scopeACL()
	host.ACL[0].ID = ""
	host.Limits = []gateway.LimitRule{{Enabled: true, Requests: 10, PeriodSeconds: 60}}
	saved, err := m.SaveDraft(host, 0, "test")
	if err != nil {
		t.Fatal(err)
	}
	if saved.Draft.ACL[0].ID == "" || saved.Draft.Limits[0].ID == "" {
		t.Fatal("IDs not generated")
	}
	if saved.Draft.ACL[0].Name != "" || saved.Draft.Limits[0].Name != "" {
		t.Fatal("names should not be required")
	}
	again, err := m.SaveDraft(saved.Draft, saved.Revision, "test")
	if err != nil {
		t.Fatal(err)
	}
	if again.Draft.ACL[0].ID != saved.Draft.ACL[0].ID {
		t.Fatal("ID changed on save")
	}
}
func TestACLScopeObserveDoesNotBypassQuota(t *testing.T) {
	m, server, _ := manager(t)
	host := baseHost()
	host.ACL = []gateway.ACLRule{{ID: "observe", Enabled: true, Mode: "observe"}}
	host.Limits = []gateway.LimitRule{{ID: "quota", Enabled: true, Requests: 1, PeriodSeconds: 60}}
	savePublish(t, m, host, 0)
	for _, want := range []int{0, http.StatusTooManyRequests} {
		w := httptest.NewRecorder()
		server.Handler().ServeHTTP(w, httptest.NewRequest("GET", "http://example.test/", nil))
		if want == 0 && w.Code == 403 {
			t.Fatal("observe rejected")
		}
		if want != 0 && w.Code != want {
			t.Fatal("quota bypass", w.Code)
		}
	}
	count := 0
	for _, event := range m.Events() {
		if event.Type == "acl" && event.RuleID == "observe" && event.Action == "observe" {
			count++
		}
	}
	if count != 2 {
		t.Fatal("observation missing", count)
	}
}
func BenchmarkACLScope(b *testing.B) {
	for _, script := range []bool{false, true} {
		name := "directory"
		if script {
			name = "script"
		}
		b.Run(name, func(b *testing.B) {
			rules := scopeACL()
			if script {
				rules[0].Allow[0].Script = `function(r){return r.method === "GET";}`
				rules[0].Allow[0].TimeoutMS = 100
			}
			acl, err := gateway.NewACL(rules)
			if err != nil {
				b.Fatal(err)
			}
			b.ReportAllocs()
			b.ResetTimer()
			b.RunParallel(func(pb *testing.PB) {
				req := httptest.NewRequest("GET", "http://example.test/admin", nil)
				req.RemoteAddr = "192.0.2.1:80"
				wrapped := service.NewRequest(req)
				for pb.Next() {
					if !acl.Evaluate(wrapped).Allowed {
						b.Error("unexpected rejection")
					}
				}
			})
		})
	}
}

func TestACLScopeAPIKeyMayEditIPsButNotScript(t *testing.T) {
	m, _, _ := manager(t)
	host := baseHost()
	host.ACL = []gateway.ACLRule{{ID: "scope", Enabled: true, Scope: []gateway.MatchCondition{{Source: "path", Operator: "prefix", Value: "/admin/"}}, Allow: []gateway.ACLGrant{{Script: `function(r){return r.method === "GET";}`}}}}
	saved, err := m.SaveDraft(host, 0, "admin")
	if err != nil {
		t.Fatal(err)
	}
	handler := m.AdminHandler()
	cookie, csrf := login(t, handler, "admin", "gateway-test-password")
	listing := adminRequest(t, handler, "/api/keys", nil, cookie, csrf)
	state, err := cast.FromJSON[map[string]any](listing.Body.Bytes())
	if err != nil {
		t.Fatal(err)
	}
	created := adminRequest(t, handler, "/api/keys/create", map[string]any{"name": "scope-editor", "permissions": []string{"draft"}, "hostIDs": []string{host.ID}, "revision": state["revision"]}, cookie, csrf)
	if created.Code != 200 {
		t.Fatal(created.Body.String())
	}
	key, err := cast.FromJSON[map[string]any](created.Body.Bytes())
	if err != nil {
		t.Fatal(err)
	}
	host = saved.Draft
	host.ACL[0].Allow = append([]gateway.ACLGrant{{Conditions: []gateway.MatchCondition{{Source: "ip", Operator: "equals", Value: "192.0.2.1"}}}}, host.ACL[0].Allow...)
	response := machineRequest(t, handler, "/api/v1/draft", cast.String(key["token"]), map[string]any{"host": host, "revision": saved.Revision})
	if response.Code != 200 {
		t.Fatal("IP edit incorrectly rejected due to script index", response.Code, response.Body.String())
	}
	updated, err := cast.FromJSON[gateway.HostRecord](response.Body.Bytes())
	if err != nil {
		t.Fatal(err)
	}
	host.ACL[0].Allow[1].Script = `function(r){return true;}`
	response = machineRequest(t, handler, "/api/v1/draft", cast.String(key["token"]), map[string]any{"host": host, "revision": updated.Revision})
	if response.Code != 403 {
		t.Fatal("API key edited script", response.Code)
	}
}
