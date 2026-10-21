package setup

import (
	"context"
	"net"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"strconv"
	"strings"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	"golang.org/x/crypto/bcrypt"
	"ts-egress/internal/gateway"
)

type fakeRuntime struct {
	connections int
	logouts     atomic.Int64
}

func (f *fakeRuntime) Connect()                     { f.connections++ }
func (f *fakeRuntime) Logout(context.Context) error { f.logouts.Add(1); return nil }
func (f *fakeRuntime) Status(context.Context) Status {
	return Status{State: "not-connected", Peers: []Peer{}}
}
func fixture(t *testing.T) (*Server, *Store, *fakeRuntime) {
	t.Helper()
	dir := t.TempDir()
	store, err := Open(dir)
	if err != nil {
		t.Fatal(err)
	}
	rt := &fakeRuntime{}
	g := gateway.New((&net.Dialer{}).DialContext)
	return New(store, rt, g, filepath.Join(dir, "routes.json")), store, rt
}
func request(s *Server, method, path, body string, cookie *http.Cookie) *httptest.ResponseRecorder {
	r := httptest.NewRequest(method, "http://localhost:8080"+path, strings.NewReader(body))
	r.Header.Set("Origin", "http://localhost:8080")
	r.Header.Set("Content-Type", "application/json")
	r.Header.Set("X-TS-Egress", "1")
	if path == "/api/routes" && method == "PUT" {
		r.Header.Set("If-Match", strconv.FormatUint(s.gateway.Revision(), 10))
	}
	if path == "/api/setup/settings" && method == "PUT" {
		r.Header.Set("If-Match", strconv.FormatUint(s.store.Revision(), 10))
	}
	if path == "/api/geolocation" && method == "POST" {
		r.Header.Set("Origin", "https://egress.tail.ts.net")
	}
	if cookie != nil {
		r.AddCookie(cookie)
	}
	w := httptest.NewRecorder()
	s.ServeHTTP(w, r)
	return w
}
func resetRate(s *Server) { s.mu.Lock(); s.nextLogin = time.Time{}; s.mu.Unlock() }

func TestConcurrentPasswordChangeRevokesOldPasswordLogin(t *testing.T) {
	for range 5 {
		s, _, _ := fixture(t)
		claimed := request(s, "POST", "/api/setup/claim", `{"password":"old long password"}`, nil)
		cookie := claimed.Result().Cookies()[0]
		resetRate(s)
		start := make(chan struct{})
		var login, changed *httptest.ResponseRecorder
		var wg sync.WaitGroup
		wg.Add(2)
		go func() {
			defer wg.Done()
			<-start
			login = request(s, "POST", "/api/setup/login", `{"password":"old long password"}`, nil)
		}()
		go func() {
			defer wg.Done()
			<-start
			changed = request(s, "POST", "/api/setup/password", `{"current":"old long password","next":"new long password"}`, cookie)
		}()
		close(start)
		wg.Wait()
		// A login that completes first rotates the sole session before the
		// password request authenticates. Retry with that session in this case.
		if changed.Code == http.StatusUnauthorized && login.Code == http.StatusNoContent {
			changed = request(s, "POST", "/api/setup/password", `{"current":"old long password","next":"new long password"}`, login.Result().Cookies()[0])
		}
		if changed.Code != http.StatusNoContent {
			t.Fatalf("password change failed: %d %s", changed.Code, changed.Body.String())
		}
		if login.Code == http.StatusNoContent {
			if w := request(s, "GET", "/api/setup/status", "", login.Result().Cookies()[0]); w.Code != http.StatusUnauthorized {
				t.Fatal("old-password login survived password change")
			}
		} else if login.Code != http.StatusForbidden {
			t.Fatalf("unexpected login status: %d", login.Code)
		}
	}
}
func TestSetupLifecycle(t *testing.T) {
	s, store, rt := fixture(t)
	if w := request(s, "GET", "/api/setup/status", "", nil); w.Code != 401 {
		t.Fatal(w.Code)
	}
	w := request(s, "POST", "/api/setup/claim", `{"password":"a long test password"}`, nil)
	if w.Code != 204 {
		t.Fatal(w.Code, w.Body.String())
	}
	cookie := w.Result().Cookies()[0]
	if !cookie.HttpOnly || cookie.SameSite != http.SameSiteStrictMode {
		t.Fatal("unsafe cookie")
	}
	if w := request(s, "GET", "/api/setup/status", "", cookie); w.Code != 200 || strings.Contains(w.Body.String(), "passwordHash") {
		t.Fatal(w.Code, w.Body.String())
	}
	resetRate(s)
	if w := request(s, "POST", "/api/setup/claim", `{"password":"another long password"}`, nil); w.Code != 400 {
		t.Fatal("claimed twice")
	}
	body := `{"hostname":"my-edge","admins":["you@example.com"],"email":"you@example.com","acceptTerms":true,"connect":true}`
	if w := request(s, "PUT", "/api/setup/settings", body, cookie); w.Code != 204 {
		t.Fatal(w.Code, w.Body.String())
	}
	if rt.connections != 1 {
		t.Fatal("did not request connection")
	}
	reopened, err := Open(filepath.Dir(store.path))
	if err != nil {
		t.Fatal(err)
	}
	if v := reopened.Get(); v.Hostname != "my-edge" || !v.Connect || v.PasswordHash == "" {
		t.Fatal("settings not persisted")
	}
	b, _ := os.ReadFile(store.path)
	if strings.Contains(string(b), "a long test password") {
		t.Fatal("plaintext password persisted")
	}
	info, _ := os.Stat(store.path)
	if info.Mode().Perm() != 0600 {
		t.Fatal("settings permissions")
	}
	if w := request(s, "GET", "/dashboard", "", cookie); w.Code != 200 || !strings.Contains(w.Body.String(), "Services · ts-egress") {
		t.Fatal("dashboard unavailable")
	}
	if w := request(s, "PUT", "/api/routes", `[{"domain":"app.example.com","upstream":"http://peer:3000","enabled":true}]`, cookie); w.Code != 204 {
		t.Fatal(w.Code, w.Body.String())
	}
	if w := request(s, "POST", "/api/setup/logout", `{}`, cookie); w.Code != 204 {
		t.Fatal(w.Code)
	}
	if w := request(s, "GET", "/api/setup/status", "", cookie); w.Code != 401 {
		t.Fatal("logout did not revoke session")
	}
	resetRate(s)
	w = request(s, "POST", "/api/setup/login", `{"password":"a long test password"}`, nil)
	if w.Code != 204 {
		t.Fatal("login failed", w.Body.String())
	}
	if w := request(s, "POST", "/api/setup/login", `{"password":"bad"}`, nil); w.Code != 429 {
		t.Fatal("rate limit not enforced")
	}
}
func TestSetupSecurity(t *testing.T) {
	s, _, _ := fixture(t)
	for _, host := range []string{"evil.example:8080", "192.0.2.1:8080", "localhost.evil.example:8080", "localhost"} {
		r := httptest.NewRequest("GET", "http://"+host+"/", nil)
		w := httptest.NewRecorder()
		s.ServeHTTP(w, r)
		if w.Code != 403 {
			t.Errorf("accepted host %s", host)
		}
	}
	r := httptest.NewRequest("POST", "http://localhost:8080/api/setup/claim", strings.NewReader(`{"password":"long enough password"}`))
	r.Header.Set("Origin", "http://evil.example")
	w := httptest.NewRecorder()
	s.ServeHTTP(w, r)
	if w.Code != 403 {
		t.Fatal("accepted cross origin")
	}
	if w := request(s, "GET", "/api/status", "", nil); w.Code != 401 {
		t.Fatal("anonymous route access")
	}
	if w := request(s, "POST", "/api/setup/claim", `{"password":"short"}`, nil); w.Code != 400 {
		t.Fatal("weak password accepted")
	}
	resetRate(s)
	if w := request(s, "POST", "/api/setup/claim", `{"password":"a long test password"} {}`, nil); w.Code != 400 {
		t.Fatal("trailing JSON accepted")
	}
}
func TestSettingsValidation(t *testing.T) {
	_, store, _ := fixture(t)
	if err := store.Claim("a long test password"); err != nil {
		t.Fatal(err)
	}
	v := store.Get()
	v.Hostname = "bad.name"
	if store.Update(v) == nil {
		t.Fatal("bad hostname accepted")
	}
	v = store.Get()
	v.AcceptTerms = true
	if store.Update(v) == nil {
		t.Fatal("missing contact accepted")
	}
	v = store.Get()
	v.Connect = true
	v.Admins = []string{"you@example.com"}
	if err := store.Update(v); err != nil {
		t.Fatal(err)
	}
	v.Hostname = "renamed"
	if store.Update(v) == nil {
		t.Fatal("renamed live node")
	}
}

func TestAdminCredentialRecovery(t *testing.T) {
	dir := t.TempDir()
	store, err := Open(dir)
	if err != nil {
		t.Fatal(err)
	}
	if err := store.Claim("initial administrator password"); err != nil {
		t.Fatal(err)
	}
	if err := store.Seal(); err != nil {
		t.Fatal(err)
	}
	if err := store.RecoverAdmin("recovered administrator password", []string{"admin@example.com"}); err != nil {
		t.Fatal(err)
	}
	reopened, err := Open(dir)
	if err != nil {
		t.Fatal(err)
	}
	if bcrypt.CompareHashAndPassword([]byte(reopened.Get().PasswordHash), []byte("recovered administrator password")) != nil {
		t.Fatal("recovery password not persisted")
	}
	if reopened.Get().Admins[0] != "admin@example.com" || !reopened.Get().TailnetOnly {
		t.Fatal("recovery did not preserve lock state or update allowlist")
	}
}

func TestPasswordChangePersistsAndRevokesSession(t *testing.T) {
	s, store, _ := fixture(t)
	if err := store.Claim("initial administrator password"); err != nil {
		t.Fatal(err)
	}
	login := request(s, "POST", "/api/setup/login", `{"password":"initial administrator password"}`, nil)
	cookie := login.Result().Cookies()[0]
	before := store.Revision()
	if err := store.Seal(); err != nil {
		t.Fatal(err)
	}
	before = store.Revision()
	identity := func(context.Context, string) (string, error) { return "admin@example.com", nil }
	private := s.gateway.AdminWithSetup(s.routesPath, "egress.tail.ts.net", "https://egress.tail.ts.net", identity, s.Tailnet())
	denied := httptest.NewRequest("POST", "https://egress.tail.ts.net/api/setup/password", strings.NewReader(`{"current":"initial administrator password","next":"replacement administrator password"}`))
	denied.Header.Set("Origin", "https://egress.tail.ts.net")
	denied.Header.Set("Content-Type", "application/json")
	denied.Header.Set("X-TS-Egress", "1")
	deniedResponse := httptest.NewRecorder()
	private.ServeHTTP(deniedResponse, denied)
	if deniedResponse.Code != http.StatusForbidden || store.Revision() != before {
		t.Fatal("tailnet password mutation allowed", deniedResponse.Code)
	}
	r := httptest.NewRequest(http.MethodPost, "http://localhost:8080/api/setup/password", strings.NewReader(`{"current":"initial administrator password","next":"replacement administrator password"}`))
	r.Header.Set("Origin", "http://localhost:8080")
	r.Header.Set("Content-Type", "application/json")
	r.Header.Set("X-TS-Egress", "1")
	r.AddCookie(cookie)
	w := httptest.NewRecorder()
	s.ServeHTTP(w, r)
	if w.Code != http.StatusNoContent || store.Revision() != before+1 {
		t.Fatal("password change failed", w.Code, store.Revision())
	}
	if w := request(s, "GET", "/api/setup/status", "", cookie); w.Code != http.StatusUnauthorized {
		t.Fatal("old session remained valid", w.Code)
	}
	resetRate(s)
	w = request(s, "POST", "/api/setup/login", `{"password":"replacement administrator password"}`, nil)
	if w.Code != http.StatusNoContent {
		t.Fatal("new password did not authenticate", w.Code)
	}
}

func TestSealedConnectionRetryUsesLocalHandler(t *testing.T) {
	s, store, rt := fixture(t)
	if err := store.Claim("initial administrator password"); err != nil {
		t.Fatal(err)
	}
	v := store.Get()
	v.Connect = true
	if err := store.Update(v); err != nil {
		t.Fatal(err)
	}
	if err := store.Seal(); err != nil {
		t.Fatal(err)
	}
	if w := request(s, "POST", "/api/setup/connect", `{}`, nil); w.Code != 401 {
		t.Fatal(w.Code)
	}
	cookie := request(s, "POST", "/api/setup/login", `{"password":"initial administrator password"}`, nil).Result().Cookies()[0]
	w := request(s, "POST", "/api/setup/connect", `{}`, cookie)
	if w.Code != http.StatusAccepted {
		t.Fatal(w.Code, w.Body.String())
	}
	if rt.connections != 1 {
		t.Fatalf("connection attempts = %d", rt.connections)
	}
	if w := request(s, "PUT", "/api/setup/settings", `{}`, cookie); w.Code != 403 {
		t.Fatal("retry reopened settings", w.Code)
	}
}

func TestSnapshotPairsRevisionAndSettings(t *testing.T) {
	_, store, _ := fixture(t)
	if err := store.Claim("initial administrator password"); err != nil {
		t.Fatal(err)
	}
	settings, revision := store.Snapshot()
	copyForUpdate := settings
	copyForUpdate.Email = "changed@example.com"
	if err := store.Update(copyForUpdate); err != nil {
		t.Fatal(err)
	}
	if settings.Email != "" || store.Get().Email != "changed@example.com" || revision+1 != store.Revision() {
		t.Fatalf("snapshot leaked mutable settings or revision: %+v rev=%d", settings, revision)
	}
}

func TestTailnetGeoSettingsRequireTailnetIdentity(t *testing.T) {
	s, _, _ := fixture(t)
	// Local session/authentication is not sufficient for the tailnet-only control.
	if w := request(s, http.MethodGet, "/api/geolocation", "", nil); w.Code != http.StatusNotFound && w.Code != http.StatusUnauthorized {
		t.Fatalf("unexpected local response %d", w.Code)
	}
}

func TestSettingsRevisionRejectsStaleUpdate(t *testing.T) {
	s, store, _ := fixture(t)
	if err := store.Claim("initial administrator password"); err != nil {
		t.Fatal(err)
	}
	cookie := request(s, "POST", "/api/setup/login", `{"password":"initial administrator password"}`, nil).Result().Cookies()[0]
	revision := store.Revision()
	first := store.Get()
	first.Email = "first@example.com"
	if err := store.Update(first); err != nil {
		t.Fatal(err)
	}
	stale := store.Get()
	stale.Email = "stale@example.com"
	r := httptest.NewRequest("PUT", "http://localhost:8080/api/setup/settings", strings.NewReader(`{"hostname":"ts-egress","admins":[],"email":"stale@example.com","acceptTerms":false,"connect":false}`))
	r.Header.Set("Origin", "http://localhost:8080")
	r.Header.Set("Content-Type", "application/json")
	r.Header.Set("X-TS-Egress", "1")
	r.Header.Set("If-Match", strconv.FormatUint(revision, 10))
	r.AddCookie(cookie)
	w := httptest.NewRecorder()
	s.ServeHTTP(w, r)
	if w.Code != http.StatusConflict || store.Get().Email != "first@example.com" {
		t.Fatal("stale settings write accepted", w.Code)
	}
}

func TestEmptyAdminAllowlist(t *testing.T) {
	_, store, _ := fixture(t)
	if err := store.Claim("a long test password"); err != nil {
		t.Fatal(err)
	}
	v := store.Get()
	v.Connect = true
	v.Admins = []string{}
	if err := store.Update(v); err != nil {
		t.Fatalf("empty allowlist rejected: %v", err)
	}
}
func TestTailnetHandoffEnforcement(t *testing.T) {
	s, store, _ := fixture(t)
	if err := store.Claim("a long test password"); err != nil {
		t.Fatal(err)
	}
	if err := store.Seal(); err != nil {
		t.Fatal(err)
	}
	reopened, err := Open(filepath.Dir(store.path))
	if err != nil || !reopened.Get().TailnetOnly {
		t.Fatal("seal not persisted", err)
	}
	for _, path := range []string{"/dashboard", "/api/status", "/api/routes", "/api/activity", "/api/certificate/retry", "/api/setup/settings"} {
		if w := request(s, "GET", path, "", nil); w.Code != 403 {
			t.Errorf("local management still reachable %s: %d", path, w.Code)
		}
	}
	// The private handler must remain behind Host and WhoIs checks.
	identity := func(context.Context, string) (string, error) { return "you@example.com", nil }
	h := s.gateway.AdminWithSetup(s.routesPath, "egress.tail.ts.net", "https://egress.tail.ts.net", identity, s.Tailnet())
	w := httptest.NewRecorder()
	h.ServeHTTP(w, httptest.NewRequest("GET", "https://egress.tail.ts.net/api/setup/session", nil))
	if w.Code != 200 || !strings.Contains(w.Body.String(), `"tailnet":true`) {
		t.Fatal(w.Code, w.Body.String())
	}
	w = httptest.NewRecorder()
	h.ServeHTTP(w, httptest.NewRequest("GET", "https://egress.tail.ts.net/setup", nil))
	if w.Code != 200 || !strings.Contains(w.Body.String(), `data-page="setup"`) {
		t.Fatal(w.Code, w.Body.String())
	}
	h = s.gateway.AdminWithSetup(s.routesPath, "egress.tail.ts.net", "https://egress.tail.ts.net", func(context.Context, string) (string, error) { return "", nil }, s.Tailnet())
	w = httptest.NewRecorder()
	h.ServeHTTP(w, httptest.NewRequest("GET", "https://egress.tail.ts.net/api/setup/status", nil))
	if w.Code != 403 {
		t.Fatal("WhoIs bypass", w.Code)
	}
}

func TestTailnetLogout(t *testing.T) {
	s, _, rt := fixture(t)
	w := request(s, "POST", "/api/setup/claim", `{"password":"a long test password"}`, nil)
	if w.Code != 204 {
		t.Fatal(w.Code, w.Body.String())
	}
	cookie := w.Result().Cookies()[0]
	// The local console may not disconnect Tailscale.
	if w := request(s, "POST", "/api/setup/tailscale/logout", `{}`, cookie); w.Code != 403 || rt.logouts.Load() != 0 {
		t.Fatal("local logout allowed", w.Code, rt.logouts.Load())
	}
	identity := func(context.Context, string) (string, error) { return "you@example.com", nil }
	h := s.gateway.AdminWithSetup(s.routesPath, "egress.tail.ts.net", "https://egress.tail.ts.net", identity, s.Tailnet())
	r := httptest.NewRequest("POST", "https://egress.tail.ts.net/api/setup/tailscale/logout", strings.NewReader(`{}`))
	r.Header.Set("Origin", "https://egress.tail.ts.net")
	r.Header.Set("Content-Type", "application/json")
	r.Header.Set("X-TS-Egress", "1")
	w = httptest.NewRecorder()
	h.ServeHTTP(w, r)
	if w.Code != http.StatusAccepted {
		t.Fatal("tailnet logout status", w.Code)
	}
	// Logout runs after the response so the browser can render instructions.
	deadline := time.Now().Add(time.Second)
	for rt.logouts.Load() != 1 && time.Now().Before(deadline) {
		time.Sleep(time.Millisecond)
	}
	if rt.logouts.Load() != 1 {
		t.Fatal("tailnet logout did not run", rt.logouts.Load())
	}
}
