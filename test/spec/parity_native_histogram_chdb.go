//go:build chdb

package spec

import (
	"database/sql"
	"encoding/json"
	"fmt"
	"math"
	"regexp"
	"strings"
	"testing"

	"github.com/tsouza/cerberus/internal/testsql"
	oracle "github.com/tsouza/cerberus/test/spec/parityoracle/promql"
)

// nativeHistogramTable is the OTel-CH table holding exponential
// (native) histograms.
//
// # Why this one needs a new oracle SHAPE and the classic table did not
//
// A classic, explicit-bounds histogram IS a set of plain float series in
// Prometheus's data model — `<name>_bucket{le=…}` plus `_count` and
// `_sum` — so teaching the reader to explode those rows was enough (see
// [readSeededClassicHistograms]). An exponential histogram is not: both
// engines carry it as ONE sample whose value is a whole histogram, and
// there is no float series it decomposes into. Nothing could be compared
// until [oracle.Result] and [oracle.Point] could carry that value, which
// is what this file's reader fills in on the input side and
// [locateHistogramColumns] reads back on cerberus's side.
const nativeHistogramTable = "otel_metrics_exponential_histogram"

// The OTel-CH exponential-histogram columns. A fixture's seed declares
// only the ones its own query needs, so every one of them is read through
// a projection that tolerates its absence — see
// [nativeHistogramProjection].
const (
	colCount                = "Count"
	colSum                  = "Sum"
	colScale                = "Scale"
	colZeroCount            = "ZeroCount"
	colPositiveOffset       = "PositiveOffset"
	colPositiveBucketCounts = "PositiveBucketCounts"
	colNegativeOffset       = "NegativeOffset"
	colNegativeBucketCounts = "NegativeBucketCounts"
)

// readSeededNativeHistograms reads each seeded exponential-histogram row
// back out as one histogram-valued sample on its series.
//
// A fixture that never created the table is not an error: most of the
// corpus seeds only the tables its own query needs.
func readSeededNativeHistograms(
	db *sql.DB, rt *RoundTripSections, allow resourceAllowlist, byKey map[string]*oracle.Series,
	nameRestorer *metricNameRestorer, exactNames, deltaSeries map[string]bool,
) error {
	seedCols := testsql.SeedTableColumns(rt.Seed)[nativeHistogramTable]
	if len(seedCols) == 0 {
		return nil
	}

	//nolint:gosec // every fragment is chosen from this file's own
	// constants, keyed off the seed's declared columns; no fixture text
	// reaches the query.
	q := "SELECT MetricName, toJSONString(Attributes), " +
		resourceAttributesProjection(seedCols) + ", " + serviceNameProjection(seedCols) + ", " +
		"toUnixTimestamp64Milli(TimeUnix), " + temporalityProjection(seedCols) + ", " +
		strings.Join(nativeHistogramProjections(seedCols), ", ") +
		" FROM " + nativeHistogramTable + " ORDER BY MetricName, TimeUnix"
	rows, err := db.Query(q)
	if err != nil {
		return nil
	}
	defer func() { _ = rows.Close() }()

	countDeclared := hasColumn(seedCols, colCount)
	sumDeclared := hasColumn(seedCols, colSum)
	for rows.Next() {
		var name, attrsJSON, resAttrsJSON, serviceName string
		var tsMillis, temporality int64
		var count, sum, zeroCount float64
		var scale, positiveOffset, negativeOffset int64
		var positiveJSON, negativeJSON string
		if err := rows.Scan(
			&name, &attrsJSON, &resAttrsJSON, &serviceName, &tsMillis, &temporality,
			&count, &sum, &scale, &zeroCount,
			&positiveOffset, &positiveJSON, &negativeOffset, &negativeJSON,
		); err != nil {
			return err
		}
		lbls, err := labelsFromSeededRow(name, attrsJSON, resAttrsJSON, serviceName, allow)
		if err != nil {
			return err
		}
		positive, err := decodeBucketCounts(colPositiveBucketCounts, positiveJSON)
		if err != nil {
			return err
		}
		negative, err := decodeBucketCounts(colNegativeBucketCounts, negativeJSON)
		if err != nil {
			return err
		}

		narrowed, err := int32Columns(map[string]int64{
			colScale:          scale,
			colPositiveOffset: positiveOffset,
			colNegativeOffset: negativeOffset,
		})
		if err != nil {
			return err
		}

		h := &oracle.Histogram{
			Count: count,
			Sum:   sum,
			Scale: narrowed[colScale],
			// The zero threshold is not stored by OTel-CH and therefore
			// cannot be read; see [oracle.Histogram]'s ZeroThreshold
			// documentation for why 0 is the only honest value and what
			// that costs.
			ZeroThreshold:   0,
			ZeroCount:       zeroCount,
			PositiveOffset:  narrowed[colPositiveOffset],
			PositiveBuckets: positive,
			NegativeOffset:  narrowed[colNegativeOffset],
			NegativeBuckets: negative,
		}
		if !countDeclared {
			h.Count = totalObservations(h)
		}

		if exactNames == nil || exactNames[normalizeMetricName(name)] {
			if err := nameRestorer.record(lbls, name); err != nil {
				return err
			}
			key := labelKey(lbls)
			s, ok := byKey[key]
			if !ok {
				s = &oracle.Series{Labels: lbls}
				byKey[key] = s
			}
			s.Points = append(s.Points, oracle.Point{TMillis: tsMillis, Histogram: h})
			markDeltaSeries(deltaSeries, key, temporality)
		}

		// Cerberus exposes these documented float companions in addition to
		// the native-histogram series. Only a physical source column earns a
		// companion: a query cannot read a column a fixture did not seed.
		if countDeclared {
			if err := appendPoint(byKey, nameRestorer, lbls, name+countSuffix, nil, tsMillis, count); err != nil {
				return err
			}
			markDeltaSeries(deltaSeries, seriesKey(lbls, name+countSuffix, nil), temporality)
		}
		if sumDeclared {
			if err := appendPoint(byKey, nameRestorer, lbls, name+sumSuffix, nil, tsMillis, sum); err != nil {
				return err
			}
			markDeltaSeries(deltaSeries, seriesKey(lbls, name+sumSuffix, nil), temporality)
		}
	}
	return rows.Err()
}

// totalObservations is a histogram's own count of what it observed, read
// off its buckets.
//
// # Why this is a derivation and not an invention
//
// Half the exponential-histogram corpus declares no `Count` column at all,
// because the queries those fixtures ask — `histogram_quantile` above all
// — are answered from the bucket populations and cerberus's own lowering
// projects no Count for them either. The reference engine is not built
// that way: its histogram_quantile reads `Count` directly, so handing it a
// zero would make it answer a question about an empty histogram and every
// such fixture would "disagree" for a reason invented here.
//
// The OpenTelemetry data model requires `count` to equal the zero-bucket
// population plus every bucket count, so a histogram's buckets ALREADY
// state its total; recomputing it is reading the same fact a different
// way, not choosing a value. When the column IS declared it is used
// verbatim and this function is not consulted — so a seed whose stated
// Count contradicts its own buckets still reaches the reference engine as
// stated, and still disagrees if that contradiction changes the answer.
func totalObservations(h *oracle.Histogram) float64 {
	total := h.ZeroCount
	for _, c := range h.PositiveBuckets {
		total += c
	}
	for _, c := range h.NegativeBuckets {
		total += c
	}
	return total
}

// nativeHistogramProjections lists the SELECT expressions that read one
// exponential-histogram row, in the order [readSeededNativeHistograms]
// scans them.
//
// # Why an absent column reads as its zero rather than failing
//
// The corpus's seeds are minimal: a `histogram_quantile` fixture declares
// the bucket columns and nothing else, because that is all cerberus's own
// emitted SQL reads. An undeclared column therefore means the metric
// carries no such component — no negative buckets, no zero-bucket
// observations, scale 0 — which is exactly what OTel's own protobuf
// defaults say a field's absence means. Substituting that default reads
// the data model rather than papering over a gap.
//
// Nothing can be silently defaulted OUT of a comparison this way, because
// an undeclared column is one cerberus cannot read either: its lowering
// projects the column by name, so a query that needs it against a table
// that lacks it fails to execute long before this reader is reached. The
// one component that could not be defaulted honestly is `Count`, which the
// reference engine reads even when cerberus does not — see
// [totalObservations].
func nativeHistogramProjections(seedCols []string) []string {
	return []string{
		floatColumnProjection(seedCols, colCount),
		floatColumnProjection(seedCols, colSum),
		intColumnProjection(seedCols, colScale),
		floatColumnProjection(seedCols, colZeroCount),
		intColumnProjection(seedCols, colPositiveOffset),
		bucketCountsProjection(seedCols, colPositiveBucketCounts),
		intColumnProjection(seedCols, colNegativeOffset),
		bucketCountsProjection(seedCols, colNegativeBucketCounts),
	}
}

func floatColumnProjection(seedCols []string, col string) string {
	if !hasColumn(seedCols, col) {
		return "toFloat64(0)"
	}
	return "toFloat64(" + col + ")"
}

func intColumnProjection(seedCols []string, col string) string {
	if !hasColumn(seedCols, col) {
		return "toInt64(0)"
	}
	return "toInt64(" + col + ")"
}

func bucketCountsProjection(seedCols []string, col string) string {
	if !hasColumn(seedCols, col) {
		return "'[]'"
	}
	return "toJSONString(" + col + ")"
}

// histogramQuantilePattern matches the two PromQL functions whose answer
// is a position ON the histogram's value axis, and therefore the only ones
// whose answer can be the zero threshold itself.
var histogramQuantilePattern = regexp.MustCompile(`\bhistogram_quantiles?\b`)

// checkZeroBucketScope requires [ScopeExceptZeroBucket] to be carried by
// exactly the fixtures that earn it — declared where it is due, and absent
// everywhere else.
//
// # What earns it
//
// A native histogram's quantile is a bucket boundary or an interpolation
// between two of them, and those boundaries are powers of the schema base,
// which is never zero. The single exception is the zero bucket, whose
// bounds are ±ZeroThreshold. So a `histogram_quantile` over an
// exponential-histogram seed answering EXACTLY 0 is, precisely, one whose
// answer came out of the zero band — and the zero threshold is the one
// quantity neither side can read, because upstream OTel-CH stores no such
// column and both sides must invent the same 0. Those fixtures agree by
// shared assumption rather than by independent computation, which is worth
// recording rather than counting as coverage.
//
// # Why the check runs in BOTH directions
//
// Requiring the scope where it is earned is the half that keeps a vacuous
// green from being silently counted. Forbidding it everywhere else is the
// half that stops it becoming an escape hatch: a scope value that can be
// added to any fixture whose comparison is inconvenient is an
// expected-failure list wearing a different name, which invariant 7
// forbids. Neither direction is optional, and a fixture cannot satisfy
// this by being left out of the parity layer — it is only reached for
// fixtures already enrolled.
func checkZeroBucketScope(
	t *testing.T, c *Case, p *Parity, rt *RoundTripSections, query string, got []referenceSample,
) {
	t.Helper()

	earned := false
	if len(testsql.SeedTableColumns(rt.Seed)[nativeHistogramTable]) > 0 &&
		histogramQuantilePattern.MatchString(query) {
		for _, s := range got {
			if s.Histogram == nil && s.Value == 0 {
				earned = true
				break
			}
		}
	}

	switch declared := p.Scope == ScopeExceptZeroBucket; {
	case earned && !declared:
		t.Errorf(
			"fixture %s: its quantile answers exactly 0, which for a native histogram can only "+
				"come from the zero band, so the answer IS the zero threshold — a value neither "+
				"side can read and both must invent as 0. Declare `scope: %s` so this agreement "+
				"is recorded rather than counted as coverage.",
			c.Name, ScopeExceptZeroBucket,
		)
	case !earned && declared:
		t.Errorf(
			"fixture %s declares `scope: %s`, but its answer does not depend on the invented zero "+
				"threshold: no sample of it is a zero-band quantile over an exponential-histogram "+
				"seed. Compare it in full — this scope records one specific un-oracleable axis and "+
				"is not a way to narrow a comparison.",
			c.Name, ScopeExceptZeroBucket,
		)
	}
}

// int32Columns narrows the scale and bucket offsets, which ClickHouse
// hands back as 64-bit integers, to the 32-bit width their OTel-CH
// declarations give them.
//
// A value outside that range is an error rather than a truncation. Every
// one of these three numbers is an EXPONENT: a wrapped scale or offset
// does not shift a histogram slightly, it relocates every bucket to a
// different power of the base, and it would do so while still producing a
// perfectly well-formed histogram for the comparison to pass or fail on.
func int32Columns(wide map[string]int64) (map[string]int32, error) {
	out := make(map[string]int32, len(wide))
	for col, v := range wide {
		if v < math.MinInt32 || v > math.MaxInt32 {
			return nil, fmt.Errorf(
				"%s is %d, outside the Int32 range its column is declared with", col, v,
			)
		}
		out[col] = int32(v)
	}
	return out, nil
}

func decodeBucketCounts(col, encoded string) ([]float64, error) {
	var counts []float64
	if err := json.Unmarshal([]byte(encoded), &counts); err != nil {
		return nil, fmt.Errorf("decode %s %q: %w", col, encoded, err)
	}
	return counts, nil
}

// histogramColumns is where each field of a native-histogram sample sits
// inside one `expected_rows:` row.
//
// These are cerberus's own projection aliases, located by NAME for the
// reason [sampleColumns] gives — the emitter chooses the order, and a
// positional rule would silently read the wrong field the first time it
// changed.
type histogramColumns struct {
	count           int
	sum             int
	scale           int
	zeroThreshold   int
	zeroCount       int
	positiveOffset  int
	positiveBuckets int
	negativeOffset  int
	negativeBuckets int
}

// The column names cerberus's histogram-shaped projection emits. They are
// declared here rather than imported from internal/chplan for the same
// reason the label mapping is written out in this package: the comparison
// must be able to notice a projection that stopped emitting one of them,
// and a shared constant cannot.
const (
	colHistogramCount           = "HistogramCount"
	colHistogramSum             = "HistogramSum"
	colHistogramScale           = "HistogramScale"
	colHistogramZeroThreshold   = "HistogramZeroThreshold"
	colHistogramZeroCount       = "HistogramZeroCount"
	colHistogramPositiveOffset  = "HistogramPositiveOffset"
	colHistogramPositiveBuckets = "HistogramPositiveBucketCounts"
	colHistogramNegativeOffset  = "HistogramNegativeOffset"
	colHistogramNegativeBuckets = "HistogramNegativeBucketCounts"
)

// colSetOpMixedIsHistogram is the per-row discriminator a Mixed
// VectorSetOr's fourteen-column projection carries (cerberus issue
// #2330): 1 when the row's Histogram* columns are the real answer, 0
// when they are the float arm's typed placeholders and Value is the real
// answer instead. Mirrors internal/chsql/vector_set_op.go's
// setOpMixedIsHistogramCol literal, duplicated here for the same reason
// the Histogram*Column names above are — this package cannot import
// internal/chsql's unexported constant, and locating by name is the
// established convention. Every row of a Mixed projection carries all
// nine Histogram* columns (real or placeholder), so [locateHistogramColumns]
// alone cannot tell a real histogram row from a placeholder one; only this
// discriminator can.
const colSetOpMixedIsHistogram = "_setop_is_histogram"

// locateHistogramColumns maps a projection's column names onto the fields
// of a native-histogram sample, or reports nil when the projection carries
// no histogram at all — the ordinary case, since most of the corpus
// answers with floats.
//
// A projection carrying SOME but not all of them is an error rather than a
// partial read. Every one of these columns is emitted together by one
// emitter, so a subset means that emitter changed shape, and guessing
// which fields the survivors correspond to is exactly the positional
// assumption locating by name exists to avoid.
func locateHistogramColumns(cols []string) (*histogramColumns, error) {
	hc := histogramColumns{
		count: -1, sum: -1, scale: -1, zeroThreshold: -1, zeroCount: -1,
		positiveOffset: -1, positiveBuckets: -1, negativeOffset: -1, negativeBuckets: -1,
	}
	fields := map[string]*int{
		colHistogramCount:           &hc.count,
		colHistogramSum:             &hc.sum,
		colHistogramScale:           &hc.scale,
		colHistogramZeroThreshold:   &hc.zeroThreshold,
		colHistogramZeroCount:       &hc.zeroCount,
		colHistogramPositiveOffset:  &hc.positiveOffset,
		colHistogramPositiveBuckets: &hc.positiveBuckets,
		colHistogramNegativeOffset:  &hc.negativeOffset,
		colHistogramNegativeBuckets: &hc.negativeBuckets,
	}
	found := 0
	for i, col := range cols {
		if slot, ok := fields[col]; ok {
			*slot = i
			found++
		}
	}
	switch found {
	case 0:
		return nil, nil
	case len(fields):
		return &hc, nil
	default:
		return nil, fmt.Errorf(
			"projection (%s) carries %d of the %d columns a native-histogram answer is made of, "+
				"so its histogram fields cannot be located by name",
			strings.Join(cols, ", "), found, len(fields),
		)
	}
}

// indices returns every located column index, for the arity check that
// proves each one is in range on a row.
func (hc *histogramColumns) indices() []int {
	if hc == nil {
		return nil
	}
	return []int{
		hc.count, hc.sum, hc.scale, hc.zeroThreshold, hc.zeroCount,
		hc.positiveOffset, hc.positiveBuckets, hc.negativeOffset, hc.negativeBuckets,
	}
}

// histogramFromExpectedRow reads cerberus's own answer for one
// histogram-valued sample out of its pinned `expected_rows:` row.
func histogramFromExpectedRow(row []any, hc *histogramColumns, i int) (*oracle.Histogram, error) {
	count, err := rowFloat(row[hc.count], i)
	if err != nil {
		return nil, err
	}
	sum, err := rowFloat(row[hc.sum], i)
	if err != nil {
		return nil, err
	}
	scale, err := rowInt32(row[hc.scale], i, colHistogramScale)
	if err != nil {
		return nil, err
	}
	zeroThreshold, err := rowFloat(row[hc.zeroThreshold], i)
	if err != nil {
		return nil, err
	}
	zeroCount, err := rowFloat(row[hc.zeroCount], i)
	if err != nil {
		return nil, err
	}
	positiveOffset, err := rowInt32(row[hc.positiveOffset], i, colHistogramPositiveOffset)
	if err != nil {
		return nil, err
	}
	positive, err := rowFloats(row[hc.positiveBuckets], i, colHistogramPositiveBuckets)
	if err != nil {
		return nil, err
	}
	negativeOffset, err := rowInt32(row[hc.negativeOffset], i, colHistogramNegativeOffset)
	if err != nil {
		return nil, err
	}
	negative, err := rowFloats(row[hc.negativeBuckets], i, colHistogramNegativeBuckets)
	if err != nil {
		return nil, err
	}
	return &oracle.Histogram{
		Count:           count,
		Sum:             sum,
		Scale:           scale,
		ZeroThreshold:   zeroThreshold,
		ZeroCount:       zeroCount,
		PositiveOffset:  positiveOffset,
		PositiveBuckets: positive,
		NegativeOffset:  negativeOffset,
		NegativeBuckets: negative,
	}, nil
}

// rowInt32 reads a bucket index or scale, which are whole numbers in both
// data models. A non-integral cell is an error rather than a truncation:
// a fractional scale would mean the projection is not carrying what its
// name says it is.
func rowInt32(cell any, row int, col string) (int32, error) {
	f, err := rowFloat(cell, row)
	if err != nil {
		return 0, err
	}
	if f != math.Trunc(f) || f < math.MinInt32 || f > math.MaxInt32 {
		return 0, fmt.Errorf(
			"expected_rows[%d].%s is %v, want a 32-bit whole number", row, col, f,
		)
	}
	return int32(f), nil
}

func rowFloats(cell any, row int, col string) ([]float64, error) {
	items, ok := cell.([]any)
	if !ok {
		return nil, fmt.Errorf("expected_rows[%d].%s is %T, want an array", row, col, cell)
	}
	out := make([]float64, 0, len(items))
	for _, item := range items {
		f, err := rowFloat(item, row)
		if err != nil {
			return nil, fmt.Errorf("%s: %w", col, err)
		}
		out = append(out, f)
	}
	return out, nil
}
