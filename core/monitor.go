package gateway

import (
	"apigo.cc/go/cast"
	goCrypto "apigo.cc/go/crypto"
	"apigo.cc/go/file"
	"apigo.cc/go/safe"
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"maps"
	"net/http"
	"net/url"
	"path/filepath"
	"sort"
	"strings"
	"sync"
	"time"
)

var errMonitorBusy = errors.New("节点检查正在执行或处于租约间隔")

type monitorAttempt struct {
	Time        time.Time
	Fingerprint string
}
type MonitorDomainResult struct {
	Domain     string
	OK         bool
	Status     int
	DurationMS int64
	Reason     string
	Failure    string
}
type MonitorOutput struct {
	Time        time.Time
	Domain      string
	Status      int
	Body        string
	ContentType string
	Location    string
	Truncated   bool
}
type MonitorSample struct {
	Output            *MonitorOutput
	Domains           []MonitorDomainResult
	Time              time.Time
	HostID            string
	Fingerprint       string
	Source            string
	OK                bool
	Node              string
	Status            int
	DurationMS        int64
	Reason            string
	Failure           string
	Failures          int
	Notifications     map[string]string
	NotificationTimes map[string]time.Time
	AlertError        string
}
type HostMonitorStatus struct {
	ConfigurationOK    bool
	ConfigurationError string
	Last               *MonitorSample
	Monitoring         bool
	Stale              bool
	HistoryError       bool
}

func (m *Manager) prepareMonitorSecrets(host *Host, previous Host) error {
	host.Monitor = monitorDefaults(host.Monitor)
	if host.Monitor.ClearHeaders {
		host.Monitor.SealedHeaders = ""
		host.Monitor.Headers = nil
	} else if host.Monitor.Headers != nil {
		if len(host.Monitor.Headers) == 0 {
			host.Monitor.SealedHeaders = ""
		} else {
			if err := validateMonitor(host.Monitor); err != nil {
				return err
			}
			raw, err := cast.ToJSONBytes(host.Monitor.Headers)
			if err != nil {
				return err
			}
			defer safe.ZeroMemory(raw)
			host.Monitor.SealedHeaders, err = m.seal(raw)
			if err != nil {
				return err
			}
		}
	} else {
		host.Monitor.SealedHeaders = previous.Monitor.SealedHeaders
	}
	host.Monitor.Headers = nil
	host.Monitor.ClearHeaders = false
	host.Monitor.HasHeaders = false
	return nil
}
func (m *Manager) monitorInput(host Host) (PreviewInput, error) {
	settings := monitorDefaults(host.Monitor)
	domain := settings.Domain
	if domain == "" && len(host.Domains) > 0 {
		domain = host.Domains[0]
	}
	if domain == "*" || strings.HasPrefix(domain, "*.") || strings.HasPrefix(domain, ":") {
		if settings.URL != "" {
			parsed, _ := url.Parse(settings.URL)
			domain = parsed.Hostname()
		} else {
			return PreviewInput{}, fmt.Errorf("监控需要具体请求域名")
		}
	}
	headers := map[string]string{}
	if settings.SealedHeaders != "" {
		plain, err := m.open(settings.SealedHeaders)
		if err != nil {
			return PreviewInput{}, err
		}
		defer safe.ZeroMemory(plain)
		if err := cast.UnmarshalJSON(plain, &headers); err != nil {
			return PreviewInput{}, err
		}
	}
	return PreviewInput{Host: host, Domain: domain, Path: settings.Path, Headers: headers, UA: "GatewayMonitor/1"}, nil
}
func (m *Manager) probeMonitorOne(ctx context.Context, host Host) MonitorSample {
	settings := monitorDefaults(host.Monitor)
	sample := MonitorSample{HostID: host.ID, Fingerprint: hostFingerprint(host), Source: "monitor", Time: time.Now(), Notifications: map[string]string{}}
	input, err := m.monitorInput(host)
	if err != nil {
		sample.Failure = "configuration_error"
		sample.Reason = "监控域名或凭据无效"
		return sample
	}
	var response UpstreamTestResult
	if settings.URL != "" {
		response, err = m.testUpstreamTarget(ctx, input, settings.URL)
	} else {
		response, err = m.TestUpstream(ctx, input)
	}
	sample.Time = time.Now()
	sample.Node = response.Node
	sample.Status = response.Status
	sample.Output = &MonitorOutput{Time: sample.Time, Domain: input.Domain, Status: response.Status, Body: response.Body, ContentType: response.ContentType, Location: response.Location, Truncated: response.Truncated}
	sample.DurationMS = response.DurationMS
	if err != nil {
		sample.Failure = "route_unmatched"
		sample.Reason = "监控路径未匹配有效上游代理"
		return sample
	}
	if !response.Reachable || response.Error != "" {
		sample.Failure = response.Failure
		if sample.Failure == "" {
			sample.Failure = "connection_failed"
		}
		sample.Reason = upstreamFailureReason(sample.Failure)
		return sample
	}
	if (settings.ExpectedStatus == 0 && (response.Status < 200 || response.Status >= 400)) || (settings.ExpectedStatus != 0 && response.Status != settings.ExpectedStatus) {
		sample.Failure = "http_status"
		if response.Status == 401 {
			sample.Failure = "unauthorized"
		} else if response.Status == 403 {
			sample.Failure = "forbidden"
		} else if response.Status >= 500 {
			sample.Failure = "server_error"
		}
		sample.Reason = fmt.Sprintf("HTTP 状态不符合预期：%d", response.Status)
		if settings.ExpectedStatus != 0 {
			sample.Reason = fmt.Sprintf("期望 HTTP %d，实际 %d", settings.ExpectedStatus, response.Status)
		}
		return sample
	}
	if settings.BodyContains != "" && !strings.Contains(response.Body, settings.BodyContains) {
		sample.Failure = "response_mismatch"
		sample.Reason = "响应内容未包含指定文本（检查前 4096 字节）"
		return sample
	}
	if settings.CheckScript != "" {
		value, err := runMonitorCode(ctx, settings.CheckScript, map[string]any{"status": response.Status, "body": response.Body, "contentType": response.ContentType, "durationMS": response.DurationMS, "truncated": response.Truncated})
		if err != nil {
			sample.Failure = "script_error"
			sample.Reason = "低代码响应检查执行失败（超时或异常）"
			return sample
		}
		pass, ok := value.(bool)
		if !ok || !pass {
			sample.Failure = "script_rejected"
			sample.Reason = "低代码检查未返回 true"
			return sample
		}
	}
	sample.OK = true
	sample.Reason = "检查通过"
	return sample
}

// A default round checks every configured domain; explicit Domain/URL is one administrator-selected target.
func (m *Manager) probeMonitor(ctx context.Context, host Host) MonitorSample {
	settings := monitorDefaults(host.Monitor)
	domains := host.Domains
	if settings.Domain != "" {
		domains = []string{settings.Domain}
	} else if settings.URL != "" {
		domains = []string{host.Domains[0]}
	}
	results := make([]MonitorSample, len(domains))
	var wg sync.WaitGroup
	slots := make(chan struct{}, 4)
	for i, domain := range domains {
		wg.Add(1)
		go func(i int, domain string) {
			defer wg.Done()
			select {
			case slots <- struct{}{}:
				defer func() { <-slots }()
			case <-ctx.Done():
				results[i] = MonitorSample{Failure: "timeout", Reason: "检查已取消或超时"}
				return
			}
			copy := host
			copy.Monitor.Domain = domain
			results[i] = m.probeMonitorOne(ctx, copy)
		}(i, domain)
	}
	wg.Wait()
	sample := MonitorSample{HostID: host.ID, Fingerprint: hostFingerprint(host), Source: "monitor", Time: time.Now(), OK: true, Reason: "全部域名检查通过", Notifications: map[string]string{}}
	for i, result := range results {
		reason := result.Reason
		if len(reason) > 256 {
			reason = "上游检查失败"
		}
		sample.Domains = append(sample.Domains, MonitorDomainResult{Domain: domains[i], OK: result.OK, Status: result.Status, DurationMS: result.DurationMS, Reason: reason, Failure: result.Failure})
		if i == 0 {
			sample.Status = result.Status
			sample.Node = result.Node
			sample.Output = result.Output
		}
		if result.DurationMS > sample.DurationMS {
			sample.DurationMS = result.DurationMS
		}
		if !result.OK && sample.OK {
			sample.OK = false
			sample.Failure = result.Failure
			sample.Status = result.Status
			sample.Node = result.Node
			sample.Output = result.Output
			sample.Reason = domains[i] + "：" + reason
		}
	}
	return sample
}
func (m *Manager) historyKey(id string) string {
	return m.config.Prefix + ":monitor-history:" + goCrypto.SHA256ToHex([]byte(id))
}
func (m *Manager) historyPath(id string) string {
	return filepath.Join(m.config.DataDir, "monitor", goCrypto.SHA256ToHex([]byte(id))+".json")
}

// MonitorHistory returns the latest 10 retained automatic checks, oldest first.
func (m *Manager) MonitorHistory(id string) ([]MonitorSample, error) {
	m.monitorMu.Lock()
	defer m.monitorMu.Unlock()
	return m.readHistoryLocked(id)
}

// Syntax validation is a storage boundary check; cast remains the semantic decoder.
func decodeMonitorSample(data []byte, id string) (MonitorSample, error) {
	if len(data) > 32768 || !json.Valid(data) {
		return MonitorSample{}, fmt.Errorf("invalid monitor sample JSON")
	}
	sample, err := cast.FromJSON[MonitorSample](data)
	if err != nil {
		return sample, err
	}
	if err := validateMonitorSample(sample, id); err != nil {
		return MonitorSample{}, err
	}
	return sample, nil
}
func validateMonitorSample(sample MonitorSample, id string) error {
	if sample.HostID != id || sample.Time.IsZero() || len(sample.Fingerprint) != 64 || strings.Trim(sample.Fingerprint, "0123456789abcdef") != "" || sample.Status < 0 || sample.Status > 599 || sample.Failures < 0 {
		return fmt.Errorf("invalid monitor sample schema")
	}
	return nil
}
func (m *Manager) readLegacyHistoryLocked(id string) ([]MonitorSample, error) {
	result := []MonitorSample{}
	if m.rd != nil {
		values := m.rd.Do("LRANGE", m.historyKey(id), 0, 199)
		if values.Error != nil {
			return nil, values.Error
		}
		entries := values.Strings()
		for i := len(entries) - 1; i >= 0; i-- {
			if len(entries[i]) > 32768 {
				return nil, fmt.Errorf("invalid monitor sample")
			}
			entry, err := decodeMonitorSample([]byte(entries[i]), id)
			if err != nil {
				return nil, err
			}
			result = append(result, entry)
		}
		return result, nil
	}
	path := m.historyPath(id)
	if !file.Exists(path) {
		return result, nil
	}
	data, err := file.ReadBytes(path)
	if err != nil {
		return nil, err
	}
	if len(data) > 256<<10 {
		return nil, fmt.Errorf("monitor history exceeds limit")
	}
	trimmed := bytes.TrimSpace(data)
	if len(trimmed) == 0 || trimmed[0] != '[' || !json.Valid(trimmed) {
		return nil, fmt.Errorf("invalid monitor history JSON")
	}
	err = cast.UnmarshalJSON(data, &result)
	if err != nil {
		return nil, err
	}
	for _, sample := range result {
		if err := validateMonitorSample(sample, id); err != nil {
			return nil, err
		}
	}
	return result, nil
}
func copyMonitorSample(sample MonitorSample) MonitorSample {
	sample.Notifications = maps.Clone(sample.Notifications)
	sample.NotificationTimes = maps.Clone(sample.NotificationTimes)
	sample.Domains = append([]MonitorDomainResult(nil), sample.Domains...)
	return sample
}
func (m *Manager) readHistoryLocked(id string) ([]MonitorSample, error) {
	values, err := m.readCompactLocked(id)
	if err != nil {
		return nil, err
	}
	if len(values) > 0 {
		return compactRecent(values, id)
	}
	history, err := m.readLegacyHistoryLocked(id)
	if len(history) > 10 {
		history = history[len(history)-10:]
	}
	return history, err
}
func (m *Manager) latestMonitor(id string) (MonitorSample, error) {
	m.monitorMu.Lock()
	defer m.monitorMu.Unlock()
	history, err := m.readHistoryLocked(id)
	if err != nil {
		return MonitorSample{}, err
	}
	if len(history) == 0 {
		return MonitorSample{}, nil
	}
	return copyMonitorSample(history[len(history)-1]), nil
}
func (m *Manager) saveSample(sample MonitorSample) error {
	m.monitorMu.Lock()
	defer m.monitorMu.Unlock()
	return m.saveCompactLocked(sample)
}

// RunMonitorCheck checks a published Host and delivers transition alerts, preserving bounded history.
func (m *Manager) RunMonitorCheck(ctx context.Context, id string) (MonitorSample, error) {
	m.monitorMu.Lock()
	if m.monitorJobs == nil {
		m.monitorJobs = map[string]bool{}
	}
	if m.monitorJobs[id] {
		m.monitorMu.Unlock()
		return MonitorSample{}, errMonitorBusy
	}
	m.monitorJobs[id] = true
	m.monitorMu.Unlock()
	defer func() { m.monitorMu.Lock(); delete(m.monitorJobs, id); m.monitorMu.Unlock() }()
	m.mu.Lock()
	if err := m.loadLocked(); err != nil {
		m.mu.Unlock()
		return MonitorSample{}, err
	}
	record := m.state.Hosts[id]
	state := cloneState(m.state)
	m.mu.Unlock()
	if record.Current == nil || !record.Current.Enabled || !monitorEnabled(*record.Current) {
		return MonitorSample{}, fmt.Errorf("Host 未发布、停用或监控未启用")
	}
	host := *state.Hosts[id].Current
	settings := monitorDefaults(host.Monitor)
	ctx, cancel := context.WithTimeout(ctx, 25*time.Second)
	defer cancel()
	previous, err := m.latestMonitor(id)
	if err != nil {
		return MonitorSample{}, err
	}
	// All entry points, including manual automatic-monitor checks, share the lease.
	if m.rd != nil {
		lease := m.rd.Do("SET", m.config.Prefix+":monitor-lease:"+id, m.config.NodeID, "NX", "EX", 45)
		if lease.Error != nil {
			return MonitorSample{}, lease.Error
		}
		if !lease.Bool() {
			return MonitorSample{}, errMonitorBusy
		}
	}
	sample := m.probeMonitor(ctx, host)
	if previous.Fingerprint == sample.Fingerprint {
		sample.Notifications = previous.Notifications
		sample.NotificationTimes = previous.NotificationTimes
		sample.Failures = previous.Failures
	}
	if sample.Notifications == nil {
		sample.Notifications = map[string]string{}
	}
	if sample.NotificationTimes == nil {
		sample.NotificationTimes = map[string]time.Time{}
	}
	if sample.OK {
		sample.Failures = 0
	} else {
		sample.Failures++
	}
	desired := ""
	if !sample.OK && sample.Failures >= settings.FailureThreshold {
		desired = "故障"
	}
	m.mu.Lock()
	loadErr := m.loadLocked()
	currentPublished := m.state.Hosts[id].Current
	currentValid := currentPublished != nil && hostFingerprint(*currentPublished) == sample.Fingerprint
	state.AlertChannels = cloneState(m.state).AlertChannels
	m.mu.Unlock()
	if loadErr != nil {
		return sample, loadErr
	}
	if !currentValid {
		return sample, fmt.Errorf("监控配置已变化，未发送报警")
	}
	activeNotifications := map[string]bool{}
	for _, channelID := range settings.ChannelIDs {
		if channel, exists := state.AlertChannels[channelID]; exists {
			activeNotifications[channelID+"@"+cast.String(channel.Revision)] = true
		}
	}
	for key := range sample.Notifications {
		if !activeNotifications[key] {
			delete(sample.Notifications, key)
			delete(sample.NotificationTimes, key)
		}
	}
	for _, channelID := range settings.ChannelIDs {
		channel, exists := state.AlertChannels[channelID]
		if !exists || !channel.Enabled {
			continue
		}
		notificationKey := channelID + "@" + cast.String(channel.Revision)
		transition := desired
		if sample.OK && sample.Notifications[notificationKey] == "故障" {
			transition = "恢复"
		}
		if transition == "" || alertQuiet(channel, sample.Time) || (sample.Notifications[notificationKey] == transition && (transition != "故障" || channel.RepeatSeconds == 0 || sample.Time.Sub(sample.NotificationTimes[notificationKey]) < time.Duration(channel.RepeatSeconds)*time.Second)) {
			continue
		}
		event := AlertEvent{HostID: id, Domain: host.Domains[0], State: transition, Status: sample.Status, Reason: sample.Reason, Time: sample.Time}
		if err := m.SendAlert(ctx, channel, event); err != nil {
			sample.AlertError = "通道 " + channel.Name + "：" + err.Error()
		} else {
			sample.Notifications[notificationKey] = transition
			sample.NotificationTimes[notificationKey] = sample.Time
		}
	}
	// Do not attach late results to a newer publication.
	m.mu.Lock()
	current := m.state.Hosts[id].Current
	unchanged := current != nil && hostFingerprint(*current) == sample.Fingerprint
	m.mu.Unlock()
	if !unchanged {
		return sample, fmt.Errorf("监控期间 Host 配置已变化，结果未写入")
	}
	if err := m.saveSample(sample); err != nil {
		return sample, err
	}
	return sample, nil
}

// MonitorTick fairly visits up to four due Hosts. task supplies lifecycle, skip policy and cancellation.
func (m *Manager) MonitorTick(ctx context.Context) error {
	m.mu.Lock()
	records := cloneState(m.state).Hosts
	m.mu.Unlock()
	ids := []string{}
	for id, record := range records {
		if record.Current != nil && record.Current.Enabled && monitorEnabled(*record.Current) {
			ids = append(ids, id)
		}
	}
	sort.Strings(ids)
	if len(ids) == 0 {
		return nil
	}
	m.monitorMu.Lock()
	for id := range m.monitorLatest {
		if _, ok := records[id]; !ok {
			delete(m.monitorLatest, id)
		}
	}
	for id := range m.monitorAttempts {
		if _, ok := records[id]; !ok {
			delete(m.monitorAttempts, id)
		}
	}
	for id := range m.manualChecks {
		if _, ok := records[id]; !ok {
			delete(m.manualChecks, id)
		}
	}
	start := m.monitorCursor % len(ids)
	m.monitorCursor = (start + 4) % len(ids)
	m.monitorMu.Unlock()
	var wg sync.WaitGroup
	var errorMu sync.Mutex
	var first error
	count := 0
	for offset := 0; offset < len(ids) && count < 4; offset++ {
		id := ids[(start+offset)%len(ids)]
		record := records[id]
		if ctx.Err() != nil {
			errorMu.Lock()
			if first == nil {
				first = ctx.Err()
			}
			errorMu.Unlock()
			break
		}
		last, err := m.latestMonitor(id)
		if err != nil {
			errorMu.Lock()
			if first == nil {
				first = err
			}
			errorMu.Unlock()
			continue
		}
		settings := monitorDefaults(record.Current.Monitor)
		fingerprint := hostFingerprint(*record.Current)
		if !last.Time.IsZero() && last.Fingerprint == fingerprint && time.Since(last.Time) < time.Duration(settings.IntervalSeconds)*time.Second {
			continue
		}
		m.monitorMu.Lock()
		attempt := m.monitorAttempts[id]
		if !attempt.Time.IsZero() && attempt.Fingerprint == fingerprint && time.Since(attempt.Time) < time.Duration(settings.IntervalSeconds)*time.Second {
			m.monitorMu.Unlock()
			continue
		}
		if m.monitorAttempts == nil {
			m.monitorAttempts = map[string]monitorAttempt{}
		}
		m.monitorAttempts[id] = monitorAttempt{Time: time.Now(), Fingerprint: fingerprint}
		m.monitorMu.Unlock()
		count++
		wg.Add(1)
		go func() {
			defer wg.Done()
			_, err := m.RunMonitorCheck(ctx, id)
			if err != nil && !errors.Is(err, errMonitorBusy) {
				errorMu.Lock()
				if first == nil {
					first = err
				}
				errorMu.Unlock()
			}
		}()
	}
	wg.Wait()
	return first
}
func (m *Manager) cacheManualCheck(host Host, response UpstreamTestResult, err error) {
	m.mu.Lock()
	host.Monitor.SealedHeaders = m.state.Hosts[host.ID].Draft.Monitor.SealedHeaders
	m.mu.Unlock()
	failure := response.Failure
	if failure == "" {
		switch {
		case err != nil:
			failure = "route_unmatched"
		case response.Status == 401:
			failure = "unauthorized"
		case response.Status == 403:
			failure = "forbidden"
		case response.Status >= 500:
			failure = "server_error"
		case response.Status >= 400:
			failure = "http_status"
		}
	}
	sample := MonitorSample{HostID: host.ID, Fingerprint: hostFingerprint(host), Source: "manual", Time: time.Now(), Status: response.Status, DurationMS: response.DurationMS, OK: err == nil && response.Error == "" && response.Reachable && response.Status >= 200 && response.Status < 400, Failure: failure, Reason: "手动业务测试"}
	m.monitorMu.Lock()
	defer m.monitorMu.Unlock()
	if m.manualChecks == nil {
		m.manualChecks = map[string]MonitorSample{}
	}
	if len(m.manualChecks) < 1000 || m.manualChecks[host.ID].HostID != "" {
		m.manualChecks[host.ID] = sample
	}
}

// MonitorStatuses includes fresh configuration validation and fingerprint-bound upstream results.
func (m *Manager) MonitorStatuses() (map[string]HostMonitorStatus, error) {
	result := map[string]HostMonitorStatus{}
	m.mu.Lock()
	if err := m.loadLocked(); err != nil {
		m.mu.Unlock()
		return nil, err
	}
	records := cloneState(m.state).Hosts
	for id, record := range records {
		entry := HostMonitorStatus{Monitoring: record.Current != nil && record.Current.Enabled && monitorEnabled(*record.Current)}
		err := validateHost(record.Draft)
		if err == nil {
			err = m.validatePublicationLocked(record.Draft, record.Version+1)
		}
		entry.ConfigurationOK = err == nil
		if err != nil {
			entry.ConfigurationError = err.Error()
		}
		result[id] = entry
	}
	m.mu.Unlock()
	for id, record := range records {
		entry := result[id]
		fingerprint := hostFingerprint(record.Draft)
		m.monitorMu.Lock()
		manual := m.manualChecks[id]
		m.monitorMu.Unlock()
		if manual.Fingerprint == fingerprint {
			copy := manual
			entry.Last = &copy
		}
		last, err := m.latestMonitor(id)
		if err != nil {
			entry.HistoryError = true
		} else if last.Fingerprint == fingerprint && !last.Time.IsZero() && (entry.Last == nil || entry.Monitoring || last.Time.After(entry.Last.Time)) {
			entry.Last = &last
		}
		if entry.Last != nil && entry.Monitoring {
			entry.Stale = time.Since(entry.Last.Time) > time.Duration(monitorDefaults(record.Draft.Monitor).IntervalSeconds*2+45)*time.Second
		}
		result[id] = entry
	}
	return result, nil
}
func (m *Manager) handleMonitor(w http.ResponseWriter, r *http.Request) {
	if r.Method != "GET" {
		http.Error(w, "method not allowed", 405)
		return
	}
	if r.URL.Path == "/api/monitor-matrix" {
		rows, err := m.MonitorMatrix()
		if err != nil {
			http.Error(w, "monitor store unavailable", 503)
			return
		}
		writeJSON(w, 200, rows)
		return
	}
	if r.URL.Path == "/api/monitor-status" {
		result, err := m.MonitorStatuses()
		if err != nil {
			http.Error(w, "monitor history unavailable", 503)
			return
		}
		writeJSON(w, 200, result)
		return
	}
	id := r.URL.Query().Get("host")
	history, err := m.MonitorHistory(id)
	if err != nil {
		http.Error(w, "monitor history unavailable", 503)
		return
	}
	writeJSON(w, 200, history)
}
