package routerrules

import "context"

// memCorpusSource is an in-memory CorpusSource over a slice of benchmark rows.
// It exists so the effectiveness benchmark can resolve params and evaluate rules
// against a generated labeled corpus with no JSONL round-trip, while taking the
// EXACT same in-Go aggregation + evaluation path the JSONL source uses (the
// shared inGoAggregate / inGoEvalRule helpers). That shared path is what keeps
// the benchmark honest: it measures the same matcher and the same quantileExact
// formula the production JSONL backend runs, not a parallel re-implementation.
type memCorpusSource struct {
	rows []BenchRow
}

// NewMemCorpusSource builds an in-memory source over benchmark rows.
func NewMemCorpusSource(rows []BenchRow) CorpusSource {
	return &memCorpusSource{rows: rows}
}

// stream feeds every row through fn as a decoded corpusRow, the shape both
// in-Go aggregation helpers consume.
func (s *memCorpusSource) stream(fn func(corpusRow) error) error {
	for _, r := range s.rows {
		if err := fn(r.toCorpusRow()); err != nil {
			return err
		}
	}
	return nil
}

func (s *memCorpusSource) Aggregate(_ context.Context, spec AggSpec) (Value, error) {
	return inGoAggregate(s.stream, spec)
}

func (s *memCorpusSource) EvalRule(_ context.Context, q RuleQuery) ([]GroupResult, error) {
	return inGoEvalRule(s.stream, q)
}
