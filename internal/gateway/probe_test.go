package gateway

import (
	"context"
	"net"
	"net/http"
	"net/http/httptest"
	"sync/atomic"
	"testing"
	"time"
)

func TestBackendProbeSeparateFromHost(t *testing.T) {
	var status atomic.Int32
	status.Store(200)
	var calls atomic.Int32
	backend := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		calls.Add(1)
		if r.Method != "HEAD" {
			t.Errorf("probe method %s", r.Method)
		}
		w.WriteHeader(int(status.Load()))
	}))
	defer backend.Close()
	g := New((&net.Dialer{}).DialContext)
	g.Health = NewHealth(t.TempDir())
	routes := []Route{{"app.example.com", backend.URL, true}}
	g.Replace(routes)
	for _, tc := range []struct {
		code  int
		state string
	}{{200, "Online"}, {401, "Online"}, {405, "Online"}, {503, "Error"}} {
		status.Store(int32(tc.code))
		g.checkBackends(context.Background(), backend.Client())
		s := g.Health.Snapshot(routes)[routes[0].Domain]
		if s.Backend.State != tc.state || s.Backend.Checked.IsZero() || s.Host.State != "Unknown" {
			t.Fatalf("health %+v", s)
		}
	}
	g.Health.mu.Lock()
	s := g.Health.services[routes[0].Domain]
	s.Backend.Checked = time.Now().Add(-2 * time.Minute)
	g.Health.services[routes[0].Domain] = s
	g.Health.mu.Unlock()
	if g.Health.Snapshot(routes)[routes[0].Domain].Backend.State != "Stale" {
		t.Fatal("stale probe shown as current")
	}
	routes[0].Enabled = false
	g.Replace(routes)
	before := calls.Load()
	g.checkBackends(context.Background(), backend.Client())
	if calls.Load() != before || g.Health.Snapshot(routes)[routes[0].Domain].Backend.State != "Disabled" {
		t.Fatal("disabled service probed")
	}
}

func TestBackendProbeDoesNotFollowRedirect(t *testing.T) {
	var redirected atomic.Int32
	target := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) { redirected.Add(1) }))
	defer target.Close()
	backend := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) { http.Redirect(w, r, target.URL, 302) }))
	defer backend.Close()
	g := New((&net.Dialer{}).DialContext)
	g.Health = NewHealth(t.TempDir())
	g.Replace([]Route{{"app.example.com", backend.URL, true}})
	ctx, cancel := context.WithCancel(context.Background())
	go func() { time.Sleep(50 * time.Millisecond); cancel() }()
	g.MonitorBackends(ctx)
	if redirected.Load() != 0 {
		t.Fatal("probe followed redirect")
	}
}
