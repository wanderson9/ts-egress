package ui

import (
	"net/http/httptest"
	"strings"
	"testing"
)

func TestStyleNonceMatchesPolicyAndChangesPerResponse(t *testing.T) {
	previous := ""
	for range 2 {
		w := httptest.NewRecorder()
		SetSecurityPolicy(w)
		Serve(w, httptest.NewRequest("GET", "/dashboard", nil), "dashboard")
		policy := w.Header().Get("Content-Security-Policy")
		_, rest, ok := strings.Cut(policy, "'nonce-")
		if !ok {
			t.Fatal("missing style nonce")
		}
		nonce, _, _ := strings.Cut(rest, "'")
		if nonce == "" || nonce == previous {
			t.Fatal("nonce absent or reused")
		}
		if !strings.Contains(w.Body.String(), `<meta name="csp-nonce" content="`+nonce+`">`) {
			t.Fatal("meta does not match policy")
		}
		if !strings.Contains(policy, "script-src 'self';") || strings.Contains(policy, "style-src 'self' 'unsafe-inline'") {
			t.Fatal("policy weakened", policy)
		}
		previous = nonce
	}
}
