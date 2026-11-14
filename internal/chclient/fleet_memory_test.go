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
