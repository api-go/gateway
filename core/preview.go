package gateway

import (
	"github.com/api-go/service"
	"fmt"
	"net/http"
	"net/http/httptest"
	"net/url"
	"regexp"
	"sort"
	"strings"
)

type PreviewInput struct {
	Host    Host
	Domain  string
	Path    string
	Method  string
	IP      string
	UA      string
	Headers map[string]string
	Cookies map[string]string
}
type PreviewResult struct {
	HostID string
	Path   string
	Target string
	Type   string
	ACL    ACLDecision
	Limits []string
	Notes  []string
}

func matchRule(path, target, requestPath string) (string, bool) {
	if strings.ContainsRune(path, '(') {
		matcher, err := regexp.Compile("^" + path + "$")
		if err != nil {
			return "", false
		}
		matches := matcher.FindStringSubmatch(requestPath)
		if len(matches) == 0 {
			return "", false
		}
		for index, value := range matches {
			target = strings.ReplaceAll(target, fmt.Sprintf("$%d", index), value)
		}
		return target, true
	}
	if strings.HasSuffix(path, "/*") {
		prefix := path[:len(path)-1]
		if !strings.HasPrefix(requestPath, prefix) {
			return "", false
		}
		return strings.TrimSuffix(target, "*") + strings.TrimPrefix(requestPath, prefix), true
	}
	return target, path == requestPath
}
func (m *Manager) Preview(input PreviewInput) (PreviewResult, error) {
	return m.preview(input, true)
}

// Route-only callers (monitoring and direct upstream tests) do not run ingress ACL.
func (m *Manager) preview(input PreviewInput, checkACL bool) (PreviewResult, error) {
	if err := validateHost(input.Host); err != nil {
		return PreviewResult{}, err
	}
	if !validPreviewURL(input.Path) {
		return PreviewResult{}, fmt.Errorf("preview requires an absolute request path")
	}
	matchedDomain := false
	for _, domain := range input.Host.Domains {
		for _, candidate := range service.HostCandidates(input.Domain) {
			if domain == candidate {
				matchedDomain = true
			}
		}
		if strings.HasPrefix(domain, "*.") && strings.HasSuffix(normalizedHost(input.Domain), domain[1:]) {
			matchedDomain = true
		}
	}
	if !matchedDomain {
		return PreviewResult{Type: "unmatched", Notes: []string{"Domain does not match this Host"}}, nil
	}
	result := PreviewResult{HostID: input.Host.ID, Path: input.Path, Type: "unmatched", Notes: []string{"Preview does not contact upstreams, consume quotas or issue certificates."}}
	method := input.Method
	if method == "" {
		method = "GET"
	}
	request := httptest.NewRequest(method, input.Path, nil)
	request.Host = input.Domain
	request.RemoteAddr = input.IP
	for key, value := range input.Headers {
		request.Header.Set(key, value)
	}
	request.Header.Set("User-Agent", input.UA)
	for name, value := range input.Cookies {
		request.AddCookie(&http.Cookie{Name: name, Value: value})
	}
	if !input.Host.Enabled {
		result.Type = "disabled"
		return result, nil
	}
	wrapped := service.NewRequest(request)
	if checkACL {
		acl, err := compileACL(input.Host.ACL)
		if err != nil {
			return result, err
		}
		result.ACL = acl.evaluate(wrapped, nil)
		if !result.ACL.Allowed {
			result.Type = "denied"
			return result, nil
		}
	}
	for _, rule := range input.Host.Limits {
		compiled, err := compileLimit(rule)
		if err != nil {
			return result, err
		}
		if rule.Enabled && compiled.matches(wrapped) {
			result.Limits = append(result.Limits, limitSummary(rule)+" ("+rule.ID+")")
		}
	}
	if input.Host.HTTPS.Force {
		result.Notes = append(result.Notes, "HTTP requests redirect to HTTPS after quota checks")
	}
	path := request.URL.Path
	query := request.URL.RawQuery
	for _, rule := range input.Host.Rewrites {
		target := rule.To
		if target == "" {
			target = rule.ToPath
		}
		matchPath := path
		if strings.ContainsRune(rule.Path, '(') {
			matchPath = request.URL.RequestURI()
		}
		if rewritten, ok := matchRule(rule.Path, target, matchPath); ok {
			if query != "" && !strings.Contains(rewritten, "?") {
				rewritten += "?" + query
			}
			result.Path = rewritten
			parsed, _ := url.Parse(rewritten)
			path = parsed.Path
			query = parsed.RawQuery
			if strings.Contains(rewritten, "://") {
				result.Type = "redirect"
				result.Target = rewritten
				return result, nil
			}
			break
		}
	}
	for _, rule := range input.Host.Proxies {
		target := rule.To
		if target == "" {
			target = rule.ToApp + rule.ToPath
		}
		// Service defaults a proxy target without a path to the root directory.
		address := target
		if _, rest, hasScheme := strings.Cut(target, "://"); hasScheme {
			address = rest
		}
		if rule.To != "" && !strings.ContainsRune(address, '/') {
			target += "/"
		}
		if resolved, ok := matchRule(rule.Path, target, path); ok {
			result.Type = "proxy"
			if query != "" {
				resolved += "?" + query
			}
			result.Target = resolved
			return result, nil
		}
	}
	mounts := make([]string, 0, len(input.Host.Statics))
	for mount := range input.Host.Statics {
		mounts = append(mounts, mount)
	}
	sort.Slice(mounts, func(i, j int) bool { return len(mounts[i]) > len(mounts[j]) })
	for _, mount := range mounts {
		root := input.Host.Statics[mount]
		prefix := strings.TrimSuffix(mount, "/")
		if path == prefix || strings.HasPrefix(path, prefix+"/") {
			result.Type = "static"
			result.Target = root
			break
		}
	}
	return result, nil
}
func (m *Manager) handlePreview(writer http.ResponseWriter, request *http.Request) {
	if request.Method != "POST" {
		http.Error(writer, "method not allowed", 405)
		return
	}
	var input PreviewInput
	if err := readJSON(writer, request, &input); err != nil {
		http.Error(writer, err.Error(), 400)
		return
	}
	result, err := m.Preview(input)
	if err != nil {
		writeJSON(writer, 400, map[string]string{"error": err.Error()})
		return
	}
	writeJSON(writer, 200, result)
}
