package rejectionparity

import (
	"context"
	"os"
	"strings"
	"testing"
	"time"

	tempotraceql "github.com/grafana/tempo/pkg/traceql"
	promparser "github.com/prometheus/prometheus/promql/parser"

	"github.com/tsouza/cerberus/internal/logql"
	"github.com/tsouza/cerberus/internal/promql"
	"github.com/tsouza/cerberus/internal/schema"
	"github.com/tsouza/cerberus/internal/traceql"
)

const (
	repoRoot      = "../.."
	cataloguePath = "catalogue.json"
)

// TestCatalogueIsRegenerable rescans the three lowering packages and
// diffs the regenerated catalogue byte-for-byte against the checked-in
// JSON. Set CERBERUS_UPDATE_INVENTORY=1 to rewrite the artifact (the
// same update-via-env convention as test/inventory). Regeneration
// preserves the curated fields of surviving sites; new sites land
// unclassified (and then fail TestCatalogueEntriesAreClassified until
// curated), removed sites drop out. Both directions are deliberate,
// reviewable diffs — a rejection site can neither appear nor vanish
// without the catalogue (and therefore the parity corpus) moving in
// lock-step.
func TestCatalogueIsRegenerable(t *testing.T) {
	t.Parallel()

	prev, err := LoadCatalogue(cataloguePath)
	if err != nil && !os.IsNotExist(err) {
		t.Fatalf("load %s: %v", cataloguePath, err)
	}
	cat, err := Generate(repoRoot, prev)
	if err != nil {
		t.Fatalf("Generate: %v", err)
	}
	want, err := MarshalCatalogue(cat)
	if err != nil {
		t.Fatalf("MarshalCatalogue: %v", err)
	}

	if os.Getenv("CERBERUS_UPDATE_INVENTORY") != "" {
		if err := os.WriteFile(cataloguePath, want, 0o644); err != nil {
			t.Fatalf("write %s: %v", cataloguePath, err)
		}
		t.Logf("rewrote %s (%d entries)", cataloguePath, len(cat.Entries))
		return
	}

	got, err := os.ReadFile(cataloguePath)
	if err != nil {
		t.Fatalf("read %s (rerun with CERBERUS_UPDATE_INVENTORY=1 to generate): %v", cataloguePath, err)
	}
	if string(got) != string(want) {
		t.Fatalf("%s is stale relative to the lowering sources — rerun with "+
			"CERBERUS_UPDATE_INVENTORY=1, curate any new entries, and commit the diff.\n"+
			"--- want %d bytes, got %d bytes", cataloguePath, len(want), len(got))
	}
}

// TestCatalogueEntriesAreClassified is the curation gate: every
// scanned site must be classified, rejection entries must carry a
// trigger query + valid endpoint, and internal entries must justify
// themselves with a rationale. An unclassified entry (the regen
// default for a new rejection site) fails here — a new deliberate
// rejection cannot land without a parity-corpus case.
func TestCatalogueEntriesAreClassified(t *testing.T) {
	t.Parallel()

	cat := loadCatalogue(t)
	seen := map[string]bool{}
	for _, e := range cat.Entries {
		if seen[e.Site.Site] {
			t.Errorf("entry %s is listed twice", e.Site.Site)
		}
		seen[e.Site.Site] = true
		switch e.Class {
		case "rejection":
			if strings.TrimSpace(e.TriggerQuery) == "" {
				t.Errorf("rejection entry %s has no trigger query", e.Site.Site)
			}
			ep := e.Endpoint
			if ep == "" {
				ep = DefaultEndpoint(e.Head)
			}
			if !ValidEndpoint(e.Head, ep) {
				t.Errorf("rejection entry %s: endpoint %q invalid for head %s", e.Site.Site, ep, e.Head)
			}
			if e.Rationale != "" {
				t.Errorf("rejection entry %s carries a rationale — rationales belong to internal entries; a rejection justifies itself via its trigger query", e.Site.Site)
			}
		case "internal":
			if strings.TrimSpace(e.Rationale) == "" {
				t.Errorf("internal entry %s carries no rationale — every internal classification must justify why the site is not wire-reachable", e.Site.Site)
			}
			if e.TriggerQuery != "" || e.Endpoint != "" {
				t.Errorf("internal entry %s carries trigger query / endpoint — those belong to rejection entries", e.Site.Site)
			}
		default:
			t.Errorf("entry %s is unclassified (class=%q) — classify as rejection (with trigger query) or internal (with rationale)", e.Site.Site, e.Class)
		}
	}
}

// TestRejectionTriggersExerciseSites is the centrepiece pin: for every
// class=rejection entry, the trigger query (a) parses with the head's
// reference parser — proving the rejection is semantic, not a parse
// error — and (b) fails the head's lowering with an error matching the
// site's message — proving the trigger actually exercises the
// catalogued site, so the parity corpus diffs the claim the site makes
// and not some other failure.
func TestRejectionTriggersExerciseSites(t *testing.T) {
	t.Parallel()

	cat := loadCatalogue(t)
	for _, e := range cat.Entries {
		if e.Class != "rejection" {
			continue
		}
		e := e
		t.Run(e.Site.Site, func(t *testing.T) {
			t.Parallel()
			errStr := lowerTrigger(t, e)
			if !ErrorMatchesMessage(errStr, e.Message) {
				t.Fatalf("trigger %q failed lowering with %q, which does not match the catalogued message %q — the trigger exercises a different site",
					e.TriggerQuery, errStr, e.Message)
			}
		})
	}
}

// TestParityCorpusMatchesCatalogue pins the third leg of the ratchet:
// the parity-corpus case set the compat driver runs is derived 1:1
// from the rejection entries — same count, same site keys — for every
// head. BuildCases is the same function the driver binary calls, so
// the corpus cannot drift from the catalogue.
func TestParityCorpusMatchesCatalogue(t *testing.T) {
	t.Parallel()

	cat := loadCatalogue(t)
	for _, head := range []string{"promql", "logql", "traceql"} {
		cases, err := BuildCases(cat, head)
		if err != nil {
			t.Fatalf("BuildCases(%s): %v", head, err)
		}
		var rejections []string
		for _, e := range cat.Entries {
			if e.Head == head && e.Class == "rejection" {
				rejections = append(rejections, e.Site.Site)
			}
		}
		if len(cases) != len(rejections) {
			t.Fatalf("head %s: %d parity cases for %d rejection entries", head, len(cases), len(rejections))
		}
		for i, c := range cases {
			if c.Name != rejections[i] {
				t.Fatalf("head %s: case %d is %s, want %s", head, i, c.Name, rejections[i])
			}
		}
		if len(cases) == 0 {
			t.Fatalf("head %s: zero parity cases — the catalogue lost its rejection entries", head)
		}
	}
}

// lowerTrigger parses + lowers one trigger query through the same
// entrypoints the HTTP handlers use and returns the lowering error
// string. Parse failures and lowering successes are both fatal — a
// trigger must be a valid-language query that the lowering rejects.
func lowerTrigger(t *testing.T, e Entry) string {
	t.Helper()
	ctx := context.Background()
	start := time.Date(2026, 1, 1, 0, 0, 0, 0, time.UTC)
	end := start.Add(5 * time.Minute)

	switch e.Head {
	case "promql":
		p := promparser.NewParser(promparser.Options{EnableExperimentalFunctions: true})
		expr, err := p.ParseExpr(e.TriggerQuery)
		if err != nil {
			t.Fatalf("trigger %q does not parse as PromQL: %v", e.TriggerQuery, err)
		}
		// Instant query shape: the prom Lang adapter always lowers via
		// LowerAtRange; /api/v1/query leaves Step at zero.
		_, lerr := promql.LowerAtRange(ctx, expr, schema.DefaultOTelMetrics(), end, end, 0)
		if lerr == nil {
			t.Fatalf("trigger %q lowered successfully — the catalogued rejection is unreachable via this query", e.TriggerQuery)
		}
		return lerr.Error()
	case "logql":
		// ParseExprPermissive is the parse entrypoint the wire path
		// uses (internal/logql/lang.go Parse); mirror it so the
		// exerciser proves wire-level reachability.
		expr, err := logql.ParseExprPermissive(e.TriggerQuery)
		if err != nil {
			t.Fatalf("trigger %q does not parse as LogQL: %v", e.TriggerQuery, err)
		}
		_, lerr := logql.LowerAtRange(ctx, expr, schema.DefaultOTelLogs(), start, end, 30*time.Second)
		if lerr == nil {
			t.Fatalf("trigger %q lowered successfully — the catalogued rejection is unreachable via this query", e.TriggerQuery)
		}
		return lerr.Error()
	case "traceql":
		expr, err := tempotraceql.Parse(e.TriggerQuery)
		if err != nil {
			t.Fatalf("trigger %q does not parse as TraceQL: %v", e.TriggerQuery, err)
		}
		_, lerr := traceql.Lower(ctx, expr, schema.DefaultOTelTraces())
		if lerr == nil {
			t.Fatalf("trigger %q lowered successfully — the catalogued rejection is unreachable via this query", e.TriggerQuery)
		}
		return lerr.Error()
	}
	t.Fatalf("entry %s has unknown head %q", e.Site.Site, e.Head)
	return ""
}

func loadCatalogue(t *testing.T) *Catalogue {
	t.Helper()
	cat, err := LoadCatalogue(cataloguePath)
	if err != nil {
		t.Fatalf("load %s (rerun TestCatalogueIsRegenerable with CERBERUS_UPDATE_INVENTORY=1 to generate): %v", cataloguePath, err)
	}
	if len(cat.Entries) == 0 {
		t.Fatalf("%s is empty", cataloguePath)
	}
	return cat
}
