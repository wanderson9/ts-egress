package gateway

import (
	"bufio"
	"io"
	"net"
	"net/http"
	"net/http/httptest"
	"testing"
	"time"
)

func TestProxyUpgradeThroughIdleMiddleware(t *testing.T) {
	upstream := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		c, rw, err := http.NewResponseController(w).Hijack()
		if err != nil {
			return
		}
		defer c.Close()
		_, _ = rw.WriteString("HTTP/1.1 101 Switching Protocols\r\nConnection: Upgrade\r\nUpgrade: websocket\r\n\r\n")
		_ = rw.Flush()
		_, _ = io.Copy(c, c)
	}))
	defer upstream.Close()
	g := New((&net.Dialer{}).DialContext)
	g.Replace([]Route{{"app.example.com", upstream.URL, true}})
	s := httptest.NewServer(IdleTimeout(NewAdmission(512).Middleware(g)))
	defer s.Close()
	c, err := net.Dial("tcp", s.Listener.Addr().String())
	if err != nil {
		t.Fatal(err)
	}
	defer c.Close()
	_ = c.SetDeadline(time.Now().Add(2 * time.Second))
	_, _ = io.WriteString(c, "GET / HTTP/1.1\r\nHost: app.example.com\r\nConnection: Upgrade\r\nUpgrade: websocket\r\n\r\n")
	r := bufio.NewReader(c)
	resp, err := http.ReadResponse(r, nil)
	if err != nil || resp.StatusCode != 101 {
		t.Fatal("upgrade failed", resp, err)
	}
	_, _ = io.WriteString(c, "ping")
	b := make([]byte, 4)
	if _, err := io.ReadFull(r, b); err != nil || string(b) != "ping" {
		t.Fatal("upgrade duplex lost", string(b), err)
	}
}

func TestStalledUploadTimesOut(t *testing.T) {
	finished := make(chan error, 1)
	s := httptest.NewServer(idleTimeout(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		_, err := io.Copy(io.Discard, r.Body)
		finished <- err
	}), 30*time.Millisecond))
	defer s.Close()
	c, err := net.Dial("tcp", s.Listener.Addr().String())
	if err != nil {
		t.Fatal(err)
	}
	defer c.Close()
	_, _ = io.WriteString(c, "POST / HTTP/1.1\r\nHost: example.com\r\nContent-Length: 100\r\n\r\nx")
	select {
	case err := <-finished:
		if err == nil {
			t.Fatal("stalled upload accepted")
		}
	case <-time.After(time.Second):
		t.Fatal("stalled upload retained slot")
	}
}

func TestIdleConnProgressAndTimeout(t *testing.T) {
	a, b := net.Pipe()
	defer a.Close()
	defer b.Close()
	c := &idleConn{Conn: a, timeout: 50 * time.Millisecond}
	go func() {
		for range 5 {
			time.Sleep(20 * time.Millisecond)
			_, _ = b.Write([]byte("x"))
		}
	}()
	for range 5 {
		if _, err := c.Read(make([]byte, 1)); err != nil {
			t.Fatal("active stream closed", err)
		}
	}
	if _, err := c.Read(make([]byte, 1)); err == nil {
		t.Fatal("idle stream did not time out")
	}
	if _, err := c.Write([]byte("x")); err == nil {
		t.Fatal("stalled writer did not time out")
	}
}

func TestAdmissionPerSource(t *testing.T) {
	a := NewAdmission(512)
	ip := clientIP(httptest.NewRequest("GET", "/", nil))
	a.clients[ip] = 32
	h := a.Middleware(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) { w.WriteHeader(204) }))
	w := httptest.NewRecorder()
	h.ServeHTTP(w, httptest.NewRequest("GET", "/", nil))
	if w.Code != 429 {
		t.Fatal(w.Code)
	}
	r := httptest.NewRequest("GET", "/", nil)
	r.RemoteAddr = "192.0.2.2:1234"
	w = httptest.NewRecorder()
	h.ServeHTTP(w, r)
	if w.Code != 204 {
		t.Fatal(w.Code)
	}
}

func TestHTTPStreamHasNoTotalDeadline(t *testing.T) {
	s := httptest.NewServer(idleTimeout(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		for range 5 {
			// An idle application stream is safe if the client is not blocking a write.
			time.Sleep(50 * time.Millisecond)
			if _, err := w.Write([]byte("x")); err != nil {
				return
			}
			if err := http.NewResponseController(w).Flush(); err != nil {
				return
			}
		}
	}), 30*time.Millisecond))
	defer s.Close()
	resp, err := s.Client().Get(s.URL)
	if err != nil {
		t.Fatal(err)
	}
	defer resp.Body.Close()
	b, err := io.ReadAll(resp.Body)
	if err != nil || string(b) != "xxxxx" {
		t.Fatal("stream lifetime limited", string(b), err)
	}
}

func TestStalledHTTPResponseTimesOut(t *testing.T) {
	finished := make(chan error, 1)
	s := httptest.NewServer(idleTimeout(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		chunk := make([]byte, 1<<20)
		for range 128 {
			if _, err := w.Write(chunk); err != nil {
				finished <- err
				return
			}
		}
		finished <- nil
	}), 30*time.Millisecond))
	defer s.Close()
	c, err := net.Dial("tcp", s.Listener.Addr().String())
	if err != nil {
		t.Fatal(err)
	}
	defer c.Close()
	_, _ = io.WriteString(c, "GET / HTTP/1.1\r\nHost: example.com\r\n\r\n")
	select {
	case err := <-finished:
		if err == nil {
			t.Fatal("stalled response did not time out")
		}
	case <-time.After(3 * time.Second):
		t.Fatal("stalled response retained slot")
	}
}
