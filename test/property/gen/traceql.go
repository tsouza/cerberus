package gen

import (
	"encoding/hex"
	"fmt"
	"sort"
	"strings"
	"time"

	"pgregory.net/rapid"

	"github.com/tsouza/cerberus/test/property"
)

// TraceQLServicePool is the fixed service.name pool the TraceQL
// dataset draws from. Small + overlapping so generator iterations
// produce spans whose `{ resource.service.name = "<X>" }` selector
// matches multiple spans (predictable count() arithmetic).
var TraceQLServicePool = []string{"api", "web", "batch"}

// TraceQLClusterPool is the resource-attribute pool for "cluster" — a
// second resource attribute alongside service.name, used to exercise
// attribute matchers beyond resource.service.name.
var TraceQLClusterPool = []string{"east", "west"}

// TraceQLHTTPMethodPool is the span-attribute pool for "http.method",
// used to exercise span-scope attribute matchers (`span.http.method`).
// Deliberately a different pool than TraceQLSpanNamePool so a
// coincidental value match between the two doesn't mask a real bug.
var TraceQLHTTPMethodPool = []string{"GET", "POST", "PUT"}

// TraceQLStatusPool is the intrinsic status pool, in the CH-stored
// (title-case) form. TraceQL query literals are lowercase
// (ok/error/unset); statusQueryLiteral bridges the two.
var TraceQLStatusPool = []string{"Ok", "Error", "Unset"}

// TraceQLDurationPoolNs is the fixed duration-bucket pool spans draw
// from, in nanoseconds. Multiple distinct buckets give the duration
// intrinsic and avg|min|max|sum(duration) filters real spread to
// discriminate on, rather than every span sharing one fixed value.
var TraceQLDurationPoolNs = []int64{10_000_000, 50_000_000, 120_000_000, 300_000_000}

// TraceQLScopeCollisionAttributeKey is an attribute name deliberately
// stamped at BOTH the resource scope and the span scope of every
// generated span, with an independently-drawn value at each scope (see
// the two pools below). TraceQL addresses the two through distinct paths
// — `resource.environment` and `span.environment` — so a query
// engine that silently reads the wrong scope's map (or merges the two)
// produces an observably wrong answer rather than a coincidental match:
// the two pools below share no string, so "resource.environment" and
// "span.environment" can never agree on the same span by accident.
const TraceQLScopeCollisionAttributeKey = "environment"

// TraceQLScopeCollisionResourceValuePool / TraceQLScopeCollisionSpanValuePool
// are the disjoint value pools TraceQLScopeCollisionAttributeKey draws
// from at each scope. Disjoint is load-bearing, not incidental: it is
// what makes a scope mix-up a distinguishable test failure rather than an
// occasional silent pass (see TraceQLScopeCollisionAttributeKey's doc).
var (
	TraceQLScopeCollisionResourceValuePool = []string{"prod", "staging"}
	TraceQLScopeCollisionSpanValuePool     = []string{"canary", "shadow"}
)

// TraceQLDurationThresholdPoolMs is the pool of millisecond thresholds
// the query generator draws for duration comparisons (`duration OP
// Nms`, `avg(duration) OP Nms`, …). Includes values that land exactly
// on a TraceQLDurationPoolNs bucket and values that fall strictly
// between buckets, so the boundary case is exercised too.
var TraceQLDurationThresholdPoolMs = []int64{10, 50, 75, 120, 200, 300}

// traceQLComparisonOps is the scalar-comparison operator pool shared by
// every numeric filter the generator draws (count(), duration, and the
// avg|min|max|sum(duration) metrics).
var traceQLComparisonOps = []string{">", ">=", "<", "<=", "="}

// traceQLEqualityOps is the equality/inequality operator pool for
// intrinsics that only support (in)equality comparison (status).
var traceQLEqualityOps = []string{"=", "!="}

// TraceQLSpanNamePool is the fixed span-name pool. Each generated
// span draws a name from this pool plus a per-span ordinal so the
// (SpanName, Timestamp) pair is unique across the dataset (Tempo's
// /api/search collapses TraceSummary rows that share name+timestamp;
// the generator avoids that collapse by stamping a unique suffix on
// each span name).
var TraceQLSpanNamePool = []string{"GET", "POST", "PUT", "DELETE"}

// SpansTableName is the OTel-CH default traces table the DDL targets.
// Matches schema.DefaultOTelTraces().SpansTable.
const SpansTableName = "otel_traces"

// traceQLAnchor is the wall-clock baseline the dataset anchors span
// timestamps to. Picked far enough in the future to avoid colliding
// with any wall-clock assertion in the chDB seeds (same convention as
// the metrics generator's anchorTime).
var traceQLAnchor = time.Date(2026, 5, 13, 12, 0, 0, 0, time.UTC)

// TraceQLAnchorTime returns the fixed wall-clock baseline the dataset
// generator anchors span timestamps to.
func TraceQLAnchorTime() time.Time { return traceQLAnchor }

// traceQLRootParentID is the empty ParentSpanId literal the OTel
// ClickHouse exporter writes for a root span. A 16-character all-zero
// value is still a non-empty parent reference, so using it here turns
// every generated root into an orphan that Tempo correctly excludes
// from structural relations.
const traceQLRootParentID = ""

// traceQLMaxTraces / traceQLMaxChainDepth bound the dataset's trace
// count and per-trace parent-chain depth. A linear chain (rather than
// a branching tree) is enough to exercise both structural operators
// the generator draws — `>` (immediate child) and `>>` (descendant at
// any depth) — while keeping ancestor/descendant computation in the
// oracle a simple walk up a unique parent chain, no general tree
// algorithm required.
const (
	traceQLMaxTraces     = 3
	traceQLMaxChainDepth = 3
)

// traceQLBranchingTreeSpanCount is the fixed span count of the bounded
// branching-tree trace shape: one root, two of its direct children
// (siblings of each other), and one grandchild descending from the
// first child. Fixed rather than randomly sized — it is already the
// smallest topology that puts a sibling pair, a direct-child edge, and a
// two-hop descendant edge in front of the same query, and a fixed shape
// leaves rapid's shrinker nothing further to reduce within the topology
// itself (see drawTraceQLBranchingTree's doc for exactly which
// structural claims this shape is built to discriminate).
//
// Blind spot, stated rather than silently assumed away: this is ONE
// fixed 4-span shape, not a sweep over branching factor or tree depth —
// it says nothing about a tree with three-or-more children per node, two
// grandchildren, or depth beyond two hops. A future widening of the
// structural-operator surface needs its own bounded shape, not an
// unbounded generalization of this one.
const traceQLBranchingTreeSpanCount = 4

// traceQLBranchingTreeRoles names the four fixed positions in the
// bounded branching-tree shape, in generation order: the root, its two
// children (childA and its sibling childB), and childA's own child (the
// grandchild, two hops from root). Used only to key rapid's per-span
// draw labels — see drawTraceQLBranchingSpan.
var traceQLBranchingTreeRoles = [traceQLBranchingTreeSpanCount]string{"root", "childA", "childB", "grandchild"}

// TraceQLDataset returns a rapid generator that draws a random
// property.Dataset of OTel-CH traces rows.
//
//   - 1–3 traces. Each independently draws one of two shapes:
//   - a linear parent→child chain of 1–3 spans (root first) — the
//     original shape, letting structural-operator queries (`>`, `>>`)
//     exercise a real ancestor/descendant relationship instead of
//     every span being trace-root;
//   - the bounded branching tree (traceQLBranchingTreeSpanCount spans:
//     root, two children, one grandchild — see
//     drawTraceQLBranchingTree's doc), which additionally puts a real
//     sibling pair and a coexisting `>` + `>>` edge from one root in
//     front of the same query, evidence a chain alone cannot supply.
//   - Each span carries a resource.service.name (TraceQLServicePool),
//     a second resource attribute resource.cluster (TraceQLClusterPool),
//     a span attribute span.http.method (TraceQLHTTPMethodPool), and the
//     scope-collision pair resource.environment / span.environment
//     (TraceQLScopeCollisionAttributeKey) — the same attribute name at
//     two different scopes with independently-drawn, disjoint-pool
//     values, so a scope-resolution bug is observable rather than
//     accidentally masked.
//   - Span names draw from TraceQLSpanNamePool plus a per-span ordinal
//     suffix so (SpanName, Timestamp) pairs are unique across the
//     dataset — Tempo's /api/search collapses traces by name+ts, and
//     the property test wants one TraceSummary per span so the count
//     comparator is exact.
//   - Per-span StartTime is anchor + i*1s (i = the global span
//     ordinal, unique across every trace); Duration and StatusCode
//     draw from TraceQLDurationPoolNs / TraceQLStatusPool so the
//     duration/status intrinsic filters have real spread to
//     discriminate on.
//
// Blind spot, stated rather than silently assumed away: two shapes per
// trace (linear chain, bounded branching tree) is not a proof over every
// possible graph shape — see traceQLBranchingTreeSpanCount's doc for what
// the branching shape itself leaves unswept (branching factor, depth).
//
// The returned Dataset's DDL is a multi-statement script
// (`CREATE OR REPLACE TABLE otel_traces (...); INSERT ...;`) the chDB
// runner replays before each query. The TracesModel mirror carries the
// same data in the shape the oracle reads — one property.SpanRecord per
// generated span, with explicit TraceID / SpanID / ParentSpanID identity
// fields, ResourceAttributes (service.name, cluster, environment),
// SpanAttributes (http.method, environment), Name, DurationNs,
// StatusCode, and TimestampMs. Unlike the PromQL/LogQL mirrors, no
// reserved-label-key encoding is involved — see [property.SpanRecord]'s
// doc.
//
// MergeTree is the chosen engine (matches PromQL property test
// rationale: Memory engine refuses PREWHERE the chsql emitter emits).
func TraceQLDataset() *rapid.Generator[property.Dataset] {
	return rapid.Custom(func(t *rapid.T) property.Dataset {
		numTraces := rapid.IntRange(1, traceQLMaxTraces).Draw(t, "numTraces")
		var spans []traceQLSpan
		spanOrdinal := 0
		for ti := 0; ti < numTraces; ti++ {
			traceID := deterministicTraceID(ti, 0xa1)
			if rapid.Bool().Draw(t, fmt.Sprintf("branchingTree_%d", ti)) {
				spans = append(spans, drawTraceQLBranchingTree(t, ti, traceID, &spanOrdinal)...)
				continue
			}
			chainDepth := rapid.IntRange(1, traceQLMaxChainDepth).Draw(t, fmt.Sprintf("chainDepth_%d", ti))
			parentID := traceQLRootParentID
			for ci := 0; ci < chainDepth; ci++ {
				service := rapid.SampledFrom(TraceQLServicePool).Draw(t, fmt.Sprintf("service_%d_%d", ti, ci))
				cluster := rapid.SampledFrom(TraceQLClusterPool).Draw(t, fmt.Sprintf("cluster_%d_%d", ti, ci))
				httpMethod := rapid.SampledFrom(TraceQLHTTPMethodPool).Draw(t, fmt.Sprintf("httpMethod_%d_%d", ti, ci))
				baseName := rapid.SampledFrom(TraceQLSpanNamePool).Draw(t, fmt.Sprintf("spanName_%d_%d", ti, ci))
				status := rapid.SampledFrom(TraceQLStatusPool).Draw(t, fmt.Sprintf("status_%d_%d", ti, ci))
				durationNs := rapid.SampledFrom(TraceQLDurationPoolNs).Draw(t, fmt.Sprintf("duration_%d_%d", ti, ci))
				// Additive on top of the six draws above (unchanged in
				// order or label): the scope-collision resource/span pair
				// every span — linear or branching — now carries.
				scopeResourceValue := rapid.SampledFrom(TraceQLScopeCollisionResourceValuePool).Draw(t, fmt.Sprintf("scopeResource_%d_%d", ti, ci))
				scopeSpanValue := rapid.SampledFrom(TraceQLScopeCollisionSpanValuePool).Draw(t, fmt.Sprintf("scopeSpan_%d_%d", ti, ci))
				// Unique suffix → unique (SpanName, Timestamp) across spans:
				// Tempo's toTraceSummaries() keys by name+timestamp and collapses
				// duplicates. Suffixing the global span ordinal is the cheapest way
				// to guarantee uniqueness without adding a second random draw.
				name := fmt.Sprintf("%s /api/%d", baseName, spanOrdinal)
				spanID := deterministicSpanID(spanOrdinal, 0xb2)
				spans = append(spans, traceQLSpan{
					traceID:            traceID,
					spanID:             spanID,
					parentID:           parentID,
					service:            service,
					cluster:            cluster,
					httpMethod:         httpMethod,
					name:               name,
					startTime:          traceQLAnchor.Add(time.Duration(spanOrdinal) * time.Second),
					durationNs:         durationNs,
					statusCode:         status,
					scopeResourceValue: scopeResourceValue,
					scopeSpanValue:     scopeSpanValue,
				})
				parentID = spanID
				spanOrdinal++
			}
		}
		records := traceQLSpansToRecords(spans)
		return property.Dataset{
			DDL:    renderTraceQLDDL(spans),
			Traces: &property.TracesModel{Spans: records},
		}
	})
}

// drawTraceQLBranchingTree draws one bounded branching-tree trace:
//
//	root
//	├── childA (direct child of root)
//	│   └── grandchild (direct child of childA, grandchild of root)
//	└── childB (direct child of root — childA's sibling)
//
// Fixed topology (traceQLBranchingTreeSpanCount spans), not randomly
// sized. It exists to put four structural claims a linear chain alone
// can never simultaneously exercise in front of the same trace:
//
//   - `root > childA` and `root > childB` both hold — two independent
//     direct-child edges from one parent (a chain has at most one);
//   - `root >> grandchild` holds but `root > grandchild` does NOT — the
//     discriminator a mutant collapsing `>` into `>>` (a "direct-child
//     mutant") would get wrong;
//   - `childA > root` does NOT hold — the discriminator a mutant with
//     parent/child swapped ("a reversed relation") would get wrong;
//   - neither `>` nor `>>` ever relates childA and childB — siblings
//     share a parent, not an ancestor/descendant edge.
//
// TestEvaluate_BranchingTree (oracle package) and
// TestTraceQLBranchingTreeStructuralRelations (this package) pin all
// four claims deterministically: the first against the from-scratch
// oracle, the second against the real cerberus HTTP pipeline.
//
// spanOrdinal is threaded by pointer so the caller's running counter —
// the same one the linear-chain branch advances — stays one global
// sequence across every trace (see TraceQLDataset's (SpanName,
// Timestamp) uniqueness doc).
func drawTraceQLBranchingTree(t *rapid.T, ti int, traceID string, spanOrdinal *int) []traceQLSpan {
	root := drawTraceQLBranchingSpan(t, ti, traceQLBranchingTreeRoles[0], traceID, traceQLRootParentID, spanOrdinal)
	childA := drawTraceQLBranchingSpan(t, ti, traceQLBranchingTreeRoles[1], traceID, root.spanID, spanOrdinal)
	childB := drawTraceQLBranchingSpan(t, ti, traceQLBranchingTreeRoles[2], traceID, root.spanID, spanOrdinal)
	grandchild := drawTraceQLBranchingSpan(t, ti, traceQLBranchingTreeRoles[3], traceID, childA.spanID, spanOrdinal)
	return []traceQLSpan{root, childA, childB, grandchild}
}

// drawTraceQLBranchingSpan draws one span's attribute values for the
// branching-tree shape — the same pools and fields the linear-chain
// branch draws (service/cluster/httpMethod/name/status/duration) plus
// the scope-collision resource/span attribute pair, keyed by trace index
// and role rather than chain position so the two topologies' rapid
// labels never collide.
func drawTraceQLBranchingSpan(t *rapid.T, ti int, role, traceID, parentID string, spanOrdinal *int) traceQLSpan {
	service := rapid.SampledFrom(TraceQLServicePool).Draw(t, fmt.Sprintf("branchService_%d_%s", ti, role))
	cluster := rapid.SampledFrom(TraceQLClusterPool).Draw(t, fmt.Sprintf("branchCluster_%d_%s", ti, role))
	httpMethod := rapid.SampledFrom(TraceQLHTTPMethodPool).Draw(t, fmt.Sprintf("branchHTTPMethod_%d_%s", ti, role))
	baseName := rapid.SampledFrom(TraceQLSpanNamePool).Draw(t, fmt.Sprintf("branchSpanName_%d_%s", ti, role))
	status := rapid.SampledFrom(TraceQLStatusPool).Draw(t, fmt.Sprintf("branchStatus_%d_%s", ti, role))
	durationNs := rapid.SampledFrom(TraceQLDurationPoolNs).Draw(t, fmt.Sprintf("branchDuration_%d_%s", ti, role))
	scopeResourceValue := rapid.SampledFrom(TraceQLScopeCollisionResourceValuePool).Draw(t, fmt.Sprintf("branchScopeResource_%d_%s", ti, role))
	scopeSpanValue := rapid.SampledFrom(TraceQLScopeCollisionSpanValuePool).Draw(t, fmt.Sprintf("branchScopeSpan_%d_%s", ti, role))
	name := fmt.Sprintf("%s /api/%d", baseName, *spanOrdinal)
	spanID := deterministicSpanID(*spanOrdinal, 0xb2)
	span := traceQLSpan{
		traceID:            traceID,
		spanID:             spanID,
		parentID:           parentID,
		service:            service,
		cluster:            cluster,
		httpMethod:         httpMethod,
		name:               name,
		startTime:          traceQLAnchor.Add(time.Duration(*spanOrdinal) * time.Second),
		durationNs:         durationNs,
		statusCode:         status,
		scopeResourceValue: scopeResourceValue,
		scopeSpanValue:     scopeSpanValue,
	}
	(*spanOrdinal)++
	return span
}

// traceQLSpan is the in-memory mirror of one row of otel_traces the
// generator emits. The Dataset.Traces mirror stores spans as
// property.SpanRecord entries; this struct is the bridge (and also
// feeds renderTraceQLRow's DDL rendering, which reads its time.Time
// startTime directly).
type traceQLSpan struct {
	traceID    string // 32-hex, shared by every span in a trace's chain
	spanID     string // 16-hex
	parentID   string // 16-hex for children; empty for a chain root
	service    string
	cluster    string
	httpMethod string
	name       string
	startTime  time.Time
	durationNs int64
	statusCode string
	// scopeResourceValue / scopeSpanValue carry TraceQLScopeCollisionAttributeKey
	// at the resource and span scopes respectively — see that const's doc for
	// why the two pools they draw from are disjoint.
	scopeResourceValue string
	scopeSpanValue     string
}

// traceQLSpansToRecords pivots the generator's spans into the
// property.SpanRecord shape the framework persists on the Dataset — one
// record per span, with explicit identity, attribute, timing, and status
// fields (see [property.SpanRecord]'s doc), rather than a MetricsModel
// series carrying the same data under reserved string label keys.
func traceQLSpansToRecords(spans []traceQLSpan) []property.SpanRecord {
	out := make([]property.SpanRecord, 0, len(spans))
	for _, s := range spans {
		out = append(out, property.SpanRecord{
			TraceID:      s.traceID,
			SpanID:       s.spanID,
			ParentSpanID: s.parentID,
			Name:         s.name,
			ResourceAttributes: map[string]string{
				"service.name":                    s.service,
				"cluster":                         s.cluster,
				TraceQLScopeCollisionAttributeKey: s.scopeResourceValue,
			},
			SpanAttributes: map[string]string{
				"http.method":                     s.httpMethod,
				TraceQLScopeCollisionAttributeKey: s.scopeSpanValue,
			},
			TimestampMs: s.startTime.UnixMilli(),
			DurationNs:  s.durationNs,
			StatusCode:  s.statusCode,
		})
	}
	return out
}

// renderTraceQLDDL produces the multi-statement seed script for an
// otel_traces table. The schema mirrors the OTel-CH traces table
// (the subset the generator + oracle care about) — TraceId, SpanId,
// ParentSpanId, ServiceName (kept for OTel parity; the generator
// queries via ResourceAttributes), SpanName, ResourceAttributes,
// SpanAttributes, StartTimeUnixNano (column name `Timestamp` per
// OTel-CH default), DurationNs (column name `Duration`), StatusCode,
// ScopeName, ScopeVersion — the last two are unvaried (always empty
// string) but must exist: schema.DefaultOTelTraces() names them, and
// the structural-join (`>`/`>>`) lowering projects them unconditionally
// (see the ScopeName/ScopeVersion comment in renderTraceQLRow).
//
// `CREATE OR REPLACE TABLE` keeps re-runs idempotent (chdb-go shares
// one catalog across sessions).
func renderTraceQLDDL(spans []traceQLSpan) string {
	var b strings.Builder
	b.WriteString(`CREATE OR REPLACE TABLE `)
	b.WriteString(SpansTableName)
	b.WriteString(` (
    Timestamp DateTime64(9),
    TraceId String,
    SpanId String,
    ParentSpanId String,
    SpanName String,
    SpanKind LowCardinality(String),
    ServiceName LowCardinality(String),
    ResourceAttributes Map(String, String),
    SpanAttributes Map(String, String),
    Duration Int64,
    StatusCode LowCardinality(String),
    StatusMessage String,
    ScopeName String,
    ScopeVersion String
) ENGINE = MergeTree ORDER BY (Timestamp, TraceId);
`)
	if len(spans) == 0 {
		return b.String()
	}
	b.WriteString(`INSERT INTO `)
	b.WriteString(SpansTableName)
	b.WriteString(` VALUES `)
	for i, s := range spans {
		if i > 0 {
			b.WriteString(", ")
		}
		b.WriteString(renderTraceQLRow(s))
	}
	b.WriteString(";\n")
	return b.String()
}

// renderTraceQLRow renders one row literal for the INSERT script. The
// column order matches renderTraceQLDDL's CREATE TABLE.
func renderTraceQLRow(s traceQLSpan) string {
	var b strings.Builder
	b.WriteByte('(')
	// Timestamp
	b.WriteString("toDateTime64('")
	b.WriteString(s.startTime.UTC().Format("2006-01-02 15:04:05.000"))
	b.WriteString("', 9)")
	b.WriteString(", ")
	// TraceId / SpanId / ParentSpanId
	b.WriteString(quoteSQL(s.traceID))
	b.WriteString(", ")
	b.WriteString(quoteSQL(s.spanID))
	b.WriteString(", ")
	b.WriteString(quoteSQL(s.parentID))
	b.WriteString(", ")
	// SpanName
	b.WriteString(quoteSQL(s.name))
	b.WriteString(", ")
	// SpanKind (fixed "Internal" — generator doesn't vary this today)
	b.WriteString("'Internal'")
	b.WriteString(", ")
	// ServiceName (mirrors ResourceAttributes['service.name'])
	b.WriteString(quoteSQL(s.service))
	b.WriteString(", ")
	// ResourceAttributes
	b.WriteString(renderTraceQLMap(map[string]string{
		"service.name":                    s.service,
		"cluster":                         s.cluster,
		TraceQLScopeCollisionAttributeKey: s.scopeResourceValue,
	}))
	b.WriteString(", ")
	// SpanAttributes
	b.WriteString(renderTraceQLMap(map[string]string{
		"http.method":                     s.httpMethod,
		TraceQLScopeCollisionAttributeKey: s.scopeSpanValue,
	}))
	b.WriteString(", ")
	// Duration
	fmt.Fprintf(&b, "%d", s.durationNs)
	b.WriteString(", ")
	// StatusCode
	b.WriteString(quoteSQL(s.statusCode))
	b.WriteString(", ")
	// StatusMessage
	b.WriteString("''")
	b.WriteString(", ")
	// ScopeName / ScopeVersion — the generator doesn't vary instrumentation
	// scope, but the columns must exist: structuralExtraProjectionColumns
	// (internal/traceql/lower.go) hard-codes schema.Traces.ScopeNameColumn /
	// ScopeVersionColumn into every structural-join (`>`/`>>`) wrap
	// projection, so a table missing them 502s with UNKNOWN_IDENTIFIER the
	// moment a structural query runs.
	b.WriteString("''")
	b.WriteString(", ")
	b.WriteString("''")
	b.WriteByte(')')
	return b.String()
}

// renderTraceQLMap renders a CH `map('k','v', 'k2','v2')` literal.
// Sorted keys for determinism.
func renderTraceQLMap(m map[string]string) string {
	if len(m) == 0 {
		return "map()"
	}
	keys := make([]string, 0, len(m))
	for k := range m {
		keys = append(keys, k)
	}
	sort.Strings(keys)
	var b strings.Builder
	b.WriteString("map(")
	for i, k := range keys {
		if i > 0 {
			b.WriteString(", ")
		}
		b.WriteString(quoteSQL(k))
		b.WriteString(", ")
		b.WriteString(quoteSQL(m[k]))
	}
	b.WriteByte(')')
	return b.String()
}

// quoteSQL renders s as a single-quoted CH SQL string literal. Embedded
// single quotes get backslash-escaped. The generator only emits values
// from fixed pools (TraceQLServicePool, TraceQLClusterPool,
// TraceQLHTTPMethodPool, TraceQLSpanNamePool, hex IDs), none of which
// contain quotes — but the escape is here so future generator widening
// can stay safe.
func quoteSQL(s string) string {
	if !strings.Contains(s, "'") && !strings.Contains(s, "\\") {
		return "'" + s + "'"
	}
	var b strings.Builder
	b.WriteByte('\'')
	for i := 0; i < len(s); i++ {
		c := s[i]
		if c == '\'' || c == '\\' {
			b.WriteByte('\\')
		}
		b.WriteByte(c)
	}
	b.WriteByte('\'')
	return b.String()
}

// deterministicTraceID renders a 32-hex string from an integer seed +
// salt. Deterministic so re-running with the same rapid seed produces
// the same SQL.
func deterministicTraceID(seed, salt int) string {
	var buf [16]byte
	for i := range buf {
		buf[i] = byte((seed*31 + salt + i) & 0xFF)
	}
	return hex.EncodeToString(buf[:])
}

// deterministicSpanID is the 8-byte variant of deterministicTraceID.
func deterministicSpanID(seed, salt int) string {
	var buf [8]byte
	for i := range buf {
		buf[i] = byte((seed*17 + salt + i) & 0xFF)
	}
	return hex.EncodeToString(buf[:])
}

// statusQueryLiteral maps a CH-stored StatusCode value (Ok/Error/Unset)
// to the lowercase literal TraceQL's `status` intrinsic expects on the
// query side (ok/error/unset).
func statusQueryLiteral(chStatus string) string {
	return strings.ToLower(chStatus)
}

// TraceQLQuery returns a rapid generator that draws a property.Query
// targeted at dataset d. The accept-set, widened from the first sweep's
// single selector shape (issue #1471) to a breadth comparable to the
// PromQL leg's drawExpr (test/property/gen/promql.go), spans 20 stable
// semantic IDs across 17 equally weighted random families:
//
//   - traceql.selector.service / resource-attribute / span-attribute /
//     regex / negated-attribute / conjunction / scope-collision-resource /
//     scope-collision-span / scope-collision-conjunction
//   - traceql.intrinsic.duration / status / name
//   - traceql.structural.child / descendant
//   - traceql.pipeline.count / duration-aggregate (avg/min/max/sum) / select
//
// The three scope-collision shapes target TraceQLScopeCollisionAttributeKey
// (`resource.environment` / `span.environment`), stamped on every generated
// span from two disjoint value pools — see that const's doc for why a
// scope mix-up cannot coincidentally pass. The structural shapes now also
// draw against the bounded branching-tree trace shape (see
// drawTraceQLBranchingTree) whenever a dataset draw includes one, giving
// `>`/`>>` a real sibling pair to discriminate against in addition to the
// original linear-chain depth.
//
// Every shape's oracle counterpart lives in test/property/oracle/traceql
// — see that package's parseQuery/Evaluate for the independent
// specification each shape is checked against. A shape is added here
// only once the oracle can evaluate it (never the reverse), so the
// property test is never vacuous for a generated shape.
//
// The selector's RHS is always a value drawn from the dataset's
// observed pools so a good share of iterations exercise a matching
// value (genuine non-empty result) and the rest exercise an absent one
// (empty result). The mix is implicit — rapid draws across the pools
// uniformly, and any one dataset draw covers only a subset of a given
// pool.
//
// EvalTs is stamped at TraceQLAnchorTime() + 1h for log completeness.
// /api/search DOES thread a time range now (the harness sends an explicit
// window bracketing the anchor — see runCerberusTraceQL — so the
// DefaultSearchLookback clamp doesn't filter this historically-anchored
// dataset out), but the query value itself doesn't carry it.
func TraceQLQuery(d property.Dataset) *rapid.Generator[property.Query] {
	return rapid.Custom(func(t *rapid.T) property.Query {
		family := rapid.SampledFrom(traceQLRandomShapeFamilies).Draw(t, "shapeFamily")
		shapeID := rapid.SampledFrom(family).Draw(t, "shapeID")
		return drawTraceQLQuery(t, d, shapeID)
	})
}

// TraceQLQueryForShape fixes the TraceQL generator to one exact roster member
// for deterministic one-per-shape execution.
func TraceQLQueryForShape(d property.Dataset, shapeID ShapeID) *rapid.Generator[property.Query] {
	if !containsShapeID(traceQLShapeRoster[:], shapeID) {
		panic("gen/traceql: unknown shape " + string(shapeID))
	}
	return rapid.Custom(func(t *rapid.T) property.Query {
		return drawTraceQLQuery(t, d, shapeID)
	})
}

func drawTraceQLQuery(t *rapid.T, d property.Dataset, shapeID ShapeID) property.Query {
	query := drawTraceQLQueryString(t, d, shapeID)

	evalTs := traceQLAnchor.Add(time.Hour).Unix()
	return property.Query{
		ShapeID: property.ShapeID(shapeID),
		String:  query,
		EvalTs:  evalTs,
	}
}

// drawTraceQLQueryString renders the query string for the given stable shape
// ID. Split out of TraceQLQuery so each shape's construction reads
// as one focused case rather than a single sprawling closure.
func drawTraceQLQueryString(t *rapid.T, d property.Dataset, shapeID ShapeID) string {
	switch shapeID {
	case traceQLServiceShape:
		service := rapid.SampledFrom(TraceQLServicePool).Draw(t, "service")
		return fmt.Sprintf(`{ resource.service.name = "%s" }`, service)
	case traceQLCountShape:
		return drawTraceQLCountQuery(t, d)
	case traceQLResourceAttributeShape:
		cluster := rapid.SampledFrom(TraceQLClusterPool).Draw(t, "cluster")
		return fmt.Sprintf(`{ resource.cluster = "%s" }`, cluster)
	case traceQLSpanAttributeShape:
		method := rapid.SampledFrom(TraceQLHTTPMethodPool).Draw(t, "httpMethod")
		return fmt.Sprintf(`{ span.http.method = "%s" }`, method)
	case traceQLDurationShape:
		op := rapid.SampledFrom(traceQLComparisonOps).Draw(t, "durationOp")
		thresholdMs := rapid.SampledFrom(TraceQLDurationThresholdPoolMs).Draw(t, "durationThresholdMs")
		return fmt.Sprintf(`{ duration %s %dms }`, op, thresholdMs)
	case traceQLStatusShape:
		op := rapid.SampledFrom(traceQLEqualityOps).Draw(t, "statusOp")
		statusCH := rapid.SampledFrom(TraceQLStatusPool).Draw(t, "status")
		return fmt.Sprintf(`{ status %s %s }`, op, statusQueryLiteral(statusCH))
	case traceQLNameShape:
		return fmt.Sprintf(`{ name = %q }`, drawTraceQLNameValue(t, d))
	case traceQLRegexShape:
		service := rapid.SampledFrom(TraceQLServicePool).Draw(t, "service")
		pattern := service[:1] + ".*"
		return fmt.Sprintf(`{ resource.service.name =~ %q }`, pattern)
	case traceQLNegatedAttributeShape:
		service := rapid.SampledFrom(TraceQLServicePool).Draw(t, "service")
		return fmt.Sprintf(`{ resource.service.name != "%s" }`, service)
	case traceQLConjunctionShape:
		service := rapid.SampledFrom(TraceQLServicePool).Draw(t, "service")
		statusCH := rapid.SampledFrom(TraceQLStatusPool).Draw(t, "status")
		return fmt.Sprintf(`{ resource.service.name = "%s" && status = %s }`,
			service, statusQueryLiteral(statusCH))
	case traceQLStructuralChildShape:
		left := rapid.SampledFrom(TraceQLServicePool).Draw(t, "leftService")
		right := rapid.SampledFrom(TraceQLServicePool).Draw(t, "rightService")
		return fmt.Sprintf(`{ resource.service.name = "%s" } > { resource.service.name = "%s" }`, left, right)
	case traceQLStructuralDescendantShape:
		left := rapid.SampledFrom(TraceQLServicePool).Draw(t, "leftService")
		right := rapid.SampledFrom(TraceQLServicePool).Draw(t, "rightService")
		return fmt.Sprintf(`{ resource.service.name = "%s" } >> { resource.service.name = "%s" }`, left, right)
	case traceQLDurationAggregateShape:
		return drawTraceQLMetricQuery(t, "avg")
	case traceQLDurationAggregateMinShape:
		return drawTraceQLMetricQuery(t, "min")
	case traceQLDurationAggregateMaxShape:
		return drawTraceQLMetricQuery(t, "max")
	case traceQLDurationAggregateSumShape:
		return drawTraceQLMetricQuery(t, "sum")
	case traceQLSelectShape:
		service := rapid.SampledFrom(TraceQLServicePool).Draw(t, "service")
		return fmt.Sprintf(`{ resource.service.name = "%s" } | select(span.http.method)`, service)
	case traceQLScopeCollisionResourceShape:
		value := rapid.SampledFrom(TraceQLScopeCollisionResourceValuePool).Draw(t, "scopeResourceValue")
		return fmt.Sprintf(`{ resource.%s = "%s" }`, TraceQLScopeCollisionAttributeKey, value)
	case traceQLScopeCollisionSpanShape:
		value := rapid.SampledFrom(TraceQLScopeCollisionSpanValuePool).Draw(t, "scopeSpanValue")
		return fmt.Sprintf(`{ span.%s = "%s" }`, TraceQLScopeCollisionAttributeKey, value)
	case traceQLScopeCollisionConjunctionShape:
		resourceValue := rapid.SampledFrom(TraceQLScopeCollisionResourceValuePool).Draw(t, "scopeResourceValueConjunction")
		spanValue := rapid.SampledFrom(TraceQLScopeCollisionSpanValuePool).Draw(t, "scopeSpanValueConjunction")
		return fmt.Sprintf(`{ resource.%s = "%s" && span.%s = "%s" }`,
			TraceQLScopeCollisionAttributeKey, resourceValue, TraceQLScopeCollisionAttributeKey, spanValue)
	}
	panic("gen/traceql: unhandled query shape " + string(shapeID))
}

// drawTraceQLCountQuery renders traceql.pipeline.count: a service selector
// with a
// mandatory `| count() OP N` scalar filter. It never collapses to the bare
// selector roster member.
func drawTraceQLCountQuery(t *rapid.T, d property.Dataset) string {
	service := rapid.SampledFrom(TraceQLServicePool).Draw(t, "service")
	query := fmt.Sprintf(`{ resource.service.name = "%s" }`, service)
	op := rapid.SampledFrom(traceQLComparisonOps).Draw(t, "countOp")
	// Bound N at [0, len(spans)+1] so the threshold is sometimes
	// satisfied and sometimes not, including the "above ceiling" case.
	n := rapid.IntRange(0, len(d.Traces.Spans)+1).Draw(t, "countN")
	return fmt.Sprintf("%s | count() %s %d", query, op, n)
}

// drawTraceQLMetricQuery renders one stable duration-aggregate reducer: a
// service selector piped into avg|min|max|sum(duration) with a scalar
// comparison. The caller fixes fn so each reducer has deterministic live
// enrollment while the random generator preserves the original family weight.
func drawTraceQLMetricQuery(t *rapid.T, fn string) string {
	service := rapid.SampledFrom(TraceQLServicePool).Draw(t, "service")
	op := rapid.SampledFrom(traceQLComparisonOps).Draw(t, "metricOp")
	thresholdMs := rapid.SampledFrom(TraceQLDurationThresholdPoolMs).Draw(t, "metricThresholdMs")
	return fmt.Sprintf(`{ resource.service.name = "%s" } | %s(duration) %s %dms`,
		service, fn, op, thresholdMs)
}

// drawTraceQLNameValue picks the RHS for shape 6's `{ name = "<v>" }`.
// Half the draws (when the dataset has spans) pick a name that's
// actually present so the equality has something to match; the rest
// pick a value that's guaranteed absent, exercising the empty-result
// path deliberately rather than by accident.
func drawTraceQLNameValue(t *rapid.T, d property.Dataset) string {
	names := d.Traces.NamesPresent()
	if len(names) > 0 && rapid.Bool().Draw(t, "nameExists") {
		return rapid.SampledFrom(names).Draw(t, "existingName")
	}
	return "nonexistent-span-name"
}
