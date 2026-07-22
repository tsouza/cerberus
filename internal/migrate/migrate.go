// Package migrate builds the offline "explain" report for cerberus's migration
// preview: for each PromQL query harvested from Prometheus rule files, it shows
// the exact ClickHouse SQL cerberus would run, the physical tables the query
// touches, and conservative offline risk flags — or marks the query
// UNSUPPORTED when the engine cannot emit it.
//
// The whole flow is read-only and offline. Corpus harvesting reads rule files;
// explanation is delegated to an Explainer (wired in cmd/migrate over
// engine.DryRunSQL, which never touches a ClickHouse connection). Nothing here
// estimates cardinality — row counts are data-dependent and cannot be known
// without a database, so the report says so explicitly rather than guessing.
package migrate

import (
	"context"
	"fmt"
	"io"
	"os"
	"path/filepath"
	"sort"
	"strings"

	"github.com/tsouza/cerberus/internal/chplan"
	"github.com/tsouza/cerberus/internal/promrules"
)

// Kind classifies a harvested query by the rule that produced it.
const (
	KindRecord = "record"
	KindAlert  = "alert"
)

// HarvestedQuery is one PromQL expression pulled from a corpus source, tagged
// with where it came from and whether it backs a recording or alerting rule.
type HarvestedQuery struct {
	Expr   string
	Source string // e.g. "rule:<file>/<group>/<name>"
	Kind   string // KindRecord | KindAlert
}

// SkippedEntry records a corpus entry the harvester could not turn into a query —
// an unreadable file, a YAML parse failure, a rule with no expression. Every
// dropped entry is reported here; the harvester never silently discards input.
type SkippedEntry struct {
	Source string
	Reason string
}

// CorpusSource yields the PromQL queries to explain, plus the entries it had to
// skip. Harvest is offline and side-effect-free beyond reading its inputs.
type CorpusSource interface {
	Harvest(ctx context.Context) ([]HarvestedQuery, []SkippedEntry, error)
}

// FileSource harvests queries from Prometheus rule files. RulePaths holds file
// paths or globs; each match is read and parsed as a rule file, and one
// HarvestedQuery is emitted per recording/alerting rule.
type FileSource struct {
	RulePaths []string
}

// Harvest expands every RulePaths entry, reads and parses each matched rule
// file, and emits one HarvestedQuery per rule. Anything it cannot use — a glob
// that matches nothing, an unreadable file, a YAML parse failure, a rule with
// no expression or no name — is recorded as a SkippedEntry rather than dropped.
func (s FileSource) Harvest(_ context.Context) ([]HarvestedQuery, []SkippedEntry, error) {
	var (
		queries []HarvestedQuery
		skipped []SkippedEntry
	)
	for _, pattern := range s.RulePaths {
		matches, err := filepath.Glob(pattern)
		if err != nil {
			skipped = append(skipped, SkippedEntry{Source: pattern, Reason: fmt.Sprintf("bad path pattern: %v", err)})
			continue
		}
		if len(matches) == 0 {
			skipped = append(skipped, SkippedEntry{Source: pattern, Reason: "no files matched"})
			continue
		}
		for _, file := range matches {
			data, err := os.ReadFile(file) //nolint:gosec // operator-supplied rule-file path; offline CLI.
			if err != nil {
				skipped = append(skipped, SkippedEntry{Source: file, Reason: fmt.Sprintf("read: %v", err)})
				continue
			}
			rg, err := promrules.Parse(data)
			if err != nil {
				skipped = append(skipped, SkippedEntry{Source: file, Reason: err.Error()})
				continue
			}
			q, sk := harvestFile(file, rg)
			queries = append(queries, q...)
			skipped = append(skipped, sk...)
		}
	}
	return queries, skipped, nil
}

// harvestFile flattens one parsed rule file's groups into HarvestedQuery /
// SkippedEntry entries. A rule with no name or no expression is skipped (counted),
// never emitted as an empty query.
func harvestFile(file string, rg promrules.RuleGroups) ([]HarvestedQuery, []SkippedEntry) {
	var (
		queries []HarvestedQuery
		skipped []SkippedEntry
	)
	for _, g := range rg.Groups {
		for _, r := range g.Rules {
			name, kind := r.Record, KindRecord
			if r.Alert != "" {
				name, kind = r.Alert, KindAlert
			}
			source := fmt.Sprintf("rule:%s/%s/%s", file, g.Name, name)
			if name == "" {
				skipped = append(skipped, SkippedEntry{Source: source, Reason: "rule has neither record nor alert name"})
				continue
			}
			if strings.TrimSpace(r.Expr) == "" {
				skipped = append(skipped, SkippedEntry{Source: source, Reason: "rule has an empty expr"})
				continue
			}
			queries = append(queries, HarvestedQuery{Expr: r.Expr, Source: source, Kind: kind})
		}
	}
	return queries, skipped
}

// Explanation is the offline result of dry-running one query through the engine:
// the parameterised ClickHouse SQL, the optimized plan, and any error. On error
// SQL is empty and Err is set; Plan may still be populated (the engine records
// the optimized plan even when the emit chokepoint rejects it).
type Explanation struct {
	SQL  string
	Plan chplan.Node
	Err  error
}

// Explainer turns a PromQL query into an Explanation, offline. The cmd/migrate
// adapter implements it over engine.DryRunSQL so the SQL is byte-identical to
// what the server would send to ClickHouse.
type Explainer interface {
	Explain(ctx context.Context, query string) Explanation
}

// Tables walks plan and returns the physical ClickHouse tables it scans —
// every *chplan.Scan's Table plus its UnionTables, qualified with the scan's
// Database when set — deduplicated and sorted for a stable report.
func Tables(plan chplan.Node) []string {
	seen := map[string]struct{}{}
	chplan.Walk(plan, func(n chplan.Node) bool {
		scan, ok := n.(*chplan.Scan)
		if !ok {
			return true
		}
		if scan.Table != "" {
			seen[qualify(scan.Database, scan.Table)] = struct{}{}
		}
		for _, t := range scan.UnionTables {
			seen[qualify(scan.Database, t)] = struct{}{}
		}
		return true
	})
	out := make([]string, 0, len(seen))
	for t := range seen {
		out = append(out, t)
	}
	sort.Strings(out)
	return out
}

// qualify renders a table reference as "database.table" when a database is set,
// otherwise the bare table name.
func qualify(db, table string) string {
	if db == "" {
		return table
	}
	return db + "." + table
}

// Lint returns conservative, offline risk flags read straight off the IR. It
// deliberately does NOT estimate cardinality — row counts depend on data the
// preview cannot see. It flags only structural fan-out that is a fact of the
// plan shape: a PromQL subquery RangeWindow evaluates its inner expression at
// many anchors per series, a work multiplier worth surfacing before cutover.
func Lint(plan chplan.Node) []string {
	seen := map[string]struct{}{}
	var risks []string
	add := func(msg string) {
		if _, ok := seen[msg]; ok {
			return
		}
		seen[msg] = struct{}{}
		risks = append(risks, msg)
	}
	chplan.Walk(plan, func(n chplan.Node) bool {
		rw, ok := n.(*chplan.RangeWindow)
		if !ok {
			return true
		}
		if rw.OuterRange > 0 && rw.Step > 0 {
			// anchors = OuterRange/Step + 1 (end-inclusive grid). This is a
			// structural fan-out factor from the plan, not a data estimate.
			anchors := int64(rw.OuterRange/rw.Step) + 1
			add(fmt.Sprintf("subquery fan-out: inner expression evaluated at %d anchors per series", anchors))
		}
		return true
	})
	return risks
}

// QueryReport is the per-query section of the report.
type QueryReport struct {
	Query       HarvestedQuery
	SQL         string
	Tables      []string
	Risks       []string
	Unsupported string // non-empty when the engine could not emit the query
}

// Report is the full offline explain result: one QueryReport per harvested
// query plus the entries the harvester skipped.
type Report struct {
	Queries []QueryReport
	Skipped []SkippedEntry
}

// BuildReport harvests the corpus, dry-runs every query through ex, and
// assembles the report. A query the engine cannot emit is marked Unsupported
// (with the engine's error) and the build keeps going — one unsupported query
// never aborts the preview.
func BuildReport(ctx context.Context, src CorpusSource, ex Explainer) (Report, error) {
	queries, skipped, err := src.Harvest(ctx)
	if err != nil {
		return Report{}, fmt.Errorf("migrate: harvest corpus: %w", err)
	}
	rep := Report{Skipped: skipped}
	for _, q := range queries {
		qr := QueryReport{Query: q}
		ex := ex.Explain(ctx, q.Expr)
		if ex.Err != nil {
			qr.Unsupported = ex.Err.Error()
		} else {
			qr.SQL = ex.SQL
			qr.Tables = Tables(ex.Plan)
			qr.Risks = Lint(ex.Plan)
		}
		rep.Queries = append(rep.Queries, qr)
	}
	return rep, nil
}

// Write renders the report as scannable, human-readable text. It leads with the
// honesty note that cardinality is unknown offline, then one block per query,
// then the skipped entries with their reasons.
func (r Report) Write(w io.Writer) error {
	bw := &errWriter{w: w}
	bw.printf("# cerberus migrate explain\n")
	bw.printf("#\n")
	bw.printf("# Offline preview: the exact ClickHouse SQL cerberus will run for each\n")
	bw.printf("# PromQL query, the physical tables it touches, and conservative IR risk\n")
	bw.printf("# flags. Row COUNT / cardinality is NOT knowable offline (it depends on\n")
	bw.printf("# data this tool never reads) — it is deliberately not estimated here.\n")
	bw.printf("#\n")
	bw.printf("# %d queries, %d skipped\n\n", len(r.Queries), len(r.Skipped))

	for _, q := range r.Queries {
		bw.printf("== [%s] %s\n", q.Query.Kind, q.Query.Source)
		bw.printf("   expr:  %s\n", q.Query.Expr)
		if q.Unsupported != "" {
			bw.printf("   UNSUPPORTED: %s\n", q.Unsupported)
			bw.printf("\n")
			continue
		}
		bw.printf("   sql:   %s\n", q.SQL)
		if len(q.Tables) > 0 {
			bw.printf("   tables: %s\n", strings.Join(q.Tables, ", "))
		}
		if len(q.Risks) > 0 {
			for _, risk := range q.Risks {
				bw.printf("   risk:  %s\n", risk)
			}
		} else {
			bw.printf("   risk:  none flagged offline (cardinality unknown)\n")
		}
		bw.printf("\n")
	}

	if len(r.Skipped) > 0 {
		bw.printf("== skipped (%d)\n", len(r.Skipped))
		for _, s := range r.Skipped {
			bw.printf("   %s: %s\n", s.Source, s.Reason)
		}
	}
	return bw.err
}

// errWriter collapses the repeated Fprintf error checks in Write into a single
// short-circuiting sink: once a write fails, later printf calls are no-ops and
// the first error is returned.
type errWriter struct {
	w   io.Writer
	err error
}

func (e *errWriter) printf(format string, args ...any) {
	if e.err != nil {
		return
	}
	_, e.err = fmt.Fprintf(e.w, format, args...)
}
