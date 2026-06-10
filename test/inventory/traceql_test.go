package inventory

import (
	"encoding/json"
	"fmt"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/grafana/tempo/pkg/traceql"
)

const (
	traceqlInventoryFile  = "traceql-feature-inventory.json"
	traceqlExclusionsFile = "traceql-feature-exclusions.json"
)

// TestTraceQLInventoryIsRegenerable regenerates the inventory from the
// pinned parser and diffs it byte-for-byte against the checked-in
// JSON. Set CERBERUS_UPDATE_INVENTORY=1 to rewrite the artifact.
func TestTraceQLInventoryIsRegenerable(t *testing.T) {
	t.Parallel()

	inv, err := GenerateTraceQL()
	if err != nil {
		t.Fatalf("GenerateTraceQL: %v", err)
	}
	want, err := MarshalInventory(inv)
	if err != nil {
		t.Fatalf("MarshalInventory: %v", err)
	}

	path := filepath.Join(inventoryDir, traceqlInventoryFile)
	if os.Getenv("CERBERUS_UPDATE_INVENTORY") != "" {
		if err := os.WriteFile(path, want, 0o644); err != nil {
			t.Fatalf("write %s: %v", path, err)
		}
		t.Logf("rewrote %s (%d rows)", path, len(inv.Rows))
		return
	}

	got, err := os.ReadFile(path)
	if err != nil {
		t.Fatalf("read %s (rerun with CERBERUS_UPDATE_INVENTORY=1 to generate): %v", path, err)
	}
	if string(got) != string(want) {
		t.Fatalf("%s is stale relative to the pinned parser — rerun with "+
			"CERBERUS_UPDATE_INVENTORY=1 and commit the diff.\n--- want %d bytes, got %d bytes",
			path, len(want), len(got))
	}
}

// TestTraceQLExclusionsAreSound validates the documented-exclusions
// artifact — same contract as the PromQL twin.
func TestTraceQLExclusionsAreSound(t *testing.T) {
	t.Parallel()

	inv := loadTraceQLInventory(t)
	exc := loadTraceQLExclusions(t)

	if exc.QL != "traceql" {
		t.Fatalf("exclusions ql = %q, want traceql", exc.QL)
	}
	rowIDs := map[string]bool{}
	for _, r := range inv.Rows {
		rowIDs[r.ID] = true
	}
	seen := map[string]bool{}
	for _, e := range exc.Exclusions {
		if !rowIDs[e.ID] {
			t.Errorf("exclusion %q references no inventory row", e.ID)
		}
		if strings.TrimSpace(e.Rationale) == "" {
			t.Errorf("exclusion %q carries no rationale — every exclusion must justify itself", e.ID)
		}
		if seen[e.ID] {
			t.Errorf("exclusion %q is listed twice", e.ID)
		}
		seen[e.ID] = true
	}
}

// TestTraceQLShowcaseCoversInventory is the coverage ratchet: every
// non-excluded inventory row must be exercised by at least one Tempo
// panel target across the provisioned dashboard directories, and every
// excluded row must NOT be exercised. Matching is AST-level via
// CollectTraceQLFeatureIDs over the pinned parser.
func TestTraceQLShowcaseCoversInventory(t *testing.T) {
	t.Parallel()

	inv := loadTraceQLInventory(t)
	exc := loadTraceQLExclusions(t)
	excluded := map[string]bool{}
	for _, e := range exc.Exclusions {
		excluded[e.ID] = true
	}

	covered := map[string]bool{}
	coveredBy := map[string]string{}
	targets := 0

	for _, dir := range dashboardDirs {
		entries, err := os.ReadDir(dir)
		if err != nil {
			t.Fatalf("read dashboards dir %s: %v", dir, err)
		}
		for _, entry := range entries {
			if entry.IsDir() || !strings.HasSuffix(entry.Name(), ".json") {
				continue
			}
			path := filepath.Join(dir, entry.Name())
			isShowcase := strings.HasPrefix(entry.Name(), "showcase-")
			for _, q := range tempoTargetQueries(t, path) {
				targets++
				parsed, err := traceql.Parse(q)
				if err != nil {
					if isShowcase {
						// Showcase targets must be plain TraceQL — a
						// templated or malformed expression cannot
						// honestly claim feature coverage.
						t.Errorf("%s: showcase target %q does not parse: %v", path, q, err)
					}
					continue
				}
				for id := range CollectTraceQLFeatureIDs(parsed) {
					covered[id] = true
					if _, ok := coveredBy[id]; !ok {
						coveredBy[id] = fmt.Sprintf("%s: %s", entry.Name(), q)
					}
				}
			}
		}
	}
	if targets == 0 {
		t.Fatal("no Tempo panel targets discovered across the dashboard dirs")
	}

	var missing []string
	for _, r := range inv.Rows {
		if excluded[r.ID] {
			if covered[r.ID] {
				t.Errorf("exclusion %q is stale: %s — remove it from %s (exclusions are shrink-only)",
					r.ID, coveredBy[r.ID], traceqlExclusionsFile)
			}
			continue
		}
		if !covered[r.ID] {
			missing = append(missing, fmt.Sprintf("%s (pin: %s)", r.ID, r.Pin))
		}
	}
	if len(missing) > 0 {
		t.Errorf("%d inventory rows have no covering panel target:\n  %s",
			len(missing), strings.Join(missing, "\n  "))
	}
}

// tempoTargetQueries returns every non-empty TraceQL query whose
// effective datasource type is tempo (target-level datasource wins
// over panel-level), recursing through row panels. Tempo targets carry
// the expression in `query` (Grafana's Tempo datasource field); the
// `expr` spelling is read too for forward-compatibility.
func tempoTargetQueries(t *testing.T, path string) []string {
	t.Helper()
	raw, err := os.ReadFile(path)
	if err != nil {
		t.Fatalf("read %s: %v", path, err)
	}
	var dash rawTempoDashboard
	if err := json.Unmarshal(raw, &dash); err != nil {
		t.Fatalf("parse %s: %v", path, err)
	}
	var out []string
	var walk func(panels []rawTempoPanel)
	walk = func(panels []rawTempoPanel) {
		for _, p := range panels {
			walk(p.Panels)
			panelType := dsType(p.Datasource)
			for _, tg := range p.Targets {
				effective := dsType(tg.Datasource)
				if effective == "" {
					effective = panelType
				}
				if effective != "tempo" {
					continue
				}
				q := strings.TrimSpace(tg.Query)
				if q == "" {
					q = strings.TrimSpace(tg.Expr)
				}
				if q == "" {
					continue
				}
				out = append(out, q)
			}
		}
	}
	walk(dash.Panels)
	return out
}

type rawTempoDashboard struct {
	Panels []rawTempoPanel `json:"panels"`
}

type rawTempoPanel struct {
	Type       string           `json:"type"`
	Datasource json.RawMessage  `json:"datasource"`
	Targets    []rawTempoTarget `json:"targets"`
	Panels     []rawTempoPanel  `json:"panels"`
}

type rawTempoTarget struct {
	Query      string          `json:"query"`
	Expr       string          `json:"expr"`
	Datasource json.RawMessage `json:"datasource"`
}

func loadTraceQLInventory(t *testing.T) *Inventory {
	t.Helper()
	raw, err := os.ReadFile(filepath.Join(inventoryDir, traceqlInventoryFile))
	if err != nil {
		t.Fatalf("read inventory: %v", err)
	}
	var inv Inventory
	if err := json.Unmarshal(raw, &inv); err != nil {
		t.Fatalf("parse inventory: %v", err)
	}
	if inv.QL != "traceql" || len(inv.Rows) == 0 {
		t.Fatalf("inventory malformed: ql=%q rows=%d", inv.QL, len(inv.Rows))
	}
	return &inv
}

func loadTraceQLExclusions(t *testing.T) *Exclusions {
	t.Helper()
	raw, err := os.ReadFile(filepath.Join(inventoryDir, traceqlExclusionsFile))
	if err != nil {
		t.Fatalf("read exclusions: %v", err)
	}
	var exc Exclusions
	if err := json.Unmarshal(raw, &exc); err != nil {
		t.Fatalf("parse exclusions: %v", err)
	}
	return &exc
}
