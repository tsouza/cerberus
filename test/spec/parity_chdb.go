//go:build chdb

package spec

import (
	"database/sql"
	"encoding/json"
	"fmt"
	"math"
	"sort"
	"strconv"
	"strings"
	"testing"
	"time"

	"github.com/tsouza/cerberus/internal/testsql"
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
		got, err = evaluatePrometheusParity(t, db, c, rt, q)
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

	cols, err := parityProjectionColumns(db, rt)
	if err != nil {
		t.Fatalf("fixture %s: %v", c.Name, err)
	}
	sc, err := locateSampleColumns(cols)
	if err != nil {
		t.Fatalf("fixture %s: %v", c.Name, err)
	}

	// A projection with no TimeUnix column cannot answer a comparison that
	// includes timestamps. Failing here rather than defaulting the missing
	// side to zero is the difference between reporting that the check
	// cannot be made and silently making a different, weaker one.
	compareTimestamps := comparesTimestamps(p.Oracle, step)
	if compareTimestamps && sc.ts < 0 {
		t.Fatalf(
			"fixture %s: its answer's timestamps participate in the comparison, but the "+
				"projection (%s) carries no %s column to compare them against",
			c.Name, strings.Join(cols, ", "), colTimeUnix,
		)
	}

	compareAgainstReference(t, c, p, rt, sc, got, compareTimestamps)
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
	t *testing.T, db *sql.DB, c *Case, rt *RoundTripSections, q parityQuery,
) ([]referenceSample, error) {
	t.Helper()

	series, err := readSeededSeries(db, rt, resourceLabelAllowlist(c))
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
func readSeededSeries(
	db *sql.DB, rt *RoundTripSections, allow resourceAllowlist,
) ([]oracle.Series, error) {
	byKey := map[string]*oracle.Series{}
	seedCols := testsql.SeedTableColumns(rt.Seed)

	for _, table := range seededMetricsTables {
		//nolint:gosec // every fragment is chosen from this file's own
		// constants, keyed off the seed's declared columns; no fixture
		// text reaches the query.
		q := "SELECT MetricName, toJSONString(Attributes), " +
			resourceAttributesProjection(seedCols[table]) + ", " +
			serviceNameProjection(seedCols[table]) + ", " +
			"toUnixTimestamp64Milli(TimeUnix), Value " +
			"FROM " + table + " ORDER BY MetricName, TimeUnix"
		rows, err := db.Query(q)
		if err != nil {
			// A fixture that never created this table is not an error.
			continue
		}
		if err := scanSeriesRows(rows, byKey, allow); err != nil {
			_ = rows.Close()
			return nil, err
		}
		if err := rows.Close(); err != nil {
			return nil, err
		}
	}

	if err := readSeededClassicHistograms(db, rt, allow, byKey); err != nil {
		return nil, err
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

func scanSeriesRows(rows *sql.Rows, byKey map[string]*oracle.Series, allow resourceAllowlist) error {
	for rows.Next() {
		var name, attrsJSON, resAttrsJSON, serviceName string
		var tsMillis int64
		var value float64
		if err := rows.Scan(
			&name, &attrsJSON, &resAttrsJSON, &serviceName, &tsMillis, &value,
		); err != nil {
			return err
		}
		lbls, err := labelsFromSeededRow(name, attrsJSON, resAttrsJSON, serviceName, allow)
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
// parityProjectionColumns returns the column names of the projection that
// produced this fixture's `expected_rows:`.
//
// The names come from the DRIVER, by running the fixture's own pinned
// `sql:` section against the already-seeded session and asking the result
// for its columns. The alternative — parsing the outer SELECT's aliases
// out of the SQL text — would be a second, hand-written answer to a
// question the driver already answers exactly, and the two would
// eventually disagree on some projection nobody thought to test.
//
// Nothing about the comparison becomes self-referential: only the column
// LABELLING is learned here. Every value still comes from the pinned
// `expected_rows:`, which is the artefact regeneration can corrupt and
// therefore the artefact the reference engine must be checked against.
func parityProjectionColumns(db *sql.DB, rt *RoundTripSections) ([]string, error) {
	query, args := substituteNow64(rt.SQL, rt.Args)
	query = testsql.ExpandStarProjection(query, testsql.SeedTableColumns(rt.Seed))
	query = testsql.RewriteMapProjections(query)
	query = testsql.NestMapOrderBy(query)

	rows, err := db.Query(query, args...)
	if err != nil {
		return nil, fmt.Errorf("re-run the fixture's own sql to read its projection: %w", err)
	}
	defer func() { _ = rows.Close() }()

	// rows.Columns() returns names without instantiating the driver's
	// column-type table, so it sidesteps the Map rows.ColumnTypes()
	// panic the round-trip runner documents.
	cols, err := rows.Columns()
	if err != nil {
		return nil, fmt.Errorf("read the projection's column names: %w", err)
	}
	return cols, nil
}

func compareAgainstReference(
	t *testing.T, c *Case, p *Parity, rt *RoundTripSections, sc sampleColumns,
	got []referenceSample, compareTimestamps bool,
) {
	t.Helper()

	want, err := referenceShapeOfExpectedRows(rt, sc)
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

// arity is the number of columns a row must have for every located
// Sample field to be in range.
func (sc sampleColumns) arity() int {
	n := 0
	for _, idx := range []int{sc.name, sc.attrs, sc.ts, sc.value} {
		if idx+1 > n {
			n = idx + 1
		}
	}
	return n
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

// classicHistogramTable is the OTel-CH table holding explicit-bounds
// histograms. It is read separately from [seededMetricsTables] because its
// rows are not samples: one row is a whole histogram.
const classicHistogramTable = "otel_metrics_histogram"

// The Prometheus series a classic histogram is exposed as, and the label
// carrying a bucket's upper bound.
const (
	bucketSuffix   = "_bucket"
	countSuffix    = "_count"
	sumSuffix      = "_sum"
	leLabel        = "le"
	positiveInfSTR = "+Inf"
)

// readSeededClassicHistograms explodes each seeded explicit-bounds
// histogram row into the float series Prometheus represents it as.
//
// # Why this needs no new oracle shape
//
// A classic histogram IS a set of plain float series in Prometheus's data
// model: `<name>_bucket` carrying an `le` label per boundary, plus
// `<name>_count` and `<name>_sum`. The reference engine has always been
// able to express that, and cerberus emits exactly those series from this
// table — so the only thing that was missing was a reader. Nothing about
// [oracle.Series] or the comparator changes, and native histograms (whose
// reference answer is a FloatHistogram rather than a float) remain out of
// reach for a genuinely different reason.
//
// # The two translations that are not identity
//
// OTel stores BucketCounts PER BUCKET; Prometheus's `le` series are
// CUMULATIVE, so the counts are accumulated as the bounds are walked. And
// BucketCounts carries one more entry than ExplicitBounds — the overflow
// bucket above the last boundary — which is Prometheus's `le="+Inf"`.
//
// A fixture that never created the table is not an error: most of the
// corpus seeds only the tables its own query needs.
func readSeededClassicHistograms(
	db *sql.DB, rt *RoundTripSections, allow resourceAllowlist, byKey map[string]*oracle.Series,
) error {
	seedCols := testsql.SeedTableColumns(rt.Seed)[classicHistogramTable]
	if len(seedCols) == 0 {
		return nil
	}

	//nolint:gosec // every fragment is chosen from this file's own
	// constants, keyed off the seed's declared columns.
	q := "SELECT MetricName, toJSONString(Attributes), " +
		resourceAttributesProjection(seedCols) + ", " + serviceNameProjection(seedCols) + ", " +
		"toUnixTimestamp64Milli(TimeUnix), Count, Sum, " +
		"toJSONString(BucketCounts), toJSONString(ExplicitBounds) " +
		"FROM " + classicHistogramTable + " ORDER BY MetricName, TimeUnix"
	rows, err := db.Query(q)
	if err != nil {
		return nil
	}
	defer func() { _ = rows.Close() }()

	for rows.Next() {
		var name, attrsJSON, resAttrsJSON, serviceName, countsJSON, boundsJSON string
		var tsMillis int64
		var count, sum float64
		if err := rows.Scan(
			&name, &attrsJSON, &resAttrsJSON, &serviceName, &tsMillis,
			&count, &sum, &countsJSON, &boundsJSON,
		); err != nil {
			return err
		}
		base, err := labelsFromSeededRow("", attrsJSON, resAttrsJSON, serviceName, allow)
		if err != nil {
			return err
		}
		var counts, bounds []float64
		if err := json.Unmarshal([]byte(countsJSON), &counts); err != nil {
			return fmt.Errorf("decode BucketCounts %q: %w", countsJSON, err)
		}
		if err := json.Unmarshal([]byte(boundsJSON), &bounds); err != nil {
			return fmt.Errorf("decode ExplicitBounds %q: %w", boundsJSON, err)
		}
		// OTel's canonical shape carries one more count than bound — the
		// overflow bucket above the last boundary. The corpus also seeds
		// the equal-length shape, which simply states no overflow bucket.
		// Both are well defined here because Prometheus's `+Inf` bucket is
		// the total Count either way; anything else is a seed whose
		// buckets cannot be read at all.
		if len(counts) != len(bounds) && len(counts) != len(bounds)+1 {
			return fmt.Errorf(
				"histogram %s has %d BucketCounts and %d ExplicitBounds; a bucket series can only "+
					"be reconstructed when the counts match the bounds, with or without a trailing "+
					"overflow bucket", name, len(counts), len(bounds),
			)
		}

		cumulative := 0.0
		for i, bound := range bounds {
			cumulative += counts[i]
			appendPoint(byKey, base, name+bucketSuffix,
				map[string]string{leLabel: formatBucketBound(bound)}, tsMillis, cumulative)
		}
		appendPoint(byKey, base, name+bucketSuffix,
			map[string]string{leLabel: positiveInfSTR}, tsMillis, count)
		appendPoint(byKey, base, name+countSuffix, nil, tsMillis, count)
		appendPoint(byKey, base, name+sumSuffix, nil, tsMillis, sum)
	}
	return rows.Err()
}

// formatBucketBound renders a bucket boundary the way the `le` label
// spells it, which must agree with ClickHouse's `toString(Float64)` — the
// expression cerberus's own emitter uses — or the two sides' bucket series
// carry different label sets and never align.
//
// Go's shortest round-trip formatting agrees with ClickHouse's across the
// range the corpus uses. The two part company only at the exponent
// thresholds ('g' switches to exponent form at >=1e21 and <1e-4), so a
// fixture seeding a boundary out there would fail on the label rather than
// silently compare the wrong bucket.
func formatBucketBound(bound float64) string {
	return strconv.FormatFloat(bound, 'g', -1, 64)
}

// appendPoint adds one sample to the series identified by the base label
// set plus __name__ and any extra labels, creating the series on first
// sight. The base map is never mutated: several synthesised series share
// one row's labels.
func appendPoint(
	byKey map[string]*oracle.Series, base map[string]string,
	name string, extra map[string]string, tsMillis int64, value float64,
) {
	lbls := make(map[string]string, len(base)+len(extra)+1)
	for k, v := range base {
		lbls[k] = v
	}
	for k, v := range extra {
		lbls[k] = v
	}
	lbls[promNameLabel] = name

	key := labelKey(lbls)
	s, ok := byKey[key]
	if !ok {
		s = &oracle.Series{Labels: lbls}
		byKey[key] = s
	}
	s.Points = append(s.Points, oracle.Point{TMillis: tsMillis, Value: value})
}

// The OTel-CH columns beyond Attributes that carry Prometheus labels, and
// the service-name key they resolve to.
const (
	colResourceAttributes = "ResourceAttributes"
	colServiceName        = "ServiceName"
	serviceNameLabel      = "service_name"
	serviceNameOTelKey    = "service.name"
)

// resourceAllowlist narrows which ResourceAttributes keys become labels.
// A nil allowlist promotes every key, which is the schema default —
// the allowlist is opt-IN narrowing, not opt-in enabling.
type resourceAllowlist map[string]bool

// resourceLabelAllowlist reads the fixture's own `resource_labels:`
// section, the same section the lowering harness uses to override the
// schema's promotion allowlist. Reading it here keeps the two sides
// looking at ONE declaration of which resource keys are labels; a fixture
// that narrows the set for the lowering but not for the reference would
// diverge on label sets for a reason unrelated to its query.
func resourceLabelAllowlist(c *Case) resourceAllowlist {
	body, ok := c.Section("resource_labels")
	if !ok {
		return nil
	}
	allow := resourceAllowlist{}
	for _, line := range strings.Split(body, "\n") {
		if key := strings.TrimSpace(line); key != "" {
			allow[key] = true
		}
	}
	return allow
}

// resourceAttributesProjection / serviceNameProjection select a column the
// seed may not have declared.
//
// The corpus's seeds are minimal — each declares only the columns its own
// query needs — so a fixture whose gauge table is (MetricName, Attributes,
// TimeUnix, Value) has no ResourceAttributes and no ServiceName to read.
// Substituting the empty literal keeps one reader able to serve every
// seed, and an absent column and an empty one mean the same thing to the
// promotion below.
func resourceAttributesProjection(seedCols []string) string {
	if !hasColumn(seedCols, colResourceAttributes) {
		return "'{}'"
	}
	return "toJSONString(" + colResourceAttributes + ")"
}

func serviceNameProjection(seedCols []string) string {
	if !hasColumn(seedCols, colServiceName) {
		return "''"
	}
	return "toString(" + colServiceName + ")"
}

func hasColumn(cols []string, want string) bool {
	for _, col := range cols {
		if strings.EqualFold(col, want) {
			return true
		}
	}
	return false
}

// sanitizeLabelName rewrites an OTel key into the Prometheus label name it
// is exposed as: every character outside [a-zA-Z0-9_] becomes '_', so
// `k8s.namespace.name` is addressable as `k8s_namespace_name`.
func sanitizeLabelName(key string) string {
	var b strings.Builder
	b.Grow(len(key))
	for _, r := range key {
		switch {
		case r >= 'a' && r <= 'z', r >= 'A' && r <= 'Z', r >= '0' && r <= '9', r == '_':
			b.WriteRune(r)
		default:
			b.WriteByte('_')
		}
	}
	return b.String()
}

// labelsFromSeededRow builds the reference-engine label set for one seeded
// row: the resource labels, overlaid by the metric attributes, plus
// service_name and __name__.
//
// # Why the translator has to do this at all
//
// A series' IDENTITY is a data-model fact, not a query-semantics one. OTel
// spreads a metric's labels across three columns — Attributes,
// ResourceAttributes and the dedicated ServiceName — while Prometheus has
// one flat label set and no notion of a resource. Something has to map
// between them, and both sides must reach the same answer or there is no
// series to align and every comparison degenerates into "the two engines
// returned different series".
//
// Reading only Attributes, as this did before, was not a neutral choice:
// it meant the reference engine could not see the labels cerberus promotes
// from the other two columns, so `sum by (service_name) (...)` grouped
// three ways on cerberus's side and one way on the reference's. That is
// not a disagreement about the query — it is the oracle being shown
// different data.
//
// # What this does and does not check
//
// The rule below is written out here rather than imported, because the
// oracle side may not link internal/schema. It is an independent
// expression of the same documented mapping, not a call into cerberus's,
// so a cerberus lowering that DEVIATES from that mapping — stops promoting
// ServiceName, sanitizes a key differently, resolves a collision the other
// way — makes the two label sets disagree and fails here.
//
// What no amount of independence can catch is a misconception SHARED by
// both: if the documented mapping is itself wrong, both sides implement
// the same wrong thing and agree. That axis belongs to the live
// compatibility harness, which compares against a real Prometheus fed by
// the real exporter rather than against a reading of the same spec.
func labelsFromSeededRow(
	metricName, attrsJSON, resAttrsJSON, serviceName string, allow resourceAllowlist,
) (map[string]string, error) {
	resAttrs, err := decodeAttributes(resAttrsJSON)
	if err != nil {
		return nil, err
	}
	attrs, err := decodeAttributes(attrsJSON)
	if err != nil {
		return nil, err
	}

	out := make(map[string]string, len(resAttrs)+len(attrs)+2)
	// Resource labels first: the metric's own attributes win a collision,
	// and service_name wins over both.
	for k, v := range resAttrs {
		if k == serviceNameOTelKey || k == serviceNameLabel {
			continue
		}
		if allow != nil && !allow[k] {
			continue
		}
		out[sanitizeLabelName(k)] = v
	}
	for k, v := range attrs {
		out[sanitizeLabelName(k)] = v
	}
	if serviceName != "" {
		out[serviceNameLabel] = serviceName
	}
	if metricName != "" {
		out[promNameLabel] = metricName
	}
	return out, nil
}

func decodeAttributes(attrsJSON string) (map[string]string, error) {
	attrs := map[string]string{}
	if trimmed := strings.TrimSpace(attrsJSON); trimmed != "" && trimmed != "{}" {
		if err := json.Unmarshal([]byte(trimmed), &attrs); err != nil {
			return nil, fmt.Errorf("decode attributes %q: %w", attrsJSON, err)
		}
	}
	return attrs, nil
}

// promNameLabel is Prometheus's metric-name label.
const promNameLabel = "__name__"

// The column names cerberus projects for each field of a Sample. They are
// the emitter's own aliases, so locating a field by name rather than by
// position is reading the projection's own labelling rather than assuming
// one.
const (
	colMetricName = "MetricName"
	colAttributes = "Attributes"
	colTimeUnix   = "TimeUnix"
	colValue      = "Value"
)

// sampleColumns is where each field of a Sample sits inside one
// `expected_rows:` row, or -1 when the projection does not carry it.
//
// # Why this is resolved by NAME and not by position
//
// The corpus projects five distinct column layouts, not one. The canonical
// `(MetricName, Attributes, TimeUnix, Value)` covers most fixtures, but an
// instant aggregation drops the two columns its result has no use for and
// projects `(Attributes, Value)`, and a subquery interposes its own
// `anchor_ts` between them. Pinning field positions therefore excluded
// every aggregation and every subquery from the parity layer — over a
// hundred fixtures — for a reason that had nothing to do with whether
// their answer could be checked, since a label set and a value is all the
// comparison needs.
//
// Resolving by name also fails LOUDLY on a projection this layer genuinely
// cannot read: a metadata catalog query projects a single `value` column
// that is a label value, not a sample, and no positional rule
// distinguishes that from a one-column sample projection.
type sampleColumns struct {
	name  int
	attrs int
	ts    int
	value int
}

// locateSampleColumns maps a projection's column names onto the Sample
// fields the comparison needs.
//
// Attributes and Value are required: without a label set there is no
// series identity to align the two answers on, and without a value there
// is nothing to compare. MetricName and TimeUnix are optional — a function
// that drops `__name__` projects no MetricName, and an instant query's
// timestamps do not participate in the comparison at all (see
// [comparesTimestamps]). Any other column, `anchor_ts` among them, is a
// scaffolding column of the emitted query rather than a field of the
// answer, and is ignored.
func locateSampleColumns(cols []string) (sampleColumns, error) {
	sc := sampleColumns{name: -1, attrs: -1, ts: -1, value: -1}
	for i, col := range cols {
		switch col {
		case colMetricName:
			sc.name = i
		case colAttributes:
			sc.attrs = i
		case colTimeUnix:
			sc.ts = i
		case colValue:
			sc.value = i
		}
	}
	if sc.attrs < 0 || sc.value < 0 {
		return sc, fmt.Errorf(
			"projection (%s) carries no %s and/or no %s column, so it is not a Sample-shaped "+
				"answer; this fixture's result cannot be parity-checked at the row layer",
			strings.Join(cols, ", "), colAttributes, colValue,
		)
	}
	return sc, nil
}

// referenceShapeOfExpectedRows converts cerberus's `expected_rows:` into
// the same flat shape the oracle returns, so the two can be compared
// element-wise.
//
// It errors rather than skipping on a row it cannot interpret. A fixture
// whose projection is not the canonical four-column Sample shape cannot be
// parity-checked at this layer, and silently comparing nothing would be
// the hollow green this mechanism exists to prevent.
func referenceShapeOfExpectedRows(rt *RoundTripSections, sc sampleColumns) ([]referenceSample, error) {
	out := make([]referenceSample, 0, len(rt.ExpectedRows))
	for i, row := range rt.ExpectedRows {
		if got, want := len(row), sc.arity(); got < want {
			return nil, fmt.Errorf(
				"expected_rows[%d] has %d column(s), too few for the projection this fixture "+
					"emits, whose Sample fields reach column %d", i, got, want,
			)
		}
		var name string
		var err error
		if sc.name >= 0 {
			if name, err = rowString(row[sc.name], i, colMetricName); err != nil {
				return nil, err
			}
		}
		attrs, err := rowAttrs(row[sc.attrs], i)
		if err != nil {
			return nil, err
		}
		var ts int64
		if sc.ts >= 0 {
			if ts, err = rowTimeMillis(row[sc.ts], i); err != nil {
				return nil, err
			}
		}
		value, err := rowFloat(row[sc.value], i)
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
