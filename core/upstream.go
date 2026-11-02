package gateway

import (
	"apigo.cc/go/cast"
	"apigo.cc/go/discover"
	gohttp "apigo.cc/go/http"
	"apigo.cc/go/log"
	"apigo.cc/go/redis"
	"context"
	"crypto/tls"
	"crypto/x509"
	"errors"
	"fmt"
	"io"
	"net"
	"net/http"
	"net/url"
	"strings"
	"time"
)

// UpstreamTestResult reports one bounded GET of the draft's matched upstream.
// Reachable means an HTTP response arrived, including authentication or business errors.
type UpstreamTestResult struct {
	Target      string
	Node        string
	Status      int
	Reachable   bool
	DurationMS  int64
	ContentType string
	Location    string
	Body        string
	Truncated   bool
	Error       string
	Failure     string
}

// TestUpstream uses draft routing without publishing or consuming gateway quotas.
// It always sends GET, never forwards administrator credentials and does not follow redirects.
func (m *Manager) TestUpstream(ctx context.Context, input PreviewInput) (UpstreamTestResult, error) {
	input.Method = http.MethodGet
	preview, err := m.preview(input, false)
	if err != nil {
		return UpstreamTestResult{}, err
	}
	if preview.Type != "proxy" {
		return UpstreamTestResult{}, fmt.Errorf("当前路径未匹配代理规则，无法测试上游")
	}
	return m.testUpstreamTarget(ctx, input, preview.Target)
}
func (m *Manager) testUpstreamTarget(ctx context.Context, input PreviewInput, addressTarget string) (UpstreamTestResult, error) {
	result := UpstreamTestResult{Target: addressTarget}
	target, err := url.Parse(addressTarget)
	if err != nil {
		return result, fmt.Errorf("invalid upstream target")
	}
	ctx, cancel := context.WithTimeout(ctx, 10*time.Second)
	defer cancel()
	address := addressTarget
	app := ""
	if target.Scheme == "" {
		app, address, _ = strings.Cut(addressTarget, "/")
		address = "/" + address
	} else if (target.Scheme != "http" && target.Scheme != "https") || target.Host == "" || target.User != nil {
		return result, fmt.Errorf("upstream requires an HTTP(S) URL or discovery service")
	}
	request, err := http.NewRequestWithContext(ctx, http.MethodGet, address, nil)
	if err != nil {
		return result, err
	}
	for name, value := range input.Headers {
		switch strings.ToLower(name) {
		case "host", "connection", "upgrade", "transfer-encoding", "content-length", "proxy-authorization", "x-csrf-token", "forwarded":
			continue
		}
		if strings.HasPrefix(strings.ToLower(name), "x-forwarded-") || strings.HasPrefix(strings.ToLower(name), "x-discover-") || strings.EqualFold(name, "X-User-Id") {
			continue
		}
		request.Header.Set(name, value)
	}
	for name, value := range input.Cookies {
		request.AddCookie(&http.Cookie{Name: name, Value: value})
	}
	request.Host = input.Domain
	request.Header.Set("Host", input.Domain)
	request.Header.Set("X-Host", input.Domain)
	request.Header.Set("X-Forwarded-Host", input.Domain)
	scheme := "https"
	if input.Host.HTTPS.Mode == "off" {
		scheme = "http"
	}
	request.Header.Set("X-Scheme", scheme)
	request.Header.Set("X-Forwarded-Proto", scheme)
	request.Header.Set("User-Agent", input.UA)
	started := time.Now()
	var response *http.Response
	if app != "" {
		// nil incoming request prevents relay of the administrator's Cookie and Authorization.
		response, result.Node, err = discover.NewCaller(nil, upstreamProbeLogger).DoRequest(app, request)
	} else {
		client := gohttp.NewClient(10 * time.Second)
		defer client.GetRawClient().CloseIdleConnections()
		response, err = client.GetRawClient().Do(request)
	}
	defer func() {
		if response != nil && response.Body != nil {
			response.Body.Close()
		}
	}()
	if err != nil {
		result.DurationMS = time.Since(started).Milliseconds()
		result.Failure = m.classifyUpstreamError(app, err)
		result.Error = upstreamFailureReason(result.Failure)
		return result, nil
	}
	result.Reachable = true
	result.Status = response.StatusCode
	result.ContentType = response.Header.Get("Content-Type")
	result.Location = response.Header.Get("Location")
	data, readErr := io.ReadAll(io.LimitReader(response.Body, 4097))
	result.Truncated = len(data) > 4096
	if result.Truncated {
		data = data[:4096]
	}
	result.Body = cast.String(data)
	result.DurationMS = time.Since(started).Milliseconds()
	if readErr != nil {
		result.Failure = "response_incomplete"
		result.Error = upstreamFailureReason(result.Failure)
	}
	return result, nil
}
func (m *Manager) handleUpstreamTest(w http.ResponseWriter, r *http.Request) {
	if r.Method != "POST" {
		http.Error(w, "method not allowed", 405)
		return
	}
	var input PreviewInput
	if err := readJSON(w, r, &input); err != nil {
		http.Error(w, err.Error(), 400)
		return
	}
	result, err := m.TestUpstream(r.Context(), input)
	m.cacheManualCheck(input.Host, result, err)
	if err != nil {
		writeJSON(w, 400, map[string]string{"error": err.Error()})
		return
	}
	writeJSON(w, 200, result)
}

// Probe failures are stored in bounded monitoring history, not duplicated into ordinary error logs every minute.
var upstreamProbeLogger = func() *log.Logger {
	logger := log.NewLogger(log.Config{Name: "gateway-probe", Level: "error"})
	logger.SetLevel(log.CLOSE)
	return logger
}()

func upstreamFailureReason(code string) string {
	switch code {
	case "service_unregistered":
		return "上游服务未注册"
	case "service_offline":
		return "上游服务没有在线节点"
	case "registry_unavailable":
		return "注册中心暂不可访问"
	case "timeout":
		return "请求超时"
	case "dns_error":
		return "上游域名解析失败"
	case "tls_error":
		return "TLS 握手或证书验证失败"
	case "response_incomplete":
		return "响应未完整读取"
	default:
		return "无法连接上游服务"
	}
}
func (m *Manager) classifyUpstreamError(app string, err error) string {
	var netErr net.Error
	if errors.Is(err, context.DeadlineExceeded) || errors.As(err, &netErr) && netErr.Timeout() {
		return "timeout"
	}
	var dnsErr *net.DNSError
	if errors.As(err, &dnsErr) {
		return "dns_error"
	}
	if app != "" && strings.Contains(err.Error(), "all nodes failed") {
		m.mu.Lock()
		name := m.registryName
		m.mu.Unlock()
		if name != "" {
			rd := redis.GetRedis(name, log.DefaultLogger)
			result := rd.Do("HGETALL", app)
			if result.Error != nil {
				return "registry_unavailable"
			}
			members := result.StringMap()
			if len(members) == 0 {
				return "service_unregistered"
			}
			online := false
			for address, weight := range members {
				if cast.Int(weight) <= 0 {
					continue
				}
				ttl := rd.Do("TTL", app+"_"+address)
				if ttl.Error != nil {
					return "registry_unavailable"
				}
				if ttl.Int() >= -1 {
					online = true
				}
			}
			if !online {
				return "service_offline"
			}
		}
	}
	var tlsErr *tls.CertificateVerificationError
	var recordErr tls.RecordHeaderError
	var authorityErr x509.UnknownAuthorityError
	var hostErr x509.HostnameError
	if errors.As(err, &tlsErr) || errors.As(err, &recordErr) || errors.As(err, &authorityErr) || errors.As(err, &hostErr) {
		return "tls_error"
	}
	return "connection_failed"
}
