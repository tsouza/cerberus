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

// queryReferencesDetectedLevel reports whether the parsed LogQL
// expression references the synthesized severity dimension anywhere
// the user could observe it. Used by the log-stream projection in
// [Lang.ProjectSamples] to gate the `withDetectedLevel` wrap so a bare
// selector query (`{service="api"}`) doesn't gain a spurious
// `detected_level` stream label that would split its single Loki
// stream into one per severity (the loki-compat
// fast/basic-selectors.yaml regression: cerberus emitted 4 streams per
// service where reference Loki emits 1, because reference Loki only
// surfaces `detected_level` on queries that explicitly reference it).
//
// The detection covers four user-visible forms:
//
//  1. Stream-selector matchers — `{detected_level="error"}`,
//     `{level="warn"}`. The matcher name lands in
//     [syntax.MatchersExpr.Mts].
//  2. Pipe-stage label filters — `| detected_level="error"`,
//     `| level=~"warn|error"`. The filter exposes its referenced label
//     names via [log.LabelFilterer.RequiredLabelNames].
//  3. Vector-aggregation grouping — `sum by (detected_level) (...)`,
//     `sum by (level) (...)`, plus the `without (...)` mirror. The
//     grouping labels live in [syntax.VectorAggregationExpr.Grouping].
//  4. Range-aggregation grouping — `count_over_time({...}[5m]) by
//     (detected_level)`, plus `without`. The grouping labels live in
//     [syntax.RangeAggregationExpr.Grouping].
//
// Pipe stages that COULD produce a `level` key as part of their
// parser-extracted labels (`| logfmt`, `| json`, `| regexp ...`,
// `| pattern ...`, `| label_format ...`) don't count: the parser-
// extracted `level` is itself a label-filter-context lookup against
// the augmented labels map (see [isDetectedLevelLabel] vs
// [isDetectedLevelGroupingLabel]) and doesn't drive the synthesized
// SeverityText projection. Only an explicit reference at one of the
// four sites above pulls `detected_level` into stream identity.
//
// Both `detected_level` and its `level` short alias trigger the wrap —
// upstream Loki treats them as the same dimension once severity
// detection settles, and the stream-identity layer surfaces the
// canonical `detected_level` regardless of which form the user wrote.
func queryReferencesDetectedLevel(expr syntax.Expr) bool {
	if expr == nil {
		return false
	}
	var found bool
	expr.Walk(func(e syntax.Expr) bool {
		if found {
			return false
		}
		switch v := e.(type) {
		case *syntax.MatchersExpr:
			for _, m := range v.Mts {
				if isDetectedLevelGroupingLabel(m.Name) {
					found = true
					return false
				}
			}
		case *syntax.LabelFilterExpr:
			if v.LabelFilterer == nil {
				return true
			}
			for _, name := range v.RequiredLabelNames() {
				if isDetectedLevelGroupingLabel(name) {
					found = true
					return false
				}
			}
		case *syntax.VectorAggregationExpr:
			if v.Grouping != nil {
				for _, g := range v.Grouping.Groups {
					if isDetectedLevelGroupingLabel(g) {
						found = true
						return false
					}
				}
			}
		case *syntax.RangeAggregationExpr:
			if v.Grouping != nil {
				for _, g := range v.Grouping.Groups {
					if isDetectedLevelGroupingLabel(g) {
						found = true
						return false
					}
				}
			}
		}
		return true
	})
	return found
}
