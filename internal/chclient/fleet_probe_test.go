package chclient

import (
	"context"
	"maps"
	"testing"
	"time"

	"github.com/tsouza/cerberus/internal/chopt"
)

// canaryProbeDialTimeout bounds the identity read against an address nothing
// listens on.
const canaryProbeDialTimeout = 100 * time.Millisecond

// TestProbeCanaries_NoVerdictWithoutAnIdentityRead: the first canary's
// verdict belongs to the server the caller's read named. When the identity
// read before the next canary fails, that canary does not run, and the pass
// is reported as not fully read, so no verdict is attributed to a server no
// read named.
func TestProbeCanaries_NoVerdictWithoutAnIdentityRead(t *testing.T) {
	t.Parallel()
	c, err := New(Config{Addr: "127.0.0.1:1", DialTimeout: canaryProbeDialTimeout, BreakerDisabled: true})
	if err != nil {
		t.Fatalf("New: %v", err)
	}
	t.Cleanup(func() { _ = c.Close() })

	ran := map[string]int{}
	canary := func(name string, v chopt.Capability) FleetCanary {
		return FleetCanary{Name: name, Probe: func(context.Context, *Client) chopt.Capability {
			ran[name]++
			return v
		}}
	}
	first := FleetNode{ID: replicaA, Version: memoryNewBuild}
	nodes, read := probeCanaries(context.Background(), c, first,
		[]FleetCanary{canary("refused", chopt.CapabilityForbidden), canary("next", chopt.CapabilityAvailable)})
	if read {
		t.Errorf("probeCanaries after a failed identity read: read = true; want false")
	}
	if len(nodes) != 1 || nodes[0].ID != replicaA || !maps.Equal(nodes[0].Verdicts, map[string]chopt.Capability{"refused": chopt.CapabilityForbidden}) {
		t.Errorf("probeCanaries nodes = %+v; want only %s with the first canary's verdict", nodes, replicaA)
	}
	if ran["next"] != 0 || ran["refused"] != 1 {
		t.Errorf("canary runs = %v; want the first once and the next never", ran)
	}

	if nodes, read := probeCanaries(context.Background(), c, first, nil); !read || len(nodes) != 1 || nodes[0].Verdicts != nil {
		t.Errorf("probeCanaries with no canaries = %+v, %v; want the first node alone, unchanged, read", nodes, read)
	}
}
