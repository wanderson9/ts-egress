package gateway

import (
	"context"
	"encoding/json"
	"io"
	"net/http"
	"net/netip"
	"os"
	"path/filepath"
	"strconv"
	"sync"
	"time"
)

type Country struct {
	Country string    `json:"country"`
	Code    string    `json:"code"`
	Flag    string    `json:"flag"`
	Expires time.Time `json:"expires"`
}
type geoDisk struct {
	Attempts     []time.Time `json:"attempts"`
	BlockedUntil time.Time   `json:"blockedUntil"`
	Enabled      bool        `json:"enabled"`
}
type Geo struct {
	mu       sync.Mutex
	cache    map[string]Country
	pending  map[string]bool
	quota    geoDisk
	path     string
	client   *http.Client
	endpoint string
	healthy  bool
	enabled  bool
	ctx      context.Context
	cancel   context.CancelFunc
}

func NewGeo(dir string) *Geo {
	ctx, cancel := context.WithCancel(context.Background())
	g := &Geo{cache: map[string]Country{}, pending: map[string]bool{}, path: filepath.Join(dir, "geo-quota.json"), client: &http.Client{Timeout: 5 * time.Second, CheckRedirect: func(*http.Request, []*http.Request) error { return http.ErrUseLastResponse }}, endpoint: "https://ipwho.is/", healthy: true, ctx: ctx, cancel: cancel}
	b, err := os.ReadFile(g.path)
	if err == nil {
		if json.Unmarshal(b, &g.quota) != nil {
			g.healthy = false
		} else {
			g.enabled = g.quota.Enabled
		}
	} else if !os.IsNotExist(err) {
		g.healthy = false
	}
	return g
}
func (g *Geo) Close() {
	g.cancel()
	g.client.CloseIdleConnections()
	g.mu.Lock()
	g.enabled = false
	g.pending = map[string]bool{}
	for i := range g.cache {
		delete(g.cache, i)
	}
	g.mu.Unlock()
}
func (g *Geo) SetEnabled(enabled bool) error {
	g.mu.Lock()
	defer g.mu.Unlock()
	if g.enabled == enabled {
		return nil
	}
	previous := g.quota.Enabled
	g.quota.Enabled = enabled
	if err := g.persist(); err != nil {
		g.quota.Enabled = previous
		g.healthy = false
		return err
	}
	g.enabled = enabled
	return nil
}
func (g *Geo) Enabled() bool { g.mu.Lock(); defer g.mu.Unlock(); return g.enabled }
func publicIP(ip netip.Addr) bool {
	ip = ip.Unmap()
	if !ip.IsGlobalUnicast() || ip.IsPrivate() || ip.IsLoopback() {
		return false
	}
	for _, s := range []string{"100.64.0.0/10", "169.254.0.0/16", "192.0.0.0/24", "192.0.2.0/24", "198.18.0.0/15", "198.51.100.0/24", "203.0.113.0/24", "240.0.0.0/4", "2001:db8::/32", "fc00::/7", "fe80::/10"} {
		if netip.MustParsePrefix(s).Contains(ip) {
			return false
		}
	}
	return true
}
func (g *Geo) persist() error {
	b, err := json.Marshal(g.quota)
	if err != nil {
		return err
	}
	f, err := os.CreateTemp(filepath.Dir(g.path), ".geo-*")
	if err != nil {
		return err
	}
	defer os.Remove(f.Name())
	if _, err = f.Write(b); err != nil {
		f.Close()
		return err
	}
	if err = f.Sync(); err != nil {
		f.Close()
		return err
	}
	if err := f.Close(); err != nil {
		return err
	}
	if err := os.Rename(f.Name(), g.path); err != nil {
		return err
	}
	dir, err := os.Open(filepath.Dir(g.path))
	if err != nil {
		return err
	}
	if err := dir.Sync(); err != nil {
		_ = dir.Close()
		return err
	}
	return dir.Close()
}

// Countries only queues the first five traffic-ranked, public client IPs. No lookups
// run on the proxy path; concurrent dashboards share pending work and cache.
func (g *Geo) Countries(clients []ClientActivity) map[string]Country {
	g.mu.Lock()
	defer g.mu.Unlock()
	now := time.Now()
	out := map[string]Country{}
	if !g.enabled {
		return out
	}
	for ip, c := range g.cache {
		if now.After(c.Expires) {
			delete(g.cache, ip)
		}
	}
	for i, c := range clients {
		ip, err := netip.ParseAddr(c.IP)
		if err != nil || !publicIP(ip) {
			continue
		}
		key := ip.Unmap().String()
		if cached, ok := g.cache[key]; ok {
			out[c.IP] = cached
			continue
		}
		if i >= 5 || g.pending[key] || len(g.pending) >= 5 || !g.healthy || now.Before(g.quota.BlockedUntil) {
			continue
		}
		kept := g.quota.Attempts[:0]
		minute := 0
		for _, t := range g.quota.Attempts {
			if now.Sub(t) < 24*time.Hour {
				kept = append(kept, t)
				if now.Sub(t) < time.Minute {
					minute++
				}
			}
		}
		g.quota.Attempts = kept
		if len(kept) >= 900 || minute >= 5 {
			continue
		}
		g.quota.Attempts = append(g.quota.Attempts, now)
		// Reserve quota durably before sending, including failed requests.
		if g.persist() != nil {
			g.healthy = false
			continue
		}
		g.pending[key] = true
		go g.lookup(g.ctx, key)
	}
	return out
}
func (g *Geo) lookup(ctx context.Context, ip string) {
	c := Country{Country: "Unavailable", Expires: time.Now().Add(time.Hour)}
	defer func() {
		g.mu.Lock()
		defer g.mu.Unlock()
		delete(g.pending, ip)
		if len(g.cache) >= 256 {
			var oldest string
			var expiry time.Time
			for key, v := range g.cache {
				if oldest == "" || v.Expires.Before(expiry) {
					oldest = key
					expiry = v.Expires
				}
			}
			delete(g.cache, oldest)
		}
		g.cache[ip] = c
	}()
	req, err := http.NewRequestWithContext(ctx, http.MethodGet, g.endpoint+ip+"?fields=success,country,country_code", nil)
	if err != nil {
		return
	}
	resp, err := g.client.Do(req)
	if err != nil {
		return
	}
	defer resp.Body.Close()
	if resp.StatusCode == 429 {
		delay := 24 * time.Hour
		if seconds, err := strconv.Atoi(resp.Header.Get("Retry-After")); err == nil && seconds > 0 {
			delay = time.Duration(seconds) * time.Second
		} else if date, err := http.ParseTime(resp.Header.Get("Retry-After")); err == nil && date.After(time.Now()) {
			delay = time.Until(date)
		}
		g.mu.Lock()
		g.quota.BlockedUntil = time.Now().Add(delay)
		if g.persist() != nil {
			g.healthy = false
		}
		g.mu.Unlock()
		return
	}
	if resp.StatusCode != 200 {
		return
	}
	var body struct {
		Success bool   `json:"success"`
		Country string `json:"country"`
		Code    string `json:"country_code"`
	}
	if json.NewDecoder(io.LimitReader(resp.Body, 16384)).Decode(&body) != nil || !body.Success || len(body.Code) != 2 {
		return
	}
	for _, r := range body.Code {
		if r < 'A' || r > 'Z' {
			return
		}
	}
	c = Country{Country: body.Country, Code: body.Code, Flag: string([]rune{0x1f1e6 + rune(body.Code[0]-'A'), 0x1f1e6 + rune(body.Code[1]-'A')}), Expires: time.Now().Add(24 * time.Hour)}
}
