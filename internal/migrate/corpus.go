package migrate

import (
	"context"
	"encoding/json"
	"fmt"
	"os"
	"sort"
)

// CorpusVersion is the schema version stamped into every emitted Corpus. Bump
// it only on a breaking change to the JSON shape; readers reject a version they
// do not understand rather than silently misparsing.
const CorpusVersion = 1

// jsonIndent is the two-space indent used for the deterministic corpus
// marshalling, so `harvest --out corpus.json` diffs cleanly in version control.
const jsonIndent = "  "

// CorpusQuery is one harvested PromQL expression in the machine-readable
// corpus, tagged with where it came from, what produced it (record / alert /
// panel), and its source language.
type CorpusQuery struct {
	Expr   string `json:"expr"`
	Source string `json:"source"`
	Kind   string `json:"kind"`
	Lang   string `json:"lang"`
}

// Corpus is the versioned, machine-readable output of `migrate harvest`: every
// PromQL query the operator actually runs (from rule files and Grafana
// dashboards) plus a full, counted accounting of everything that was dropped.
// It marshals deterministically so the same inputs always produce byte-identical
// output.
type Corpus struct {
	Version int            `json:"version"`
	Queries []CorpusQuery  `json:"queries"`
	Skipped []SkippedEntry `json:"skipped"`
}

// BuildCorpus assembles a Corpus from harvested queries and skips, sorting both
// lists into a stable order so the JSON is deterministic regardless of the order
// sources were harvested in. Nil slices become empty slices so the JSON always
// carries `[]` rather than `null`.
func BuildCorpus(queries []HarvestedQuery, skipped []SkippedEntry) Corpus {
	cqs := make([]CorpusQuery, 0, len(queries))
	for _, q := range queries {
		// HarvestedQuery and CorpusQuery share the same fields (the latter adds
		// only json tags), so a struct conversion carries every field — including
		// Lang — without a field-by-field copy that could silently drop one.
		cqs = append(cqs, CorpusQuery(q))
	}
	sort.SliceStable(cqs, func(i, j int) bool {
		if cqs[i].Source != cqs[j].Source {
			return cqs[i].Source < cqs[j].Source
		}
		return cqs[i].Expr < cqs[j].Expr
	})

	sk := make([]SkippedEntry, len(skipped))
	copy(sk, skipped)
	sort.SliceStable(sk, func(i, j int) bool {
		if sk[i].Source != sk[j].Source {
			return sk[i].Source < sk[j].Source
		}
		return sk[i].Reason < sk[j].Reason
	})

	return Corpus{Version: CorpusVersion, Queries: cqs, Skipped: sk}
}

// Marshal renders the corpus as deterministic, indented JSON with a trailing
// newline, suitable for writing to a file that will be diffed in review.
func (c Corpus) Marshal() ([]byte, error) {
	data, err := json.MarshalIndent(c, "", jsonIndent)
	if err != nil {
		return nil, fmt.Errorf("migrate: marshal corpus: %w", err)
	}
	return append(data, '\n'), nil
}

// CorpusFileSource reads a corpus.json produced by `migrate harvest` and yields
// its queries and skips back as a CorpusSource, so `migrate explain --corpus`
// can dry-run a previously-harvested corpus. The skips recorded at harvest time
// are carried through unchanged so the explain report's skip count matches the
// harvest's.
type CorpusFileSource struct {
	Path string
}

// Harvest reads and decodes the corpus file. An unreadable or unparseable
// corpus file, or one whose version this build does not understand, is a hard
// error (the entire input is unusable) rather than a per-entry skip.
func (s CorpusFileSource) Harvest(_ context.Context) ([]HarvestedQuery, []SkippedEntry, error) {
	data, err := os.ReadFile(s.Path) //nolint:gosec // operator-supplied corpus path; offline CLI.
	if err != nil {
		return nil, nil, fmt.Errorf("migrate: read corpus %q: %w", s.Path, err)
	}
	var c Corpus
	if err := json.Unmarshal(data, &c); err != nil {
		return nil, nil, fmt.Errorf("migrate: parse corpus %q: %w", s.Path, err)
	}
	if c.Version != CorpusVersion {
		return nil, nil, fmt.Errorf("migrate: corpus %q has unsupported version %d (this build reads version %d)",
			s.Path, c.Version, CorpusVersion)
	}
	queries := make([]HarvestedQuery, 0, len(c.Queries))
	for _, q := range c.Queries {
		// Struct conversion (identical fields) carries Kind + Lang back out intact.
		queries = append(queries, HarvestedQuery(q))
	}
	return queries, c.Skipped, nil
}

// MultiSource harvests several CorpusSources in order and concatenates their
// queries and skips, letting `migrate harvest`/`explain` combine a rule-file
// source with a dashboard source in one pass.
type MultiSource []CorpusSource

// Harvest runs each underlying source in order, concatenating results. The first
// source that returns a hard error aborts the whole harvest.
func (m MultiSource) Harvest(ctx context.Context) ([]HarvestedQuery, []SkippedEntry, error) {
	var (
		queries []HarvestedQuery
		skipped []SkippedEntry
	)
	for _, src := range m {
		q, sk, err := src.Harvest(ctx)
		if err != nil {
			return nil, nil, err
		}
		queries = append(queries, q...)
		skipped = append(skipped, sk...)
	}
	return queries, skipped, nil
}
