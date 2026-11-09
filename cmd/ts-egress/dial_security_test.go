package main

import (
	"context"
	"encoding/json"
	"net"
	"net/http"
	"net/http/httptest"
	"net/netip"
	"testing"

	"tailscale.com/client/local"
	"tailscale.com/ipn/ipnstate"
	"tailscale.com/types/key"
)

type recordingDialNode struct {
	readyNode
	addresses []string
}

func (n *recordingDialNode) Dial(_ context.Context, network, address string) (net.Conn, error) {
	n.addresses = append(n.addresses, network+" "+address)
	a, b := net.Pipe()
	b.Close()
	return a, nil
}

// Exercise the production dialer, not the unrestricted dialer used by proxy tests.
func TestDialSecurityBoundary(t *testing.T) {
	status := &ipnstate.Status{Peer: map[key.NodePublic]*ipnstate.PeerStatus{
		{}: {DNSName: "backend.example.ts.net.", TailscaleIPs: []netip.Addr{netip.MustParseAddr("100.100.1.2")}},
	}}
	api := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		json.NewEncoder(w).Encode(status)
	}))
	defer api.Close()
	node := &recordingDialNode{readyNode: readyNode{client: &local.Client{Dial: func(ctx context.Context, network, _ string) (net.Conn, error) {
		return (&net.Dialer{}).DialContext(ctx, network, api.Listener.Addr().String())
	}}}}
	rt := &runtime{ts: node}
	for _, host := range []string{"backend", "BACKEND.EXAMPLE.TS.NET", "100.100.1.2"} {
		conn, err := rt.Dial(context.Background(), "tcp", net.JoinHostPort(host, "8080"))
		if err != nil {
			t.Fatalf("known peer %q rejected: %v", host, err)
		}
		conn.Close()
		if got := node.addresses[len(node.addresses)-1]; got != "tcp 100.100.1.2:8080" {
			t.Fatalf("dial was not pinned to peer IP: %s", got)
		}
	}
	before := len(node.addresses)
	for _, host := range []string{"127.0.0.1", "169.254.169.254", "10.0.0.1", "100.100.100.100", "100.100.1.3", "evil.example", "backend.evil.example", "::1"} {
		if conn, err := rt.Dial(context.Background(), "tcp", net.JoinHostPort(host, "80")); err == nil {
			conn.Close()
			t.Errorf("non-peer %q accepted", host)
		}
	}
	if len(node.addresses) != before {
		t.Fatal("rejected destination reached tsnet dialer")
	}
}
