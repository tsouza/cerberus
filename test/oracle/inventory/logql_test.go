package inventory

import (
	"encoding/json"
	"fmt"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/grafana/loki/v3/pkg/logql/syntax"
)

const (
	logQLInventoryFile  = "logql-feature-inventory.json"
	logQLExclusionsFile = "logql-feature-exclusions.json"
)

// TestLogQLInventoryIsRegenerable regenerates the inventory from the
// pinned parser and diffs it byte-for-byte against the checked-in
// JSON. Set CERBERUS_UPDATE_INVENTORY=1 to rewrite the artifact (the
// same update-via-env convention as `just update-golden`).
func TestLogQLInventoryIsRegenerable(t *testing.T) {
	t.Parallel()

	inv, err := GenerateLogQL()
	if err != nil {
		t.Fatalf("GenerateLogQL: %v", err)
	}
	want, err := MarshalInventory(inv)
	if err != nil {
		t.Fatalf("MarshalInventory: %v", err)
	}

	path := filepath.Join(inventoryDir, logQLInventoryFile)
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

// TestLogQLExclusionsAreSound validates the documented-exclusions
// artifact: every exclusion must reference an existing inventory row,
// carry a non-empty rationale, and appear at most once. The
// shrink-only half of the contract lives in
// TestLogQLShowcaseCoversInventory: an exclusion whose row a panel
// target now covers is stale and fails there.
func TestLogQLExclusionsAreSound(t *testing.T) {
	t.Parallel()

	inv := loadLogQLInventory(t)
	exc := loadLogQLExclusions(t)

	if exc.QL != "logql" {
		t.Fatalf("exclusions ql = %q, want logql", exc.QL)
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

// TestLogQLShowcaseCoversInventory is the coverage ratchet: every
// non-excluded inventory row must be exercised by at least one Loki
// panel target across the provisioned dashboard directories, and every
// excluded row must NOT be exercised (a covered exclusion is stale —
// shrink the exclusions file).
//
// Matching is AST-level: each target expression is parsed with the
// pinned LogQL parser and walked via CollectLogQLFeatureIDs, so "rate"
// can never match "bytes_rate" and an operator token inside a label
// value or line-filter string can never count as coverage.
func TestLogQLShowcaseCoversInventory(t *testing.T) {
	t.Parallel()

	inv := loadLogQLInventory(t)
	exc := loadLogQLExclusions(t)
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
			for _, expr := range lokiTargetExprs(t, path) {
				targets++
				parsed, err := syntax.ParseExpr(degrafanaLogQL(expr))
				if err != nil {
					if isShowcase {
						// Showcase targets must be plain LogQL — a
						// templated or malformed expression cannot
						// honestly claim feature coverage.
						t.Errorf("%s: showcase target %q does not parse: %v", path, expr, err)
					}
					// Ops-family targets may carry Grafana template
					// constructs beyond the conventional built-ins
					// substituted by degrafanaLogQL; they simply
					// contribute no coverage.
					continue
				}
				for id := range CollectLogQLFeatureIDs(parsed) {
					covered[id] = true
					if _, ok := coveredBy[id]; !ok {
						coveredBy[id] = fmt.Sprintf("%s: %s", entry.Name(), expr)
					}
				}
			}
		}
	}
	if targets == 0 {
		t.Fatal("no Loki panel targets discovered across the dashboard dirs")
	}

	var missing []string
	for _, r := range inv.Rows {
		if excluded[r.ID] {
			if covered[r.ID] {
				t.Errorf("exclusion %q is stale: %s — remove it from %s (exclusions are shrink-only)",
					r.ID, coveredBy[r.ID], logQLExclusionsFile)
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

// lokiTargetExprs returns every non-empty `expr` whose effective
// datasource type is loki (target-level datasource wins over
// panel-level), recursing through row panels. Mirrors promTargetExprs.
func lokiTargetExprs(t *testing.T, path string) []string {
	t.Helper()
	raw, err := os.ReadFile(path)
	if err != nil {
		t.Fatalf("read %s: %v", path, err)
	}
	var dash rawDashboard
	if err := json.Unmarshal(raw, &dash); err != nil {
		t.Fatalf("parse %s: %v", path, err)
	}
	var out []string
	var walk func(panels []rawPanel)
	walk = func(panels []rawPanel) {
		for _, p := range panels {
			walk(p.Panels)
			panelType := dsType(p.Datasource)
			for _, tg := range p.Targets {
				effective := dsType(tg.Datasource)
				if effective == "" {
					effective = panelType
				}
				if effective != "loki" || strings.TrimSpace(tg.Expr) == "" {
					continue
				}
				out = append(out, tg.Expr)
			}
		}
	}
	walk(dash.Panels)
	return out
}

// degrafanaLogQL substitutes the conventional Grafana built-in
// variables so ops-family expressions parse as plain LogQL. Showcase
// dashboards are expected to avoid templating entirely.
func degrafanaLogQL(expr string) string {
	r := strings.NewReplacer(
		"$__auto", "5m",
		"$__interval", "1m",
		"$__range", "1h",
	)
	return r.Replace(expr)
}

// --- artifact loading --------------------------------------------------------

func loadLogQLInventory(t *testing.T) *Inventory {
	t.Helper()
	raw, err := os.ReadFile(filepath.Join(inventoryDir, logQLInventoryFile))
	if err != nil {
		t.Fatalf("read inventory: %v", err)
	}
	var inv Inventory
	if err := json.Unmarshal(raw, &inv); err != nil {
		t.Fatalf("parse inventory: %v", err)
	}
	if inv.QL != "logql" || len(inv.Rows) == 0 {
		t.Fatalf("inventory malformed: ql=%q rows=%d", inv.QL, len(inv.Rows))
	}
	return &inv
}

func loadLogQLExclusions(t *testing.T) *Exclusions {
	t.Helper()
	raw, err := os.ReadFile(filepath.Join(inventoryDir, logQLExclusionsFile))
	if err != nil {
		t.Fatalf("read exclusions: %v", err)
	}
	var exc Exclusions
	if err := json.Unmarshal(raw, &exc); err != nil {
		t.Fatalf("parse exclusions: %v", err)
	}
	return &exc
}
