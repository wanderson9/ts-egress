package gateway

import (
	"bufio"
	"context"
	"crypto/tls"
	"crypto/x509"
	"fmt"
	"io"
	"net"
	"net/http"
	"net/http/httptest"
	"net/netip"
	"path/filepath"
	"runtime"
	"strings"
	"sync/atomic"
	"testing"
	"time"
)

type roundTripFunc func(*http.Request) (*http.Response, error)

func (f roundTripFunc) RoundTrip(r *http.Request) (*http.Response, error) { return f(r) }

func TestUpstreamTransportRoutesUpgrades(t *testing.T) {
	var used string
	tr := upstreamTransport{
		normal:  roundTripFunc(func(*http.Request) (*http.Response, error) { used = "normal"; return nil, nil }),
		upgrade: roundTripFunc(func(*http.Request) (*http.Response, error) { used = "upgrade"; return nil, nil }),
	}
	tr.RoundTrip(httptest.NewRequest("GET", "http://app.example.com/", nil))
	if used != "normal" {
		t.Fatalf("plain request used %q transport", used)
	}
	ws := httptest.NewRequest("GET", "http://app.example.com/", nil)
	ws.Header.Set("Connection", "keep-alive, Upgrade")
	ws.Header.Set("Upgrade", "websocket")
	tr.RoundTrip(ws)
	if used != "upgrade" {
		t.Fatalf("upgrade request used %q transport", used)
	}
}

func TestUpgradeTransportPinsHTTP1(t *testing.T) {
	g := New((&net.Dialer{}).DialContext)
	if _, ok := g.transport.TLSNextProto["h2"]; !ok {
		t.Fatal("ordinary transport should offer HTTP/2 to upstreams")
	}
	if len(g.upgradeTransport.TLSNextProto) != 0 {
		t.Fatalf("upgrade transport should disable HTTP/2, got %v", g.upgradeTransport.TLSNextProto)
	}
}

func TestActivityUpgradeOverHTTPSUpstream(t *testing.T) {
	// An HTTPS upstream that advertises HTTP/2 must still serve upgrades over
	// HTTP/1.1: Go's transport only pins HTTP/1.1 for "websocket", so the
	// gateway's upgrade transport has to pin it for every upgrade protocol.
	for _, proto := range []string{"websocket", "echo"} {
		t.Run(proto, func(t *testing.T) {
			up := httptest.NewUnstartedServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
				conn, rw, err := w.(http.Hijacker).Hijack()
				if err != nil {
					return
				}
				defer conn.Close()
				rw.WriteString("HTTP/1.1 101 Switching Protocols\r\nConnection: Upgrade\r\nUpgrade: " + proto + "\r\n\r\n")
				rw.Flush()
				buf := make([]byte, 4)
				if _, err := io.ReadFull(rw, buf); err == nil {
					rw.Write(buf)
					rw.Flush()
				}
			}))
			up.EnableHTTP2 = true // advertise h2 over ALPN; upgrades must still use HTTP/1.1
			up.StartTLS()
			defer up.Close()
			g := New((&net.Dialer{}).DialContext)
			pool := x509.NewCertPool()
			pool.AddCert(up.Certificate())
			// Mutate the existing config so a transport that auto-configures
			// HTTP/2 keeps doing so; only trust our test certificate.
			for _, tr := range []*http.Transport{g.transport, g.upgradeTransport} {
				if tr.TLSClientConfig == nil {
					tr.TLSClientConfig = &tls.Config{}
				}
				tr.TLSClientConfig.RootCAs = pool
			}
			g.Replace([]Route{{"app.example.com", up.URL, true, "", "", false}})
			public := httptest.NewServer(g)
			defer public.Close()
			conn, err := net.DialTimeout("tcp", strings.TrimPrefix(public.URL, "http://"), time.Second)
			if err != nil {
				t.Fatal(err)
			}
			defer conn.Close()
			conn.SetDeadline(time.Now().Add(3 * time.Second))
			io.WriteString(conn, "GET / HTTP/1.1\r\nHost: app.example.com\r\nConnection: Upgrade\r\nUpgrade: "+proto+"\r\n\r\n")
			reader := bufio.NewReader(conn)
			resp, err := http.ReadResponse(reader, nil)
			if err != nil || resp.StatusCode != 101 {
				t.Fatalf("upgrade over HTTPS upstream failed: %v (status %v)", err, resp.StatusCode)
			}
			io.WriteString(conn, "ping")
			buf := make([]byte, 4)
			if _, err := io.ReadFull(reader, buf); err != nil || string(buf) != "ping" {
				t.Fatal("upgrade data over HTTPS upstream failed", err)
			}
		})
	}
}

func TestActivityUpgrade(t *testing.T) {
	up := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		conn, rw, err := w.(http.Hijacker).Hijack()
		if err != nil {
			return
		}
		defer conn.Close()
		rw.WriteString("HTTP/1.1 101 Switching Protocols\r\nConnection: Upgrade\r\nUpgrade: echo\r\n\r\n")
		rw.Flush()
		buf := make([]byte, 4)
		if _, err := io.ReadFull(rw, buf); err == nil {
			rw.Write(buf)
			rw.Flush()
		}
	}))
	defer up.Close()
	g := New((&net.Dialer{}).DialContext)
	g.Replace([]Route{{"app.example.com", up.URL, true, "", "", false}})
	public := httptest.NewServer(g)
	defer public.Close()
	conn, err := net.DialTimeout("tcp", strings.TrimPrefix(public.URL, "http://"), time.Second)
	if err != nil {
		t.Fatal(err)
	}
	defer conn.Close()
	conn.SetDeadline(time.Now().Add(3 * time.Second))
	io.WriteString(conn, "GET / HTTP/1.1\r\nHost: app.example.com\r\nConnection: Upgrade\r\nUpgrade: echo\r\n\r\n")
	reader := bufio.NewReader(conn)
	resp, err := http.ReadResponse(reader, nil)
	if err != nil || resp.StatusCode != 101 {
		t.Fatal("upgrade failed", err)
	}
	io.WriteString(conn, "ping")
	buf := make([]byte, 4)
	if _, err := io.ReadFull(reader, buf); err != nil || string(buf) != "ping" {
		t.Fatal("upgrade data failed", err)
	}
	// Receiving the echo does not synchronize with the upload goroutine's
	// accounting after its upstream Write returns. Wait for both directions.
	deadline := time.Now().Add(time.Second)
	for {
		s := g.Activity.Snapshot("app.example.com")
		if s.Received == 4 && s.Sent == 4 && s.Requests == 1 {
			break
		}
		if time.Now().After(deadline) {
			t.Fatal("upgrade not counted", s)
		}
		time.Sleep(time.Millisecond)
	}
}

func TestActivityProxy(t *testing.T) {
	up := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		io.Copy(io.Discard, r.Body)
		w.WriteHeader(503)
		w.Write([]byte("response"))
	}))
	defer up.Close()
	g := New((&net.Dialer{}).DialContext)
	routes := []Route{{"app.example.com", up.URL, true, "", "", false}}
	g.Replace(routes)
	r := httptest.NewRequest("POST", "http://app.example.com/", strings.NewReader("upload"))
	r.RemoteAddr = "203.0.113.4:1234"
	r.Header.Set("X-Forwarded-For", "1.2.3.4")
	g.ServeHTTP(httptest.NewRecorder(), r)
	s := g.Activity.Snapshot("app.example.com")
	if s.Requests != 1 || s.Received != 6 || s.Sent != 8 || s.Errors != 1 || len(s.Clients) != 1 || s.Clients[0].IP != "203.0.113.4" {
		t.Fatalf("bad counters: %+v", s)
	}
	routes[0].Enabled = false
	g.Replace(routes)
	if g.Activity.Snapshot("app.example.com").Requests != 1 {
		t.Fatal("disable cleared activity")
	}
}
func TestActivityWindowAndLimit(t *testing.T) {
	var a Activity
	now := time.Now()
	ip := netip.MustParseAddr("8.8.8.8")
	a.recordAt("app.example.com", ip, Totals{Requests: 1}, now.Add(-25*time.Hour))
	a.recordAt("app.example.com", ip, Totals{Requests: 2}, now)
	if a.Snapshot("app.example.com").Requests != 2 {
		t.Fatal("old bucket retained")
	}
	shard := &a.shards[shardFor("app.example.com")]
	shard.mu.Lock()
	shard.records = maxActivityRecords/len(a.shards) + 1
	shard.mu.Unlock()
	a.record("app.example.com", netip.MustParseAddr("1.1.1.1"), Totals{Requests: 1})
	s := a.Snapshot("app.example.com")
	if !s.Limited || s.Requests != 3 {
		t.Fatal("overflow not aggregated")
	}
}
func TestGeoBudgetCacheAndPrivate(t *testing.T) {
	var calls atomic.Int32
	provider := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		calls.Add(1)
		w.Write([]byte(`{"success":true,"country":"United States","country_code":"US"}`))
	}))
	defer provider.Close()
	dir := t.TempDir()
	g := NewGeo(dir)
	if err := g.SetEnabled(true); err != nil {
		t.Fatal(err)
	}
	g.endpoint = provider.URL + "/"
	clients := []ClientActivity{{IP: "8.8.8.8"}, {IP: "1.1.1.1"}, {IP: "9.9.9.9"}, {IP: "8.8.4.4"}, {IP: "1.0.0.1"}, {IP: "208.67.222.222"}}
	for range 10 {
		g.Countries(clients)
	}
	deadline := time.Now().Add(3 * time.Second)
	for time.Now().Before(deadline) {
		g.mu.Lock()
		done := len(g.pending) == 0
		g.mu.Unlock()
		if done {
			break
		}
		time.Sleep(time.Millisecond * 10)
	}
	if calls.Load() != 5 {
		t.Fatalf("expected 5 lookups got %d", calls.Load())
	}
	got := g.Countries(clients)
	if got["8.8.8.8"].Flag != "🇺🇸" {
		t.Fatal("flag missing", got)
	}
	restored := NewGeo(dir)
	restored.endpoint = provider.URL + "/"
	restored.Countries(clients)
	if calls.Load() != 5 {
		t.Fatal("restart reset minute quota")
	}
	for _, s := range []string{"127.0.0.1", "100.100.1.1", "192.168.1.1", "::1", "fd7a:115c:a1e0::1", "2001:db8::1"} {
		if publicIP(netip.MustParseAddr(s)) {
			t.Fatal("private/reserved IP accepted", s)
		}
	}
}
func TestGeoDisabledDoesNotQueueLookups(t *testing.T) {
	var calls atomic.Int32
	provider := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) { calls.Add(1) }))
	defer provider.Close()
	g := NewGeo(t.TempDir())
	g.endpoint = provider.URL + "/"
	g.Countries([]ClientActivity{{IP: "8.8.8.8"}})
	time.Sleep(30 * time.Millisecond)
	if calls.Load() != 0 {
		t.Fatal("lookup ran while geolocation disabled")
	}
}

func TestGeoDisabledByDefaultAndExplicitEnable(t *testing.T) {
	dir := t.TempDir()
	g := NewGeo(dir)
	if g.Enabled() {
		t.Fatal("geolocation should default to disabled")
	}
	if err := g.SetEnabled(true); err != nil {
		t.Fatal(err)
	}
	if !g.Enabled() {
		t.Fatal("explicit enable did not take effect")
	}
	reopened := NewGeo(dir)
	if !reopened.Enabled() {
		t.Fatal("geolocation preference was not persisted")
	}
}
func TestGeoEnablePersistenceFailureDoesNotClaimSuccess(t *testing.T) {
	g := NewGeo(t.TempDir())
	g.path = filepath.Join(t.TempDir(), "missing-dir", "geo.json")
	if err := g.SetEnabled(true); err == nil {
		t.Fatal("expected persistence error")
	}
	if g.Enabled() {
		t.Fatal("failed preference write changed in-memory setting")
	}
}
func TestGeo429AndDailyQuota(t *testing.T) {
	var calls atomic.Int32
	provider := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		calls.Add(1)
		w.Header().Set("Retry-After", "3600")
		w.WriteHeader(429)
	}))
	defer provider.Close()
	dir := t.TempDir()
	g := NewGeo(dir)
	if err := g.SetEnabled(true); err != nil {
		t.Fatal(err)
	}
	g.endpoint = provider.URL + "/"
	g.Countries([]ClientActivity{{IP: "8.8.8.8"}})
	deadline := time.Now().Add(time.Second)
	for time.Now().Before(deadline) {
		g.mu.Lock()
		done := len(g.pending) == 0
		g.mu.Unlock()
		if done {
			break
		}
		time.Sleep(10 * time.Millisecond)
	}
	restored := NewGeo(dir)
	if !restored.quota.BlockedUntil.After(time.Now().Add(50 * time.Minute)) {
		t.Fatal("429 backoff not persisted")
	}
	g = NewGeo(t.TempDir())
	if err := g.SetEnabled(true); err != nil {
		t.Fatal(err)
	}
	for range 900 {
		g.quota.Attempts = append(g.quota.Attempts, time.Now().Add(-2*time.Hour))
	}
	g.endpoint = provider.URL + "/"
	g.Countries([]ClientActivity{{IP: "1.1.1.1"}})
	if len(g.pending) != 0 {
		t.Fatal("daily budget exceeded")
	}
}
func BenchmarkActivityRecord(b *testing.B) {
	var a Activity
	ip := netip.MustParseAddr("8.8.8.8")
	for b.Loop() {
		a.record("app.example.com", ip, Totals{Requests: 1})
	}
}
func BenchmarkActivityRecordParallel(b *testing.B) {
	var a Activity
	ip := netip.MustParseAddr("8.8.8.8")
	b.RunParallel(func(pb *testing.PB) {
		for pb.Next() {
			a.record("app.example.com", ip, Totals{Requests: 1})
		}
	})
}
func TestActivityClientResponseIsBounded(t *testing.T) {
	var a Activity
	now := time.Now()
	for i := range 1200 {
		ip := netip.AddrFrom4([4]byte{10, byte(i >> 16), byte(i >> 8), byte(i)})
		a.recordAt("app.example.com", ip, Totals{Requests: 1}, now)
	}
	s := a.Snapshot("app.example.com")
	if len(s.Clients) > maxActivityClientsPerSite || !s.Limited {
		t.Fatalf("unbounded snapshot: clients=%d limited=%v", len(s.Clients), s.Limited)
	}
	var sum uint64
	for _, c := range s.Clients {
		sum += c.Requests
	}
	if sum != s.Requests {
		t.Fatalf("bounded response discarded activity: clients total=%d site total=%d", sum, s.Requests)
	}
	if len(s.Clients) != maxActivityClientsPerSite || s.Clients[len(s.Clients)-1].IP != "Other / limited" {
		t.Fatalf("expected an aggregate row at the response cap: %+v", s.Clients[len(s.Clients)-1])
	}
}
func TestActivitySupportsRouteLimitOnCollidingShard(t *testing.T) {
	var a Activity
	now := time.Now()
	ip := netip.MustParseAddr("8.8.8.8")
	const routes = 1000
	var domains []string
	for i := 0; len(domains) < routes; i++ {
		domain := fmt.Sprintf("route-%d.example", i)
		if shardFor(domain) == 7 {
			domains = append(domains, domain)
		}
	}
	for _, d := range domains {
		a.recordAt(d, ip, Totals{Requests: 1}, now)
	}
	for _, d := range domains {
		if got := a.Snapshot(d).Requests; got != 1 {
			t.Fatalf("domain %s lost totals: %d", d, got)
		}
	}
}
func TestActivityRetainedMemory(t *testing.T) {
	runtime.GC()
	var before, after runtime.MemStats
	runtime.ReadMemStats(&before)
	var a Activity
	now := time.Now()
	for i := range maxActivityRecords {
		ip := netip.AddrFrom4([4]byte{11, byte(i >> 16), byte(i >> 8), byte(i)})
		a.recordAt("app.example.com", ip, Totals{Requests: 1}, now.Add(-time.Duration(i%288)*5*time.Minute))
	}
	runtime.GC()
	runtime.ReadMemStats(&after)
	runtime.KeepAlive(&a)
	if after.HeapAlloc > before.HeapAlloc {
		t.Logf("retained heap for 50k site/IP bucket records: %.2f MiB", float64(after.HeapAlloc-before.HeapAlloc)/1024/1024)
	}
}
func TestActivityAPI(t *testing.T) {
	g := New((&net.Dialer{}).DialContext)
	g.Replace([]Route{{"app.example.com", "http://server", true, "", "", false}})
	g.Activity.record("app.example.com", netip.MustParseAddr("8.8.8.8"), Totals{Requests: 1})
	h := g.Admin("unused", "egress.tail.ts.net", func(context.Context, string) (string, error) { return "admin", nil })
	w := httptest.NewRecorder()
	h.ServeHTTP(w, httptest.NewRequest("GET", "https://egress.tail.ts.net/api/activity?domain=app.example.com", nil))
	if w.Code != 200 || !strings.Contains(w.Body.String(), "8.8.8.8") {
		t.Fatal(w.Code, w.Body.String())
	}
}
