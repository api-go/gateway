package gateway_test

import (
	"apigo.cc/go/cast"
	"apigo.cc/go/file"
	gateway "apigo.cc/go/gateway/core"
	"apigo.cc/go/log"
	"apigo.cc/go/redis"
	"apigo.cc/go/service"
	"errors"
	"fmt"
	"net/http/httptest"
	"os"
	"path/filepath"
	"strings"
	"sync"
	"testing"
	"time"
)

func TestHostTrashRestoreAndPurge(t *testing.T) {
	m, server, config := manager(t)
	host := baseHost()
	host.Rewrites = []service.RewriteRule{{Path: "/", To: "https://published.test/"}}
	published := savePublish(t, m, host, 0)
	host.Rewrites[0].To = "https://draft.test/"
	draft, err := m.SaveDraft(host, published.Revision, "editor")
	if err != nil {
		t.Fatal(err)
	}
	if err = m.Purge(host.ID, draft.Revision, "editor"); err == nil {
		t.Fatal("purge accepted an active Host")
	}
	if err = m.Delete(host.ID, draft.Revision-1, "editor"); !errors.Is(err, gateway.ErrConflict) {
		t.Fatal("stale deletion", err)
	}
	if err = m.Delete(host.ID, draft.Revision, "editor"); err != nil {
		t.Fatal(err)
	}
	if len(m.Hosts()) != 0 || len(m.Trash()) != 1 {
		t.Fatal("delete did not move Host")
	}
	response := httptest.NewRecorder()
	server.Handler().ServeHTTP(response, httptest.NewRequest("GET", "http://example.test/", nil))
	if response.Header().Get("Location") != "" {
		t.Fatal("trashed Host still serving")
	}
	item := m.Trash()[0]
	if item.Record.Revision <= draft.Revision || item.DeletedBy != "editor" || item.DeletedAt.IsZero() {
		t.Fatal("deletion metadata", item)
	}
	if _, err = m.SaveDraft(host, 0, "stale-editor"); err == nil {
		t.Fatal("trash ID silently recreated")
	}
	if _, err = m.Publish(host.ID, draft.Revision, "editor"); err == nil {
		t.Fatal("published trashed Host")
	}
	if _, err = m.Restore(host.ID, draft.Revision, "editor"); !errors.Is(err, gateway.ErrConflict) {
		t.Fatal("stale restore", err)
	}
	// Reading the bin cannot mutate retained records.
	item.Record.Draft.Domains[0] = "mutated.test"
	if m.Trash()[0].Record.Draft.Domains[0] != "example.test" {
		t.Fatal("trash view aliases state")
	}
	restarted, err := gateway.NewManager(config, service.NewWebServer())
	if err != nil {
		t.Fatal(err)
	}
	defer restarted.Close()
	if len(restarted.Trash()) != 1 || len(restarted.Hosts()) != 0 {
		t.Fatal("trash did not survive restart")
	}
	restored, err := restarted.Restore(host.ID, m.Trash()[0].Record.Revision, "restorer")
	if err != nil {
		t.Fatal(err)
	}
	if restored.Current.Rewrites[0].To != "https://published.test/" || restored.Draft.Rewrites[0].To != "https://draft.test/" {
		t.Fatal("restoration published pending draft")
	}
	if restored.Version != published.Version+1 || restored.Revision <= m.Trash()[0].Record.Revision || len(restored.History) != 2 {
		t.Fatal("restore lost history or monotonic revisions")
	}
	response = httptest.NewRecorder()
	restarted.Handler().ServeHTTP(response, httptest.NewRequest("GET", "http://example.test/", nil))
	if response.Header().Get("Location") != "https://published.test/" {
		t.Fatal("restore did not reactivate prior publication")
	}
	if err = restarted.Delete(host.ID, restored.Revision, "editor"); err != nil {
		t.Fatal(err)
	}
	item = restarted.Trash()[0]
	if err = restarted.Purge(host.ID, item.Record.Revision-1, "editor"); !errors.Is(err, gateway.ErrConflict) {
		t.Fatal("stale purge", err)
	}
	if err = restarted.Purge(host.ID, item.Record.Revision, "editor"); err != nil {
		t.Fatal(err)
	}
	if len(restarted.Trash()) != 0 {
		t.Fatal("purge retained item")
	}
	if _, err = restarted.Restore(host.ID, item.Record.Revision, "editor"); err == nil {
		t.Fatal("purged item restored")
	}
	if err = m.Reconcile(); err != nil {
		t.Fatal(err)
	}
	if len(m.Trash()) != 0 || len(m.Hosts()) != 0 {
		t.Fatal("purge not persisted")
	}
	found := map[string]bool{}
	for _, event := range m.Events() {
		found[event.Action] = true
	}
	if !found["trash"] || !found["restore"] || !found["purge"] {
		t.Fatal("missing audit lifecycle", found)
	}
}

func TestHostTrashDraftAndDomainConflict(t *testing.T) {
	m, _, _ := manager(t)
	host := baseHost()
	draft, err := m.SaveDraft(host, 0, "test")
	if err != nil {
		t.Fatal(err)
	}
	if err = m.Delete(host.ID, draft.Revision, "test"); err != nil {
		t.Fatal(err)
	}
	restored, err := m.Restore(host.ID, m.Trash()[0].Record.Revision, "test")
	if err != nil {
		t.Fatal(err)
	}
	if restored.Current != nil || restored.Version != 0 {
		t.Fatal("draft-only restoration affected publication")
	}
	published, err := m.Publish(host.ID, restored.Revision, "test")
	if err != nil {
		t.Fatal(err)
	}
	if err = m.Delete(host.ID, published.Revision, "test"); err != nil {
		t.Fatal(err)
	}
	item := m.Trash()[0]
	other := host
	other.ID = "new-owner"
	savePublish(t, m, other, 0)
	if _, err = m.Restore(host.ID, item.Record.Revision, "test"); err == nil {
		t.Fatal("restore stole domain")
	}
	if len(m.Trash()) != 1 || m.Trash()[0].Record.Revision != item.Record.Revision || len(m.Hosts()) != 1 {
		t.Fatal("failed restore mutated state")
	}
}

func TestHostTrashRetainsEncryptedCredentials(t *testing.T) {
	m, _, config := manager(t)
	host := baseHost()
	host.Monitor = gateway.MonitorConfig{Configured: true, Headers: map[string]string{"Authorization": "test-private-header"}}
	record, err := m.SaveDraft(host, 0, "test")
	if err != nil {
		t.Fatal(err)
	}
	cert, key := certificate(t, "example.test")
	record, err = m.SetCertificate(host.ID, record.Revision, string(cert), key, "test")
	if err != nil {
		t.Fatal(err)
	}
	record, err = m.Publish(host.ID, record.Revision, "test")
	if err != nil {
		t.Fatal(err)
	}
	if err = m.Delete(host.ID, record.Revision, "test"); err != nil {
		t.Fatal(err)
	}
	item := m.Trash()[0]
	encoded, _ := cast.ToJSON(item)
	if strings.Contains(encoded, "test-private-header") || strings.Contains(encoded, "PRIVATE KEY") || item.Record.Draft.HTTPS.SealedKey != "" || item.Record.Current.Monitor.SealedHeaders != "" {
		t.Fatal("trash exposed credentials")
	}
	raw, err := file.ReadBytes(filepath.Join(config.DataDir, "state.json"))
	if err != nil {
		t.Fatal(err)
	}
	state, err := cast.FromJSON[gateway.State](raw)
	if err != nil {
		t.Fatal(err)
	}
	if state.Trash[host.ID].Record.Draft.HTTPS.SealedKey == "" || state.Trash[host.ID].Record.Draft.Monitor.SealedHeaders == "" {
		t.Fatal("credentials not retained encrypted")
	}
	wrong := config
	wrong.CertificateKey = strings.Repeat("11", 32)
	if broken, err := gateway.NewManager(wrong, service.NewWebServer()); err == nil {
		broken.Close()
		t.Fatal("wrong key accepted with credentials only in trash")
	}
	restarted, err := gateway.NewManager(config, service.NewWebServer())
	if err != nil {
		t.Fatal(err)
	}
	defer restarted.Close()
	restored, err := restarted.Restore(host.ID, item.Record.Revision, "test")
	if err != nil {
		t.Fatal(err)
	}
	if !restored.Draft.Monitor.HasHeaders || restored.Current.HTTPS.Certificate == "" {
		t.Fatal("restored credential state lost")
	}
}

func TestHostTrashAdminAndScopedAPI(t *testing.T) {
	m, _, _ := manager(t)
	host := baseHost()
	record := savePublish(t, m, host, 0)
	h := m.AdminHandler()
	cookie, csrf := login(t, h, "admin", "gateway-test-password")
	body := map[string]any{"host": map[string]string{"id": host.ID}, "revision": record.Revision}
	if w := adminRequest(t, h, "/api/delete", body, cookie, ""); w.Code != 403 {
		t.Fatal("missing CSRF accepted")
	}
	if w := adminRequest(t, h, "/api/delete", body, cookie, csrf); w.Code != 200 {
		t.Fatal(w.Code, w.Body.String())
	}
	if w := adminRequest(t, h, "/api/trash", nil, nil, ""); w.Code != 401 {
		t.Fatal("anonymous bin readable")
	}
	if w := adminRequest(t, h, "/api/trash", nil, cookie, csrf); w.Code != 200 || !strings.Contains(w.Body.String(), host.ID) {
		t.Fatal("admin bin", w.Code, w.Body.String())
	}
	create := func(ids, permissions []string) string {
		listing := adminRequest(t, h, "/api/keys", nil, cookie, csrf)
		state, _ := cast.FromJSON[map[string]any](listing.Body.Bytes())
		w := adminRequest(t, h, "/api/keys/create", map[string]any{"name": "trash-test", "hostIDs": ids, "permissions": permissions, "revision": state["revision"]}, cookie, csrf)
		if w.Code != 200 {
			t.Fatal(w.Code, w.Body.String())
		}
		result, _ := cast.FromJSON[map[string]any](w.Body.Bytes())
		return cast.String(result["token"])
	}
	reader := create([]string{host.ID}, []string{"read"})
	other := create([]string{"other"}, []string{"read", "delete", "publish"})
	deleter := create([]string{host.ID}, []string{"read", "delete"})
	publisher := create([]string{host.ID}, []string{"read", "publish"})
	if w := machineRequest(t, h, "/api/v1/trash", reader, nil); w.Code != 200 || !strings.Contains(w.Body.String(), host.ID) {
		t.Fatal("scoped trash read", w.Code)
	}
	if w := machineRequest(t, h, "/api/v1/trash", other, nil); strings.Contains(w.Body.String(), host.ID) {
		t.Fatal("other Host leaked")
	}
	body["revision"] = m.Trash()[0].Record.Revision
	for _, action := range []string{"restore", "purge"} {
		if w := machineRequest(t, h, "/api/v1/"+action, reader, body); w.Code != 403 {
			t.Fatal("readonly mutation accepted", action, w.Code)
		}
		if w := machineRequest(t, h, "/api/v1/"+action, other, body); w.Code != 403 {
			t.Fatal("cross Host mutation accepted", action, w.Code)
		}
	}
	if w := machineRequest(t, h, "/api/v1/restore", deleter, body); w.Code != 403 {
		t.Fatal("delete-only key activated traffic")
	}
	if w := machineRequest(t, h, "/api/v1/restore", publisher, body); w.Code != 200 {
		t.Fatal(w.Code, w.Body.String())
	}
	record = m.Hosts()[0]
	body["revision"] = record.Revision
	if w := machineRequest(t, h, "/api/v1/delete", deleter, body); w.Code != 200 {
		t.Fatal(w.Code, w.Body.String())
	}
	body["revision"] = m.Trash()[0].Record.Revision
	if w := adminRequest(t, h, "/api/restore", body, cookie, ""); w.Code != 403 {
		t.Fatal("restore missing CSRF accepted")
	}
	if w := adminRequest(t, h, "/api/purge", body, cookie, ""); w.Code != 403 {
		t.Fatal("purge missing CSRF accepted")
	}
	if w := machineRequest(t, h, "/api/v1/purge", deleter, body); w.Code != 200 {
		t.Fatal(w.Code, w.Body.String())
	}
}

func TestHostTrashRedisConvergenceAndConcurrentRestore(t *testing.T) {
	if os.Getenv("GATEWAY_REDIS_INTEGRATION") != "1" {
		t.Skip("set GATEWAY_REDIS_INTEGRATION=1")
	}
	rd := redis.GetRedis("127.0.0.1:6379", log.DefaultLogger)
	prefix := fmt.Sprintf("gateway-trash-%d", time.Now().UnixNano())
	t.Cleanup(func() {
		for _, key := range rd.Do("KEYS", prefix+":*").Strings() {
			rd.Do("DEL", key)
		}
	})
	config := gateway.Config{Redis: "127.0.0.1:6379", Prefix: prefix, DataDir: t.TempDir(), AdminDisabled: true, NodeID: "trash-one"}
	one, err := gateway.NewManager(config, service.NewWebServer())
	if err != nil {
		t.Fatal(err)
	}
	defer one.Close()
	config.DataDir = t.TempDir()
	config.NodeID = "trash-two"
	two, err := gateway.NewManager(config, service.NewWebServer())
	if err != nil {
		t.Fatal(err)
	}
	defer two.Close()
	host := baseHost()
	record := savePublish(t, one, host, 0)
	if err = one.Delete(host.ID, record.Revision, "test"); err != nil {
		t.Fatal(err)
	}
	if err = two.Reconcile(); err != nil {
		t.Fatal(err)
	}
	if len(two.Hosts()) != 0 || len(two.Trash()) != 1 {
		t.Fatal("bin not synchronized")
	}
	// Recover the complete retained bin from a node's snapshot if shared state is lost.
	rd.Do("DEL", prefix+":state")
	if err = two.Reconcile(); err != nil {
		t.Fatal(err)
	}
	if len(two.Trash()) != 1 || rd.Do("GET", prefix+":state").String() == "" {
		t.Fatal("snapshot recovery lost retained trash")
	}
	revision := two.Trash()[0].Record.Revision
	var wg sync.WaitGroup
	results := make(chan error, 2)
	for _, m := range []*gateway.Manager{one, two} {
		wg.Add(1)
		go func() { defer wg.Done(); _, err := m.Restore(host.ID, revision, "test"); results <- err }()
	}
	wg.Wait()
	close(results)
	successes := 0
	for err := range results {
		if err == nil {
			successes++
		}
	}
	if successes != 1 {
		t.Fatal("concurrent restore winners", successes)
	}
	for _, m := range []*gateway.Manager{one, two} {
		if err = m.Reconcile(); err != nil {
			t.Fatal(err)
		}
	}
	if len(two.Trash()) != 0 || len(one.Hosts()) != 1 || len(two.Hosts()) != 1 {
		t.Fatal("restore not converged")
	}
	record = one.Hosts()[0]
	if err = one.Delete(host.ID, record.Revision, "test"); err != nil {
		t.Fatal(err)
	}
	if err = two.Reconcile(); err != nil {
		t.Fatal(err)
	}
	revision = two.Trash()[0].Record.Revision
	if err = two.Purge(host.ID, revision, "test"); err != nil {
		t.Fatal(err)
	}
	if err = one.Reconcile(); err != nil {
		t.Fatal(err)
	}
	if len(one.Trash()) != 0 {
		t.Fatal("purge not synchronized")
	}
	// A missing shared state must recover the purged snapshot, without resurrecting it.
	rd.Do("DEL", prefix+":state")
	if err = one.Reconcile(); err != nil {
		t.Fatal(err)
	}
	if len(one.Trash()) != 0 || len(one.Hosts()) != 0 {
		t.Fatal("recovery resurrected purged Host")
	}
}

func BenchmarkHostTrashListing(b *testing.B) {
	m, err := gateway.NewManager(gateway.Config{DataDir: b.TempDir(), AdminDisabled: true}, service.NewWebServer())
	if err != nil {
		b.Fatal(err)
	}
	defer m.Close()
	for index := range 32 {
		host := baseHost()
		host.ID = fmt.Sprintf("trash-%d", index)
		host.Domains = []string{fmt.Sprintf("trash-%d.test", index)}
		record, err := m.SaveDraft(host, 0, "bench")
		if err != nil {
			b.Fatal(err)
		}
		if err = m.Delete(host.ID, record.Revision, "bench"); err != nil {
			b.Fatal(err)
		}
	}
	b.ReportAllocs()
	b.ResetTimer()
	for b.Loop() {
		if len(m.Trash()) != 32 {
			b.Fatal("missing records")
		}
	}
}
