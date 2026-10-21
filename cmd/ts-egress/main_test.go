package main

import (
	"context"
	"encoding/json"
	"errors"
	"net"
	"net/http"
	"net/http/httptest"
	"sync/atomic"
	"testing"
	"time"

	"tailscale.com/client/local"
	"tailscale.com/ipn/ipnstate"
	"ts-egress/internal/gateway"
	"ts-egress/internal/setup"
)

type failingNode struct{ starts, closes *atomic.Int32 }

func (n *failingNode) Start() error                                 { n.starts.Add(1); return errors.New("injected start failure") }
func (n *failingNode) Close() error                                 { n.closes.Add(1); return nil }
func (*failingNode) LocalClient() (*local.Client, error)            { return nil, errors.New("unused") }
func (*failingNode) Up(context.Context) (*ipnstate.Status, error)   { return nil, errors.New("unused") }
func (*failingNode) ListenTLS(string, string) (net.Listener, error) { return nil, errors.New("unused") }
func (*failingNode) Dial(context.Context, string, string) (net.Conn, error) {
	return nil, errors.New("unused")
}

type readyNode struct {
	failingNode
	client *local.Client
}

func (n *readyNode) Start() error                                 { n.starts.Add(1); return nil }
func (n *readyNode) LocalClient() (*local.Client, error)          { return n.client, nil }
func (n *readyNode) Up(context.Context) (*ipnstate.Status, error) { return &ipnstate.Status{}, nil }
func (n *readyNode) ListenTLS(string, string) (net.Listener, error) {
	return nil, errors.New("injected listener failure")
}

func TestListenerFailureAfterUpCanRetry(t *testing.T) {
	api := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		json.NewEncoder(w).Encode(&ipnstate.Status{BackendState: "Running", CertDomains: []string{"egress.ts.net"}, CurrentTailnet: &ipnstate.TailnetStatus{MagicDNSEnabled: true}})
	}))
	defer api.Close()
	lc := &local.Client{Dial: func(ctx context.Context, network, _ string) (net.Conn, error) {
		return (&net.Dialer{}).DialContext(ctx, network, api.Listener.Addr().String())
	}}
	store, err := setup.Open(t.TempDir())
	if err != nil {
		t.Fatal(err)
	}
	var starts, closes atomic.Int32
	rt := &runtime{ctx: context.Background(), store: store, newNode: func() tailnetNode { return &readyNode{failingNode{&starts, &closes}, lc} }}
	for range 2 {
		rt.Connect()
		rt.mu.RLock()
		done := rt.done
		rt.mu.RUnlock()
		select {
		case <-done:
		case <-time.After(time.Second):
			t.Fatal("listener failure did not unwind")
		}
		if status := rt.Status(context.Background()); status.State != "error" || status.Error != "injected listener failure" {
			t.Fatalf("status: %+v", status)
		}
	}
	if starts.Load() != 2 || closes.Load() != 2 {
		t.Fatalf("starts/closes %d/%d", starts.Load(), closes.Load())
	}
}

func TestLogoutKeepsNodeForReauthentication(t *testing.T) {
	var logouts, logins atomic.Int32
	api := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		switch r.URL.Path {
		case "/localapi/v0/logout":
			logouts.Add(1)
			w.WriteHeader(http.StatusNoContent)
		case "/localapi/v0/login-interactive":
			logins.Add(1)
			w.WriteHeader(http.StatusNoContent)
		default:
			json.NewEncoder(w).Encode(&ipnstate.Status{BackendState: "NeedsLogin"})
		}
	}))
	defer api.Close()
	lc := &local.Client{Dial: func(ctx context.Context, network, _ string) (net.Conn, error) {
		return (&net.Dialer{}).DialContext(ctx, network, api.Listener.Addr().String())
	}}
	var starts, closes atomic.Int32
	node := &readyNode{failingNode{&starts, &closes}, lc}
	rt := &runtime{ctx: context.Background(), ts: node, gateway: gateway.New((&net.Dialer{}).DialContext)}
	if err := rt.Logout(context.Background()); err != nil {
		t.Fatal(err)
	}
	if rt.ts != node || closes.Load() != 0 || logouts.Load() != 1 {
		t.Fatal("logout tore down node before recovery")
	}
	if err := rt.Reauthenticate(context.Background()); err != nil {
		t.Fatal(err)
	}
	if logins.Load() != 2 {
		t.Fatalf("interactive login calls %d", logins.Load())
	}
}

func TestConnectFailureCleansNodeAndCanRetry(t *testing.T) {
	store, err := setup.Open(t.TempDir())
	if err != nil {
		t.Fatal(err)
	}
	var starts, closes atomic.Int32
	rt := &runtime{ctx: context.Background(), store: store, stateDir: t.TempDir(), newNode: func() tailnetNode { return &failingNode{&starts, &closes} }}
	for attempt := 0; attempt < 2; attempt++ {
		rt.Connect()
		rt.mu.RLock()
		done := rt.done
		rt.mu.RUnlock()
		<-done
		rt.mu.RLock()
		node, state := rt.ts, rt.status.State
		rt.mu.RUnlock()
		if node != nil {
			t.Fatal("failed node retained")
		}
		if state != "error" {
			t.Fatalf("state = %q", state)
		}
	}
	if starts.Load() != 2 || closes.Load() != 2 {
		t.Fatalf("start/close counts = %d/%d", starts.Load(), closes.Load())
	}
}

func TestConnectShutdownCancelsAttemptAndClosesNode(t *testing.T) {
	store, err := setup.Open(t.TempDir())
	if err != nil {
		t.Fatal(err)
	}
	ctx, cancel := context.WithCancel(context.Background())
	started := make(chan struct{})
	upStarted := make(chan struct{})
	var starts, closes atomic.Int32
	rt := &runtime{ctx: ctx, store: store, stateDir: t.TempDir(), newNode: func() tailnetNode {
		return &cancelNode{failingNode: failingNode{&starts, &closes}, started: started, upStarted: upStarted}
	}}
	rt.Connect()
	<-started
	select {
	case <-upStarted:
	case <-time.After(time.Second):
		t.Fatal("startup never reached Up")
	}
	cancel()
	rt.mu.RLock()
	done := rt.done
	rt.mu.RUnlock()
	select {
	case <-done:
	case <-time.After(time.Second):
		t.Fatal("runtime shutdown did not finish")
	}
	if closes.Load() != 1 || rt.ts != nil {
		t.Fatalf("shutdown leaked node: closes=%d node=%v", closes.Load(), rt.ts)
	}
}

type cancelNode struct {
	failingNode
	started   chan struct{}
	upStarted chan struct{}
}

func (n *cancelNode) LocalClient() (*local.Client, error) { return &local.Client{}, nil }

func (n *cancelNode) Start() error { n.starts.Add(1); close(n.started); return nil }
func (n *cancelNode) Up(ctx context.Context) (*ipnstate.Status, error) {
	close(n.upStarted)
	<-ctx.Done()
	return nil, ctx.Err()
}

func TestConnectCoalescesConcurrentAttempts(t *testing.T) {
	store, err := setup.Open(t.TempDir())
	if err != nil {
		t.Fatal(err)
	}
	started := make(chan struct{})
	release := make(chan struct{})
	var starts, closes atomic.Int32
	rt := &runtime{ctx: context.Background(), store: store, stateDir: t.TempDir(), newNode: func() tailnetNode {
		return &blockingNode{failingNode: failingNode{&starts, &closes}, started: started, release: release}
	}}
	rt.Connect()
	<-started
	for range 10 {
		rt.Connect()
	}
	if starts.Load() != 1 {
		t.Fatalf("concurrent Connect started %d nodes", starts.Load())
	}
	close(release)
	rt.mu.RLock()
	done := rt.done
	rt.mu.RUnlock()
	<-done
	if closes.Load() != 1 {
		t.Fatalf("close count = %d", closes.Load())
	}
}

type blockingNode struct {
	failingNode
	started chan struct{}
	release chan struct{}
}

func (n *blockingNode) Start() error {
	n.starts.Add(1)
	close(n.started)
	<-n.release
	return errors.New("injected start failure")
}
