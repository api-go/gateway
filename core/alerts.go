package gateway

import (
	goapi "apigo.cc/go/api"
	"apigo.cc/go/cast"
	goCrypto "apigo.cc/go/crypto"
	"apigo.cc/go/encoding"
	"apigo.cc/go/log"
	"apigo.cc/go/safe"
	"context"
	"errors"
	"fmt"
	"net/http"
	"net/url"
	"strings"
	"time"
)

type alertCredentials struct {
	Webhook string
	Secret  string
}
type alertInput struct {
	Channel  AlertChannel
	Webhook  string
	Secret   string
	Revision int64
}
type AlertEvent struct {
	HostID string
	Domain string
	State  string
	Status int
	Reason string
	Time   time.Time
}
type webhookAction struct {
	payload map[string]any
	config  map[string]any
}

func (a *webhookAction) ActionName() string           { return "gateway.monitor.webhook" }
func (a *webhookAction) Config() map[string]any       { return a.config }
func (a *webhookAction) MarshalJSON() ([]byte, error) { return cast.ToJSONBytes(a.payload) }

type robotSigner struct{}

func init() { goapi.RegisterSigner("gateway-monitor-robot", robotSigner{}) }
func (robotSigner) Sign(req *goapi.HttpRequest, config map[string]any) error {
	action, ok := req.Payload.(*webhookAction)
	if !ok {
		return fmt.Errorf("invalid webhook payload")
	}
	req.Payload = action.payload
	if !cast.Bool(config["sign"]) {
		return nil
	}
	secret, ok := config["secret"].(*safe.SecretPlaintext)
	if !ok || len(secret.Data) == 0 {
		return fmt.Errorf("signing secret required")
	}
	provider := cast.String(config["provider"])
	stamp := cast.String(time.Now().Unix())
	if provider == "dingtalk" {
		stamp = cast.String(time.Now().UnixMilli())
	}
	signInput := safe.Concat(stamp, "\n", secret)
	defer signInput.Close()
	plain := signInput.Open()
	defer plain.Close()
	if provider == "dingtalk" {
		parsed, err := url.Parse(req.Url)
		if err != nil {
			return err
		}
		query := parsed.Query()
		query.Set("timestamp", stamp)
		query.Set("sign", encoding.Base64(goCrypto.HMACSHA256(secret.Data, plain.Data)))
		parsed.RawQuery = query.Encode()
		req.Url = parsed.String()
	} else if provider == "feishu" {
		action.payload["timestamp"] = stamp
		action.payload["sign"] = encoding.Base64(goCrypto.HMACSHA256(plain.Data))
		req.Payload = action.payload
	} else {
		return fmt.Errorf("provider does not support signing")
	}
	return nil
}

var alertLogger = log.NewLogger(log.Config{Name: "gateway-alert", Level: "error"}) // Never log credential-bearing webhook URLs or bodies.
func callWebhook(ctx context.Context, rawURL string, payload map[string]any, headers map[string]any, provider string, secret *safe.SafeBuf, sign bool, method string) (*goapi.Result, error) {
	if !validWebhook(rawURL) {
		return nil, fmt.Errorf("Webhook URL 无效")
	}
	if method == "" {
		method = "POST"
	}
	if method != "POST" && method != "GET" && (provider != "custom" || method != "PUT" && method != "PATCH" && method != "DELETE" && method != "HEAD") {
		return nil, fmt.Errorf("报警请求方法无效")
	}
	encoded, err := cast.ToJSONBytes(payload)
	if err != nil || len(encoded) > 65536 {
		return nil, fmt.Errorf("报警请求体无效或超过 64 KiB")
	}
	safe.ZeroMemory(encoded)
	action := &webhookAction{payload: payload, config: map[string]any{"url": rawURL, "method": method, "format": "json", "signer": "gateway-monitor-robot", "sign": sign, "secret": secret, "provider": provider, "headers": headers, "logging": map[string]any{"request": false, "response": false}}}
	var reply []byte
	result, err := goapi.Call(action, &goapi.CallOptions{Context: ctx, Timeout: 5 * time.Second, Logger: alertLogger, Stream: &goapi.StreamOptions{OnData: func(chunk []byte) error {
		if len(reply)+len(chunk) > 65536 {
			return fmt.Errorf("robot response exceeds 64 KiB")
		}
		reply = append(reply, chunk...)
		return nil
	}}})
	if err == nil && result != nil {
		var decodeErr error
		result.Data, decodeErr = cast.FromJSON[any](reply)
		if decodeErr != nil {
			result.Data = cast.String(reply)
		}
	}
	return result, err
}

// SendAlert sends one administrator-configured channel, including provider-level acknowledgement checks.
func (m *Manager) SendAlert(ctx context.Context, channel AlertChannel, event AlertEvent) error {
	if !channel.Enabled {
		return fmt.Errorf("报警通道已停用")
	}
	encrypted, err := m.open(channel.SealedCredentials)
	if err != nil {
		return fmt.Errorf("报警凭据不可用")
	}
	defer safe.ZeroMemory(encrypted)
	credentials, err := cast.FromJSON[alertCredentials](encrypted)
	if err != nil {
		return err
	}
	secret := safe.NewSafeBufAndErase([]byte(credentials.Secret))
	defer secret.Close()
	ctx, cancel := context.WithTimeout(ctx, 8*time.Second)
	defer cancel()
	if channel.Provider == "custom" {
		calls, successes, exceeded := 0, 0, false
		send := func(request map[string]any) map[string]any {
			calls++
			if calls > 5 || ctx.Err() != nil {
				exceeded = true
				return map[string]any{"ok": false, "error": "发送次数或时间超过限制"}
			}
			target := cast.String(request["url"])
			if target == "" {
				target = credentials.Webhook
			}
			payload, _ := cast.ToMap[string, any](request["payload"])
			headers, _ := cast.ToMap[string, any](request["headers"])
			response, err := callWebhook(ctx, target, payload, headers, "custom", secret, false, strings.ToUpper(cast.String(request["method"])))
			if err != nil || response == nil || !response.Ok {
				failure := map[string]any{"ok": false, "error": "自定义报警请求失败"}
				if response != nil {
					failure["statusCode"] = response.StatusCode
					failure["data"] = response.ErrorDetail
					failure["headers"] = response.Headers
				}
				return failure
			}
			successes++
			return map[string]any{"ok": true, "statusCode": response.StatusCode, "data": response.Data, "headers": response.Headers}
		}
		result, err := runMonitorCode(ctx, channel.Script, map[string]any{"hostID": event.HostID, "domain": event.Domain, "state": event.State, "status": event.Status, "reason": event.Reason, "time": event.Time.Format(time.RFC3339)}, map[string]any{"request": send, "webhook": credentials.Webhook, "secret": credentials.Secret}, send)
		if err != nil || calls == 0 || successes == 0 || exceeded {
			return fmt.Errorf("自定义报警脚本失败或未成功执行 HTTP 请求")
		}
		acknowledged, _ := cast.ToMap[string, any](result)
		if !cast.Bool(acknowledged["ok"]) {
			return fmt.Errorf("自定义报警脚本须返回 {ok:true} 的发送结果")
		}
		return nil
	}
	text := fmt.Sprintf("Gateway 监控 %s\nHost：%s\nHTTP：%d\n%s", event.State, event.Domain, event.Status, event.Reason)
	payload := map[string]any{"msgtype": "text", "text": map[string]any{"content": text}}
	if channel.Provider == "feishu" {
		payload = map[string]any{"msg_type": "text", "content": map[string]any{"text": text}}
	}
	result, err := callWebhook(ctx, credentials.Webhook, payload, nil, channel.Provider, secret, channel.Sign, "POST")
	if err != nil || result == nil || !result.Ok {
		return fmt.Errorf("机器人 HTTP 请求失败")
	}
	reply, err := cast.ToMap[string, any](result.Data)
	if err != nil {
		return fmt.Errorf("机器人返回了无效回执")
	}
	code, exists := reply["errcode"]
	if channel.Provider == "feishu" {
		code, exists = reply["code"]
		if !exists {
			code, exists = reply["StatusCode"]
		}
	}
	if !exists || cast.Int(code) != 0 {
		return fmt.Errorf("机器人拒绝消息（错误码 %v）", code)
	}
	return nil
}
func (m *Manager) handleAlertChannels(w http.ResponseWriter, r *http.Request, actor string) {
	m.mu.Lock()
	defer m.mu.Unlock()
	if err := m.loadLocked(); err != nil {
		http.Error(w, "configuration store unavailable", 503)
		return
	}
	if r.Method == "GET" && r.URL.Path == "/api/alerts" {
		channels := []AlertChannel{}
		for _, entry := range m.state.AlertChannels {
			entry.HasCredentials = entry.SealedCredentials != ""
			entry.SealedCredentials = ""
			channels = append(channels, entry)
		}
		writeJSON(w, 200, map[string]any{"channels": channels, "revision": m.state.Revision})
		return
	}
	if r.Method != "POST" {
		http.Error(w, "method not allowed", 405)
		return
	}
	var input alertInput
	if err := readJSON(w, r, &input); err != nil {
		http.Error(w, err.Error(), 400)
		return
	}
	if input.Revision != m.state.Revision {
		http.Error(w, ErrConflict.Error(), 409)
		return
	}
	next := cloneState(m.state)
	if next.AlertChannels == nil {
		next.AlertChannels = map[string]AlertChannel{}
	}
	entry := input.Channel
	if r.URL.Path == "/api/alerts/delete" {
		for _, record := range next.Hosts {
			hosts := []Host{record.Draft}
			if record.Current != nil {
				hosts = append(hosts, *record.Current)
			}
			for _, host := range hosts {
				for _, id := range host.Monitor.ChannelIDs {
					if id == entry.ID {
						http.Error(w, "通道仍被 Host 引用，请先取消引用", 400)
						return
					}
				}
			}
		}
		delete(next.AlertChannels, entry.ID)
	} else if r.URL.Path == "/api/alerts/save" {
		if entry.ID == "" {
			entry.ID = encoding.Hex(safe.MakeSafeToken(12))
		}
		if len(entry.ID) > 80 || strings.ContainsAny(entry.ID, "/:* \r\n\t") || entry.Name == "" || len(entry.Name) > 100 || len(next.AlertChannels) >= 50 && next.AlertChannels[entry.ID].ID == "" {
			http.Error(w, "invalid channel", 400)
			return
		}
		if entry.Provider != "dingtalk" && entry.Provider != "feishu" && entry.Provider != "wecom" && entry.Provider != "custom" {
			http.Error(w, "invalid provider", 400)
			return
		}
		if entry.Sign && entry.Provider != "dingtalk" && entry.Provider != "feishu" {
			http.Error(w, "此通道不支持内置签名", 400)
			return
		}
		if entry.Provider == "custom" && (entry.Script == "" || validateCode(entry.Script) != nil) {
			http.Error(w, "自定义通道需要有效低代码函数", 400)
			return
		}
		if err := validateAlertSchedule(&entry); err != nil {
			http.Error(w, err.Error(), 400)
			return
		}
		previous := next.AlertChannels[entry.ID]
		if previous.ID != "" && previous.Provider != entry.Provider && entry.Provider != "custom" && input.Webhook == "" {
			http.Error(w, "切换通道类型时请重新填写 Webhook", 400)
			return
		}
		var credentials alertCredentials
		if previous.SealedCredentials != "" {
			raw, err := m.open(previous.SealedCredentials)
			if err != nil {
				http.Error(w, "credential decryption failed", 503)
				return
			}
			credentials, _ = cast.FromJSON[alertCredentials](raw)
			safe.ZeroMemory(raw)
		}
		if input.Webhook != "" {
			credentials.Webhook = input.Webhook
		}
		if input.Secret != "" {
			credentials.Secret = input.Secret
		}
		if entry.Provider != "custom" && (!validWebhook(credentials.Webhook) || entry.Sign && credentials.Secret == "") {
			http.Error(w, "Webhook 或加签密钥无效", 400)
			return
		}
		if entry.Provider == "custom" && input.Webhook == "" && previous.Provider != "custom" {
			credentials = alertCredentials{}
		}
		data, _ := cast.ToJSONBytes(credentials)
		defer safe.ZeroMemory(data)
		sealed, err := m.seal(data)
		if err != nil {
			http.Error(w, err.Error(), 500)
			return
		}
		entry.SealedCredentials = sealed
		entry.HasCredentials = false
		entry.Revision = previous.Revision + 1
		next.AlertChannels[entry.ID] = entry
	} else {
		http.NotFound(w, r)
		return
	}
	addAudit(&next, Event{Type: "alert-channel", Actor: actor, Action: r.URL.Path, Message: entry.ID})
	if err := m.saveLocked(next); err != nil {
		status := 503
		if errors.Is(err, ErrConflict) {
			status = 409
		}
		http.Error(w, err.Error(), status)
		return
	}
	entry.SealedCredentials = ""
	entry.HasCredentials = true
	writeJSON(w, 200, map[string]any{"channel": entry, "revision": m.state.Revision})
}

func (m *Manager) handleAlertTest(w http.ResponseWriter, r *http.Request) {
	if r.Method != "POST" {
		http.Error(w, "method not allowed", 405)
		return
	}
	var input alertInput
	if err := readJSON(w, r, &input); err != nil {
		http.Error(w, err.Error(), 400)
		return
	}
	m.mu.Lock()
	err := m.loadLocked()
	channel, exists := m.state.AlertChannels[input.Channel.ID]
	m.mu.Unlock()
	if err != nil {
		http.Error(w, "credential store unavailable", 503)
		return
	}
	if !exists {
		http.NotFound(w, r)
		return
	}
	if err := m.SendAlert(r.Context(), channel, AlertEvent{Domain: "测试消息", State: "测试", Reason: "Gateway 报警通道测试", Time: time.Now()}); err != nil {
		writeJSON(w, 400, map[string]string{"error": err.Error()})
		return
	}
	writeJSON(w, 200, map[string]bool{"ok": true})
}
