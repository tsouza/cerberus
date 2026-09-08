package telemetry_test

import (
	"net/http"
	"testing"

	"github.com/tsouza/cerberus/internal/telemetry"
)

// TestClassifyStatus pins the status → (result, reason, class) mapping.
// The reason column is the whole point of the dimension: the same
// result="error" bucket has to keep a caller-side rejection and a
// gateway-side failure apart, because the two demand opposite responses
// — one is the caller's query to fix, the other is a page.
func TestClassifyStatus(t *testing.T) {
	cases := []struct {
		name   string
		status int
		want   telemetry.Outcome
	}{
		{
			name:   "implicit_200_when_handler_never_wrote_a_status",
			status: 0,
			want:   telemetry.Outcome{Result: telemetry.ResultOK, Reason: telemetry.ReasonNone, StatusClass: telemetry.StatusClass2xx},
		},
		{
			name:   "200_ok",
			status: http.StatusOK,
			want:   telemetry.Outcome{Result: telemetry.ResultOK, Reason: telemetry.ReasonNone, StatusClass: telemetry.StatusClass2xx},
		},
		{
			name:   "101_switching_protocols_is_a_clean_upgrade",
			status: http.StatusSwitchingProtocols,
			want:   telemetry.Outcome{Result: telemetry.ResultOK, Reason: telemetry.ReasonNone, StatusClass: telemetry.StatusClass1xx},
		},
		{
			name:   "304_not_modified",
			status: http.StatusNotModified,
			want:   telemetry.Outcome{Result: telemetry.ResultOK, Reason: telemetry.ReasonNone, StatusClass: telemetry.StatusClass3xx},
		},
		{
			name:   "400_malformed_query",
			status: http.StatusBadRequest,
			want:   telemetry.Outcome{Result: telemetry.ResultError, Reason: telemetry.ReasonBadRequest, StatusClass: telemetry.StatusClass4xx},
		},
		{
			name:   "404_unknown_route",
			status: http.StatusNotFound,
			want:   telemetry.Outcome{Result: telemetry.ResultError, Reason: telemetry.ReasonBadRequest, StatusClass: telemetry.StatusClass4xx},
		},
		{
			name:   "408_request_timeout",
			status: http.StatusRequestTimeout,
			want:   telemetry.Outcome{Result: telemetry.ResultError, Reason: telemetry.ReasonTimeout, StatusClass: telemetry.StatusClass4xx},
		},
		{
			name:   "422_unlowerable_query",
			status: http.StatusUnprocessableEntity,
			want:   telemetry.Outcome{Result: telemetry.ResultError, Reason: telemetry.ReasonBadRequest, StatusClass: telemetry.StatusClass4xx},
		},
		{
			name:   "429_admission_control",
			status: http.StatusTooManyRequests,
			want:   telemetry.Outcome{Result: telemetry.ResultError, Reason: telemetry.ReasonResourceExhausted, StatusClass: telemetry.StatusClass4xx},
		},
		{
			name:   "500_defect",
			status: http.StatusInternalServerError,
			want:   telemetry.Outcome{Result: telemetry.ResultError, Reason: telemetry.ReasonInternal, StatusClass: telemetry.StatusClass5xx},
		},
		{
			name:   "502_backend_gone",
			status: http.StatusBadGateway,
			want:   telemetry.Outcome{Result: telemetry.ResultError, Reason: telemetry.ReasonBackendUnavailable, StatusClass: telemetry.StatusClass5xx},
		},
		{
			name:   "503_backend_refusing",
			status: http.StatusServiceUnavailable,
			want:   telemetry.Outcome{Result: telemetry.ResultError, Reason: telemetry.ReasonBackendUnavailable, StatusClass: telemetry.StatusClass5xx},
		},
		{
			name:   "504_backend_timeout",
			status: http.StatusGatewayTimeout,
			want:   telemetry.Outcome{Result: telemetry.ResultError, Reason: telemetry.ReasonTimeout, StatusClass: telemetry.StatusClass5xx},
		},
		{
			name:   "507_insufficient_storage",
			status: http.StatusInsufficientStorage,
			want:   telemetry.Outcome{Result: telemetry.ResultError, Reason: telemetry.ReasonResourceExhausted, StatusClass: telemetry.StatusClass5xx},
		},
		{
			name:   "unclassified_5xx_falls_back_to_internal",
			status: http.StatusNotExtended,
			want:   telemetry.Outcome{Result: telemetry.ResultError, Reason: telemetry.ReasonInternal, StatusClass: telemetry.StatusClass5xx},
		},
		{
			name:   "out_of_range_code_has_no_family",
			status: 999,
			want:   telemetry.Outcome{Result: telemetry.ResultError, Reason: telemetry.ReasonInternal, StatusClass: telemetry.StatusClassUnknown},
		},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			if got := telemetry.ClassifyStatus(tc.status); got != tc.want {
				t.Errorf("ClassifyStatus(%d) = %+v; want %+v", tc.status, got, tc.want)
			}
		})
	}
}

// TestClassifyStatus_ReasonVocabularyIsClosed walks the whole status
// space and asserts every code maps into the declared enum. This is the
// bound on the label's cardinality: no status code, however exotic, can
// introduce a reason value dashboards and alert rules have never seen.
func TestClassifyStatus_ReasonVocabularyIsClosed(t *testing.T) {
	// Derived from the one authoritative membership list rather than
	// re-typed: a hand-written copy here silently stops gating the moment a
	// member is added to the enum and not to the copy.
	reasons := map[string]bool{}
	for _, r := range telemetry.ErrorReasons() {
		reasons[r] = true
	}
	classes := map[string]bool{
		telemetry.StatusClass1xx:     true,
		telemetry.StatusClass2xx:     true,
		telemetry.StatusClass3xx:     true,
		telemetry.StatusClass4xx:     true,
		telemetry.StatusClass5xx:     true,
		telemetry.StatusClassUnknown: true,
	}
	const beyondAnyStatusCode = 1000
	for status := range beyondAnyStatusCode {
		out := telemetry.ClassifyStatus(status)
		if !reasons[out.Reason] {
			t.Fatalf("ClassifyStatus(%d): reason %q outside the enum", status, out.Reason)
		}
		if !classes[out.StatusClass] {
			t.Fatalf("ClassifyStatus(%d): status class %q outside the enum", status, out.StatusClass)
		}
		if out.Result != telemetry.ResultOK && out.Result != telemetry.ResultError {
			t.Fatalf("ClassifyStatus(%d): result %q outside the enum", status, out.Result)
		}
		// Success and failure must never share a reason: "none" is what
		// makes the ok series' label set identical to the error series'
		// without claiming a failure that did not happen.
		if (out.Result == telemetry.ResultOK) != (out.Reason == telemetry.ReasonNone) {
			t.Fatalf("ClassifyStatus(%d) = %+v: result and reason disagree", status, out)
		}
	}
}

// TestAsCanceled_StaysInsideTheEnumAndOnlyRelabelsErrors pins the one reason
// no status code can produce, so it is the one member the walk above cannot
// reach.
//
// A client cancellation surfaces as 499 on Tempo and 503 on Prometheus/Loki —
// the same event under two statuses, which reasonForStatus reads as
// bad_request and backend_unavailable respectively. Neither is true, and
// neither status can move (Tempo's 499 keeps a client hang-up out of the 5xx
// band; prom/loki's 503 is upstream wire parity). AsCanceled is what makes the
// three heads agree (cerberus issue #3197).
func TestAsCanceled_StaysInsideTheEnumAndOnlyRelabelsErrors(t *testing.T) {
	reasons := map[string]bool{}
	for _, r := range telemetry.ErrorReasons() {
		reasons[r] = true
	}

	// Every status a head actually answers a cancellation with must land on
	// the SAME reason, which is the whole point of the change.
	for _, status := range []int{
		tempoClientClosedRequest,
		http.StatusServiceUnavailable,
	} {
		got := telemetry.ClassifyStatus(status).AsCanceled()
		if got.Reason != telemetry.ReasonCanceled {
			t.Errorf("ClassifyStatus(%d).AsCanceled() reason = %q; want %q — a cancellation must read the same on every head",
				status, got.Reason, telemetry.ReasonCanceled)
		}
		if !reasons[got.Reason] {
			t.Errorf("AsCanceled produced %q, outside the closed enum", got.Reason)
		}
		if got.Result != telemetry.ResultError {
			t.Errorf("ClassifyStatus(%d).AsCanceled() result = %q; want error", status, got.Result)
		}
	}

	// A successful query whose client disconnected during the write was still
	// answered: it must stay ok/none, or the ok-vs-error ratio starts counting
	// successes as failures.
	ok := telemetry.ClassifyStatus(http.StatusOK).AsCanceled()
	if ok.Result != telemetry.ResultOK || ok.Reason != telemetry.ReasonNone {
		t.Errorf("ClassifyStatus(200).AsCanceled() = %+v; want ok/none unchanged", ok)
	}
}

// tempoClientClosedRequest is the 499 Tempo answers a cancellation with. Named
// locally rather than imported: internal/telemetry must not depend on a head.
const tempoClientClosedRequest = 499

// TestOutcomeOK is the shorthand used by call sites with no HTTP status
// to classify.
func TestOutcomeOK(t *testing.T) {
	if got, want := telemetry.OutcomeOK(), telemetry.ClassifyStatus(http.StatusOK); got != want {
		t.Errorf("OutcomeOK() = %+v; want %+v", got, want)
	}
}
