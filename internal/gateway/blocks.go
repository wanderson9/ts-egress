package gateway

import (
	"encoding/json"
	"errors"
	"net/netip"
	"os"
	"path/filepath"
	"sort"
	"sync"
)

// Blocks applies to public proxy requests only, never management or ACME.
type Blocks struct {
	mu   sync.RWMutex
	path string
	ips  map[netip.Addr]bool
}

func OpenBlocks(path string) (*Blocks, error) {
	b := &Blocks{path: path, ips: map[netip.Addr]bool{}}
	data, err := os.ReadFile(path)
	if errors.Is(err, os.ErrNotExist) {
		return b, nil
	}
	if err != nil {
		return nil, err
	}
	var ips []string
	if err := json.Unmarshal(data, &ips); err != nil {
		return nil, err
	}
	for _, value := range ips {
		ip, err := netip.ParseAddr(value)
		if err != nil || ip.Zone() != "" {
			return nil, errors.New("invalid blocked IP")
		}
		b.ips[ip.Unmap()] = true
	}
	return b, nil
}
func (b *Blocks) Contains(ip netip.Addr) bool {
	if b == nil {
		return false
	}
	b.mu.RLock()
	defer b.mu.RUnlock()
	return b.ips[ip.Unmap()]
}
func (b *Blocks) List() []string {
	b.mu.RLock()
	defer b.mu.RUnlock()
	return blockList(b.ips)
}
func blockList(ips map[netip.Addr]bool) []string {
	out := make([]string, 0, len(ips))
	for ip := range ips {
		out = append(out, ip.String())
	}
	sort.Strings(out)
	return out
}
func (b *Blocks) Set(value string, blocked bool) error {
	ip, err := netip.ParseAddr(value)
	if err != nil || ip.Zone() != "" {
		return errors.New("invalid client IP")
	}
	ip = ip.Unmap()
	b.mu.Lock()
	defer b.mu.Unlock()
	if b.ips[ip] == blocked {
		return nil
	}
	if blocked && len(b.ips) >= 10000 {
		return errors.New("maximum 10000 blocked IPs")
	}
	next := make(map[netip.Addr]bool, len(b.ips)+1)
	for k, v := range b.ips {
		next[k] = v
	}
	if blocked {
		next[ip] = true
	} else {
		delete(next, ip)
	}
	data, err := json.Marshal(blockList(next))
	if err != nil {
		return err
	}
	f, err := os.CreateTemp(filepath.Dir(b.path), ".blocks-*")
	if err != nil {
		return err
	}
	defer os.Remove(f.Name())
	if _, err = f.Write(data); err != nil {
		f.Close()
		return err
	}
	if err = f.Sync(); err != nil {
		f.Close()
		return err
	}
	if err = f.Close(); err != nil {
		return err
	}
	if err = os.Rename(f.Name(), b.path); err != nil {
		return err
	}
	b.ips = next
	dir, err := os.Open(filepath.Dir(b.path))
	if err != nil {
		return err
	}
	defer dir.Close()
	if err := dir.Sync(); err != nil {
		return err
	}
	return nil
}
