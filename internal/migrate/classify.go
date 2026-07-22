package migrate

import (
	"encoding/json"
	"fmt"
	"io"
)

// Bucket names for `migrate classify`. Classification is a re-view of the exact
// offline explain pipeline (parse → lower → emit via engine.DryRunSQL),
// specialised into a ledger of how cleanly each corpus query maps onto
// cerberus's PromQL support:
//
//   - BucketSupported   — the query PARSES, LOWERS, and EMITS ClickHouse SQL
//     cleanly. This is the "PromQL-pure / rewritable" bucket: cerberus has a
//     translation for it. It does NOT assert result parity (see the honesty
//     note in Write): only `migrate verify` proves the numbers match.
//   - BucketUnsupported — the offline pipeline rejected the query (a parse,
//     lower, or emit error). This is the "no-equivalent" bucket; the offending
//     construct/error is captured verbatim in ClassifiedQuery.Construct so the
//     operator knows exactly what to rewrite before cutover.
//
// "risky" is not a third bucket — it is a flag on a SUPPORTED query that also
// carries at least one offline Lint fan-out risk. It is counted separately so a
// clean-but-expensive query is visible without being demoted out of supported.
const (
	BucketSupported   = "supported"
	BucketUnsupported = "unsupported"
)

// ClassifiedQuery is one corpus query placed in a bucket. Expr/Source/Kind are
// carried from the harvested query so the JSON ledger is self-contained.
// Construct is the engine's rejection message — the offending construct — when
// Bucket is unsupported, and empty otherwise. Risky is set on a supported query
// that carries at least one offline Lint risk flag; Risks lists them.
type ClassifiedQuery struct {
	Expr      string   `json:"expr"`
	Source    string   `json:"source"`
	Kind      string   `json:"kind"`
	Bucket    string   `json:"bucket"`
	Construct string   `json:"construct,omitempty"`
	Risky     bool     `json:"risky"`
	Risks     []string `json:"risks,omitempty"`
}

// BucketCounts is the per-bucket tally. Total is every classified query;
// Supported + Unsupported == Total. Risky is a subset of Supported (a supported
// query that also carries an offline fan-out risk), so it is NOT added to Total.
type BucketCounts struct {
	Total       int `json:"total"`
	Supported   int `json:"supported"`
	Unsupported int `json:"unsupported"`
	Risky       int `json:"risky"`
}

// Classification is the full classify ledger: per-bucket counts, one
// ClassifiedQuery per harvested query, and the entries the harvester skipped
// (carried straight through from the report so the skip count never gets lost).
type Classification struct {
	Counts  BucketCounts      `json:"counts"`
	Queries []ClassifiedQuery `json:"queries"`
	Skipped []SkippedEntry    `json:"skipped"`
}

// Classify turns an already-built explain Report into a bucketed ledger. It does
// not re-run the engine — it reads the outcome the report already recorded: a
// query with a non-empty Unsupported field lands in the unsupported bucket
// (construct = the engine error), everything else is supported, and a supported
// query carrying offline Lint risks is additionally flagged risky.
func Classify(rep Report) Classification {
	cl := Classification{
		Queries: make([]ClassifiedQuery, 0, len(rep.Queries)),
		Skipped: rep.Skipped,
	}
	for _, q := range rep.Queries {
		cq := ClassifiedQuery{
			Expr:   q.Query.Expr,
			Source: q.Query.Source,
			Kind:   q.Query.Kind,
		}
		if q.Unsupported != "" {
			cq.Bucket = BucketUnsupported
			cq.Construct = q.Unsupported
			cl.Counts.Unsupported++
		} else {
			cq.Bucket = BucketSupported
			cl.Counts.Supported++
			if len(q.Risks) > 0 {
				cq.Risky = true
				cq.Risks = q.Risks
				cl.Counts.Risky++
			}
		}
		cl.Queries = append(cl.Queries, cq)
	}
	cl.Counts.Total = len(rep.Queries)
	return cl
}

// Write renders the classification as scannable text. It leads with the honesty
// note — "supported" means the query TRANSLATES, not that cerberus returns the
// same numbers as Prometheus — then the per-bucket counts, then one block per
// bucket, then the skipped entries with their reasons.
func (c Classification) Write(w io.Writer) error {
	bw := &errWriter{w: w}
	bw.printf("# cerberus migrate classify\n")
	bw.printf("#\n")
	bw.printf("# Buckets each corpus query by how cleanly it maps onto cerberus's PromQL\n")
	bw.printf("# support, using the exact offline explain pipeline (parse -> lower -> emit):\n")
	bw.printf("#   supported   — parses, lowers, and EMITS ClickHouse SQL (PromQL-pure / rewritable)\n")
	bw.printf("#   unsupported — the offline pipeline rejected it (no-equivalent; construct named)\n")
	bw.printf("#   risky       — a SUPPORTED query that also carries an offline fan-out risk flag\n")
	bw.printf("#\n")
	bw.printf("# HONESTY: \"supported\" means the query TRANSLATES to SQL, NOT that cerberus\n")
	bw.printf("# returns the same numbers as Prometheus — only `migrate verify` proves parity.\n")
	bw.printf("#\n")
	bw.printf("# %d queries: %d supported (%d risky), %d unsupported; %d skipped\n\n",
		c.Counts.Total, c.Counts.Supported, c.Counts.Risky, c.Counts.Unsupported, len(c.Skipped))

	bw.printf("== supported (%d)\n", c.Counts.Supported)
	for _, q := range c.Queries {
		if q.Bucket != BucketSupported {
			continue
		}
		bw.printf("   [%s] %s\n", q.Kind, q.Source)
		bw.printf("     expr: %s\n", q.Expr)
		for _, risk := range q.Risks {
			bw.printf("     RISKY: %s\n", risk)
		}
	}

	bw.printf("\n== unsupported (%d)\n", c.Counts.Unsupported)
	for _, q := range c.Queries {
		if q.Bucket != BucketUnsupported {
			continue
		}
		bw.printf("   [%s] %s\n", q.Kind, q.Source)
		bw.printf("     expr:      %s\n", q.Expr)
		bw.printf("     construct: %s\n", q.Construct)
	}

	if len(c.Skipped) > 0 {
		bw.printf("\n== skipped (%d)\n", len(c.Skipped))
		for _, s := range c.Skipped {
			bw.printf("   %s: %s\n", s.Source, s.Reason)
		}
	}
	return bw.err
}

// WriteJSON renders the classification as deterministic, indented JSON with a
// trailing newline. Nil slices become empty slices so the ledger always carries
// `[]` rather than `null`, matching the corpus JSON convention.
func (c Classification) WriteJSON(w io.Writer) error {
	if c.Queries == nil {
		c.Queries = []ClassifiedQuery{}
	}
	if c.Skipped == nil {
		c.Skipped = []SkippedEntry{}
	}
	data, err := json.MarshalIndent(c, "", jsonIndent)
	if err != nil {
		return fmt.Errorf("migrate: marshal classification: %w", err)
	}
	if _, err := w.Write(append(data, '\n')); err != nil {
		return fmt.Errorf("migrate: write classification: %w", err)
	}
	return nil
}
