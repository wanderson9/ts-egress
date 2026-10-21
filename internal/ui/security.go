package ui

import (
	"crypto/rand"
	"encoding/base64"
	"net/http"
)

// React Aria reads csp-nonce when inserting its interaction/overlay styles.
// Keep scripts self-hosted and permit only nonce-bearing inline style elements.
func SetSecurityPolicy(w http.ResponseWriter) {
	var random [24]byte
	if _, err := rand.Read(random[:]); err != nil {
		panic(err)
	}
	nonce := base64.StdEncoding.EncodeToString(random[:])
	w.Header().Set("Content-Security-Policy", "default-src 'none'; script-src 'self'; style-src 'self' 'nonce-"+nonce+"'; style-src-attr 'unsafe-inline'; connect-src 'self'; frame-ancestors 'none'; base-uri 'none'; form-action 'self'")
}
