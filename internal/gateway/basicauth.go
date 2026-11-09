package gateway

import (
	"crypto/hmac"
	"crypto/rand"
	"crypto/sha256"
	"crypto/subtle"
	"encoding/base64"
	"encoding/binary"
	"net/http"
	"net/netip"
	"strconv"
	"strings"
	"sync"
	"time"

	"golang.org/x/crypto/bcrypt"
)

const (
	// MinBasicAuthPassword applies when a client sets a new password. Verifiers
	// already stored for older, shorter passwords continue to work.
	MinBasicAuthPassword = 12

	authCacheTTL      = 5 * time.Minute
	authCacheEntries  = 4096
	authFailureLimit  = 10
	authFailureWindow = time.Minute
	authFailureSlots  = 65536
	authQueueWait     = 5 * time.Second
)

// dummyVerifier gives a wrong username the same bcrypt cost as a wrong password.
var dummyVerifier = sync.OnceValue(func() []byte {
	hash, err := bcrypt.GenerateFromPassword([]byte("ts-egress dummy verifier"), bcrypt.DefaultCost)
	if err != nil {
		panic(err)
	}
	return hash
})

type authFailures struct {
	count int
	reset time.Time
}

// authGuard keeps bcrypt away from the normal request path and limits guessing.
// Verified credentials are remembered only as keyed hashes, and the key is
// random per process, so neither passwords nor offline-attackable digests are
// retained.
type authGuard struct {
	key      [32]byte
	mu       sync.Mutex
	verified map[[sha256.Size]byte]time.Time
	failures map[netip.Addr]authFailures
	now      func() time.Time
	wait     time.Duration
}

func newAuthGuard() *authGuard {
	a := &authGuard{verified: map[[sha256.Size]byte]time.Time{}, failures: map[netip.Addr]authFailures{}, now: time.Now, wait: authQueueWait}
	if _, err := rand.Read(a.key[:]); err != nil {
		panic(err)
	}
	return a
}

// fingerprint binds a presented credential to the current route and verifier, so
// changing the password or username invalidates any remembered success.
func (a *authGuard) fingerprint(host, user, password, verifier string) [sha256.Size]byte {
	mac := hmac.New(sha256.New, a.key[:])
	for _, field := range []string{host, user, password, verifier} {
		var size [8]byte
		binary.BigEndian.PutUint64(size[:], uint64(len(field)))
		mac.Write(size[:])
		mac.Write([]byte(field))
	}
	var out [sha256.Size]byte
	mac.Sum(out[:0])
	return out
}

func (a *authGuard) isVerified(fingerprint [sha256.Size]byte) bool {
	a.mu.Lock()
	defer a.mu.Unlock()
	expires, ok := a.verified[fingerprint]
	if ok && a.now().Before(expires) {
		return true
	}
	delete(a.verified, fingerprint)
	return false
}

func (a *authGuard) remember(fingerprint [sha256.Size]byte) {
	a.mu.Lock()
	defer a.mu.Unlock()
	now := a.now()
	if len(a.verified) >= authCacheEntries {
		for key, expires := range a.verified {
			if !now.Before(expires) {
				delete(a.verified, key)
			}
		}
		if len(a.verified) >= authCacheEntries {
			a.verified = map[[sha256.Size]byte]time.Time{}
		}
	}
	a.verified[fingerprint] = now.Add(authCacheTTL)
}

func (a *authGuard) throttled(source netip.Addr) bool {
	a.mu.Lock()
	defer a.mu.Unlock()
	entry, ok := a.failures[source]
	if !ok {
		return false
	}
	if !a.now().Before(entry.reset) {
		delete(a.failures, source)
		return false
	}
	return entry.count >= authFailureLimit
}

func (a *authGuard) failed(source netip.Addr) {
	a.mu.Lock()
	defer a.mu.Unlock()
	now := a.now()
	entry, ok := a.failures[source]
	if !ok || !now.Before(entry.reset) {
		if !ok && len(a.failures) >= authFailureSlots {
			for key, value := range a.failures {
				if !now.Before(value.reset) {
					delete(a.failures, key)
				}
			}
			// Under a distributed attack the bcrypt concurrency bound still applies.
			if len(a.failures) >= authFailureSlots {
				return
			}
		}
		entry = authFailures{reset: now.Add(authFailureWindow)}
	}
	entry.count++
	a.failures[source] = entry
}

func challenge(w http.ResponseWriter, host string) {
	w.Header().Set("WWW-Authenticate", `Basic realm="`+host+`", charset="UTF-8"`)
	http.Error(w, "authentication required", http.StatusUnauthorized)
}

func rejectThrottled(w http.ResponseWriter) {
	w.Header().Set("Retry-After", strconv.Itoa(int(authFailureWindow/time.Second)))
	http.Error(w, "too many failed attempts", http.StatusTooManyRequests)
}

// verifyPassword always runs bcrypt, including for unknown usernames and
// oversized passwords, so response timing does not reveal which part was wrong.
func verifyPassword(verifier, user, want, password string) bool {
	hash := dummyVerifier()
	stored := false
	if encoded, ok := strings.CutPrefix(verifier, "bcrypt:"); ok {
		if decoded, err := base64.RawStdEncoding.DecodeString(encoded); err == nil {
			hash = decoded
			stored = true
		}
	}
	candidate := password
	if len(candidate) > 72 {
		candidate = candidate[:72]
	}
	matches := bcrypt.CompareHashAndPassword(hash, []byte(candidate)) == nil
	userMatches := subtle.ConstantTimeCompare([]byte(user), []byte(want)) == 1
	return stored && matches && userMatches && len(password) <= 72
}

// authenticate writes the failure response itself and reports whether to proceed.
func (g *Gateway) authenticate(w http.ResponseWriter, r *http.Request, host string, credential authCredential) bool {
	user, password, ok := r.BasicAuth()
	if !ok {
		challenge(w, host)
		return false
	}
	fingerprint := g.auth.fingerprint(host, user, password, credential.password)
	if g.auth.isVerified(fingerprint) {
		return true
	}
	source := sourceKey(clientIP(r))
	if g.auth.throttled(source) {
		rejectThrottled(w)
		return false
	}
	timer := time.NewTimer(g.auth.wait)
	defer timer.Stop()
	select {
	case g.authSlots <- struct{}{}:
		defer func() { <-g.authSlots }()
	case <-r.Context().Done():
		return false
	case <-timer.C:
		w.Header().Set("Retry-After", "1")
		http.Error(w, "authentication service busy", http.StatusServiceUnavailable)
		return false
	}
	// A parallel request may have verified this credential, or used up this
	// source's remaining attempts, while this one waited for a slot.
	if g.auth.isVerified(fingerprint) {
		return true
	}
	if g.auth.throttled(source) {
		rejectThrottled(w)
		return false
	}
	if verifyPassword(credential.password, user, credential.user, password) {
		g.auth.remember(fingerprint)
		return true
	}
	g.auth.failed(source)
	challenge(w, host)
	return false
}
