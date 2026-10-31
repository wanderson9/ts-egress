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

func TestEmptyAllowlistNeedsConfirmationOnlyWhenRemovingRestriction(t *testing.T) {
	if !requiresAdminConfirmation([]string{"admin@example.com"}, nil, "") {
		t.Fatal("restricted-to-empty transition must be confirmed")
	}
	if requiresAdminConfirmation(nil, nil, "") {
		t.Fatal("initial empty-list setup must remain savable")
	}
	if !requiresAdminConfirmation([]string{"admin@example.com"}, []string{"other@example.com"}, "admin@example.com") {
		t.Fatal("self-removal must remain confirmed")
	}
}

func TestTailnetOpenAdminAccessRequiresDedicatedConfirmation(t *testing.T) {
	s, store, _ := fixture(t)
	if err := store.Claim("a long test password"); err != nil {
		t.Fatal(err)
	}
	v := store.Get()
	v.Admins = []string{"me@example.com"}
	if err := store.Update(v); err != nil {
		t.Fatal(err)
	}
	host := "egress.tail.ts.net"
	h := s.gateway.AdminWithSetup("unused", host, "https://"+host, func(context.Context, string) (string, error) { return "me@example.com", nil }, s.Tailnet())
	v = store.Get()
	v.Admins = nil
	body, _ := json.Marshal(v)
	revision := store.Revision()
	for _, header := range []string{"", "X-Confirm-Admin-Lockout", "X-Confirm-Admin-Change"} {
		r := httptest.NewRequest("PUT", "https://"+host+"/api/setup/settings", strings.NewReader(string(body)))
		r.Header.Set("Origin", "https://"+host)
		r.Header.Set("Content-Type", "application/json")
		r.Header.Set("X-TS-Egress", "1")
		r.Header.Set("If-Match", strconv.FormatUint(revision, 10))
		if header != "" {
			r.Header.Set(header, "1")
		}
		w := httptest.NewRecorder()
		h.ServeHTTP(w, r)
		if header != "X-Confirm-Admin-Change" {
			if w.Code != http.StatusConflict || store.Revision() != revision {
				t.Fatalf("unconfirmed access broadening: %d %s", w.Code, w.Body.String())
			}
		} else if w.Code != http.StatusNoContent {
			t.Fatalf("confirmed save: %d %s", w.Code, w.Body.String())
		}
	}
}
