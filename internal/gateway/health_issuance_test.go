package gateway

import (
	"crypto/ecdsa"
	"crypto/elliptic"
	"crypto/rand"
	"crypto/x509"
	"encoding/pem"
	"os"
	"path/filepath"
	"testing"
	"time"
)

func TestBeginIssuanceTracksConcurrentFirstRequests(t *testing.T) {
	h := NewHealth(t.TempDir())
	finish1 := h.BeginIssuance("example.test")
	finish2 := h.BeginIssuance("example.test")
	if got := h.Snapshot([]Route{{Domain: "example.test"}})["example.test"].Certificate.Attempt; got != "Issuing" {
		t.Fatalf("Attempt = %q", got)
	}
	finish1()
	if got := h.Snapshot([]Route{{Domain: "example.test"}})["example.test"].Certificate.Attempt; got != "Issuing" {
		t.Fatalf("Attempt after one completion = %q", got)
	}
	finish2()
	if got := h.Snapshot([]Route{{Domain: "example.test"}})["example.test"].Certificate.Attempt; got != "" {
		t.Fatalf("Attempt after completion = %q", got)
	}
}

func writeHealthCert(t *testing.T, dir, domain string, expiry time.Time) {
	t.Helper()
	cert := &x509.Certificate{DNSNames: []string{domain}, NotBefore: time.Now().Add(-time.Hour), NotAfter: expiry}
	key, err := ecdsa.GenerateKey(elliptic.P256(), rand.Reader)
	if err != nil {
		t.Fatal(err)
	}
	der, err := x509.CreateCertificate(rand.Reader, cert, cert, key.Public(), key)
	if err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(dir, domain), pem.EncodeToMemory(&pem.Block{Type: "CERTIFICATE", Bytes: der}), 0600); err != nil {
		t.Fatal(err)
	}
}

func TestBeginIssuanceOnlySkipsUsableCachedCertificate(t *testing.T) {
	domain := "cache.example"
	dir := t.TempDir()
	h := NewHealth(dir)
	writeHealthCert(t, dir, domain, time.Now().Add(time.Hour))
	finish := h.BeginIssuance(domain)
	finish()
	if got := h.Snapshot([]Route{{Domain: domain}})[domain].Certificate.Attempt; got != "" {
		t.Fatalf("valid cache attempt = %q", got)
	}
	if err := os.Remove(filepath.Join(dir, domain)); err != nil {
		t.Fatal(err)
	}
	h.mu.Lock()
	delete(h.usableCache, domain)
	h.mu.Unlock()
	writeHealthCert(t, dir, domain, time.Now().Add(-time.Hour))
	finish = h.BeginIssuance(domain)
	if got := h.Snapshot([]Route{{Domain: domain}})[domain].Certificate.Attempt; got != "Issuing" {
		t.Fatalf("expired cache attempt = %q", got)
	}
	finish()
}

func TestConcurrentIssuancePreservesFinalFailure(t *testing.T) {
	h := NewHealth(t.TempDir())
	finish1 := h.BeginIssuance("failed.example")
	finish2 := h.BeginIssuance("failed.example")
	h.Certificate("failed.example", nil, os.ErrNotExist)
	finish1()
	finish2()
	got := h.Snapshot([]Route{{Domain: "failed.example"}})["failed.example"].Certificate
	if got.Attempt != "Failed" || got.Detail == "" {
		t.Fatalf("failure overwritten: %+v", got)
	}
}

func TestIssuanceCompletionDoesNotRestoreRemovedDomain(t *testing.T) {
	h := NewHealth(t.TempDir())
	h.Reconcile([]Route{{Domain: "removed.example"}})
	finish := h.BeginIssuance("removed.example")
	h.Reconcile(nil)
	h.Certificate("removed.example", nil, os.ErrNotExist)
	finish()
	if _, exists := h.certificates["removed.example"]; exists {
		t.Fatal("completion restored removed certificate health")
	}
}

func TestIssuanceMemoryCacheExpires(t *testing.T) {
	h := NewHealth(t.TempDir())
	h.usableCache["expired.example"] = time.Now().Add(-time.Second)
	finish := h.BeginIssuance("expired.example")
	defer finish()
	if h.certificates["expired.example"].Attempt != "Issuing" {
		t.Fatal("expired memory cache suppressed issuance")
	}
}
