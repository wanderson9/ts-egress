package gateway

import (
	"encoding/json"
	"errors"
	"fmt"
	"net"
	"net/netip"
	"net/url"
	"os"
	"path/filepath"
	"regexp"
	"strconv"
	"strings"
)

type Route struct {
	Domain   string `json:"domain"`
	Upstream string `json:"upstream"`
	Enabled  bool   `json:"enabled"`
}

var label = regexp.MustCompile(`^[a-z0-9](?:[a-z0-9-]{0,61}[a-z0-9])?$`)

func ValidDomain(s string) bool {
	if len(s) > 253 || !strings.Contains(s, ".") || net.ParseIP(s) != nil {
		return false
	}
	for _, part := range strings.Split(s, ".") {
		if !label.MatchString(part) {
			return false
		}
	}
	return true
}

func Validate(routes []Route) error {
	if len(routes) > 1000 {
		return errors.New("maximum 1000 routes")
	}
	seen := map[string]bool{}
	for _, r := range routes {
		if !ValidDomain(r.Domain) {
			return fmt.Errorf("invalid public domain %q (use lowercase DNS names)", r.Domain)
		}
		if seen[r.Domain] {
			return fmt.Errorf("duplicate domain %q", r.Domain)
		}
		seen[r.Domain] = true
		u, err := url.Parse(r.Upstream)
		if err != nil || u == nil {
			return errors.New("invalid upstream URL")
		}
		if (u.Scheme != "http" && u.Scheme != "https") || u.User != nil || u.RawQuery != "" || u.Fragment != "" || u.Opaque != "" || (u.Path != "" && u.Path != "/") {
			return errors.New("upstream must be an http(s) origin without credentials, path, query or fragment")
		}
		host := u.Hostname()
		if host == "" {
			return errors.New("upstream hostname required")
		}
		if p := u.Port(); p != "" {
			n, err := strconv.Atoi(p)
			if err != nil || n < 1 || n > 65535 {
				return errors.New("invalid upstream port")
			}
		}
		if ip, err := netip.ParseAddr(host); err == nil {
			if !tailIP(ip) {
				return errors.New("upstream IP must be a Tailscale address")
			}
		} else {
			for _, part := range strings.Split(host, ".") {
				if !label.MatchString(part) {
					return errors.New("invalid upstream DNS name")
				}
			}
		}
	}
	return nil
}

func tailIP(ip netip.Addr) bool {
	return netip.MustParsePrefix("100.64.0.0/10").Contains(ip) || netip.MustParsePrefix("fd7a:115c:a1e0::/48").Contains(ip)
}

func Load(path string) ([]Route, error) {
	b, err := os.ReadFile(path)
	if errors.Is(err, os.ErrNotExist) {
		return []Route{}, nil
	}
	if err != nil {
		return nil, err
	}
	var routes []Route
	if err := json.Unmarshal(b, &routes); err != nil {
		return nil, err
	}
	return routes, Validate(routes)
}

// Save replaces the configuration atomically; a failed write never changes live routing.
func Save(path string, routes []Route) error {
	if err := Validate(routes); err != nil {
		return err
	}
	b, err := json.MarshalIndent(routes, "", "  ")
	if err != nil {
		return err
	}
	f, err := os.CreateTemp(filepath.Dir(path), ".routes-*")
	if err != nil {
		return err
	}
	defer os.Remove(f.Name())
	if _, err := f.Write(b); err != nil {
		f.Close()
		return err
	}
	if err := f.Sync(); err != nil {
		f.Close()
		return err
	}
	if err := f.Close(); err != nil {
		return err
	}
	if err := os.Rename(f.Name(), path); err != nil {
		return err
	}
	dir, err := os.Open(filepath.Dir(path))
	if err != nil {
		return err
	}
	if err := dir.Sync(); err != nil {
		_ = dir.Close()
		return err
	}
	return dir.Close()
}
