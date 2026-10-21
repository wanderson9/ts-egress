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
	revision uint64
}
type Gateway struct {
	configMu         sync.Mutex
	live             atomic.Pointer[table]
	transport        *http.Transport
	upgradeTransport *http.Transport
	buffers          pool
	Requests         atomic.Uint64
	Failures         atomic.Uint64
	Activity         Activity
	Geo              *Geo
	Health           *Health
	RetryCertificate func(context.Context, string) (*tls.Certificate, error)
	Connection       func(context.Context) map[string]any
	Admission        *Admission
	RetryContext     context.Context
	Blocks           *Blocks
}

func (g *Gateway) EnableGeo(enabled bool) {
	if g.Geo != nil {
		g.Geo.SetEnabled(enabled)
	}
}

type Admission struct{ slots chan struct{} }

func NewAdmission(limit int) *Admission {
	if limit < 1 {
		limit = 1
	}
	return &Admission{slots: make(chan struct{}, limit)}
}
func (a *Admission) Middleware(next http.Handler) http.Handler {
	if a == nil {
		return next
	}
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
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
	g := &Gateway{transport: transport(true), upgradeTransport: transport(false)}
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
	var revision uint64 = 1
	if previous := g.live.Load(); previous != nil {
		revision = previous.revision + 1
	}
	t := &table{routes: append([]Route{}, routes...), handlers: make(map[string]http.Handler), revision: revision}
	for _, r := range routes {
		if !r.Enabled {
			continue
		}
		target, _ := url.Parse(r.Upstream)
		domain := r.Domain
		t.handlers[r.Domain] = &httputil.ReverseProxy{
			Transport: upstreamTransport{g.transport, g.upgradeTransport}, BufferPool: &g.buffers,
			Rewrite: func(p *httputil.ProxyRequest) {
				ip := clientIP(p.In)
				p.Out = p.Out.WithContext(context.WithValue(p.Out.Context(), activityIPKey{}, ip))
				if p.Out.Body != nil && p.Out.Body != http.NoBody {
					p.Out.Body = &measuredBody{ReadCloser: p.Out.Body, count: func(n int) { g.Activity.record(domain, ip, Totals{Received: uint64(n)}) }}
				}
				p.SetURL(target)
				// Never trust client-supplied forwarding or Tailscale identity headers.
				for k := range p.Out.Header {
					if strings.HasPrefix(strings.ToLower(k), "tailscale-") || strings.HasPrefix(strings.ToLower(k), "x-forwarded-") {
						p.Out.Header.Del(k)
					}
				}
				p.Out.Header.Del("Forwarded")
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
				body := &measuredBody{ReadCloser: resp.Body, count: func(n int) { g.Activity.record(domain, ip, Totals{Sent: uint64(n)}) }}
				if rw, ok := resp.Body.(io.ReadWriteCloser); ok {
					resp.Body = &measuredDuplex{measuredBody: body, writer: rw, sent: func(n int) { g.Activity.record(domain, ip, Totals{Received: uint64(n)}) }}
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
	}
	g.live.Store(t)
	g.CloseIdleConnections()
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
	host := hostname(r.Host)
	if r.TLS != nil && !strings.EqualFold(r.TLS.ServerName, host) {
		ErrorPage(w, r, http.StatusMisdirectedRequest)
		return
	}
	h, ok := g.live.Load().handlers[host]
	if !ok {
		ErrorPage(w, r, http.StatusNotFound)
		return
	}
	if g.Blocks.Contains(clientIP(r)) {
		ErrorPage(w, r, http.StatusForbidden)
		return
	}
	g.Requests.Add(1)
	g.Activity.record(host, clientIP(r), Totals{Requests: 1})
	h.ServeHTTP(w, r)
}

type activityIPKey struct{}

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
