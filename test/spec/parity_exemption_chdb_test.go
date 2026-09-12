//go:build chdb_agpl_oracle

package spec

import (
	"errors"
	"os"
	"path/filepath"
	"reflect"
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
	for _, tt := range []struct {
		name string
		err  error
	}{
		{name: "disagreement", err: parityDisagreement(errors.New("values differ"))},
		{name: "refusal", err: parityRefusal(errors.New("shape has no comparator"))},
	} {
		t.Run(tt.name+" remains live", func(t *testing.T) {
			if err := exemptionVerdict(c, exemption, p, tt.err); err != nil {
				t.Fatalf("verdict = %v, want live exemption", err)
			}
		})
	}

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
	}{
		{name: "non-span projection", rows: [][]any{{1}}, wantRefusal: true},
		{name: "missing identity", rows: [][]any{{"span", map[string]any{}, "timestamp", 1}}, wantRefusal: true},
		{name: "duplicate identity", rows: [][]any{
			{"span", identity, "timestamp", 1},
			{"span", identity, "timestamp", 1},
		}, wantRefusal: true},
		{name: "malformed attributes remain hard", rows: [][]any{{"span", "not-an-object", "timestamp", 1}}},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			_, err := spanIdentitiesOfExpectedRows(&RoundTripSections{ExpectedRows: tt.rows})
			if err == nil {
				t.Fatal("spanIdentitiesOfExpectedRows returned nil error")
			}
			if got := errors.Is(err, errTempoSpanIdentityUnavailable); got != tt.wantRefusal {
				t.Fatalf("errors.Is(err, errTempoSpanIdentityUnavailable) = %t, want %t: %v", got, tt.wantRefusal, err)
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
