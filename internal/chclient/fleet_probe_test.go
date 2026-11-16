package chclient

import (
	"context"
	"maps"
	"slices"
	"testing"
	"time"

	"github.com/ClickHouse/clickhouse-go/v2"

	"github.com/tsouza/cerberus/internal/chopt"
)

// canaryProbeDialTimeout bounds the identity read against an address nothing
// listens on.
const canaryProbeDialTimeout = 100 * time.Millisecond

// unreachableCanaryClient is a client whose every statement fails to dial.
func unreachableCanaryClient(t *testing.T) *Client {
	t.Helper()
	return unreachableCanaryClientOver(t, clickhouse.Native)
}

// unreachableCanaryClientOver is unreachableCanaryClient over protocol.
func unreachableCanaryClientOver(t *testing.T, protocol clickhouse.Protocol) *Client {
	t.Helper()
	c, err := New(Config{Addr: "127.0.0.1:1", DialTimeout: canaryProbeDialTimeout, BreakerDisabled: true, Protocol: protocol})
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
	nodes, _, read := probeCanaries(context.Background(), unreachableCanaryClient(t), first, nil, []FleetCanary{
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
	nodes, _, read := probeCanaries(context.Background(), c, first, nil, []FleetCanary{
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

	if nodes, _, read := probeCanaries(context.Background(), c, first, nil, nil); !read || len(nodes) != 1 || nodes[0].Verdicts != nil {
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
	_, _, read := probeCanaries(context.Background(), unreachableCanaryClient(t), FleetNode{ID: replicaA, Version: memoryNewBuild}, nil, []FleetCanary{
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
	nodes, _, read := probeCanaries(context.Background(), unreachableCanaryClient(t), FleetNode{ID: replicaA, Version: memoryNewBuild}, order, []FleetCanary{
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

// TestProbeCanaries_HTTPLeavesUnnamedVerdictsUnattributed: over HTTP, an
// accepted canary that names its server is attributed to it, but a refusal
// and an answer that names no server are returned unattributed — each was its
// own request, which a balancer may have routed to a server no identity read
// named. No identity read follows the refusal (the client here cannot dial,
// so one would end the pass), and every canary runs.
func TestProbeCanaries_HTTPLeavesUnnamedVerdictsUnattributed(t *testing.T) {
	t.Parallel()
	ran := map[string]int{}
	onB := FleetNode{ID: replicaB, Version: memoryNewBuild}
	nodes, unattributed, read := probeCanaries(context.Background(), unreachableCanaryClientOver(t, clickhouse.HTTP), FleetNode{ID: replicaA, Version: memoryNewBuild}, nil, []FleetCanary{
		scriptedCanary("refused", canaryAnswer{verdict: chopt.CapabilityForbidden}, ran),
		scriptedCanary("accepted on B", canaryAnswer{verdict: chopt.CapabilityAvailable, node: onB, identified: true}, ran),
		scriptedCanary("unnamed", canaryAnswer{verdict: chopt.CapabilityAvailable}, ran),
	})
	if !read {
		t.Fatalf("probeCanaries over HTTP: read = false; want every canary run")
	}
	want := map[string]map[string]chopt.Capability{replicaA: {}, replicaB: {"accepted on B": chopt.CapabilityAvailable}}
	if got := verdictsByServer(nodes); !maps.EqualFunc(got, want, maps.Equal) {
		t.Errorf("attributed verdicts = %v; want %v", got, want)
	}
	wantUnattributed := []CanaryReading{{Canary: "refused", Verdict: chopt.CapabilityForbidden}, {Canary: "unnamed", Verdict: chopt.CapabilityAvailable}}
	if !slices.Equal(unattributed, wantUnattributed) {
		t.Errorf("unattributed = %v; want %v", unattributed, wantUnattributed)
	}
}

// TestProbeCanaries_PassOnlyVerdictIsUnattributed: a PassOnly verdict is
// returned unattributed, with its flag, over either protocol, so no server
// keeps it past the pass. Over the native protocol the server it ran on
// records an unreachable verdict for it, and the error closed the connection,
// so the identity is read again before the next canary.
func TestProbeCanaries_PassOnlyVerdictIsUnattributed(t *testing.T) {
	t.Parallel()
	for _, protocol := range []clickhouse.Protocol{clickhouse.Native, clickhouse.HTTP} {
		ran := map[string]int{}
		nodes, unattributed, read := probeCanaries(context.Background(), unreachableCanaryClientOver(t, protocol), FleetNode{ID: replicaA, Version: memoryNewBuild}, nil, []FleetCanary{
			scriptedCanary("unclassified", canaryAnswer{verdict: chopt.CapabilityForbidden, passOnly: true}, ran),
			scriptedCanary("next", canaryAnswer{verdict: chopt.CapabilityAvailable}, ran),
		})
		wantOnA := map[string]chopt.Capability{}
		if protocol == clickhouse.Native {
			wantOnA["unclassified"] = chopt.CapabilityUnreachable
		}
		if got := verdictsByServer(nodes); !maps.Equal(got[replicaA], wantOnA) {
			t.Errorf("over %s: verdicts on the server = %v; want %v", protocol, got[replicaA], wantOnA)
		}
		want := CanaryReading{Canary: "unclassified", Verdict: chopt.CapabilityForbidden, PassOnly: true}
		if len(unattributed) == 0 || unattributed[0] != want {
			t.Errorf("over %s: unattributed = %v; want %v first", protocol, unattributed, want)
		}
		if wantRead := protocol == clickhouse.HTTP; read != wantRead || (ran["next"] == 1) != wantRead {
			t.Errorf("over %s: read=%v, runs %v; want the next canary run only over http, where no identity read precedes it", protocol, read, ran)
		}
	}
}

// TestProbeCanaries_PassOnlyVerdictMovesTheCanaryOrderOn: over the native
// protocol, a canary that read a PassOnly verdict on a server counts as run
// there, so the next pass that lands on that server runs another canary
// first. Otherwise a canary failing with an unclassified error on every pass
// would always run first there and, closing the connection, keep a refusal of
// the next canary on that server from ever being read.
func TestProbeCanaries_PassOnlyVerdictMovesTheCanaryOrderOn(t *testing.T) {
	t.Parallel()
	ran := map[string]int{}
	unclassified := scriptedCanary("unclassified", canaryAnswer{verdict: chopt.CapabilityForbidden, passOnly: true}, ran)
	other := scriptedCanary("other", canaryAnswer{verdict: chopt.CapabilityAvailable}, ran)
	m := NewFleetMemory()
	nodes, unattributed, _ := probeCanaries(context.Background(), unreachableCanaryClient(t), FleetNode{ID: replicaA, Version: memoryNewBuild}, m.CanaryOrder, []FleetCanary{unclassified, other})
	m.Observe(FleetPass{Nodes: nodes, Unattributed: unattributed, Complete: true})
	if got := m.CanaryOrder(replicaA, []FleetCanary{unclassified, other}); got[0].Name != other.Name {
		t.Errorf("canary order on the server after a PassOnly verdict = %s first; want %s", got[0].Name, other.Name)
	}
}
