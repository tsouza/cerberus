package logql

import (
	"github.com/tsouza/cerberus/internal/chplan"
	"github.com/tsouza/cerberus/internal/schema"
)

// detectedLevelLabel is the synthesized label name Loki 3.x exposes for
// the "detected" log level — a normalised, lower-case severity drawn
// from the record's structured-metadata `detected_level` label or the
// record's `severity_text` / OTel `SeverityText` field.
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
const detectedLevelLabel = "detected_level"

// isDetectedLevelLabel reports whether a matcher name targets the
// synthesized `detected_level` label.
func isDetectedLevelLabel(name string) bool {
	return name == detectedLevelLabel
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
