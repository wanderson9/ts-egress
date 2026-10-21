package gateway

import (
	"context"
	"net/http"
	"time"
)

type BackendHealth struct {
	State   string    `json:"state"`
	Detail  string    `json:"detail,omitempty"`
	Checked time.Time `json:"checked,omitempty"`
}

// At the 1,000-route limit, 128 workers finish even a fully timed-out sweep
// in 40 seconds. Including the 30-second pause, checks stay below the
// 90-second freshness threshold. Probe work has a separate concurrency budget
// from public requests.
const backendProbeWorkers = 128

// MonitorBackends checks the configured origin, never redirects or application
// paths. It uses the proxy's verified TLS transport and tailnet-only dialer.
func (g *Gateway) MonitorBackends(ctx context.Context) {
	client := &http.Client{Transport: g.transport, Timeout: 5 * time.Second, CheckRedirect: func(*http.Request, []*http.Request) error { return http.ErrUseLastResponse }}
	for {
		g.checkBackends(ctx, client)
		timer := time.NewTimer(30 * time.Second)
		select {
		case <-ctx.Done():
			timer.Stop()
			return
		case <-timer.C:
		}
	}
}

func (g *Gateway) checkBackends(ctx context.Context, client *http.Client) {
	jobs := make(chan Route)
	done := make(chan struct{}, backendProbeWorkers)
	for range backendProbeWorkers {
		go func() {
			defer func() { done <- struct{}{} }()
			for route := range jobs {
				if ctx.Err() != nil {
					continue
				}
				probeCtx, cancel := context.WithTimeout(ctx, 5*time.Second)
				req, err := http.NewRequestWithContext(probeCtx, http.MethodHead, route.Upstream, nil)
				v := BackendHealth{State: "Offline"}
				if err == nil {
					req.Header.Set("User-Agent", "ts-egress-health/1")
					var resp *http.Response
					resp, err = client.Do(req)
					if err == nil {
						resp.Body.Close()
						v.Detail = resp.Status
						if resp.StatusCode < 500 {
							v.State = "Online"
						} else {
							v.State = "Error"
						}
					}
				}
				if err != nil {
					v.Detail = err.Error()
				}
				cancel()
				v.Checked = time.Now()
				if ctx.Err() == nil && g.Health != nil {
					g.Health.mu.Lock()
					if g.Health.upstreams[route.Domain] == route.Upstream {
						s := g.Health.services[route.Domain]
						s.Backend = v
						g.Health.services[route.Domain] = s
					}
					g.Health.mu.Unlock()
				}
			}
		}()
	}
	for _, route := range g.Routes() {
		if !route.Enabled {
			continue
		}
		select {
		case jobs <- route:
		case <-ctx.Done():
		}
		if ctx.Err() != nil {
			break
		}
	}
	close(jobs)
	for range backendProbeWorkers {
		<-done
	}
}
