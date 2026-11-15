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
	nodes, read := probeCanaries(context.Background(), unreachableCanaryClient(t), first, nil, []FleetCanary{
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
	nodes, read := probeCanaries(context.Background(), c, first, nil, []FleetCanary{
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

	if nodes, read := probeCanaries(context.Background(), c, first, nil, nil); !read || len(nodes) != 1 || nodes[0].Verdicts != nil {
		t.Errorf("probeCanaries with no canaries = %+v, %v; want the first node alone, unchanged, read", nodes, read)
	}
}

// TestProbeCanaries_FailedReadIsReadAgain: a canary that got no verdict (a
// transport failure) closes the connection like a refusal, so the identity is
// read again before the next canary; when that read fails, the next canary
// does not run.
func TestProbeCanaries_FailedReadIsReadAgain(t *testing.T) {
	t.Parallel()
	ran := map[string]int{}
	_, read := probeCanaries(context.Background(), unreachableCanaryClient(t), FleetNode{ID: replicaA, Version: memoryNewBuild}, nil, []FleetCanary{
		scriptedCanary("failed", canaryAnswer{verdict: chopt.CapabilityUnreachable}, ran),
		scriptedCanary("next", canaryAnswer{verdict: chopt.CapabilityAvailable}, ran),
	})
	if read || ran["next"] != 0 {
		t.Fatalf("after an unreachable canary and a failed identity read: read=%v, runs %v; want not read and the next canary unrun", read, ran)
	}
}

// TestProbeCanaries_OrdersForTheServerItRunsOn: before each canary, the order
// is asked for the canaries still pending on the server the next one runs on
// — the first server, then the one an accepted canary named — and the canary
// it puts first is the one that runs.
func TestProbeCanaries_OrdersForTheServerItRunsOn(t *testing.T) {
	t.Parallel()
	ran := map[string]int{}
	onB := FleetNode{ID: replicaB, Version: memoryNewBuild}
	var asked []string
	order := func(server string, pending []FleetCanary) []FleetCanary {
		asked = append(asked, server)
		return slices.Concat(pending[1:], pending[:1])
	}
	var runs []string
	record := func(name string, answer canaryAnswer) FleetCanary {
		c := scriptedCanary(name, answer, ran)
		probe := c.probe
		c.probe = func(ctx context.Context, cl *Client) canaryAnswer {
			runs = append(runs, name)
			return probe(ctx, cl)
		}
		return c
	}
	nodes, read := probeCanaries(context.Background(), unreachableCanaryClient(t), FleetNode{ID: replicaA, Version: memoryNewBuild}, order, []FleetCanary{
		record("x", canaryAnswer{verdict: chopt.CapabilityAvailable}),
		record("y", canaryAnswer{verdict: chopt.CapabilityAvailable, node: onB, identified: true}),
	})
	if !read || !slices.Equal(runs, []string{"y", "x"}) || !slices.Equal(asked, []string{replicaA, replicaB}) {
		t.Fatalf("runs %v, order asked for %v, read=%v; want [y x] asked for [%s %s]", runs, asked, read, replicaA, replicaB)
	}
	want := map[string]map[string]chopt.Capability{replicaA: {}, replicaB: {"y": chopt.CapabilityAvailable, "x": chopt.CapabilityAvailable}}
	if got := verdictsByServer(nodes); !maps.EqualFunc(got, want, maps.Equal) {
		t.Fatalf("verdicts %v; want %v", got, want)
	}
}
