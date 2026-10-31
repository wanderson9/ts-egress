package gateway

import (
	"context"
	"errors"
	"io"
	"net"
	"net/http"
	"net/http/httptest"
	"testing"
	"time"
)

type failingResponseBody struct{ err error }

func (b failingResponseBody) Read([]byte) (int, error) { return 0, b.err }
func (failingResponseBody) Close() error               { return nil }

func TestObservedUpstreamBodyReportsFailureOnce(t *testing.T) {
	calls := 0
	b := &observedUpstreamBody{measuredBody: measuredBody{ReadCloser: failingResponseBody{err: io.ErrUnexpectedEOF}}, failed: func(error) { calls++ }}
	for i := 0; i < 2; i++ {
		_, _ = b.Read(make([]byte, 1))
	}
	if calls != 1 {
		t.Fatalf("failure callback calls = %d, want 1", calls)
	}
}

func TestObservedUpstreamBodyIgnoresEOF(t *testing.T) {
	calls := 0
	b := &observedUpstreamBody{measuredBody: measuredBody{ReadCloser: failingResponseBody{err: io.EOF}}, failed: func(error) { calls++ }}
	_, err := b.Read(make([]byte, 1))
	if !errors.Is(err, io.EOF) || calls != 0 {
		t.Fatalf("err=%v calls=%d", err, calls)
	}
}

func TestBodyTimeoutErrorIsClassified(t *testing.T) {
	if !isUpstreamBodyTimeout(upstreamBodyTimeoutError{}) {
		t.Fatal("timeout error not recognized")
	}
	if isUpstreamBodyTimeout(errors.New("other")) {
		t.Fatal("ordinary error recognized as timeout")
	}
}

func integrationGateway(t *testing.T, upstream string, timeout time.Duration) (*Gateway, *httptest.Server) {
	t.Helper()
	g := New((&net.Dialer{}).DialContext)
	g.upstreamBodyTimeout = timeout
	g.Health = NewHealth(t.TempDir())
	g.Replace([]Route{{Domain: "app.example.com", Upstream: upstream, Enabled: true}})
	proxy := httptest.NewServer(g)
	t.Cleanup(g.CloseIdleConnections)
	t.Cleanup(proxy.Close)
	return g, proxy
}

func doBodyRequest(t *testing.T, proxy *httptest.Server, ctx context.Context) (*http.Response, error) {
	t.Helper()
	req, err := http.NewRequestWithContext(ctx, http.MethodGet, proxy.URL, nil)
	if err != nil {
		t.Fatal(err)
	}
	req.Host = "app.example.com"
	return proxy.Client().Do(req)
}

func verifyObservedFailure(t *testing.T, g *Gateway, failures uint64, errors uint64, detail string) {
	t.Helper()
	if got := g.Failures.Load(); got != failures {
		t.Errorf("Failures=%d, want %d", got, failures)
	}
	var activity Totals
	for _, value := range g.Activity.Summaries() {
		activity.add(value)
	}
	if activity.Errors != errors {
		t.Errorf("Activity Errors=%d, want %d", activity.Errors, errors)
	}
	service := g.Health.Snapshot(g.Routes())["app.example.com"]
	if detail == "" {
		if service.LastError != "" {
			t.Errorf("LastError=%q, want empty", service.LastError)
		}
	} else if service.LastError != detail {
		t.Errorf("LastError=%q, want %q", service.LastError, detail)
	}
}

func TestProxyStalledResponseBodyFailure(t *testing.T) {
	started := make(chan struct{})
	upstream := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.WriteHeader(http.StatusOK)
		w.(http.Flusher).Flush()
		close(started)
		<-r.Context().Done()
	}))
	defer upstream.Close()
	g, proxy := integrationGateway(t, upstream.URL, 30*time.Millisecond)
	resp, err := doBodyRequest(t, proxy, context.Background())
	if err != nil {
		verifyObservedFailure(t, g, 1, 1, "upstream response body idle timeout")
		return
	}
	<-started
	_, _ = io.Copy(io.Discard, resp.Body)
	resp.Body.Close()
	verifyObservedFailure(t, g, 1, 1, "upstream response body idle timeout")
}

func TestProxyTruncatedResponseBodyFailure(t *testing.T) {
	upstream := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		w.Header().Set("Content-Length", "10")
		w.WriteHeader(http.StatusOK)
		w.(http.Flusher).Flush()
		_, _ = io.WriteString(w, "short")
	}))
	defer upstream.Close()
	g, proxy := integrationGateway(t, upstream.URL, time.Second)
	resp, err := doBodyRequest(t, proxy, context.Background())
	if err != nil {
		verifyObservedFailure(t, g, 1, 1, io.ErrUnexpectedEOF.Error())
		return
	}
	_, _ = io.Copy(io.Discard, resp.Body)
	resp.Body.Close()
	verifyObservedFailure(t, g, 1, 1, io.ErrUnexpectedEOF.Error())
}

func TestProxyDownstreamCancellationIsNotFailure(t *testing.T) {
	started := make(chan struct{})
	finished := make(chan struct{})
	upstream := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.WriteHeader(http.StatusOK)
		w.(http.Flusher).Flush()
		close(started)
		<-r.Context().Done()
		close(finished)
	}))
	defer upstream.Close()
	g, proxy := integrationGateway(t, upstream.URL, time.Second)
	ctx, cancel := context.WithCancel(context.Background())
	resp, err := doBodyRequest(t, proxy, ctx)
	if err != nil {
		t.Fatal(err)
	}
	<-started
	cancel()
	_, _ = io.Copy(io.Discard, resp.Body)
	resp.Body.Close()
	select {
	case <-finished:
	case <-time.After(time.Second):
		t.Fatal("downstream cancellation did not reach upstream")
	}
	verifyObservedFailure(t, g, 0, 0, "")
}

func TestProxyTruncated5xxBodyCountsOnce(t *testing.T) {
	upstream := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		w.Header().Set("Content-Length", "10")
		w.WriteHeader(http.StatusBadGateway)
		w.(http.Flusher).Flush()
		_, _ = io.WriteString(w, "short")
	}))
	defer upstream.Close()
	g, proxy := integrationGateway(t, upstream.URL, time.Second)
	resp, err := doBodyRequest(t, proxy, context.Background())
	if err != nil {
		verifyObservedFailure(t, g, 1, 1, io.ErrUnexpectedEOF.Error())
		return
	}
	_, _ = io.Copy(io.Discard, resp.Body)
	resp.Body.Close()
	verifyObservedFailure(t, g, 1, 1, io.ErrUnexpectedEOF.Error())
}
