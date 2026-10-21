package main

import (
	"bufio"
	"context"
	"crypto/tls"
	"errors"
	"flag"
	"fmt"
	"log/slog"
	"net"
	"net/http"
	"os"
	"os/signal"
	"path/filepath"
	"strings"
	"sync"
	"syscall"
	"time"

	"golang.org/x/crypto/acme/autocert"
	"tailscale.com/client/local"
	"tailscale.com/ipn/ipnstate"
	"tailscale.com/tsnet"
	"ts-egress/internal/gateway"
	"ts-egress/internal/setup"
)

func main() {
	if err := run(); err != nil {
		slog.Error("ts-egress stopped", "error", err)
		os.Exit(1)
	}
}

type runtime struct {
	ctx           context.Context
	cancelAttempt context.CancelFunc
	store         *setup.Store
	stateDir      string
	gateway       *gateway.Gateway
	mu            sync.RWMutex
	authMu        sync.Mutex
	ts            tailnetNode
	status        setup.Status
	starting      bool
	admin         *http.Server
	done          chan struct{}
	setup         *setup.Server
	newNode       func() tailnetNode
	attempt       uint64
	geo           *gateway.Geo
}

type tailnetNode interface {
	Start() error
	Close() error
	LocalClient() (*local.Client, error)
	Up(context.Context) (*ipnstate.Status, error)
	ListenTLS(string, string) (net.Listener, error)
	Dial(context.Context, string, string) (net.Conn, error)
}

func (rt *runtime) Connect() {
	rt.mu.Lock()
	if rt.starting {
		rt.mu.Unlock()
		return
	}
	rt.starting = true
	attemptCtx, cancel := context.WithCancel(rt.ctx)
	rt.cancelAttempt = cancel
	rt.attempt++
	attempt := rt.attempt
	rt.done = make(chan struct{})
	rt.status.State = "connecting"
	rt.status.Error = ""
	rt.status.AuthURL = ""
	rt.status.AdminURL = ""
	rt.mu.Unlock()
	go rt.connect(attemptCtx, rt.done, attempt)
}
func (rt *runtime) connect(attemptCtx context.Context, done chan struct{}, attempt uint64) {
	defer func() {
		rt.mu.Lock()
		if rt.attempt == attempt {
			rt.starting = false
			rt.ts = nil
			if rt.cancelAttempt != nil {
				rt.cancelAttempt()
				rt.cancelAttempt = nil
			}
			if rt.status.State != "error" && rt.status.State != "needs-login" {
				rt.status.State = "not-connected"
			}
			if rt.ctx.Err() != nil && rt.status.State != "error" {
				rt.status.State = "stopped"
			}
			rt.admin = nil
		}
		rt.mu.Unlock()
		close(done)
	}()
	defer func() {
		if v := recover(); v != nil {
			rt.mu.Lock()
			if rt.attempt == attempt {
				rt.status.State = "error"
				rt.status.Error = fmt.Sprint(v)
			}
			rt.mu.Unlock()
			slog.Error("tailnet lifecycle panic", "panic", v)
		}
	}()
	newTSNode := rt.newNode
	if newTSNode == nil {
		newTSNode = func() tailnetNode {
			return &tsnet.Server{
				Dir:      filepath.Join(rt.stateDir, "tailscale"),
				Hostname: rt.store.Get().Hostname,
				UserLogf: func(format string, args ...any) {
					msg := fmt.Sprintf(format, args...)
					slog.Info("tailscale", "message", msg)
				},
			}
		}
	}
	ts := newTSNode()
	rt.mu.Lock()
	rt.ts = ts
	rt.mu.Unlock()
	fail := func(err error) {
		rt.mu.Lock()
		if rt.attempt == attempt {
			rt.status.State = "error"
			rt.status.Error = err.Error()
		}
		rt.mu.Unlock()
		slog.Error("tailnet startup", "error", err)
	}
	if err := ts.Start(); err != nil {
		_ = ts.Close()
		fail(err)
		return
	}
	defer func() { _ = ts.Close() }()
	lc, err := ts.LocalClient()
	if err != nil {
		fail(err)
		return
	}
	// Poll status while Up waits so the browser can show the official login URL.
	pollCtx, cancel := context.WithCancel(attemptCtx)
	defer cancel()
	go func() {
		tick := time.NewTicker(time.Second)
		defer tick.Stop()
		for {
			select {
			case <-pollCtx.Done():
				return
			case <-tick.C:
				st, err := lc.Status(pollCtx)
				if err == nil {
					rt.mu.Lock()
					if rt.attempt == attempt {
						rt.status.AuthURL = st.AuthURL
					}
					if rt.attempt == attempt && st.AuthURL != "" && rt.status.State != "error" {
						rt.status.State = "needs-login"
					}
					rt.mu.Unlock()
				}
			}
		}
	}()
	st, err := ts.Up(attemptCtx)
	if err != nil {
		fail(err)
		return
	}
	cancel()
	rt.mu.Lock()
	if rt.attempt == attempt {
		rt.status.AuthURL = ""
		rt.status.State = "connected"
	}
	rt.mu.Unlock()
	// HTTPS can be enabled in the Tailscale console after enrollment. Keep
	// checking instead of forcing a restart when those prerequisites are absent.
	for {
		st, err = lc.Status(attemptCtx)
		if err == nil && len(st.CertDomains) > 0 && st.CurrentTailnet != nil && st.CurrentTailnet.MagicDNSEnabled {
			break
		}
		select {
		case <-attemptCtx.Done():
			return
		case <-time.After(3 * time.Second):
		}
	}
	domain := st.CertDomains[0]
	ln, err := ts.ListenTLS("tcp", ":443")
	if err != nil {
		fail(err)
		return
	}
	identify := func(ctx context.Context, addr string) (string, error) {
		ctx, cancel := context.WithTimeout(ctx, 5*time.Second)
		defer cancel()
		who, err := lc.WhoIs(ctx, addr)
		if err != nil {
			return "", err
		}
		if who.UserProfile == nil || who.Node == nil || len(who.Node.Tags) != 0 {
			return "", errors.New("user identity required")
		}
		login := who.UserProfile.LoginName
		admins := rt.store.Get().Admins
		// An empty allowlist admits any untagged user on the tailnet. The tsnet
		// listener is already reachable only by this tailnet, so the allowlist
		// only narrows access further when an operator chooses to set it.
		if len(admins) == 0 {
			return login, nil
		}
		for _, a := range admins {
			if a == login {
				return a, nil
			}
		}
		return "", errors.New("identity not in admin allowlist")
	}
	admin := server(rt.gateway.AdminWithSetup(filepath.Join(rt.stateDir, "routes.json"), domain, "https://"+domain, identify, rt.setup.Tailnet()))
	admin.ReadTimeout = 15 * time.Second
	admin.WriteTimeout = 30 * time.Second
	rt.mu.Lock()
	rt.admin = admin
	rt.status.AdminURL = "https://" + domain
	rt.mu.Unlock()
	if err := rt.store.Seal(); err != nil {
		ln.Close()
		fail(fmt.Errorf("lock local management: %w", err))
		return
	}
	slog.Info("tailnet dashboard ready", "url", "https://"+domain)
	served := make(chan error, 1)
	go func() { served <- admin.Serve(ln) }()
	select {
	case err := <-served:
		if !errors.Is(err, http.ErrServerClosed) {
			fail(err)
		}
		_ = ln.Close()
		if errors.Is(err, http.ErrServerClosed) {
			fail(errors.New("tailnet dashboard stopped unexpectedly"))
		}
	case <-attemptCtx.Done():
		shutdown, cancel := context.WithTimeout(context.Background(), 15*time.Second)
		defer cancel()
		if err := admin.Shutdown(shutdown); err != nil {
			admin.Close()
		}
		<-served
		_ = ln.Close()
	}
}
func (rt *runtime) Reauthenticate(ctx context.Context) error {
	rt.authMu.Lock()
	defer rt.authMu.Unlock()
	rt.mu.RLock()
	ts := rt.ts
	rt.mu.RUnlock()
	if ts == nil {
		rt.Connect()
		return nil
	}
	lc, err := ts.LocalClient()
	if err != nil {
		return err
	}
	st, err := lc.Status(ctx)
	if err != nil {
		return err
	}
	if st.BackendState != "NeedsLogin" && st.AuthURL == "" {
		return errors.New("Tailscale does not require authentication")
	}
	return lc.StartLoginInteractive(ctx)
}
func (rt *runtime) Disconnect() {
	rt.mu.Lock()
	if rt.cancelAttempt != nil {
		rt.cancelAttempt()
	}
	rt.mu.Unlock()
}
func (rt *runtime) Logout(ctx context.Context) (logoutErr error) {
	rt.authMu.Lock()
	defer rt.authMu.Unlock()
	defer func() {
		if logoutErr != nil {
			rt.mu.Lock()
			rt.status.Error = "Tailscale logout failed: " + logoutErr.Error()
			rt.mu.Unlock()
		}
		if rt.gateway != nil {
			rt.gateway.CloseIdleConnections()
		}
	}()
	rt.mu.RLock()
	ts := rt.ts
	rt.mu.RUnlock()
	if ts == nil {
		return errors.New("tailnet has not started")
	}
	lc, err := ts.LocalClient()
	if err != nil {
		return err
	}
	if err := lc.Logout(ctx); err != nil {
		return err
	}
	rt.mu.Lock()
	rt.status.State = "needs-login"
	rt.status.AuthURL = ""
	rt.status.Error = ""
	rt.mu.Unlock()
	// Request a fresh authorization URL now so the local console can offer a
	// single-click sign-in instead of requiring a second request.
	if err := lc.StartLoginInteractive(ctx); err != nil {
		slog.Warn("request Tailscale login URL", "error", err)
	}
	return nil
}
func (rt *runtime) Status(ctx context.Context) setup.Status {
	rt.mu.RLock()
	v := rt.status
	ts := rt.ts
	rt.mu.RUnlock()
	v.Peers = []setup.Peer{}
	if ts == nil {
		return v
	}
	ctx, cancel := context.WithTimeout(ctx, 3*time.Second)
	defer cancel()
	lc, err := ts.LocalClient()
	if err != nil {
		v.State = "unknown"
		v.Error = "Tailscale status unavailable: " + err.Error()
		return v
	}
	st, err := lc.Status(ctx)
	if err != nil {
		v.State = "unknown"
		v.Error = "Tailscale status unavailable: " + err.Error()
		return v
	}
	v.AuthURL = st.AuthURL
	// Derive backend connectivity live, while preserving dashboard startup errors.
	switch st.BackendState {
	case "NeedsLogin":
		v.State = "needs-login"
	case "Running":
		if v.State != "error" {
			v.State = "connected"
		}
	default:
		v.State = "connecting"
	}
	v.MagicDNS = st.CurrentTailnet != nil && st.CurrentTailnet.MagicDNSEnabled
	v.HTTPS = len(st.CertDomains) > 0
	for _, p := range st.Peer {
		peer := setup.Peer{Name: strings.TrimSuffix(p.DNSName, "."), Online: p.Online, IPs: []string{}}
		for _, ip := range p.TailscaleIPs {
			peer.IPs = append(peer.IPs, ip.String())
		}
		v.Peers = append(v.Peers, peer)
	}
	return v
}
func (rt *runtime) Dial(ctx context.Context, network, address string) (net.Conn, error) {
	ctx, cancel := context.WithTimeout(ctx, 10*time.Second)
	defer cancel()
	rt.mu.RLock()
	ts := rt.ts
	rt.mu.RUnlock()
	if ts == nil {
		return nil, errors.New("tailnet not connected")
	}
	host, port, err := net.SplitHostPort(address)
	if err != nil {
		return nil, err
	}
	lc, err := ts.LocalClient()
	if err != nil {
		return nil, err
	}
	st, err := lc.Status(ctx)
	if err != nil {
		return nil, err
	}
	for _, peer := range st.Peer {
		full := strings.TrimSuffix(peer.DNSName, ".")
		match := strings.EqualFold(host, full) || strings.EqualFold(host, strings.Split(full, ".")[0])
		for _, ip := range peer.TailscaleIPs {
			if host == ip.String() {
				match = true
			}
		}
		if match && len(peer.TailscaleIPs) > 0 {
			return ts.Dial(ctx, network, net.JoinHostPort(peer.TailscaleIPs[0].String(), port))
		}
	}
	return nil, fmt.Errorf("upstream %q is not a known tailnet peer", host)
}
func server(h http.Handler) *http.Server {
	return &http.Server{Handler: h, ReadHeaderTimeout: 10 * time.Second, IdleTimeout: 90 * time.Second, MaxHeaderBytes: 64 * 1024}
}
func run() error {
	recoverMode := flag.Bool("recover-admin", false, "reset the local administrator password and optionally set the Tailscale admin allowlist, then exit")
	recoveryAdmins := flag.String("recover-admins", "", "comma-separated replacement Tailscale administrator logins for -recover-admin; omit to preserve the current allowlist")
	state := flag.String("state-dir", "./data", "persistent state directory (Docker defaults to /data)")
	setupAddr := flag.String("setup", "127.0.0.1:8080", "local console address; publish on host loopback only")
	httpAddr := flag.String("http", ":80", "public HTTP/ACME address")
	httpsAddr := flag.String("https", ":443", "public HTTPS address")
	flag.Parse()
	store, err := setup.Open(*state)
	if err != nil {
		return err
	}
	if *recoverMode {
		var admins []string
		if flag.Lookup("recover-admins").Value.String() != "" {
			admins = strings.Split(*recoveryAdmins, ",")
		}
		return recoverAdmin(store, admins)
	}

	geo := gateway.NewGeo(*state)
	routesPath := filepath.Join(*state, "routes.json")
	routes, err := gateway.Load(routesPath)
	if err != nil {
		return err
	}
	ctx, stop := signal.NotifyContext(context.Background(), os.Interrupt, syscall.SIGTERM)
	defer stop()
	rt := &runtime{ctx: ctx, store: store, stateDir: *state, status: setup.Status{State: "not-connected"}, done: make(chan struct{})}
	rt.geo = geo
	g := gateway.New(rt.Dial)
	g.RetryContext = ctx
	g.Admission = gateway.NewAdmission(512)
	g.Geo = geo
	g.Blocks, err = gateway.OpenBlocks(filepath.Join(*state, "blocked-ips.json"))
	if err != nil {
		return err
	}
	g.Health = gateway.NewHealth(filepath.Join(*state, "certs"))
	g.Replace(routes)
	rt.gateway = g
	go g.MonitorBackends(ctx)
	g.Connection = func(ctx context.Context) map[string]any {
		st := rt.Status(ctx)
		return map[string]any{"configured": store.Get().Connect, "needsLogin": st.AuthURL != "" || st.State == "needs-login", "state": st.State}
	}
	go func() {
		tick := time.NewTicker(10 * time.Second)
		defer tick.Stop()
		for {
			select {
			case <-ctx.Done():
				return
			case <-tick.C:
				st := rt.Status(ctx)
				peers := map[string]gateway.PeerHealth{}
				for _, p := range st.Peers {
					state := "Offline"
					if p.Online {
						state = "Online"
					}
					v := gateway.PeerHealth{State: state, Detail: "Tailscale host status; service availability is not probed"}
					peers[p.Name] = v
					peers[strings.Split(p.Name, ".")[0]] = v
					for _, ip := range p.IPs {
						peers[ip] = v
					}
				}
				g.Health.SetPeers(peers)
			}
		}
	}()
	rt.setup = setup.New(store, rt, g, routesPath)
	local := server(rt.setup)
	local.ReadTimeout = 15 * time.Second
	local.WriteTimeout = 30 * time.Second
	manager := &autocert.Manager{Prompt: func(tos string) bool { return store.Get().AcceptTerms }, Cache: autocert.DirCache(filepath.Join(*state, "certs")), HostPolicy: func(ctx context.Context, host string) error {
		if !store.Get().AcceptTerms {
			return errors.New("accept certificate terms in setup first")
		}
		return g.Allows(ctx, host)
	}}
	// Read the contact from persistent settings for each new ACME account. The
	// manager is created after first-run settings exist, never with a stale email.
	var certMu sync.Mutex
	var active *autocert.Manager
	certificate := func(hello *tls.ClientHelloInfo) (*tls.Certificate, error) {
		if err := g.Allows(ctx, hello.ServerName); err != nil {
			return nil, err
		}
		v := store.Get()
		if !v.AcceptTerms {
			return nil, errors.New("certificate terms not accepted")
		}
		certMu.Lock()
		if active == nil {
			manager.Email = v.Email
			active = manager
		}
		m := active
		certMu.Unlock()
		// ACME validation handshakes must not overwrite issuance state.
		challenge := len(hello.SupportedProtos) == 1 && hello.SupportedProtos[0] == "acme-tls/1"
		cert, err := m.GetCertificate(hello)
		if !challenge {
			if err != nil || cert == nil || len(cert.Certificate) == 0 {
				g.Health.Issuing(hello.ServerName)
			}
			g.Health.Certificate(hello.ServerName, cert, err)
		}
		return cert, err
	}
	g.RetryCertificate = func(jobCtx context.Context, domain string) (*tls.Certificate, error) {
		if err := g.Allows(jobCtx, domain); err != nil {
			return nil, err
		}
		v := store.Get()
		if !v.AcceptTerms {
			return nil, errors.New("certificate terms not accepted")
		}
		certMu.Lock()
		if active == nil {
			manager.Email = v.Email
			active = manager
		}
		m := active
		certMu.Unlock()
		type result struct {
			cert *tls.Certificate
			err  error
		}
		done := make(chan result, 1)
		// autocert uses its own five-minute context, not ClientHelloInfo.Context.
		// Detach our waiter on cancellation; it must never publish stale health.
		go func() {
			cert, err := m.GetCertificate(&tls.ClientHelloInfo{ServerName: domain, SupportedVersions: []uint16{tls.VersionTLS13, tls.VersionTLS12}, CipherSuites: []uint16{tls.TLS_ECDHE_ECDSA_WITH_AES_128_GCM_SHA256}, SupportedCurves: []tls.CurveID{tls.CurveP256}, SignatureSchemes: []tls.SignatureScheme{tls.ECDSAWithP256AndSHA256}})
			done <- result{cert, err}
		}()
		select {
		case <-jobCtx.Done():
			return nil, jobCtx.Err()
		case r := <-done:
			return r.cert, r.err
		}
	}
	tlsConfig := manager.TLSConfig()
	tlsConfig.MinVersion = tls.VersionTLS12
	tlsConfig.GetCertificate = certificate
	plain := server(manager.HTTPHandler(http.HandlerFunc(g.Redirect)))
	secure := server(g.Admission.Middleware(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if !store.Get().AcceptTerms {
			gateway.ErrorPage(w, r, http.StatusServiceUnavailable)
			return
		}
		g.ServeHTTP(w, r)
	})))
	secure.TLSConfig = tlsConfig
	listeners := []net.Listener{}
	defer func() {
		for _, ln := range listeners {
			ln.Close()
		}
		g.CloseIdleConnections()
		g.Health.Close()
		geo.Close()
	}()
	for _, addr := range []string{*setupAddr, *httpAddr, *httpsAddr} {
		ln, err := net.Listen("tcp", addr)
		if err != nil {
			return err
		}
		listeners = append(listeners, ln)
	}
	errs := make(chan error, 3)
	go func() { errs <- local.Serve(listeners[0]) }()
	go func() { errs <- plain.Serve(listeners[1]) }()
	go func() { errs <- secure.ServeTLS(listeners[2], "", "") }()
	if store.Get().Connect {
		rt.Connect()
	}
	slog.Info("setup console ready", "address", listeners[0].Addr(), "note", "open http://localhost:8080; Docker must publish setup on host loopback only")
	var serveErr error
	select {
	case <-ctx.Done():
	case serveErr = <-errs:
	}
	stop()
	rt.mu.RLock()
	admin := rt.admin
	rt.mu.RUnlock()
	shutdown, cancel := context.WithTimeout(context.Background(), 15*time.Second)
	defer cancel()
	for _, s := range []*http.Server{local, plain, secure, admin} {
		if s != nil {
			if err := s.Shutdown(shutdown); err != nil {
				s.Close()
			}
		}
	}
	// Start/connect can still be unwinding; wait before accessing the node.
	rt.Disconnect()
	rt.mu.RLock()
	started := rt.starting
	done := rt.done
	rt.mu.RUnlock()
	if started {
		<-done
		rt.mu.RLock()
		ts := rt.ts
		rt.mu.RUnlock()
		if ts != nil {
			ts.Close()
		}
	}
	if errors.Is(serveErr, http.ErrServerClosed) {
		return nil
	}
	return serveErr
}

func recoverAdmin(store *setup.Store, admins []string) error {
	reader := bufio.NewReader(os.Stdin)
	fmt.Fprint(os.Stderr, "New administrator password (12–72 bytes): ")
	password, err := reader.ReadString('\n')
	if err != nil && len(password) == 0 {
		return err
	}
	password = strings.TrimRight(password, "\r\n")
	if len(password) < 12 || len(password) > 72 {
		return errors.New("password must be 12–72 bytes")
	}
	if err := store.RecoverAdmin(password, admins); err != nil {
		return err
	}
	fmt.Fprintln(os.Stderr, "Password reset. Restart ts-egress to invalidate existing sessions.")
	return nil
}
