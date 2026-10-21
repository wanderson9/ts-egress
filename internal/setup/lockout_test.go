package setup

import (
	"context"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"strconv"
	"strings"
	"testing"
)

func TestTailnetAdminRemovalRequiresConfirmation(t *testing.T) {
	s, store, _ := fixture(t)
	if err := store.Claim("a long test password"); err != nil {
		t.Fatal(err)
	}
	host := "egress.tail.ts.net"
	h := s.gateway.AdminWithSetup("unused", host, "https://"+host, func(context.Context, string) (string, error) { return "me@example.com", nil }, s.Tailnet())
	status := httptest.NewRecorder()
	h.ServeHTTP(status, httptest.NewRequest("GET", "https://"+host+"/api/setup/status", nil))
	if !strings.Contains(status.Body.String(), `"identity":"me@example.com"`) {
		t.Fatal("verified identity missing", status.Body.String())
	}
	v := store.Get()
	v.Admins = []string{"someone@example.com"}
	body, _ := json.Marshal(v)
	revision := store.Revision()
	for _, confirmed := range []bool{false, true} {
		r := httptest.NewRequest("PUT", "https://"+host+"/api/setup/settings", strings.NewReader(string(body)))
		r.Header.Set("Origin", "https://"+host)
		r.Header.Set("Content-Type", "application/json")
		r.Header.Set("X-TS-Egress", "1")
		r.Header.Set("If-Match", strconv.FormatUint(revision, 10))
		if confirmed {
			r.Header.Set("X-Confirm-Admin-Lockout", "1")
		}
		w := httptest.NewRecorder()
		h.ServeHTTP(w, r)
		if !confirmed && (w.Code != http.StatusConflict || store.Revision() != revision) {
			t.Fatal("unconfirmed lockout saved", w.Code)
		}
		if confirmed && w.Code != http.StatusNoContent {
			t.Fatal("confirmed lockout failed", w.Code, w.Body.String())
		}
	}
}
