package gateway

import (
	"context"
	"errors"
	"net"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
)

func TestErrorPages(t *testing.T) {
	for _, status := range []int{403, 404, 421, 502, 503, 504} {
		for _, method := range []string{"GET", "HEAD"} {
			r := httptest.NewRequest(method, "http://evil.example/?token=secret", nil)
			w := httptest.NewRecorder()
			ErrorPage(w, r, status)
			if w.Code != status || !strings.HasPrefix(w.Header().Get("Content-Type"), "text/html") || !strings.Contains(w.Header().Get("Content-Security-Policy"), "sha256-") {
				t.Fatal(status, w.Code, w.Header())
			}
			if strings.Contains(w.Body.String(), "evil.example") || strings.Contains(w.Body.String(), "secret") || strings.Contains(w.Body.String(), "/setup") {
				t.Fatal("error leaked request or management information")
			}
			if method == "HEAD" && w.Body.Len() != 0 {
				t.Fatal("HEAD response has body")
			}
		}
	}
}
func TestProxyFailurePage(t *testing.T) {
	g := New(func(context.Context, string, string) (net.Conn, error) {
		return nil, errors.New("private backend detail")
	})
	g.Replace([]Route{{"app.example.com", "http://server:3000", true}})
	w := httptest.NewRecorder()
	g.ServeHTTP(w, httptest.NewRequest("GET", "http://app.example.com/", nil))
	if w.Code != http.StatusBadGateway || !strings.Contains(w.Body.String(), "Service unavailable") || strings.Contains(w.Body.String(), "private backend detail") {
		t.Fatal(w.Code, w.Body.String())
	}
	if proxyErrorStatus(context.DeadlineExceeded) != 504 {
		t.Fatal("timeout not classified")
	}
}
func TestUpstreamErrorsPreserved(t *testing.T) {
	up := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.WriteHeader(404)
		w.Write([]byte("application not found"))
	}))
	defer up.Close()
	g := New((&net.Dialer{}).DialContext)
	g.Replace([]Route{{"app.example.com", up.URL, true}})
	w := httptest.NewRecorder()
	g.ServeHTTP(w, httptest.NewRequest("GET", "http://app.example.com/", nil))
	if w.Code != 404 || w.Body.String() != "application not found" {
		t.Fatal("replaced upstream error")
	}
}
