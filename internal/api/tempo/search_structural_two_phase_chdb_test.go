//go:build chdb

// chDB-backed parity + memory-bound proof for the /api/search two-phase
// structural fetch (fix/traceql-structural-twophase). A positive recursive
// structural search (`A >> B`) used to lower to ONE query: a WITH RECURSIVE
// closure INNER JOINed to the WIDE R projection, materialising every matched
// span's ResourceAttributes/SpanAttributes maps only to return the top-N
// traces — the prod OOM. The handler now splits it: phase A ranks narrowly to
// the top-N TraceIds, phase B re-runs the closure projecting wide but
// restricted to just those traces.
//
// The correctness bar is that the two-phase result is byte-identical to what
// the single wide query + toTraceSummaries + sort + TruncateSummaries produce.
// This test proves it WITHOUT a test hook, using a structural property: when
// the request limit is >= the number of matching traces, phase A returns ALL
// of them, so phase B's `TraceId IN (<all ids>)` restriction is a no-op and
// the response is exactly the single wide query's result, fully ranked. That
// full response is therefore the single-query reference; a small-limit
// two-phase response must equal its first-N prefix — proving phase A selects
// exactly the traces TruncateSummaries would keep.
package tempo_test

import (
	"fmt"
	"net/url"
	"reflect"
	"strconv"
	"testing"
)

// structTwoPhaseTraceCount traces, each a root-svc root with
// structTwoPhaseLeavesPerTrace leaf-svc descendants, so
// `{ resource.service.name = "root-svc" } >> { resource.service.name =
// "leaf-svc" }` matches every leaf. Each trace's startNS is the min of its
// matched (leaf) timestamps; trace i's leaves sit at minute i so the ranking
// (min-start DESC) is the highest-i-first order, deterministic without ties.
const (
	structTwoPhaseTraceCount     = 6
	structTwoPhaseLeavesPerTrace = 2
	structTwoPhaseSmallLimit     = 2
)

// structuralTwoPhaseSeed builds the ancestor/descendant fixture. Root span:
// service.name=root-svc, ParentSpanId=” (a true root). Leaf spans:
// service.name=leaf-svc, parented at the root (so they are descendants the
// closure reaches). Both sides are selective Filter(Scan) leaves, so the
// candidate prefilter also fires — this fixture exercises wins (b) + (c)
// together.
func structuralTwoPhaseSeed() string {
	rows := make([]string, 0, structTwoPhaseTraceCount*(1+structTwoPhaseLeavesPerTrace))
	for i := 1; i <= structTwoPhaseTraceCount; i++ {
		traceID := fmt.Sprintf("a%031x", i)
		root := fmt.Sprintf("%016x", i*100+1)
		rootTS := fmt.Sprintf("2026-05-01 10:%02d:00.000000001", i)
		rows = append(rows, fmt.Sprintf(
			"('%s', '%s', '', 'root', 'Server', 1000, toDateTime64('%s', 9), 'Unset', '', '', '', map(), map('service.name', 'root-svc'))",
			traceID, root, rootTS,
		))
		for j := 1; j <= structTwoPhaseLeavesPerTrace; j++ {
			leaf := fmt.Sprintf("%016x", i*100+1+j)
			// Leaf timestamps a few ns after the root; the earliest leaf is the
			// trace's matched-min start.
			leafTS := fmt.Sprintf("2026-05-01 10:%02d:00.00000000%d", i, j+1)
			rows = append(rows, fmt.Sprintf(
				"('%s', '%s', '%s', 'leaf-%d', 'Internal', %d, toDateTime64('%s', 9), 'Unset', '', '', '', map(), map('service.name', 'leaf-svc'))",
				traceID, leaf, root, j, 500+j, leafTS,
			))
		}
	}
	insert := "INSERT INTO otel_traces VALUES\n    "
	for i, r := range rows {
		if i > 0 {
			insert += ",\n    "
		}
		insert += r
	}
	return insert + ";"
}

const structuralQuery = `{ resource.service.name = "root-svc" } >> { resource.service.name = "leaf-svc" }`

// TestSearch_StructuralTwoPhase_Parity_ChDB proves the two-phase fetch returns
// a result byte-identical to the single wide query, and that the small-limit
// fetch is memory-bounded (its drain is strictly smaller than the full fetch's).
func TestSearch_StructuralTwoPhase_Parity_ChDB(t *testing.T) {
	srv := newManyTracesChDBServer(t, structuralTwoPhaseSeed())

	q := url.QueryEscape(structuralQuery)
	// limit >= matching traces: phase A returns ALL trace ids, phase B's
	// `TraceId IN (<all>)` is a no-op restriction, so this is exactly the
	// single wide query's fully-ranked result — the parity reference.
	fullLimit := structTwoPhaseTraceCount + 5
	full := doSearch(t, srv, fmt.Sprintf("/api/search?q=%s&limit=%d&spss=20%s", q, fullLimit, seedWindow))
	small := doSearch(t, srv, fmt.Sprintf("/api/search?q=%s&limit=%d&spss=20%s", q, structTwoPhaseSmallLimit, seedWindow))

	// The reference must actually contain every matching trace (guard against a
	// degenerate seed that would make the prefix comparison vacuous).
	if len(full.Traces) != structTwoPhaseTraceCount {
		t.Fatalf("full fetch returned %d traces, want all %d (limit=%d) — seed or closure misconfigured",
			len(full.Traces), structTwoPhaseTraceCount, fullLimit)
	}
	// Ranking: min-start DESC means the highest-i traces come first.
	for rank := 0; rank < len(full.Traces); rank++ {
		wantID := fmt.Sprintf("a%031x", structTwoPhaseTraceCount-rank)
		if full.Traces[rank].TraceID != wantID {
			t.Errorf("full.Traces[%d] = %q, want %q (min-start DESC)", rank, full.Traces[rank].TraceID, wantID)
		}
	}
	// Whole-list start-desc invariant.
	for i := 1; i < len(full.Traces); i++ {
		prev, _ := strconv.ParseUint(full.Traces[i-1].StartTimeUnixNano, 10, 64)
		cur, _ := strconv.ParseUint(full.Traces[i].StartTimeUnixNano, 10, 64)
		if prev < cur {
			t.Errorf("full fetch not start-desc at %d: %d < %d", i, prev, cur)
		}
	}

	// ★ Parity: the small-limit two-phase response equals the single-query
	// reference truncated to the same limit — same TraceIDs, order, start
	// times, durations, root metadata, and spansets, field-for-field.
	if len(small.Traces) != structTwoPhaseSmallLimit {
		t.Fatalf("small fetch returned %d traces, want %d", len(small.Traces), structTwoPhaseSmallLimit)
	}
	for i := 0; i < structTwoPhaseSmallLimit; i++ {
		if !reflect.DeepEqual(small.Traces[i], full.Traces[i]) {
			t.Errorf("two-phase divergence at trace %d:\n  two-phase(limit=%d): %+v\n  single-query prefix:  %+v",
				i, structTwoPhaseSmallLimit, small.Traces[i], full.Traces[i])
		}
	}

	// ★ Memory bound: the small fetch drains strictly fewer spans than the full
	// fetch — phase B materialised the wide projection for only the top-N
	// traces, not every matched span. Without the two-phase restriction the
	// small-limit query would drain the full match set (the OOM).
	if small.Metrics.InspectedTraces >= full.Metrics.InspectedTraces {
		t.Errorf("small-limit drain (%d spans) not bounded below full drain (%d spans) — the wide fetch was not restricted to the top-N",
			small.Metrics.InspectedTraces, full.Metrics.InspectedTraces)
	}
	// The small fetch drains exactly its kept traces' matched spans.
	wantSmallDrain := structTwoPhaseSmallLimit * structTwoPhaseLeavesPerTrace
	if small.Metrics.InspectedTraces != wantSmallDrain {
		t.Errorf("small-limit drain = %d spans, want %d (%d traces * %d matched leaves)",
			small.Metrics.InspectedTraces, wantSmallDrain, structTwoPhaseSmallLimit, structTwoPhaseLeavesPerTrace)
	}
}

// TestSearch_WrappedSelectTwoPhase_Parity_ChDB proves the two-phase seam engages
// for a row-preserving `| select(...)` Project wrapped over the positive closure,
// and stays byte-identical + memory-bounded. If the gate did NOT unwrap the
// Project, this query would fall to the single-query path and the small-limit
// drain would NOT be bounded below the full drain (no top-N restriction) — so the
// memory-bound assertion below is what proves the wrapped shape actually routed
// two-phase.
func TestSearch_WrappedSelectTwoPhase_Parity_ChDB(t *testing.T) {
	srv := newManyTracesChDBServer(t, structuralTwoPhaseSeed())

	// A pure column re-projection over the same closure. select(...) does not
	// drop rows or mutate Timestamp, so phase A still ranks over the inner join.
	q := url.QueryEscape(structuralQuery + ` | select(resource.service.name)`)
	fullLimit := structTwoPhaseTraceCount + 5
	full := doSearch(t, srv, fmt.Sprintf("/api/search?q=%s&limit=%d&spss=20%s", q, fullLimit, seedWindow))
	small := doSearch(t, srv, fmt.Sprintf("/api/search?q=%s&limit=%d&spss=20%s", q, structTwoPhaseSmallLimit, seedWindow))

	if len(full.Traces) != structTwoPhaseTraceCount {
		t.Fatalf("full fetch returned %d traces, want all %d — wrapped-select closure misconfigured",
			len(full.Traces), structTwoPhaseTraceCount)
	}
	if len(small.Traces) != structTwoPhaseSmallLimit {
		t.Fatalf("small fetch returned %d traces, want %d", len(small.Traces), structTwoPhaseSmallLimit)
	}
	// Parity: small-limit two-phase == single-query reference prefix, field-for-field.
	for i := 0; i < structTwoPhaseSmallLimit; i++ {
		if !reflect.DeepEqual(small.Traces[i], full.Traces[i]) {
			t.Errorf("wrapped-select two-phase divergence at trace %d:\n  two-phase: %+v\n  reference: %+v",
				i, small.Traces[i], full.Traces[i])
		}
	}
	// Memory bound: proves two-phase actually engaged for the wrapped shape.
	if small.Metrics.InspectedTraces >= full.Metrics.InspectedTraces {
		t.Errorf("wrapped-select small drain (%d) not bounded below full drain (%d) — the Project was not unwrapped, so it fell to single-query",
			small.Metrics.InspectedTraces, full.Metrics.InspectedTraces)
	}
	if want := structTwoPhaseSmallLimit * structTwoPhaseLeavesPerTrace; small.Metrics.InspectedTraces != want {
		t.Errorf("wrapped-select small drain = %d, want %d", small.Metrics.InspectedTraces, want)
	}
}

// TestSearch_StructuralTwoPhase_EmptyResult_ChDB proves the phase-A "no trace
// matched" branch: when the descendant side matches nothing, phase A returns
// zero ids and the handler returns an empty result (rather than emitting a
// literal `IN ()`, a CH syntax error) — the same empty response the single
// query would produce.
func TestSearch_StructuralTwoPhase_EmptyResult_ChDB(t *testing.T) {
	srv := newManyTracesChDBServer(t, structuralTwoPhaseSeed())

	q := url.QueryEscape(`{ resource.service.name = "root-svc" } >> { resource.service.name = "does-not-exist" }`)
	sr := doSearch(t, srv, fmt.Sprintf("/api/search?q=%s&limit=5%s", q, seedWindow))

	if len(sr.Traces) != 0 {
		t.Fatalf("got %d traces, want 0 (descendant side matches nothing)", len(sr.Traces))
	}
	if sr.Metrics.InspectedTraces != 0 {
		t.Errorf("InspectedTraces = %d, want 0 (phase A short-circuits, phase B never runs)", sr.Metrics.InspectedTraces)
	}
}
