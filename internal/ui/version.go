package ui

import (
	"context"
	"encoding/json"
	"io"
	"net/http"
	"strconv"
	"strings"
	"sync"
	"time"
)

const Version = "v0.2.0"
const Repository = "https://github.com/wanderson9/ts-egress"

var releases = struct {
	sync.Mutex
	checked  time.Time
	checking bool
	latest   string
}{}

// VersionInfo never waits on GitHub. Failed checks do not imply an update.
func VersionInfo(w http.ResponseWriter, r *http.Request) {
	releases.Lock()
	latest := releases.latest
	if !releases.checking && time.Since(releases.checked) > time.Hour {
		releases.checking = true
		go checkRelease()
	}
	releases.Unlock()
	w.Header().Set("Content-Type", "application/json")
	w.Header().Set("Cache-Control", "no-store")
	json.NewEncoder(w).Encode(map[string]any{"version": Version, "latest": latest, "updateAvailable": newerRelease(latest, Version), "releasesURL": Repository + "/releases"})
}

func checkRelease() {
	latest := ""
	defer func() {
		releases.Lock()
		defer releases.Unlock()
		releases.checked = time.Now()
		releases.checking = false
		releases.latest = latest
	}()
	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancel()
	req, err := http.NewRequestWithContext(ctx, "GET", "https://api.github.com/repos/wanderson9/ts-egress/releases/latest", nil)
	if err != nil {
		return
	}
	req.Header.Set("Accept", "application/vnd.github+json")
	req.Header.Set("User-Agent", "ts-egress/"+Version)
	client := &http.Client{Timeout: 5 * time.Second, CheckRedirect: func(*http.Request, []*http.Request) error { return http.ErrUseLastResponse }}
	resp, err := client.Do(req)
	if err != nil {
		return
	}
	defer resp.Body.Close()
	if resp.StatusCode != http.StatusOK {
		return
	}
	var release struct {
		Tag        string `json:"tag_name"`
		Draft      bool   `json:"draft"`
		Prerelease bool   `json:"prerelease"`
	}
	if json.NewDecoder(io.LimitReader(resp.Body, 65536)).Decode(&release) != nil || release.Draft || release.Prerelease {
		return
	}
	if _, ok := releaseNumbers(release.Tag); ok {
		latest = release.Tag
	}
}

func releaseNumbers(value string) ([3]uint64, bool) {
	var result [3]uint64
	parts := strings.Split(strings.TrimPrefix(value, "v"), ".")
	if len(parts) < 2 || len(parts) > 3 {
		return result, false
	}
	for i, p := range parts {
		n, err := strconv.ParseUint(p, 10, 64)
		if err != nil {
			return result, false
		}
		result[i] = n
	}
	return result, true
}
func newerRelease(latest, current string) bool {
	a, ok := releaseNumbers(latest)
	if !ok {
		return false
	}
	b, ok := releaseNumbers(current)
	if !ok {
		return false
	}
	for i := range a {
		if a[i] != b[i] {
			return a[i] > b[i]
		}
	}
	return false
}
