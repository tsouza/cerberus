package chsql

import (
	"strings"
	"testing"

	"github.com/tsouza/cerberus/internal/chplan"
)

func TestHistogramQuantileValueFrag_DividesRankBeforeWidth(t *testing.T) {
	t.Parallel()

	b := NewBuilder()
	histogramQuantileValueFrag(&chplan.HistogramQuantile{
		Phi:                  0.5,
		BucketCountsColumn:   "BucketCounts",
		ExplicitBoundsColumn: "ExplicitBounds",
	})(b)

	if !strings.Contains(b.String(), " * (((0.5 *") {
		t.Fatalf("interpolation must divide the rank offset before multiplying by the bucket width:\n%s", b.String())
	}
}
