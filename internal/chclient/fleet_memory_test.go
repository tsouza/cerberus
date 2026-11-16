package chclient

import (
	"slices"
	"testing"

	"github.com/tsouza/cerberus/internal/chopt"
)

// Builds either side of a rolling rollback, and the servers that run them
// behind one load-balanced address.
var (
	memoryNewBuild = chopt.Version{Major: 26, Minor: 8, Patch: 10, Build: 6}
	memoryOldBuild = chopt.Version{Major: 25, Minor: 10, Patch: 7, Build: 6}
)

const (
	replicaA = "8f1c2b5e-0000-4000-8000-00000000000a"
	replicaB = "8f1c2b5e-0000-4000-8000-00000000000b"
)

// landed is a complete pass through one load-balanced address that landed on
// server id running v.
func landed(id string, v chopt.Version) FleetPass {
	return FleetPass{Nodes: []FleetNode{{ID: id, Version: v}}, Complete: true}
}

// TestFleetMemory_LoadBalancedPassesHoldTheOlderReplica: passes that alternate
// between a replica on the newer build and one rolled back to the older build
// describe a fleet holding both, so its lowest build stays the older one on
// every pass after the older replica was first reached, and Held marks every
// such pass: each one rests on the build of the replica it did not reach.
func TestFleetMemory_LoadBalancedPassesHoldTheOlderReplica(t *testing.T) {
	t.Parallel()
	m := NewFleetMemory()
	if fv := m.Observe(landed(replicaA, memoryNewBuild)); !slices.Equal(fv.Versions, []chopt.Version{memoryNewBuild}) || fv.Held || !fv.Complete {
		t.Fatalf("first pass = %+v; want the newer replica alone, complete, not held", fv)
	}
	for i := range 2 * twoServerRetention {
		pass := landed(replicaB, memoryOldBuild)
		if i%2 == 1 {
			pass = landed(replicaA, memoryNewBuild)
		}
		fv := m.Observe(pass)
		lowest, ok := fv.Lowest()
		if !ok || lowest != memoryOldBuild || !fv.Held || !fv.Complete {
			t.Fatalf("pass %d through the balancer = %+v; want lowest %s, held, complete", i+2, fv, memoryOldBuild)
		}
		if want := []chopt.Version{memoryOldBuild, memoryNewBuild}; !slices.Equal(fv.Versions, want) {
			t.Fatalf("pass %d fleet = %v; want %v", i+2, fv.Versions, want)
		}
	}
}

// TestFleetMemory_RollForwardRaisesOnTheReplicasOwnPass: the rolled-back
// replica reporting the newer build under its own identity replaces its entry
// on that pass, so the fleet rises with no wait.
func TestFleetMemory_RollForwardRaisesOnTheReplicasOwnPass(t *testing.T) {
	t.Parallel()
	m := NewFleetMemory()
	m.Observe(landed(replicaA, memoryNewBuild))
	m.Observe(landed(replicaB, memoryOldBuild))
	fv := m.Observe(landed(replicaB, memoryNewBuild))
	if lowest, _ := fv.Lowest(); lowest != memoryNewBuild || fv.Held {
		t.Fatalf("after replica B rolled forward: %+v; want lowest %s, not held", fv, memoryNewBuild)
	}
}

// twoServerRetention is how many consecutive complete misses a memory holding
// replicas A and B allows either of them.
var twoServerRetention = retentionPasses(2)

// TestFleetMemory_ForgetsAServerAfterTheRetentionPasses: a server no complete
// pass reaches stays for one miss short of the retention and is gone on the
// next one.
func TestFleetMemory_ForgetsAServerAfterTheRetentionPasses(t *testing.T) {
	t.Parallel()
	m := NewFleetMemory()
	m.Observe(landed(replicaB, memoryOldBuild))
	for i := range twoServerRetention - 1 {
		if lowest, _ := m.Observe(landed(replicaA, memoryNewBuild)).Lowest(); lowest != memoryOldBuild {
			t.Fatalf("miss %d: lowest %s; want the remembered %s until %d misses", i+1, lowest, memoryOldBuild, twoServerRetention)
		}
	}
	fv := m.Observe(landed(replicaA, memoryNewBuild))
	if !slices.Equal(fv.Versions, []chopt.Version{memoryNewBuild}) || fv.Held {
		t.Fatalf("after %d misses: %+v; want the older server forgotten", twoServerRetention, fv)
	}
}

// TestFleetMemory_RetentionScalesWithTheFleet: behind a balancer over three
// replicas each one is missed by more passes in a row than over two, so the
// memory allows a remembered server the three-server retention, not the
// two-server one.
func TestFleetMemory_RetentionScalesWithTheFleet(t *testing.T) {
	t.Parallel()
	const replicaC = "8f1c2b5e-0000-4000-8000-00000000000c"
	threeServerRetention := retentionPasses(3)
	if threeServerRetention <= twoServerRetention {
		t.Fatalf("premise: retention over three servers %d is not above two servers' %d", threeServerRetention, twoServerRetention)
	}
	m := NewFleetMemory()
	m.Observe(landed(replicaC, memoryNewBuild))
	m.Observe(landed(replicaB, memoryOldBuild))
	for i := range threeServerRetention - 1 {
		pass := landed(replicaA, memoryNewBuild)
		if i%2 == 1 {
			pass = landed(replicaC, memoryNewBuild)
		}
		if lowest, _ := m.Observe(pass).Lowest(); lowest != memoryOldBuild {
			t.Fatalf("miss %d of B: lowest %s; want the remembered %s until %d misses", i+1, lowest, memoryOldBuild, threeServerRetention)
		}
	}
	if fv := m.Observe(landed(replicaA, memoryNewBuild)); !slices.Equal(fv.Versions, []chopt.Version{memoryNewBuild, memoryNewBuild}) {
		t.Fatalf("after %d misses of B: %+v; want B forgotten and A and C kept", threeServerRetention, fv)
	}
}

// TestFleetMemory_IncompletePassesCountNoMiss: a pass that failed on an
// address may have missed the server behind it, so it neither counts a miss
// nor clears the misses already counted. Complete misses before and after any
// number of incomplete passes add up to the retention.
func TestFleetMemory_IncompletePassesCountNoMiss(t *testing.T) {
	t.Parallel()
	m := NewFleetMemory()
	m.Observe(landed(replicaB, memoryOldBuild))
	before := twoServerRetention / 2
	for range before {
		m.Observe(landed(replicaA, memoryNewBuild))
	}
	partial := FleetPass{Nodes: []FleetNode{{ID: replicaA, Version: memoryNewBuild}}}
	for range 2 * twoServerRetention {
		fv := m.Observe(partial)
		if lowest, _ := fv.Lowest(); lowest != memoryOldBuild || fv.Complete || !fv.Held {
			t.Fatalf("incomplete pass: %+v; want the remembered %s, incomplete, held", fv, memoryOldBuild)
		}
	}
	for i := range twoServerRetention - before - 1 {
		if lowest, _ := m.Observe(landed(replicaA, memoryNewBuild)).Lowest(); lowest != memoryOldBuild {
			t.Fatalf("complete miss %d: lowest %s; want %s remembered until %d complete misses", before+i+1, lowest, memoryOldBuild, twoServerRetention)
		}
	}
	if fv := m.Observe(landed(replicaA, memoryNewBuild)); fv.Held {
		t.Fatalf("after %d complete misses around the incomplete passes: %+v; want B forgotten", twoServerRetention, fv)
	}
}

// TestFleetMemory_NothingReachedDescribesNoFleet: a pass that reached no
// server reports no build, so resolution falls back to the supported floor,
// and leaves the memory intact for the next pass that answers.
func TestFleetMemory_NothingReachedDescribesNoFleet(t *testing.T) {
	t.Parallel()
	m := NewFleetMemory()
	m.Observe(landed(replicaB, memoryOldBuild))
	for _, pass := range []FleetPass{{}, {Complete: true}} {
		if fv := m.Observe(pass); len(fv.Versions) != 0 || fv.Held || fv.Complete != pass.Complete {
			t.Fatalf("pass %+v reaching nothing = %+v; want no build", pass, fv)
		}
	}
	if fv := m.Observe(landed(replicaA, memoryNewBuild)); !fv.Held {
		t.Fatalf("after passes reaching nothing: %+v; want the older server still held", fv)
	}
}

// TestFleetMemory_EnumeratedPassReplacesTheMemory: a complete pass that read
// the cluster's replica list is the whole fleet, so a server it does not list
// is forgotten at once. An enumerated pass that is incomplete is not.
func TestFleetMemory_EnumeratedPassReplacesTheMemory(t *testing.T) {
	t.Parallel()
	m := NewFleetMemory()
	m.Observe(landed(replicaB, memoryOldBuild))
	incomplete := FleetPass{Nodes: []FleetNode{{ID: replicaA, Version: memoryNewBuild}}, Enumerated: true}
	if lowest, _ := m.Observe(incomplete).Lowest(); lowest != memoryOldBuild {
		t.Fatalf("incomplete enumerated pass: lowest %s; want %s kept", lowest, memoryOldBuild)
	}
	enumerated := FleetPass{Nodes: []FleetNode{{ID: replicaA, Version: memoryNewBuild}}, Complete: true, Enumerated: true}
	if fv := m.Observe(enumerated); !slices.Equal(fv.Versions, []chopt.Version{memoryNewBuild}) || fv.Held {
		t.Fatalf("complete enumerated pass: %+v; want exactly the listed replica", fv)
	}
}

// TestFleetMemory_OneServerReadTwiceInAPassIsOneServer: the address arm and
// the cluster arm both read the server the address reached; it is one entry.
func TestFleetMemory_OneServerReadTwiceInAPassIsOneServer(t *testing.T) {
	t.Parallel()
	pass := FleetPass{Nodes: []FleetNode{
		{ID: replicaA, Version: memoryNewBuild},
		{ID: replicaA, Version: memoryNewBuild},
		{ID: replicaB, Version: memoryOldBuild},
	}, Complete: true, Enumerated: true}
	if fv := NewFleetMemory().Observe(pass); !slices.Equal(fv.Versions, []chopt.Version{memoryOldBuild, memoryNewBuild}) || fv.Held {
		t.Fatalf("pass reading replica A twice = %+v; want one entry per server", fv)
	}
}

// TestFleetMemory_ClonedIdentityKeepsTheOlderBuild: servers cloned from one
// data directory share a serverUUID(). A pass that reads that identity on two
// builds keeps the older one, in either order, so the clone on the older build
// still holds the fleet down.
func TestFleetMemory_ClonedIdentityKeepsTheOlderBuild(t *testing.T) {
	t.Parallel()
	for _, nodes := range [][]FleetNode{
		{{ID: replicaA, Version: memoryNewBuild}, {ID: replicaA, Version: memoryOldBuild}},
		{{ID: replicaA, Version: memoryOldBuild}, {ID: replicaA, Version: memoryNewBuild}},
	} {
		pass := FleetPass{Nodes: nodes, Complete: true, Enumerated: true}
		if fv := NewFleetMemory().Observe(pass); !slices.Equal(fv.Versions, []chopt.Version{memoryOldBuild}) {
			t.Fatalf("pass %+v = %+v; want the older build kept for the shared identity", nodes, fv)
		}
	}
}

// memoryCanary names the canary the verdict tests below run.
const memoryCanary = "ts_grid"

// landedWith is a complete pass through one load-balanced address that landed
// on server id running memoryNewBuild, whose canary gave verdict.
func landedWith(id string, verdict chopt.Capability) FleetPass {
	return FleetPass{Nodes: []FleetNode{{ID: id, Version: memoryNewBuild, Verdicts: map[string]chopt.Capability{memoryCanary: verdict}}}, Complete: true}
}

// TestFleetMemory_LoadBalancedCanaryHoldsTheRefusingReplica: passes that
// alternate between a replica whose canary permits and one whose canary
// refuses, on one build, describe a fleet whose verdict is the refusal on
// every pass after the refusing replica was first reached. Held marks
// exactly the passes that land on the permitting replica: the refusal they
// rest on comes from the replica they did not reach. When the refusing
// replica's own pass permits, the fleet permits.
func TestFleetMemory_LoadBalancedCanaryHoldsTheRefusingReplica(t *testing.T) {
	t.Parallel()
	m := NewFleetMemory()
	if fv := m.Observe(landedWith(replicaA, chopt.CapabilityAvailable)); fv.Capability(memoryCanary) != chopt.CapabilityAvailable || fv.Held {
		t.Fatalf("first pass = %+v; want the permitting replica's verdict, not held", fv)
	}
	for i := range 2 * twoServerRetention {
		onB := i%2 == 0
		pass := landedWith(replicaA, chopt.CapabilityAvailable)
		if onB {
			pass = landedWith(replicaB, chopt.CapabilityForbidden)
		}
		fv := m.Observe(pass)
		if got := fv.Capability(memoryCanary); got != chopt.CapabilityForbidden {
			t.Fatalf("pass %d (landed on B: %v): verdict %s; want %s while B is remembered", i+2, onB, got, chopt.CapabilityForbidden)
		}
		if fv.Held == onB {
			t.Fatalf("pass %d (landed on B: %v): held=%v; want held exactly when the pass missed B", i+2, onB, fv.Held)
		}
	}
	if fv := m.Observe(landedWith(replicaB, chopt.CapabilityAvailable)); fv.Capability(memoryCanary) != chopt.CapabilityAvailable || fv.Held {
		t.Fatalf("after B permitted: %+v; want the fleet to permit, not held", fv)
	}
}

// TestFleetMemory_CanaryNotRunKeepsTheRememberedVerdict: a pass that reads a
// server without running a canary on it — the cluster arm, or a pass whose
// identity read failed before that canary — keeps the server's remembered
// verdict, enumerated or not. The fleet is held on it, since a pass that runs
// no canary on B cannot see B's verdict change. A server no canary ever ran on
// adds no verdict.
func TestFleetMemory_CanaryNotRunKeepsTheRememberedVerdict(t *testing.T) {
	t.Parallel()
	m := NewFleetMemory()
	m.Observe(landedWith(replicaB, chopt.CapabilityForbidden))
	for _, pass := range []FleetPass{
		{Nodes: []FleetNode{{ID: replicaB, Version: memoryNewBuild}}, Complete: true},
		{Nodes: []FleetNode{{ID: replicaB, Version: memoryNewBuild}, {ID: replicaA, Version: memoryNewBuild}}, Complete: true, Enumerated: true},
	} {
		fv := m.Observe(pass)
		if got := fv.Capability(memoryCanary); got != chopt.CapabilityForbidden {
			t.Fatalf("pass %+v that ran no canary: verdict %s; want B's remembered %s", pass, got, chopt.CapabilityForbidden)
		}
		if !fv.Held {
			t.Fatalf("pass %+v ran no canary on B: not held; want held on B's remembered verdict", pass)
		}
	}
	if got := NewFleetMemory().Observe(landed(replicaA, memoryNewBuild)).Capability(memoryCanary); got != chopt.CapabilityUnreachable {
		t.Fatalf("fleet no canary ran on: verdict %s; want %s", got, chopt.CapabilityUnreachable)
	}
}

// TestFleetMemory_OneServerTwoVerdictsInAPassKeepsTheConservativeOne: two
// configured addresses in front of one balancer can read one server twice in
// a pass; its two verdicts on one canary keep the more conservative, in
// either order.
func TestFleetMemory_OneServerTwoVerdictsInAPassKeepsTheConservativeOne(t *testing.T) {
	t.Parallel()
	read := func(v chopt.Capability) FleetNode {
		return FleetNode{ID: replicaA, Version: memoryNewBuild, Verdicts: map[string]chopt.Capability{memoryCanary: v}}
	}
	for _, nodes := range [][]FleetNode{
		{read(chopt.CapabilityAvailable), read(chopt.CapabilityForbidden)},
		{read(chopt.CapabilityForbidden), read(chopt.CapabilityAvailable)},
	} {
		if got := NewFleetMemory().Observe(FleetPass{Nodes: nodes, Complete: true}).Capability(memoryCanary); got != chopt.CapabilityForbidden {
			t.Fatalf("pass %+v: verdict %s; want %s", nodes, got, chopt.CapabilityForbidden)
		}
	}
}

// TestFleetMemory_RefusingReplicaReachedIsNotHeld: a replica that refuses two
// canaries gives one refusal per pass, since the refusal closes the
// connection. A pass that lands on it still reaches the server holding both
// refusals in force, so the fleet is not held; a pass that lands on the
// permitting replica is.
func TestFleetMemory_RefusingReplicaReachedIsNotHeld(t *testing.T) {
	t.Parallel()
	const second = "result_cache"
	m := NewFleetMemory()
	refusesOne := func(canary string) FleetPass {
		return FleetPass{Nodes: []FleetNode{{ID: replicaB, Version: memoryNewBuild, Verdicts: map[string]chopt.Capability{canary: chopt.CapabilityForbidden}}}, Complete: true}
	}
	m.Observe(refusesOne(memoryCanary))
	m.Observe(refusesOne(second))
	for i := range 4 {
		onB := i%2 == 0
		pass := FleetPass{Nodes: []FleetNode{{ID: replicaA, Version: memoryNewBuild, Verdicts: map[string]chopt.Capability{
			memoryCanary: chopt.CapabilityAvailable, second: chopt.CapabilityAvailable,
		}}}, Complete: true}
		if onB {
			pass = refusesOne(memoryCanary)
		}
		fv := m.Observe(pass)
		if fv.Capability(memoryCanary) != chopt.CapabilityForbidden || fv.Capability(second) != chopt.CapabilityForbidden {
			t.Fatalf("pass %d: verdicts %v; want both refused", i+1, fv.Verdicts)
		}
		if fv.Held == onB {
			t.Fatalf("pass %d (landed on B: %v): held=%v; want held exactly when the pass missed B", i+1, onB, fv.Held)
		}
	}
}

// TestFleetMemory_NoVerdictKeepsTheRememberedOne: a canary that got no verdict
// from a server (unreachable) keeps the verdict remembered for it, refusal or
// permit, since the failed read is no evidence; a server with nothing
// remembered takes the unreachable verdict.
func TestFleetMemory_NoVerdictKeepsTheRememberedOne(t *testing.T) {
	t.Parallel()
	for _, was := range []chopt.Capability{chopt.CapabilityForbidden, chopt.CapabilityAvailable} {
		m := NewFleetMemory()
		m.Observe(landedWith(replicaB, was))
		if got := m.Observe(landedWith(replicaB, chopt.CapabilityUnreachable)).Capability(memoryCanary); got != was {
			t.Fatalf("remembered %s, then unreachable: verdict %s; want %s kept", was, got, was)
		}
	}
	if got := NewFleetMemory().Observe(landedWith(replicaB, chopt.CapabilityUnreachable)).Capability(memoryCanary); got != chopt.CapabilityUnreachable {
		t.Fatalf("first verdict unreachable: %s; want %s", got, chopt.CapabilityUnreachable)
	}
}

// TestFleetMemory_CanaryOrderFollowsTheServersOwnHistory: for each server, a
// canary no pass has run on it comes first, then the one run on it longest
// ago, keeping the given order among equals; another server's history does
// not move it.
func TestFleetMemory_CanaryOrderFollowsTheServersOwnHistory(t *testing.T) {
	t.Parallel()
	canaries := []FleetCanary{{Name: "a"}, {Name: "b"}, {Name: "c"}}
	names := func(cs []FleetCanary) []string {
		out := make([]string, 0, len(cs))
		for _, c := range cs {
			out = append(out, c.Name)
		}
		return out
	}
	ran := func(id string, canary ...string) FleetPass {
		verdicts := map[string]chopt.Capability{}
		for _, c := range canary {
			verdicts[c] = chopt.CapabilityForbidden
		}
		return FleetPass{Nodes: []FleetNode{{ID: id, Version: memoryNewBuild, Verdicts: verdicts}}, Complete: true}
	}
	m := NewFleetMemory()
	if got := names(m.CanaryOrder(replicaB, canaries)); !slices.Equal(got, []string{"a", "b", "c"}) {
		t.Fatalf("unknown server: %v; want the given order", got)
	}
	m.Observe(ran(replicaB, "a"))
	m.Observe(ran(replicaA, "b", "c"))
	if got := names(m.CanaryOrder(replicaB, canaries)); !slices.Equal(got, []string{"b", "c", "a"}) {
		t.Fatalf("after B ran a: %v; want [b c a]", got)
	}
	m.Observe(ran(replicaB, "c"))
	if got := names(m.CanaryOrder(replicaB, canaries)); !slices.Equal(got, []string{"b", "a", "c"}) {
		t.Fatalf("after B ran a then c: %v; want [b a c]", got)
	}
}

// TestFleetMemory_ARealReadingBeatsAnUnreachableOneInAPass: a server read
// twice in one pass, once without a verdict (unreachable) and once with one,
// takes the real verdict in either order, so a permit read after a remembered
// refusal is not lost to the failed reading.
func TestFleetMemory_ARealReadingBeatsAnUnreachableOneInAPass(t *testing.T) {
	t.Parallel()
	read := func(v chopt.Capability) FleetNode {
		return FleetNode{ID: replicaA, Version: memoryNewBuild, Verdicts: map[string]chopt.Capability{memoryCanary: v}}
	}
	for _, nodes := range [][]FleetNode{
		{read(chopt.CapabilityAvailable), read(chopt.CapabilityUnreachable)},
		{read(chopt.CapabilityUnreachable), read(chopt.CapabilityAvailable)},
	} {
		m := NewFleetMemory()
		m.Observe(FleetPass{Nodes: []FleetNode{read(chopt.CapabilityForbidden)}, Complete: true})
		if got := m.Observe(FleetPass{Nodes: nodes, Complete: true}).Capability(memoryCanary); got != chopt.CapabilityAvailable {
			t.Fatalf("remembered forbidden, then pass %+v: verdict %s; want %s", nodes, got, chopt.CapabilityAvailable)
		}
	}
}

// overHTTP is a complete pass through one HTTP address whose identity read
// landed on server id, and whose canary verdict, if any, no statement tied to
// a server.
func overHTTP(id string, verdicts ...chopt.Capability) FleetPass {
	pass := landed(id, memoryNewBuild)
	for _, v := range verdicts {
		pass.Unattributed = append(pass.Unattributed, CanaryReading{Canary: memoryCanary, Verdict: v})
	}
	return pass
}

// TestFleetMemory_UnattributedRefusalHoldsWhileItIsRead: behind an HTTP
// balancer, the refusing replica's verdict reaches the memory unattributed,
// whichever server the pass's identity read named, and a permitting
// replica's unattributed verdict does not displace it. The fleet refuses on
// every pass from the first refusal on, and Held marks exactly the passes
// that did not read the refusal.
func TestFleetMemory_UnattributedRefusalHoldsWhileItIsRead(t *testing.T) {
	t.Parallel()
	m := NewFleetMemory()
	if fv := m.Observe(overHTTP(replicaA, chopt.CapabilityAvailable)); fv.Capability(memoryCanary) != chopt.CapabilityAvailable || fv.Held {
		t.Fatalf("first pass = %+v; want the permitting verdict, not held", fv)
	}
	for i := range 2 * twoServerRetention {
		readRefusal := i%2 == 0
		// The identity read and the canary land on different replicas: the
		// refusal arrives on passes whose identity read named A.
		pass := overHTTP(replicaB, chopt.CapabilityAvailable)
		if readRefusal {
			pass = overHTTP(replicaA, chopt.CapabilityForbidden)
		}
		fv := m.Observe(pass)
		if got := fv.Capability(memoryCanary); got != chopt.CapabilityForbidden {
			t.Fatalf("pass %d (read the refusal: %v): verdict %s; want %s", i+2, readRefusal, got, chopt.CapabilityForbidden)
		}
		if fv.Held == readRefusal {
			t.Fatalf("pass %d (read the refusal: %v): held=%v; want held exactly when the pass did not read it", i+2, readRefusal, fv.Held)
		}
	}
}

// TestFleetMemory_UnattributedRefusalIsForgottenAfterTheRetentionPasses: an
// unattributed refusal no complete pass reads again counts toward the fleet's
// verdict for retentionPasses(servers held) - 1 passes and is forgotten on the
// next. Incomplete passes count no miss, and an unreachable reading neither
// holds a verdict nor refreshes one.
func TestFleetMemory_UnattributedRefusalIsForgottenAfterTheRetentionPasses(t *testing.T) {
	t.Parallel()
	m := NewFleetMemory()
	m.Observe(overHTTP(replicaA, chopt.CapabilityForbidden))
	m.Observe(landed(replicaB, memoryNewBuild))
	incomplete := overHTTP(replicaA, chopt.CapabilityAvailable)
	incomplete.Complete = false
	// The pass that brought B in counted the first miss.
	for i := range twoServerRetention - 2 {
		pass := overHTTP(replicaA, chopt.CapabilityAvailable, chopt.CapabilityUnreachable)
		if i == 0 {
			if fv := m.Observe(incomplete); fv.Capability(memoryCanary) != chopt.CapabilityForbidden {
				t.Fatalf("incomplete pass: verdict %s; want the held refusal", fv.Capability(memoryCanary))
			}
		}
		fv := m.Observe(pass)
		if got := fv.Capability(memoryCanary); got != chopt.CapabilityForbidden || !fv.Held {
			t.Fatalf("miss %d: verdict %s held=%v; want the held refusal until %d misses", i+2, got, fv.Held, twoServerRetention)
		}
	}
	if fv := m.Observe(overHTTP(replicaA, chopt.CapabilityAvailable)); fv.Capability(memoryCanary) != chopt.CapabilityAvailable || fv.Held {
		t.Fatalf("after %d misses: %+v; want the refusal forgotten and the fleet permitting, not held", twoServerRetention, fv)
	}
	if fv := m.Observe(overHTTP(replicaA, chopt.CapabilityUnreachable)); fv.Capability(memoryCanary) != chopt.CapabilityAvailable {
		t.Fatalf("an unreachable unattributed reading: verdict %s; want the held permit", fv.Capability(memoryCanary))
	}
}

// TestFleetMemory_UnattributedRefusalOutweighsAnAttributedPermit: a server's
// own permit and an unattributed refusal combine to the refusal, which the
// server's later permits do not clear — the refusal may come from another
// replica behind the same address.
func TestFleetMemory_UnattributedRefusalOutweighsAnAttributedPermit(t *testing.T) {
	t.Parallel()
	m := NewFleetMemory()
	pass := landedWith(replicaA, chopt.CapabilityAvailable)
	pass.Unattributed = []CanaryReading{{Canary: memoryCanary, Verdict: chopt.CapabilityForbidden}}
	m.Observe(pass)
	for i := range 3 {
		if fv := m.Observe(landedWith(replicaA, chopt.CapabilityAvailable)); fv.Capability(memoryCanary) != chopt.CapabilityForbidden || !fv.Held {
			t.Fatalf("permit %d from A: %+v; want the unattributed refusal held", i+1, fv)
		}
	}
}

// TestFleetMemory_PassOnlyVerdictCountsForItsPassOnly: an unattributed
// PassOnly refusal keeps the fleet's verdict at forbidden for the pass that
// read it, without marking the fleet held, and the next pass that does not
// read it reads the permit again.
func TestFleetMemory_PassOnlyVerdictCountsForItsPassOnly(t *testing.T) {
	t.Parallel()
	m := NewFleetMemory()
	m.Observe(overHTTP(replicaA, chopt.CapabilityAvailable))
	pass := landed(replicaA, memoryNewBuild)
	pass.Unattributed = []CanaryReading{{Canary: memoryCanary, Verdict: chopt.CapabilityForbidden, PassOnly: true}}
	if fv := m.Observe(pass); fv.Capability(memoryCanary) != chopt.CapabilityForbidden || fv.Held {
		t.Fatalf("pass that read the PassOnly refusal = %+v; want %s, not held", fv, chopt.CapabilityForbidden)
	}
	if fv := m.Observe(overHTTP(replicaA, chopt.CapabilityAvailable)); fv.Capability(memoryCanary) != chopt.CapabilityAvailable || fv.Held {
		t.Fatalf("next pass = %+v; want %s, not held", fv, chopt.CapabilityAvailable)
	}
}
