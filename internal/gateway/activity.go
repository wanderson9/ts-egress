package gateway

import (
	"hash/fnv"
	"io"
	"net/netip"
	"sort"
	"sync"
	"time"
)

const activityBuckets = 288
const maxActivityRecords = 50000
const maxActivityClientsPerSite = 500

type Totals struct {
	Requests uint64 `json:"requests"`
	Received uint64 `json:"received"`
	Sent     uint64 `json:"sent"`
	Errors   uint64 `json:"errors"`
}

func (t *Totals) add(v Totals) {
	t.Requests += v.Requests
	t.Received += v.Received
	t.Sent += v.Sent
	t.Errors += v.Errors
}

type clientKey struct {
	domain string
	ip     netip.Addr
}
type bucket struct {
	epoch   int64
	sites   map[string]Totals
	clients map[clientKey]Totals
	last    map[clientKey]int64
}
type Activity struct {
	shards [64]activityShard
}
type activityShard struct {
	mu         sync.Mutex
	buckets    [activityBuckets]bucket
	records    int
	lastExpiry int64
}
type ClientActivity struct {
	IP string `json:"ip"`
	Totals
	LastSeen int64 `json:"lastSeen"`
}
type SiteActivity struct {
	Totals
	Clients []ClientActivity `json:"clients"`
	Limited bool             `json:"limited"`
}

func (a *Activity) record(domain string, ip netip.Addr, v Totals) {
	index := shardFor(domain)
	a.recordAtShard(&a.shards[index], domain, ip, v, time.Now())
}
func fnv32(s string) uint32 { h := fnv.New32a(); _, _ = h.Write([]byte(s)); return h.Sum32() }
func shardFor(domain string) uint32 {
	h := fnv32(domain)
	h ^= h >> 16
	h *= 0x7feb352d
	h ^= h >> 15
	return h % 64
}
func (a *Activity) recordAt(domain string, ip netip.Addr, v Totals, now time.Time) {
	// Deterministic helper shares the production sharding path.
	index := shardFor(domain)
	a.recordAtShard(&a.shards[index], domain, ip, v, now)
}
func (a *Activity) recordAtShard(s *activityShard, domain string, ip netip.Addr, v Totals, now time.Time) {
	s.mu.Lock()
	defer s.mu.Unlock()
	epoch := now.Unix() / 300
	if s.lastExpiry != epoch {
		s.lastExpiry = epoch
		for i := range s.buckets {
			b := &s.buckets[i]
			if b.sites != nil && b.epoch <= epoch-activityBuckets {
				s.records -= len(b.clients)
				*b = bucket{}
			}
		}
	}
	b := &s.buckets[epoch%activityBuckets]
	if b.sites == nil {
		*b = bucket{epoch: epoch, sites: map[string]Totals{}, clients: map[clientKey]Totals{}, last: map[clientKey]int64{}}
	}
	total := b.sites[domain]
	total.add(v)
	b.sites[domain] = total
	key := clientKey{domain, ip}
	c, exists := b.clients[key]
	shardLimit := maxActivityRecords / len(a.shards)
	if !ip.IsValid() || (!exists && s.records >= shardLimit) {
		key = clientKey{domain, netip.Addr{}}
		c, exists = b.clients[key]
		if !exists && s.records >= shardLimit+32 {
			return
		}
	}
	if !exists {
		s.records++
	}
	c.add(v)
	b.clients[key] = c
	b.last[key] = now.Unix()
}
func (a *Activity) Snapshot(domain string) SiteActivity {
	result := SiteActivity{Clients: []ClientActivity{}}
	var tracked Totals
	clients := map[netip.Addr]ClientActivity{}
	epoch := time.Now().Unix() / 300
	for i := range a.shards {
		s := &a.shards[i]
		s.mu.Lock()
		if s.lastExpiry != epoch {
			s.lastExpiry = epoch
			for j := range s.buckets {
				b := &s.buckets[j]
				if b.sites != nil && b.epoch <= epoch-activityBuckets {
					s.records -= len(b.clients)
					*b = bucket{}
				}
			}
		}
		for _, b := range s.buckets {
			result.Totals.add(b.sites[domain])
			for key, total := range b.clients {
				if key.domain != domain {
					continue
				}
				c := clients[key.ip]
				c.Totals.add(total)
				if b.last[key] > c.LastSeen {
					c.LastSeen = b.last[key]
				}
				if key.ip.IsValid() {
					c.IP = key.ip.String()
				} else {
					c.IP = "Other / untracked IPs"
					result.Limited = true
				}
				clients[key.ip] = c
			}
		}
		s.mu.Unlock()
	}
	for _, c := range clients {
		tracked.add(c.Totals)
		result.Clients = append(result.Clients, c)
	}
	if result.Requests > tracked.Requests || result.Received > tracked.Received || result.Sent > tracked.Sent || result.Errors > tracked.Errors {
		missing := Totals{Requests: result.Requests - tracked.Requests, Received: result.Received - tracked.Received, Sent: result.Sent - tracked.Sent, Errors: result.Errors - tracked.Errors}
		found := false
		for i := range result.Clients {
			if result.Clients[i].IP == "Other / untracked IPs" {
				result.Clients[i].Totals.add(missing)
				found = true
				break
			}
		}
		if !found {
			result.Clients = append(result.Clients, ClientActivity{IP: "Other / untracked IPs", Totals: missing})
		}
		result.Limited = true
	}
	sort.Slice(result.Clients, func(i, j int) bool {
		iTraffic := result.Clients[i].Received + result.Clients[i].Sent
		jTraffic := result.Clients[j].Received + result.Clients[j].Sent
		if iTraffic == jTraffic {
			return result.Clients[i].IP < result.Clients[j].IP
		}
		return iTraffic > jTraffic
	})
	if len(result.Clients) > maxActivityClientsPerSite {
		var omitted Totals
		for _, c := range result.Clients[maxActivityClientsPerSite-1:] {
			omitted.add(c.Totals)
		}
		result.Clients = result.Clients[:maxActivityClientsPerSite-1]
		result.Clients = append(result.Clients, ClientActivity{IP: "Other / limited", Totals: omitted})
		result.Limited = true
	}
	return result
}
func (a *Activity) Summaries() map[string]Totals {
	out := map[string]Totals{}
	epoch := time.Now().Unix() / 300
	for i := range a.shards {
		s := &a.shards[i]
		s.mu.Lock()
		if s.lastExpiry != epoch {
			s.lastExpiry = epoch
			for j := range s.buckets {
				b := &s.buckets[j]
				if b.sites != nil && b.epoch <= epoch-activityBuckets {
					s.records -= len(b.clients)
					*b = bucket{}
				}
			}
		}
		for _, b := range s.buckets {
			for domain, v := range b.sites {
				t := out[domain]
				t.add(v)
				out[domain] = t
			}
		}
		s.mu.Unlock()
	}
	return out
}

type measuredBody struct {
	io.ReadCloser
	count func(int)
}

func (b *measuredBody) Read(p []byte) (int, error) {
	n, err := b.ReadCloser.Read(p)
	if n > 0 {
		b.count(n)
	}
	return n, err
}

type measuredDuplex struct {
	*measuredBody
	writer io.Writer
	sent   func(int)
}

func (b *measuredDuplex) Write(p []byte) (int, error) {
	n, err := b.writer.Write(p)
	if n > 0 {
		b.sent(n)
	}
	return n, err
}
