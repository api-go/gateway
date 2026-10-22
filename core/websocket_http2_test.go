package gateway_test

import (
	gohttp "apigo.cc/go/http"
	"apigo.cc/go/service"
	"github.com/gorilla/websocket"
	"golang.org/x/net/http2"
	"io"
	"net"
	"net/http"
	"net/http/httptest"
	"os"
	"strings"
	"sync/atomic"
	"testing"
	"time"
)

func TestGatewayWebSocketHTTP2TLS(t *testing.T) {
	if !strings.Contains(os.Getenv("GODEBUG"), "http2xconnect=1") {
		t.Skip("requires startup GODEBUG=http2xconnect=1")
	}
	for _, upstreamH2 := range []bool{false, true} {
		name := "h2-h1"
		if upstreamH2 {
			name = "h2-h2"
		}
		t.Run(name, func(t *testing.T) {
			m, server, _ := manager(t)
			upstream := httptest.NewUnstartedServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
				if (r.ProtoMajor == 2) != upstreamH2 || r.Host != "example.test" {
					t.Errorf("upstream: %s %s", r.Proto, r.Host)
				}
				upgrader := websocket.Upgrader{CheckOrigin: func(*http.Request) bool { return true }}
				conn, err := gohttp.UpgradeWebSocket(&upgrader, w, r, http.Header{"Set-Cookie": {"first=1", "second=2"}})
				if err != nil {
					t.Error(err)
					return
				}
				defer conn.Close()
				for {
					kind, data, err := conn.ReadMessage()
					if err != nil {
						return
					}
					if conn.WriteMessage(kind, data) != nil {
						return
					}
				}
			}))
			upstream.EnableHTTP2 = upstreamH2
			upstream.StartTLS()
			defer upstream.Close()
			proxyClient := gohttp.NewClient(time.Second)
			proxyClient.GetRawClient().Transport = upstream.Client().Transport
			server.SetProxyHTTPClient(proxyClient)
			host := baseHost()
			host.Proxies = []service.ProxyRule{{Path: "/*", To: upstream.URL + "/"}}
			record := savePublish(t, m, host, 0)
			ingress := httptest.NewUnstartedServer(server.Handler())
			ingress.EnableHTTP2 = true
			var connections atomic.Int32
			ingress.Config.ConnState = func(_ net.Conn, state http.ConnState) {
				if state == http.StateNew {
					connections.Add(1)
				}
			}
			ingress.StartTLS()
			defer ingress.Close()
			transport := &http2.Transport{TLSClientConfig: ingress.Client().Transport.(*http.Transport).TLSClientConfig}
			defer transport.CloseIdleConnections()
			client := gohttp.NewClient(time.Second)
			client.GetRawClient().Transport = transport
			// Default client mode prefers H2 without an application switch.
			open := func() *gohttp.WebSocketConn {
				t.Helper()
				conn, response, err := client.Open("wss"+ingress.URL[5:]+"/socket", http.Header{"Host": {"example.test"}})
				if err != nil {
					t.Fatalf("open: %v %+v", err, response)
				}
				if response.ProtoMajor != 2 || response.StatusCode != 200 || len(response.Header.Values("Set-Cookie")) < 2 {
					t.Fatalf("handshake: %+v", response)
				}
				return conn
			}
			first := open()
			defer first.Close()
			second := open()
			second.Close()
			// Publish a new route while the existing WebSocket remains on its stream.
			next := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) { io.WriteString(w, "new-route") }))
			defer next.Close()
			host.Proxies = []service.ProxyRule{{Path: "/*", To: next.URL + "/"}}
			savePublish(t, m, host, record.Revision)
			first.SetReadDeadline(time.Now().Add(time.Second))
			if err := first.Send("existing"); err != nil {
				t.Fatal(err)
			}
			if value, err := first.ReadString(); err != nil || value != "existing" {
				t.Fatalf("existing stream: %q %v", value, err)
			}
			request, _ := http.NewRequest("GET", ingress.URL+"/ordinary", nil)
			request.Host = "example.test"
			response, err := client.GetRawClient().Do(request)
			if err != nil {
				t.Fatal(err)
			}
			defer response.Body.Close()
			body, _ := io.ReadAll(response.Body)
			if response.ProtoMajor != 2 || string(body) != "new-route" {
				t.Fatalf("new route: %s %s", response.Proto, body)
			}
			if connections.Load() != 1 {
				t.Fatalf("HTTP/2 connections: %d", connections.Load())
			}
		})
	}
}
