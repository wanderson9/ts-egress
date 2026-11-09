package gateway

import (
	"bufio"
	"context"
	"crypto/tls"
	"io"
	"net"
	"net/http"
	"net/http/httptest"
	"net/netip"
	"os"
	"path/filepath"
	"strconv"
	"strings"
	"sync"
	"testing"
	"time"
)

const authTestPassword = "correct horse battery"

func authTestGateway(t *testing.T, backend http.Handler) *Gateway {
	t.Helper()
	up := httptest.NewServer(backend)
	t.Cleanup(up.Close)
	g := New(func(ctx context.Context, _, _ string) (net.Conn, error) {
		return (&net.Dialer{}).DialContext(ctx, "tcp", up.Listener.Addr().String())
	})
	routes, err := PersistedRoutes([]Route{{Domain: "auth.example.com", Upstream: "http://backend.example.ts.net:80", Enabled: true, BasicAuthUser: "alice", BasicAuthPassword: authTestPassword}})
	if err != nil {
		t.Fatal(err)
	}
	g.Replace(routes)
	return g
}

func authTestRequest(remote, user, password string) *http.Request {
	r := httptest.NewRequest("GET", "https://auth.example.com/", nil)
	r.TLS = &tls.ConnectionState{ServerName: "auth.example.com"}
	r.RemoteAddr = remote
	if user != "" {
		r.SetBasicAuth(user, password)
	}
	return r
}

func ok() http.Handler {
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {})
}

func TestBasicAuthParallelValidRequestsAllSucceed(t *testing.T) {
	g := authTestGateway(t, ok())
	var wg sync.WaitGroup
	var mu sync.Mutex
	codes := map[int]int{}
	start := make(chan struct{})
	for range 64 {
		wg.Add(1)
		go func() {
			defer wg.Done()
			<-start
			w := httptest.NewRecorder()
			g.ServeHTTP(w, authTestRequest("198.51.100.7:1", "alice", authTestPassword))
			mu.Lock()
			codes[w.Code]++
			mu.Unlock()
		}()
	}
	close(start)
	wg.Wait()
	if codes[200] != 64 {
		t.Fatalf("valid parallel requests were rejected: %v", codes)
	}
}

func TestBasicAuthVerifiedCredentialSkipsBcryptSlots(t *testing.T) {
	g := authTestGateway(t, ok())
	first := httptest.NewRecorder()
	g.ServeHTTP(first, authTestRequest("198.51.100.7:1", "alice", authTestPassword))
	if first.Code != 200 {
		t.Fatal(first.Code)
	}
	for range cap(g.authSlots) {
		g.authSlots <- struct{}{}
	}
	w := httptest.NewRecorder()
	g.ServeHTTP(w, authTestRequest("198.51.100.8:1", "alice", authTestPassword))
	if w.Code != 200 {
		t.Fatalf("flooded slots blocked a verified credential: %d", w.Code)
	}
}

func TestBasicAuthQueueIsBounded(t *testing.T) {
	g := authTestGateway(t, ok())
	g.auth.wait = 20 * time.Millisecond
	for range cap(g.authSlots) {
		g.authSlots <- struct{}{}
	}
	w := httptest.NewRecorder()
	g.ServeHTTP(w, authTestRequest("198.51.100.7:1", "alice", authTestPassword))
	if w.Code != http.StatusServiceUnavailable || w.Header().Get("Retry-After") == "" {
		t.Fatalf("slot wait unbounded or unadvertised: %d", w.Code)
	}
}

func TestBasicAuthFailuresAreThrottledBySource(t *testing.T) {
	g := authTestGateway(t, ok())
	now := time.Now()
	g.auth.now = func() time.Time { return now }
	attempt := func(remote, password string) int {
		w := httptest.NewRecorder()
		g.ServeHTTP(w, authTestRequest(remote, "alice", password))
		return w.Code
	}
	for i := range authFailureLimit {
		if got := attempt("[2001:db8:1:2::1]:1", "wrong"); got != 401 {
			t.Fatalf("attempt %d: %d", i, got)
		}
	}
	if got := attempt("[2001:db8:1:2::1]:1", authTestPassword); got != 429 {
		t.Fatalf("correct guess after limit: %d", got)
	}
	if got := attempt("[2001:db8:1:2:ffff::9]:1", "wrong"); got != 429 {
		t.Fatalf("same /64 escaped the limit: %d", got)
	}
	if got := attempt("198.51.100.7:1", authTestPassword); got != 200 {
		t.Fatalf("unrelated client throttled: %d", got)
	}
	// Once verified, a credential keeps working for clients sharing the source.
	if got := attempt("[2001:db8:1:2::1]:1", authTestPassword); got != 200 {
		t.Fatalf("verified credential throttled: %d", got)
	}
	now = now.Add(authFailureWindow + time.Second)
	if got := attempt("[2001:db8:1:2::1]:1", "wrong"); got != 401 {
		t.Fatalf("throttle did not expire: %d", got)
	}
}

func TestBasicAuthWrongUsernameCostsSameAsWrongPassword(t *testing.T) {
	g := authTestGateway(t, ok())
	elapsed := func(user string) time.Duration {
		begin := time.Now()
		g.ServeHTTP(httptest.NewRecorder(), authTestRequest("198.51.100.7:1", user, "wrong"))
		return time.Since(begin)
	}
	known, unknown := elapsed("alice"), elapsed("mallory")
	if unknown < known/4 {
		t.Fatalf("username timing oracle: unknown=%v known=%v", unknown, known)
	}
}

func TestBasicAuthChallengeHeaderIsWellFormed(t *testing.T) {
	g := authTestGateway(t, ok())
	want := `Basic realm="auth.example.com", charset="UTF-8"`
	for _, r := range []*http.Request{authTestRequest("198.51.100.7:1", "", ""), authTestRequest("198.51.100.7:1", "alice", "wrong")} {
		w := httptest.NewRecorder()
		g.ServeHTTP(w, r)
		if got := w.Header().Get("WWW-Authenticate"); w.Code != 401 || got != want {
			t.Fatalf("%d %q", w.Code, got)
		}
	}
}

func TestBasicAuthRejectsUnhashedStoredPassword(t *testing.T) {
	g := New((&net.Dialer{}).DialContext)
	g.Replace([]Route{{Domain: "auth.example.com", Upstream: "http://backend", Enabled: true, BasicAuthUser: "alice", BasicAuthPassword: authTestPassword}})
	w := httptest.NewRecorder()
	g.ServeHTTP(w, authTestRequest("198.51.100.7:1", "alice", authTestPassword))
	if w.Code != 401 {
		t.Fatalf("plaintext verifier accepted: %d", w.Code)
	}
}

func TestBasicAuthClientPasswordMinimum(t *testing.T) {
	g := New((&net.Dialer{}).DialContext)
	h := g.Admin(filepath.Join(t.TempDir(), "routes.json"), "admin.example.com", func(context.Context, string) (string, error) { return "admin", nil })
	revision := func() map[string]string {
		return map[string]string{"If-Match": strconv.FormatUint(g.Revision(), 10)}
	}
	route := func(password string) []byte {
		return []byte(`[{"domain":"auth.example.com","upstream":"http://backend","enabled":true,"basicAuthUser":"u","basicAuthPassword":"` + password + `"}]`)
	}
	if w := adminTestRequest(h, "PUT", "/api/routes", route("short"), revision()); w.Code != 400 {
		t.Fatalf("short password accepted: %d", w.Code)
	}
	if w := adminTestRequest(h, "PUT", "/api/routes", route("twelve bytes"), revision()); w.Code != 204 {
		t.Fatalf("minimum-length password rejected: %d %s", w.Code, w.Body)
	}
	// A blank password keeps the stored verifier and must not trip the minimum.
	if w := adminTestRequest(h, "PUT", "/api/routes", route(""), revision()); w.Code != 204 {
		t.Fatalf("keeping stored password rejected: %d %s", w.Code, w.Body)
	}
}

func TestLoadMigratesPlaintextPasswords(t *testing.T) {
	path := filepath.Join(t.TempDir(), "routes.json")
	legacy := `[{"domain":"auth.example.com","upstream":"http://backend","enabled":true,"basicAuthUser":"u","basicAuthPassword":"short"}]`
	if err := os.WriteFile(path, []byte(legacy), 0o600); err != nil {
		t.Fatal(err)
	}
	routes, err := Load(path)
	if err != nil {
		t.Fatal(err)
	}
	if !strings.HasPrefix(routes[0].BasicAuthPassword, "bcrypt:") {
		t.Fatalf("not migrated in memory: %q", routes[0].BasicAuthPassword)
	}
	data, err := os.ReadFile(path)
	if err != nil {
		t.Fatal(err)
	}
	if strings.Contains(string(data), `"short"`) {
		t.Fatal("plaintext password left on disk")
	}
	if !verifyPassword(routes[0].BasicAuthPassword, "u", "u", "short") {
		t.Fatal("migrated verifier does not match the old password")
	}
}

func TestSpoofableHeadersAreStrippedOnTheWire(t *testing.T) {
	seen := make(chan http.Header, 1)
	up := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) { seen <- r.Header.Clone() }))
	defer up.Close()
	g := New(func(ctx context.Context, _, _ string) (net.Conn, error) {
		return (&net.Dialer{}).DialContext(ctx, "tcp", up.Listener.Addr().String())
	})
	g.Replace([]Route{{Domain: "open.example.com", Upstream: "http://backend.example.ts.net:80", Enabled: true}})
	front := httptest.NewServer(g)
	defer front.Close()
	c, err := net.Dial("tcp", front.Listener.Addr().String())
	if err != nil {
		t.Fatal(err)
	}
	defer c.Close()
	headers := []string{"Tailscale-User-Login", "Tailscale_User_Login", "X-Forwarded-For", "X_Forwarded_For", "X-Forwarded-Host", "X_Forwarded_Host", "Forwarded", "X-Real-IP", "X_Real_IP", "True-Client-IP", "CF-Connecting-IP", "X-Client-IP", "Fastly-Client-IP", "X-Custom-App"}
	request := "GET / HTTP/1.1\r\nHost: open.example.com\r\nConnection: close\r\n"
	for _, name := range headers {
		request += name + ": spoofed\r\n"
	}
	if _, err := io.WriteString(c, request+"\r\n"); err != nil {
		t.Fatal(err)
	}
	got := <-seen
	for name, values := range got {
		if strings.Contains(strings.Join(values, ","), "spoofed") && name != "X-Custom-App" {
			t.Errorf("backend received spoofed %s", name)
		}
	}
	if got.Get("X-Custom-App") != "spoofed" {
		t.Error("ordinary application header was removed")
	}
	if got.Get("X-Forwarded-Host") != "open.example.com" {
		t.Errorf("gateway forwarding header missing: %q", got.Get("X-Forwarded-Host"))
	}
}

func TestAdmissionGroupsIPv6ByPrefix(t *testing.T) {
	a := NewAdmission(512)
	release := make(chan struct{})
	var entered, done sync.WaitGroup
	h := a.Middleware(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) { entered.Done(); <-release }))
	ip := netip.MustParseAddr("2001:db8:1:2::1")
	for range 32 {
		entered.Add(1)
		done.Add(1)
		r := httptest.NewRequest("GET", "/", nil)
		r.RemoteAddr = net.JoinHostPort(ip.String(), "1")
		ip = ip.Next()
		go func() { defer done.Done(); h.ServeHTTP(httptest.NewRecorder(), r) }()
	}
	entered.Wait()
	same := httptest.NewRequest("GET", "/", nil)
	same.RemoteAddr = "[2001:db8:1:2:ffff::1]:1"
	w := httptest.NewRecorder()
	h.ServeHTTP(w, same)
	close(release)
	done.Wait()
	if w.Code != 429 {
		t.Fatalf("same /64 not limited: %d", w.Code)
	}
}

func TestPerRouteLimitProtectsOtherRoutes(t *testing.T) {
	g := New((&net.Dialer{}).DialContext)
	up := httptest.NewServer(ok())
	defer up.Close()
	g.Replace([]Route{{Domain: "busy.example.com", Upstream: up.URL, Enabled: true}, {Domain: "quiet.example.com", Upstream: up.URL, Enabled: true}})
	g.live.Load().state["busy.example.com"].inflight.Store(maxRouteRequests)
	code := func(host string) int {
		w := httptest.NewRecorder()
		g.ServeHTTP(w, httptest.NewRequest("GET", "http://"+host+"/", nil))
		return w.Code
	}
	if got := code("busy.example.com"); got != http.StatusServiceUnavailable {
		t.Fatalf("route over its limit: %d", got)
	}
	if got := g.live.Load().state["busy.example.com"].inflight.Load(); got != maxRouteRequests {
		t.Fatalf("rejected request leaked a slot: %d", got)
	}
	if got := code("quiet.example.com"); got != 200 {
		t.Fatalf("other route affected: %d", got)
	}
}

func TestBlockRangesAndLimits(t *testing.T) {
	path := filepath.Join(t.TempDir(), "blocks.json")
	b, err := OpenBlocks(path)
	if err != nil {
		t.Fatal(err)
	}
	if err := b.Set("2001:db8:1:2:aaaa::1/64", true); err != nil {
		t.Fatal(err)
	}
	if err := b.Set("198.51.100.0/24", true); err != nil {
		t.Fatal(err)
	}
	if err := b.Set("203.0.113.9", true); err != nil {
		t.Fatal(err)
	}
	if b, err = OpenBlocks(path); err != nil {
		t.Fatal(err)
	}
	for ip, want := range map[string]bool{
		"2001:db8:1:2::77":    true,
		"2001:db8:1:3::77":    false,
		"198.51.100.200":      true,
		"::ffff:198.51.100.1": true,
		"198.51.101.1":        false,
		"203.0.113.9":         true,
		"203.0.113.10":        false,
	} {
		if got := b.Contains(netip.MustParseAddr(ip)); got != want {
			t.Errorf("Contains(%s)=%v", ip, got)
		}
	}
	if got := strings.Join(b.List(), " "); got != "198.51.100.0/24 2001:db8:1:2::/64 203.0.113.9" {
		t.Fatalf("list: %s", got)
	}
	for _, bad := range []string{"0.0.0.0/0", "10.0.0.0/7", "::/0", "2001::/31", "::ffff:1.2.3.4/120", "not-an-ip", "1.2.3.4/33"} {
		if err := b.Set(bad, true); err == nil {
			t.Errorf("accepted %q", bad)
		}
	}
	if err := b.Set("198.51.100.0/24", false); err != nil || b.Contains(netip.MustParseAddr("198.51.100.200")) {
		t.Fatalf("range not removed: %v", err)
	}
}

func TestStreamEndsWhenRouteChanges(t *testing.T) {
	upstream := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		c, rw, err := http.NewResponseController(w).Hijack()
		if err != nil {
			return
		}
		defer c.Close()
		_, _ = rw.WriteString("HTTP/1.1 101 Switching Protocols\r\nConnection: Upgrade\r\nUpgrade: websocket\r\n\r\n")
		_ = rw.Flush()
		_, _ = io.Copy(c, c)
	}))
	defer upstream.Close()
	route := Route{Domain: "app.example.com", Upstream: upstream.URL, Enabled: true}
	for name, change := range map[string]func(*Gateway){
		"disabled": func(g *Gateway) { disabled := route; disabled.Enabled = false; g.Replace([]Route{disabled}) },
		"removed":  func(g *Gateway) { g.Replace(nil) },
		"credentials": func(g *Gateway) {
			protected := route
			protected.BasicAuthUser, protected.BasicAuthPassword = "u", authTestPassword
			g.Replace([]Route{protected})
		},
	} {
		t.Run(name, func(t *testing.T) {
			g := New((&net.Dialer{}).DialContext)
			g.Replace([]Route{route})
			front := httptest.NewServer(g)
			defer front.Close()
			c, err := net.Dial("tcp", front.Listener.Addr().String())
			if err != nil {
				t.Fatal(err)
			}
			defer c.Close()
			_ = c.SetDeadline(time.Now().Add(5 * time.Second))
			_, _ = io.WriteString(c, "GET / HTTP/1.1\r\nHost: app.example.com\r\nConnection: Upgrade\r\nUpgrade: websocket\r\n\r\n")
			r := bufio.NewReader(c)
			resp, err := http.ReadResponse(r, nil)
			if err != nil || resp.StatusCode != 101 {
				t.Fatal("upgrade failed", err)
			}
			// An unrelated save that leaves the route unchanged keeps the stream.
			g.Replace([]Route{route})
			_, _ = io.WriteString(c, "ping")
			buf := make([]byte, 4)
			if _, err := io.ReadFull(r, buf); err != nil {
				t.Fatalf("unchanged route lost its stream: %v", err)
			}
			change(g)
			if _, err := r.ReadByte(); err == nil {
				t.Fatal("stream survived the route change")
			} else if ne, ok := err.(net.Error); ok && ne.Timeout() {
				t.Fatal("stream still open after the route change")
			}
		})
	}
}
