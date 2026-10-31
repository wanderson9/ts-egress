package setup

import (
	"context"
	"encoding/json"
	"errors"
	"io"
	"net"
	"net/http"
	"strconv"
	"strings"
	"sync"
	"time"

	"golang.org/x/crypto/bcrypt"
	"ts-egress/internal/gateway"
	"ts-egress/internal/ui"
)

type Status struct {
	State    string `json:"state"`
	AuthURL  string `json:"authURL,omitempty"`
	AdminURL string `json:"adminURL,omitempty"`
	Error    string `json:"error,omitempty"`
	Peers    []Peer `json:"peers"`
	MagicDNS bool   `json:"magicDNS"`
	HTTPS    bool   `json:"https"`
}
type Peer struct {
	Name   string   `json:"name"`
	IPs    []string `json:"ips"`
	Online bool     `json:"online"`
}
type Runtime interface {
	Connect()
	Status(context.Context) Status
}
type session struct{ expires time.Time }
type tailnetKey struct{}
type sessionKey struct{}

// Tailnet is used only behind the gateway's Host + WhoIs authorization wrapper.
func (s *Server) Tailnet() http.Handler {
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		s.ServeHTTP(w, r.WithContext(context.WithValue(r.Context(), tailnetKey{}, true)))
	})
}

type Server struct {
	store         *Store
	runtime       Runtime
	gateway       *gateway.Gateway
	routesPath    string
	mu            sync.Mutex
	credentialsMu sync.Mutex // Serializes validation/issuance with password changes/revocation.
	sessions      map[string]session
	nextLogin     time.Time
}

func New(store *Store, runtime Runtime, g *gateway.Gateway, routesPath string) *Server {
	return &Server{store: store, runtime: runtime, gateway: g, routesPath: routesPath, sessions: map[string]session{}}
}
func (s *Server) authenticated(r *http.Request) bool {
	c, err := r.Cookie("ts_egress_session")
	if err != nil {
		return false
	}
	s.mu.Lock()
	defer s.mu.Unlock()
	v, ok := s.sessions[c.Value]
	if ok && time.Now().Before(v.expires) {
		return true
	}
	delete(s.sessions, c.Value)
	return false
}
func (s *Server) issue(w http.ResponseWriter) {
	s.mu.Lock()
	defer s.mu.Unlock()
	// Bound session memory, and invalidate previous sessions on a new login.
	t := token()
	s.sessions = map[string]session{t: {expires: time.Now().Add(8 * time.Hour)}}
	http.SetCookie(w, &http.Cookie{Name: "ts_egress_session", Value: t, Path: "/", HttpOnly: true, SameSite: http.SameSiteStrictMode, MaxAge: 8 * 3600})
}
func decode(w http.ResponseWriter, r *http.Request, v any) error {
	r.Body = http.MaxBytesReader(w, r.Body, 16*1024)
	d := json.NewDecoder(r.Body)
	d.DisallowUnknownFields()
	if err := d.Decode(v); err != nil {
		return err
	}
	if d.Decode(new(any)) != io.EOF {
		return errors.New("unexpected trailing JSON")
	}
	return nil
}
func localHost(host string) bool {
	h, _, err := net.SplitHostPort(host)
	if err != nil {
		return false
	}
	return h == "127.0.0.1" || h == "localhost" || h == "::1"
}
func (s *Server) ServeHTTP(w http.ResponseWriter, r *http.Request) {
	private, _ := r.Context().Value(tailnetKey{}).(bool)
	if !private && r.URL.Path == "/settings" {
		http.Error(w, "settings are available through the tailnet dashboard", http.StatusForbidden)
		return
	}
	w.Header().Set("Cache-Control", "no-store")
	w.Header().Set("X-Content-Type-Options", "nosniff")
	w.Header().Set("Referrer-Policy", "no-referrer")
	ui.SetSecurityPolicy(w)
	// Docker DNAT changes RemoteAddr; the host port must be published on loopback.
	// Reject non-loopback Host headers as a second layer, including DNS rebinding.
	if !private && !localHost(r.Host) {
		http.Error(w, "use localhost or an SSH tunnel for the setup console", 403)
		return
	}
	origin := "http://" + r.Host
	if private {
		origin = "https://" + r.Host
	}
	if r.Method != "GET" && r.Method != "HEAD" {
		if r.Header.Get("Origin") != origin || r.Header.Get("X-TS-Egress") != "1" || r.Header.Get("Content-Type") != "application/json" {
			http.Error(w, "invalid request origin", 403)
			return
		}
	}
	if r.Method == "GET" && r.URL.Path == "/api/setup/session" {
		w.Header().Set("Content-Type", "application/json")
		json.NewEncoder(w).Encode(map[string]bool{"claimed": s.store.Get().PasswordHash != "", "authenticated": private || s.authenticated(r), "tailnet": private})
		return
	}
	if r.Method == http.MethodOptions {
		w.WriteHeader(http.StatusNoContent)
		return
	}
	// Seal is persistent: a process restart or temporary tailnet outage must not
	// reopen local management. The local console retains only login, handoff
	// status and reauthentication, never service/configuration writes.
	if !private && s.store.Get().TailnetOnly && (r.URL.Path == "/dashboard" || r.URL.Path == "/api/status" || r.URL.Path == "/api/routes" || r.URL.Path == "/api/activity" || r.URL.Path == "/api/certificate/retry" || r.URL.Path == "/api/setup/settings") {
		http.Error(w, "management is available only through the tailnet", http.StatusForbidden)
		return
	}
	if r.Method == "POST" && (r.URL.Path == "/api/setup/claim" || r.URL.Path == "/api/setup/login") {
		if private {
			http.Error(w, "use Tailscale identity", 403)
			return
		}
		s.mu.Lock()
		if time.Now().Before(s.nextLogin) {
			s.mu.Unlock()
			http.Error(w, "try again shortly", 429)
			return
		}
		s.nextLogin = time.Now().Add(time.Second)
		s.mu.Unlock()
		var body struct {
			Password string `json:"password"`
		}
		if decode(w, r, &body) != nil {
			http.Error(w, "invalid JSON", 400)
			return
		}
		s.credentialsMu.Lock()
		defer s.credentialsMu.Unlock()
		if r.URL.Path == "/api/setup/claim" {
			if err := s.store.Claim(body.Password); err != nil {
				http.Error(w, err.Error(), 400)
				return
			}
		} else {
			if len(body.Password) > 72 || bcrypt.CompareHashAndPassword([]byte(s.store.Get().PasswordHash), []byte(body.Password)) != nil {
				http.Error(w, "invalid password", 403)
				return
			}
		}
		s.issue(w)
		w.WriteHeader(204)
		return
	}
	if strings.HasPrefix(r.URL.Path, "/api/") || strings.HasPrefix(r.URL.Path, "/dashboard") {
		if !private && !s.authenticated(r) {
			http.Error(w, "sign in required", 401)
			return
		}
		if !private {
			if cookie, err := r.Cookie("ts_egress_session"); err == nil {
				r = r.WithContext(context.WithValue(r.Context(), sessionKey{}, cookie.Value))
			}
		}
		switch {
		case r.Method == "GET" && r.URL.Path == "/api/version":
			ui.VersionInfo(w, r)
		case r.Method == "POST" && r.URL.Path == "/api/setup/tailscale/logout":
			if !private {
				http.Error(w, "use the tailnet dashboard", 403)
				return
			}
			runtime, ok := s.runtime.(interface{ Logout(context.Context) error })
			if !ok {
				http.Error(w, "logout unavailable", 503)
				return
			}
			// Accepted, not completed: logout may tear down this response's
			// connection. Keep the node alive for local reauthentication.
			w.WriteHeader(http.StatusAccepted)
			if f, ok := w.(http.Flusher); ok {
				f.Flush()
			}
			go func() {
				ctx, cancel := context.WithTimeout(context.WithoutCancel(r.Context()), 15*time.Second)
				defer cancel()
				_ = runtime.Logout(ctx) // Runtime records failures in connection status.
			}()
		case r.Method == "POST" && r.URL.Path == "/api/setup/connect":
			if private {
				http.Error(w, "use the local console", http.StatusForbidden)
				return
			}
			if !s.store.Get().Connect {
				http.Error(w, "save connection settings first", 400)
				return
			}
			s.runtime.Connect()
			w.WriteHeader(http.StatusAccepted)
		case r.Method == "POST" && r.URL.Path == "/api/setup/reauthenticate":
			runtime, ok := s.runtime.(interface{ Reauthenticate(context.Context) error })
			if !ok {
				http.Error(w, "reauthentication unavailable", 503)
				return
			}
			if err := runtime.Reauthenticate(r.Context()); err != nil {
				http.Error(w, err.Error(), 400)
				return
			}
			// Return the authorization URL so the console can open it directly.
			// It can take a moment to appear after the login request.
			authURL := ""
			deadline := time.Now().Add(3 * time.Second)
			for authURL == "" && time.Now().Before(deadline) {
				authURL = s.runtime.Status(r.Context()).AuthURL
				if authURL == "" {
					time.Sleep(100 * time.Millisecond)
				}
			}
			w.Header().Set("Content-Type", "application/json")
			json.NewEncoder(w).Encode(map[string]string{"authURL": authURL})
		case r.Method == "POST" && r.URL.Path == "/api/setup/logout":
			s.mu.Lock()
			s.sessions = map[string]session{}
			s.mu.Unlock()
			http.SetCookie(w, &http.Cookie{Name: "ts_egress_session", Path: "/", MaxAge: -1, HttpOnly: true, SameSite: http.SameSiteStrictMode})
			w.WriteHeader(204)
		case r.Method == "GET" && r.URL.Path == "/api/setup/status":
			w.Header().Set("Content-Type", "application/json")
			settings, revision := s.store.Snapshot()
			w.Header().Set("ETag", `"`+strconv.FormatUint(revision, 10)+`"`)
			st := s.runtime.Status(r.Context())
			if !private && settings.TailnetOnly {
				settings.Admins = nil
				settings.Email = ""
				st.Peers = []Peer{}
			}
			json.NewEncoder(w).Encode(map[string]any{"settings": settings, "settingsRevision": revision, "tailnet": st, "identity": gateway.AdminIdentity(r.Context())})
		case r.Method == "POST" && r.URL.Path == "/api/setup/password":
			if private {
				http.Error(w, "change password from the local console", http.StatusForbidden)
				return
			}
			var body struct {
				Current string `json:"current"`
				Next    string `json:"next"`
			}
			if decode(w, r, &body) != nil {
				http.Error(w, "invalid JSON", 400)
				return
			}
			s.credentialsMu.Lock()
			defer s.credentialsMu.Unlock()
			if err := s.store.ChangePassword(body.Current, body.Next); err != nil {
				http.Error(w, err.Error(), 400)
				return
			}
			s.mu.Lock()
			s.sessions = map[string]session{}
			s.mu.Unlock()
			if _, err := r.Cookie("ts_egress_session"); err == nil {
				http.SetCookie(w, &http.Cookie{Name: "ts_egress_session", Path: "/", MaxAge: -1, HttpOnly: true, SameSite: http.SameSiteStrictMode})
			}
			w.WriteHeader(http.StatusNoContent)
		case r.Method == "PUT" && r.URL.Path == "/api/setup/settings":
			var v Settings
			if decode(w, r, &v) != nil {
				http.Error(w, "invalid settings JSON", 400)
				return
			}
			current := s.store.Get()
			needsConfirm := private && requiresAdminConfirmation(current.Admins, v.Admins, gateway.AdminIdentity(r.Context()))
			confirmed := r.Header.Get("X-Confirm-Admin-Lockout") == "1"
			if len(current.Admins) > 0 && len(v.Admins) == 0 {
				confirmed = r.Header.Get("X-Confirm-Admin-Change") == "1"
			}
			if needsConfirm && !confirmed {
				http.Error(w, "administrator change needs explicit confirmation", http.StatusConflict)
				return
			}
			revision, err := strconv.ParseUint(r.Header.Get("If-Match"), 10, 64)
			if err != nil {
				http.Error(w, "settings changed; reload before saving", http.StatusConflict)
				return
			}
			if err := s.store.UpdateAtRevision(v, revision, true); err != nil {
				if strings.Contains(err.Error(), "settings changed") {
					http.Error(w, err.Error(), http.StatusConflict)
					return
				}
				http.Error(w, err.Error(), 400)
				return
			}
			if v.Connect {
				s.runtime.Connect()
			}
			w.WriteHeader(204)
		case r.Method == "GET" && r.URL.Path == "/dashboard":
			ui.Serve(w, r, "dashboard")
		case r.URL.Path == "/api/status" || r.URL.Path == "/api/routes" || r.URL.Path == "/api/activity" || r.URL.Path == "/api/certificate/retry":
			s.dashboard(r.Host, origin).ServeHTTP(w, r)
		default:
			http.NotFound(w, r)
		}
		return
	}
	if r.Method != "GET" {
		http.NotFound(w, r)
		return
	}
	ui.Serve(w, r, "setup")
}

func excludesAdmin(admins []string, actor string) bool {
	if actor == "" || len(admins) == 0 {
		return false
	}
	for _, a := range admins {
		if strings.TrimSpace(a) == actor {
			return false
		}
	}
	return true
}

// An empty allowlist permits every untagged tailnet user, so moving from a
// restricted list to empty is a security-sensitive public exposure change.
func requiresAdminConfirmation(current, next []string, actor string) bool {
	return len(current) > 0 && len(next) == 0 || excludesAdmin(next, actor)
}
func (s *Server) dashboard(host, origin string) http.Handler {
	return s.gateway.AdminOrigin(s.routesPath, host, origin, func(context.Context, string) (string, error) { return "local administrator", nil })
}
