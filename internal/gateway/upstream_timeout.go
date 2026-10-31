package gateway

import (
	"context"
	"errors"
	"io"
	"net/http"
	"sync"
	"time"
)

// observedUpstreamBody records only terminal read failures. EOF is normal, and
// an already-canceled downstream request must not be reported as backend error.
type observedUpstreamBody struct {
	measuredBody
	failed func(error)
	once   sync.Once
}

func (b *observedUpstreamBody) Read(p []byte) (int, error) {
	n, err := b.measuredBody.Read(p)
	if err != nil && !errors.Is(err, io.EOF) && b.failed != nil {
		b.once.Do(func() { b.failed(err) })
	}
	return n, err
}

type observedDuplex struct {
	measuredDuplex
	observed *observedUpstreamBody
}

func (b *observedDuplex) Read(p []byte) (int, error) { return b.observed.Read(p) }

func isUpstreamBodyTimeout(err error) bool {
	var timeout upstreamBodyTimeoutError
	return errors.As(err, &timeout)
}

// upstreamBodyIdleTimeout bounds stalled upstream body reads, not stream age.
const upstreamBodyIdleTimeout = PublicIdleTimeout

// upstreamTimeoutTransport gives each request a private cancellation context.
// The body wrapper bounds each blocked body read; cancellation
// stops only this request/HTTP2 stream, never the shared connection.
type upstreamTimeoutTransport struct {
	base    http.RoundTripper
	timeout time.Duration
}

func (t upstreamTimeoutTransport) RoundTrip(req *http.Request) (*http.Response, error) {
	ctx, cancel := context.WithCancel(req.Context())
	resp, err := t.base.RoundTrip(req.Clone(ctx))
	if err != nil {
		cancel()
		return nil, err
	}
	if resp.Body == nil || resp.Body == http.NoBody {
		cancel()
		return resp, nil
	}
	if resp.StatusCode == http.StatusSwitchingProtocols {
		if duplex, ok := resp.Body.(io.ReadWriteCloser); ok {
			resp.Body = &cancelOnCloseBody{ReadWriteCloser: duplex, cancel: cancel}
		} else {
			cancel()
		}
		return resp, nil
	}
	resp.Body = &upstreamIdleBody{ReadCloser: resp.Body, timeout: t.timeout, cancel: cancel}
	return resp, nil
}

type cancelOnCloseBody struct {
	io.ReadWriteCloser
	cancel context.CancelFunc
}

func (b *cancelOnCloseBody) Close() error { b.cancel(); return b.ReadWriteCloser.Close() }

type upstreamIdleBody struct {
	io.ReadCloser
	timeout    time.Duration
	cancel     context.CancelFunc
	mu         sync.Mutex
	timer      *time.Timer
	expired    bool
	generation uint64
}

func (b *upstreamIdleBody) beginRead() uint64 {
	b.mu.Lock()
	defer b.mu.Unlock()
	b.generation++
	gen := b.generation
	b.expired = false
	b.timer = time.AfterFunc(b.timeout, func() { b.expire(gen) })
	return gen
}
func (b *upstreamIdleBody) expire(gen uint64) {
	b.mu.Lock()
	if gen != b.generation {
		b.mu.Unlock()
		return
	}
	b.expired = true
	b.mu.Unlock()
	b.cancel()
}
func (b *upstreamIdleBody) Read(p []byte) (int, error) {
	gen := b.beginRead()
	n, err := b.ReadCloser.Read(p)
	b.mu.Lock()
	if gen == b.generation && b.timer != nil {
		b.timer.Stop()
		// Stop may race an already queued callback. Invalidate it before
		// returning, so downstream backpressure cannot expire this read.
		b.generation++
	}
	expired := b.expired
	b.mu.Unlock()
	if expired {
		return 0, upstreamBodyTimeoutError{}
	}
	return n, err
}
func (b *upstreamIdleBody) stop() {
	b.mu.Lock()
	b.generation++
	if b.timer != nil {
		b.timer.Stop()
	}
	b.mu.Unlock()
}
func (b *upstreamIdleBody) Close() error {
	b.stop()
	b.cancel()
	return b.ReadCloser.Close()
}

type upstreamBodyTimeoutError struct{}

func (upstreamBodyTimeoutError) Error() string   { return "upstream response body idle timeout" }
func (upstreamBodyTimeoutError) Timeout() bool   { return true }
func (upstreamBodyTimeoutError) Temporary() bool { return true }
