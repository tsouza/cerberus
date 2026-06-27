package ddl

import (
	"strings"
	"testing"

	"github.com/tsouza/cerberus/internal/schema"
)

// mergeTreeStmts returns the four MergeTree CREATE TABLE statements (the five
// metrics tables, the traces spans table, and the traces trace_id_ts lookup,
// plus the logs table) that carry a SETTINGS tail — i.e. every auto-created
// table EXCEPT the traces materialized view, which has no SETTINGS clause.
func mergeTreeStmts(t *testing.T, cfg Config) []string {
	t.Helper()
	var out []string
	m, err := renderSignal(cfg, Metrics)
	if err != nil {
		t.Fatalf("renderSignal(Metrics): %v", err)
	}
	// The metric-name ADD PROJECTION ALTERs carry no SETTINGS tail (they
	// inherit the table's settings); only the CREATE TABLE statements do.
	for _, stmt := range m {
		if strings.HasPrefix(stmt, "ALTER TABLE") {
			continue
		}
		out = append(out, stmt)
	}
	l, err := renderSignal(cfg, Logs)
	if err != nil {
		t.Fatalf("renderSignal(Logs): %v", err)
	}
	out = append(out, l...)
	tr, err := renderSignal(cfg, Traces)
	if err != nil {
		t.Fatalf("renderSignal(Traces): %v", err)
	}
	// tr[2] is the materialized view (no SETTINGS tail) — exclude it.
	out = append(out, tr[0], tr[1])
	return out
}

// replicatedCfg is the Replicated-database engine mode, exercised alongside the
// default MergeTree mode so the SETTINGS append is proven orthogonal to the
// engine / ON CLUSTER shape.
func replicatedCfg(settings []schema.KV) Config {
	return Config{
		Settings: settings,
		DatabaseEngine: DatabaseEngine{
			Replicated:        true,
			ReplicatedZooPath: "/clickhouse/databases/otel",
		},
	}.withDefaults()
}

// TestSettings_UnsetByteIdentical is the backward-compat regression gate: an
// unset (nil) Settings slice must render the auto-create DDL byte-identical to
// the bare upstream template, in BOTH the default MergeTree mode and the
// Replicated-database mode. Any drift here means the append seam leaked into
// the zero-config path.
func TestSettings_UnsetByteIdentical(t *testing.T) {
	modes := map[string]struct{ base, withEmpty Config }{
		"mergetree": {
			base:      Config{}.withDefaults(),
			withEmpty: Config{Settings: nil}.withDefaults(),
		},
		"replicated": {
			base:      replicatedCfg(nil),
			withEmpty: replicatedCfg([]schema.KV{}),
		},
	}
	for name, m := range modes {
		t.Run(name, func(t *testing.T) {
			base := mergeTreeStmts(t, m.base)
			empty := mergeTreeStmts(t, m.withEmpty)
			for i := range base {
				if base[i] != empty[i] {
					t.Errorf("stmt[%d] not byte-identical with empty Settings:\n--- base ---\n%s\n--- empty ---\n%s",
						i, base[i], empty[i])
				}
			}
		})
	}
}

// TestSettings_AppendedInBothModes pins that a configured Settings list lands
// as a comma-continued tail on every MergeTree table's SETTINGS clause — and
// renders the RHS quoting by type (string single-quoted, int bare) — in both
// engine modes. The materialized view (excluded above) carries no SETTINGS, so
// it is not asserted on here.
func TestSettings_AppendedInBothModes(t *testing.T) {
	settings := []schema.KV{
		{Key: "storage_policy", Value: "s3_tiered"},
		{Key: "min_bytes_for_wide_part", Value: int64(0)},
	}
	const wantTail = ", storage_policy = 's3_tiered', min_bytes_for_wide_part = 0"
	modes := map[string]Config{
		"mergetree":  Config{Settings: settings}.withDefaults(),
		"replicated": replicatedCfg(settings),
	}
	for name, cfg := range modes {
		t.Run(name, func(t *testing.T) {
			for i, stmt := range mergeTreeStmts(t, cfg) {
				// The continuation must extend the existing baked SETTINGS
				// clause — never open a second SETTINGS keyword.
				if n := strings.Count(stmt, "SETTINGS"); n != 1 {
					t.Errorf("stmt[%d]: want exactly one SETTINGS keyword, got %d:\n%s", i, n, stmt)
				}
				if !strings.Contains(stmt, wantTail) {
					t.Errorf("stmt[%d]: missing appended settings tail %q:\n%s", i, wantTail, stmt)
				}
				// The baked tail must still be present and precede the append.
				baked := strings.Index(stmt, "ttl_only_drop_parts")
				appended := strings.Index(stmt, "storage_policy")
				if baked < 0 || appended < 0 || appended < baked {
					t.Errorf("stmt[%d]: appended settings must follow the baked tail:\n%s", i, stmt)
				}
			}
		})
	}
}

// TestSettings_MaterializedViewUntouched pins that the traces materialized
// view never gains a SETTINGS clause even when Settings is configured — it has
// no MergeTree SETTINGS tail to continue, so the append must skip it.
func TestSettings_MaterializedViewUntouched(t *testing.T) {
	cfg := Config{Settings: []schema.KV{{Key: "storage_policy", Value: "s3_tiered"}}}.withDefaults()
	tr, err := renderSignal(cfg, Traces)
	if err != nil {
		t.Fatalf("renderSignal(Traces): %v", err)
	}
	mv := tr[2]
	if !strings.Contains(mv, "CREATE MATERIALIZED VIEW") {
		t.Fatalf("traces[2] is not the materialized view:\n%s", mv)
	}
	if strings.Contains(mv, "storage_policy") {
		t.Errorf("materialized view must not carry appended settings:\n%s", mv)
	}
}

// TestSettingsClause_EmptyIsBlank pins the helper contract directly: no
// configured settings renders an empty clause (the source of the byte-identical
// default), and a configured list renders the leading-comma continuation.
func TestSettingsClause_EmptyIsBlank(t *testing.T) {
	if got := (Config{}).settingsClause(); got != "" {
		t.Errorf("empty settingsClause = %q, want \"\"", got)
	}
	cfg := Config{Settings: []schema.KV{{Key: "k", Value: int64(1)}}}
	if got, want := cfg.settingsClause(), ", k = 1"; got != want {
		t.Errorf("settingsClause = %q, want %q", got, want)
	}
}
