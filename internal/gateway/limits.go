package gateway

import (
	"bufio"
	"io"
	"net"
	"net/http"
	"time"
)

// PublicIdleTimeout bounds stalled transfers, not total stream lifetime.
// WebSocket clients must send a ping or other traffic within this interval.
const PublicIdleTimeout = 60 * time.Second

func IdleTimeout(next http.Handler) http.Handler {
	return idleTimeout(next, PublicIdleTimeout)
}

func idleTimeout(next http.Handler, timeout time.Duration) http.Handler {
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		controller := http.NewResponseController(w)
		if r.Body != nil && r.Body != http.NoBody {
			r.Body = &idleBody{ReadCloser: r.Body, controller: controller, timeout: timeout}
		}
		defer controller.SetReadDeadline(time.Time{})
		defer controller.SetWriteDeadline(time.Time{})
		next.ServeHTTP(&idleWriter{ResponseWriter: w, timeout: timeout}, r)
	})
}

type idleBody struct {
	io.ReadCloser
	controller *http.ResponseController
	timeout    time.Duration
}

func (b *idleBody) Read(p []byte) (int, error) {
	_ = b.controller.SetReadDeadline(time.Now().Add(b.timeout))
	n, err := b.ReadCloser.Read(p)
	_ = b.controller.SetReadDeadline(time.Time{})
	return n, err
}

type idleWriter struct {
	http.ResponseWriter
	timeout time.Duration
}

func (w *idleWriter) Unwrap() http.ResponseWriter { return w.ResponseWriter }
func (w *idleWriter) Write(p []byte) (int, error) {
	c := http.NewResponseController(w.ResponseWriter)
	_ = c.SetWriteDeadline(time.Now().Add(w.timeout))
	defer c.SetWriteDeadline(time.Time{})
	return w.ResponseWriter.Write(p)
}
func (w *idleWriter) FlushError() error {
	c := http.NewResponseController(w.ResponseWriter)
	_ = c.SetWriteDeadline(time.Now().Add(w.timeout))
	defer c.SetWriteDeadline(time.Time{})
	return c.Flush()
}
func (w *idleWriter) Hijack() (net.Conn, *bufio.ReadWriter, error) {
	conn, rw, err := http.NewResponseController(w.ResponseWriter).Hijack()
	if err != nil {
		return nil, nil, err
	}
	// ReverseProxy writes the upgrade response through the original buffered
	// writer before copying through conn, so bound that first flush as well.
	_ = conn.SetWriteDeadline(time.Now().Add(w.timeout))
	return &idleConn{Conn: conn, timeout: w.timeout}, rw, nil
}

type idleConn struct {
	net.Conn
	timeout time.Duration
}

func (c *idleConn) Read(p []byte) (int, error) {
	_ = c.Conn.SetReadDeadline(time.Now().Add(c.timeout))
	return c.Conn.Read(p)
}
func (c *idleConn) Write(p []byte) (int, error) {
	_ = c.Conn.SetWriteDeadline(time.Now().Add(c.timeout))
	return c.Conn.Write(p)
}
