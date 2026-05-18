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
// The cerberus first-pass implementation emits a CH expression that
// covers step (1) — read through the OTel `SeverityText` column,
// normalised to Loki's canonical lowercase set. Production OTel-CH
// ingestion always populates SeverityText (and cerberus's loki-compat
// seeder mirrors that), so this single path catches the common case.
//
// The content-scan path (step 3 — JSON / logfmt / keyword scan against
// the log Body) needs Loki's `pkg/pattern` drain machinery on the CH
// side; we ship it as a follow-up. The structured-metadata flavour of
// step (1) — `LogAttributes['detected_level']` written by an upstream
// processor — is left out of the first pass for the same reason: it
// would double the emitted CH expression size for a path the
// harness seed doesn't exercise yet.
const detectedLevelLabel = "detected_level"

// isDetectedLevelLabel reports whether a matcher name targets the
// synthesized `detected_level` label.
func isDetectedLevelLabel(name string) bool {
	return name == detectedLevelLabel
}

// detectedLevelExpr returns the chplan expression that computes the
// synthesized `detected_level` value for the current row. The first-pass
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
