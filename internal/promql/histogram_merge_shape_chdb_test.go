//go:build chdb

package promql_test

import (
	"context"
	"math"
	"testing"
	"time"

	promparser "github.com/prometheus/prometheus/promql/parser"

	"github.com/tsouza/cerberus/internal/chplan"
	"github.com/tsouza/cerberus/internal/chsql"
	"github.com/tsouza/cerberus/internal/promql"
	"github.com/tsouza/cerberus/internal/schema"
)

// mergedHistogramShape is the part of a merged native-histogram output row
// the merge-bound tests assert on: the scale the merge settled on, the
// positive ladder's bucket count at that scale, and the merged Count.
// Reading these three scalars back — rather than only `count()` over the
// merged row — is what lets a test tell a CORRECT compaction (every input
// bucket still lands in some output bucket, at the minimum downscale the
// width cap requires) from a merge that survived the budget guard by
// collapsing everything into one bucket.
type mergedHistogramShape struct {
	Scale int64
	Width int64
	Count float64
}

// readMergedHistogramShapes lowers query over [start, end] at step (0 for
// an instant query), emits it, and reads every output row's
// [mergedHistogramShape] in (MetricName, Attributes, TimeUnix) order. Only
// scalars leave ClickHouse: chdb-go's parquet driver cannot decode the
// merged row's Map/Array columns, so the width is read as `length(...)`.
func readMergedHistogramShapes(t *testing.T, fixture *chdbFixture, query string, start, end time.Time, step time.Duration, opts promql.LowerOpts) ([]mergedHistogramShape, error) {
	t.Helper()
	s := schema.DefaultOTelMetrics()
	p := promparser.NewParser(promparser.Options{})
	expr, err := p.ParseExpr(query)
	if err != nil {
		t.Fatalf("ParseExpr(%q): %v", query, err)
	}
	plan, err := promql.LowerAtRangeOpts(context.Background(), expr, s, start, end, step, opts)
	if err != nil {
		t.Fatalf("LowerAtRangeOpts(%q): %v", query, err)
	}
	sqlStr, args, err := chsql.Emit(context.Background(), plan)
	if err != nil {
		t.Fatalf("Emit(%q): %v", query, err)
	}
	wrapped := "SELECT toInt64(" + chplan.HistogramScaleColumn + "), toInt64(length(" + chplan.HistogramPositiveBucketCountsColumn + ")), toFloat64(" + chplan.HistogramCountColumn + ")" +
		" FROM (" + sqlStr + ") ORDER BY " + s.MetricNameColumn + ", " + s.AttributesColumn + ", " + s.TimestampColumn
	rows, qerr := fixture.db.Query(wrapped, args...)
	if qerr != nil {
		return nil, qerr
	}
	defer func() { _ = rows.Close() }()
	var shapes []mergedHistogramShape
	for rows.Next() {
		var shape mergedHistogramShape
		if err := rows.Scan(&shape.Scale, &shape.Width, &shape.Count); err != nil {
			t.Fatalf("scan merged histogram shape: %v", err)
		}
		shapes = append(shapes, shape)
	}
	if err := rows.Err(); err != nil {
		return nil, err
	}
	return shapes, nil
}

// readMergedHistogramShape is [readMergedHistogramShapes] for an instant
// query that must produce exactly one merged row.
func readMergedHistogramShape(t *testing.T, fixture *chdbFixture, query string, opts promql.LowerOpts) (mergedHistogramShape, error) {
	t.Helper()
	shapes, err := readMergedHistogramShapes(t, fixture, query, histogramMergeBoundEvalTS, histogramMergeBoundEvalTS, 0, opts)
	if err != nil {
		return mergedHistogramShape{}, err
	}
	if len(shapes) != 1 {
		t.Fatalf("query %q returned %d merged rows, want exactly 1", query, len(shapes))
	}
	return shapes[0], nil
}

// expectedRefinedScale is the scale [refinedMergeScaleExpr] must settle on
// for a merge whose natural (min(Scale)-only) width is naturalWidth at
// rawScale: one extra downscale step per halving needed to bring the width
// under [chplan.OTelExpoHistogramDefaultMaxSize], none when it already is.
func expectedRefinedScale(rawScale, naturalWidth int64) int64 {
	steps := max(int64(math.Ceil(math.Log2(float64(naturalWidth)/float64(chplan.OTelExpoHistogramDefaultMaxSize)))), 0)
	return rawScale - steps
}

// maxRefinedMergeWidth is the widest a compacted merge may be: the cap
// plus one, because a downscale step maps bucket index i to floor(i/2^k),
// and floor(hi/2^k) - floor(lo/2^k) + 1 can exceed (hi - lo + 1)/2^k by
// one when lo and hi fall on opposite sides of a 2^k boundary.
const maxRefinedMergeWidth = chplan.OTelExpoHistogramDefaultMaxSize + 1

// assertCompactedMerge checks that got is the CORRECT compaction of a merge
// of `rows` single-bucket rows at rawScale whose natural width is
// naturalWidth: the scale is exactly the minimum downscale the cap
// requires, the width respects the cap, and every input observation is
// still counted.
func assertCompactedMerge(t *testing.T, got mergedHistogramShape, rawScale, naturalWidth int64, wantCount float64) {
	t.Helper()
	if want := expectedRefinedScale(rawScale, naturalWidth); got.Scale != want {
		t.Fatalf("merged Scale = %d, want %d (raw %d, natural width %d, cap %d)", got.Scale, want, rawScale, naturalWidth, chplan.OTelExpoHistogramDefaultMaxSize)
	}
	if got.Width < 1 || got.Width > maxRefinedMergeWidth {
		t.Fatalf("merged width = %d, want within [1, %d]", got.Width, maxRefinedMergeWidth)
	}
	if got.Count != wantCount {
		t.Fatalf("merged Count = %v, want %v (every input observation must survive the compaction)", got.Count, wantCount)
	}
}
