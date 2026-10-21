package gateway

import (
	"context"
	"crypto/subtle"
	"encoding/json"
	"io"
	"log/slog"
	"net/http"
	"strconv"
	"ts-egress/internal/ui"
)

type Identity func(context.Context, string) (string, error)
type adminAccess struct{ actor string }
type adminAccessKey struct{}

// AdminIdentity is set only by the verified tailnet authorization wrapper.
func AdminIdentity(ctx context.Context) string {
	a, _ := ctx.Value(adminAccessKey{}).(adminAccess)
	return a.actor
}

func (g *Gateway) Admin(path, host string, identify Identity) http.Handler {
	return g.AdminOrigin(path, host, "https://"+host, identify)
}

// AdminOrigin also supports the authenticated loopback recovery console.
func (g *Gateway) AdminOrigin(path, host, origin string, identify Identity) http.Handler {
	return g.AdminWithSetup(path, host, origin, identify, nil)
}
func (g *Gateway) AdminWithSetup(path, host, origin string, identify Identity, settings http.Handler) http.Handler {
	mux := http.NewServeMux()
	mux.HandleFunc("GET /api/version", ui.VersionInfo)
	mux.HandleFunc("GET /settings", func(w http.ResponseWriter, r *http.Request) { ui.Serve(w, r, "settings") })
	configurationAdmin := func(r *http.Request) bool {
		access, ok := r.Context().Value(adminAccessKey{}).(adminAccess)
		return ok && access.actor != ""
	}
	if settings != nil {
		wrapped := http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
			if r.URL.Path == "/api/setup/settings" {
				if !configurationAdmin(r) {
					http.Error(w, "local administrator credentials required", http.StatusForbidden)
					return
				}
			} else if !privateIdentityRequest(r) && r.URL.Path != "/api/setup/password" {
				http.Error(w, "not authorized", http.StatusForbidden)
				return
			}
			settings.ServeHTTP(w, r)
		})
		mux.Handle("GET /api/setup/", wrapped)
		mux.Handle("POST /api/setup/", wrapped)
		mux.Handle("OPTIONS /api/setup/", wrapped)
		mux.Handle("PATCH /api/setup/", wrapped)
		mux.Handle("PUT /api/setup/", wrapped)
		mux.Handle("GET /setup", wrapped)
		mux.Handle("POST /api/setup/password", wrapped)
	}
	validMutation := func(r *http.Request) bool {
		if r.Header.Get("Origin") != origin || r.Header.Get("Content-Type") != "application/json" || r.Header.Get("X-TS-Egress") != "1" {
			return false
		}
		access, ok := r.Context().Value(adminAccessKey{}).(adminAccess)
		return ok && access.actor != "" && subtle.ConstantTimeCompare([]byte(r.Header.Get("Origin")), []byte(origin)) == 1
	}
	mux.HandleFunc("GET /api/blocks", func(w http.ResponseWriter, r *http.Request) {
		if g.Blocks == nil {
			http.Error(w, "IP blocking unavailable", 503)
			return
		}
		json.NewEncoder(w).Encode(map[string]any{"ips": g.Blocks.List()})
	})
	mux.HandleFunc("PUT /api/blocks", func(w http.ResponseWriter, r *http.Request) {
		if !validMutation(r) {
			http.Error(w, "invalid request origin", 403)
			return
		}
		if g.Blocks == nil {
			http.Error(w, "IP blocking unavailable", 503)
			return
		}
		r.Body = http.MaxBytesReader(w, r.Body, 1024)
		var body struct {
			IP      string `json:"ip"`
			Blocked bool   `json:"blocked"`
		}
		dec := json.NewDecoder(r.Body)
		dec.DisallowUnknownFields()
		if dec.Decode(&body) != nil || dec.Decode(new(any)) != io.EOF {
			http.Error(w, "invalid JSON", 400)
			return
		}
		if err := g.Blocks.Set(body.IP, body.Blocked); err != nil {
			http.Error(w, "could not save IP block: "+err.Error(), 500)
			return
		}
		w.WriteHeader(http.StatusNoContent)
	})
	mux.HandleFunc("GET /api/status", func(w http.ResponseWriter, r *http.Request) {
		routes, revision := g.Configuration()
		health := map[string]ServiceHealth{}
		if g.Health != nil {
			health = g.Health.Snapshot(routes)
		}
		connection := map[string]any{}
		if g.Connection != nil {
			connection = g.Connection(r.Context())
		}
		w.Header().Set("ETag", `"`+strconv.FormatUint(revision, 10)+`"`)
		json.NewEncoder(w).Encode(map[string]any{"routes": routes, "revision": revision, "requests": g.Requests.Load(), "failures": g.Failures.Load(), "sites": g.Activity.Summaries(), "health": health, "connection": connection})
	})
	mux.HandleFunc("POST /api/certificate/retry", func(w http.ResponseWriter, r *http.Request) {
		if !validMutation(r) {
			http.Error(w, "invalid request origin", 403)
			return
		}
		r.Body = http.MaxBytesReader(w, r.Body, 4096)
		var body struct {
			Domain string `json:"domain"`
		}
		dec := json.NewDecoder(r.Body)
		dec.DisallowUnknownFields()
		if dec.Decode(&body) != nil || dec.Decode(new(any)) != io.EOF {
			http.Error(w, "invalid JSON", 400)
			return
		}
		if g.Allows(r.Context(), body.Domain) != nil {
			http.Error(w, "service must be enabled", 400)
			return
		}
		if g.Health == nil || g.RetryCertificate == nil {
			http.Error(w, "certificate retry unavailable", 503)
			return
		}
		if !g.Health.RetryContext(g.RetryContext, body.Domain, g.RetryCertificate) {
			http.Error(w, "retry unavailable; wait one minute after the previous attempt", 409)
			return
		}
		w.WriteHeader(http.StatusAccepted)
	})
	mux.HandleFunc("GET /api/activity", func(w http.ResponseWriter, r *http.Request) {
		domain := r.URL.Query().Get("domain")
		found := false
		for _, route := range g.Routes() {
			if route.Domain == domain {
				found = true
				break
			}
		}
		if !found {
			http.NotFound(w, r)
			return
		}
		activity := g.Activity.Snapshot(domain)
		countries := map[string]Country{}
		if g.Geo != nil && g.Geo.Enabled() && r.URL.Query().Get("geolocation") == "enabled" {
			countries = g.Geo.Countries(activity.Clients)
		}
		blocked := []string{}
		if g.Blocks != nil {
			blocked = g.Blocks.List()
		}
		json.NewEncoder(w).Encode(map[string]any{"activity": activity, "countries": countries, "blockedIPs": blocked, "blockingAvailable": g.Blocks != nil})
	})
	mux.HandleFunc("GET /api/geolocation", func(w http.ResponseWriter, r *http.Request) {
		if !privateIdentityRequest(r) {
			http.Error(w, "tailnet identity required", http.StatusForbidden)
			return
		}
		if g.Geo == nil {
			http.Error(w, "geolocation unavailable", http.StatusServiceUnavailable)
			return
		}
		w.Header().Set("Content-Type", "application/json")
		_ = json.NewEncoder(w).Encode(map[string]bool{"enabled": g.Geo.Enabled()})
	})
	mux.HandleFunc("PUT /api/geolocation", func(w http.ResponseWriter, r *http.Request) {
		if !privateIdentityRequest(r) {
			http.Error(w, "tailnet identity required", http.StatusForbidden)
			return
		}
		if !validMutation(r) {
			http.Error(w, "invalid request origin or content type", http.StatusForbidden)
			return
		}
		if g.Geo == nil {
			http.Error(w, "geolocation unavailable", http.StatusServiceUnavailable)
			return
		}
		r.Body = http.MaxBytesReader(w, r.Body, 1024)
		var body struct {
			Enabled bool `json:"enabled"`
		}
		dec := json.NewDecoder(r.Body)
		dec.DisallowUnknownFields()
		if dec.Decode(&body) != nil || dec.Decode(new(any)) != io.EOF {
			http.Error(w, "invalid JSON", http.StatusBadRequest)
			return
		}
		if err := g.Geo.SetEnabled(body.Enabled); err != nil {
			http.Error(w, "could not persist geolocation preference", http.StatusInternalServerError)
			return
		}
		w.WriteHeader(http.StatusNoContent)
	})
	mux.HandleFunc("PUT /api/routes", func(w http.ResponseWriter, r *http.Request) {
		if !validMutation(r) {
			http.Error(w, "invalid request origin or content type", 403)
			return
		}
		r.Body = http.MaxBytesReader(w, r.Body, 256*1024)
		dec := json.NewDecoder(r.Body)
		dec.DisallowUnknownFields()
		var routes []Route
		if err := dec.Decode(&routes); err != nil {
			http.Error(w, "invalid route JSON", 400)
			return
		}
		if err := dec.Decode(new(any)); err != io.EOF {
			http.Error(w, "unexpected trailing data", 400)
			return
		}
		if err := Validate(routes); err != nil {
			http.Error(w, err.Error(), 400)
			return
		}
		revision, err := strconv.ParseUint(r.Header.Get("If-Match"), 10, 64)
		if err != nil {
			http.Error(w, "route configuration changed; reload before saving", http.StatusConflict)
			return
		}
		g.configMu.Lock()
		defer g.configMu.Unlock()
		if revision != g.Revision() {
			http.Error(w, "route configuration changed; reload before saving", http.StatusConflict)
			return
		}
		if revision == ^uint64(0) {
			http.Error(w, "route revision exhausted", http.StatusConflict)
			return
		}
		if err := Save(path, routes); err != nil {
			slog.Error("save routes", "error", err)
			http.Error(w, "could not persist routes", 500)
			return
		}
		g.replace(routes)
		slog.Info("routes updated", "actor", r.Context().Value(actorKey{}), "count", len(routes))
		w.WriteHeader(http.StatusNoContent)
	})
	mux.HandleFunc("GET /", func(w http.ResponseWriter, r *http.Request) {
		ui.Serve(w, r, "dashboard")
	})
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "application/json")
		w.Header().Set("Cache-Control", "no-store")
		w.Header().Set("X-Content-Type-Options", "nosniff")
		w.Header().Set("Referrer-Policy", "no-referrer")
		if len(origin) >= 8 && origin[:8] == "https://" {
			w.Header().Set("Strict-Transport-Security", "max-age=31536000")
		}
		ui.SetSecurityPolicy(w)
		if r.Host != host {
			http.Error(w, "invalid host", 403)
			return
		}
		if r.Method == http.MethodOptions {
			w.WriteHeader(http.StatusNoContent)
			return
		}
		who, err := identify(r.Context(), r.RemoteAddr)
		if err != nil || who == "" {
			http.Error(w, "not authorized", 403)
			return
		}
		access := adminAccess{actor: who}
		ctx := context.WithValue(context.WithValue(r.Context(), actorKey{}, who), adminAccessKey{}, access)
		if r.Method != http.MethodGet && r.Method != http.MethodHead && r.Method != http.MethodOptions && r.Header.Get("Origin") != origin {
			http.Error(w, "invalid request origin", http.StatusForbidden)
			return
		}
		mux.ServeHTTP(w, r.WithContext(ctx))
	})
}

func privateIdentityRequest(r *http.Request) bool {
	a, ok := r.Context().Value(adminAccessKey{}).(adminAccess)
	return ok && a.actor != ""
}

type actorKey struct{}
