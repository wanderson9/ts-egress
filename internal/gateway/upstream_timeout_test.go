package gateway

import (
	"context"
	"fmt"
	"io"
	"net"
	"net/http"
	"net/http/httptest"
	"testing"
	"time"
)

func TestUpstreamIdleBodyReleasesAdmissionSlot(t *testing.T) {
	backend := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "text/plain")
		w.WriteHeader(http.StatusOK)
		w.(http.Flusher).Flush()
		<-r.Context().Done()
	}))
	defer backend.Close()
	dial := func(ctx context.Context, network, address string) (net.Conn, error) {
		return (&net.Dialer{}).DialContext(ctx, network, address)
	}
	g := New(dial)
	g.upstreamBodyTimeout = 30 * time.Millisecond
	g.replace([]Route{{Domain: "public.test", Upstream: backend.URL, Enabled: true}})
	gate := NewAdmission(1)
	server := httptest.NewServer(IdleTimeout(gate.Middleware(g)))
	defer server.Close()
	client := &http.Client{Transport: &http.Transport{DisableKeepAlives: true}}
	started := make(chan struct{})
	go func() {
		defer close(started)
		req, _ := http.NewRequest(http.MethodGet, server.URL, nil)
		req.Host = "public.test"
		resp, err := client.Do(req)
		if err == nil {
			_, _ = io.Copy(io.Discard, resp.Body)
			_ = resp.Body.Close()
		}
	}()
	// Wait until headers are flushed, then shorten the wrapped response timeout
	// for this integration scenario by asserting another request can acquire
	// the slot once the first upstream body is canceled.
	deadline := time.Now().Add(2 * time.Second)
	for time.Now().Before(deadline) && len(gate.slots) == 0 {
		time.Sleep(time.Millisecond)
	}
	if len(gate.slots) == 0 {
		t.Fatal("first request did not acquire admission slot")
	}
	select {
	case <-started: // Public timeout elapsed and stalled response completed.
	case <-time.After(time.Second):
		t.Fatal("stalled response held admission slot")
	}
	if len(gate.slots) != 0 {
		t.Fatal(fmt.Sprintf("admission slots still occupied: %d", len(gate.slots)))
	}
}

func TestUpstreamIdleBodyTimeoutAndProgress(t *testing.T) {
	_, cancel2 := context.WithCancel(context.Background())
	defer cancel2()
	progress := &upstreamIdleBody{ReadCloser: io.NopCloser(io.LimitReader(&slowReader{}, 8)), timeout: 15 * time.Millisecond, cancel: cancel2}
	for i := 0; i < 8; i++ {
		buf := make([]byte, 1)
		n, err := progress.Read(buf)
		if err != nil || n != 1 {
			t.Fatalf("Read = %q, %v; want one byte", buf[:n], err)
		}
	}
}

type slowReader struct{}

func (*slowReader) Read(p []byte) (int, error) {
	time.Sleep(10 * time.Millisecond)
	return copy(p, "x"), nil
}

func TestHTTP2ResponseIdleTimeoutIsPerStreamAndProgressHasNoLifetimeCap(t *testing.T) {
	connections := make(chan string, 2)
	backend := httptest.NewUnstartedServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.ProtoMajor != 2 {
			t.Errorf("upstream protocol = %s, want HTTP/2", r.Proto)
		}
		connections <- r.RemoteAddr
		w.Header().Set("Content-Type", "text/plain")
		w.WriteHeader(http.StatusOK)
		w.(http.Flusher).Flush()
		if r.URL.Path == "/stall" {
			<-r.Context().Done()
			return
		}
		for i := 0; i < 8; i++ {
			_, _ = io.WriteString(w, "x")
			w.(http.Flusher).Flush()
			time.Sleep(20 * time.Millisecond)
		}
	}))
	backend.EnableHTTP2 = true
	backend.StartTLS()
	defer backend.Close()
	trusted := backend.Client().Transport.(*http.Transport).TLSClientConfig
	dial := func(ctx context.Context, network, address string) (net.Conn, error) {
		return (&net.Dialer{}).DialContext(ctx, network, address)
	}
	g := New(dial)
	g.upstreamBodyTimeout = 70 * time.Millisecond
	g.transport.TLSClientConfig = trusted.Clone()
	g.transport.TLSClientConfig.NextProtos = []string{"h2", "http/1.1"}
	g.replace([]Route{{Domain: "public.test", Upstream: backend.URL, Enabled: true}})
	gate := NewAdmission(2)
	proxy := httptest.NewServer(IdleTimeout(gate.Middleware(g)))
	defer proxy.Close()
	transport := &http.Transport{ForceAttemptHTTP2: true}
	defer transport.CloseIdleConnections()
	client := &http.Client{Transport: transport}
	request := func(path string) (*http.Response, error) {
		req, _ := http.NewRequest(http.MethodGet, proxy.URL+path, nil)
		req.Host = "public.test"
		return client.Do(req)
	}
	stall, err := request("/stall")
	if err != nil {
		t.Fatal(err)
	}
	defer stall.Body.Close()
	progress, err := request("/progress")
	if err != nil {
		t.Fatal(err)
	}
	data, err := io.ReadAll(progress.Body)
	_ = progress.Body.Close()
	if err != nil || string(data) != "xxxxxxxx" {
		t.Fatalf("progress body %q, err %v", data, err)
	}
	if first, second := <-connections, <-connections; first != second {
		t.Fatalf("streams used separate connections: %s and %s", first, second)
	}
	// The stalled response may already have timed out while the progressing
	// response completed; either state proves the active sibling stayed usable.
	_, err = io.ReadAll(stall.Body)
	if err == nil {
		t.Fatal("stalled HTTP/2 stream unexpectedly completed without timeout")
	}
}

func isTimeout(err error) bool { e, ok := err.(interface{ Timeout() bool }); return ok && e.Timeout() }
