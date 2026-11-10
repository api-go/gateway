package gateway

import (
	"github.com/api-go/cast"
	"github.com/api-go/encoding"
	gohttp "github.com/api-go/http"
	"github.com/api-go/safe"
	"github.com/api-go/service"
	"context"
	"fmt"
	"io"
	"net"
	"net/http"
	"net/url"
	"strings"
	"time"
)

const affinityPeerPath = "/api/affinity/peer"

func (m *Manager) affinityAdvertise(address net.Addr) string {
	if m.config.AdminDisabled {
		return ""
	}
	if m.config.AffinityAdvertise != "" {
		return strings.TrimSuffix(m.config.AffinityAdvertise, "/")
	}
	if address == nil {
		return ""
	}
	host, port, err := net.SplitHostPort(address.String())
	if err != nil {
		return ""
	}
	if host == "" || host == "0.0.0.0" || host == "::" {
		host = service.GetServerIp()
	}
	return "http://" + net.JoinHostPort(host, port)
}
func (m *Manager) affinityPeerToken(path string) (string, error) {
	return m.seal([]byte(m.config.Prefix + "\n" + cast.String(time.Now().Unix()) + "\n" + path))
}
func (m *Manager) handleAffinityPeer(w http.ResponseWriter, r *http.Request) {
	if r.Method != "GET" {
		http.Error(w, "method not allowed", 405)
		return
	}
	plain, err := m.open(r.Header.Get("X-Gateway-Peer"))
	if err != nil {
		http.Error(w, "peer authentication required", 401)
		return
	}
	defer safe.ZeroMemory(plain)
	parts := strings.SplitN(string(plain), "\n", 3)
	if len(parts) != 3 || parts[0] != m.config.Prefix || parts[2] != r.URL.RequestURI() || absAffinityTime(time.Now().Unix()-cast.Int64(parts[1])) > 30 {
		http.Error(w, "invalid peer authentication", 401)
		return
	}
	s := m.affinity
	s.snapshotMu.Lock()
	defer s.snapshotMu.Unlock()
	if !s.ready.Load() {
		http.Error(w, "peer synchronization pending", 503)
		return
	}
	now := time.Now().Unix()
	for token, snapshot := range s.snapshots {
		if snapshot.Expires <= now {
			delete(s.snapshots, token)
		}
	}
	token := r.URL.Query().Get("token")
	offset := cast.Int(r.URL.Query().Get("offset"))
	var snapshot affinitySnapshot
	if token == "" {
		if len(s.snapshots) >= 2 {
			http.Error(w, "peer snapshots busy", 429)
			return
		}
		token = encoding.Hex(safe.MakeSafeToken(16))
		published := s.publishedCursor.Load()
		snapshot = affinitySnapshot{Cursor: published.ID, Marker: published.Marker, Entries: s.exportLocked(), Token: token, Expires: now + 120}
		s.snapshots[token] = snapshot
	} else {
		var ok bool
		snapshot, ok = s.snapshots[token]
		if !ok {
			http.Error(w, "snapshot expired", 409)
			return
		}
	}
	if offset < 0 || offset > len(snapshot.Entries) {
		http.Error(w, "invalid snapshot offset", 400)
		return
	}
	end := min(offset+512, len(snapshot.Entries))
	next := end
	if end == len(snapshot.Entries) {
		next = -1
	}
	page := affinitySnapshot{Cursor: snapshot.Cursor, Marker: snapshot.Marker, Entries: snapshot.Entries[offset:end], Token: token, Next: next, Expires: snapshot.Expires}
	raw, err := cast.ToJSONBytes(page)
	// Shard table events are larger than individual session bindings. Bound the
	// encoded page as well as its entry count, including encryption overhead.
	for err == nil && len(raw) > 512<<10 && end > offset+1 {
		end = offset + (end-offset)/2
		next = end
		page.Entries = snapshot.Entries[offset:end]
		page.Next = next
		raw, err = cast.ToJSONBytes(page)
	}
	if err != nil {
		http.Error(w, "snapshot encoding failed", 500)
		return
	}
	sealed, err := m.seal(raw)
	if err != nil {
		http.Error(w, "snapshot encryption failed", 500)
		return
	}
	w.Header().Set("Content-Type", "text/plain")
	w.Write([]byte(sealed))
	if next == -1 {
		delete(s.snapshots, token)
	}
}
func absAffinityTime(n int64) int64 {
	if n < 0 {
		return -n
	}
	return n
}
func (m *Manager) joinAffinityPeer(ctx context.Context, base string) error {
	parsed, err := url.Parse(base)
	if err != nil || (parsed.Scheme != "http" && parsed.Scheme != "https") || parsed.Host == "" || parsed.User != nil || parsed.RawQuery != "" || parsed.Fragment != "" {
		return fmt.Errorf("invalid peer address")
	}
	client := gohttp.NewClient(5 * time.Second)
	defer client.GetRawClient().CloseIdleConnections()
	client.GetRawClient().CheckRedirect = func(*http.Request, []*http.Request) error { return http.ErrUseLastResponse }
	token := ""
	offset := 0
	cursor := ""
	marker := ""
	count := 0
	for {
		path := affinityPeerPath
		if token != "" {
			path += "?token=" + url.QueryEscape(token) + "&offset=" + cast.String(offset)
		}
		auth, err := m.affinityPeerToken(path)
		if err != nil {
			return err
		}
		request, err := http.NewRequestWithContext(ctx, "GET", strings.TrimSuffix(base, "/")+path, nil)
		if err != nil {
			return err
		}
		request.Header.Set("X-Gateway-Peer", auth)
		response, err := client.GetRawClient().Do(request)
		if err != nil {
			return fmt.Errorf("peer snapshot unavailable")
		}
		data, readErr := io.ReadAll(io.LimitReader(response.Body, (1<<20)+1))
		response.Body.Close()
		if response.StatusCode != 200 || readErr != nil || len(data) > 1<<20 {
			return fmt.Errorf("peer snapshot rejected")
		}
		plain, err := m.open(string(data))
		if err != nil {
			return err
		}
		page, err := cast.FromJSON[affinitySnapshot](plain)
		safe.ZeroMemory(plain)
		if err != nil {
			return err
		}
		if token != "" && (page.Token != token || page.Cursor != cursor || page.Marker != marker) {
			return fmt.Errorf("peer snapshot changed")
		}
		token = page.Token
		cursor = page.Cursor
		marker = page.Marker
		for _, event := range page.Entries {
			if event.Table == nil && event.Load == nil {
				count++
			}
		}
		if count > m.affinity.capacity {
			return fmt.Errorf("peer snapshot exceeds capacity")
		}
		if err = m.affinity.apply(page.Entries); err != nil {
			return err
		}
		if page.Next == -1 {
			m.affinity.cursor = cursor
			m.affinity.cursorMarker = marker
			return nil
		}
		if page.Next <= offset {
			return fmt.Errorf("invalid peer pagination")
		}
		offset = page.Next
	}
}
