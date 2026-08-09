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

// RunParity checks a fixture's answer against a REAL reference engine.
//
// It is the half of the parity layer that must live in package spec,
// because it touches the chDB session and therefore chdbEngineMu. The
// evaluation itself lives in test/spec/parityoracle/promql, which imports
// nothing from cerberus — see that package's doc comment, and the
// disjointness gate in test/regression that enforces it.
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
	if p.Oracle != OraclePrometheus {
		t.Fatalf("fixture %s: oracle %q has no runner in this lane", c.Name, p.Oracle)
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

	query, ok := c.Section("query.promql")
	if !ok {
		t.Fatalf("fixture %s: `parity:` requires a query.promql section", c.Name)
	}

	// Serialize the whole engine span, same contract RunRoundTrip honours.
	chdbEngineMu.Lock()
	defer chdbEngineMu.Unlock()

	db := OpenChDB(t)
	ApplySeed(t, db, rt.Seed)

	series, err := readSeededSeries(db)
	if err != nil {
		t.Fatalf("read seeded series back: %v", err)
	}
	if len(series) == 0 {
		t.Fatalf(
			"fixture %s: seed produced no readable series, so the reference engine would "+
				"trivially agree with any answer", c.Name,
		)
	}

	got, err := oracle.Evaluate(t, series, oracle.Query{
		Expr:  trimQuery(query),
		Start: evalStart,
		End:   evalEnd,
		Step:  step,
	})
	if err != nil {
		t.Fatalf("fixture %s: %v", c.Name, err)
	}

	compareAgainstReference(t, c, p, rt, got, step > 0)
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

// compareAgainstReference is the assertion itself.
//
// # Why an instant query does not compare timestamps
//
// Prometheus stamps an INSTANT query's output at the EVALUATION INSTANT,
// not at the source sample's own time. Cerberus's SQL row carries the
// source sample time (`max(TimeUnix) AS lwr_ts`), and the HTTP layer is
// what presents it at eval time. Comparing those two numbers at this layer
// would compare two different quantities and fail every sparse fixture for
// a reason unrelated to the query — so for an instant query the comparison
// is over label sets and VALUES, and the reference's own timestamps are
// asserted to be the eval instant instead.
//
// Nothing is lost from the bug classes this exists to catch: `timestamp()`
// reports the sample's time as a VALUE, so a wrong sample-time selection
// still shows up in the value comparison.
//
// A RANGE query is different — both sides stamp at step anchors — so
// timestamps ARE compared there.
func compareAgainstReference(
	t *testing.T, c *Case, p *Parity, rt *RoundTripSections, got []oracle.Result, isRange bool,
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
		if !oracle.EqualValues(g.Value, w.Value) {
			t.Errorf("fixture %s sample %d (%v): value differs\n  reference: %v\n  cerberus:  %v",
				c.Name, i, g.Labels, g.Value, w.Value)
		}
		if isRange && g.TMillis != w.TMillis {
			t.Errorf("fixture %s sample %d (%v): timestamp differs\n  reference: %d\n  cerberus:  %d",
				c.Name, i, g.Labels, g.TMillis, w.TMillis)
		}
	}

	if !p.ComparesInFull() {
		t.Logf("fixture %s compared with scope %q", c.Name, p.Scope)
	}
}

func trimQuery(q string) string { return strings.TrimSpace(q) }

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
func referenceShapeOfExpectedRows(rt *RoundTripSections) ([]oracle.Result, error) {
	out := make([]oracle.Result, 0, len(rt.ExpectedRows))
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
		out = append(out, oracle.Result{Labels: lbls, TMillis: ts, Value: value})
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
