package gateway

import (
	"context"
	"crypto/tls"
	"errors"
	"io"
	"log/slog"
	"net"
	"net/http"
	"net/http/httputil"
	"net/netip"
	"net/url"
	"strings"
	"sync"
	"sync/atomic"
	"time"
)

type table struct {
	routes   []Route
	handlers map[string]http.Handler
	auth     map[string]authCredential
	state    map[string]*routeState
	revision uint64
}
type authCredential struct{ user, password string }

// maxRouteRequests stops one public service from taking every gateway-wide slot.
const maxRouteRequests = 256

// routeState survives configuration saves that leave a route's upstream and
// credentials unchanged. Any other change, or disabling or removing the route,
// cancels ctx, which also ends its established streams such as WebSockets.
type routeState struct {
	fingerprint string
	ctx         context.Context
	cancel      context.CancelFunc
	inflight    atomic.Int64
}

func routeFingerprint(r Route) string {
	return r.Upstream + "\x00" + r.BasicAuthUser + "\x00" + r.BasicAuthPassword
}

type Gateway struct {
	configMu            sync.Mutex
	live                atomic.Pointer[table]
	transport           *http.Transport
	upgradeTransport    *http.Transport
	upstreamBodyTimeout time.Duration
	buffers             pool
	Requests            atomic.Uint64
	Failures            atomic.Uint64
	Activity            Activity
	Geo                 *Geo
	Health              *Health
	RetryCertificate    func(context.Context, string) (*tls.Certificate, error)
	Connection          func(context.Context) map[string]any
	Admission           *Admission
	RetryContext        context.Context
	Blocks              *Blocks
	authSlots           chan struct{}
	auth                *authGuard
}

func (g *Gateway) EnableGeo(enabled bool) {
	if g.Geo != nil {
		g.Geo.SetEnabled(enabled)
	}
}

type Admission struct {
	slots   chan struct{}
	mu      sync.Mutex
	clients map[netip.Addr]int
}

func NewAdmission(limit int) *Admission {
	if limit < 1 {
		limit = 1
	}
	return &Admission{slots: make(chan struct{}, limit), clients: map[netip.Addr]int{}}
}

// sourceKey groups addresses one client controls together. An IPv6 subscriber
// typically holds at least a /64, so per-address accounting would not limit it.
func sourceKey(ip netip.Addr) netip.Addr {
	ip = ip.Unmap().WithZone("")
	if ip.Is6() {
		if prefix, err := ip.Prefix(64); err == nil {
			return prefix.Addr()
		}
	}
	return ip
}

func (a *Admission) Middleware(next http.Handler) http.Handler {
	if a == nil {
		return next
	}
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		ip := sourceKey(clientIP(r))
		a.mu.Lock()
		if a.clients[ip] >= 32 {
			a.mu.Unlock()
			http.Error(w, "client is at its connection limit", http.StatusTooManyRequests)
			return
		}
		a.clients[ip]++
		a.mu.Unlock()
		defer func() {
			a.mu.Lock()
			a.clients[ip]--
			if a.clients[ip] == 0 {
				delete(a.clients, ip)
			}
			a.mu.Unlock()
		}()
		select {
		case a.slots <- struct{}{}:
			defer func() { <-a.slots }()
			next.ServeHTTP(w, r)
		default:
			http.Error(w, "gateway is at its connection limit", http.StatusServiceUnavailable)
		}
	})
}

func (g *Gateway) Revision() uint64 { return g.live.Load().revision }

type pool struct{ sync.Pool }

func (p *pool) Get() []byte {
	if b := p.Pool.Get(); b != nil {
		return b.([]byte)
	}
	return make([]byte, 32*1024)
}
func (p *pool) Put(b []byte) { p.Pool.Put(b) }

func New(dial func(context.Context, string, string) (net.Conn, error)) *Gateway {
	transport := func(forceHTTP2 bool) *http.Transport {
		t := &http.Transport{
			DialContext: dial, ForceAttemptHTTP2: forceHTTP2,
			MaxIdleConns: 1024, MaxIdleConnsPerHost: 128, MaxConnsPerHost: 512,
			IdleConnTimeout: 90 * time.Second, TLSHandshakeTimeout: 10 * time.Second,
			ResponseHeaderTimeout: 30 * time.Second, ExpectContinueTimeout: time.Second,
		}
		if !forceHTTP2 {
			// httputil.ReverseProxy completes a protocol switch by hijacking
			// the connection, which HTTP/2 cannot carry. Pin upgrade requests
			// to HTTP/1.1 so WebSockets work against HTTPS upstreams that
			// would otherwise negotiate h2 over ALPN.
			t.TLSNextProto = map[string]func(string, *tls.Conn) http.RoundTripper{}
		}
		return t
	}
	g := &Gateway{transport: transport(true), upgradeTransport: transport(false), upstreamBodyTimeout: upstreamBodyIdleTimeout, authSlots: make(chan struct{}, 4), auth: newAuthGuard()}
	g.RetryContext = context.Background()
	g.Replace(nil)
	return g
}

// upstreamTransport routes protocol-upgrade requests (for example WebSockets)
// through a transport that speaks HTTP/1.1, and everything else through the
// ordinary transport.
type upstreamTransport struct{ normal, upgrade http.RoundTripper }

func (t upstreamTransport) RoundTrip(r *http.Request) (*http.Response, error) {
	if isUpgrade(r) {
		return t.upgrade.RoundTrip(r)
	}
	return t.normal.RoundTrip(r)
}

func isUpgrade(r *http.Request) bool {
	if r.Header.Get("Upgrade") == "" {
		return false
	}
	for _, v := range r.Header.Values("Connection") {
		for _, token := range strings.Split(v, ",") {
			if strings.EqualFold(strings.TrimSpace(token), "upgrade") {
				return true
			}
		}
	}
	return false
}

func (g *Gateway) Replace(routes []Route) {
	g.configMu.Lock()
	defer g.configMu.Unlock()
	g.replace(routes)
}
func (g *Gateway) replace(routes []Route) {
	if g.Health != nil {
		g.Health.Reconcile(routes)
	}
	revision := NewRevision()
	previous := g.live.Load()
	if previous != nil {
		revision = previous.revision + 1
	}
	t := &table{routes: append([]Route{}, routes...), handlers: make(map[string]http.Handler), auth: make(map[string]authCredential), state: make(map[string]*routeState), revision: revision}
	for _, r := range routes {
		if r.BasicAuthUser != "" {
			t.auth[r.Domain] = authCredential{r.BasicAuthUser, r.BasicAuthPassword}
		}
		if !r.Enabled {
			continue
		}
		target, _ := url.Parse(r.Upstream)
		domain := r.Domain
		var proxy http.Handler = &httputil.ReverseProxy{
			Transport: upstreamTimeoutTransport{base: upstreamTransport{g.transport, g.upgradeTransport}, timeout: g.upstreamBodyTimeout}, BufferPool: &g.buffers,
			Rewrite: func(p *httputil.ProxyRequest) {
				ip := clientIP(p.In)
				ctx := context.WithValue(p.Out.Context(), activityIPKey{}, ip)
				ctx = context.WithValue(ctx, downstreamContextKey{}, p.In.Context())
				p.Out = p.Out.WithContext(ctx)
				if p.Out.Body != nil && p.Out.Body != http.NoBody {
					p.Out.Body = &measuredBody{ReadCloser: p.Out.Body, count: func(n int) { g.Activity.record(domain, ip, Totals{Received: uint64(n)}) }}
				}
				p.SetURL(target)
				// Dial and verify TLS against the upstream, but let applications
				// construct redirects and cookies using the public request host.
				p.Out.Host = p.In.Host
				stripUntrustedHeaders(p.Out.Header)
				p.SetXForwarded()
			},
			ModifyResponse: func(resp *http.Response) error {
				if g.Health != nil {
					detail := ""
					if resp.StatusCode >= 500 {
						detail = resp.Status
					}
					g.Health.ResponseForUpstream(domain, target.String(), detail)
				}
				ip := clientIP(resp.Request)
				if resp.StatusCode >= 500 {
					g.Activity.record(domain, ip, Totals{Errors: 1})
				}
				body := &observedUpstreamBody{measuredBody: measuredBody{ReadCloser: resp.Body, count: func(n int) { g.Activity.record(domain, ip, Totals{Sent: uint64(n)}) }}, failed: func(err error) {
					// A downstream disconnect cancels the request context and is not
					// evidence of an upstream failure.
					downstream := resp.Request.Context().Value(downstreamContextKey{}).(context.Context)
					if downstream == nil || downstream.Err() != nil || errors.Is(err, context.Canceled) || errors.Is(err, context.DeadlineExceeded) && !isUpstreamBodyTimeout(err) {
						return
					}
					g.Failures.Add(1)
					if resp.StatusCode < 500 {
						g.Activity.record(domain, ip, Totals{Errors: 1})
					}
					if g.Health != nil {
						g.Health.ResponseForUpstream(domain, target.String(), err.Error())
					}
					slog.Warn("upstream response body failed", "domain", domain, "upstream", target.String(), "status", resp.StatusCode, "error", err)
				}}
				if rw, ok := resp.Body.(io.ReadWriteCloser); ok {
					resp.Body = &observedDuplex{measuredDuplex: measuredDuplex{measuredBody: &body.measuredBody, writer: rw, sent: func(n int) { g.Activity.record(domain, ip, Totals{Received: uint64(n)}) }}, observed: body}
				} else {
					resp.Body = body
				}
				return nil
			},
			ErrorHandler: func(w http.ResponseWriter, r *http.Request, err error) {
				g.Failures.Add(1)
				if g.Health != nil {
					g.Health.ResponseForUpstream(domain, target.String(), err.Error())
				}
				g.Activity.record(domain, clientIP(r), Totals{Errors: 1})
				slog.Warn("upstream failed", "domain", r.Host, "error", err)
				ErrorPage(w, r, proxyErrorStatus(err))
			},
		}
		t.handlers[r.Domain] = proxy
		fingerprint := routeFingerprint(r)
		if previous != nil && previous.state[r.Domain] != nil && previous.state[r.Domain].fingerprint == fingerprint {
			t.state[r.Domain] = previous.state[r.Domain]
		} else {
			ctx, cancel := context.WithCancel(context.Background())
			t.state[r.Domain] = &routeState{fingerprint: fingerprint, ctx: ctx, cancel: cancel}
		}
	}
	g.live.Store(t)
	if previous != nil {
		for domain, state := range previous.state {
			if t.state[domain] != state {
				state.cancel()
			}
		}
	}
	g.CloseIdleConnections()
}

// spoofableHeaders are client-address headers that applications commonly trust
// behind a proxy. This gateway sets only X-Forwarded-*.
var spoofableHeaders = map[string]bool{
	"forwarded":           true,
	"x-real-ip":           true,
	"x-client-ip":         true,
	"x-cluster-client-ip": true,
	"x-originating-ip":    true,
	"x-remote-ip":         true,
	"x-remote-addr":       true,
	"true-client-ip":      true,
	"cf-connecting-ip":    true,
	"fastly-client-ip":    true,
}

// stripUntrustedHeaders removes client-supplied identity and address headers.
// Names are compared with underscores as hyphens because CGI, WSGI, and PHP
// backends treat X_Forwarded_For and X-Forwarded-For as the same header.
func stripUntrustedHeaders(header http.Header) {
	for key := range header {
		name := strings.ReplaceAll(strings.ToLower(key), "_", "-")
		if strings.HasPrefix(name, "tailscale-") || strings.HasPrefix(name, "x-forwarded-") || spoofableHeaders[name] {
			delete(header, key)
		}
	}
}

// CloseIdleConnections drops pooled upstream connections, for example after a
// tailnet logout, so the next request dials a fresh peer.
func (g *Gateway) CloseIdleConnections() {
	g.transport.CloseIdleConnections()
	g.upgradeTransport.CloseIdleConnections()
}

func (g *Gateway) Routes() []Route { return append([]Route{}, g.live.Load().routes...) }
func (g *Gateway) Configuration() ([]Route, uint64) {
	t := g.live.Load()
	return append([]Route{}, t.routes...), t.revision
}
func (g *Gateway) ReplaceAtRevision(routes []Route, revision uint64) bool {
	g.configMu.Lock()
	defer g.configMu.Unlock()
	if g.Revision() != revision {
		return false
	}
	g.replace(routes)
	return true
}
func (g *Gateway) Allows(_ context.Context, host string) error {
	if _, ok := g.live.Load().handlers[strings.ToLower(host)]; !ok {
		return errors.New("domain is not enabled")
	}
	return nil
}
func hostname(host string) string {
	if h, _, err := net.SplitHostPort(host); err == nil {
		host = h
	}
	return strings.ToLower(host)
}
func (g *Gateway) ServeHTTP(w http.ResponseWriter, r *http.Request) {
	snapshot := g.live.Load()
	host := hostname(r.Host)
	if r.TLS != nil && !strings.EqualFold(r.TLS.ServerName, host) {
		ErrorPage(w, r, http.StatusMisdirectedRequest)
		return
	}
	h, ok := snapshot.handlers[host]
	if !ok {
		ErrorPage(w, r, http.StatusNotFound)
		return
	}
	if g.Blocks.Contains(clientIP(r)) {
		ErrorPage(w, r, http.StatusForbidden)
		return
	}
	if credential, required := snapshot.auth[host]; required {
		if r.TLS == nil {
			http.Error(w, "HTTPS required", http.StatusUpgradeRequired)
			return
		}
		if !g.authenticate(w, r, host, credential) {
			return
		}
		r = r.Clone(r.Context())
		r.Header.Del("Authorization")
	}
	state := snapshot.state[host]
	if state.inflight.Add(1) > maxRouteRequests {
		state.inflight.Add(-1)
		ErrorPage(w, r, http.StatusServiceUnavailable)
		return
	}
	defer state.inflight.Add(-1)
	// Ending the request context also closes an upgraded stream's upstream side.
	ctx, cancel := context.WithCancel(r.Context())
	defer cancel()
	defer context.AfterFunc(state.ctx, cancel)()
	r = r.WithContext(ctx)
	g.Requests.Add(1)
	g.Activity.record(host, clientIP(r), Totals{Requests: 1})
	h.ServeHTTP(w, r)
}

type activityIPKey struct{}
type downstreamContextKey struct{}

func clientIP(r *http.Request) netip.Addr {
	if ip, ok := r.Context().Value(activityIPKey{}).(netip.Addr); ok {
		return ip
	}
	host, _, err := net.SplitHostPort(r.RemoteAddr)
	if err != nil {
		return netip.Addr{}
	}
	ip, _ := netip.ParseAddr(host)
	return ip.Unmap()
}

// Redirect only known domains; never reflect an untrusted Host into Location.
func (g *Gateway) Redirect(w http.ResponseWriter, r *http.Request) {
	host := hostname(r.Host)
	if g.Allows(r.Context(), host) != nil {
		ErrorPage(w, r, http.StatusNotFound)
		return
	}
	http.Redirect(w, r, "https://"+host+r.URL.RequestURI(), http.StatusPermanentRedirect)
}
