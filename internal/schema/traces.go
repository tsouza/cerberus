package schema

// TagCatalogTable / TagCatalogScopeColumn / TagCatalogKeyColumn /
// TagCatalogTopValuesStateColumn name the Tempo tag-catalog table
// (cerberus issue #2771, the Tempo sibling of the loki_label_catalog
// pattern — see LabelCatalogTable's doc comment for the full rationale
// this one repeats verbatim): cerberus-invented, not upstream OTel-CH
// names, and NOT a Traces struct field, for the exact same reason
// LabelCatalogTable isn't a Logs field — a field would false-positive
// TestReadSurfaceCoversEveryReadSideSchemaField on a cluster that never
// enabled this version-gated, opt-in table. Package-level constants are
// exported because internal/schema/ddl (which creates the table),
// internal/api/tempo (which queries it), and cmd/cerberus (which queries
// system.view_refreshes for the view) all need the SAME literal, and
// internal/schema is the one leaf package all three already import.
//
// TagCatalogScopeResource / TagCatalogScopeSpan are the two values the
// Scope column carries — the two attribute-map scopes the catalog
// covers (see internal/schema/ddl's Tempo catalog doc comment for why
// event/link/instrumentation stay off the catalog). They are exported
// from here, not from internal/api/tempo's own scope-vocabulary
// constants, because the DDL package (which renders these exact string
// literals into the view body) cannot import internal/api/tempo without
// an import cycle; internal/api/tempo's tagScopeResource / tagScopeSpan
// alias these instead of duplicating the literal — see that package's
// tag_catalog.go.
const (
	TagCatalogTable                = "tempo_tag_catalog"
	TagCatalogScopeColumn          = "Scope"
	TagCatalogKeyColumn            = "TagKey"
	TagCatalogTopValuesStateColumn = "TopValuesState"
	TagCatalogScopeResource        = "resource"
	TagCatalogScopeSpan            = "span"

	// TagCatalogTopValuesLimit is the N in the catalog's
	// `topKState(N)(...)` / `topKMerge(N)(...)` aggregate pair — the
	// single source of truth for both the WRITE side
	// (internal/schema/ddl's refreshable view, which computes the state)
	// and the READ side (internal/api/tempo's catalog query, which merges
	// it): the two must agree, since a read-side N larger than the
	// write-side N could never return more values than the state
	// actually retained, and a mismatch would be silently wrong rather
	// than a compile error if each side carried its own literal.
	TagCatalogTopValuesLimit = 50

	// TagCatalogViewSuffix is appended to TagCatalogTable to name the
	// refreshable materialized view feeding it — the same convention
	// LabelCatalogViewSuffix uses for its Loki sibling.
	TagCatalogViewSuffix = "_mv"
)

// Traces describes how cerberus reads spans from ClickHouse. The default
// (returned by DefaultOTelTraces) matches the OpenTelemetry ClickHouse
// Exporter v0.x traces schema; users with custom layouts override
// individual fields.
//
// Column names mirror the upstream `traces_table.sql` template
// verbatim, plus a synthetic `EndTimeColumn` (`Timestamp` — OTel-CH
// stores duration; end = start + duration) for the TraceQL emitter.
type Traces struct {
	// SpansTable is the table holding span records.
	SpansTable string

	// TraceIDTsTable names the `<spans>_trace_id_ts` lookup table the
	// OTel-CH exporter populates via a materialized view: one row per
	// TraceId carrying (Start, End) = (min, max) of the trace's span
	// Timestamps. Cerberus reads it to inject a Timestamp-window
	// pre-filter into the trace-by-ID spans scan so the spans table can
	// Partition/PrimaryKey/MinMax-prune to ~1 granule instead of scanning
	// every part to apply the bloom filter. Defaults to
	// `<SpansTable>_trace_id_ts`.
	TraceIDTsTable string
	// TraceIDTsStartColumn names the trace-window lower-bound column on
	// TraceIDTsTable (min of the trace's span Timestamps). OTel-CH default
	// "Start".
	TraceIDTsStartColumn string
	// TraceIDTsEndColumn names the trace-window upper-bound column on
	// TraceIDTsTable (max of the trace's span Timestamps). OTel-CH default
	// "End".
	TraceIDTsEndColumn string

	// TraceIDTsEnabled gates the Timestamp-window pre-filter described on
	// TraceIDTsTable. OFF by default: the window reads the lookup table via
	// scalar subqueries, and if the MV is absent or unpopulated those
	// subqueries yield NULL and the windowed scan matches nothing. The
	// operator opts in (CERBERUS_SCHEMA_TRACES_TS_LOOKUP) only after
	// confirming the MV is populated — matching the AutoCreateSchema
	// "operator owns DDL" posture and avoiding an emit-time table-existence
	// probe (a layering violation). With the gate off, lowerTraceByID emits
	// today's plain `TraceId = ?` filter unchanged.
	TraceIDTsEnabled bool

	// TraceIDColumn names the trace-id column. Upstream `traces_table.sql`
	// types it `String`, not FixedString: the OTel ClickHouse Exporter
	// writes the already hex-encoded form (`hex.EncodeToString`, lowercase,
	// zero-padded to 32 chars), never raw 16-byte binary. This is the exact
	// representation cerberus's Tempo head reads/emits verbatim (see
	// internal/api/tempo/traceid_hex_test.go) and the PromQL exemplars path
	// passes through unmodified (internal/api/prom/exemplars_encoding_test.go).
	TraceIDColumn string
	// SpanIDColumn names the span-id column. Same `String`-typed,
	// already-hex-encoded shape as TraceIDColumn, zero-padded to 16 chars.
	SpanIDColumn string
	// ParentSpanIDColumn names the parent-span-id column.
	ParentSpanIDColumn string
	// TraceStateColumn names the W3C trace-state column.
	TraceStateColumn string
	// SpanNameColumn names the span name column.
	SpanNameColumn string
	// SpanKindColumn names the span kind column ("Client", "Server", ...).
	SpanKindColumn string
	// ServiceNameColumn names the dedicated service.name column.
	ServiceNameColumn string

	// DurationColumn names the span-duration column (UInt64 nanoseconds).
	DurationColumn string
	// StartTimeColumn names the span-start timestamp column.
	StartTimeColumn string
	// EndTimeColumn is a cerberus synthetic — OTel-CH stores duration;
	// end-time is derived as `Timestamp + Duration`. The emitter substitutes
	// the literal computation when this string equals StartTimeColumn.
	EndTimeColumn string

	// StatusCodeColumn names the status code column ("Unset", "Ok", "Error").
	StatusCodeColumn string
	// StatusMessageColumn names the status message column.
	StatusMessageColumn string

	// AttributesColumn names the span-level attribute map.
	AttributesColumn string
	// ResourceAttributesColumn names the resource attribute map (carries
	// service-identity labels).
	ResourceAttributesColumn string
	// ScopeNameColumn names the instrumentation-scope name column.
	ScopeNameColumn string
	// ScopeVersionColumn names the instrumentation-scope version column.
	ScopeVersionColumn string
	// ScopeAttributesColumn names the instrumentation-scope attribute map.
	// NOTE: the upstream `traces_table.sql` template does not currently
	// declare a ScopeAttributes column; cerberus carries this field so
	// custom-schema users can point it at their own column. The default
	// is the empty string so emitters can skip it when unset.
	ScopeAttributesColumn string

	// EventsColumn names the Nested span-events column (Timestamp /
	// Name / Attributes per row).
	EventsColumn string
	// LinksColumn names the Nested span-links column (TraceId / SpanId /
	// TraceState / Attributes per row).
	LinksColumn string

	// TimestampColumn names the canonical event timestamp column. For
	// OTel-CH this is the same as StartTimeColumn ("Timestamp" in newer
	// schemas, often "StartTimeUnix" in older).
	TimestampColumn string

	// MaterializedSpanAttributeColumns / MaterializedResourceAttributeColumns
	// map a hot SpanAttributes / ResourceAttributes key (e.g.
	// `http.status_code`, `k8s.namespace.name`) to the dedicated top-level
	// column cerberus provisions for it (cerberus issue #2776) — see
	// MaterializedAttributeColumnKindFor for the (usually
	// LowCardinality(String), occasionally numeric) type each key is
	// declared as.
	//
	// UNLIKE Logs.MaterializedResourceColumns, this is NOT exporter-owned
	// DDL: the traces spans table ships from the upstream OTel ClickHouse
	// Exporter template with no such columns, so this is cerberus's first
	// ADD COLUMN onto a table it does not otherwise own the CREATE TABLE
	// body of (see internal/schema/ddl's renderAddTraceMaterializedAttrColumns).
	// Each column is declared `LowCardinality(String) DEFAULT
	// <map>['<key>']` by default, or the numeric shape
	// MaterializedAttributeColumnKindFor names for a key like
	// `http.status_code` (cerberus issue #2869) — DEFAULT, not MATERIALIZED
	// either way. That choice is load-bearing:
	// a DEFAULT column's value for a row in a part that predates the ALTER is
	// computed LAZILY, at read time, from that row's own already-stored
	// Attributes map — confirmed against a real ClickHouse 26.6 server (see
	// the issue #2776 PR body): a fresh ADD COLUMN reads byte-identical to
	// the map on every existing row immediately, with zero mutation queued,
	// and stays byte-identical through and after the optional `MATERIALIZE
	// COLUMN` backfill (verified with zero divergence across 150M+ rows,
	// including rows inserted concurrently while the backfill mutation was
	// in flight). So — unlike the logs top-level scalar columns
	// (SeverityText, ServiceName, …), which the exporter's ingest pipeline
	// writes INDEPENDENTLY of the map and which therefore need
	// `coalesce(nullIf(col,''), map[k])` to cover the two paths diverging —
	// a materialized attribute column here can NEVER diverge from the map
	// by construction, so no map-fallback coalesce is needed on the routed
	// read: the column IS the map access, always, immediately. This also
	// means read routing needs no separate "backfill verified" operator
	// declaration the way schema.Config.DeltaPrefixReadEnabled does — a
	// not-yet-backfilled column degrades only in read PERFORMANCE (it falls
	// back to the lazy per-row DEFAULT evaluation, decoding the source Map
	// exactly as an unmaterialized key already does today), never in
	// correctness.
	//
	// nil (the default, DefaultOTelTraces leaves both maps empty) is the
	// opt-in gate: cerberus never routes to a column it was not told
	// exists. An operator turns this on by setting
	// CERBERUS_SCHEMA_TRACES_MATERIALIZED_ATTRS_ENABLED, which populates
	// both maps from DefaultMaterializedSpanAttributeColumns /
	// DefaultMaterializedResourceAttributeColumns AND (independently, see
	// internal/schema/ddl.Config.TraceMaterializedAttributesEnabled) drives
	// AutoCreateSchema to provision the columns — see config.go's
	// SchemaProvisioning.TraceMaterializedAttrsEnabled doc for why one
	// operator-facing flag is enough for both halves (the DownsampleTier
	// precedent: a single verdict may gate both provisioning and query
	// routing whenever an un-backfilled state degrades safely, which the
	// evidence above establishes for this feature).
	//
	// Split into two maps, one per attribute scope, rather than a single
	// map carrying a scope tag: TraceQL resolves a scoped identifier like
	// `span.http.status_code` / `resource.k8s.namespace.name` to a (scope,
	// key) pair up front (see internal/traceql/ast.Attribute), so a
	// scope-partitioned lookup is a direct map hit with no secondary
	// disambiguation — and the two maps are keyed from disjoint semconv
	// vocabularies in practice (span-level vs resource-level attributes),
	// so nothing is lost by not sharing one namespace.
	MaterializedSpanAttributeColumns     map[string]string
	MaterializedResourceAttributeColumns map[string]string
}

// materializedAttributeColumnPrefix names the cerberus-invented column
// prefix for a materialized span/resource attribute (cerberus issue
// #2776). Deliberately distinct from Logs' materializedColumnPrefix
// (`__otel_materialized_`), which spells out the exporter's OWN naming —
// these columns are cerberus-authored DDL the upstream exporter template
// knows nothing about, so the prefix says so.
const materializedAttributeColumnPrefix = "__cerberus_materialized_"

// MaterializedAttributeColumnType is the ClickHouse type a
// MaterializedColumnKindString materialized span/resource attribute column
// is declared as (cerberus issue #2776) — the single source of truth
// internal/schema/ddl (which renders the ADD COLUMN type) and
// internal/preflight (which validates the deployed type via system.columns)
// both consume, so the two can never drift apart.
const MaterializedAttributeColumnType = "LowCardinality(String)"

// NumericMaterializedAttributeColumnType is the ClickHouse type a
// MaterializedColumnKindNumeric materialized column is declared as
// (cerberus issue #2869) — the numeric counterpart of
// MaterializedAttributeColumnType, consumed the same way by
// internal/schema/ddl and internal/preflight. Nullable, not a bare Int32:
// the DEFAULT expression is toInt32OrNull(<map>[<key>]), which returns NULL
// for an absent key or a non-numeric value rather than erroring the row,
// and TraceQL's NULL-drops-row WHERE semantics needs that NULL to survive
// as a real NULL rather than get silently coerced to a non-Nullable
// column's zero default.
const NumericMaterializedAttributeColumnType = "Nullable(Int32)"

// MaterializedColumnKind classifies the ClickHouse type family a
// materialized span/resource attribute column is provisioned as. Every
// consumer that must render or validate the column differently by type —
// internal/schema/ddl's DDL rendering, internal/preflight's schema-shape
// check, internal/traceql's numeric-coercion skip, and the tempo tag-values
// API's UNION-arm projection (cerberus issue #2870's auto-scope routing) —
// derives its answer from MaterializedAttributeColumnKindFor, the single
// source of truth keyed by the semconv attribute key (cerberus issue
// #2869).
type MaterializedColumnKind int

const (
	// MaterializedColumnKindString is the default: LowCardinality(String)
	// DEFAULT <map>['<key>'], value-identical to the map's own String cell
	// (cerberus issue #2776).
	MaterializedColumnKindString MaterializedColumnKind = iota
	// MaterializedColumnKindNumeric is a native ClickHouse numeric type
	// (NumericMaterializedAttributeColumnType) DEFAULT
	// to<Type>OrNull(<map>['<key>']) instead — see
	// numericMaterializedAttributeKeys for which keys opt into this
	// (cerberus issue #2869).
	MaterializedColumnKindNumeric
)

// numericMaterializedAttributeKeys is the set of default-registry keys
// (defaultMaterializedSpanAttributeKeys / defaultMaterializedResourceAttributeKeys
// above) provisioned as MaterializedColumnKindNumeric instead of the
// MaterializedColumnKindString default (cerberus issue #2869).
// http.status_code is the only member today: OTel semconv defines it as an
// integer, and it is the exact numeric-coercion example cerberus issue
// #2776 named as the deferred stretch goal. Deliberately NOT extended to
// other numeric-shaped semconv keys (e.g. a future retry-count or
// byte-size attribute) speculatively — each addition is its own scope
// decision (see the issue's own scope note), this map just makes adding
// one a one-line change rather than a new coercion/DDL/preflight path.
var numericMaterializedAttributeKeys = map[string]bool{
	"http.status_code": true,
}

// MaterializedAttributeColumnKindFor reports the ClickHouse type family the
// materialized column for a span/resource attribute key is provisioned as.
// Keyed by the semconv attribute KEY rather than by scope or by column
// name: a key's numeric-ness is a property of the semconv definition, not
// of which map it happens to be materialized in, and every consumer
// already has the key in hand at the point it needs this answer (DDL
// renders per key, preflight validates per key alongside its column,
// TraceQL resolves a scoped identifier to a key before routing).
func MaterializedAttributeColumnKindFor(key string) MaterializedColumnKind {
	if numericMaterializedAttributeKeys[key] {
		return MaterializedColumnKindNumeric
	}
	return MaterializedColumnKindString
}

// defaultMaterializedSpanAttributeKeys / defaultMaterializedResourceAttributeKeys
// are the curated, deliberately small default set of hot TraceQL
// attribute keys cerberus materializes (cerberus issue #2776). Each is
// chosen for being both a near-universal OTel semantic-convention key and
// one of the highest-value TraceQL filter/group-by targets:
//
//   - http.status_code (span): present on virtually every HTTP-instrumented
//     span; the single most common error/latency-triage filter
//     (`{ span.http.status_code >= 500 }`) and the exact numeric-coercion
//     example the issue names.
//   - rpc.method (span): the RPC-world sibling of http.status_code —
//     high-cardinality-adjacent but low-cardinality-in-practice
//     (bounded by the service's own RPC surface), a common structural
//     filter/group-by for gRPC-instrumented systems.
//   - k8s.namespace.name (resource): the same key Logs.MaterializedResourceColumns
//     already materializes for logs (internal/schema/logs.go) — nearly
//     every multi-tenant/k8s deployment's primary "which namespace"
//     drilldown filter, and picking the identical key keeps the two
//     signals' materialized sets consistent for an operator reasoning
//     about both.
//
// Deliberately NOT an exhaustive semconv sweep: internal/schema/ddl.Column-per-key
// does not scale past roughly a dozen keys (each is a real ADD COLUMN +
// storage cost) — a wider or deployment-specific set is a config override,
// and a systematically wide set is the JSON-column migration's job, not
// this one's (see the issue's own scope note).
var (
	defaultMaterializedSpanAttributeKeys     = []string{"http.status_code", "rpc.method"}
	defaultMaterializedResourceAttributeKeys = []string{"k8s.namespace.name"}
)

// materializedAttributeColumns builds the {map-key -> materialized-column}
// table from a key list by prepending materializedAttributeColumnPrefix to
// each key, mirroring Logs' defaultMaterializedResourceColumns.
func materializedAttributeColumns(keys []string) map[string]string {
	out := make(map[string]string, len(keys))
	for _, key := range keys {
		out[key] = materializedAttributeColumnPrefix + key
	}
	return out
}

// DefaultMaterializedSpanAttributeColumns / DefaultMaterializedResourceAttributeColumns
// return the curated default {key -> column} tables described on
// defaultMaterializedSpanAttributeKeys / defaultMaterializedResourceAttributeKeys.
// internal/schema/env.go's CERBERUS_SCHEMA_TRACES_MATERIALIZED_ATTRS_ENABLED
// resolution and internal/schema/ddl's provisioning path both call these —
// the single source of truth for which keys the opt-in default set covers.
func DefaultMaterializedSpanAttributeColumns() map[string]string {
	return materializedAttributeColumns(defaultMaterializedSpanAttributeKeys)
}

func DefaultMaterializedResourceAttributeColumns() map[string]string {
	return materializedAttributeColumns(defaultMaterializedResourceAttributeKeys)
}

// DefaultOTelTraces returns the schema produced by the upstream OTel
// ClickHouse Exporter for traces.
func DefaultOTelTraces() Traces {
	return Traces{
		SpansTable:               "otel_traces",
		TraceIDTsTable:           "otel_traces_trace_id_ts",
		TraceIDTsStartColumn:     "Start",
		TraceIDTsEndColumn:       "End",
		TraceIDTsEnabled:         false,
		TraceIDColumn:            "TraceId",
		SpanIDColumn:             "SpanId",
		ParentSpanIDColumn:       "ParentSpanId",
		TraceStateColumn:         "TraceState",
		SpanNameColumn:           "SpanName",
		SpanKindColumn:           "SpanKind",
		ServiceNameColumn:        "ServiceName",
		DurationColumn:           "Duration",
		StartTimeColumn:          "Timestamp",
		EndTimeColumn:            "Timestamp", // OTel-CH stores duration; end = start + duration
		StatusCodeColumn:         "StatusCode",
		StatusMessageColumn:      "StatusMessage",
		AttributesColumn:         "SpanAttributes",
		ResourceAttributesColumn: "ResourceAttributes",
		ScopeNameColumn:          "ScopeName",
		ScopeVersionColumn:       "ScopeVersion",
		// Upstream traces_table.sql has no ScopeAttributes column; leave
		// empty so callers that consult it can skip the projection.
		ScopeAttributesColumn: "",
		EventsColumn:          "Events",
		LinksColumn:           "Links",
		TimestampColumn:       "Timestamp",
		// MaterializedSpanAttributeColumns / MaterializedResourceAttributeColumns
		// stay nil: unlike Logs' materialized columns, these are new
		// cerberus-authored DDL a fresh cluster does not carry until an
		// operator opts in (CERBERUS_SCHEMA_TRACES_MATERIALIZED_ATTRS_ENABLED)
		// — see the fields' own doc comment.
	}
}
