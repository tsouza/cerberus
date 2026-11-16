//go:build agpl_oracle

package agpl_oracle

import (
	"testing"

	tempo "github.com/grafana/tempo/pkg/traceql"

	ast "github.com/tsouza/cerberus/internal/traceql/ast"
)

// validateCorpus holds queries both grammars accept, so the verdict compared
// is the static validation the reference's query frontend runs after parsing
// (ParseNoOptimizations, then Validate). It covers the scalar-filter operand
// shapes and the topk / bottomk limit, at each position a pipeline carries
// them, and the order in which rules report when several apply.
var validateCorpus = []string{
	// The operand shapes a scalar filter admits.
	`{ } | count() > 1`,
	`{ } | count() > -1`,
	`{ } | avg(duration) > 1s`,
	`{ } | count() = (1)`,
	`{ } | count() + 1 > 2`,
	`{ } | count() * 2 > 1`,
	`{ } | 1 > count()`,
	`{ } | 1 = 1`,
	`{ } | 1 + 1 > count()`,
	`{ } | count() > 1 + 1`,
	`{ } | count() > (1 + 1)`,
	`{ } | count() > count()`,
	`{ } | count() > count() + 1s`,
	`{ } | max(duration) - min(duration) >= 0`,
	`{ } | max(duration) > avg(duration)`,
	// Each position a scalar filter takes.
	`({ } | 1 > count()) && ({ } | count() > 1)`,
	`({ } | count() > 1) >> ({ } | count() + 1 > 1)`,
	`{ } | count() > 1 | count() + 1 > 2`,
	`{ } | count() + 1 > 2 | rate()`,
	`{ } | count() > 1 + 1 | select(.a)`,
	// Which rule reports when several apply.
	`{ } | count() + 1 > avg(1)`,
	`{ } | avg(1) + 1 > 2`,
	`{ } | count() + 1 > 1 | by(1)`,
	`{ } | by(1) | count() + 1 > 1`,
	// The topk / bottomk limit, alone and in a chain of second stages.
	`{ } | rate() | topk(1)`,
	`{ } | rate() | bottomk(1)`,
	`{ } | rate() | topk(0)`,
	`{ } | rate() | bottomk(0)`,
	`{ } | rate() | topk(1) | bottomk(0)`,
	`{ } | rate() by(.a) | topk(0) > 2`,
	`{ } | rate() | topk(0) | topk(0)`,
	`{ } | compare({ .a = 1 }) | topk(0)`,
	`{ } | quantile_over_time(duration, 2) | topk(0)`,
	`{ } | count() + 1 > 2 | rate() | topk(0)`,
}

// TestValidate_AB pins cerberus's verdict and message for each query in
// validateCorpus to the reference frontend's: accepted by both, or rejected
// by both with the same `invalid TraceQL query: ` message.
func TestValidate_AB(t *testing.T) {
	for _, q := range validateCorpus {
		t.Run(q, func(t *testing.T) {
			ref, err := tempo.ParseNoOptimizations(q)
			if err != nil {
				t.Fatalf("reference grammar rejects %q (%v): the corpus holds only queries both grammars accept", q, err)
			}
			want := ""
			if verr := tempo.Validate(ref); verr != nil {
				want = "invalid TraceQL query: " + verr.Error()
			}
			got := ""
			if _, perr := ast.Parse(q); perr != nil {
				got = perr.Error()
			}
			if got != want {
				t.Errorf("ast.Parse(%q) = %q, reference = %q", q, got, want)
			}
		})
	}
}
