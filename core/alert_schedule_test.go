package gateway_test

import (
	"apigo.cc/go/cast"
	goCrypto "apigo.cc/go/crypto"
	"apigo.cc/go/encoding"
	"apigo.cc/go/file"
	gateway "apigo.cc/go/gateway/core"
	"apigo.cc/go/js"
	"apigo.cc/go/service"
	"context"
	"io"
	"net/http"
	"net/http/httptest"
	"path/filepath"
	"strings"
	"sync/atomic"
	"testing"
	"time"
)

func storedAlert(t *testing.T, config gateway.Config, id string) gateway.AlertChannel {
	t.Helper()
	raw, err := file.ReadBytes(filepath.Join(config.DataDir, "state.json"))
	if err != nil {
		t.Fatal(err)
	}
	state, err := cast.FromJSON[gateway.State](raw)
	if err != nil {
		t.Fatal(err)
	}
	return state.AlertChannels[id]
}
func TestAdvancedDingTalkExample(t *testing.T) {
	m, _, config := manager(t)
	var calls atomic.Int32
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		calls.Add(1)
		stamp := r.URL.Query().Get("timestamp")
		want := encoding.Base64(goCrypto.HMACSHA256([]byte("fixture-secret"), []byte(stamp+"\nfixture-secret")))
		if r.URL.Query().Get("sign") != want {
			t.Error("invalid example signature")
		}
		body, _ := io.ReadAll(r.Body)
		value, _ := cast.FromJSON[map[string]any](body)
		if value["msgtype"] != "text" {
			t.Error("invalid message")
		}
		w.Header().Set("Content-Type", "application/json")
		io.WriteString(w, `{"errcode":0}`)
	}))
	defer server.Close()
	source, err := file.Read("web/monitor.js")
	if err != nil {
		t.Fatal(err)
	}
	literal := strings.SplitN(strings.SplitN(source, "const dingTalkExample=", 2)[1], ";\n$('insertDingExample')", 2)[0]
	pool := js.NewPool()
	defer pool.Stop(context.Background())
	if err := pool.Define("template", "function(){return "+literal+";}", 1); err != nil {
		t.Fatal(err)
	}
	result, callErr := pool.CallContext(context.Background(), "template", time.Second, nil)
	if callErr != nil {
		t.Fatal(callErr)
	}
	code := strings.ReplaceAll(cast.String(result), "https://oapi.dingtalk.com/robot/send?access_token=替换为自己的token", server.URL+"?access_token=fixture")
	code = strings.ReplaceAll(code, "替换为自己的SEC密钥", "fixture-secret")
	channel := saveAlert(t, m, "custom", "", "", code, false)
	if err := m.SendAlert(context.Background(), storedAlert(t, config, channel.ID), gateway.AlertEvent{Domain: "example.test", State: "故障"}); err != nil {
		t.Fatal(err)
	}
	if calls.Load() != 1 {
		t.Fatal("example did not send")
	}
}
func TestAlertQuietHoursAndReminder(t *testing.T) {
	m, _, config := manager(t)
	var alerts atomic.Int32
	robot := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) { alerts.Add(1); io.WriteString(w, `{"errcode":0}`) }))
	defer robot.Close()
	upstream := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) { w.WriteHeader(503) }))
	defer upstream.Close()
	channel := saveAlert(t, m, "wecom", robot.URL, "", "", false)
	h := m.AdminHandler()
	cookie, csrf := login(t, h, "admin", "gateway-test-password")
	save := func(start, end string) {
		listing := adminRequest(t, h, "/api/alerts", nil, cookie, csrf)
		state, _ := cast.FromJSON[map[string]any](listing.Body.Bytes())
		channel.QuietStart = start
		channel.QuietEnd = end
		channel.Timezone = "UTC"
		channel.RepeatSeconds = 60
		w := adminRequest(t, h, "/api/alerts/save", map[string]any{"channel": channel, "revision": state["revision"]}, cookie, csrf)
		if w.Code != 200 {
			t.Fatal(w.Body.String())
		}
	}
	now := time.Now().UTC()
	save(now.Add(-time.Hour).Format("15:04"), now.Add(time.Hour).Format("15:04"))
	host := baseHost()
	host.Proxies = []service.ProxyRule{{Path: "/*", To: upstream.URL}}
	host.Monitor = gateway.MonitorConfig{Enabled: true, FailureThreshold: 1, ChannelIDs: []string{channel.ID}}
	savePublish(t, m, host, 0)
	if _, err := m.RunMonitorCheck(context.Background(), host.ID); err != nil {
		t.Fatal(err)
	}
	if alerts.Load() != 0 {
		t.Fatal("sent during quiet hours")
	}
	save("", "")
	one, err := m.RunMonitorCheck(context.Background(), host.ID)
	if err != nil {
		t.Fatal(err)
	}
	if _, err := m.RunMonitorCheck(context.Background(), host.ID); err != nil {
		t.Fatal(err)
	}
	if alerts.Load() != 1 {
		t.Fatal("reminded too soon")
	}
	for key := range one.NotificationTimes {
		one.NotificationTimes[key] = time.Now().Add(-2 * time.Minute)
	}
	// Persist a historical delivery time and restart to test the durable repeat interval.
	path := filepath.Join(config.DataDir, "monitor-compact", goCrypto.SHA256ToHex([]byte(host.ID))+".json")
	entry, _ := cast.ToJSON(map[string]any{"at": one.Time.UnixMilli(), "sample": one})
	raw, _ := cast.ToJSONBytes(map[string]string{"r:00": entry})
	if err := file.WriteBytes(path, raw); err != nil {
		t.Fatal(err)
	}
	m.Close()
	restarted, err := gateway.NewManager(config, service.NewWebServer())
	if err != nil {
		t.Fatal(err)
	}
	defer restarted.Close()
	if _, err := restarted.RunMonitorCheck(context.Background(), host.ID); err != nil {
		t.Fatal(err)
	}
	if alerts.Load() != 2 {
		t.Fatal("durable reminder not sent")
	}

}
