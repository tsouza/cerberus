//go:build chdb

package spec

import (
	"database/sql"
	"encoding/json"
	"fmt"
	"math"
	"sort"
	"strings"
	"testing"
	"time"

	oracle "github.com/tsouza/cerberus/test/spec/parityoracle/promql"
)

// parityQuery is one evaluation to run against a reference engine,
// independent of which engine answers it.
type parityQuery struct {
	// Expr is the query text, verbatim from the fixture.
	Expr string

	// Start is the instant for an instant query, or the range start.
	Start time.Time

	// End equals Start for an instant query.
	End time.Time

	// Step is zero for an instant query, and the range step otherwise.
	Step time.Duration
}

// referenceSample is one output sample, in the single shape both oracles
// are flattened into so one comparator can serve every head.
type referenceSample struct {
	Labels  map[string]string
	TMillis int64
	Value   float64
}

// lokiParityEvaluator is the seam onto the AGPL-licensed Loki oracle.
//
// It is nil unless the `chdb_agpl_oracle` build tag is set, which is what
// compiles parity_loki_chdb.go and its init(). The indirection is not
// decoration: `github.com/grafana/loki/v3/pkg/logql` is AGPLv3 and this
// file is in the plain `chdb` build configuration, so a direct import
// here would drag AGPL code into every chdb-tagged lane. Every
// `//go:build` line in this tree is a single term (see
// test/regression/lint_build_tags_test.go), so `chdb && agpl_oracle`
// cannot be spelled directly and `chdb_agpl_oracle` — the synthetic tag
// CI already sets alongside both — carries the composition.
//
// A nil evaluator with a Loki-enrolled fixture is a hard failure, never a
// skip: it means the fixture's oracle was compiled out of the lane that
// was supposed to run it, which is exactly the silent non-check this
// whole mechanism exists to prevent.
var lokiParityEvaluator func(
	t *testing.T, db *sql.DB, c *Case, q parityQuery,
) ([]referenceSample, error)

// RunParity checks a fixture's answer against a REAL reference engine.
//
// It is the half of the parity layer that must live in package spec,
// because it touches the chDB session and therefore chdbEngineMu. The
// evaluation itself lives in test/spec/parityoracle/{promql,logql}, which
// import nothing from cerberus — see those packages' doc comments, and
// the disjointness gate in test/regression that enforces it.
//
// # There is no update path here, on purpose
//
// [RunRoundTrip] rewrites `expected_rows` under GOLDEN_UPDATE=1. This
// function has no such branch and never will: the value it compares
// against is computed by the reference engine on every run, so there is no
// artefact for regeneration to overwrite. That is the whole design.
// GOLDEN_UPDATE=1 changes nothing about this check, which is what makes a
// semantic regression impossible to absorb rather than merely likely to be
// noticed.
//
// A fixture with no `parity:` section is a no-op, exactly as RunRoundTrip
// is for a fixture with no `seed:`.
func RunParity(t *testing.T, c *Case, evalStart, evalEnd time.Time, step time.Duration) {
	t.Helper()

	p, enrolled, err := LoadParity(c)
	if err != nil {
		t.Fatalf("LoadParity: %v", err)
	}
	if !enrolled {
		return
	}

	rt, err := LoadRoundTrip(c)
	if err != nil {
		t.Fatalf("LoadRoundTrip: %v", err)
	}
	if !rt.IsRoundTrip() {
		t.Fatalf(
			"fixture %s carries a `parity:` section but no executable round-trip. "+
				"The parity check reads the seeded rows back out of chDB, so it needs the same "+
				"`seed:` + `expected_rows:` opt-in RunRoundTrip needs.", c.Name,
		)
	}

	querySection := parityQuerySections[p.Oracle]
	if querySection == "" {
		t.Fatalf("fixture %s: oracle %q has no runner in this lane", c.Name, p.Oracle)
	}
	query, ok := c.Section(querySection)
	if !ok {
		t.Fatalf("fixture %s: oracle %q requires a %s section", c.Name, p.Oracle, querySection)
	}

	// Serialize the whole engine span, same contract RunRoundTrip honours.
	chdbEngineMu.Lock()
	defer chdbEngineMu.Unlock()

	db := OpenChDB(t)
	ApplySeed(t, db, rt.Seed)

	q := parityQuery{
		Expr:  strings.TrimSpace(query),
		Start: evalStart,
		End:   evalEnd,
		Step:  step,
	}

	var got []referenceSample
	switch p.Oracle {
	case OraclePrometheus:
		got, err = evaluatePrometheusParity(t, db, c, q)
	case OracleLoki:
		if lokiParityEvaluator == nil {
			t.Fatalf(
				"fixture %s is enrolled against the %q oracle, but this lane was built without "+
					"the `chdb_agpl_oracle` build tag, so the Loki oracle is compiled out and the "+
					"fixture would be checked against nothing. Run this package with "+
					"`-tags chdb,agpl_oracle,chdb_agpl_oracle`.", c.Name, p.Oracle,
			)
		}
		got, err = lokiParityEvaluator(t, db, c, q)
	default:
		t.Fatalf("fixture %s: oracle %q has no runner in this lane", c.Name, p.Oracle)
	}
	if err != nil {
		t.Fatalf("fixture %s: %v", c.Name, err)
	}

	compareAgainstReference(t, c, p, rt, got, comparesTimestamps(p.Oracle, step))
}

// parityQuerySections maps an oracle to the TXTAR section holding the
// query it answers. A missing entry is what makes an oracle with no
// runner in this lane fail loudly rather than evaluate nothing.
var parityQuerySections = map[string]string{
	OraclePrometheus: "query.promql",
	OracleLoki:       "query.logql",
}

// comparesTimestamps decides whether the output timestamps participate in
// the comparison, and the answer differs by head because the two heads
// stamp an INSTANT result differently.
//
// Prometheus stamps an instant result at the EVALUATION INSTANT, while
// cerberus's PromQL row carries the source sample time (`max(TimeUnix) AS
// lwr_ts`) and the HTTP layer is what re-presents it at eval time.
// Comparing those two numbers would compare two different quantities and
// fail every sparse fixture for a reason unrelated to the query. Nothing
// is lost from the bug classes this exists to catch: `timestamp()`
// reports the sample's time as a VALUE.
//
// The LogQL instant lowering has no such asymmetry — cerberus projects
// `now64(?) AS TimeUnix`, the eval instant itself, which is exactly what
// Loki stamps — so its timestamps ARE compared, and a fixture that
// anchored its answer to the wrong instant fails here.
//
// A RANGE query on either head stamps at step anchors on both sides, so
// timestamps are always compared there.
func comparesTimestamps(oracleName string, step time.Duration) bool {
	return step > 0 || oracleName == OracleLoki
}

// evaluatePrometheusParity answers q with the real upstream Prometheus
// engine over the rows the seed actually landed in chDB.
func evaluatePrometheusParity(
	t *testing.T, db *sql.DB, c *Case, q parityQuery,
) ([]referenceSample, error) {
	t.Helper()

	series, err := readSeededSeries(db)
	if err != nil {
		return nil, fmt.Errorf("read seeded series back: %w", err)
	}
	if len(series) == 0 {
		return nil, fmt.Errorf(
			"fixture %s: seed produced no readable series, so the reference engine would "+
				"trivially agree with any answer", c.Name,
		)
	}

	got, err := oracle.Evaluate(t, series, oracle.Query{
		Expr:  q.Expr,
		Start: q.Start,
		End:   q.End,
		Step:  q.Step,
	})
	if err != nil {
		return nil, err
	}

	out := make([]referenceSample, 0, len(got))
	for _, r := range got {
		out = append(out, referenceSample{Labels: r.Labels, TMillis: r.TMillis, Value: r.Value})
	}
	return out, nil
}

// seededMetricsTables are the metric tables a PromQL fixture's seed may
// create. The reader unions whichever ones exist, because a fixture seeds
// only the tables its own query needs.
var seededMetricsTables = []string{
	"otel_metrics_gauge",
	"otel_metrics_sum",
}

// readSeededSeries reads the seeded rows back OUT of chDB and groups them
// into reference-engine series.
//
// Reading back rather than re-parsing the `seed:` text is deliberate: it
// shows the oracle the data as it ACTUALLY LANDED — after DEFAULTs, after
// coercion — instead of as the SQL claims it will land. Otherwise a
// disagreement about what the seed means would be invisible to exactly the
// check meant to find disagreements.
func readSeededSeries(db *sql.DB) ([]oracle.Series, error) {
	byKey := map[string]*oracle.Series{}

	for _, table := range seededMetricsTables {
		//nolint:gosec // table comes from seededMetricsTables, not from fixture text.
		q := "SELECT MetricName, toJSONString(Attributes), toUnixTimestamp64Milli(TimeUnix), Value " +
			"FROM " + table + " ORDER BY MetricName, TimeUnix"
		rows, err := db.Query(q)
		if err != nil {
			// A fixture that never created this table is not an error.
			continue
		}
		if err := scanSeriesRows(rows, byKey); err != nil {
			_ = rows.Close()
			return nil, err
		}
		if err := rows.Close(); err != nil {
			return nil, err
		}
	}

	out := make([]oracle.Series, 0, len(byKey))
	for _, s := range byKey {
		out = append(out, *s)
	}
	sort.Slice(out, func(i, j int) bool {
		return labelKey(out[i].Labels) < labelKey(out[j].Labels)
	})
	return out, nil
}

func scanSeriesRows(rows *sql.Rows, byKey map[string]*oracle.Series) error {
	for rows.Next() {
		var name, attrsJSON string
		var tsMillis int64
		var value float64
		if err := rows.Scan(&name, &attrsJSON, &tsMillis, &value); err != nil {
			return err
		}
		lbls, err := labelsFromAttributesJSON(name, attrsJSON)
		if err != nil {
			return err
		}
		key := labelKey(lbls)
		s, ok := byKey[key]
		if !ok {
			s = &oracle.Series{Labels: lbls}
			byKey[key] = s
		}
		s.Points = append(s.Points, oracle.Point{TMillis: tsMillis, Value: value})
	}
	return rows.Err()
}

// compareAgainstReference is the assertion itself. See
// [comparesTimestamps] for which axes participate and why.
func compareAgainstReference(
	t *testing.T, c *Case, p *Parity, rt *RoundTripSections,
	got []referenceSample, compareTimestamps bool,
) {
	t.Helper()

	want, err := referenceShapeOfExpectedRows(rt)
	if err != nil {
		t.Fatalf("fixture %s: %v", c.Name, err)
	}

	if len(got) != len(want) {
		t.Fatalf(
			"fixture %s: reference engine returned %d sample(s), cerberus %d.\n"+
				"  reference: %v\n  cerberus:  %v\n"+
				"This is a real disagreement about the answer, not a golden to regenerate — "+
				"there is no update path for this check.",
			c.Name, len(got), len(want), got, want,
		)
	}

	for i := range got {
		g, w := got[i], want[i]
		if labelKey(g.Labels) != labelKey(w.Labels) {
			t.Errorf("fixture %s sample %d: labels differ\n  reference: %v\n  cerberus:  %v",
				c.Name, i, g.Labels, w.Labels)
			continue
		}
		// oracle.EqualValues is the promql oracle's NaN-aware float
		// predicate. It is used for BOTH heads on purpose: sample equality
		// is one rule, not one per head, and duplicating it would let the
		// two drift into disagreeing about what "equal" means.
		if !oracle.EqualValues(g.Value, w.Value) {
			t.Errorf("fixture %s sample %d (%v): value differs\n  reference: %v\n  cerberus:  %v",
				c.Name, i, g.Labels, g.Value, w.Value)
		}
		if compareTimestamps && g.TMillis != w.TMillis {
			t.Errorf("fixture %s sample %d (%v): timestamp differs\n  reference: %d\n  cerberus:  %d",
				c.Name, i, g.Labels, g.TMillis, w.TMillis)
		}
	}

	if !p.ComparesInFull() {
		t.Logf("fixture %s compared with scope %q", c.Name, p.Scope)
	}
}

// labelKey renders a label set as a stable, comparable string. Both sides
// are keyed through this one function so series identity can never differ
// by map iteration order or by formatting.
func labelKey(lbls map[string]string) string {
	keys := make([]string, 0, len(lbls))
	for k := range lbls {
		keys = append(keys, k)
	}
	sort.Strings(keys)
	var b strings.Builder
	for i, k := range keys {
		if i > 0 {
			b.WriteByte(',')
		}
		b.WriteString(k)
		b.WriteByte('=')
		b.WriteString(lbls[k])
	}
	return b.String()
}

// labelsFromAttributesJSON builds the reference-engine label set for one
// seeded row: the OTel attribute map plus __name__.
//
// The empty-string metric name is dropped rather than written as an empty
// __name__, because cerberus emits `” AS MetricName` for derived samples
// (PromQL drops __name__ from a function's output) and Prometheus
// represents that as the label being ABSENT.
func labelsFromAttributesJSON(metricName, attrsJSON string) (map[string]string, error) {
	attrs := map[string]string{}
	if trimmed := strings.TrimSpace(attrsJSON); trimmed != "" && trimmed != "{}" {
		if err := json.Unmarshal([]byte(trimmed), &attrs); err != nil {
			return nil, fmt.Errorf("decode Attributes %q: %w", attrsJSON, err)
		}
	}
	out := make(map[string]string, len(attrs)+1)
	for k, v := range attrs {
		out[k] = v
	}
	if metricName != "" {
		out[promNameLabel] = metricName
	}
	return out, nil
}

// promNameLabel is Prometheus's metric-name label.
const promNameLabel = "__name__"

// expectedRowNameIdx / expectedRowAttrsIdx / expectedRowTimeIdx /
// expectedRowValueIdx are the positions of the four canonical Sample
// columns inside an `expected_rows:` row. The projection order is fixed by
// the Sample contract (MetricName, Attributes, TimeUnix, Value).
const (
	expectedRowNameIdx  = 0
	expectedRowAttrsIdx = 1
	expectedRowTimeIdx  = 2
	expectedRowValueIdx = 3
	expectedRowArity    = 4
)

// referenceShapeOfExpectedRows converts cerberus's `expected_rows:` into
// the same flat shape the oracle returns, so the two can be compared
// element-wise.
//
// It errors rather than skipping on a row it cannot interpret. A fixture
// whose projection is not the canonical four-column Sample shape cannot be
// parity-checked at this layer, and silently comparing nothing would be
// the hollow green this mechanism exists to prevent.
func referenceShapeOfExpectedRows(rt *RoundTripSections) ([]referenceSample, error) {
	out := make([]referenceSample, 0, len(rt.ExpectedRows))
	for i, row := range rt.ExpectedRows {
		if len(row) != expectedRowArity {
			return nil, fmt.Errorf(
				"expected_rows[%d] has %d column(s), not the canonical Sample shape "+
					"(MetricName, Attributes, TimeUnix, Value); this fixture's projection cannot "+
					"be parity-checked at the row layer", i, len(row),
			)
		}
		name, err := rowString(row[expectedRowNameIdx], i, "MetricName")
		if err != nil {
			return nil, err
		}
		attrs, err := rowAttrs(row[expectedRowAttrsIdx], i)
		if err != nil {
			return nil, err
		}
		ts, err := rowTimeMillis(row[expectedRowTimeIdx], i)
		if err != nil {
			return nil, err
		}
		value, err := rowFloat(row[expectedRowValueIdx], i)
		if err != nil {
			return nil, err
		}

		lbls := make(map[string]string, len(attrs)+1)
		for k, v := range attrs {
			lbls[k] = v
		}
		if name != "" {
			lbls[promNameLabel] = name
		}
		out = append(out, referenceSample{Labels: lbls, TMillis: ts, Value: value})
	}

	sort.SliceStable(out, func(i, j int) bool {
		if ki, kj := labelKey(out[i].Labels), labelKey(out[j].Labels); ki != kj {
			return ki < kj
		}
		return out[i].TMillis < out[j].TMillis
	})
	return out, nil
}

func rowString(cell any, row int, col string) (string, error) {
	s, ok := cell.(string)
	if !ok {
		return "", fmt.Errorf("expected_rows[%d].%s is %T, want string", row, col, cell)
	}
	return s, nil
}

func rowAttrs(cell any, row int) (map[string]string, error) {
	m, ok := cell.(map[string]any)
	if !ok {
		return nil, fmt.Errorf("expected_rows[%d].Attributes is %T, want an object", row, cell)
	}
	out := make(map[string]string, len(m))
	for k, v := range m {
		s, ok := v.(string)
		if !ok {
			return nil, fmt.Errorf("expected_rows[%d].Attributes[%q] is %T, want string", row, k, v)
		}
		out[k] = s
	}
	return out, nil
}

func rowTimeMillis(cell any, row int) (int64, error) {
	s, ok := cell.(string)
	if !ok {
		return 0, fmt.Errorf("expected_rows[%d].TimeUnix is %T, want an RFC3339 string", row, cell)
	}
	ts, err := time.Parse(time.RFC3339Nano, s)
	if err != nil {
		return 0, fmt.Errorf("expected_rows[%d].TimeUnix %q: %w", row, s, err)
	}
	return ts.UnixMilli(), nil
}

func rowFloat(cell any, row int) (float64, error) {
	switch v := cell.(type) {
	case float64:
		return v, nil
	case json.Number:
		f, err := v.Float64()
		if err != nil {
			return 0, fmt.Errorf("expected_rows[%d].Value %q: %w", row, v.String(), err)
		}
		return f, nil
	case string:
		f, ok := nonFiniteSentinel(v)
		if !ok {
			return 0, fmt.Errorf(
				"expected_rows[%d].Value is the string %q, which is not a number and not one of "+
					"the non-finite sentinels (%s, %s, %s)",
				row, v, sentinelNaN, sentinelPosInf, sentinelNegInf,
			)
		}
		return f, nil
	default:
		return 0, fmt.Errorf("expected_rows[%d].Value is %T, want a number", row, cell)
	}
}

// nonFiniteSentinel decodes the string spelling `expected_rows:` uses for a
// value JSON cannot represent — ±Inf and NaN — back into the float it
// stands for.
//
// This DECODES an encoding; it does not relax a comparison. The decoded
// float goes through the same exact [oracle.EqualValues] every other value
// does, and a fixture whose reference answer is +Inf still fails if
// cerberus answers -Inf or 3. Without it, every fixture whose honest answer
// is non-finite — a quantile with phi outside [0,1], a clamp against a NaN
// bound — is unreachable by the parity check purely because of how JSON
// spells infinity, which would silently confine the check to the fixtures
// least likely to expose an edge-case lowering bug.
func nonFiniteSentinel(s string) (float64, bool) {
	switch s {
	case sentinelNaN:
		return math.NaN(), true
	case sentinelPosInf, sentinelBareInf:
		return math.Inf(+1), true
	case sentinelNegInf:
		return math.Inf(-1), true
	}
	return 0, false
}
