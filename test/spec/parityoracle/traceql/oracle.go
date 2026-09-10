//go:build agpl_oracle

// Package traceql evaluates a fixture's query on the REAL upstream Tempo
// engine, so a TraceQL TXTAR fixture's answer can be checked against
// something other than cerberus's own output.
//
// It is the TraceQL half of the mechanism test/spec/parity.go describes;
// read that file first for why a fixture stores the parity CONTRACT and
// never the parity ANSWER.
//
// # The one rule this package exists to enforce
//
// It imports NOTHING from internal/{promql,logql,traceql,chplan,chsql,
// optimizer,schema}. An oracle that shares code with the system under test
// cannot disagree with it about the thing they share, so any such import
// silently converts this package from an oracle into a mirror.
// test/regression's parity contract test enforces the rule mechanically
// with `go list -deps -test`.
//
// # Why this needs a fork accessor, and why that does not make it a mirror
//
// TraceQL's structural operators — `>>` (descendant), `>` (child), `~`
// (sibling) — are declared on the traceql.Span INTERFACE, which invites the
// conclusion that implementing traceql.Span here would mean writing those
// semantics ourselves, i.e. mirroring the code under test. That conclusion
// is wrong, and the reason is worth stating precisely because it is the
// whole justification for the fork:
//
// The production implementation is vparquet4's unexported `span`, and each
// of the three methods type-asserts EVERY element of its arguments back to
// that concrete type in order to read three integers — nestedSetLeft,
// nestedSetRight, nestedSetParent. They touch no parquet page, no row
// number, no block. A caller supplying its own traceql.Span therefore
// cannot reach them at all; it would have to reimplement them.
//
// So this package does not implement traceql.Span. It builds vparquet4's
// own span through the fork's vparquet4.NewSpan constructor and hands it to
// upstream's own engine. What this package supplies is the nested-set
// NUMBERING — walking a trace's parent/child tree and assigning
// pre/post-order numbers. That is ordinary tree numbering, the same
// data-preparation step upstream performs when it WRITES a block, and it is
// emphatically not the operator semantics. Cerberus's `>>` / `>` / `~`
// lowering is therefore checked against Tempo's real implementation of
// those operators, which is what makes this an oracle rather than a mirror.
//
// # Why the input is rows, not a seed
//
// Evaluate takes spans already read back out of the seeded chDB session
// rather than the fixture's `-- seed --` SQL, for the same two reasons the
// PromQL oracle does: it keeps every chDB concern in package spec where the
// session mutex lives, and it means the oracle sees the data as it ACTUALLY
// LANDED in ClickHouse — after DEFAULTs, after coercion — rather than as
// the seed text claims it will land.
//
// # The one place this package must restate a cerberus fact
//
// OTel-ClickHouse stores span attributes as Map(String, String) and the
// status/kind enums as TitleCase strings. Turning those back into
// traceql.Static values is a fact about the STORAGE ENCODING, not about
// query semantics, so restating it here costs the oracle nothing: an
// encoding disagreement would show up as a total mismatch on the very first
// enrolled fixture, not as a subtly wrong answer. Every attribute therefore
// enters the engine as a string, which is the honest reading of what the
// column holds — EXCEPT for the one case attrTypeHints exists to handle
// (see its doc comment): an attribute the query itself compares against a
// typed literal, where cerberus casts the stored string rather than
// stringifying the literal, and the oracle must cast it the same way to
// stay an honest comparison rather than a storage-encoding artifact.
package traceql

import (
	"fmt"
	"sort"
	"strconv"
	"testing"
	"time"

	tempotraceql "github.com/grafana/tempo/pkg/traceql"
	"github.com/grafana/tempo/tempodb/encoding/vparquet4"
)

// Span is one seeded span, exactly as it was read back out of ClickHouse.
// Field names follow the OTel-CH trace columns they came from.
type Span struct {
	TraceID       string
	SpanID        string
	ParentSpanID  string
	Name          string
	Kind          string
	StatusCode    string
	StatusMessage string
	StartUnixNano uint64
	DurationNanos uint64
	ResourceAttrs map[string]string
	SpanAttrs     map[string]string

	// ServiceName is OTel-ClickHouse's dedicated column for the
	// resource attribute of the same name. The exporter writes the value
	// to BOTH the column and the ResourceAttributes map, but a seed is
	// free to populate only one, so the two are merged in
	// resourceAttributes below rather than one being preferred.
	ServiceName string

	// ScopeName and ScopeVersion back the `instrumentation:` intrinsics.
	// They are OTel-CH's own scalar columns, not attribute maps.
	ScopeName    string
	ScopeVersion string

	// Events and Links are the span's nested child records, read back
	// from OTel-CH's `Events.*` and `Links.*` array columns. See
	// [scopedChildRecords] for why at most one of each is accepted.
	Events []Event
	Links  []Link
}

// Event is one entry of a span's `Events.*` nested columns: the event
// name plus that event's own attribute map.
type Event struct {
	Name  string
	Attrs map[string]string
}

// Link is one entry of a span's `Links.*` nested columns: the linked
// span's identity plus that link's own attribute map.
type Link struct {
	TraceID string
	SpanID  string
	Attrs   map[string]string
}

// resourceAttributes is the span's resource scope as the engine must see
// it: the ResourceAttributes map, plus service.name recovered from the
// dedicated column when the map does not carry it.
//
// The map wins where both are set. That is not a preference between two
// sources of truth — it is the reading that keeps a fixture which
// deliberately seeds a service.name into the map from being overridden by
// a column it never set.
func (s Span) resourceAttributes() map[string]string {
	if s.ServiceName == "" {
		return s.ResourceAttrs
	}
	if _, ok := s.ResourceAttrs[serviceNameAttr]; ok {
		return s.ResourceAttrs
	}
	merged := make(map[string]string, len(s.ResourceAttrs)+1)
	for k, v := range s.ResourceAttrs {
		merged[k] = v
	}
	merged[serviceNameAttr] = s.ServiceName
	return merged
}

// Result is one span the reference engine matched, identified the same way
// cerberus's projection identifies it.
type Result struct {
	TraceID string
	SpanID  string
}

// Evaluate runs query against the real Tempo engine over spans, and returns
// the matched spans sorted deterministically.
//
// It returns an error rather than failing the test so the caller can
// attribute a failure precisely: a Tempo PARSE error on a fixture's query
// usually means the fixture exercises a cerberus extension upstream does
// not accept, which is a fact about the fixture and not a parity failure.
func Evaluate(tb testing.TB, spans []Span, query string) ([]Result, error) {
	tb.Helper()

	root, evaluate, _, _, _, err := tempotraceql.Compile(query)
	if err != nil {
		return nil, fmt.Errorf("reference engine rejected the query: %w", err)
	}
	hints := collectAttrTypeHints(root)

	var out []Result
	for _, trace := range groupByTrace(spans) {
		built, err := buildTrace(trace, hints)
		if err != nil {
			return nil, err
		}

		// The engine evaluates one trace's spanset at a time — that is
		// exactly what ExecuteSearch's SecondPass does per fetched
		// spanset — so structural operators never reach across traces,
		// which is the semantics cerberus's TraceId-keyed joins encode.
		results, err := evaluate([]*tempotraceql.Spanset{{
			TraceID: []byte(trace.id),
			Spans:   built.spans,
		}})
		if err != nil {
			return nil, fmt.Errorf("reference engine evaluation failed on trace %s: %w", trace.id, err)
		}

		for _, ss := range results {
			for _, s := range ss.Spans {
				id, ok := built.idOf[s]
				if !ok {
					return nil, fmt.Errorf(
						"reference engine returned a span on trace %s that was never fed to it; "+
							"the oracle cannot identify it", trace.id,
					)
				}
				out = append(out, Result{TraceID: trace.id, SpanID: id})
			}
		}
	}

	return dedupeAndSort(out), nil
}

// --- trace grouping and nested-set numbering -------------------------

// trace is one trace's spans, in a deterministic order.
type trace struct {
	id    string
	spans []Span
}

// groupByTrace buckets spans by trace and orders both the traces and the
// spans within a trace by ID, so a run's result never depends on the order
// ClickHouse happened to return rows in.
func groupByTrace(spans []Span) []trace {
	byID := map[string][]Span{}
	for _, s := range spans {
		byID[s.TraceID] = append(byID[s.TraceID], s)
	}

	out := make([]trace, 0, len(byID))
	for id, group := range byID {
		sort.Slice(group, func(i, j int) bool { return group[i].SpanID < group[j].SpanID })
		out = append(out, trace{id: id, spans: group})
	}
	sort.Slice(out, func(i, j int) bool { return out[i].id < out[j].id })
	return out
}

// builtTrace is one trace turned into engine spans, plus the reverse map
// that turns an engine span back into the span ID cerberus projects.
type builtTrace struct {
	spans []tempotraceql.Span
	idOf  map[tempotraceql.Span]string
}

// buildTrace numbers a trace's parent/child forest and constructs the
// engine spans.
//
// # The numbering
//
// Standard nested-set (modified preorder) numbering: walking the forest
// depth-first, each span takes `left` on the way down and `right` on the
// way up, and records its parent's `left` as its own `nestedSetParent`.
// That is precisely the invariant upstream's operators decode —
// descendant is `left > ancestor.left && right < ancestor.right`, child is
// `parent.left == child.nestedSetParent` — and it is what a block writer
// computes when it stores a trace.
//
// Numbering starts at 1 because upstream treats 0 as "missing data, never a
// match" for all three fields (see the guards in DescendantOf and
// SiblingOf).
//
// Two details are copied from upstream's own writer
// (vparquet4/nested_set_model.go) rather than invented here, because
// getting either wrong would make the oracle disagree with Tempo about
// something that is not the query:
//
//   - A ROOT — a span whose ParentSpanID is EMPTY — takes the sentinel
//     parent -1, not 0. That is load-bearing: SiblingOf's rule is
//     "same nestedSetParent, and neither is 0", so under the sentinel two
//     roots of one trace ARE siblings, where 0 would have made them not.
//
//   - A span with a NON-EMPTY ParentSpanID naming no span in the trace is
//     not a root. Upstream's walk starts only from real roots, so such a
//     span is never reached and keeps 0/0/0 — present in the spanset, able
//     to match plain filters, and never a match for any structural
//     operator. Numbering it as a root here would invent a relationship
//     upstream does not see.
func buildTrace(t trace, hints attrTypeHints) (builtTrace, error) {
	children := map[string][]Span{}
	present := make(map[string]bool, len(t.spans))
	for _, s := range t.spans {
		if present[s.SpanID] {
			return builtTrace{}, fmt.Errorf(
				"trace %s: span ID %s appears on more than one row, so the seeded parent/child "+
					"edges are not a tree and cannot be nested-set numbered",
				t.id, s.SpanID,
			)
		}
		present[s.SpanID] = true
		if err := validateChildRecords(s); err != nil {
			return builtTrace{}, err
		}
	}

	var roots []Span
	for _, s := range t.spans {
		if s.ParentSpanID == "" {
			roots = append(roots, s)
			continue
		}
		children[s.ParentSpanID] = append(children[s.ParentSpanID], s)
	}

	built := builtTrace{
		spans: make([]tempotraceql.Span, 0, len(t.spans)),
		idOf:  make(map[tempotraceql.Span]string, len(t.spans)),
	}
	meta := traceMetaOf(t, roots)

	add := func(s Span, parentLeft, left, right int32) {
		engineSpan := newEngineSpan(s, meta, parentLeft, left, right, hints)
		built.spans = append(built.spans, engineSpan)
		built.idOf[engineSpan] = s.SpanID
	}

	// nestedSetCounterOrigin is 1 because upstream reads 0 as "this span
	// carries no nested-set data" and refuses to match on it.
	const nestedSetCounterOrigin = 1
	counter := int32(nestedSetCounterOrigin)

	visited := make(map[string]bool, len(t.spans))
	var walk func(s Span, parentLeft int32)
	walk = func(s Span, parentLeft int32) {
		// Unique span IDs are checked above, so no span can be reached
		// twice and the recursion always terminates.
		visited[s.SpanID] = true

		left := counter
		counter++
		kids := children[s.SpanID]
		sort.Slice(kids, func(i, j int) bool { return kids[i].SpanID < kids[j].SpanID })
		for _, kid := range kids {
			walk(kid, left)
		}
		right := counter
		counter++

		add(s, parentLeft, left, right)
	}

	// nestedSetRootParent mirrors upstream's sentinel of the same name.
	const nestedSetRootParent = -1
	for _, r := range roots {
		walk(r, nestedSetRootParent)
	}

	// unnumberedBound is what upstream leaves on a span its walk never
	// reaches, and what its operators read as "missing data".
	//
	// Reaching every span is NOT required, and treating a gap as an error
	// would be the oracle inventing a rule: upstream's walk descends only
	// from real roots and simply reports the trace as disconnected,
	// leaving whatever it did not reach at 0/0/0. That covers a chain
	// hanging off an absent parent and a ParentSpanId cycle alike — both
	// are unreachable from a root, and both are already handled by
	// numbering nothing.
	const unnumberedBound = 0
	for _, s := range t.spans {
		if visited[s.SpanID] {
			continue
		}
		add(s, unnumberedBound, unnumberedBound, unnumberedBound)
	}

	return built, nil
}

// traceMeta is the trace-level state the `trace:` intrinsics read.
type traceMeta struct {
	id              string
	durationNanos   uint64
	rootSpanName    string
	rootServiceName string
}

// serviceNameAttr is the resource attribute upstream reads a trace's root
// service name from.
const serviceNameAttr = "service.name"

// traceMetaOf derives the trace-level intrinsics the same way upstream's
// writer does: the duration is the whole trace's span (max end minus min
// start) rather than any one span's, and the root name/service come from
// the root span.
//
// Upstream takes the FIRST root it encounters while scanning the trace,
// which is an order it never defines. Taking the lowest span ID makes the
// choice deterministic here; a trace with two roots and two different root
// names has no defined answer on either side, and a fixture that depended
// on one is asking about scan order rather than about TraceQL.
func traceMetaOf(t trace, roots []Span) traceMeta {
	meta := traceMeta{id: t.id}

	var start, end uint64
	for i, s := range t.spans {
		spanEnd := s.StartUnixNano + s.DurationNanos
		if i == 0 || s.StartUnixNano < start {
			start = s.StartUnixNano
		}
		if i == 0 || spanEnd > end {
			end = spanEnd
		}
	}
	meta.durationNanos = end - start

	if len(roots) > 0 {
		root := roots[0]
		for _, r := range roots[1:] {
			if r.SpanID < root.SpanID {
				root = r
			}
		}
		meta.rootSpanName = root.Name
		meta.rootServiceName = root.resourceAttributes()[serviceNameAttr]
	}
	return meta
}

// newEngineSpan constructs the vparquet4 span the engine will evaluate,
// populating the same attribute scopes upstream's own parquet decoder does.
func newEngineSpan(s Span, meta traceMeta, parentLeft, left, right int32, hints attrTypeHints) tempotraceql.Span {
	spanAttrs := []vparquet4.SpanAttr{
		{Attr: tempotraceql.IntrinsicSpanIDAttribute, Value: tempotraceql.NewStaticString(s.SpanID)},
		{Attr: tempotraceql.IntrinsicParentIDAttribute, Value: tempotraceql.NewStaticString(s.ParentSpanID)},
		{Attr: tempotraceql.IntrinsicNameAttribute, Value: tempotraceql.NewStaticString(s.Name)},
		{
			Attr:  tempotraceql.IntrinsicDurationAttribute,
			Value: tempotraceql.NewStaticDuration(time.Duration(s.DurationNanos)), //nolint:gosec // nanosecond count, not a conversion between signed domains.
		},
		{Attr: tempotraceql.IntrinsicStatusAttribute, Value: tempotraceql.NewStaticStatus(statusFromColumn(s.StatusCode))},
		{Attr: tempotraceql.IntrinsicStatusMessageAttribute, Value: tempotraceql.NewStaticString(s.StatusMessage)},
		{Attr: tempotraceql.IntrinsicKindAttribute, Value: tempotraceql.NewStaticKind(kindFromColumn(s.Kind))},
		{Attr: tempotraceql.IntrinsicNestedSetParentAttribute, Value: tempotraceql.NewStaticInt(int(parentLeft))},
		{Attr: tempotraceql.IntrinsicNestedSetLeftAttribute, Value: tempotraceql.NewStaticInt(int(left))},
		{Attr: tempotraceql.IntrinsicNestedSetRightAttribute, Value: tempotraceql.NewStaticInt(int(right))},
	}
	spanAttrs = appendScoped(spanAttrs, tempotraceql.AttributeScopeSpan, s.SpanAttrs, hints)

	resourceAttrs := appendScoped(nil, tempotraceql.AttributeScopeResource, s.resourceAttributes(), hints)

	eventAttrs, linkAttrs := s.childScopeAttrs(hints)
	instrumentationAttrs := []vparquet4.SpanAttr{
		{Attr: tempotraceql.IntrinsicInstrumentationNameAttribute, Value: tempotraceql.NewStaticString(s.ScopeName)},
		{Attr: tempotraceql.IntrinsicInstrumentationVersionAttribute, Value: tempotraceql.NewStaticString(s.ScopeVersion)},
	}

	// The `trace:` intrinsics are per-TRACE facts that upstream's decoder
	// nonetheless hangs off every span, because the engine resolves an
	// attribute through whichever span it is currently evaluating.
	traceAttrs := []vparquet4.SpanAttr{
		{Attr: tempotraceql.IntrinsicTraceIDAttribute, Value: tempotraceql.NewStaticString(meta.id)},
		{
			Attr:  tempotraceql.IntrinsicTraceDurationAttribute,
			Value: tempotraceql.NewStaticDuration(time.Duration(meta.durationNanos)), //nolint:gosec // nanosecond count, not a signed-domain conversion.
		},
		{Attr: tempotraceql.IntrinsicTraceRootSpanAttribute, Value: tempotraceql.NewStaticString(meta.rootSpanName)},
		{Attr: tempotraceql.IntrinsicTraceRootServiceAttribute, Value: tempotraceql.NewStaticString(meta.rootServiceName)},
	}

	return vparquet4.NewSpan(vparquet4.SpanData{
		ID:                   []byte(s.SpanID),
		StartTimeUnixNanos:   s.StartUnixNano,
		DurationNanos:        s.DurationNanos,
		NestedSetParent:      parentLeft,
		NestedSetLeft:        left,
		NestedSetRight:       right,
		SpanAttrs:            spanAttrs,
		ResourceAttrs:        resourceAttrs,
		TraceAttrs:           traceAttrs,
		EventAttrs:           eventAttrs,
		LinkAttrs:            linkAttrs,
		InstrumentationAttrs: instrumentationAttrs,
	})
}

// childScopeAttrs renders the span's events and links into the flat
// `event.` and `link.` attribute scopes upstream's own decoder builds.
//
// The flattening is upstream's, not this package's invention: vparquet4
// decodes every event's attributes into ONE EventAttrs slice and every
// link's into ONE LinkAttrs slice, and the engine's AttributeFor resolves
// a scoped read against that flat slice. Per-event and per-link matching
// happens in Tempo's FETCH layer, which this in-process oracle does not
// run.
//
// That is exactly why [validateChildRecords] refuses a span carrying more
// than one event or more than one link: with two events the flat slice
// can hold the same key twice, AttributeFor answers with whichever came
// first, and the oracle would report a confident wrong answer where real
// Tempo matches on the other event. One event and one link per span is
// the region where the flat model and the fetch layer cannot disagree.
func (s Span) childScopeAttrs(hints attrTypeHints) (events, links []vparquet4.SpanAttr) {
	for _, e := range s.Events {
		events = append(events, vparquet4.SpanAttr{
			Attr:  tempotraceql.IntrinsicEventNameAttribute,
			Value: tempotraceql.NewStaticString(e.Name),
		})
		events = appendScoped(events, tempotraceql.AttributeScopeEvent, e.Attrs, hints)
	}
	for _, l := range s.Links {
		links = append(
			links,
			vparquet4.SpanAttr{
				Attr:  tempotraceql.IntrinsicLinkTraceIDAttribute,
				Value: tempotraceql.NewStaticString(l.TraceID),
			},
			vparquet4.SpanAttr{
				Attr:  tempotraceql.IntrinsicLinkSpanIDAttribute,
				Value: tempotraceql.NewStaticString(l.SpanID),
			},
		)
		links = appendScoped(links, tempotraceql.AttributeScopeLink, l.Attrs, hints)
	}
	return events, links
}

// validateChildRecords refuses a span the flat event/link model cannot
// represent faithfully. See [Span.childScopeAttrs] for why more than one
// of either is the boundary.
func validateChildRecords(s Span) error {
	const maxFlattenableChildRecords = 1
	if len(s.Events) > maxFlattenableChildRecords {
		return fmt.Errorf(
			"span %s carries %d events; the in-process oracle flattens every event's attributes "+
				"into one `event.` scope, so a second event's value for the same key would be "+
				"invisible to it while real Tempo's fetch layer matches on it",
			s.SpanID, len(s.Events),
		)
	}
	if len(s.Links) > maxFlattenableChildRecords {
		return fmt.Errorf(
			"span %s carries %d links; the in-process oracle flattens every link's attributes "+
				"into one `link.` scope, so a second link's value for the same key would be "+
				"invisible to it while real Tempo's fetch layer matches on it",
			s.SpanID, len(s.Links),
		)
	}
	return nil
}

// appendScoped adds an attribute map under one scope, in sorted key order
// so construction is deterministic. Each value is a Go string — the honest
// reading of the Map(String, String) column it came from — UNLESS hints
// says the query itself compares this key against a typed literal, in
// which case coerceAttrValue reads it as that type instead (see
// attrTypeHints's doc comment for why this is sound and cerberus does the
// same coercion).
func appendScoped(
	dst []vparquet4.SpanAttr, scope tempotraceql.AttributeScope, attrs map[string]string, hints attrTypeHints,
) []vparquet4.SpanAttr {
	keys := make([]string, 0, len(attrs))
	for k := range attrs {
		keys = append(keys, k)
	}
	sort.Strings(keys)
	for _, k := range keys {
		dst = append(dst, vparquet4.SpanAttr{
			Attr:  tempotraceql.NewScopedAttribute(scope, false, k),
			Value: coerceAttrValue(attrs[k], attrHint(hints, scope, k)),
		})
	}
	return dst
}

// attrHint looks up the type hint for a concretely-scoped attribute
// (span.foo, resource.foo, …), falling back to the hint recorded for the
// UNSCOPED spelling of the same name (.foo) when there is no scope-specific
// one. An unscoped query attribute resolves against span first, then
// resource — the same search order cerberus's own lowering emits as
// `if(mapContains(SpanAttributes, k), SpanAttributes[k], ResourceAttributes[k])`
// — so a comparison written as `.foo = true` must coerce whichever of the
// two concrete maps actually holds `foo`, not only a hit under
// AttributeScopeNone (which nothing in either map is ever stored under).
func attrHint(hints attrTypeHints, scope tempotraceql.AttributeScope, name string) tempotraceql.StaticType {
	if hint := hints[attrTypeHintKey{scope: scope, name: name}]; hint != tempotraceql.TypeNil {
		return hint
	}
	return hints[attrTypeHintKey{scope: tempotraceql.AttributeScopeNone, name: name}]
}

// --- attribute type hints (issue #3259) --------------------------------
//
// OTel-ClickHouse stores every ordinary span/resource attribute as
// Map(String, String), which loses the original OTel value type (an
// IntValue and a StringValue holding "500" land in the same cell) —
// unrecoverably, since nothing in the column records which one it was.
// That loss is real and belongs to the exporter's schema, not to this
// harness; nothing here tries to undo it.
//
// What this harness CAN observe, without recovering that lost type, is
// what cerberus itself does with the string: internal/traceql/lower.go
// casts the STORED STRING to match whatever type the QUERY'S OWN LITERAL
// is (`toFloat64OrNull` for a numeric/duration literal, the literal
// stringified to match a boolean attribute's encoding) rather than
// trusting a type the string was never guaranteed to carry. Mirroring that
// same per-comparison cast here — keyed off the query's literal type, not
// off guessing from the string's shape — lets the oracle agree with
// cerberus for exactly the fixtures that exercise it, with no schema
// change and no risk to any fixture that does not: a key never compared
// against a typed literal keeps reading as a plain string, unchanged.
//
// Guessing from the string's shape ALONE (e.g. "every numeric-looking
// value is a number") would be unsound, not merely imprecise: a fixture
// seeding a genuinely STRING attribute whose value happens to look
// numeric — an account ID "500" queried as `{ span.account_id = "500" }`
// — would then get silently retyped to Int, mismatching the query's own
// String literal and turning a passing fixture into a false parity
// failure. Reading the QUERY's literal type instead of the VALUE's shape
// is what keeps this sound: cerberus's own coercion is equally
// query-driven (a String literal never gets cast either), so this mirrors
// the fact under test rather than inventing a new one.

// attrTypeHintKey identifies one ordinary (non-intrinsic) attribute by
// scope and name. Parent-qualified references (`parent.span.foo`) resolve
// through the SAME per-span attribute set on whichever span the engine is
// currently walking, so the hint is keyed on scope+name only — the parent
// bit changes which span's map is consulted, never what type that span's
// value should be read as.
type attrTypeHintKey struct {
	scope tempotraceql.AttributeScope
	name  string
}

// attrTypeHints maps an attribute to the StaticType a fixture's query
// compares it against, for the coercible types (TypeBoolean, TypeInt,
// TypeFloat, TypeDuration). A key absent from the map — the common case —
// reads as a plain string, same as before this mechanism existed.
type attrTypeHints map[attrTypeHintKey]tempotraceql.StaticType

// isCoercibleHintType reports whether t is a type coerceAttrValue knows
// how to recover from a stored string. TypeString itself, and every other
// StaticType, is deliberately excluded: a String comparison never coerces
// (matching cerberus, which only casts for a NUMERIC or BOOLEAN literal),
// and the array/status/kind types have no single scalar parse to attempt.
func isCoercibleHintType(t tempotraceql.StaticType) bool {
	switch t {
	case tempotraceql.TypeBoolean, tempotraceql.TypeInt, tempotraceql.TypeFloat, tempotraceql.TypeDuration:
		return true
	default:
		return false
	}
}

// collectAttrTypeHints walks a compiled query's spanset-filter pipeline —
// the portion Evaluate's SpansetFilterFunc actually runs — and records,
// for every ordinary attribute compared against a coercible literal, which
// type that comparison implies. An attribute reached only through an
// intrinsic, or only ever compared against a String (or another
// attribute), is not recorded, and its value keeps reading as a string.
func collectAttrTypeHints(root *tempotraceql.RootExpr) attrTypeHints {
	hints := attrTypeHints{}

	record := func(candidate, literal tempotraceql.FieldExpression) {
		attr, ok := candidate.(tempotraceql.Attribute)
		if !ok || attr.Intrinsic != tempotraceql.IntrinsicNone {
			return
		}
		static, ok := literal.(tempotraceql.Static)
		if !ok || !isCoercibleHintType(static.Type) {
			return
		}
		hints[attrTypeHintKey{scope: attr.Scope, name: attr.Name}] = static.Type
	}

	var walkField func(tempotraceql.FieldExpression)
	walkField = func(e tempotraceql.FieldExpression) {
		switch n := e.(type) {
		case *tempotraceql.BinaryOperation:
			record(n.LHS, n.RHS)
			record(n.RHS, n.LHS)
			walkField(n.LHS)
			walkField(n.RHS)
		case tempotraceql.UnaryOperation:
			walkField(n.Expression)
		}
	}

	var walkSpanset func(tempotraceql.SpansetExpression)
	walkSpanset = func(e tempotraceql.SpansetExpression) {
		switch n := e.(type) {
		case *tempotraceql.SpansetFilter:
			walkField(n.Expression)
		case tempotraceql.SpansetOperation:
			walkSpanset(n.LHS)
			walkSpanset(n.RHS)
		}
	}

	for _, el := range root.Pipeline.Elements {
		if se, ok := el.(tempotraceql.SpansetExpression); ok {
			walkSpanset(se)
		}
	}
	return hints
}

// coerceAttrValue reads raw as hint's type when hint is coercible and raw
// actually parses as that type, matching the ONE cast cerberus's own
// lowering performs for the same Map(String, String) column
// (internal/traceql/lower.go's coerceNumericFieldAccess /
// coerceBoolFieldAccess): Int, Float and Duration all fold to a float
// comparison in both engines (upstream Static.compare/Equals treat them as
// one numeric family), so a single ParseFloat covers all three — cerberus
// itself never distinguishes them either, casting every numeric literal's
// peer through the same toFloat64OrNull. A value that does not parse as
// the hinted type — or a key with no hint at all — reads as the plain
// string the column holds, the same as every other attribute.
func coerceAttrValue(raw string, hint tempotraceql.StaticType) tempotraceql.Static {
	switch hint {
	case tempotraceql.TypeBoolean:
		if b, err := strconv.ParseBool(raw); err == nil {
			return tempotraceql.NewStaticBool(b)
		}
	case tempotraceql.TypeInt, tempotraceql.TypeFloat, tempotraceql.TypeDuration:
		if f, err := strconv.ParseFloat(raw, 64); err == nil {
			return tempotraceql.NewStaticFloat(f)
		}
	}
	return tempotraceql.NewStaticString(raw)
}

// statusFromColumn decodes the OTel-CH StatusCode column.
//
// The column holds TitleCase strings ("Unset" / "Ok" / "Error"); an empty
// column — which several fixtures seed by DEFAULT — is Unset, the same
// reading upstream's own decoder gives an absent status.
func statusFromColumn(code string) tempotraceql.Status {
	switch code {
	case "Error":
		return tempotraceql.StatusError
	case "Ok":
		return tempotraceql.StatusOk
	default:
		return tempotraceql.StatusUnset
	}
}

// kindFromColumn decodes the OTel-CH SpanKind column, which holds TitleCase
// strings. Anything else — an empty column, or any other spelling — is
// Unspecified.
//
// The match is EXACT, for the same reason statusFromColumn's is: the
// TitleCase spelling is the storage encoding the OTel-CH exporter writes,
// so a case-folding decode would not be leniency about presentation, it
// would be the oracle recognising a value the column cannot hold. It
// would also make the oracle disagree with cerberus about data rather
// than about the query — cerberus emits `SpanKind = 'Client'` verbatim
// (internal/traceql/lower.go's kind rendering), so a row spelling the
// kind any other way is a non-match on the cerberus side and must be one
// here too.
func kindFromColumn(kind string) tempotraceql.Kind {
	switch kind {
	case "Internal":
		return tempotraceql.KindInternal
	case "Client":
		return tempotraceql.KindClient
	case "Server":
		return tempotraceql.KindServer
	case "Producer":
		return tempotraceql.KindProducer
	case "Consumer":
		return tempotraceql.KindConsumer
	default:
		return tempotraceql.KindUnspecified
	}
}

// dedupeAndSort collapses a span that several spansets matched and orders
// the result, so comparison never depends on the engine's internal
// ordering or on how a pipeline happened to fan out.
//
// Collapsing is correct rather than lenient: cerberus's projection returns
// each matched span once, so counting a span twice here would compare
// multiplicities neither side claims to model.
func dedupeAndSort(rs []Result) []Result {
	seen := make(map[Result]bool, len(rs))
	out := make([]Result, 0, len(rs))
	for _, r := range rs {
		if seen[r] {
			continue
		}
		seen[r] = true
		out = append(out, r)
	}
	sort.Slice(out, func(i, j int) bool {
		if out[i].TraceID != out[j].TraceID {
			return out[i].TraceID < out[j].TraceID
		}
		return out[i].SpanID < out[j].SpanID
	})
	return out
}
