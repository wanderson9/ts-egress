package gateway

import (
	"bytes"
	"context"
	"crypto/tls"
	"encoding/json"
	"errors"
	"net"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"strconv"
	"strings"
	"sync"
	"testing"
)

func TestValidate(t *testing.T) {
	for _, upstream := range []string{"http://server:3000", "https://server.tail.ts.net", "http://100.100.1.2:80", "http://[fd7a:115c:a1e0::1]:8080"} {
		if err := Validate([]Route{{"app.example.com", upstream, true}}); err != nil {
			t.Errorf("%s: %v", upstream, err)
		}
	}
	for _, upstream := range []string{"http://127.0.0.1", "http://169.254.169.254", "http://[::1]", "http://8.8.8.8", "file:///etc/passwd", "http://user:pass@server", "http://server/path", "http://server:99999", "http://server?q=1"} {
		if Validate([]Route{{"app.example.com", upstream, true}}) == nil {
			t.Errorf("accepted unsafe upstream %s", upstream)
		}
	}
	for _, domain := range []string{"localhost", "*.example.com", "UPPER.example.com", "app.example.com:443", "https://app.example.com", "a..com"} {
		if Validate([]Route{{domain, "http://server", true}}) == nil {
			t.Errorf("accepted %s", domain)
		}
	}
}

func TestProxy(t *testing.T) {
	up := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.Host != "app.example.com" {
			t.Errorf("public Host replaced: %s", r.Host)
		}
		if r.URL.Path != "/hello" || r.URL.RawQuery != "q=1" {
			t.Error("lost request URL")
		}
		if r.Header.Get("Tailscale-User-Login") != "" || r.Header.Get("Forwarded") != "" {
			t.Error("trusted spoofed identity")
		}
		if r.Header.Get("X-Forwarded-For") != "192.0.2.1" {
			t.Errorf("bad forwarding: %s", r.Header.Get("X-Forwarded-For"))
		}
		w.Write([]byte("hello"))
	}))
	defer up.Close()
	g := New((&net.Dialer{}).DialContext)
	g.Replace([]Route{{"app.example.com", up.URL, true}})
	r := httptest.NewRequest("GET", "https://app.example.com/hello?q=1", nil)
	r.TLS = &tls.ConnectionState{ServerName: "app.example.com"}
	r.Header.Set("Tailscale-User-Login", "admin@example.com")
	r.Header.Set("X-Forwarded-For", "evil")
	r.Header.Set("Forwarded", "for=evil")
	w := httptest.NewRecorder()
	g.ServeHTTP(w, r)
	if w.Code != 200 || w.Body.String() != "hello" {
		t.Fatalf("%d: %s", w.Code, w.Body.String())
	}
	r.TLS.ServerName = "other.example.com"
	w = httptest.NewRecorder()
	g.ServeHTTP(w, r)
	if w.Code != 421 {
		t.Fatal("accepted mismatched SNI")
	}
	r = httptest.NewRequest("GET", "http://unknown.example.com/api/status", nil)
	w = httptest.NewRecorder()
	g.ServeHTTP(w, r)
	if w.Code != 404 {
		t.Fatal("unknown host not rejected")
	}
}

func TestProxyPreservesPublicHostForRedirects(t *testing.T) {
	up := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.Header.Get("X-Forwarded-Host") != "contour.egress.wa9.us" || r.Header.Get("X-Forwarded-Proto") != "https" {
			t.Errorf("incorrect forwarded origin: %v", r.Header)
		}
		http.Redirect(w, r, "https://"+r.Host+"/login", http.StatusFound)
	}))
	defer up.Close()
	g := New((&net.Dialer{}).DialContext)
	g.Replace([]Route{{"contour.egress.wa9.us", up.URL, true}})
	r := httptest.NewRequest("GET", "https://contour.egress.wa9.us/", nil)
	r.TLS = &tls.ConnectionState{ServerName: "contour.egress.wa9.us"}
	r.Header.Set("X-Forwarded-Host", "spoofed.example")
	r.Header.Set("X-Forwarded-Proto", "http")
	w := httptest.NewRecorder()
	g.ServeHTTP(w, r)
	if w.Code != http.StatusFound || w.Header().Get("Location") != "https://contour.egress.wa9.us/login" {
		t.Fatalf("incorrect redirect: %d %s", w.Code, w.Header().Get("Location"))
	}
}

func TestPersistence(t *testing.T) {
	p := filepath.Join(t.TempDir(), "routes.json")
	routes := []Route{{"app.example.com", "http://server:3000", true}}
	if err := Save(p, routes); err != nil {
		t.Fatal(err)
	}
	loaded, err := Load(p)
	if err != nil || len(loaded) != 1 {
		t.Fatal(loaded, err)
	}
	st, _ := os.Stat(p)
	if st.Mode().Perm() != 0600 {
		t.Fatal("configuration not private")
	}
	if err := Save(p, []Route{{"bad", "http://server", true}}); err == nil {
		t.Fatal("saved invalid configuration")
	}
	loaded, _ = Load(p)
	if len(loaded) != 1 {
		t.Fatal("invalid write corrupted configuration")
	}
}

func TestAdmin(t *testing.T) {
	g := New((&net.Dialer{}).DialContext)
	host := "egress.tail.ts.net"
	p := filepath.Join(t.TempDir(), "routes.json")
	identity := func(context.Context, string) (string, error) { return "admin@example.com", nil }
	h := g.Admin(p, host, identity)
	geoDir := t.TempDir()
	g.Geo = NewGeo(geoDir)
	body := `[{"domain":"app.example.com","upstream":"http://server:3000","enabled":true}]`
	revision := strconv.FormatUint(g.Revision(), 10)
	for _, tc := range []struct {
		origin, marker string
		code           int
	}{{"https://evil.example", "1", 403}, {"https://" + host, "", 403}, {"https://" + host, "1", 204}} {
		r := httptest.NewRequest("PUT", "https://"+host+"/api/routes", strings.NewReader(body))
		r.Header.Set("Origin", tc.origin)
		r.Header.Set("Content-Type", "application/json")
		r.Header.Set("X-TS-Egress", tc.marker)
		r.Header.Set("If-Match", revision)
		w := httptest.NewRecorder()
		h.ServeHTTP(w, r)
		if w.Code != tc.code {
			t.Fatalf("expected %d got %d: %s", tc.code, w.Code, w.Body.String())
		}
		if tc.code == http.StatusNoContent {
			break
		}
	}
	r := httptest.NewRequest("PUT", "https://"+host+"/api/routes", strings.NewReader(`[]`))
	r.Header.Set("Origin", "https://"+host)
	r.Header.Set("Content-Type", "application/json")
	r.Header.Set("X-TS-Egress", "1")
	r.Header.Set("If-Match", "1")
	w := httptest.NewRecorder()
	h.ServeHTTP(w, r)
	if w.Code != http.StatusConflict || len(g.Routes()) != 1 {
		t.Fatal("stale revision replaced routes", w.Code)
	}
	if len(g.Routes()) != 1 {
		t.Fatal("route not activated")
	}
	geoBody := strings.NewReader(`{"enabled":true}`)
	geoReq := httptest.NewRequest("PUT", "https://"+host+"/api/geolocation", geoBody)
	geoReq.Header.Set("Origin", "https://"+host)
	geoReq.Header.Set("Content-Type", "application/json")
	geoReq.Header.Set("X-TS-Egress", "1")
	geoW := httptest.NewRecorder()
	h.ServeHTTP(geoW, geoReq)
	if geoW.Code != http.StatusNoContent || !NewGeo(geoDir).Enabled() {
		t.Fatal("tailnet geolocation opt-in did not persist", geoW.Code)
	}
	deny := g.Admin(p, host, func(context.Context, string) (string, error) { return "", errors.New("denied") })
	w = httptest.NewRecorder()
	deny.ServeHTTP(w, httptest.NewRequest("GET", "https://"+host+"/", nil))
	if w.Code != 403 {
		t.Fatal("anonymous admin accepted")
	}
	w = httptest.NewRecorder()
	h.ServeHTTP(w, httptest.NewRequest("GET", "https://evil.example/api/status", nil))
	if w.Code != 403 {
		t.Fatal("host not checked")
	}
	w = httptest.NewRecorder()
	h.ServeHTTP(w, httptest.NewRequest("GET", "https://"+host+"/", nil))
	if w.Code != 200 || !strings.Contains(w.Body.String(), "ts-egress") {
		t.Fatal("UI not served")
	}
}

func TestConcurrentRouting(t *testing.T) {
	g := New((&net.Dialer{}).DialContext)
	var wg sync.WaitGroup
	for range 8 {
		wg.Go(func() {
			for range 100 {
				g.Replace([]Route{{"app.example.com", "http://server", false}})
				g.Routes()
				g.Allows(context.Background(), "app.example.com")
				g.ServeHTTP(httptest.NewRecorder(), httptest.NewRequest("GET", "http://unknown.example.com/", nil))
			}
		})
	}
	wg.Wait()
}

func TestAdmissionBoundsConcurrentRequests(t *testing.T) {
	gate := NewAdmission(1)
	entered := make(chan struct{})
	release := make(chan struct{})
	h := gate.Middleware(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) { close(entered); <-release; w.WriteHeader(204) }))
	first := httptest.NewRecorder()
	done := make(chan struct{})
	go func() { h.ServeHTTP(first, httptest.NewRequest("GET", "/", nil)); close(done) }()
	<-entered
	second := httptest.NewRecorder()
	h.ServeHTTP(second, httptest.NewRequest("GET", "/", nil))
	if second.Code != http.StatusServiceUnavailable {
		t.Fatal("admission did not reject excess request", second.Code)
	}
	close(release)
	<-done
	if first.Code != http.StatusNoContent {
		t.Fatal("admitted request failed", first.Code)
	}
}

func TestRouteSaveRejectsStaleRevision(t *testing.T) {
	g := New((&net.Dialer{}).DialContext)
	host := "egress.tail.ts.net"
	path := filepath.Join(t.TempDir(), "routes.json")
	routes := []Route{{"app.example.com", "http://server:3000", true}}
	g.Replace(routes)
	h := g.Admin(path, host, func(context.Context, string) (string, error) { return "admin", nil })
	request := func(revision string, routes []Route) int {
		body, _ := json.Marshal(routes)
		r := httptest.NewRequest("PUT", "https://"+host+"/api/routes", bytes.NewReader(body))
		r.Header.Set("Origin", "https://"+host)
		r.Header.Set("Content-Type", "application/json")
		r.Header.Set("X-TS-Egress", "1")
		r.Header.Set("If-Match", revision)
		w := httptest.NewRecorder()
		h.ServeHTTP(w, r)
		return w.Code
	}
	fresh := strconv.FormatUint(g.Revision(), 10)
	if code := request(fresh, []Route{}); code != http.StatusNoContent {
		t.Fatal("fresh save failed", code)
	}
	if code := request(fresh, routes); code != http.StatusConflict {
		t.Fatal("stale save was not rejected", code)
	}
	if len(g.Routes()) != 0 {
		t.Fatal("stale save changed live routes")
	}
}

func BenchmarkRouteLookup(b *testing.B) {
	g := New((&net.Dialer{}).DialContext)
	g.Replace([]Route{{"app.example.com", "http://server", true}})
	for b.Loop() {
		_ = g.Allows(context.Background(), "app.example.com")
	}
}
