package gateway

import (
	"context"
	"net"
	"net/http"
	"net/http/httptest"
	"net/netip"
	"path/filepath"
	"strings"
	"testing"
)

func TestBlocksPersistAndRejectPublicRequests(t *testing.T) {
	path := filepath.Join(t.TempDir(), "blocked-ips.json")
	b, err := OpenBlocks(path)
	if err != nil {
		t.Fatal(err)
	}
	if err = b.Set("::ffff:8.8.8.8", true); err != nil {
		t.Fatal(err)
	}
	b, err = OpenBlocks(path)
	if err != nil {
		t.Fatal(err)
	}
	if !b.Contains(netip.MustParseAddr("8.8.8.8")) {
		t.Fatal("block not persisted/canonicalized")
	}
	g := New((&net.Dialer{}).DialContext)
	g.Blocks = b
	g.Replace([]Route{{"app.example.com", "http://server:3000", true}})
	r := httptest.NewRequest("GET", "http://app.example.com/", nil)
	r.RemoteAddr = "8.8.8.8:1234"
	r.Header.Set("X-Forwarded-For", "1.1.1.1")
	w := httptest.NewRecorder()
	g.ServeHTTP(w, r)
	if w.Code != http.StatusForbidden || g.Requests.Load() != 0 {
		t.Fatal("blocked request reached proxy", w.Code)
	}
	if err = b.Set("8.8.8.8", false); err != nil {
		t.Fatal(err)
	}
	b, err = OpenBlocks(path)
	if err != nil || b.Contains(netip.MustParseAddr("8.8.8.8")) {
		t.Fatal("unblock not persisted", err)
	}
}

func TestBlockAPIRequiresIdentityAndOrigin(t *testing.T) {
	g := New((&net.Dialer{}).DialContext)
	var err error
	g.Blocks, err = OpenBlocks(filepath.Join(t.TempDir(), "blocked-ips.json"))
	if err != nil {
		t.Fatal(err)
	}
	for _, tc := range []struct {
		actor, origin string
		code          int
	}{{"", "https://egress.ts.net", 403}, {"admin", "https://evil.example", 403}, {"admin", "https://egress.ts.net", 204}} {
		h := g.Admin("unused", "egress.ts.net", func(context.Context, string) (string, error) { return tc.actor, nil })
		r := httptest.NewRequest("PUT", "https://egress.ts.net/api/blocks", strings.NewReader(`{"ip":"8.8.8.8","blocked":true}`))
		r.Header.Set("Origin", tc.origin)
		r.Header.Set("Content-Type", "application/json")
		r.Header.Set("X-TS-Egress", "1")
		w := httptest.NewRecorder()
		h.ServeHTTP(w, r)
		if w.Code != tc.code {
			t.Fatal(w.Code, w.Body.String())
		}
	}
	g.Blocks.path = filepath.Join(t.TempDir(), "missing", "blocks.json")
	if err := g.Blocks.Set("1.1.1.1", true); err == nil || g.Blocks.Contains(netip.MustParseAddr("1.1.1.1")) {
		t.Fatal("failed write applied block")
	}
}

func TestSettingsPageRequiresIdentity(t *testing.T) {
	g := New((&net.Dialer{}).DialContext)
	for _, actor := range []string{"", "admin"} {
		h := g.Admin("unused", "egress.ts.net", func(context.Context, string) (string, error) { return actor, nil })
		w := httptest.NewRecorder()
		h.ServeHTTP(w, httptest.NewRequest("GET", "https://egress.ts.net/settings", nil))
		if actor == "" && w.Code != 403 {
			t.Fatal(w.Code)
		}
		if actor != "" && (w.Code != 200 || !strings.Contains(w.Body.String(), `data-page="settings"`)) {
			t.Fatal(w.Code, w.Body.String())
		}
	}
}

func TestClientRankingUsesCombinedTraffic(t *testing.T) {
	var a Activity
	a.record("app.example.com", netip.MustParseAddr("1.1.1.1"), Totals{Requests: 100, Received: 10, Sent: 10})
	a.record("app.example.com", netip.MustParseAddr("8.8.8.8"), Totals{Requests: 1, Received: 100, Sent: 1000})
	s := a.Snapshot("app.example.com")
	if len(s.Clients) != 2 || s.Clients[0].IP != "8.8.8.8" {
		t.Fatalf("clients not traffic ranked: %+v", s.Clients)
	}
}
