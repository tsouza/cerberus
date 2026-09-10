// Package spec — parity exemption declarations.
//
// This file defines the optional `parity_exempt:` section: the other half
// of the closure the `parity:` section (parity.go) leaves open. A fixture
// is either enrolled against a real reference engine (`parity:`) or it
// declares, in a reviewed and closed vocabulary, structurally WHY it
// cannot be — never neither. test/regression's
// TestParityCoverageIsComplete is what enforces the "never neither"
// half; this file is what keeps the declared reason honest rather than a
// free-form excuse.
//
// What nothing yet re-checks is whether a declared reason is still TRUE.
// Three of the reasons below name a gap that will be closed rather than a
// permanent boundary, so their fixtures become enrollable the day the gap
// lands, and today that transition is invisible. #3261 tracks the check
// that would catch it: run every exempt fixture against its oracle and
// fail if the oracle agrees.
//
// # Why a new section rather than a new value inside `parity:`
//
// compareSampleValue's own doc comment already rejected a new required
// `parity:` key for a narrower reason (a float tolerance) because
// LoadParity treats every key as required on every enrolled fixture, so
// adding one would force an edit onto every already-enrolled fixture. The
// same argument applies here with more force: an exemption fixture has no
// oracle, no endpoint and no scope to declare. Bolting `status: exempt`
// onto the `parity:` vocabulary would either make those three keys
// conditionally required — reintroducing exactly the "some keys are
// required, some aren't" ambiguity LoadParity's error path exists to
// prevent — or force meaningless placeholder values onto every exemption.
// A separate section, mutually exclusive with `parity:` (enforced by
// TestParityCoverageIsComplete), keeps both shapes simple and keeps
// LoadParity's "every key required" invariant intact for the shape it was
// built for.
//
// # Why the reason is a closed vocabulary and not free text
//
// A `reason: <anything>` line would be indistinguishable from the
// allow-list invariant 7 forbids: any excuse, once typed, would silently
// satisfy the corpus-completeness gate. Restricting `reason` to a small,
// closed set of STRUCTURAL claims — never "not gotten to yet", never
// "flaky", never a fixture-specific excuse — keeps the escape hatch
// auditable: TestParityExemptVocabulariesAreClosed pins the set, and
// widening it is a reviewed source-line change, not a per-fixture opt-out.
// `detail:` carries the free-text, fixture-specific explanation a reviewer
// needs, but it participates in no vocabulary check beyond "non-empty" —
// the STRUCTURAL claim a reason must prove is carried entirely by
// `reason:`.
package spec

import (
	"fmt"
	"slices"
	"sort"
	"strings"
)

// SectionParityExempt is the TXTAR section name a fixture carries to
// declare, instead of enrolling, why it cannot be compared against a
// reference engine.
const SectionParityExempt = "parity_exempt"

// Exemption reasons. Each is a claim about the fixture's STRUCTURE, never
// about its current state of being un-gotten-to — see the package doc.
const (
	// ReasonNondeterministicSelection covers an operator whose own
	// reference implementation documents its survivor/answer set as
	// implementation-defined (PromQL's limitk tie-break, confirmed
	// against prometheus/promql/engine.go: LIMITK keeps whichever K
	// series arrive first in the input matrix's iteration order, an
	// order neither engine promises, versus cerberus's own
	// `row_number() OVER ()` with no ORDER BY). There is no "reference
	// answer" for a second implementation to agree with, however
	// faithfully it reimplements the operator.
	//
	// This is NOT the reason for limit_ratio: that sampler is
	// hash-based on the label set (HashRatioSampler in the same file),
	// and cerberus's `xxHash64` predicate reproduces the reference
	// stringlabels encoding bit-for-bit (see lowerLimitRatio's own
	// doc comment) — limit_ratio fixtures are enrollable, not exempt.
	ReasonNondeterministicSelection = "nondeterministic-selection"

	// ReasonNoComparableOracle covers a fixture whose answer shape has
	// no comparator wired into RunParity — a real upstream HTTP concept
	// that simply is not one of the `endpoint:`-dispatched comparisons
	// RunParity performs today. The upstream endpoint exists; RunParity's
	// dispatch table does not reach it.
	//
	// On the Prometheus side that is a label-name list, a label-value
	// list, or an exemplar array, none of which is Sample-shaped. On the
	// Tempo side it is a query whose answer is not a SPANSET: a
	// spanset-aggregation projection (`| count() > N`, whose rows carry a
	// per-trace Value rather than a span) or a metrics-pipeline one
	// (`| rate()`, `| compare(...)`), which produce a time series.
	// runTempoParity compares WHICH SPANS matched and nothing else, so
	// neither shape has a comparison to make — see
	// spanIdentitiesOfExpectedRows, which errors on any row that is not
	// the canonical four-column span shape rather than comparing a subset.
	//
	// A fixture in this class must not be enrolled just because its
	// answer happens to be EMPTY today. A zero-row aggregate projection
	// never reaches the shape check, so it would pass while asserting a
	// comparison its projection cannot support, and would begin failing
	// on the shape the moment its seed produced a matching row.
	ReasonNoComparableOracle = "no-comparable-oracle"

	// ReasonRejectionOnly covers a fixture whose query must be REJECTED
	// rather than answered. Neither RunRoundTrip (needs `seed:` +
	// `expected_rows:`) nor RunParity (needs a round trip on top of
	// that) has a shape for an expected error; the executable assertion
	// lives in a dedicated Go test instead (see each fixture's own
	// header comment for the exact test).
	ReasonRejectionOnly = "rejection-only"

	// ReasonVacuousEmptyInput covers a fixture whose query reads a
	// SELECTOR (confirmed by RunParity's own exprReadsSeries — a
	// VectorSelector or MatrixSelector genuinely present in the parsed
	// expression) but whose seed intentionally provisions zero rows in
	// every table that selector could read. evaluatePrometheusParity
	// already refuses this case at runtime (see its own doc comment: "a
	// genuinely empty series set is only a vacuous check ... when the
	// query reads one"), so enrolling would not check cerberus against
	// the reference engine, it would check cerberus against a reference
	// engine handed no data to disagree with — every candidate answer
	// passes, so a real lowering bug in the selector path would pass
	// too. This is a fact about the SEED, not about the operator: it is
	// the reverse of ReasonNoComparableOracle (whose gap is in
	// RunParity's own dispatch table, not in the fixture's data).
	//
	// This is NOT the reason for a query that reads no selector at all
	// — `pi()`, `time()`, `vector(N)`, the zero-argument date functions,
	// and any of those same shapes used as a subquery inner — whose
	// zero-series seed is not vacuous but the query's own correct,
	// data-independent input (exprReadsSeries returns false for all of
	// them; see pi_constant.txtar, hour_default.txtar, and
	// subquery_inner_anchor_synthesised.txtar, all enrolled with a real
	// `parity:` section for exactly that reason).
	ReasonVacuousEmptyInput = "vacuous-empty-input"

	// ReasonDuplicateTimestampSeed covers a fixture whose seed
	// DELIBERATELY carries two metric samples at one (series, timestamp)
	// with DIFFERENT values, in order to pin how cerberus's own emitted
	// shape treats that pair. Like [ReasonVacuousEmptyInput] this is a
	// fact about the SEED rather than about the operator.
	//
	// Which of the two survives is implementation-defined on both sides.
	// Prometheus's TSDB appender stores at most one sample per (series,
	// timestamp): parityoracle/promql/oracle.go's appendSeries feeds a
	// real teststorage head, and the second sample is dropped at commit,
	// so the survivor is whichever arrived first — a fact about the
	// appender and about ingestion order, not about PromQL. ClickHouse
	// keeps both rows, and which one a lowering surfaces depends on the
	// emitted shape: the sorted-slab over_time path sums both, the
	// array-fold path's arraySort(groupArray((ts, value))) breaks the tie
	// by VALUE, and the rate family deduplicates by design. So the
	// reference has no answer for cerberus to agree with, however
	// faithfully it reimplements the operator.
	//
	// The contract such a fixture pins is not lost with the enrolment: it
	// is pinned by a cerberus-vs-cerberus differential over the same seed
	// (internal/promql/duplicate_timestamp_seed_chdb_test.go), a sharper
	// oracle than a reference backend for this question — it compares the
	// strategy under test against the fan-out strategy it must agree
	// with, rather than against an engine that discarded one of the
	// samples before evaluating anything.
	//
	// This is NOT the reason for a duplicate whose rows carry IDENTICAL
	// values. Prometheus still keeps one row there and ClickHouse still
	// keeps two, so a counting reducer can still diverge — but that
	// divergence has a RIGHT answer (the reference's) and cerberus can be
	// made to match it, which is an ordinary bug to fix at the source
	// rather than a structural claim.
	// increase_duplicate_timestamp_dedup.txtar seeds exactly that, stays
	// ENROLLED, and passes because the rate family deduplicates.
	ReasonDuplicateTimestampSeed = "duplicate-timestamp-seed"

	// ReasonLogQueryAnswer covers a LogQL fixture whose query is a LOG
	// query rather than a metric one. The two engines answer such a
	// query in shapes that have no element-wise correspondence at all:
	// the reference returns [logqlmodel.Streams], a set of log LINES
	// grouped by stream, while cerberus returns the `SELECT *` row set
	// whose column layout is whatever the fixture's own `seed:` DDL
	// happened to declare.
	//
	// This is not a gap in RunParity's dispatch table that a comparator
	// could close. test/spec/parityoracle/logql's flatten already
	// REFUSES the [logqlmodel.Streams] case by name, with the same
	// reasoning: manufacturing a correspondence between lines and a
	// fixture-defined projection would manufacture a green. The
	// oracle's own doc comment states the resulting rule as "enrol
	// metric queries only".
	//
	// Membership is decided by the UPSTREAM parser, not by inspecting
	// the emitted SQL: syntax.ParseExpr returns a syntax.SampleExpr for
	// a metric query and a syntax.LogSelectorExpr for a log one, and
	// that is the same classification the reference engine itself makes
	// when it picks which answer shape to build.
	//
	// This is NOT a reason to reach for when a metric fixture merely
	// happens to be hard to compare. A metric query whose seed is too
	// thin, or whose lowering emits a projection the comparator cannot
	// read, is an ordinary corpus or harness deficiency to fix at the
	// source — it stays undeclared until it is fixed, rather than
	// borrowing this reason.
	ReasonLogQueryAnswer = "log-query-answer"

	// ReasonStructuredMetadataUnobservable covers a LogQL metric fixture
	// whose answer depends on data upstream's in-process querier never
	// puts in front of its engine.
	//
	// [logql.NewMockQuerier] is that querier, and its processStream /
	// processSeries helpers call the pipeline with `labels.EmptyLabels()`
	// for structured metadata, so an entry's StructuredMetadata is
	// DISCARDED before evaluation begins. Cerberus carries structured
	// metadata in LogAttributes and severity in SeverityText, and folds
	// both into the synthesised `detected_level` label and into `level`
	// grouping keys. None of that has any counterpart the reference can
	// be handed.
	//
	// The class therefore has two faces, and both are this one reason:
	// a seed that POPULATES LogAttributes or SeverityText, which
	// parity_loki_chdb.go's rejectOpaqueColumn refuses at read time; and
	// an answer carrying the `detected_level` label cerberus synthesises
	// even when severity is absent (as `unknown`), which reaches the
	// comparator as a label-set difference instead. Upstream produces
	// `detected_level` at INGESTION, in a level-discovery step the
	// in-process querier does not run, so the label cannot appear on the
	// reference side at any value — not even as absent-meaning-unknown.
	//
	// A subset comparison over the remaining columns is deliberately not
	// offered. It would run, pass, and prove nothing about the axis it
	// dropped, which is the hollow green the whole parity layer exists
	// to eliminate.
	ReasonStructuredMetadataUnobservable = "structured-metadata-unobservable"

	// ReasonReferenceShardedPathOnly covers a fixture whose operator the
	// reference engine implements ONLY on its sharded query path, which
	// the in-process oracle deliberately does not run.
	//
	// `approx_topk` is the case. Upstream's pkg/logql/optimize.go
	// rewrites it — unconditionally, on the unsharded path too — into
	// `topk(k, CountMinSketchEval<__count_min_sketch__(...)>)`, and the
	// CountMinSketchEvalExpr evaluator consumes sketches that only the
	// sharded downstream path produces. test/spec/parityoracle/logql
	// runs [logql.NewMockQuerier] with mockQuerierShards = 0, because
	// sharding would change which streams a query observes for a reason
	// that has nothing to do with the lowering under test. So the
	// reference yields an empty vector — no error, no panic, no answer.
	//
	// Raising the oracle's shard count for this one fixture is not the
	// fix hiding behind this reason. It would be a per-fixture knob on
	// the oracle's own configuration, which is the shape invariant 7
	// forbids, and it would change the observed stream set for every
	// other fixture sharing the session.
	ReasonReferenceShardedPathOnly = "reference-sharded-path-only"
	// ReasonReferenceFetchLayer covers a TraceQL fixture whose reference
	// answer is produced partly OUTSIDE the spanset pipeline the oracle
	// evaluates. test/spec/parityoracle/traceql runs upstream Tempo's
	// pipeline over every span it is handed; reference `/api/search`
	// additionally has a FETCH layer that narrows and, for some shapes,
	// decides the answer outright. Where the two disagree they disagree
	// about what was READ, not about the query.
	//
	// Four shapes fall in this class:
	//
	//   - `search_window:` and `search_limit:`, which bound which rows
	//     cerberus reads and how many it returns. Upstream applies both
	//     before the pipeline runs. parity_tempo_chdb_agpl_oracle.go's
	//     rejectNarrowingSections already refuses this combination
	//     outright rather than comparing across the difference.
	//
	//   - `{ !(<expr>) }` as the whole spanset filter, and a NOT operand
	//     of a logical AND/OR wrapping a single comparison. Reference
	//     `/api/search` matches ZERO traces for the first and silently
	//     drops the operand for the second, established by differential
	//     probing against a real instance (#1711/#1712) and implemented
	//     in internal/traceql/lower.go's lowerUnaryNot. The in-process
	//     engine, having no fetch layer, returns the logically correct
	//     answer instead — so the oracle and the endpoint disagree, and
	//     cerberus matches the endpoint, which is the contract it ships.
	//
	//   - `<attribute> = nil`, whose answer upstream produces with the
	//     fetch layer and the pipeline acting as two halves of ONE
	//     mechanism. The grammar rewrites `= nil` to the unary
	//     `OpNotExists` (Tempo's expr.y), and that operator does not test
	//     for a nil Static — ast_execute.go compares the resolved value
	//     against the Static STRING "nil". The sentinel is written by the
	//     FETCH layer: vparquet4's block_traceql.go turns an OpNotExists
	//     condition into a key-absence iterator
	//     (NewIncludeNilStringEqualPredicate + NilSyncIterator) and its
	//     collector materialises the absent key as NewStaticString("nil").
	//     An in-process engine handed hand-built spans holds only the
	//     second half, so the sentinel is never written and `= nil`
	//     matches nothing at all — not even the spans that genuinely lack
	//     the attribute. Cerberus answers those spans, which is what real
	//     `/api/search` answers; upstream's own
	//     TestBackendNilKeyBlockSearchTraceQL pins that through Fetch.
	//     The mirror-image `!= nil` is OpExists, which reads the Static's
	//     type directly and needs no fetch-layer cooperation, so those
	//     fixtures ARE enrolled and pass.
	//
	//   - a span carrying MORE THAN ONE event or more than one link.
	//     vparquet4 decodes every event's attributes into one flat
	//     `event.` scope and every link's into one flat `link.` scope, and
	//     per-record matching is the fetch layer's iterator join, not the
	//     pipeline's. With two events the flat scope can hold one key
	//     twice, AttributeFor answers with whichever came first, and the
	//     in-process oracle would report a confident wrong answer where
	//     real Tempo matches on the other event.
	//     test/spec/parityoracle/traceql's validateChildRecords refuses
	//     that span outright rather than guessing, for the same reason
	//     rejectNarrowingSections refuses the first shape.
	//
	// This is NOT a reason for an ordinary disagreement about a query
	// both layers evaluate the same way. It requires a NAMED mechanism
	// outside the pipeline that accounts for the difference.
	ReasonReferenceFetchLayer = "reference-fetch-layer"

	// ReasonEmittedSQLOnly covers a fixture that carries neither `seed:`
	// nor `expected_rows:`. What it pins is the emitted SQL — that a
	// request window is folded into the leaf scan, say — and it never
	// executes anything. There is no answer for a reference engine to
	// compare against, so the exemption is not a limitation of any
	// oracle but a fact about what the fixture asserts.
	//
	// RunParity already refuses such a fixture by name: a `parity:`
	// section on a fixture with no executable round trip is a fatal
	// error, because the parity check reads the seeded rows back out of
	// chDB and there are none.
	//
	// This is NOT a licence to leave a fixture unseeded in order to
	// avoid enrolling it. The claim is about a fixture whose PURPOSE is
	// the emitted SQL; a fixture that asserts an answer needs a seed and
	// an enrolment.
	ReasonEmittedSQLOnly = "emitted-sql-only"

	// ReasonDuplicateSpanSeed covers a TraceQL fixture whose seed
	// DELIBERATELY delivers the same (TraceId, SpanId) on more than one
	// row, in order to pin how cerberus's emitted shape treats the
	// repeat — at-least-once redelivery, or the same span rewritten with
	// its attribute Map keys in a different order. It is the TraceQL
	// sibling of [ReasonDuplicateTimestampSeed], and like it a fact
	// about the SEED rather than about the operator.
	//
	// Span identity IS the comparison key: runTempoParity compares two
	// SETS of (TraceID, SpanID), and spanIdentitiesOfExpectedRows
	// rejects a repeated identity outright because a set cannot express
	// "the same span twice". The reference engine, handed the duplicate
	// rows, has no answer to that question either — upstream's own
	// storage deduplicates before the pipeline sees the spans, so it
	// never observes the repeat these fixtures exist to provoke.
	//
	// This is NOT a reason for a fixture that merely HAPPENS to seed two
	// spans with equal attributes under distinct span ids. Those are
	// distinct spans on both sides, they compare normally, and any
	// disagreement about them is an ordinary bug to fix at the source.
	ReasonDuplicateSpanSeed = "duplicate-span-seed"

	// ReasonOracleUntypedAttributes covered a TraceQL fixture whose query
	// compared an attribute against a NON-STRING literal — a boolean, a
	// bare integer, a float — before #3259's attrTypeHints mechanism
	// (test/spec/parityoracle/traceql/oracle.go) closed the general case:
	// the oracle now reads such an attribute's stored string as the type
	// the QUERY's own literal implies, the same per-comparison coercion
	// cerberus's own lowering performs on the identical Map(String,
	// String) column, rather than always handing the reference engine a
	// String static no typed literal could ever match. No fixture wears
	// this reason any more (bool_attr.txtar / unscoped_bool_attr.txtar /
	// unscoped_bool_attr_false.txtar were its only three, and are now
	// enrolled for real against `oracle: tempo`).
	//
	// The reason stays declared, not removed, because attrTypeHints is
	// sound rather than complete: it only recovers a type the fixture's
	// OWN query states directly in a comparison its AST walk reaches. An
	// attribute whose non-string nature is observable only some other
	// way — through a metrics pipeline stage the walk does not cover, or
	// a comparison shape it does not recognize — would still reach the
	// reference engine as a plain string, and a fixture exercising that
	// residual gap earns this reason again with a `detail` naming
	// specifically what attrTypeHints could not see.
	ReasonOracleUntypedAttributes = "oracle-untyped-attributes"

	// ReasonReferenceIntrinsicUnsupported covers a fixture whose query
	// uses an intrinsic the reference engine itself does not implement.
	// Upstream Tempo's evaluator rejects `parent` outright — "intrinsic
	// (parent) not yet supported" — so there is no reference answer for
	// cerberus to agree with, whatever it does.
	//
	// This is the opposite of ReasonNoComparableOracle, and the two must
	// not be confused: that one names a gap in RunParity's own dispatch
	// table, where the upstream endpoint exists and this harness cannot
	// reach it. Here the harness reaches the engine perfectly well and
	// the ENGINE declines.
	//
	// It is also not ReasonReferenceFetchLayer: nothing outside the
	// pipeline supplies the answer, because there is no answer.
	ReasonReferenceIntrinsicUnsupported = "reference-intrinsic-unsupported"

	// ReasonReferencePipelineError covers a LogQL fixture whose seed
	// deliberately provokes a PIPELINE ERROR — a line the unwrap stage
	// cannot parse, say — in order to pin how that failure is carried.
	//
	// The two engines carry it at different layers. Upstream aborts the
	// whole query and answers with an error, so there are no samples to
	// compare. Cerberus carries the failure as data: the offending series
	// comes back as a zero-valued row wearing `__error__` and
	// `__error_details__` labels, and internal/api/loki's
	// pipelineErrorFor turns that row into the same error RESPONSE at the
	// HTTP layer. The two therefore agree on the wire and differ only in
	// the row layer this comparator reads, which is the one layer where
	// the fixture's contract lives.
	//
	// This is NOT a reason for a fixture that merely happens to produce
	// an error the harness finds inconvenient. It requires the error to
	// be the fixture's OWN subject, visible as an `__error__` label in
	// its `expected_rows:`.
	ReasonReferencePipelineError = "reference-pipeline-error"
)

// parityExemptReasons is the single source of truth for the `reason`
// key's closed vocabulary. `detail` is deliberately absent from this
// list: it is free text, checked only for non-emptiness by
// LoadParityExempt.
var parityExemptReasons = []string{
	ReasonNondeterministicSelection,
	ReasonNoComparableOracle,
	ReasonRejectionOnly,
	ReasonVacuousEmptyInput,
	ReasonDuplicateTimestampSeed,
	ReasonLogQueryAnswer,
	ReasonStructuredMetadataUnobservable,
	ReasonReferenceShardedPathOnly,
	ReasonReferenceFetchLayer,
	ReasonEmittedSQLOnly,
	ReasonDuplicateSpanSeed,
	ReasonOracleUntypedAttributes,
	ReasonReferenceIntrinsicUnsupported,
	ReasonReferencePipelineError,
}

// ParityExemptReasons returns the accepted `reason` values, sorted.
// Exported for the contract test, mirroring ParityValues.
func ParityExemptReasons() []string {
	vals := append([]string(nil), parityExemptReasons...)
	sort.Strings(vals)
	return vals
}

// ParityExempt is the parsed `parity_exempt:` section.
type ParityExempt struct {
	// Reason is the closed-vocabulary structural claim.
	Reason string

	// Detail is the fixture-specific, human-authored explanation. Never
	// empty — see LoadParityExempt.
	Detail string
}

// LoadParityExempt parses a fixture's `parity_exempt:` section.
//
// The bool reports whether the fixture declared an exemption at all. A
// fixture with neither this section nor `parity:` is not caught here —
// see TestParityCoverageIsComplete, the sibling check that makes
// "neither" a failure.
//
// A fixture WITH the section but a malformed body IS an error, for the
// same reason LoadParity treats a malformed `parity:` body as an error: a
// typo must fail loudly, not quietly stop counting as either enrolled or
// exempt.
func LoadParityExempt(c *Case) (*ParityExempt, bool, error) {
	body, ok := c.Section(SectionParityExempt)
	if !ok {
		return nil, false, nil
	}

	got := map[string]string{}
	for i, line := range strings.Split(body, "\n") {
		line = strings.TrimSpace(line)
		if line == "" {
			continue
		}
		key, value, found := strings.Cut(line, ":")
		if !found {
			return nil, false, fmt.Errorf(
				"%s section line %d: %q is not `key: value`", SectionParityExempt, i+1, line,
			)
		}
		key, value = strings.TrimSpace(key), strings.TrimSpace(value)

		if _, dup := got[key]; dup {
			return nil, false, fmt.Errorf("%s section: key %q appears twice", SectionParityExempt, key)
		}

		switch key {
		case "reason":
			if !slices.Contains(ParityExemptReasons(), value) {
				return nil, false, fmt.Errorf(
					"%s section: key %q has value %q (accepted: %s)",
					SectionParityExempt, key, value, strings.Join(ParityExemptReasons(), ", "),
				)
			}
		case "detail":
			if value == "" {
				return nil, false, fmt.Errorf(
					"%s section: key %q must not be empty — it is the reviewed, fixture-specific "+
						"explanation a `reason:` category alone cannot carry",
					SectionParityExempt, key,
				)
			}
		default:
			return nil, false, fmt.Errorf(
				"%s section: unknown key %q (accepted: detail, reason)", SectionParityExempt, key,
			)
		}
		got[key] = value
	}

	for _, key := range []string{"detail", "reason"} {
		if _, present := got[key]; !present {
			return nil, false, fmt.Errorf("%s section: missing required key %q", SectionParityExempt, key)
		}
	}

	return &ParityExempt{Reason: got["reason"], Detail: got["detail"]}, true, nil
}
