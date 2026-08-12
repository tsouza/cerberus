package regression

import (
	"strings"
	"testing"
)

// Tier-2's own published-port band, and the pins that keep it honest.
//
// Tier-1 has had TestMigrationTier1PortBand since it was written; Tier-2 had
// none, so its ports were governed only by a comment in the compose file. That
// was survivable while the ruler tier published two ports. It stopped being
// survivable when the incumbent leg (a second ruler, its own Alertmanager and
// its own dead-end receiver) added three more: a stack whose ports drift out of
// band does not fail loudly, it cross-wires onto a leftover container from
// another stack and reports parity against a stale dataset.
const (
	tier2PortBandLow  = 27400
	tier2PortBandHigh = 27500
)

// tier2ComposePath is the ruler tier's compose file, which merges on top of
// tier-1's rather than standing alone.
const tier2ComposePath = "../e2e/migration/tiers/tier2-ruler/docker-compose.ruler.yml"

// TestMigrationTier2PortBand pins every port the ruler tier publishes inside
// its own band, and disjoint from tier-1's — which matters more here than for
// any other pair in the tree, because the two files are always merged into ONE
// compose project. A collision between them is not two stacks fighting; it is
// one stack that cannot start.
func TestMigrationTier2PortBand(t *testing.T) {
	t.Parallel()

	tier2 := hostPorts(t, tier2ComposePath)
	if len(tier2) == 0 {
		t.Fatalf("%s publishes no host ports; the ruler substrate is unreachable from the test host", tier2ComposePath)
	}
	for port, svc := range tier2 {
		if port < tier2PortBandLow || port > tier2PortBandHigh {
			t.Fatalf("tier-2 service %s publishes host port %d, outside the declared %d-%d band. "+
				"The band is what keeps a tier-2 run from cross-wiring onto another stack's leftovers.",
				svc, port, tier2PortBandLow, tier2PortBandHigh)
		}
	}

	// Tier-1 is merged into the SAME project, so its ports are the ones a
	// collision would actually break.
	for port, svc := range hostPorts(t, tier1ComposePath) {
		if tier2Svc, clash := tier2[port]; clash {
			t.Fatalf("host port %d is published by BOTH tier-2 service %s and tier-1 service %s, and the two "+
				"files are merged into one compose project — the stack cannot start. Move the tier-2 mapping "+
				"to a free port inside the %d-%d band.",
				port, tier2Svc, svc, tier2PortBandLow, tier2PortBandHigh)
		}
	}

	for _, other := range otherComposeFiles {
		for port, svc := range hostPorts(t, other) {
			if tier2Svc, clash := tier2[port]; clash {
				t.Fatalf("host port %d is published by BOTH tier-2 service %s and %s service %s. "+
					"Move the tier-2 mapping to a free port inside the %d-%d band.",
					port, tier2Svc, other, svc, tier2PortBandLow, tier2PortBandHigh)
			}
		}
	}
}

// The incumbent leg's two services, and the reference-Prometheus tag the ruler
// one must carry.
const (
	tier2IncumbentRuler       = "incumbent-ruler"
	tier2IncumbentAlertmgr    = "incumbent-alertmanager"
	tier2IncumbentReceiver    = "incumbent-dead-end-receiver"
	tier2ShadowReceiver       = "dead-end-receiver"
	tier2ReferencePromService = "relay-prom"
)

// TestMigrationTier2IncumbentRulerTracksTheReferencePrometheus pins the
// incumbent ruler to the SAME reference-Prometheus image the rest of the tree
// runs.
//
// MIG-18 and MIG-19 diff cerberus against this container's engine, so it is the
// oracle both scenarios' verdicts rest on. An incumbent left behind on an older
// tag would make every divergence an argument about which Prometheus version
// was right rather than about cerberus — and it would do so silently, because a
// stale oracle still answers every query.
func TestMigrationTier2IncumbentRulerTracksTheReferencePrometheus(t *testing.T) {
	t.Parallel()

	cf := readCompose(t, tier2ComposePath)
	relay, ok := cf.Services[tier2ReferencePromService]
	if !ok {
		t.Fatalf("%s declares no %s service to take the reference-Prometheus tag from", tier2ComposePath, tier2ReferencePromService)
	}
	incumbent, ok := cf.Services[tier2IncumbentRuler]
	if !ok {
		t.Fatalf("%s declares no %s service; MIG-18/MIG-19 would have no incumbent leg to diff against",
			tier2ComposePath, tier2IncumbentRuler)
	}
	if incumbent.Image != relay.Image {
		t.Fatalf("the incumbent ruler runs %q but the reference Prometheus in the same file runs %q. "+
			"The incumbent is the oracle MIG-18/MIG-19 diff cerberus against, so it must be the same engine "+
			"version the rest of the tree calls the reference.",
			incumbent.Image, relay.Image)
	}
	if !hasFlag(incumbent.Command, "--web.enable-remote-write-receiver") {
		t.Fatalf("the incumbent ruler does not enable the remote-write receiver, so the harness cannot seed it " +
			"the SAME samples it writes to ClickHouse — and a diff over different inputs measures the seeder")
	}
}

// TestMigrationTier2IncumbentHasItsOwnReceiver pins the two dead-end receivers
// apart.
//
// One receiver serving both rulers would interleave their notification streams
// into a single capture list, and nothing in a webhook payload names which
// ruler emitted an edge — so MIG-18's diff would silently become a stream
// compared against itself, which is clean by construction and proves nothing.
func TestMigrationTier2IncumbentHasItsOwnReceiver(t *testing.T) {
	t.Parallel()

	cf := readCompose(t, tier2ComposePath)
	for _, name := range []string{tier2ShadowReceiver, tier2IncumbentReceiver, tier2IncumbentAlertmgr} {
		if _, ok := cf.Services[name]; !ok {
			t.Fatalf("%s declares no %s service; the incumbent leg is incomplete and MIG-18's diff would have "+
				"only one stream", tier2ComposePath, name)
		}
	}
	shadowPorts := cf.Services[tier2ShadowReceiver].Ports
	incumbentPorts := cf.Services[tier2IncumbentReceiver].Ports
	if len(shadowPorts) == 0 || len(incumbentPorts) == 0 {
		t.Fatalf("both dead-end receivers must publish a host port so each ruler's stream is separately readable")
	}
	if shadowPorts[0] == incumbentPorts[0] {
		t.Fatalf("both dead-end receivers publish %q — they are one endpoint, so the two rulers' streams would "+
			"be read from the same capture list", shadowPorts[0])
	}
}

// hasFlag reports whether a compose command list carries an exact flag.
func hasFlag(command []string, flag string) bool {
	for _, arg := range command {
		if arg == flag || strings.HasPrefix(arg, flag+"=") {
			return true
		}
	}
	return false
}
