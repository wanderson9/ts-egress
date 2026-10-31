package gateway

import (
	"context"
	"crypto/rand"
	"crypto/rsa"
	"crypto/tls"
	"crypto/x509"
	"encoding/pem"
	"fmt"
	"math/big"
	"os"
	"path/filepath"
	"testing"
	"time"
)

func writeCachedHealthCert(t *testing.T, dir, domain string, serial int64, expiry time.Time) []byte {
	t.Helper()
	key, err := rsa.GenerateKey(rand.Reader, 1024)
	if err != nil {
		t.Fatal(err)
	}
	template := &x509.Certificate{SerialNumber: big.NewInt(serial), DNSNames: []string{domain}, NotBefore: time.Now().Add(-time.Hour), NotAfter: expiry}
	der, err := x509.CreateCertificate(rand.Reader, template, template, &key.PublicKey, key)
	if err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(dir, domain), pem.EncodeToMemory(&pem.Block{Type: "CERTIFICATE", Bytes: der}), 0600); err != nil {
		t.Fatal(err)
	}
	return der
}

func TestCertificateRefreshDoesNotBlockHealthUpdates(t *testing.T) {
	h := NewHealth(t.TempDir())
	route := Route{"app.example.test", "http://backend", true, "", "", false}
	h.Reconcile([]Route{route})
	// A queued or slow certificate refresh must not own the shared health lock.
	h.cacheMu.Lock()
	done := make(chan struct{})
	go func() { h.Snapshot([]Route{route}); close(done) }()
	updated := make(chan struct{})
	go func() { h.ResponseForUpstream(route.Domain, route.Upstream, "failure"); close(updated) }()
	select {
	case <-updated:
	case <-time.After(time.Second):
		h.cacheMu.Unlock()
		t.Fatal("certificate refresh blocked public health update")
	}
	h.cacheMu.Unlock()
	<-done
}

func TestReconcilePrunesCertificateMetadata(t *testing.T) {
	h := NewHealth(t.TempDir())
	for i := 0; i < 20; i++ {
		route := Route{fmt.Sprintf("app%d.example.test", i), "http://backend", true, "", "", false}
		h.Reconcile([]Route{route})
		job := h.StartIssuance(route.Domain)
		h.Snapshot([]Route{route})
		job.Finish(h)
	}
	h.Reconcile(nil)
	if len(h.generations)+len(h.diskCache)+len(h.cacheRefreshed)+len(h.usableCache)+len(h.issuance) != 0 {
		t.Fatal("removed certificate metadata retained")
	}
}

func TestCachedHandshakeRemovalStillInvalidatesItsCallback(t *testing.T) {
	dir := t.TempDir()
	h := NewHealth(dir)
	route := Route{"cached-remove.example.test", "http://backend", true, "", "", false}
	h.Reconcile([]Route{route})
	writeCachedHealthCert(t, dir, route.Domain, 1, time.Now().Add(time.Hour))
	old := h.StartIssuance(route.Domain)
	if old.tracked {
		t.Fatal("cached handshake unexpectedly tracked as issuance")
	}
	h.Reconcile(nil)
	h.Reconcile([]Route{route})
	old.Complete(h, nil, os.ErrPermission)
	old.Finish(h)
	if got := h.Snapshot([]Route{route})[route.Domain].Certificate; got.Attempt != "" {
		t.Fatalf("cached old callback changed replacement state: %+v", got)
	}
}

func TestOldIssuanceCompletionIgnoredAfterRemoveAndReadd(t *testing.T) {
	h := NewHealth(t.TempDir())
	domain := "old.example.test"
	route := Route{domain, "http://backend", true, "", "", false}
	h.Reconcile([]Route{route})
	old := h.StartIssuance(domain)
	h.Reconcile(nil)
	h.Reconcile([]Route{route})
	old.Complete(h, nil, nil)
	old.Finish(h)
	if got := h.Snapshot([]Route{route})[domain].Certificate; got.Attempt != "" || got.State != "Not issued" {
		t.Fatalf("stale issuance affected replacement route: %+v", got)
	}
}

func TestOldIssuanceCannotFinishReplacementJob(t *testing.T) {
	h := NewHealth(t.TempDir())
	route := Route{"replace.example.test", "http://backend", true, "", "", false}
	h.Reconcile([]Route{route})
	old := h.StartIssuance(route.Domain)
	h.Reconcile(nil)
	h.Reconcile([]Route{route})
	current := h.StartIssuance(route.Domain)
	old.Complete(h, nil, os.ErrPermission)
	old.Finish(h)
	if h.certificates[route.Domain].Attempt != "Issuing" || h.issuance[route.Domain] != 1 {
		t.Fatal("old callback modified replacement job")
	}
	current.Complete(h, nil, os.ErrNotExist)
	current.Finish(h)
	if h.certificates[route.Domain].Attempt != "Failed" {
		t.Fatal("replacement result lost")
	}
}

func TestCachedIssuanceFinishDoesNotClearOtherIssuingAttempt(t *testing.T) {
	dir := t.TempDir()
	domain := "cached.example.test"
	h := NewHealth(dir)
	der := writeCachedHealthCert(t, dir, domain, 1, time.Now().Add(time.Hour))
	h.mu.Lock()
	h.cacheRefreshed[domain] = time.Time{}
	h.mu.Unlock()
	if got := h.Snapshot([]Route{{domain, "http://backend", true, "", "", false}})[domain].Certificate.State; got != "Valid" {
		t.Fatalf("cache not loaded: %s", got)
	}
	h.Issuing(domain) // e.g. an independent retry job owns this attempt state.
	_, cancel := context.WithCancel(context.Background())
	defer cancel()
	h.retryJobs[domain] = &retryJob{cancel: cancel}
	cached := h.StartIssuance(domain)
	cached.Complete(h, &tls.Certificate{Certificate: [][]byte{der}}, nil)
	cached.Finish(h)
	if got := h.Snapshot([]Route{{domain, "http://backend", true, "", "", false}})[domain].Certificate.Attempt; got != "Issuing" {
		t.Fatalf("cached request cleared other attempt: %q", got)
	}
}

func TestCertificateDiskCacheRefreshesRenewalAndExpiry(t *testing.T) {
	dir := t.TempDir()
	domain := "renew.example.test"
	routes := []Route{{domain, "http://backend", true, "", "", false}}
	h := NewHealth(dir)
	writeCachedHealthCert(t, dir, domain, 1, time.Now().Add(24*time.Hour))
	if got := h.Snapshot(routes)[domain].Certificate; got.State != "Valid" {
		t.Fatalf("initial cache: %+v", got)
	}
	writeCachedHealthCert(t, dir, domain, 2, time.Now().Add(48*time.Hour))
	if got := h.Snapshot(routes)[domain].Certificate; got.Expires.After(time.Now().Add(25 * time.Hour)) {
		t.Fatalf("disk cache was reread before refresh interval: %+v", got)
	}
	h.mu.Lock()
	h.cacheRefreshed[domain] = time.Now().Add(-6 * time.Second)
	h.mu.Unlock()
	if got := h.Snapshot(routes)[domain].Certificate; got.Expires.Before(time.Now().Add(47 * time.Hour)) {
		t.Fatalf("renewal not refreshed: %+v", got)
	}
	writeCachedHealthCert(t, dir, domain, 3, time.Now().Add(-time.Second))
	h.mu.Lock()
	h.cacheRefreshed[domain] = time.Now().Add(-6 * time.Second)
	h.mu.Unlock()
	if got := h.Snapshot(routes)[domain].Certificate; got.State != "Expired" {
		t.Fatalf("expired cache not reflected: %+v", got)
	}
}
