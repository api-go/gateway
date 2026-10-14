package gateway_test

import (
	"io"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
	"time"

	"apigo.cc/go/service"
	"github.com/gorilla/websocket"
)

func TestPublishedProxySharesHTTP2AndWebSocketTLSListener(t *testing.T) {
	m, server, _ := manager(t)
	upstream := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if !websocket.IsWebSocketUpgrade(r) {
			_, _ = io.WriteString(w, "ordinary")
			return
		}
		if r.Host != "example.test" || r.URL.RequestURI() != "/socket?q=1" || r.Header.Get("Cookie") != "user=one" || r.Header.Get("Origin") != "https://example.test" {
			t.Errorf("handshake %s %s %v", r.Host, r.URL, r.Header)
		}
		upgrader := websocket.Upgrader{CheckOrigin: func(*http.Request) bool { return true }, Subprotocols: []string{"chat"}}
		conn, err := upgrader.Upgrade(w, r, http.Header{"Set-Cookie": {"first=1; Path=/", "second=2; Path=/"}})
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
	defer upstream.Close()
	host := baseHost()
	host.Proxies = []service.ProxyRule{{Path: "/*", To: upstream.URL + "/"}}
	savePublish(t, m, host, 0)
	gateway := httptest.NewUnstartedServer(server.Handler())
	gateway.EnableHTTP2 = true
	gateway.StartTLS()
	defer gateway.Close()
	ordinary, _ := http.NewRequest("GET", gateway.URL+"/ordinary", nil)
	ordinary.Host = "example.test"
	response, err := gateway.Client().Do(ordinary)
	if err != nil {
		t.Fatal(err)
	}
	body, _ := io.ReadAll(response.Body)
	response.Body.Close()
	if response.ProtoMajor != 2 || string(body) != "ordinary" {
		t.Fatalf("ordinary %s %q", response.Proto, body)
	}
	tlsConfig := gateway.Client().Transport.(*http.Transport).TLSClientConfig.Clone()
	tlsConfig.NextProtos = []string{"http/1.1"}
	dialer := websocket.Dialer{TLSClientConfig: tlsConfig, HandshakeTimeout: time.Second, Subprotocols: []string{"chat"}}
	conn, handshake, err := dialer.Dial("wss"+strings.TrimPrefix(gateway.URL, "https")+"/socket?q=1", http.Header{"Host": {"example.test"}, "Origin": {"https://example.test"}, "Cookie": {"user=one"}})
	if err != nil {
		t.Fatal(err)
	}
	defer conn.Close()
	cookies := strings.Join(handshake.Header.Values("Set-Cookie"), "\n")
	if conn.Subprotocol() != "chat" || !strings.Contains(cookies, "first=1; Path=/") || !strings.Contains(cookies, "second=2; Path=/") {
		t.Fatal(handshake.Header)
	}
	_ = conn.SetReadDeadline(time.Now().Add(time.Second))
	if err = conn.WriteMessage(websocket.BinaryMessage, []byte("published")); err != nil {
		t.Fatal(err)
	}
	kind, data, err := conn.ReadMessage()
	if err != nil || kind != websocket.BinaryMessage || string(data) != "published" {
		t.Fatalf("echo %d %q %v", kind, data, err)
	}
	_ = conn.WriteControl(websocket.CloseMessage, websocket.FormatCloseMessage(1000, "done"), time.Now().Add(time.Second))
	_, _, err = conn.ReadMessage()
	if !websocket.IsCloseError(err, 1000) {
		t.Fatalf("close %v", err)
	}
}

func TestPublishedProxyRetainsWebSocketRejection(t *testing.T) {
	m, server, _ := manager(t)
	upstream := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Add("Set-Cookie", "rejected=1")
		w.WriteHeader(403)
		_, _ = io.WriteString(w, "denied")
	}))
	defer upstream.Close()
	host := baseHost()
	host.Proxies = []service.ProxyRule{{Path: "/*", To: upstream.URL + "/"}}
	savePublish(t, m, host, 0)
	gateway := httptest.NewServer(server.Handler())
	defer gateway.Close()
	_, response, err := websocket.DefaultDialer.Dial("ws"+strings.TrimPrefix(gateway.URL, "http")+"/socket", http.Header{"Host": {"example.test"}})
	if err == nil || response == nil {
		t.Fatalf("response %v %v", response, err)
	}
	defer response.Body.Close()
	body, _ := io.ReadAll(response.Body)
	if response.StatusCode != 403 || string(body) != "denied" || response.Header.Get("Set-Cookie") != "rejected=1" {
		t.Fatalf("response %v %q", response, body)
	}
}
