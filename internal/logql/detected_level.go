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
// Cerberus emits a CH `multiIf(...)` precedence cascade (see
// [detectedLevelSourceExpr]) that covers steps (1) and (2): the
// structured-metadata `detected_level` key wins, then the allowed
// level/severity keys ([allowedLevelFields]) in the LogAttributes map,
// then the dedicated OTel `SeverityText` column as the terminal source.
// The resolved value is normalised to Loki's canonical lowercase set
// ([normaliseLevelExpr]); an all-empty resolution maps to `unknown`,
// matching reference Loki's `constants.LogLevelUnknown` stamping.
//
// The content-scan path (step 3 — JSON / logfmt / keyword scan against
// the log Body) remains out of scope: it would require parsing the
// (arbitrarily large) line body inside the level expression, where the
// OTel-CH model already routes a record's level into SeverityText or a
// structured-metadata key at ingest. A record whose level lives only in
// the body text maps to `unknown` here, where a reference Loki with
// content discovery enabled would refine it.
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

// allowedLevelFields is the structured-metadata key set cerberus scans
// for a log level before falling back to the dedicated SeverityText
// column. It is the OTel-relevant subset of reference Loki's
// `validation.DefaultAllowedLevelFields` (pkg/validation/limits.go): a
// record whose LogAttributes map carries any of these keys with a
// non-empty value resolves `detected_level` from that value
// (normalised), matching the distributor-side `extractLogLevel`
// precedence (pkg/distributor/field_detection.go).
//
// Upstream's full list also enumerates pure case variants (`LEVEL`,
// `Level`, `Lvl`, `Severity`, …). Those are dropped here on purpose:
// OTel structured metadata reaches the OTel-CH LogAttributes map after
// the collector's attribute processing, where level/severity keys are
// conventionally lowercase, and `normaliseLevelExpr` is already
// case-insensitive on the VALUE (it lowercases before matching). Adding
// a dozen case-variant KEY probes would multiply the per-query map
// lookups (and the emitted SQL size) for cases OTel-CH data doesn't
// produce. `SeverityText` is upstream's final map key too; cerberus
// resolves it from the dedicated column instead, as the terminal branch
// of [detectedLevelSourceExpr].
var allowedLevelFields = []string{
	"level",
	"log.level",
	"severity",
	"severity_text",
}

// detectedLevelExpr returns the chplan expression that computes the
// synthesized `detected_level` value for the current row. The source
// value is resolved with reference Loki's `extractLogLevel` precedence
// (see [detectedLevelSourceExpr]) and then normalised to Loki's
// canonical lowercase level set via [normaliseLevelExpr]'s `multiIf(...)`
// chain:
//
//	multiIf(
//	  lower(src) IN ('trace', 'trc'),                 'trace',
//	  lower(src) IN ('debug', 'dbg'),                 'debug',
//	  lower(src) IN ('info', 'inf', 'information'),   'info',
//	  lower(src) IN ('warn', 'wrn', 'warning'),       'warn',
//	  lower(src) IN ('error', 'err'),                 'error',
//	  lower(src) =  'critical',                        'critical',
//	  lower(src) =  'fatal',                           'fatal',
//	  lower(src))
//
// Inputs that don't match any group fall through to the lowercased
// original — matching upstream `normalizeLogLevel`'s default branch.
// An empty resolved source maps to `unknown` (see [normaliseLevelExpr]).
//
// chplan's typed `Expr` surface has no IN frag; the IN clauses above
// are encoded as left-folded OR-chains of equality comparisons. The
// emitted SQL is byte-identical to a hand-written `multiIf(... OR ...,
// ..., ... OR ...)` expression.
func detectedLevelExpr(s schema.Logs) chplan.Expr {
	return normaliseLevelExpr(detectedLevelSourceExpr(s))
}

// detectedLevelSourceExpr resolves the raw (pre-normalisation) log-level
// source string for the current row, mirroring reference Loki's
// `extractLogLevel` precedence (pkg/distributor/field_detection.go):
//
//  1. `LogAttributes['detected_level']` — an upstream processor already
//     stamped the canonical key, pass it through.
//  2. The first non-empty allowed level field
//     ([allowedLevelFields] — `level` / `severity` / `lvl` / …) present
//     in the LogAttributes (structured-metadata) map.
//  3. The dedicated `SeverityText` column — cerberus's stand-in for the
//     OTLP severity source reference Loki reads from
//     `__otlp_severity_number__` structured metadata.
//
// The shape is a `multiIf(...)` cascade that returns the first non-empty
// candidate; an all-empty row yields `”`, which [normaliseLevelExpr]
// maps to `unknown`. When the schema carries no structured-metadata
// column (custom-schema opt-out, `AttributesColumn == ""`) the cascade
// collapses to the bare `SeverityText` column — byte-identical to the
// prior single-source behaviour, so custom schemas without LogAttributes
// see zero churn.
//
// Why this matters: production OTel pipelines that route a `level` /
// `severity` structured-metadata attribute (without populating the
// dedicated SeverityText column) previously collapsed to
// `detected_level="unknown"` because cerberus only read SeverityText.
// Reference Loki resolves those records' level from the structured-
// metadata field — this cascade restores that parity.
func detectedLevelSourceExpr(s schema.Logs) chplan.Expr {
	severity := chplan.Expr(&chplan.ColumnRef{Name: s.SeverityColumn})
	if s.AttributesColumn == "" {
		return severity
	}

	// Each candidate contributes a (LogAttributes[key] != '', LogAttributes[key])
	// pair to the multiIf cascade. The detected_level key leads, then the
	// allowed level fields, then SeverityText as the final fallback branch.
	// The keys are Loki/OTLP-convention structured-metadata names stored
	// verbatim — NOT OTel dotted resource attributes — so a plain
	// MapAccess (no dotted-form fallback) is the correct, lean lookup.
	keys := make([]string, 0, len(allowedLevelFields)+1)
	keys = append(keys, detectedLevelLabel)
	keys = append(keys, allowedLevelFields...)

	attrCol := &chplan.ColumnRef{Name: s.AttributesColumn}
	args := make([]chplan.Expr, 0, len(keys)*2+1)
	for _, key := range keys {
		lookup := &chplan.MapAccess{Map: attrCol, Key: &chplan.LitString{V: key}}
		args = append(
			args,
			&chplan.Binary{Op: chplan.OpNe, Left: lookup, Right: &chplan.LitString{V: ""}},
			lookup,
		)
	}
	// Final fallback: the dedicated severity column.
	args = append(args, severity)
	return &chplan.FuncCall{Name: "multiIf", Args: args}
}

// normaliseLevelExpr returns a CH `multiIf(...)` chain that maps the
// case-insensitive forms upstream Loki accepts (`err`/`error`,
// `warn`/`wrn`/`warning`, `inf`/`info`/`information`, `dbg`/`debug`,
// `trc`/`trace`, `critical`, `fatal`) onto Loki's canonical lowercase
// level strings. Non-empty inputs that don't match any group fall
// through to the lowercased original value — matching upstream
// `normalizeLogLevel`'s default branch.
//
// An EMPTY input maps to `unknown`: reference Loki's level detection
// (pkg/distributor/field_detection.go — default-on via the
// `discover_log_levels` limit) stamps `detected_level` as structured
// metadata on EVERY ingested record, falling back to
// `constants.LogLevelUnknown` ("unknown") when nothing detectable
// exists. A record whose OTel SeverityText is empty therefore shows
// `detected_level="unknown"` on any reference Loki deployment — the
// k3d crawl pinned this on the Logs Drilldown labels tab, where the
// `detected_level` breakdown rendered "No data" for filelog-collected
// rows (no SeverityText) because cerberus dropped the key instead of
// emitting `unknown` (run 27327766381). Reference Loki's
// content-scan fallback (JSON / logfmt / keyword scan of the line
// itself) remains out of scope — see the package comment at the top
// of this file — so a severity-free row whose BODY carries a level
// keyword maps to `unknown` here where a reference Loki with content
// discovery would refine it.
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

	args := make([]chplan.Expr, 0, (len(groups)+1)*2+1)
	// Empty severity first — reference Loki stamps "unknown" when no
	// level is detectable (constants.LogLevelUnknown), it never leaves
	// the label absent or empty.
	args = append(
		args,
		&chplan.Binary{Op: chplan.OpEq, Left: lowerValue, Right: &chplan.LitString{V: ""}},
		&chplan.LitString{V: "unknown"},
	)
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
// The synthesized value is never empty — rows without severity
// metadata map to `unknown`, mirroring reference Loki's distributor-
// side stamping (see [normaliseLevelExpr]) — so the `mapFilter` never
// drops the `detected_level` entry itself; it remains for the
// outer-by top-level columns [withDetectedLevelAndColumns] folds into
// the same synthesized map, whose `toString(...)` values CAN be empty
// on rows that don't populate the column.
//
// Used by both the log-stream projection (Lang.ProjectSamples for log
// queries, where the surfaced label splits the streams response into
// one Stream per detected_level) and the bare range-aggregation
// projection (lowerRangeAggregation when no by/without grouping, where
// the augmented identity drives the RangeWindow GROUP BY to emit one
// series per detected_level).
func withDetectedLevel(s schema.Logs, baseLabels chplan.Expr) chplan.Expr {
	return withDetectedLevelAndColumns(s, baseLabels, nil)
}

// withDetectedLevelAndColumns is the column-aware companion of
// [withDetectedLevel]: it augments the identity map with the
// synthesised `detected_level` key AND with one synthesised key per
// top-level OTel-CH scalar column (SeverityText, ServiceName, ...)
// named in `outerByLabels`. The outer-by-labels list comes from
// [lowerCtx.OuterByLabels] — i.e. the by-clause of the enclosing
// vector aggregation, threaded down so the inner identity exposes
// exactly the top-level columns the outer aggregate needs.
//
// The map shape becomes
//
//	mapConcat(
//	    <baseLabels>,
//	    mapFilter((k, v) -> v != '',
//	        map('detected_level', multiIf(...),
//	            '<col1>',         toString(<col1>),
//	            '<col2>',         toString(<col2>),
//	            ...)))
//
// `toString` coerces non-String top-level columns (SeverityNumber,
// TraceFlags) into the Map(String, String) value slot. String-typed
// columns are already string-shaped so the coercion is a no-op the
// emitter elides. `mapFilter` drops empty entries the same way it
// does for `detected_level`, so a row with an empty severity column
// doesn't gain a spurious `{SeverityText:""}` key.
//
// When `outerByLabels` is empty the function behaves identically to
// the original [withDetectedLevel] — bare `rate({}[5m])` and other
// no-outer-grouping queries keep their lean identity map.
func withDetectedLevelAndColumns(s schema.Logs, baseLabels chplan.Expr, outerByLabels []string) chplan.Expr {
	args := []chplan.Expr{
		&chplan.LitString{V: detectedLevelLabel},
		detectedLevelExpr(s),
	}
	for _, col := range topLevelColumnsReferencedBy(outerByLabels, s) {
		args = append(
			args,
			&chplan.LitString{V: col},
			&chplan.FuncCall{
				Name: "toString",
				Args: []chplan.Expr{topLevelColumnRef(col)},
			},
		)
	}
	// Non-top-level outer-by keys (e.g. an OTel structured-metadata
	// attribute like `query_kind`) are inflated into the synthesized
	// identity map so the post-RangeWindow outer aggregation
	// ([levelAwareGroupKey]) can read them back from the
	// ResourceAttributes-aliased identity column. Each value resolves
	// with the structured-metadata > stream precedence
	// [structuredOrStreamLookup] applies — without this inflation a
	// `sum by (query_kind) (count_over_time({...}[5m]))` collapses every
	// row into one `{query_kind:""}` series because `query_kind` lives in
	// LogAttributes, not in the bare ResourceAttributes identity base
	// (task #59). The enclosing `mapFilter((k, v) -> v != '')` drops the
	// key on rows where neither map carries it, so a stream-only or
	// absent key keeps its prior (empty-dropped) shape.
	for _, key := range structuredOuterByKeys(outerByLabels, s) {
		args = append(
			args,
			&chplan.LitString{V: key},
			structuredOrStreamLookup(s, key),
		)
	}
	synthMap := &chplan.FuncCall{Name: "map", Args: args}
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
			synthMap,
		},
	}
	return &chplan.FuncCall{
		Name: "mapConcat",
		Args: []chplan.Expr{baseLabels, filtered},
	}
}

// structuredMetadataExpr returns the chplan expression that surfaces a
// log row's OTel-CH LogAttributes map as Loki structured metadata — the
// third element of each `[ts, line, {metadata}]` value tuple in a
// streams response. The shape is
//
//	mapFilter((k, v) -> v != '', LogAttributes)
//
// Empty-valued entries are dropped so a row that doesn't populate a given
// attribute doesn't advertise an empty column — mirroring reference
// Loki, which only attaches structured-metadata keys that carry a value.
// The keys are stored verbatim (Loki/OTLP-convention names like
// `duration` / `read_bytes` / `query_id`), already matching the
// structured-metadata grammar, so no per-key normalisation runs here;
// the handler normalises on the way out alongside the stream labels.
//
// Callers gate on a non-empty AttributesColumn — a custom schema without
// a structured-metadata column never reaches this expression.
func structuredMetadataExpr(s schema.Logs) chplan.Expr {
	return &chplan.FuncCall{
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
			&chplan.ColumnRef{Name: s.AttributesColumn},
		},
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
