package gateway

import (
	"apigo.cc/go/cast"
	"apigo.cc/go/encoding"
	"apigo.cc/go/safe"
	"crypto/subtle"
	"embed"
	"errors"
	"fmt"
	"golang.org/x/crypto/bcrypt"
	"io"
	"io/fs"
	"net"
	"net/http"
	"net/url"
	"strings"
	"time"
)

//go:embed web/*
var adminFiles embed.FS

type adminSession struct {
	User    string
	Hash    string
	CSRF    string
	Expires time.Time
}
type loginInput struct {
	Password string
}
type editInput struct {
	Host        Host
	Revision    int64
	Version     int64
	Certificate string
	Key         string
}

func writeJSON(writer http.ResponseWriter, status int, value any) {
	encoded, err := cast.ToJSONBytes(value)
	if err != nil {
		http.Error(writer, "serialization failed", 500)
		return
	}
	writer.Header().Set("Content-Type", "application/json; charset=utf-8")
	writer.WriteHeader(status)
	writer.Write(encoded)
}
func readJSON(writer http.ResponseWriter, request *http.Request, value any) error {
	if !strings.HasPrefix(request.Header.Get("Content-Type"), "application/json") {
		return fmt.Errorf("application/json required")
	}
	request.Body = http.MaxBytesReader(writer, request.Body, 1<<20)
	defer request.Body.Close()
	data, err := io.ReadAll(request.Body)
	if err != nil {
		return err
	}
	defer safe.ZeroMemory(data)
	return cast.UnmarshalJSON(data, value)
}
func sameOrigin(request *http.Request) bool {
	origin := request.Header.Get("Origin")
	if origin == "" {
		return true
	}
	parsed, err := url.Parse(origin)
	if err != nil {
		return false
	}
	scheme := "http"
	if request.TLS != nil {
		scheme = "https"
	}
	return parsed.Scheme == scheme && strings.EqualFold(parsed.Host, request.Host)
}
func (m *Manager) login(writer http.ResponseWriter, request *http.Request) {
	if request.Method != "POST" || !sameOrigin(request) {
		http.Error(writer, "forbidden", 403)
		return
	}
	ip, _, _ := net.SplitHostPort(request.RemoteAddr)
	limit := &compiledLimit{rule: LimitRule{PeriodSeconds: 60, Requests: 20, Scope: "node"}}
	count, _, err := m.loginLimiter.check(limit, ip, time.Now())
	if err != nil || count > 20 {
		http.Error(writer, "login rate exceeded", 429)
		return
	}
	var input loginInput
	if err := readJSON(writer, request, &input); err != nil {
		http.Error(writer, err.Error(), 400)
		return
	}
	if len(input.Password) > 72 {
		http.Error(writer, "invalid credentials", 401)
		return
	}
	hash, err := m.adminHash()
	if err != nil {
		http.Error(writer, "credential store unavailable", 503)
		return
	}
	if hash == "" {
		http.Error(writer, "administrator password not set; run gateway admin-password", 423)
		return
	}
	password := []byte(input.Password)
	defer safe.ZeroMemory(password)
	if bcrypt.CompareHashAndPassword([]byte(hash), password) != nil {
		http.Error(writer, "invalid credentials", 401)
		return
	}
	token := encoding.Hex(safe.MakeSafeToken(32))
	session := adminSession{User: "admin", Hash: hash, CSRF: encoding.Hex(safe.MakeSafeToken(24)), Expires: time.Now().Add(8 * time.Hour)}
	m.mu.Lock()
	if len(m.sessions) >= 1000 {
		m.mu.Unlock()
		http.Error(writer, "session capacity exceeded", 503)
		return
	}
	m.sessions[token] = session
	m.mu.Unlock()
	http.SetCookie(writer, &http.Cookie{Name: "gateway-admin", Value: token, HttpOnly: true, Secure: request.TLS != nil, SameSite: http.SameSiteStrictMode, Path: "/", MaxAge: 28800})
	writeJSON(writer, 200, map[string]any{"csrf": session.CSRF})
}
func (m *Manager) authenticate(request *http.Request) (adminSession, bool) {
	cookie, err := request.Cookie("gateway-admin")
	if err != nil {
		return adminSession{}, false
	}
	m.mu.Lock()
	defer m.mu.Unlock()
	session, exists := m.sessions[cookie.Value]
	if !exists || time.Now().After(session.Expires) {
		delete(m.sessions, cookie.Value)
		return adminSession{}, false
	}
	hash, err := m.adminHash()
	if err != nil || hash == "" || subtle.ConstantTimeCompare([]byte(hash), []byte(session.Hash)) != 1 {
		delete(m.sessions, cookie.Value)
		return adminSession{}, false
	}
	return session, true
}

func (m *Manager) AdminHandler() http.Handler {
	assets, _ := fs.Sub(adminFiles, "web")
	static := http.FileServer(http.FS(assets))
	return http.HandlerFunc(func(writer http.ResponseWriter, request *http.Request) {
		writer.Header().Set("Cache-Control", "no-store")
		writer.Header().Set("X-Content-Type-Options", "nosniff")
		writer.Header().Set("X-Frame-Options", "DENY")
		writer.Header().Set("Content-Security-Policy", "default-src 'self'; script-src 'self'; style-src 'self'; img-src 'self' data:; connect-src 'self'; frame-ancestors 'none'; base-uri 'none'; form-action 'self'")
		if !strings.HasPrefix(request.URL.Path, "/api/") {
			static.ServeHTTP(writer, request)
			return
		}
		if request.URL.Path == "/api/login" {
			m.login(writer, request)
			return
		}
		session, ok := m.authenticate(request)
		if !ok {
			http.Error(writer, "login required", 401)
			return
		}
		if request.Method != "GET" {
			csrf := request.Header.Get("X-CSRF-Token")
			if !sameOrigin(request) || subtle.ConstantTimeCompare([]byte(csrf), []byte(session.CSRF)) != 1 {
				http.Error(writer, "invalid CSRF token", 403)
				return
			}

		}
		switch request.URL.Path {
		case "/api/session":
			if request.Method != "GET" {
				http.Error(writer, "method not allowed", 405)
				return
			}
			writeJSON(writer, 200, map[string]any{"csrf": session.CSRF})
			return
		case "/api/logout":
			if request.Method != "POST" {
				http.Error(writer, "method not allowed", 405)
				return
			}
			cookie, _ := request.Cookie("gateway-admin")
			m.mu.Lock()
			delete(m.sessions, cookie.Value)
			m.mu.Unlock()
			http.SetCookie(writer, &http.Cookie{Name: "gateway-admin", Value: "", Path: "/", MaxAge: -1, HttpOnly: true, SameSite: http.SameSiteStrictMode})
			writeJSON(writer, 200, map[string]bool{"ok": true})
			return
		case "/api/hosts":
			if request.Method != "GET" {
				http.Error(writer, "method not allowed", 405)
				return
			}
			if err := m.Reconcile(); err != nil {
				http.Error(writer, "configuration store unavailable", 503)
				return
			}
			writeJSON(writer, 200, m.Hosts())
			return
		case "/api/certificates", "/api/acme", "/api/certificates/save", "/api/certificates/delete", "/api/certificates/issue":
			m.handleCertificateLibrary(writer, request, session.User)
			return
		case "/api/events":
			if request.Method != "GET" {
				http.Error(writer, "method not allowed", 405)
				return
			}
			writeJSON(writer, 200, m.Events())
			return
		case "/api/nodes":
			if request.Method != "GET" {
				http.Error(writer, "method not allowed", 405)
				return
			}
			writeJSON(writer, 200, m.Nodes())
			return
		case "/api/services":
			if request.Method != "GET" {
				http.Error(writer, "method not allowed", 405)
				return
			}
			writeJSON(writer, 200, m.Services())
			return
		case "/api/settings":
			if request.Method != "GET" {
				http.Error(writer, "method not allowed", 405)
				return
			}
			writeJSON(writer, 200, m.Settings())
			return
		case "/api/export":
			if request.Method != "GET" {
				http.Error(writer, "method not allowed", 405)
				return
			}
			writer.Header().Set("Content-Disposition", "attachment; filename=gateway-hosts.json")
			writeJSON(writer, 200, m.Hosts())
			return
		case "/api/preview":
			m.handlePreview(writer, request)
			return
		}
		if request.Method != "POST" {
			http.Error(writer, "method not allowed", 405)
			return
		}
		var input editInput
		if err := readJSON(writer, request, &input); err != nil {
			http.Error(writer, err.Error(), 400)
			return
		}
		var result any
		var err error
		switch request.URL.Path {
		case "/api/draft":
			result, err = m.SaveDraft(input.Host, input.Revision, session.User)
		case "/api/publish":
			result, err = m.Publish(input.Host.ID, input.Revision, session.User)
		case "/api/rollback":
			result, err = m.Rollback(input.Host.ID, input.Version, input.Revision, session.User)
		case "/api/delete":
			err = m.Delete(input.Host.ID, input.Revision, session.User)
			result = map[string]bool{"ok": err == nil}
		case "/api/certificate":
			result, err = m.SetCertificate(input.Host.ID, input.Revision, input.Certificate, []byte(input.Key), session.User)
		default:
			http.NotFound(writer, request)
			return
		}
		if err != nil {
			status := 400
			if errors.Is(err, ErrConflict) {
				status = 409
			}
			writeJSON(writer, status, map[string]string{"error": err.Error()})
			return
		}
		writeJSON(writer, 200, result)
	})
}
