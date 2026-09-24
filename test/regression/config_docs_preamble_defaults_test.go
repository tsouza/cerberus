package regression

// Pins the hand-typed defaults in docs/configuration.md's solver, actuals
// and resource-bound preamble to the constants the binary actually ships.
//
// The generated table reads every default LIVE from the loader, but the
// preamble that follows it (cmd/cerberus/cmd_configdocs.go) enumerates the
// knobs three packages resolve OUTSIDE that loader — internal/solver,
// internal/actuals, internal/promql and internal/chsql — and spells each
// default as a literal in prose. Nothing else ties those literals to the
// Go constants: a retuned ceiling or a widened floor leaves the doc quietly
// asserting the old number. This test reads every "(type, default `X`)"
// bullet from the rendered doc and checks it against the live value —
// exported defaults through their package's DefaultConfig /
// DefaultResourceBounds, unexported chsql ceilings through the constant's
// own declaration — and fails on a bullet it has no source for, so a knob
// added to the preamble without a pin here is refused too.

import (
	"go/ast"
	"go/parser"
	"go/token"
	"os"
	"path/filepath"
	"regexp"
	"strconv"
	"strings"
	"testing"
	"time"

	"github.com/tsouza/cerberus/internal/actuals"
	"github.com/tsouza/cerberus/internal/promql"
	"github.com/tsouza/cerberus/internal/solver"
)

const (
	configDocsPath      = "../../docs/configuration.md"
	chsqlBoundDir       = "../../internal/chsql"
	lwrFanoutBoundFile  = "lwr_fanout_bound.go"
	rateWindowBoundFile = "rate_window_fanout_bound.go"
	emitSizeBoundFile   = "emit_size_bound.go"
)

// preambleBullet matches one joined bullet of the form
// **`CERBERUS_X`** (type, default `V`) ..., or the paired form
// **`CERBERUS_X_LOWER`** / **`_UPPER`** (float, default `A` / `B`).
var preambleBullet = regexp.MustCompile(
	"^\\*\\*`(CERBERUS_[A-Z0-9_]+)`\\*\\*(?: / \\*\\*`((?:CERBERUS_|_)[A-Z0-9_]+)`\\*\\*)? \\((int|int64|float|duration|bool), default `([^`]+)`(?: / `([^`]+)`)?\\)",
)

// documentedDefault is one knob's default as the doc spells it.
type documentedDefault struct {
	kind  string
	value string
}

// preambleDefaults returns every knob the preamble documents with a
// literal default, keyed by env var. Continuation lines (indented by two
// spaces) are joined onto their bullet so a "(int64, default\n  `N`)"
// wrap is read as one bullet.
func preambleDefaults(t *testing.T) map[string]documentedDefault {
	t.Helper()
	raw, err := os.ReadFile(configDocsPath)
	if err != nil {
		t.Fatalf("read %s: %v", configDocsPath, err)
	}
	var bullets []string
	for _, line := range strings.Split(string(raw), "\n") {
		switch {
		case strings.HasPrefix(line, "- "):
			bullets = append(bullets, strings.TrimPrefix(line, "- "))
		case strings.HasPrefix(line, "  ") && len(bullets) > 0:
			bullets[len(bullets)-1] += " " + strings.TrimSpace(line)
		default:
			bullets = append(bullets, "")
		}
	}
	out := map[string]documentedDefault{}
	for _, b := range bullets {
		m := preambleBullet.FindStringSubmatch(b)
		if m == nil {
			continue
		}
		name, suffix, kind, first, second := m[1], m[2], m[3], m[4], m[5]
		out[name] = documentedDefault{kind: kind, value: first}
		if suffix != "" {
			if second == "" {
				t.Fatalf("%s pairs a second knob %s but documents one default", name, suffix)
			}
			// A pair names its second knob either in full or by the tail
			// that differs from the first (CERBERUS_..._DRIFT_LOWER_RATIO /
			// _UPPER_RATIO); a tail is grafted onto the first name's prefix.
			pair := suffix
			if strings.HasPrefix(suffix, "_") {
				pair = name[:strings.LastIndex(name, "_LOWER")] + suffix
			}
			out[pair] = documentedDefault{kind: kind, value: second}
		}
	}
	return out
}

// goIntConst reads the integer literal an unexported package-level
// constant is declared with, e.g. `maxRangeBucketFanoutRows = 4_000_000`.
func goIntConst(t *testing.T, file, name string) int64 {
	t.Helper()
	fset := token.NewFileSet()
	f, err := parser.ParseFile(fset, filepath.Join(chsqlBoundDir, file), nil, 0)
	if err != nil {
		t.Fatalf("parse %s: %v", file, err)
	}
	for _, decl := range f.Decls {
		gd, ok := decl.(*ast.GenDecl)
		if !ok || gd.Tok != token.CONST {
			continue
		}
		for _, spec := range gd.Specs {
			vs := spec.(*ast.ValueSpec)
			for i, ident := range vs.Names {
				if ident.Name != name || i >= len(vs.Values) {
					continue
				}
				lit, ok := vs.Values[i].(*ast.BasicLit)
				if !ok || lit.Kind != token.INT {
					t.Fatalf("%s.%s is not an integer literal", file, name)
				}
				v, err := strconv.ParseInt(strings.ReplaceAll(lit.Value, "_", ""), 0, 64)
				if err != nil {
					t.Fatalf("%s.%s: %v", file, name, err)
				}
				return v
			}
		}
	}
	t.Fatalf("%s: constant %s not declared", file, name)
	return 0
}

// liveDefault renders a Go value the way the preamble spells it, so the
// comparison is on parsed values (a duration written `60s` equals
// 60*time.Second however String() would print it).
func assertDocumented(t *testing.T, doc map[string]documentedDefault, name string, live any) {
	t.Helper()
	d, ok := doc[name]
	if !ok {
		t.Errorf("%s: the preamble no longer documents a default for it", name)
		return
	}
	switch v := live.(type) {
	case int:
		got, err := strconv.ParseInt(d.value, 10, 64)
		if err != nil || got != int64(v) || (d.kind != "int" && d.kind != "int64") {
			t.Errorf("%s: doc says (%s, default `%s`), binary ships %d", name, d.kind, d.value, v)
		}
	case int64:
		got, err := strconv.ParseInt(d.value, 10, 64)
		if err != nil || got != v || d.kind != "int64" {
			t.Errorf("%s: doc says (%s, default `%s`), binary ships %d", name, d.kind, d.value, v)
		}
	case float64:
		got, err := strconv.ParseFloat(d.value, 64)
		if err != nil || got != v || d.kind != "float" {
			t.Errorf("%s: doc says (%s, default `%s`), binary ships %g", name, d.kind, d.value, v)
		}
	case time.Duration:
		got, err := time.ParseDuration(d.value)
		if err != nil || got != v || d.kind != "duration" {
			t.Errorf("%s: doc says (%s, default `%s`), binary ships %s", name, d.kind, d.value, v)
		}
	case bool:
		got, err := strconv.ParseBool(d.value)
		if err != nil || got != v || d.kind != "bool" {
			t.Errorf("%s: doc says (%s, default `%s`), binary ships %t", name, d.kind, d.value, v)
		}
	default:
		t.Fatalf("%s: unsupported live value type %T", name, live)
	}
	delete(doc, name)
}

// TestConfigDocsPreambleDefaultsMatchBinary fails when any literal default
// in the preamble disagrees with the constant the binary ships, when a
// pinned knob drops out of the preamble, or when the preamble documents a
// default this test has no live source for.
func TestConfigDocsPreambleDefaultsMatchBinary(t *testing.T) {
	doc := preambleDefaults(t)
	if len(doc) == 0 {
		t.Fatalf("%s: no preamble bullets with a literal default found", configDocsPath)
	}

	sc := solver.DefaultConfig()
	assertDocumented(t, doc, "CERBERUS_SHARD_MIN_FANOUT", sc.MinFanout)
	assertDocumented(t, doc, "CERBERUS_SHARD_MIN_ANCHOR_PAIRS", sc.MinAnchorPairs)
	assertDocumented(t, doc, "CERBERUS_SHARD_MAX_K", sc.MaxK)
	assertDocumented(t, doc, "CERBERUS_SHARD_MIN_ANCHORS_PER_SLICE", sc.MinAnchorsPerSlice)
	assertDocumented(t, doc, "CERBERUS_SHARD_PARALLEL", sc.Parallel)
	assertDocumented(t, doc, "CERBERUS_SOLVER_TIMEOUT", sc.Timeout)
	assertDocumented(t, doc, "CERBERUS_SHARD_MAX_OUTPUT_ROWS", sc.MaxOutputRows)

	ac := actuals.DefaultConfig()
	assertDocumented(t, doc, "CERBERUS_QUERY_ACTUALS_ENABLED", ac.Enabled)
	assertDocumented(t, doc, "CERBERUS_QUERY_ACTUALS_DRIFT_LOWER_RATIO", ac.DriftLowerRatio)
	assertDocumented(t, doc, "CERBERUS_QUERY_ACTUALS_DRIFT_UPPER_RATIO", ac.DriftUpperRatio)
	assertDocumented(t, doc, "CERBERUS_QUERY_ACTUALS_MIN_OBSERVATIONS", ac.MinObservations)
	assertDocumented(t, doc, "CERBERUS_QUERY_ACTUALS_EMA_ALPHA", ac.EMAAlpha)
	assertDocumented(t, doc, "CERBERUS_QUERY_ACTUALS_ENTRY_TTL", ac.EntryTTL)
	assertDocumented(t, doc, "CERBERUS_QUERY_ACTUALS_QUERY_LOG_POLL_INTERVAL", ac.QueryLogPollInterval)
	assertDocumented(t, doc, "CERBERUS_QUERY_ACTUALS_QUERY_LOG_LOOKBACK", ac.QueryLogLookback)
	assertDocumented(t, doc, "CERBERUS_QUERY_ACTUALS_QUERY_LOG_SETTLE_DELAY", ac.QueryLogSettleDelay)

	pb := promql.DefaultResourceBounds()
	assertDocumented(t, doc, "CERBERUS_PROMQL_HISTOGRAM_MERGE_MAX_COST_UNITS", pb.HistogramMergeMaxCostUnits)
	assertDocumented(t, doc, "CERBERUS_PROMQL_CLASSIC_BUCKET_MERGE_MAX_COST_UNITS", pb.ClassicBucketMergeMaxCostUnits)

	// internal/chsql may not be imported for its unexported ceilings; the
	// constant declarations are the source of truth the engine's env
	// overrides fall back to.
	assertDocumented(t, doc, "CERBERUS_CH_RANGE_BUCKET_FANOUT_MAX_ROWS", goIntConst(t, lwrFanoutBoundFile, "maxRangeBucketFanoutRows"))
	assertDocumented(t, doc, "CERBERUS_CH_RANGE_LWR_FANOUT_MAX_ROWS", goIntConst(t, lwrFanoutBoundFile, "maxRangeLWRFanoutRows"))
	assertDocumented(t, doc, "CERBERUS_CH_RATE_WINDOW_FANOUT_MAX_ROWS", goIntConst(t, rateWindowBoundFile, "maxRateWindowFanoutRows"))
	assertDocumented(t, doc, "CERBERUS_CH_MAX_EMITTED_SQL_BYTES", goIntConst(t, emitSizeBoundFile, "maxEmittedSQLBytes"))

	for name, d := range doc {
		t.Errorf("%s: preamble documents (%s, default `%s`) but this test has no live source to pin it to — add one", name, d.kind, d.value)
	}
}
