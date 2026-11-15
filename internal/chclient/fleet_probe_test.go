package chclient

import (
	"context"
	"maps"
	"slices"
	"testing"
	"time"

	"github.com/tsouza/cerberus/internal/chopt"
)

// canaryProbeDialTimeout bounds the identity read against an address nothing
// listens on.
const canaryProbeDialTimeout = 100 * time.Millisecond

// unreachableCanaryClient is a client whose every statement fails to dial.
func unreachableCanaryClient(t *testing.T) *Client {
	t.Helper()
	c, err := New(Config{Addr: "127.0.0.1:1", DialTimeout: canaryProbeDialTimeout, BreakerDisabled: true})
	if err != nil {
		t.Fatalf("New: %v", err)
	}
	t.Cleanup(func() { _ = c.Close() })
	return c
}

// scriptedCanary is a canary that answers with answer without contacting the
// server, counting its runs in ran.
func scriptedCanary(name string, answer canaryAnswer, ran map[string]int) FleetCanary {
	return FleetCanary{Name: name, probe: func(context.Context, *Client) canaryAnswer {
		ran[name]++
		return answer
	}}
}

// verdictsByServer collects the verdicts nodes carry, per server identity.
func verdictsByServer(nodes []FleetNode) map[string]map[string]chopt.Capability {
	out := map[string]map[string]chopt.Capability{}
	for _, n := range nodes {
		if out[n.ID] == nil {
			out[n.ID] = map[string]chopt.Capability{}
		}
		maps.Copy(out[n.ID], n.Verdicts)
	}
	return out
}

// TestProbeCanaries_AttributesEachVerdictToTheServerThatGaveIt: an accepted
// canary that names the server that accepted it is attributed to that
// server, not to the one the connection's earlier read named; a verdict that
// names no server — here an answer from a canary whose statement returns no
// identity — goes to the server the last read named, which after an
// identified answer is that answer's server.
func TestProbeCanaries_AttributesEachVerdictToTheServerThatGaveIt(t *testing.T) {
	t.Parallel()
	ran := map[string]int{}
	first := FleetNode{ID: replicaA, Version: memoryNewBuild}
	onB := FleetNode{ID: replicaB, Version: memoryNewBuild}
	nodes, read := probeCanaries(context.Background(), unreachableCanaryClient(t), first, []FleetCanary{
		scriptedCanary("accepted on B", canaryAnswer{verdict: chopt.CapabilityAvailable, node: onB, identified: true}, ran),
		scriptedCanary("unnamed", canaryAnswer{verdict: chopt.CapabilityAvailable}, ran),
	})
	want := map[string]map[string]chopt.Capability{
		replicaA: {},
		replicaB: {"accepted on B": chopt.CapabilityAvailable, "unnamed": chopt.CapabilityAvailable},
	}
	if got := verdictsByServer(nodes); !read || !maps.EqualFunc(got, want, maps.Equal) {
		t.Fatalf("probeCanaries = %v, read=%v; want %v, read", got, read, want)
	}
}

// TestProbeCanaries_NoVerdictWithoutAnIdentityRead: a refused canary closes
// the connection, so the identity is read again before the next canary. When
// that read fails, the next canary does not run and the pass is reported as
// not fully read, so no verdict is attributed to a server no read named.
func TestProbeCanaries_NoVerdictWithoutAnIdentityRead(t *testing.T) {
	t.Parallel()
	c := unreachableCanaryClient(t)
	ran := map[string]int{}
	first := FleetNode{ID: replicaA, Version: memoryNewBuild}
	nodes, read := probeCanaries(context.Background(), c, first, []FleetCanary{
		scriptedCanary("refused", canaryAnswer{verdict: chopt.CapabilityForbidden}, ran),
		scriptedCanary("next", canaryAnswer{verdict: chopt.CapabilityAvailable}, ran),
	})
	if read {
		t.Errorf("probeCanaries after a failed identity read: read = true; want false")
	}
	want := map[string]map[string]chopt.Capability{replicaA: {"refused": chopt.CapabilityForbidden}}
	if got := verdictsByServer(nodes); !maps.EqualFunc(got, want, maps.Equal) {
		t.Errorf("probeCanaries verdicts = %v; want %v", got, want)
	}
	if ran["next"] != 0 || ran["refused"] != 1 {
		t.Errorf("canary runs = %v; want the first once and the next never", ran)
	}

	if nodes, read := probeCanaries(context.Background(), c, first, nil); !read || len(nodes) != 1 || nodes[0].Verdicts != nil {
		t.Errorf("probeCanaries with no canaries = %+v, %v; want the first node alone, unchanged, read", nodes, read)
	}
}

// TestRotateCanaries_EveryCanaryRunsFirst: over as many consecutive passes as
// there are canaries, each canary leads the order once, and every rotation
// keeps every canary.
func TestRotateCanaries_EveryCanaryRunsFirst(t *testing.T) {
	t.Parallel()
	canaries := []FleetCanary{TSGridCanary(), ResultCacheCanary(), QueryLogUnionCanary(), QueryWorkloadCanary("w")}
	names := func(cs []FleetCanary) []string {
		out := make([]string, 0, len(cs))
		for _, c := range cs {
			out = append(out, c.Name)
		}
		return out
	}
	all := names(canaries)
	var leaders []string
	for pass := range len(canaries) {
		order := names(RotateCanaries(canaries, len(canaries)+pass))
		if !slices.Equal(slices.Sorted(slices.Values(order)), slices.Sorted(slices.Values(all))) {
			t.Fatalf("pass %d order %v; want every canary of %v", pass, order, all)
		}
		leaders = append(leaders, order[0])
	}
	if !slices.Equal(leaders, all) {
		t.Fatalf("leaders over %d passes = %v; want each canary once, %v", len(canaries), leaders, all)
	}
	if got := RotateCanaries(nil, 1); len(got) != 0 {
		t.Fatalf("RotateCanaries(nil) = %v; want none", got)
	}
}
