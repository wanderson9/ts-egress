package gateway

import (
	"context"
	"net"
	"net/http"
	"net/http/httptest"
	"testing"
)

func TestProbePublicHostAndUpstreamTLS(t *testing.T) {
	for _, secure := range []bool{false, true} {
		name := "HTTP"
		if secure {
			name = "HTTPS"
		}
		t.Run(name, func(t *testing.T) {
			hosts := make(chan string, 1)
			handler := http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
				hosts <- r.Host
				if r.Host != "app.example.com" {
					w.WriteHeader(http.StatusServiceUnavailable)
					return
				}
				w.WriteHeader(http.StatusOK)
			})
			var backend *httptest.Server
			if secure {
				backend = httptest.NewTLSServer(handler)
			} else {
				backend = httptest.NewServer(handler)
			}
			defer backend.Close()
			g := New((&net.Dialer{}).DialContext)
			defer g.CloseIdleConnections()
			g.Health = NewHealth(t.TempDir())
			routes := []Route{{"app.example.com", backend.URL, true, "", "", false}}
			g.Replace(routes)
			// Trust the upstream certificate, which is not valid for the public domain.
			g.checkBackends(context.Background(), backend.Client())
			s := g.Health.Snapshot(routes)[routes[0].Domain]
			if s.Backend.State != "Online" {
				t.Fatalf("probe: %+v", s.Backend)
			}
			select {
			case host := <-hosts:
				if host != routes[0].Domain {
					t.Fatalf("Host = %q", host)
				}
			default:
				t.Fatal("backend not reached")
			}
		})
	}
}
