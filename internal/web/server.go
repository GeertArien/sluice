// Package web is the operator UI and webhook endpoint: session auth for a
// single admin, CSRF-protected POSTs, and secret-authenticated webhooks
// (spec §3, §8, §9.6).
package web

import (
	"crypto/rand"
	"crypto/subtle"
	"embed"
	"encoding/hex"
	"fmt"
	"html/template"
	"log"
	"net/http"
	"strings"
	"sync"
	"time"

	"github.com/geertarien/sluice/internal/jobs"
	"github.com/geertarien/sluice/internal/secrets"
	"github.com/geertarien/sluice/internal/store"
)

//go:embed templates/*.html static/*
var assets embed.FS

type Server struct {
	Store         *store.Store
	Box           *secrets.Box
	Jobs          *jobs.Service
	AdminPassword string

	tmpl     *template.Template
	mux      *http.ServeMux
	sessions map[string]*session
	sessMu   sync.Mutex
}

type session struct {
	expires time.Time
	csrf    string
}

func randHex(n int) string {
	b := make([]byte, n)
	if _, err := rand.Read(b); err != nil {
		panic(err)
	}
	return hex.EncodeToString(b)
}

func NewServer(st *store.Store, box *secrets.Box, js *jobs.Service, adminPassword string) (*Server, error) {
	funcs := template.FuncMap{
		"short": func(sha string) string {
			if len(sha) > 12 {
				return sha[:12]
			}
			return sha
		},
		"timefmt": func(t *time.Time) string {
			if t == nil {
				return "never"
			}
			return t.Local().Format("2006-01-02 15:04:05")
		},
		"join": strings.Join,
		"deref": func(b *bool) bool {
			return b != nil && *b
		},
		"deref_i": func(i *int64) int64 {
			if i == nil {
				return 0
			}
			return *i
		},
		"dur": func(a, b *time.Time) string {
			if a == nil || b == nil {
				return ""
			}
			return b.Sub(*a).Round(time.Millisecond).String()
		},
	}
	tmpl, err := template.New("").Funcs(funcs).ParseFS(assets, "templates/*.html")
	if err != nil {
		return nil, err
	}
	s := &Server{
		Store: st, Box: box, Jobs: js, AdminPassword: adminPassword,
		tmpl: tmpl, mux: http.NewServeMux(), sessions: map[string]*session{},
	}
	s.routes()
	return s, nil
}

func (s *Server) routes() {
	s.mux.Handle("GET /static/", http.FileServer(http.FS(assets)))
	s.mux.HandleFunc("GET /login", s.handleLoginForm)
	s.mux.HandleFunc("POST /login", s.handleLogin)
	s.mux.HandleFunc("POST /logout", s.auth(s.handleLogout))
	s.mux.HandleFunc("GET /{$}", s.auth(s.handleDashboard))
	s.mux.HandleFunc("GET /bridges/new", s.auth(s.handleBridgeNewForm))
	s.mux.HandleFunc("POST /bridges", s.auth(s.handleBridgeCreate))
	s.mux.HandleFunc("GET /bridges/{slug}", s.auth(s.handleBridgeDetail))
	s.mux.HandleFunc("GET /bridges/{slug}/settings", s.auth(s.handleBridgeSettingsForm))
	s.mux.HandleFunc("POST /bridges/{slug}/settings", s.auth(s.handleBridgeSettings))
	s.mux.HandleFunc("POST /bridges/{slug}/delete", s.auth(s.handleBridgeDelete))
	s.mux.HandleFunc("POST /bridges/{slug}/sync", s.auth(s.handleEnqueue("sync")))
	s.mux.HandleFunc("POST /bridges/{slug}/verify", s.auth(s.handleEnqueue("verify")))
	s.mux.HandleFunc("POST /bridges/{slug}/init", s.auth(s.handleEnqueue("init")))
	s.mux.HandleFunc("POST /bridges/{slug}/activate", s.auth(s.handleActivate))
	s.mux.HandleFunc("POST /bridges/{slug}/pause", s.auth(s.handlePause))
	s.mux.HandleFunc("GET /bridges/{slug}/preflight", s.auth(s.handlePreflight))
	s.mux.HandleFunc("POST /bridges/{slug}/promote", s.auth(s.handlePromote))
	s.mux.HandleFunc("POST /promotions/{id}/finalize", s.auth(s.handlePromotionFinalize))
	s.mux.HandleFunc("POST /promotions/{id}/abort", s.auth(s.handlePromotionAbort))
	s.mux.HandleFunc("POST /promotions/{id}/mark-promoted", s.auth(s.handlePromotionMarkPromoted))
	s.mux.HandleFunc("GET /jobs/{id}", s.auth(s.handleJobDetail))
	s.mux.HandleFunc("GET /jobs/{id}/log", s.auth(s.handleJobLog))
	s.mux.HandleFunc("GET /audit", s.auth(s.handleAudit))
	s.mux.HandleFunc("GET /keys", s.auth(s.handleSSHKeys))
	s.mux.HandleFunc("POST /keys", s.auth(s.handleSSHKeyCreate))
	s.mux.HandleFunc("POST /keys/{id}/delete", s.auth(s.handleSSHKeyDelete))
	s.mux.HandleFunc("GET /tokens", s.auth(s.handleGiteaTokens))
	s.mux.HandleFunc("POST /tokens", s.auth(s.handleGiteaTokenCreate))
	s.mux.HandleFunc("POST /tokens/{id}/delete", s.auth(s.handleGiteaTokenDelete))
	s.mux.HandleFunc("GET /hosts", s.auth(s.handleHosts))
	s.mux.HandleFunc("POST /hosts/scan", s.auth(s.handleHostScan))
	s.mux.HandleFunc("POST /hosts", s.auth(s.handleHostTrust))
	s.mux.HandleFunc("POST /hosts/{id}/delete", s.auth(s.handleHostDelete))
	s.mux.HandleFunc("POST /hooks/{slug}", s.handleWebhook) // secret-authenticated, no session
}

func (s *Server) Handler() http.Handler { return s.mux }

// ---------- auth & CSRF ----------

const sessionCookie = "sluice_session"

func (s *Server) currentSession(r *http.Request) (*session, string) {
	c, err := r.Cookie(sessionCookie)
	if err != nil {
		return nil, ""
	}
	s.sessMu.Lock()
	defer s.sessMu.Unlock()
	sess := s.sessions[c.Value]
	if sess == nil || time.Now().After(sess.expires) {
		delete(s.sessions, c.Value)
		return nil, ""
	}
	return sess, c.Value
}

// auth wraps handlers with session + CSRF checks (CSRF on all POSTs, §9.6).
func (s *Server) auth(next http.HandlerFunc) http.HandlerFunc {
	return func(w http.ResponseWriter, r *http.Request) {
		sess, _ := s.currentSession(r)
		if sess == nil {
			http.Redirect(w, r, "/login", http.StatusSeeOther)
			return
		}
		if r.Method == http.MethodPost {
			if err := r.ParseForm(); err != nil ||
				subtle.ConstantTimeCompare([]byte(r.PostFormValue("csrf")), []byte(sess.csrf)) != 1 {
				http.Error(w, "CSRF token mismatch", http.StatusForbidden)
				return
			}
		}
		next(w, r)
	}
}

func (s *Server) csrfToken(r *http.Request) string {
	sess, _ := s.currentSession(r)
	if sess == nil {
		return ""
	}
	return sess.csrf
}

func (s *Server) handleLoginForm(w http.ResponseWriter, r *http.Request) {
	s.render(w, "login.html", map[string]any{"Error": ""})
}

func (s *Server) handleLogin(w http.ResponseWriter, r *http.Request) {
	if s.AdminPassword == "" {
		http.Error(w, "SLUICE_ADMIN_PASSWORD is not configured", http.StatusServiceUnavailable)
		return
	}
	if subtle.ConstantTimeCompare([]byte(r.PostFormValue("password")), []byte(s.AdminPassword)) != 1 {
		time.Sleep(500 * time.Millisecond)
		s.render(w, "login.html", map[string]any{"Error": "wrong password"})
		return
	}
	token := randHex(32)
	s.sessMu.Lock()
	s.sessions[token] = &session{expires: time.Now().Add(12 * time.Hour), csrf: randHex(32)}
	s.sessMu.Unlock()
	http.SetCookie(w, &http.Cookie{
		Name: sessionCookie, Value: token, Path: "/",
		HttpOnly: true, SameSite: http.SameSiteLaxMode,
		Expires: time.Now().Add(12 * time.Hour),
	})
	s.Store.Audit(0, "admin", "login", nil)
	http.Redirect(w, r, "/", http.StatusSeeOther)
}

func (s *Server) handleLogout(w http.ResponseWriter, r *http.Request) {
	if _, token := s.currentSession(r); token != "" {
		s.sessMu.Lock()
		delete(s.sessions, token)
		s.sessMu.Unlock()
	}
	http.SetCookie(w, &http.Cookie{Name: sessionCookie, Value: "", Path: "/", MaxAge: -1})
	http.Redirect(w, r, "/login", http.StatusSeeOther)
}

// ---------- webhook (spec §5.3, §9.6) ----------

func (s *Server) handleWebhook(w http.ResponseWriter, r *http.Request) {
	bridge, err := s.Store.BridgeBySlug(r.PathValue("slug"))
	if err != nil {
		http.NotFound(w, r)
		return
	}
	provided := r.Header.Get("X-Sluice-Secret")
	if provided == "" {
		provided = r.URL.Query().Get("secret")
	}
	want := ""
	if len(bridge.WebhookSecretEnc) > 0 {
		if want, err = s.Box.Decrypt(bridge.WebhookSecretEnc); err != nil {
			http.Error(w, "server error", http.StatusInternalServerError)
			return
		}
	}
	if want == "" || subtle.ConstantTimeCompare([]byte(provided), []byte(want)) != 1 {
		http.Error(w, "forbidden", http.StatusForbidden)
		return
	}
	if bridge.Status != "active" {
		http.Error(w, "bridge is not active", http.StatusConflict)
		return
	}
	s.Jobs.WebhookSync(bridge.ID)
	w.WriteHeader(http.StatusAccepted)
	fmt.Fprintln(w, "sync scheduled")
}

// ---------- rendering ----------

func (s *Server) render(w http.ResponseWriter, name string, data map[string]any) {
	w.Header().Set("Content-Type", "text/html; charset=utf-8")
	if err := s.tmpl.ExecuteTemplate(w, name, data); err != nil {
		log.Printf("render %s: %v", name, err)
		http.Error(w, "template error: "+err.Error(), http.StatusInternalServerError)
	}
}

func (s *Server) renderPage(w http.ResponseWriter, r *http.Request, name string, data map[string]any) {
	if data == nil {
		data = map[string]any{}
	}
	data["CSRF"] = s.csrfToken(r)
	s.render(w, name, data)
}

func httpError(w http.ResponseWriter, code int, format string, args ...any) {
	http.Error(w, fmt.Sprintf(format, args...), code)
}
