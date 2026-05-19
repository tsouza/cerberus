package logql

import (
	"github.com/grafana/loki/v3/pkg/logql/syntax"

	"github.com/tsouza/cerberus/internal/chplan"
	"github.com/tsouza/cerberus/internal/schema"
)

// detectedLevelLabel is the synthesized label name Loki 3.x exposes for
// the "detected" log level — a normalised, lower-case severity drawn
// from the record's structured-metadata `detected_level` label or the
// record's `severity_text` / OTel `SeverityText` field.
//
// `level` is Loki's documented short alias — `pkg/distributor/field_detection.go`
// treats `level`, `LEVEL`, `Level`, `severity`, `SEVERITY`, `Severity`,
// `lvl`, `LVL`, and `Lvl` as the source labels detection scans. Once
// detection settles, downstream consumers see both `detected_level` and
// `level` referring to the same normalised value. Cerberus mirrors the
// alias surface here so a user query that uses `by (level)` /
// `without (level)` resolves against the synthesized SeverityText-derived
// expression rather than collapsing every record into an empty-value
// group (since cerberus's ResourceAttributes map has no bare `level` key).
//
// Upstream Loki's reference derivation
// (`github.com/grafana/loki/pkg/distributor/field_detection.go::extractLogLevel`)
// is layered:
//
//  1. If the record's StructuredMetadata already carries `detected_level`,
//     pass it through (after a lowercase normalise).
//  2. Else if a stream/structured-metadata label matching one of the
//     configured "level fields" (`level`, `severity`, `severity_text`, …)
//     exists, normalise that.
//  3. Else inspect the log line itself — try JSON/logfmt parsing first,
//     then fall back to a keyword scan (ERROR / WARN / INFO / DEBUG /
//     TRACE / FATAL / CRITICAL with word-boundary awareness).
//
// Cerberus emits a CH expression that covers step (1) — read through
// the OTel `SeverityText` column, normalised to Loki's canonical
// lowercase set. Production OTel-CH ingestion always populates
// SeverityText (and cerberus's loki-compat seeder mirrors that), so
// this single path catches the common case. The content-scan path
// (step 3 — JSON / logfmt / keyword scan against the log Body) and
// the structured-metadata flavour of step (1)
// (`LogAttributes['detected_level']` written by an upstream processor)
// are out of scope for this implementation: the seed datasets the
// harness exercises rely on SeverityText, and emitting the broader
// shape would double the CH expression size without changing the
// observable result.
const (
	detectedLevelLabel = "detected_level"
	// levelLabelAlias is the short alias Loki accepts as equivalent to
	// `detected_level` once severity detection settles. The aggregation
	// grouping path (by/without) routes both forms through the same
	// SeverityText-derived expression so a query that uses either form
	// returns the same series set. Label-filter / stream-selector matchers
	// keep the literal-key semantics so a `| logfmt | level="error"`
	// pipeline still resolves `level` against the parser-extracted map.
	levelLabelAlias = "level"
)

// isDetectedLevelLabel reports whether a matcher name targets the
// synthesized `detected_level` label by its canonical name. Label-filter
// and stream-selector matchers use this to route ONLY the
// `detected_level` form through the SeverityText-derived expression —
// the `level` short alias keeps the literal-key path so parser-extracted
// `level` (from `| logfmt`, `| json`, etc.) still resolves through
// labelsExpr.
func isDetectedLevelLabel(name string) bool {
	return name == detectedLevelLabel
}

// isDetectedLevelGroupingLabel reports whether `name` references the
// synthesized severity dimension in an aggregation `by(...)` / `without(...)`
// clause. Both `detected_level` and its `level` short alias resolve here
// because the downstream identity map (Project + RangeWindow) carries
// only the canonical `detected_level` key — never a raw `level` —
// regardless of whether the user wrote one form or the other. Matchers
// take the stricter [isDetectedLevelLabel] path because parser stages
// produce a real `level` key in the labels map that should win over
// the synthesized expression.
func isDetectedLevelGroupingLabel(name string) bool {
	return name == detectedLevelLabel || name == levelLabelAlias
}

// detectedLevelExpr returns the chplan expression that computes the
// synthesized `detected_level` value for the current row. The emitted
// shape normalises `SeverityText` to Loki's canonical lowercase level
// set via a `multiIf(...)` chain:
//
//	multiIf(
//	  lower(SeverityText) IN ('trace', 'trc'),                 'trace',
//	  lower(SeverityText) IN ('debug', 'dbg'),                 'debug',
//	  lower(SeverityText) IN ('info', 'inf', 'information'),   'info',
//	  lower(SeverityText) IN ('warn', 'wrn', 'warning'),       'warn',
//	  lower(SeverityText) IN ('error', 'err'),                 'error',
//	  lower(SeverityText) =  'critical',                        'critical',
//	  lower(SeverityText) =  'fatal',                           'fatal',
//	  lower(SeverityText))
//
// Inputs that don't match any group fall through to the lowercased
// original — matching upstream `normalizeLogLevel`'s default branch.
// Empty SeverityText emits the empty string (lower(”) = ”).
//
// chplan's typed `Expr` surface has no IN frag; the IN clauses above
// are encoded as left-folded OR-chains of equality comparisons. The
// emitted SQL is byte-identical to a hand-written `multiIf(... OR ...,
// ..., ... OR ...)` expression.
func detectedLevelExpr(s schema.Logs) chplan.Expr {
	return normaliseLevelExpr(&chplan.ColumnRef{Name: s.SeverityColumn})
}

// normaliseLevelExpr returns a CH `multiIf(...)` chain that maps the
// case-insensitive forms upstream Loki accepts (`err`/`error`,
// `warn`/`wrn`/`warning`, `inf`/`info`/`information`, `dbg`/`debug`,
// `trc`/`trace`, `critical`, `fatal`) onto Loki's canonical lowercase
// level strings. Inputs that don't match any group fall through to
// the lowercased original value — matching upstream `normalizeLogLevel`'s
// default branch.
func normaliseLevelExpr(value chplan.Expr) chplan.Expr {
	lowerValue := &chplan.FuncCall{
		Name: "lower",
		Args: []chplan.Expr{value},
	}

	// Each (variants, canonical) pair builds an OR-chain comparison.
	// Order matches upstream Loki's `normalizeLogLevel` switch:
	// trace / debug / info / warn / error / critical / fatal.
	type group struct {
		variants  []string
		canonical string
	}
	groups := []group{
		{[]string{"trace", "trc"}, "trace"},
		{[]string{"debug", "dbg"}, "debug"},
		{[]string{"info", "inf", "information"}, "info"},
		{[]string{"warn", "wrn", "warning"}, "warn"},
		{[]string{"error", "err"}, "error"},
		{[]string{"critical"}, "critical"},
		{[]string{"fatal"}, "fatal"},
	}

	args := make([]chplan.Expr, 0, len(groups)*2+1)
	for _, g := range groups {
		args = append(args, anyEqual(lowerValue, g.variants), &chplan.LitString{V: g.canonical})
	}
	// Default branch — pass through the lowercased original. Matches
	// upstream Loki's `default: return level` behaviour.
	args = append(args, lowerValue)

	return &chplan.FuncCall{Name: "multiIf", Args: args}
}

// anyEqual returns a left-folded OR-chain of `expr = variant`
// comparisons. Single-variant groups short-circuit to a plain
// `expr = variant`.
func anyEqual(expr chplan.Expr, variants []string) chplan.Expr {
	var out chplan.Expr
	for _, v := range variants {
		eq := &chplan.Binary{
			Op:    chplan.OpEq,
			Left:  expr,
			Right: &chplan.LitString{V: v},
		}
		if out == nil {
			out = eq
			continue
		}
		out = &chplan.Binary{Op: chplan.OpOr, Left: out, Right: eq}
	}
	return out
}

// withDetectedLevel wraps a labels-map expression so the result carries
// the synthesized `detected_level` key whenever the row's SeverityText
// is non-empty. The emitted shape is
//
//	mapConcat(
//	    <baseLabels>,
//	    mapFilter((k, v) -> v != '', map('detected_level', multiIf(...))))
//
// `mapFilter` drops the `detected_level` entry when the row's
// SeverityText is empty (the multiIf default branch returns
// `lower(”) = ”`), so rows without a severity-bearing column don't
// gain a spurious empty-string label. The shape mirrors Loki's stream-
// identity contract: `detected_level` is part of the output stream
// label set whenever the upstream row carries severity metadata, and
// absent otherwise.
//
// Used by both the log-stream projection (Lang.ProjectSamples for log
// queries, where the surfaced label splits the streams response into
// one Stream per detected_level) and the bare range-aggregation
// projection (lowerRangeAggregation when no by/without grouping, where
// the augmented identity drives the RangeWindow GROUP BY to emit one
// series per detected_level).
func withDetectedLevel(s schema.Logs, baseLabels chplan.Expr) chplan.Expr {
	levelMap := &chplan.FuncCall{
		Name: "map",
		Args: []chplan.Expr{
			&chplan.LitString{V: detectedLevelLabel},
			detectedLevelExpr(s),
		},
	}
	filtered := &chplan.FuncCall{
		Name: "mapFilter",
		Args: []chplan.Expr{
			&chplan.Lambda{
				Params: []string{"k", "v"},
				Body: &chplan.Binary{
					Op:    chplan.OpNe,
					Left:  &chplan.BareIdent{Name: "v"},
					Right: &chplan.LitString{V: ""},
				},
			},
			levelMap,
		},
	}
	return &chplan.FuncCall{
		Name: "mapConcat",
		Args: []chplan.Expr{baseLabels, filtered},
	}
}

// queryShouldSurfaceDetectedLevel reports whether the parsed LogQL
// expression should carry the synthesized `detected_level` label on its
// output stream identity. Used by the log-stream projection in
// [Lang.ProjectSamples] to gate the `withDetectedLevel` wrap.
//
// Reference Loki surfaces `detected_level` as a stream-identity label
// whenever the underlying records carry severity metadata that the
// detection pipeline can resolve to a canonical level value. The
// detection sources are (mirrored from
// `github.com/grafana/loki/pkg/distributor/field_detection.go::extractLogLevel`):
//
//  1. Stream / structured-metadata label named `detected_level` /
//     `level` / `severity` / `severity_text` / …
//  2. Parser-stage extraction (`| logfmt`, `| json`, `| regexp ...`,
//     `| pattern ...`, `| unpack`) that surfaces a `level` key from the
//     log line's structured payload.
//  3. Content scan over the log line (JSON / logfmt / keyword scan
//     for ERROR / WARN / INFO / DEBUG / TRACE / FATAL / CRITICAL).
//
// Cerberus's seeder always populates the OTel `SeverityText` column,
// so every log row that reaches the projection carries a non-empty
// severity value. The `mapFilter` inside [withDetectedLevel] drops the
// `detected_level` entry when `SeverityText` is empty, so the wrap is
// idempotent on rows without severity — there's no observable downside
// to applying it broadly.
//
// In light of that, the gate is permissive: every log-stream query
// triggers the wrap. The previous restrictive gate (only when the user
// referenced `detected_level` / `level` explicitly) caused the
// loki-compat `fast/basic-selectors.yaml` regressions where Loki splits
// the response into one Stream per detected_level even for queries
// that never name the label (bare selectors, line filters, label
// filters on unrelated keys). Returning true universally restores
// stream-identity parity with reference Loki.
//
// Pipe stages with parser-extracted `level` keys (`| logfmt`,
// `| json`, `| regexp ...`, `| pattern ...`, `| label_format ...`)
// keep going through their existing label-filter-context lookups —
// see [isDetectedLevelLabel] vs [isDetectedLevelGroupingLabel] for
// the matcher / grouping split. The wrap surfaces `detected_level`
// alongside any parser-derived keys; both can coexist in the output
// label map without conflict (Loki's reference response carries both
// when applicable).
//
// The function still walks the AST defensively so a `nil` expression
// (only the metric branch should hit ProjectSamples without an `expr`
// in [engine.Meta.Extra], but the log branch is the documented caller)
// returns false rather than panicking. The walk is otherwise a no-op
// for log queries — every log-shaped expression returns true. Metric
// queries don't reach this code path (the metric branch in
// [Lang.ProjectSamples] doesn't consult this gate).
func queryShouldSurfaceDetectedLevel(expr syntax.Expr) bool {
	// Every parsed log-stream expression triggers the wrap. The
	// signature stays AST-aware so future revisions can re-gate
	// specific shapes (e.g., `| drop detected_level` if/when cerberus
	// honours the drop-stage label set) without re-plumbing the
	// projection site. A nil expression (defensive: callers should
	// always populate `engine.Meta.Extra["expr"]`) opts out so the
	// wrap doesn't run against an empty AST.
	return expr != nil
}
