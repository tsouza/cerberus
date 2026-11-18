package traceql

import (
	"testing"

	"github.com/tsouza/cerberus/internal/schema"
)

// TestLeadingStageLowersAsImplicitSpansetFilter pins that a pipeline opening
// with a scalar filter, `by(...)` or `select(...)` — the three non-spanset
// stages Tempo's grammar admits first — lowers exactly as the same pipeline
// behind `{ }`. Reference Tempo hands such a stage every span of every trace,
// which is what `{ }` hands it. Each query used to answer 422.
func TestLeadingStageLowersAsImplicitSpansetFilter(t *testing.T) {
	t.Parallel()
	s := schema.DefaultOTelTraces()
	pairs := [][2]string{
		{`count() > 1`, `{ } | count() > 1`},
		{`count() > 1 | rate()`, `{ } | count() > 1 | rate()`},
		{`select(name)`, `{ } | select(name)`},
		{`select(name) | rate()`, `{ } | select(name) | rate()`},
		{`by(name)`, `{ } | by(name)`},
		{`by(name) | count() > 1`, `{ } | by(name) | count() > 1`},
		{`(count() > 1) && ({ })`, `({ } | count() > 1) && ({ })`},
		{`(select(name)) || ({ })`, `({ } | select(name)) || ({ })`},
	}
	for _, p := range pairs {
		got := lowerPlain(t, p[0], s)
		want := lowerPlain(t, p[1], s)
		if !got.Equal(want) {
			t.Errorf("%q did not lower equivalently to %q", p[0], p[1])
		}
	}
}
