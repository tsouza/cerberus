package logql

import (
	"strings"

	syntax "github.com/tsouza/cerberus/internal/logql/lsyntax"

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

	args := make([]chplan.Expr, 0, (len(levelNormalizationGroups)+1)*2+1)
	// Empty severity first — reference Loki stamps "unknown" when no
	// level is detectable (constants.LogLevelUnknown), it never leaves
	// the label absent or empty.
	args = append(
		args,
		&chplan.Binary{Op: chplan.OpEq, Left: lowerValue, Right: &chplan.LitString{V: ""}},
		&chplan.LitString{V: "unknown"},
	)
	for _, g := range levelNormalizationGroups {
		args = append(args, anyEqual(lowerValue, g.variants), &chplan.LitString{V: g.canonical})
	}
	// Default branch — pass through the lowercased original. Matches
	// upstream Loki's `default: return level` behaviour.
	args = append(args, lowerValue)

	return &chplan.FuncCall{Name: "multiIf", Args: args}
}

// levelNormalizationGroup pairs the input-variant spellings upstream
// Loki's `normalizeLogLevel` accepts with the canonical lowercase level
// string they map to.
type levelNormalizationGroup struct {
	variants  []string
	canonical string
}

// levelNormalizationGroups is the single source of truth for the
// variant→canonical mapping, order matching upstream Loki's
// `normalizeLogLevel` switch: trace / debug / info / warn / error /
// critical / fatal. Shared by [normaliseLevelExpr] (the SQL `multiIf`
// cascade) and [NormalizeDetectedLevel] (the plain-Go equivalent) so the
// two derivations can't drift apart.
var levelNormalizationGroups = []levelNormalizationGroup{
	{[]string{"trace", "trc"}, "trace"},
	{[]string{"debug", "dbg"}, "debug"},
	{[]string{"info", "inf", "information"}, "info"},
	{[]string{"warn", "wrn", "warning"}, "warn"},
	{[]string{"error", "err"}, "error"},
	{[]string{"critical"}, "critical"},
	{[]string{"fatal"}, "fatal"},
}

// NormalizeDetectedLevel maps a raw severity/level string to Loki's
// canonical lowercase level set entirely in Go — the string-typed
// counterpart to [normaliseLevelExpr]'s SQL `multiIf` cascade, built from
// the same [levelNormalizationGroups] table so the two can't diverge.
//
// An empty input maps to `"unknown"`, matching reference Loki's
// `constants.LogLevelUnknown` stamping (see [normaliseLevelExpr]'s doc
// comment). A non-empty input that matches none of the known variants
// falls through to its lowercased form, matching upstream
// `normalizeLogLevel`'s default branch.
//
// Used by `/loki/api/v1/patterns` (internal/api/loki/patterns.go) to
// bucket pattern mining per detected level: that handler resolves each
// row's SeverityText once in Go rather than pushing a chplan expression
// through the optimizer/emit pipeline for a value it already has in
// hand.
func NormalizeDetectedLevel(raw string) string {
	lower := strings.ToLower(raw)
	if lower == "" {
		return "unknown"
	}
	for _, g := range levelNormalizationGroups {
		for _, v := range g.variants {
			if lower == v {
				return g.canonical
			}
		}
	}
	return lower
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
// `levelValue` is the projected value expression the caller obtained
// from [detectedLevelIdentityExpr]; a nil value means the query's
// pipeline removes `detected_level` from the output label set on every
// row, and the key is left out of the synthesized map entirely.
//
// Used by both the log-stream projection (Lang.ProjectSamples for log
// queries, where the surfaced label splits the streams response into
// one Stream per detected_level) and the bare range-aggregation
// projection (lowerRangeAggregation when no by/without grouping, where
// the augmented identity drives the RangeWindow GROUP BY to emit one
// series per detected_level).
func withDetectedLevel(s schema.Logs, baseLabels, levelValue chplan.Expr) chplan.Expr {
	return withDetectedLevelAndColumns(s, baseLabels, levelValue, nil, nil)
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
//
// A nil `levelValue` drops the `detected_level` pair from the
// synthesized map (the query's `| drop` / `| keep` projection removes
// the label on every row — see [detectedLevelIdentityExpr]). When that
// leaves nothing to synthesize at all, the base labels are returned
// untouched so the plan carries no vestigial `mapConcat(base, map())`.
func withDetectedLevelAndColumns(s schema.Logs, baseLabels, levelValue chplan.Expr, outerByLabels []string, parsedLabels chplan.Expr) chplan.Expr {
	var args []chplan.Expr
	if levelValue != nil {
		args = append(args, &chplan.LitString{V: detectedLevelLabel}, levelValue)
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
	// with the parsed > structured-metadata > stream precedence
	// [structuredOrStreamLookupOnMap] applies — without this inflation a
	// `sum by (query_kind) (count_over_time({...}[5m]))` collapses every
	// row into one `{query_kind:""}` series because `query_kind` lives in
	// LogAttributes, not in the bare ResourceAttributes identity base
	// (task #59). The enclosing `mapFilter((k, v) -> v != '')` drops the
	// key on rows where neither map carries it, so a stream-only or
	// absent key keeps its prior (empty-dropped) shape.
	//
	// `parsedLabels` carries the pipeline's parser-merged labels map so a
	// key a parser stage extracted (`sum by (category) (count_over_time(
	// {...} | json [5m]))`) resolves from the extraction rather than from
	// the two raw columns, which never carry it. It must be a plain
	// column reference — the caller materialises the merge into an
	// intermediate projection first — because the merge itself contains
	// lambdas and ClickHouse rejects a lambda whose source map is built
	// by another lambda. A nil value means the pipeline has no parser
	// stage to consult, and the two raw columns are the whole story.
	for _, key := range structuredOuterByKeys(outerByLabels, s) {
		value := structuredOrStreamLookup(s, key)
		if parsedLabels != nil {
			value = structuredOrStreamLookupOnMap(s, parsedLabels, key)
		}
		args = append(args, &chplan.LitString{V: key}, value)
	}
	if len(args) == 0 {
		return baseLabels
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
//	toJSONString(mapFilter((k, v) -> v != '', LogAttributes))
//
// Empty-valued entries are dropped so a row that doesn't populate a given
// attribute doesn't advertise an empty column — mirroring reference
// Loki, which only attaches structured-metadata keys that carry a value.
// The keys are stored verbatim (Loki/OTLP-convention names like
// `duration` / `read_bytes` / `query_id`), already matching the
// structured-metadata grammar, so no per-key normalisation runs here;
// the handler normalises on the way out alongside the stream labels.
//
// The filtered map is rendered to a JSON object string via `toJSONString`
// rather than projected as a raw `Map(String, String)`. A native Map
// column scans cleanly on prod ClickHouse (clickhouse-go) but the chDB
// probe lane (chdb-go's Parquet wire format) cannot cast a Map into a Go
// `map[string]string` — see internal/chclient/chdb_probe_test.go, which
// pins this exact shim. `toJSONString` returns a plain `String` that
// BOTH backends scan; the cursor json.Unmarshal's it back into a map.
//
// Callers gate on a non-empty AttributesColumn — a custom schema without
// a structured-metadata column never reaches this expression.
func structuredMetadataExpr(s schema.Logs) chplan.Expr {
	return &chplan.FuncCall{
		Name: "toJSONString",
		Args: []chplan.Expr{
			&chplan.FuncCall{
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
			},
		},
	}
}

// detectedLevelIdentityExpr returns the value expression the
// synthesized `detected_level` identity key carries for `expr`, or nil
// when the query must not surface the label at all.
//
// Reference Loki stamps `detected_level` as STRUCTURED METADATA at
// ingest (`pkg/distributor/field_detection.go::extractLogLevel`, default-on
// via the `discover_log_levels` limit), so by the time a query's
// pipeline runs the label is an ordinary member of the record's label
// set — one a `| drop` / `| keep` stage projects exactly like any
// other. Cerberus synthesizes the key instead of reading it off a
// column, and splices it into the identity map AFTER
// [narrowIdentityByProjection] has filtered the real map entries, so
// the projection is applied to the synthesized VALUE — see
// [projectSyntheticLabelValue] for the three outcomes and why an empty
// value is equivalent to a filtered-out key.
//
// The detection sources cerberus mirrors (from the same upstream
// function) are:
//
//  1. Stream / structured-metadata label named `detected_level` /
//     `level` / `severity` / `severity_text` / …
//  2. Parser-stage extraction (`| logfmt`, `| json`, `| regexp ...`,
//     `| pattern ...`, `| unpack`) that surfaces a `level` key from the
//     log line's structured payload.
//  3. Content scan over the log line (JSON / logfmt / keyword scan
//     for ERROR / WARN / INFO / DEBUG / TRACE / FATAL / CRITICAL).
//
// Cerberus's [detectedLevelSourceExpr] covers (1) and (2); (3) stays
// out of scope (see this file's package comment), resolving to
// `unknown`. Every log row therefore carries a non-empty derived value,
// which is why the gate is otherwise permissive: a query that never
// names the label still gets it, because reference Loki splits the
// response into one Stream per detected_level even for bare selectors,
// line filters, and label filters on unrelated keys. The earlier
// restrictive gate (surface only when the user named
// `detected_level` / `level`) is what caused the loki-compat
// `fast/basic-selectors.yaml` stream-identity regressions.
//
// A nil `expr` returns nil rather than panicking: only the metric
// branch of [Lang.ProjectSamples] reaches a projection without an
// `expr` in [engine.Meta.Extra], and it doesn't consult this.
//
// Pipe stages with parser-extracted `level` keys (`| logfmt`,
// `| json`, `| regexp ...`, `| pattern ...`, `| label_format ...`)
// keep going through their existing label-filter-context lookups —
// see [isDetectedLevelLabel] vs [isDetectedLevelGroupingLabel] for
// the matcher / grouping split. The synthesized key coexists with any
// parser-derived keys in the output label map (Loki's reference
// response carries both when applicable).
func detectedLevelIdentityExpr(s schema.Logs, expr syntax.Expr) chplan.Expr {
	if expr == nil {
		return nil
	}
	return projectSyntheticLabelValue(expr, detectedLevelLabel, func() chplan.Expr {
		return detectedLevelExpr(s)
	})
}
