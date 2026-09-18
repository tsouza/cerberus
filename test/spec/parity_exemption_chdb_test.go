//go:build chdb_agpl_oracle

package spec

import (
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"reflect"
	"slices"
	"strings"
	"testing"
	"time"

	oracle "github.com/tsouza/cerberus/test/spec/parityoracle/promql"
)

func TestExemptionVerdict(t *testing.T) {
	c := &Case{Name: "synthetic"}
	exemption := &ParityExempt{Reason: ReasonVacuousEmptyInput, Detail: "fixture has no rows"}
	p := &Parity{Oracle: OraclePrometheus, Endpoint: EndpointInstant, Scope: ScopeFull}

	t.Run("agreement is stale", func(t *testing.T) {
		if err := exemptionVerdict(c, exemption, p, nil); err == nil {
			t.Fatal("agreement accepted, want stale-exemption error")
		}
	})
	t.Run("the refusal the reason names remains live", func(t *testing.T) {
		err := parityRefusal(refusalEmptySeedForSelector, errors.New("seed produced no readable series"))
		if err := exemptionVerdict(c, exemption, p, err); err != nil {
			t.Fatalf("verdict = %v, want live exemption", err)
		}
	})
	t.Run("a disagreement for a reason that promises a refusal cites the wrong reason", func(t *testing.T) {
		err := exemptionVerdict(c, exemption, p, parityDisagreement(errors.New("values differ")))
		if err == nil || !strings.Contains(err.Error(), "cites the wrong reason") {
			t.Fatalf("verdict = %v, want wrong-reason failure", err)
		}
	})
	t.Run("a refusal of another kind cites the wrong reason", func(t *testing.T) {
		err := exemptionVerdict(c, exemption, p, parityRefusal(refusalNonSampleProjection, errors.New("no Value column")))
		if err == nil || !strings.Contains(err.Error(), "cites the wrong reason") {
			t.Fatalf("verdict = %v, want wrong-reason failure", err)
		}
	})
	t.Run("a disagreement for a reason about the engines answering differently remains live", func(t *testing.T) {
		fetch := &ParityExempt{Reason: ReasonReferenceFetchLayer, Detail: "NOT operand"}
		if err := exemptionVerdict(c, fetch, p, parityDisagreement(errors.New("matched 0, cerberus 1"))); err != nil {
			t.Fatalf("verdict = %v, want live exemption", err)
		}
	})

	for _, tt := range []struct {
		name string
		err  error
	}{
		{name: "mismatched round-trip handoff", err: errors.New("parity requires the successful RunRoundTripSQL result for the same fixture")},
		{name: "missing compiled oracle", err: errors.New("lane was built without the chdb_agpl_oracle build tag")},
	} {
		t.Run(tt.name+" is hard failure", func(t *testing.T) {
			if err := exemptionVerdict(c, exemption, p, tt.err); !errors.Is(err, tt.err) {
				t.Fatalf("verdict = %v, want propagated harness failure", err)
			}
		})
	}
}

// Every declared reason must name the evidence that proves it, and every
// refusal class must prove at least one reason: an entry on one side with
// no partner on the other is a reason nothing can keep live, or a refusal
// no exemption may cite.
func TestParityExemptReasonsAllHaveEvidence(t *testing.T) {
	for _, reason := range ParityExemptReasons() {
		evidence, ok := exemptionEvidenceByReason[reason]
		if !ok {
			t.Errorf("reason %q has no exemptionEvidenceByReason entry", reason)
			continue
		}
		if !evidence.disagreement && len(evidence.refusals) == 0 {
			t.Errorf("reason %q declares no evidence at all: nothing could keep it live", reason)
		}
	}
	for reason := range exemptionEvidenceByReason {
		if !slices.Contains(ParityExemptReasons(), reason) {
			t.Errorf("exemptionEvidenceByReason names %q, which is not a declared reason", reason)
		}
	}
	claimed := map[refusalClass]bool{}
	for _, evidence := range exemptionEvidenceByReason {
		for _, class := range evidence.refusals {
			claimed[class] = true
		}
	}
	for _, class := range []refusalClass{
		refusalNoRoundTrip, refusalNonSampleProjection, refusalConflictingDuplicateTimestamp,
		refusalEmptySeedForSelector, refusalOrderSensitiveReference, refusalReferenceEvaluation,
		refusalLogStreamAnswer, refusalSeedNotStreams, refusalOpaqueColumn, refusalNarrowingSection,
		refusalEmptySpanSeed, refusalSeedWithoutSpanIdentity, refusalDuplicateSpanIdentity,
		refusalReferenceRejectedQuery, refusalUnrepresentableSpan, refusalNonSpanProjection,
	} {
		switch class {
		case refusalSeedNotStreams, refusalEmptySpanSeed, refusalSeedWithoutSpanIdentity:
			// A seed the comparator cannot read at all proves no exemption:
			// it is a fixture deficiency to fix (add the columns, seed a
			// row), not an obstacle any reason names.
			if claimed[class] {
				t.Errorf("refusal %q is a fixture deficiency and must prove no reason", class)
			}
		default:
			if !claimed[class] {
				t.Errorf("refusal %q proves no declared reason", class)
			}
		}
	}
}

func TestRefusalClassOf(t *testing.T) {
	wrapped := fmt.Errorf("fixture x: %w", parityRefusal(refusalNarrowingSection, errors.New("search_limit")))
	class, ok := refusalClassOf(wrapped)
	if !ok || class != refusalNarrowingSection {
		t.Fatalf("refusalClassOf = %q, %t; want %q, true", class, ok, refusalNarrowingSection)
	}
	if !errors.Is(wrapped, errParityRefusal) {
		t.Fatal("a classed refusal must still satisfy errors.Is(err, errParityRefusal)")
	}
	if _, ok := refusalClassOf(parityDisagreement(errors.New("x"))); ok {
		t.Fatal("a disagreement is not a refusal")
	}
}

func TestConflictingDuplicateTimestampRefusal(t *testing.T) {
	series := []oracle.Series{{Points: []oracle.Point{
		{TMillis: 1, Value: 1},
		{TMillis: 1, Value: 2},
	}}}
	if !hasConflictingDuplicateTimestamp(series) {
		t.Fatal("different values at one timestamp were accepted as a stable comparison")
	}
	series[0].Points[1].Value = 1
	if hasConflictingDuplicateTimestamp(series) {
		t.Fatal("identical duplicate samples were refused")
	}
}

func TestOrderSensitiveSelectionPermutation(t *testing.T) {
	for _, tt := range []struct {
		expr string
		want bool
	}{
		{expr: "limitk(1, up)", want: true},
		{expr: "sum(limitk(1, up))", want: true},
		{expr: "topk(1, up)", want: false},
	} {
		got, err := hasOrderSensitiveSelection(tt.expr)
		if err != nil {
			t.Fatalf("hasOrderSensitiveSelection(%q): %v", tt.expr, err)
		}
		if got != tt.want {
			t.Fatalf("hasOrderSensitiveSelection(%q) = %t, want %t", tt.expr, got, tt.want)
		}
	}

	series := []oracle.Series{
		{Labels: map[string]string{"instance": "a"}},
		{Labels: map[string]string{"instance": "b"}},
	}
	got, err := reverseSeriesSchedule(series)
	if err != nil {
		t.Fatalf("reverseSeriesSchedule: %v", err)
	}
	if got[0].Labels["instance"] != "b" || series[0].Labels["instance"] != "a" {
		t.Fatalf("permutation = %v, original = %v", got, series)
	}
}

func TestLimitKReferenceAnswerChangesWithSeriesSchedule(t *testing.T) {
	instant := time.Unix(1, 0).UTC()
	series := []oracle.Series{
		{Labels: map[string]string{"__name__": "up", "instance": "a"}, Points: []oracle.Point{{TMillis: 1000, Value: 1}}},
		{Labels: map[string]string{"__name__": "up", "instance": "b"}, Points: []oracle.Point{{TMillis: 1000, Value: 2}}},
	}
	query := oracle.Query{Expr: "limitk(1, up)", Start: instant, End: instant}
	first, err := oracle.Evaluate(t, series, query)
	if err != nil {
		t.Fatalf("evaluate original schedule: %v", err)
	}
	permuted, err := reverseSeriesSchedule(series)
	if err != nil {
		t.Fatalf("reverseSeriesSchedule: %v", err)
	}
	second, err := oracle.Evaluate(t, permuted, query)
	if err != nil {
		t.Fatalf("evaluate reversed schedule: %v", err)
	}
	if reflect.DeepEqual(first, second) {
		t.Fatalf("LIMITK answer unchanged after schedule perturbation: %v", first)
	}
}

func TestCompareAgainstReferenceClassifiesOnlyAnswerMismatch(t *testing.T) {
	c := &Case{Name: "synthetic"}
	p := &Parity{Oracle: OraclePrometheus, Endpoint: EndpointInstant, Scope: ScopeFull}
	sc := sampleColumns{name: -1, attrs: 0, ts: -1, value: 1, mixedIsHistogram: -1}

	t.Run("sample count mismatch", func(t *testing.T) {
		rt := &RoundTripSections{ExpectedRows: [][]any{{map[string]any{}, float64(1)}}}
		err := compareAgainstReference(t, c, p, rt, sc, nil, false, "vector(1)")
		if !errors.Is(err, errParityDisagreement) {
			t.Fatalf("error = %v, want classified disagreement", err)
		}
	})

	t.Run("malformed expected row", func(t *testing.T) {
		rt := &RoundTripSections{ExpectedRows: [][]any{{map[string]any{}}}}
		err := compareAgainstReference(t, c, p, rt, sc, nil, false, "vector(1)")
		if err == nil || errors.Is(err, errParityDisagreement) || errors.Is(err, errParityRefusal) {
			t.Fatalf("error = %v, want unclassified harness failure", err)
		}
	})

	t.Run("scope contract", func(t *testing.T) {
		rt := &RoundTripSections{Seed: "CREATE TABLE otel_metrics_exponential_histogram (Value Float64)"}
		got := []referenceSample{{Value: 0}}
		err := checkZeroBucketScope(c, p, rt, "histogram_quantile(0.5, metric)", got)
		if err == nil || errors.Is(err, errParityDisagreement) || errors.Is(err, errParityRefusal) {
			t.Fatalf("error = %v, want unclassified scope-contract failure", err)
		}
	})
}

func TestSynthesizedParity(t *testing.T) {
	tests := []struct {
		name     string
		section  string
		eval     ParityEval
		oracle   string
		endpoint string
	}{
		{name: "promql instant", section: "query.promql", oracle: OraclePrometheus, endpoint: EndpointInstant},
		{
			name:     "promql range",
			section:  "query.promql",
			eval:     ParityEval{Step: time.Minute},
			oracle:   OraclePrometheus,
			endpoint: EndpointRange,
		},
		{name: "logql instant", section: "query.logql", oracle: OracleLoki, endpoint: EndpointInstant},
		{
			name:     "logql range",
			section:  "query.logql",
			eval:     ParityEval{Step: time.Minute},
			oracle:   OracleLoki,
			endpoint: EndpointRange,
		},
		{name: "traceql", section: "query.traceql", oracle: OracleTempo, endpoint: EndpointSearch},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			c := loadSyntheticParityCase(t, "-- "+tt.section+" --\nquery\n")
			got, err := synthesizedParity(c, tt.eval)
			if err != nil {
				t.Fatalf("synthesizedParity: %v", err)
			}
			if got.Oracle != tt.oracle || got.Endpoint != tt.endpoint || got.Scope != ScopeFull {
				t.Fatalf("synthesized parity = %+v, want oracle=%q endpoint=%q scope=%q", got, tt.oracle, tt.endpoint, ScopeFull)
			}
		})
	}
}

func TestSynthesizedParityRequiresExactlyOneQuerySection(t *testing.T) {
	for _, tt := range []struct {
		name string
		body string
	}{
		{name: "none"},
		{name: "multiple", body: "-- query.promql --\nup\n-- query.logql --\n{job=\\\"api\\\"}\n"},
	} {
		t.Run(tt.name, func(t *testing.T) {
			c := loadSyntheticParityCase(t, tt.body)
			if _, err := synthesizedParity(c, ParityEval{}); err == nil {
				t.Fatal("synthesizedParity succeeded, want an ambiguous-contract error")
			}
		})
	}
}

func TestTempoSpanIdentityErrorClassification(t *testing.T) {
	identity := map[string]any{
		spanIdentityTraceIDKey: "trace-1",
		spanIdentitySpanIDKey:  "span-1",
	}
	tests := []struct {
		name        string
		rows        [][]any
		wantRefusal bool
		wantClass   refusalClass
	}{
		{name: "non-span projection", rows: [][]any{{1}}, wantRefusal: true, wantClass: refusalNonSpanProjection},
		{
			name: "aggregate projection without identity keys", rows: [][]any{{"span", map[string]any{}, "timestamp", 1}},
			wantRefusal: true, wantClass: refusalNonSpanProjection,
		},
		{
			name: "span row with empty identity", rows: [][]any{{"span", map[string]any{
				spanIdentityTraceIDKey: "", spanIdentitySpanIDKey: "",
			}, "timestamp", 1}},
			wantRefusal: true, wantClass: refusalSeedWithoutSpanIdentity,
		},
		{name: "duplicate identity", rows: [][]any{
			{"span", identity, "timestamp", 1},
			{"span", identity, "timestamp", 1},
		}, wantRefusal: true, wantClass: refusalDuplicateSpanIdentity},
		{name: "malformed attributes remain hard", rows: [][]any{{"span", "not-an-object", "timestamp", 1}}},
		{
			name: "empty answer from a non-span projection is a shape refusal, not an empty span set",
			rows: [][]any{}, wantRefusal: true, wantClass: refusalNonSpanProjection,
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			_, err := spanIdentitiesOfExpectedRows(&RoundTripSections{ExpectedRows: tt.rows}, false)
			if err == nil {
				t.Fatal("spanIdentitiesOfExpectedRows returned nil error")
			}
			if got := errors.Is(err, errTempoSpanIdentityUnavailable); got != tt.wantRefusal {
				t.Fatalf("errors.Is(err, errTempoSpanIdentityUnavailable) = %t, want %t: %v", got, tt.wantRefusal, err)
			}
			var identityErr *tempoSpanIdentityError
			if errors.As(err, &identityErr) != tt.wantRefusal {
				t.Fatalf("errors.As tempoSpanIdentityError = %t, want %t: %v", !tt.wantRefusal, tt.wantRefusal, err)
			}
			if tt.wantRefusal && identityErr.class != tt.wantClass {
				t.Fatalf("class = %q, want %q", identityErr.class, tt.wantClass)
			}
		})
	}
}

func loadSyntheticParityCase(t *testing.T, body string) *Case {
	t.Helper()
	path := filepath.Join(t.TempDir(), "synthetic.txtar")
	if err := os.WriteFile(path, []byte(body), 0o600); err != nil {
		t.Fatalf("write fixture: %v", err)
	}
	c, err := Load(path)
	if err != nil {
		t.Fatalf("Load: %v", err)
	}
	return c
}

// An empty canonical span answer stays comparable: the projection writes a
// span identity, so zero matched spans is a real (empty) set, not a shape gap.
func TestSpanIdentitiesOfExpectedRowsEmptySpanAnswerIsComparable(t *testing.T) {
	want, err := spanIdentitiesOfExpectedRows(&RoundTripSections{ExpectedRows: [][]any{}}, true)
	if err != nil {
		t.Fatalf("empty span-shaped answer refused: %v", err)
	}
	if len(want) != 0 {
		t.Fatalf("want = %v, want empty", want)
	}
}

// projectionIdentifiesSpans reads the executed argument list, so a span
// search (whose wrap binds the __cerberus_spanID key) and a per-trace
// aggregate (which binds only the trace-level keys) are told apart
// without a single row.
func TestProjectionIdentifiesSpans(t *testing.T) {
	search := loadSyntheticParityCase(t, "-- query.traceql --\n{}\n-- args_optimized --\n[0] string = \"__cerberus_traceID\"\n[1] string = \"__cerberus_parentSpanID\"\n[2] string = \"__cerberus_spanID\"\n")
	if !projectionIdentifiesSpans(search) {
		t.Fatal("a wrap binding __cerberus_spanID must identify spans")
	}
	aggregate := loadSyntheticParityCase(t, "-- query.traceql --\n{} | count() > 0\n-- args_optimized --\n[0] string = \"__cerberus_traceID\"\n[1] string = \"__cerberus_parentSpanID\"\n[2] string = \"__cerberus_traceDurationNs\"\n")
	if projectionIdentifiesSpans(aggregate) {
		t.Fatal("a per-trace aggregate wrap must not identify spans")
	}
}
