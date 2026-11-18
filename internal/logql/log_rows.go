package logql

import (
	"fmt"

	"github.com/tsouza/cerberus/internal/chplan"
	syntax "github.com/tsouza/cerberus/internal/logql/lsyntax"
	"github.com/tsouza/cerberus/internal/schema"
)

// lowerLogQuery lowers a log query — a bare stream selector or a pipeline
// — to the rows the streams response decodes, already in the positional
// shape the chclient cursor scans: (<String>, Attributes, TimeUnix[,
// Metadata]). A log line is a String, so it takes the first column — the
// slot a metric query fills with the metric name — under the
// [LogLineColumn] alias, and there is NO fourth Float64 column: a log
// stream has no numeric value, so projecting a placeholder would be width
// every log query pays for and then discards (issue #1430). The chclient
// cursor recognises the shape by this leading alias and binds a scan with
// no numeric destination. chclient.DecodeLogRows turns that row into the
// named chclient.LogRow the Loki streams pivot consumes.
//
// The rows are one per-row stage ([rowLayers]): the stream scan, the
// synthesized level ([defineDetectedLevel]), the pipeline's stages, the
// optional `ORDER BY … LIMIT` ([logLineLimit]) and the output projection.
// Each labels map a parser stage produces and the level are defined once
// as columns; the label filters and the Attributes column read them by
// name. [rowLayers.hoist] keeps below the limit only the values a filter
// reads, so a value only the Attributes column reads is computed for the
// rows the limit returns. The Project that reads the filtered rows — the
// projection, or the one the sort reads when there is a limit — is marked
// [chplan.Project.OneScopeRows]: the filters run in the SELECT that reads
// the table. A range aggregation's per-row stage reads the table in one
// SELECT only for the part [rowLayers.gatherAtScan] moves there, and
// renders one SELECT per layer above it.
//
// The Attributes column carries the synthesized `detected_level` label.
// Reference Loki surfaces `detected_level` on every log query that
// returns rows whose severity can be detected from stream /
// structured-metadata labels, parser-stage extraction, or content scan
// (see [detectedLevelIdentityExpr]'s doc comment for the upstream
// detection sources cerberus mirrors, and for how a `| drop` / `| keep`
// stage projects the label away again). Reference Loki splits even a bare
// selector query into one Stream per detected_level value, so the label
// is surfaced for bare selectors, line filters, label filters on
// unrelated keys and parser-stage queries alike.
//
// When the pipeline carries a SQL-side labels stage (see
// [projectedStreamLabels]), the Attributes column starts from the
// pipeline's final labels map rather than the bare `ResourceAttributes`
// column, so each row carries the union of stream-selector labels and
// parser-extracted keys. [toStreamsWithTransform] groups by the projected
// label set, so unique (resource-label, extracted-key) tuples land in
// distinct Streams — matching reference Loki's stream-identity contract.
// Without it a query like `{cluster="c"} | logfmt` collapses hundreds of
// upstream-Loki streams into a single one.
func lowerLogQuery(sel syntax.LogSelectorExpr, s schema.Logs, lc lowerCtx) (chplan.Node, error) {
	var stream *syntax.MatchersExpr
	var pipeline *syntax.PipelineExpr
	switch e := sel.(type) {
	case *syntax.MatchersExpr:
		stream = e
	case *syntax.PipelineExpr:
		stream, pipeline = e.Left, e
	default:
		return nil, fmt.Errorf("logql: log query is %T (not MatchersExpr or PipelineExpr)", sel)
	}
	rows := newRowLayers(lowerMatchers(stream, s, lc))
	_, level := defineDetectedLevel(rows, s)
	labels := chplan.Expr(&chplan.ColumnRef{Name: s.ResourceAttributesColumn})
	if pipeline != nil {
		lc.row.level = level
		stageLabels, err := lowerPipelineStages(pipeline, s, lc, rows)
		if err != nil {
			return nil, err
		}
		labels = projectedStreamLabels(pipeline, s, stageLabels, labels)
	}
	attrs := labels
	if levelValue := detectedLevelIdentityExpr(sel, level); levelValue != nil {
		attrs = withDetectedLevel(s, attrs, levelValue)
	}
	// `| unpack` replaces the line with the payload's `_entry` member, and
	// it does so in SQL alongside the labels it extracts from the same
	// payload (see [PipelineLineExpr]). Projecting the bare body column
	// here would hand callers the packed JSON under labels that describe
	// its contents.
	line := chplan.Expr(&chplan.ColumnRef{Name: s.BodyColumn})
	if rewritten, err := PipelineLineExpr(sel, s); err == nil && rewritten != nil {
		line = rewritten
	}
	projections := []chplan.Projection{
		{Expr: line, Alias: LogLineColumn},
		{Expr: attrs, Alias: sampleAttributesCol},
		{Expr: &chplan.ColumnRef{Name: s.TimestampColumn}, Alias: sampleTimeUnixCol},
	}
	// Surface the OTel-CH LogAttributes map as Loki structured metadata —
	// the third element of each `[ts, line, {metadata}]` value tuple. The
	// stream identity (Attributes) stays the low-cardinality
	// ResourceAttributes + detected_level set so streams don't fragment;
	// the genuinely per-line keys (duration / bytes / query_id / …) ride
	// here instead, which is exactly the shape Grafana's Logs Drilldown
	// reads to render clean, well-formed columns. The chclient cursor
	// detects this trailing `Metadata` column and binds a four-destination
	// log-row scan; the four-column metric / prom / tempo paths are
	// untouched. Schemas without a structured-metadata column
	// (AttributesColumn == "") skip the projection entirely, leaving the
	// bare three-column log-row shape.
	if s.AttributesColumn != "" {
		projections = append(projections, chplan.Projection{
			Expr:  structuredMetadataExpr(s),
			Alias: logMetadataCol,
		})
	}
	order, limit := logLineLimit(sel, s, lc)
	rows.hoist(logMapColumns(s), order, limit)
	out := &chplan.Project{
		Roles:       logSampleRoles(),
		Input:       rows.node,
		Projections: projections,
		// With a limit the filtered rows sit under the sort, and the
		// Project the sort reads carries the mark instead.
		OneScopeRows: len(rows.passes) == 0,
	}
	rows.seal(out)
	return out, nil
}

// logMetadataCol is the alias of a log row's structured-metadata column.
const logMetadataCol = "Metadata"

// projectedStreamLabels returns the labels map a log row's Attributes
// column starts from: the pipeline's final labels map, stageLabels, when
// a stage changes the SQL-side label set ([HasLabelMutatingStage]), and
// base — the bare `ResourceAttributes` column — otherwise.
//
// A pipeline whose lowering skipped a stage is the exception. After a
// `| pattern` stage, an `__error__` / `__error_details__` label filter is
// applied in Go (see [lowerPipelineStages]'s dynamicLabels gate), so
// stageLabels lacks whatever that filter would stamp. For it the labels
// are re-derived from every stage by [PipelineLabelsExpr], and fall back
// to base when that walk rejects a stage the lowering skipped. Such a
// pipeline is never limited in SQL ([pipelineCanDropRowsInGo]), so its
// labels are computed for every row either way.
func projectedStreamLabels(pipeline *syntax.PipelineExpr, s schema.Logs, stageLabels, base chplan.Expr) chplan.Expr {
	if !HasLabelMutatingStage(pipeline) {
		return base
	}
	if !pipelineCanDropRowsInGo(pipeline.MultiStages) {
		return stageLabels
	}
	if parsed, err := PipelineLabelsExpr(pipeline, s); err == nil {
		return parsed
	}
	return base
}
