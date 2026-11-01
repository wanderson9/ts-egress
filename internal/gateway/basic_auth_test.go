package gateway

import (
	"context"
	"crypto/tls"
	"encoding/json"
	"net"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"strconv"
	"strings"
	"testing"
)

func TestBasicAuthHTTPSProxyAndAuthorizationStrip(t *testing.T) {
	var authorization string
	up := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		authorization = r.Header.Get("Authorization")
		w.WriteHeader(http.StatusNoContent)
	}))
	defer up.Close()
	backend := strings.TrimPrefix(up.URL, "http://")
	_, port, _ := net.SplitHostPort(backend)
	g := New(func(ctx context.Context, network, address string) (net.Conn, error) {
		return (&net.Dialer{}).DialContext(ctx, network, net.JoinHostPort("127.0.0.1", port))
	})
	path := filepath.Join(t.TempDir(), "routes.json")
	routes, err := PersistedRoutes([]Route{{Domain: "auth.example.com", Upstream: "http://backend.tail.ts.net:" + port, Enabled: true, BasicAuthUser: "user", BasicAuthPassword: "secret"}})
	if err != nil {
		t.Fatal(err)
	}
	if err := Save(path, routes); err != nil {
		t.Fatal(err)
	}
	routes, err = Load(path)
	if err != nil {
		t.Fatal(err)
	}
	g.Replace(routes)
	request := func(tls bool, auth string) *httptest.ResponseRecorder {
		scheme := "https"
		if !tls {
			scheme = "http"
		}
		r := httptest.NewRequest("GET", scheme+"://auth.example.com/", nil)
		if tls {
			r.TLS = &tlsState
		}
		if auth != "" {
			r.Header.Set("Authorization", auth)
		}
		w := httptest.NewRecorder()
		g.ServeHTTP(w, r)
		return w
	}
	if got := request(true, "Basic dXNlcjpzZWNyZXQ="); got.Code != http.StatusNoContent {
		t.Fatalf("valid credentials: %d %s", got.Code, got.Body)
	}
	if authorization != "" {
		t.Fatalf("auth header forwarded: %q", authorization)
	}
	if got := request(true, "Basic Ym9iOnNlY3JldA=="); got.Code != http.StatusUnauthorized {
		t.Fatalf("wrong username accepted: %d", got.Code)
	}
	if got := request(true, "Basic dXNlcjp3cm9uZw=="); got.Code != http.StatusUnauthorized || got.Header().Get("WWW-Authenticate") == "" {
		t.Fatalf("invalid credentials: %d", got.Code)
	}
	if got := request(false, "Basic dXNlcjpzZWNyZXQ="); got.Code != http.StatusUpgradeRequired {
		t.Fatalf("plaintext authenticated: %d", got.Code)
	}
}

func TestSaveHashesCredentialsAndLoadPreservesVerifier(t *testing.T) {
	path := filepath.Join(t.TempDir(), "routes.json")
	routes := []Route{{Domain: "auth.example.com", Upstream: "http://backend", Enabled: true, BasicAuthUser: "u", BasicAuthPassword: "passphrase"}}
	if err := Save(path, routes); err != nil {
		t.Fatal(err)
	}
	data, err := os.ReadFile(path)
	if err != nil {
		t.Fatal(err)
	}
	if strings.Contains(string(data), "passphrase") {
		t.Fatal("plaintext credential persisted")
	}
	loaded, err := Load(path)
	if err != nil {
		t.Fatal(err)
	}
	if !strings.HasPrefix(loaded[0].BasicAuthPassword, "bcrypt:") {
		t.Fatalf("missing verifier: %q", loaded[0].BasicAuthPassword)
	}
	stored := loaded[0].BasicAuthPassword
	if err := Save(path, loaded); err != nil {
		t.Fatal(err)
	}
	loaded, err = Load(path)
	if err != nil || loaded[0].BasicAuthPassword != stored {
		t.Fatalf("verifier changed on save: %v", err)
	}
}

var tlsState = structTLSState()

func structTLSState() tls.ConnectionState { return tls.ConnectionState{ServerName: "auth.example.com"} }

func adminTestRequest(h http.Handler, method, path string, body []byte, headers map[string]string) *httptest.ResponseRecorder {
	r := httptest.NewRequest(method, path, strings.NewReader(string(body)))
	r.Host = "admin.example.com"
	r.RemoteAddr = "127.0.0.1:1234"
	r.Header.Set("Origin", "https://admin.example.com")
	r.Header.Set("Content-Type", "application/json")
	r.Header.Set("X-TS-Egress", "1")
	for k, v := range headers {
		r.Header.Set(k, v)
	}
	r = r.WithContext(context.WithValue(r.Context(), adminAccessKey{}, adminAccess{actor: "admin"}))
	w := httptest.NewRecorder()
	h.ServeHTTP(w, r)
	return w
}

func TestBasicAuthStatusRedactionPreservationAndRemoval(t *testing.T) {
	g := New((&net.Dialer{}).DialContext)
	initial := Route{Domain: "auth.example.com", Upstream: "http://backend", Enabled: true, BasicAuthUser: "alice", BasicAuthPassword: "first password"}
	g.Replace([]Route{initial})
	path := filepath.Join(t.TempDir(), "routes.json")
	h := g.Admin(path, "admin.example.com", func(context.Context, string) (string, error) { return "admin", nil })
	status := adminTestRequest(h, "GET", "/api/status", nil, nil)
	if status.Code != 200 || strings.Contains(status.Body.String(), "first password") || strings.Contains(status.Body.String(), "bcrypt:") {
		t.Fatalf("status leaked secret: %d %s", status.Code, status.Body)
	}
	var payload struct {
		Routes   []Route `json:"routes"`
		Revision uint64  `json:"revision"`
	}
	if err := json.Unmarshal(status.Body.Bytes(), &payload); err != nil {
		t.Fatal(err)
	}
	if len(payload.Routes) != 1 || !payload.Routes[0].BasicAuth || payload.Routes[0].BasicAuthPassword != "" {
		t.Fatalf("unexpected status route: %+v", payload.Routes)
	}
	edit := payload.Routes
	edit[0].Upstream = "http://other"
	b, _ := json.Marshal(edit)
	put := adminTestRequest(h, "PUT", "/api/routes", b, map[string]string{"If-Match": strconv.FormatUint(payload.Revision, 10)})
	if put.Code != 204 {
		t.Fatalf("preserve auth edit: %d %s", put.Code, put.Body)
	}
	if g.Routes()[0].BasicAuthPassword == "" {
		t.Fatal("credential not preserved")
	}
	status = adminTestRequest(h, "GET", "/api/status", nil, nil)
	if err := json.Unmarshal(status.Body.Bytes(), &payload); err != nil {
		t.Fatal(err)
	}
	edit = payload.Routes
	edit[0].BasicAuthUser = "bob"
	b, _ = json.Marshal(edit)
	put = adminTestRequest(h, "PUT", "/api/routes", b, map[string]string{"If-Match": strconv.FormatUint(payload.Revision, 10)})
	if put.Code != 400 {
		t.Fatalf("username change without new password: %d", put.Code)
	}
	edit[0].BasicAuthPassword = "replacement"
	b, _ = json.Marshal(edit)
	put = adminTestRequest(h, "PUT", "/api/routes", b, map[string]string{"If-Match": strconv.FormatUint(payload.Revision, 10)})
	if put.Code != 204 {
		t.Fatalf("username/password change: %d %s", put.Code, put.Body)
	}
	if g.Routes()[0].BasicAuthUser != "bob" {
		t.Fatal("username change not applied")
	}
	encoded, _ := os.ReadFile(path)
	if strings.Contains(string(encoded), "first password") {
		t.Fatal("plaintext credential persisted")
	}
	loaded, err := Load(path)
	if err != nil {
		t.Fatal(err)
	}
	if !strings.HasPrefix(loaded[0].BasicAuthPassword, "bcrypt:") {
		t.Fatalf("not stored as bcrypt verifier: %s", loaded[0].BasicAuthPassword)
	}
	status = adminTestRequest(h, "GET", "/api/status", nil, nil)
	json.Unmarshal(status.Body.Bytes(), &payload)
	edit = payload.Routes
	edit[0].BasicAuth = false
	edit[0].BasicAuthUser = ""
	edit[0].BasicAuthPassword = ""
	b, _ = json.Marshal(edit)
	base := map[string]string{"If-Match": strconv.FormatUint(payload.Revision, 10)}
	put = adminTestRequest(h, "PUT", "/api/routes", b, base)
	if put.Code != 409 {
		t.Fatalf("removal without confirmation: %d", put.Code)
	}
	base["X-Confirm-Auth-Removal"] = "1"
	put = adminTestRequest(h, "PUT", "/api/routes", b, base)
	if put.Code != 204 {
		t.Fatalf("confirmed removal: %d %s", put.Code, put.Body)
	}
	if g.Routes()[0].BasicAuthUser != "" {
		t.Fatal("authentication not removed")
	}

	// Removing the route entirely must also require explicit confirmation.
	status = adminTestRequest(h, "GET", "/api/status", nil, nil)
	if err := json.Unmarshal(status.Body.Bytes(), &payload); err != nil {
		t.Fatal(err)
	}
	edit = payload.Routes
	edit[0].BasicAuthUser = "alice"
	edit[0].BasicAuthPassword = "restore password"
	edit[0].BasicAuth = true
	b, _ = json.Marshal(edit)
	put = adminTestRequest(h, "PUT", "/api/routes", b, map[string]string{"If-Match": strconv.FormatUint(payload.Revision, 10)})
	if put.Code != 204 {
		t.Fatalf("restore authentication: %d %s", put.Code, put.Body)
	}
	status = adminTestRequest(h, "GET", "/api/status", nil, nil)
	if err := json.Unmarshal(status.Body.Bytes(), &payload); err != nil {
		t.Fatal(err)
	}
	b, _ = json.Marshal([]Route{})
	base = map[string]string{"If-Match": strconv.FormatUint(payload.Revision, 10)}
	put = adminTestRequest(h, "PUT", "/api/routes", b, base)
	if put.Code != 409 {
		t.Fatalf("route deletion without confirmation: %d %s", put.Code, put.Body)
	}
	base["X-Confirm-Auth-Removal"] = "1"
	put = adminTestRequest(h, "PUT", "/api/routes", b, base)
	if put.Code != 204 {
		t.Fatalf("confirmed route deletion: %d %s", put.Code, put.Body)
	}
}

func TestBasicAuthRejectsBadVerifierAndMalformedFlag(t *testing.T) {
	for _, route := range []Route{
		{Domain: "auth.example.com", Upstream: "http://backend", Enabled: true, BasicAuth: true},
		{Domain: "auth.example.com", Upstream: "http://backend", Enabled: true, BasicAuthUser: "u", BasicAuthPassword: "bcrypt:not-a-hash"},
	} {
		if Validate([]Route{route}) == nil {
			t.Fatalf("accepted invalid route %+v", route)
		}
	}
}
