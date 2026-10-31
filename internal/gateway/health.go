package gateway

import (
	"context"
	"crypto/tls"
	"crypto/x509"
	"encoding/pem"
	"net/url"
	"os"
	"path/filepath"
	"sync"
	"time"
)

type PeerHealth struct {
	State  string `json:"state"`
	Detail string `json:"detail,omitempty"`
}
type CertificateHealth struct {
	State   string    `json:"state"`
	Attempt string    `json:"attempt,omitempty"`
	Detail  string    `json:"detail,omitempty"`
	Expires time.Time `json:"expires,omitempty"`
}
type ServiceHealth struct {
	Host         PeerHealth        `json:"host"`
	Backend      BackendHealth     `json:"backend"`
	Certificate  CertificateHealth `json:"certificate"`
	LastError    string            `json:"lastError,omitempty"`
	LastResponse time.Time         `json:"lastResponse,omitempty"`
}
type Health struct {
	mu             sync.RWMutex
	peers          map[string]PeerHealth
	certificates   map[string]CertificateHealth
	services       map[string]ServiceHealth
	certDir        string
	retries        map[string]time.Time
	failedAt       map[string]time.Time
	upstreams      map[string]string
	retryJobs      map[string]*retryJob
	issuance       map[string]int
	generations    map[string]uint64
	reconciled     bool
	usableCache    map[string]time.Time
	diskCache      map[string]CertificateHealth
	cacheRefreshed map[string]time.Time
	cacheMu        sync.Mutex // Serializes disk refreshes, never held by health updates.
	nextGeneration uint64
}
type retryJob struct{ cancel context.CancelFunc }

func NewHealth(certDir string) *Health {
	return &Health{peers: map[string]PeerHealth{}, certificates: map[string]CertificateHealth{}, services: map[string]ServiceHealth{}, certDir: certDir, retries: map[string]time.Time{}, failedAt: map[string]time.Time{}, upstreams: map[string]string{}, retryJobs: map[string]*retryJob{}, issuance: map[string]int{}, generations: map[string]uint64{}, usableCache: map[string]time.Time{}, diskCache: map[string]CertificateHealth{}, cacheRefreshed: map[string]time.Time{}}
}

// Issuance ties a certificate callback to the route lifetime that started it.
// Cache-hit handshakes do not own pending issuance bookkeeping.
type Issuance struct {
	domain     string
	generation uint64
	tracked    bool
	once       sync.Once
}

func (h *Health) BeginIssuance(domain string) func() {
	j := h.StartIssuance(domain)
	return func() { j.Finish(h) }
}

// StartIssuance tracks a pending certificate request without marking cached
// certificate handshakes as new issuance attempts.
func (h *Health) StartIssuance(domain string) *Issuance {
	h.mu.Lock()
	generation := h.generations[domain]
	if _, exists := h.upstreams[domain]; h.reconciled && !exists {
		h.mu.Unlock()
		return &Issuance{domain: domain, generation: generation}
	}
	if time.Now().Before(h.usableCache[domain]) {
		h.mu.Unlock()
		return &Issuance{domain: domain, generation: generation}
	}
	h.mu.Unlock()
	cached := cachedCertificate(h.certDir, domain)
	h.mu.Lock()
	if h.generations[domain] != generation {
		h.mu.Unlock()
		return &Issuance{domain: domain, generation: generation}
	}
	if h.issuance[domain] == 0 {
		usable := cached.State == "Valid"
		if usable {
			h.usableCache[domain] = cached.Expires
		}
		if usable {
			h.mu.Unlock()
			return &Issuance{domain: domain, generation: generation}
		}
		v := h.certificates[domain]
		v.Attempt = "Issuing"
		v.Detail = ""
		h.certificates[domain] = v
	}
	h.issuance[domain]++
	j := &Issuance{domain: domain, generation: generation, tracked: true}
	h.mu.Unlock()
	return j
}
func (j *Issuance) Complete(h *Health, cert *tls.Certificate, err error) {
	if j == nil {
		return
	}
	h.mu.Lock()
	defer h.mu.Unlock()
	if h.generations[j.domain] != j.generation {
		return
	}
	if _, exists := h.upstreams[j.domain]; h.reconciled && !exists {
		return
	}
	// A cache-hit handshake is not the owner of an independent retry job.
	// Its success must not make a pending retry appear completed.
	if !j.tracked && h.retryJobs[j.domain] != nil {
		return
	}
	h.certificate(j.domain, cert, err)
}
func (j *Issuance) Finish(h *Health) {
	if j == nil {
		return
	}
	j.once.Do(func() {
		h.mu.Lock()
		defer h.mu.Unlock()
		domain := j.domain
		if h.generations[domain] != j.generation {
			return
		}
		if !j.tracked {
			return
		}
		if h.issuance[domain] > 1 {
			h.issuance[domain]--
			return
		}
		delete(h.issuance, domain)
		if _, exists := h.upstreams[domain]; h.reconciled && !exists {
			return
		}
		v := h.certificates[domain]
		if v.Attempt == "Issuing" {
			v.Attempt = ""
			h.certificates[domain] = v
		}
	})
}
func (h *Health) Reconcile(routes []Route) {
	h.mu.Lock()
	defer h.mu.Unlock()
	h.reconciled = true
	current := make(map[string]string, len(routes))
	for _, route := range routes {
		previous, exists := h.upstreams[route.Domain]
		if !exists || previous != route.Upstream {
			if job := h.retryJobs[route.Domain]; job != nil {
				job.cancel()
				delete(h.retryJobs, route.Domain)
			}
			delete(h.services, route.Domain)
			delete(h.certificates, route.Domain)
			h.nextGeneration++
			h.generations[route.Domain] = h.nextGeneration
			delete(h.issuance, route.Domain)
			delete(h.failedAt, route.Domain)
			delete(h.retries, route.Domain)
		}
		current[route.Domain] = route.Upstream
	}
	for domain := range h.services {
		if _, ok := current[domain]; !ok {
			delete(h.services, domain)
			if job := h.retryJobs[domain]; job != nil {
				job.cancel()
				delete(h.retryJobs, domain)
			}
		}
	}
	for domain, job := range h.retryJobs {
		if _, ok := current[domain]; !ok {
			job.cancel()
			delete(h.retryJobs, domain)
		}
	}
	for domain := range h.certificates {
		if _, ok := current[domain]; !ok {
			delete(h.certificates, domain)
			delete(h.issuance, domain)
			delete(h.failedAt, domain)
			delete(h.retries, domain)
		}
	}
	for domain := range h.generations {
		if _, ok := current[domain]; !ok {
			delete(h.generations, domain)
			delete(h.diskCache, domain)
			delete(h.cacheRefreshed, domain)
			delete(h.usableCache, domain)
			delete(h.issuance, domain)
		}
	}
	h.upstreams = current
}
func (h *Health) Retry(domain string, request func(context.Context, string) (*tls.Certificate, error)) bool {
	return h.RetryContext(context.Background(), domain, request)
}
func (h *Health) RetryContext(ctx context.Context, domain string, request func(context.Context, string) (*tls.Certificate, error)) bool {
	if ctx == nil {
		ctx = context.Background()
	}
	h.mu.Lock()
	if _, running := h.retryJobs[domain]; running {
		h.mu.Unlock()
		return false
	}
	v := h.certificates[domain]
	if (v.Attempt != "Failed" && v.State != "Failed" && v.State != "Error") || time.Since(h.retries[domain]) < time.Minute {
		h.mu.Unlock()
		return false
	}
	h.retries[domain] = time.Now()
	delay := time.Until(h.failedAt[domain].Add(65 * time.Second))
	v.Attempt = "Issuing"
	v.Detail = ""
	if delay > 0 {
		v.Detail = "Waiting for certificate authority retry cooldown"
	}
	h.certificates[domain] = v
	jobCtx, cancel := context.WithCancel(ctx)
	job := &retryJob{cancel: cancel}
	h.retryJobs[domain] = job
	h.mu.Unlock()
	go func() {
		defer cancel()
		defer func() {
			h.mu.Lock()
			if h.retryJobs[domain] == job {
				delete(h.retryJobs, domain)
				if jobCtx.Err() != nil {
					v := h.certificates[domain]
					if v.Attempt == "Issuing" {
						v.Attempt = ""
						v.Detail = ""
						h.certificates[domain] = v
					}
				}
			}
			h.mu.Unlock()
		}()
		if delay > 0 {
			timer := time.NewTimer(delay)
			defer timer.Stop()
			select {
			case <-timer.C:
			case <-jobCtx.Done():
				return
			}
		}
		if jobCtx.Err() != nil {
			return
		}
		cert, err := request(jobCtx, domain)
		h.mu.Lock()
		if h.retryJobs[domain] == job && jobCtx.Err() == nil {
			h.certificate(domain, cert, err)
		}
		h.mu.Unlock()
	}()
	return true
}
func (h *Health) Close() {
	h.mu.Lock()
	for domain, job := range h.retryJobs {
		job.cancel()
		v := h.certificates[domain]
		if v.Attempt == "Issuing" {
			v.Attempt = ""
			v.Detail = ""
			h.certificates[domain] = v
		}
		delete(h.retryJobs, domain)
	}
	h.mu.Unlock()
}
func (h *Health) SetPeers(peers map[string]PeerHealth) {
	h.mu.Lock()
	defer h.mu.Unlock()
	h.peers = peers
}
func (h *Health) Response(domain string, err string) {
	h.mu.Lock()
	defer h.mu.Unlock()
	v := h.services[domain]
	v.LastResponse = time.Now()
	v.LastError = err
	h.services[domain] = v
}
func (h *Health) ResponseForUpstream(domain, upstream, err string) {
	h.mu.Lock()
	defer h.mu.Unlock()
	if current, ok := h.upstreams[domain]; !ok || current != upstream {
		return
	}
	v := h.services[domain]
	v.LastResponse = time.Now()
	v.LastError = err
	h.services[domain] = v
}
func (h *Health) Issuing(domain string) {
	h.mu.Lock()
	defer h.mu.Unlock()
	v := h.certificates[domain]
	v.Attempt = "Issuing"
	h.certificates[domain] = v
}
func (h *Health) Certificate(domain string, cert *tls.Certificate, err error) {
	h.mu.Lock()
	defer h.mu.Unlock()
	if _, exists := h.upstreams[domain]; h.reconciled && !exists {
		return
	}
	h.certificate(domain, cert, err)
}
func (h *Health) certificate(domain string, cert *tls.Certificate, err error) {
	pending := h.issuance[domain] > 1
	v := CertificateHealth{}
	if err != nil {
		v.Attempt = "Failed"
		v.Detail = err.Error()
		h.failedAt[domain] = time.Now()
	} else if cert != nil && len(cert.Certificate) > 0 {
		leaf, e := x509.ParseCertificate(cert.Certificate[0])
		if e == nil {
			v = certificateState(leaf, domain)
			v.Attempt = "Succeeded"
		}
	}
	if pending && err == nil {
		v.Attempt = "Issuing"
		v.Detail = ""
	}
	h.certificates[domain] = v
	if err == nil && v.State == "Valid" {
		h.usableCache[domain] = v.Expires
		h.diskCache[domain] = v
		h.cacheRefreshed[domain] = time.Now()
	}
}
func (h *Health) ClearAttempt(domain string) {
	h.mu.Lock()
	defer h.mu.Unlock()
	v := h.certificates[domain]
	if v.Attempt == "Issuing" {
		v.Attempt = ""
		v.Detail = ""
		h.certificates[domain] = v
	}
}
func certificateState(cert *x509.Certificate, domain string) CertificateHealth {
	v := CertificateHealth{State: "Valid", Expires: cert.NotAfter}
	if err := cert.VerifyHostname(domain); err != nil {
		v.State = "Invalid"
		v.Detail = err.Error()
	} else if time.Now().Before(cert.NotBefore) {
		v.State = "Not yet valid"
	} else if !time.Now().Before(cert.NotAfter) {
		v.State = "Expired"
	}
	return v
}
func (h *Health) Snapshot(routes []Route) map[string]ServiceHealth {
	// Refresh disk metadata before taking the health lock. Cache only certificate
	// metadata, and periodically re-read so autocert renewals are observed.
	h.refreshCertificates(routes)
	h.mu.RLock()
	defer h.mu.RUnlock()
	out := map[string]ServiceHealth{}
	for _, r := range routes {
		v := h.services[r.Domain]
		if !r.Enabled {
			v.Backend = BackendHealth{State: "Disabled"}
		} else if v.Backend.Checked.IsZero() {
			v.Backend.State = "Unknown"
		} else if time.Since(v.Backend.Checked) > 90*time.Second {
			v.Backend.State = "Stale"
		}
		u, _ := url.Parse(r.Upstream)
		v.Host = PeerHealth{State: "Unknown"}
		if u != nil {
			if p, ok := h.peers[u.Hostname()]; ok {
				v.Host = p
			} else if len(h.peers) > 0 {
				v.Host = PeerHealth{State: "Not found"}
			}
		}
		v.Certificate = h.certificates[r.Domain]
		if cached := h.diskCache[r.Domain]; cached.State != "" {
			attempt, detail := v.Certificate.Attempt, v.Certificate.Detail
			v.Certificate = cached
			v.Certificate.Attempt, v.Certificate.Detail = attempt, detail
		}
		if v.Certificate.State == "" {
			v.Certificate.State = "Not issued"
			if v.Certificate.Attempt == "" && v.Certificate.Detail == "" {
				v.Certificate.Detail = "Waiting on traffic"
			}
		}
		if v.Certificate.State == "Valid" && !v.Certificate.Expires.After(time.Now()) {
			v.Certificate.State = "Expired"
		}
		out[r.Domain] = v
	}
	return out
}

func (h *Health) refreshCertificates(routes []Route) {
	h.cacheMu.Lock()
	defer h.cacheMu.Unlock()
	now := time.Now()
	h.mu.RLock()
	refresh := make(map[string]uint64)
	for _, r := range routes {
		if _, exists := h.upstreams[r.Domain]; h.reconciled && !exists {
			continue
		}
		if _, ok := h.diskCache[r.Domain]; !ok || now.Sub(h.cacheRefreshed[r.Domain]) >= 5*time.Second {
			refresh[r.Domain] = h.generations[r.Domain]
		}
	}
	h.mu.RUnlock()
	refreshed := make(map[string]CertificateHealth, len(refresh))
	for domain := range refresh {
		refreshed[domain] = cachedCertificate(h.certDir, domain)
	}
	// Publish refreshed disk results under exclusive lock after disk IO completes.
	if len(refreshed) > 0 {
		h.mu.Lock()
		for domain, cert := range refreshed {
			if h.generations[domain] != refresh[domain] {
				continue
			}
			if _, exists := h.upstreams[domain]; h.reconciled && !exists {
				continue
			}
			if h.cacheRefreshed[domain].After(now) {
				continue
			}
			h.diskCache[domain] = cert
			h.cacheRefreshed[domain] = now
		}
		h.mu.Unlock()
	}
}

func cachedCertificate(dir, domain string) CertificateHealth {
	for _, name := range []string{domain, domain + "+rsa"} {
		b, err := os.ReadFile(filepath.Join(dir, name))
		if err != nil {
			continue
		}
		for len(b) > 0 {
			block, rest := pem.Decode(b)
			if block == nil {
				break
			}
			b = rest
			if block.Type == "CERTIFICATE" {
				if cert, err := x509.ParseCertificate(block.Bytes); err == nil {
					return certificateState(cert, domain)
				}
				break
			}
		}
	}
	return CertificateHealth{}
}
