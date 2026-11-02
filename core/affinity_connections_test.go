package gateway_test

import (
	"bufio"
	"context"
	"fmt"
	"io"
	"net"
	"net/http"
	"net/http/httptest"
	"net/http/httptrace"
	"os"
	"strings"
	"sync"
	"testing"
	"time"

	"apigo.cc/go/discover"
	gateway "apigo.cc/go/gateway/core"
	"apigo.cc/go/service"
	"github.com/gorilla/websocket"
)

// Changing owners affects subsequent requests, including streams multiplexed on
// an existing H2 connection. It must not interrupt already-running WS/SSE flows.
func TestAffinityLiveConnectionsAcrossRoutingChange(t *testing.T) {
	if os.Getenv("GATEWAY_REDIS_INTEGRATION") != "1" {
		t.Skip("requires isolated Service registry")
	}
	m, server, _ := manager(t)
	startAffinityServer(t, server)
	discover.Init()
	app := fmt.Sprintf("gateway-live-streams-%d", affinitySequence.Add(1))
	discover.AddExternalApp(app, "1")
	release := make(chan struct{})
	var releaseOnce sync.Once
	finishStream := func() { releaseOnce.Do(func() { close(release) }) }
	defer finishStream()
	addresses := []string{}
	for _, label := range []string{"old", "new"} {
		backend := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
			if websocket.IsWebSocketUpgrade(r) {
				upgrader := websocket.Upgrader{CheckOrigin: func(*http.Request) bool { return true }}
				conn, err := upgrader.Upgrade(w, r, nil)
				if err != nil {
					return
				}
				defer conn.Close()
				for {
					kind, data, err := conn.ReadMessage()
					if err != nil {
						return
					}
					if conn.WriteMessage(kind, []byte(label+":"+string(data))) != nil {
						return
					}
				}
			}
			if r.URL.Path == "/stream" {
				w.Header().Set("Content-Type", "text/event-stream")
				fmt.Fprint(w, strings.Repeat("data: "+label+"\n\n", 1024))
				w.(http.Flusher).Flush()
				select {
				case <-release:
				case <-r.Context().Done():
					return
				}
				fmt.Fprint(w, "data: end-"+label+"\n\n")
				return
			}
			fmt.Fprint(w, label)
		}))
		t.Cleanup(backend.Close)
		address := strings.TrimPrefix(backend.URL, "http://")
		addresses = append(addresses, address)
		affinityFixtureNode(t, app, address, 100)
		t.Cleanup(func() { affinityFixtureNode(t, app, address, 0) })
	}
	host := baseHost()
	host.Proxies = []service.ProxyRule{{Path: "/*", To: app + "/"}}
	host.Affinity = gateway.AffinityConfig{Mode: "manual", Source: "header", Name: "sessionid", Mappings: []gateway.AffinityMapping{{Values: []string{"same", "another"}, Nodes: addresses[:1]}}}
	record := savePublish(t, m, host, 0)
	ingress := httptest.NewUnstartedServer(m.Handler())
	ingress.EnableHTTP2 = true
	ingress.StartTLS()
	defer ingress.Close()
	client := ingress.Client()
	client.Timeout = 10 * time.Second
	defer client.CloseIdleConnections()
	var h2conn net.Conn
	request := func(path, id string) *http.Response {
		t.Helper()
		req, _ := http.NewRequest("GET", ingress.URL+path, nil)
		req.Host = "example.test"
		req.Header.Set("sessionid", id)
		trace := &httptrace.ClientTrace{GotConn: func(info httptrace.GotConnInfo) {
			if h2conn == nil {
				h2conn = info.Conn
			} else if info.Conn != h2conn {
				t.Error("H2 connection was not reused")
			}
		}}
		res, err := client.Do(req.WithContext(httptrace.WithClientTrace(context.Background(), trace)))
		if err != nil {
			t.Fatal(err)
		}
		if res.ProtoMajor != 2 || res.StatusCode != 200 {
			res.Body.Close()
			t.Fatalf("expected H2/200: %s/%d", res.Proto, res.StatusCode)
		}
		return res
	}
	stream := request("/stream", "same")
	defer stream.Body.Close()
	reader := bufio.NewReader(stream.Body)
	if line, err := reader.ReadString('\n'); err != nil || line != "data: old\n" {
		t.Fatalf("SSE start: %q %v", line, err)
	}
	wsIngress := httptest.NewServer(m.Handler())
	defer wsIngress.Close()
	ws, _, err := websocket.DefaultDialer.Dial("ws"+strings.TrimPrefix(wsIngress.URL, "http"), http.Header{"Host": {"example.test"}, "Sessionid": {"same"}})
	if err != nil {
		t.Fatal(err)
	}
	defer ws.Close()
	echo := func() {
		t.Helper()
		ws.SetReadDeadline(time.Now().Add(5 * time.Second))
		ws.SetWriteDeadline(time.Now().Add(5 * time.Second))
		if err := ws.WriteMessage(websocket.TextMessage, []byte("keep")); err != nil {
			t.Fatal(err)
		}
		_, data, err := ws.ReadMessage()
		if err != nil || string(data) != "old:keep" {
			t.Fatalf("established WS moved: %q %v", data, err)
		}
	}
	echo()
	host.Affinity.Mappings[0].Nodes = addresses[1:]
	savePublish(t, m, host, record.Revision)
	for i := range 200 {
		res := request("/", []string{"same", "another"}[i%2])
		data, err := io.ReadAll(res.Body)
		res.Body.Close()
		if err != nil || string(data) != "new" {
			t.Fatalf("subsequent H2 stream did not use new owner: %q %v", data, err)
		}
		echo()
	}
	finishStream()
	data, err := io.ReadAll(reader)
	if err != nil || !strings.HasSuffix(string(data), "data: end-old\n\n") || strings.Contains(string(data), "new") {
		t.Fatalf("in-flight SSE interrupted: %v", err)
	}
	t.Log("same H2 connection: 200 new requests use new owner; established WS 201 echoes and SSE remain on old owner")
}
