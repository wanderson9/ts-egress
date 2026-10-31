package gateway

import (
	"context"
	"crypto/rand"
	"crypto/rsa"
	"crypto/tls"
	"crypto/x509"
	"encoding/pem"
	"errors"
	"math/big"
	"net"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"
)

func TestCertificateRetryAPI(t *testing.T) {
	g := New((&net.Dialer{}).DialContext)
	g.Health = NewHealth(t.TempDir())
	g.Replace([]Route{{"app.example.com", "http://server", true, "", "", false}})
	g.Health.Certificate("app.example.com", nil, errors.New("DNS validation failed"))
	if got := g.Health.Snapshot(g.Routes())["app.example.com"].Certificate; got.Detail != "DNS validation failed" || got.Attempt != "Failed" {
		t.Fatalf("failure diagnostic lost: %+v", got)
	}
	g.Health.failedAt["app.example.com"] = time.Now().Add(-2 * time.Minute)
	called := make(chan string, 1)
	g.RetryCertificate = func(_ context.Context, domain string) (*tls.Certificate, error) {
		called <- domain
		return nil, errors.New("still failed")
	}
	h := g.Admin("unused", "egress.tail.ts.net", func(context.Context, string) (string, error) { return "admin", nil })
	send := func(origin string) int {
		r := httptest.NewRequest("POST", "https://egress.tail.ts.net/api/certificate/retry", strings.NewReader(`{"domain":"app.example.com"}`))
		r.Header.Set("Origin", origin)
		r.Header.Set("Content-Type", "application/json")
		r.Header.Set("X-TS-Egress", "1")
		w := httptest.NewRecorder()
		ctx, cancel := context.WithCancel(r.Context())
		h.ServeHTTP(w, r.WithContext(ctx))
		cancel() // net/http cancels the request after the handler returns.
		return w.Code
	}
	if code := send("https://evil.example"); code != http.StatusForbidden {
		t.Fatal(code)
	}
	if code := send("https://egress.tail.ts.net"); code != http.StatusAccepted {
		t.Fatal(code)
	}
	select {
	case domain := <-called:
		if domain != "app.example.com" {
			t.Fatal(domain)
		}
	case <-time.After(time.Second):
		t.Fatal("retry did not run")
	}
	if code := send("https://egress.tail.ts.net"); code != http.StatusConflict {
		t.Fatal("duplicate retry accepted", code)
	}
}

func TestServiceHealth(t *testing.T) {
	dir := t.TempDir()
	h := NewHealth(dir)
	routes := []Route{{"app.example.com", "http://server:3000", true, "", "", false}}
	h.SetPeers(map[string]PeerHealth{"server": {State: "Offline"}})
	h.Reconcile(routes)
	h.ResponseForUpstream("app.example.com", "http://server:3000", "connection refused")
	s := h.Snapshot(routes)["app.example.com"]
	if s.Host.State != "Offline" || s.Certificate.State != "Not issued" {
		t.Fatal(s)
	}
	key, _ := rsa.GenerateKey(rand.Reader, 2048)
	template := &x509.Certificate{SerialNumber: big.NewInt(1), DNSNames: []string{"app.example.com"}, NotBefore: time.Now().Add(-time.Hour), NotAfter: time.Now().Add(24 * time.Hour)}
	der, err := x509.CreateCertificate(rand.Reader, template, template, &key.PublicKey, key)
	if err != nil {
		t.Fatal(err)
	}
	os.WriteFile(filepath.Join(dir, "app.example.com"), pem.EncodeToMemory(&pem.Block{Type: "CERTIFICATE", Bytes: der}), 0600)
	h.mu.Lock()
	h.cacheRefreshed["app.example.com"] = time.Time{}
	h.mu.Unlock()
	if s := h.Snapshot(routes)["app.example.com"]; s.Certificate.State != "Valid" {
		t.Fatal(s)
	}
	h.Issuing("app.example.com")
	// A background renewal updates the disk cache but not the TLS callback.
	h.Certificate("app.example.com", &tls.Certificate{Certificate: [][]byte{der}}, nil)
	template.SerialNumber = big.NewInt(2)
	template.NotAfter = time.Now().Add(60 * 24 * time.Hour)
	renewed, err := x509.CreateCertificate(rand.Reader, template, template, &key.PublicKey, key)
	if err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(dir, "app.example.com"), pem.EncodeToMemory(&pem.Block{Type: "CERTIFICATE", Bytes: renewed}), 0600); err != nil {
		t.Fatal(err)
	}
	h.mu.Lock()
	h.cacheRefreshed["app.example.com"] = time.Time{}
	h.mu.Unlock()
	if s := h.Snapshot(routes)["app.example.com"]; s.Certificate.Expires.Before(time.Now().Add(59 * 24 * time.Hour)) {
		t.Fatal("renewal not reflected", s)
	}
	h.Issuing("app.example.com")
	if h.Snapshot(routes)["app.example.com"].Certificate.Attempt != "Issuing" {
		t.Fatal("missing issuance state")
	}
	h.Certificate("app.example.com", &tls.Certificate{Certificate: [][]byte{der}}, nil)
	h.Response("app.example.com", "connection refused")
	s = h.Snapshot(routes)["app.example.com"]
	if s.LastError != "connection refused" || s.Certificate.State != "Valid" {
		t.Fatal(s)
	}
	h.ResponseForUpstream("app.example.com", "http://server:3000", "old upstream failure")
	h.Reconcile([]Route{{"app.example.com", "http://new-server:3000", true, "", "", false}})
	updated := []Route{{"app.example.com", "http://new-server:3000", true, "", "", false}}
	if s := h.Snapshot(updated)["app.example.com"]; s.LastError != "" || !s.LastResponse.IsZero() {
		t.Fatal("upstream health carried across backend change", s)
	}
	h.ResponseForUpstream("app.example.com", "http://server:3000", "late old response")
	if s := h.Snapshot(updated)["app.example.com"]; s.LastError != "" {
		t.Fatal("late old response overwrote current route", s)
	}
	h.Response("removed.example.com", "stale")
	h.Reconcile(nil)
	if len(h.services) != 0 {
		t.Fatal("removed route health retained")
	}
	h.Response("app.example.com", "")
	if h.Snapshot(routes)["app.example.com"].LastError != "" {
		t.Fatal("stale error")
	}
}

func TestCertificateRetryCanBeCancelled(t *testing.T) {
	h := NewHealth(t.TempDir())
	h.Reconcile([]Route{{"app.example.com", "http://server:3000", true, "", "", false}})
	h.Certificate("app.example.com", nil, errors.New("failed"))
	h.mu.Lock()
	h.failedAt["app.example.com"] = time.Now()
	h.mu.Unlock()
	ctx, cancel := context.WithCancel(context.Background())
	called := make(chan struct{}, 1)
	if !h.RetryContext(ctx, "app.example.com", func(context.Context, string) (*tls.Certificate, error) { called <- struct{}{}; return nil, nil }) {
		t.Fatal("retry not accepted")
	}
	cancel()
	select {
	case <-called:
		t.Fatal("cancelled retry request ran")
	case <-time.After(20 * time.Millisecond):
	}
	if h.Snapshot([]Route{{"app.example.com", "http://server:3000", true, "", "", false}})["app.example.com"].Certificate.Attempt == "Issuing" {
		t.Fatal("cancel left stale issuing status")
	}
}

func TestCertificateRetryUsesServiceLifetime(t *testing.T) {
	h := NewHealth(t.TempDir())
	domain := "app.example.com"
	h.Reconcile([]Route{{domain, "http://server:3000", true, "", "", false}})
	h.Certificate(domain, nil, errors.New("failed"))
	h.mu.Lock()
	h.failedAt[domain] = time.Now().Add(-2 * time.Minute)
	h.mu.Unlock()
	serviceCtx, cancel := context.WithCancel(context.Background())
	defer cancel()
	called := make(chan struct{}, 1)
	if !h.RetryContext(serviceCtx, domain, func(context.Context, string) (*tls.Certificate, error) { called <- struct{}{}; return nil, nil }) {
		t.Fatal("retry rejected")
	}
	select {
	case <-called:
	case <-time.After(time.Second):
		t.Fatal("retry ended with HTTP request lifetime")
	}
}

func TestCancelledActiveRetryCannotRestoreRemovedRouteHealth(t *testing.T) {
	h := NewHealth(t.TempDir())
	domain := "app.example.com"
	h.Reconcile([]Route{{domain, "http://server", true, "", "", false}})
	h.Certificate(domain, nil, errors.New("failed"))
	h.mu.Lock()
	h.failedAt[domain] = time.Now().Add(-2 * time.Minute)
	h.mu.Unlock()
	started, finished := make(chan struct{}), make(chan struct{})
	if !h.RetryContext(context.Background(), domain, func(ctx context.Context, _ string) (*tls.Certificate, error) {
		close(started)
		<-ctx.Done()
		close(finished)
		return nil, errors.New("late failure")
	}) {
		t.Fatal("retry rejected")
	}
	<-started
	h.Reconcile(nil)
	select {
	case <-finished:
	case <-time.After(time.Second):
		t.Fatal("active callback not cancelled")
	}
	h.mu.RLock()
	defer h.mu.RUnlock()
	if _, ok := h.certificates[domain]; ok {
		t.Fatal("removed route health restored")
	}
}

func TestPublicListenersNeverServeManagement(t *testing.T) {
	g := New((&net.Dialer{}).DialContext)
	for _, host := range []string{"localhost:8080", "egress.tail.ts.net", "unknown.example.com"} {
		for _, path := range []string{"/", "/setup", "/dashboard", "/app.js", "/api/setup/session", "/api/routes", "/api/certificate/retry"} {
			r := httptest.NewRequest("GET", "http://"+host+path, nil)
			for _, handler := range []http.Handler{g, http.HandlerFunc(g.Redirect)} {
				w := httptest.NewRecorder()
				handler.ServeHTTP(w, r)
				if w.Code != 404 {
					t.Fatalf("public management path accepted %s%s: %d", host, path, w.Code)
				}
			}
		}
	}
}
