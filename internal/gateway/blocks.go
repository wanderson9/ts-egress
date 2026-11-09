package gateway

import (
	"encoding/json"
	"errors"
	"net/netip"
	"os"
	"path/filepath"
	"slices"
	"sort"
	"strings"
	"sync"
)

// The broadest accepted ranges, which keep a typo from blocking most of the internet.
const (
	minBlockBitsIPv4 = 8
	minBlockBitsIPv6 = 32
)

// Blocks applies to public proxy requests only, never management or ACME.
// Entries are single addresses or CIDR ranges, stored as prefixes.
type Blocks struct {
	mu      sync.RWMutex
	path    string
	entries map[netip.Prefix]bool
	lengths []int
}

func parseBlock(value string) (netip.Prefix, error) {
	if strings.Contains(value, "/") {
		prefix, err := netip.ParsePrefix(value)
		if err != nil || prefix.Addr().Is4In6() {
			return netip.Prefix{}, errors.New("invalid client IP or range")
		}
		if (prefix.Addr().Is4() && prefix.Bits() < minBlockBitsIPv4) || (prefix.Addr().Is6() && prefix.Bits() < minBlockBitsIPv6) {
			return netip.Prefix{}, errors.New("block range is too broad; use at least /8 for IPv4 or /32 for IPv6")
		}
		return prefix.Masked(), nil
	}
	ip, err := netip.ParseAddr(value)
	if err != nil || ip.Zone() != "" {
		return netip.Prefix{}, errors.New("invalid client IP or range")
	}
	ip = ip.Unmap()
	return netip.PrefixFrom(ip, ip.BitLen()), nil
}

func prefixLengths(entries map[netip.Prefix]bool) []int {
	lengths := []int{}
	for prefix := range entries {
		if !slices.Contains(lengths, prefix.Bits()) {
			lengths = append(lengths, prefix.Bits())
		}
	}
	return lengths
}

func OpenBlocks(path string) (*Blocks, error) {
	b := &Blocks{path: path, entries: map[netip.Prefix]bool{}}
	data, err := os.ReadFile(path)
	if errors.Is(err, os.ErrNotExist) {
		return b, nil
	}
	if err != nil {
		return nil, err
	}
	var values []string
	if err := json.Unmarshal(data, &values); err != nil {
		return nil, err
	}
	for _, value := range values {
		prefix, err := parseBlock(value)
		if err != nil {
			return nil, errors.New("invalid blocked IP")
		}
		b.entries[prefix] = true
	}
	b.lengths = prefixLengths(b.entries)
	return b, nil
}
func (b *Blocks) Contains(ip netip.Addr) bool {
	if b == nil {
		return false
	}
	ip = ip.Unmap().WithZone("")
	b.mu.RLock()
	defer b.mu.RUnlock()
	for _, bits := range b.lengths {
		if prefix, err := ip.Prefix(bits); err == nil && b.entries[prefix] {
			return true
		}
	}
	return false
}
func (b *Blocks) List() []string {
	b.mu.RLock()
	defer b.mu.RUnlock()
	return blockList(b.entries)
}

// blockList writes single addresses without a suffix, the form the UI matches
// against client IPs and the form earlier releases stored.
func blockList(entries map[netip.Prefix]bool) []string {
	out := make([]string, 0, len(entries))
	for prefix := range entries {
		if prefix.IsSingleIP() {
			out = append(out, prefix.Addr().String())
		} else {
			out = append(out, prefix.String())
		}
	}
	sort.Strings(out)
	return out
}
func (b *Blocks) Set(value string, blocked bool) error {
	prefix, err := parseBlock(value)
	if err != nil {
		return err
	}
	b.mu.Lock()
	defer b.mu.Unlock()
	if b.entries[prefix] == blocked {
		return nil
	}
	if blocked && len(b.entries) >= 10000 {
		return errors.New("maximum 10000 blocked IPs")
	}
	next := make(map[netip.Prefix]bool, len(b.entries)+1)
	for k, v := range b.entries {
		next[k] = v
	}
	if blocked {
		next[prefix] = true
	} else {
		delete(next, prefix)
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
	b.entries = next
	b.lengths = prefixLengths(next)
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
