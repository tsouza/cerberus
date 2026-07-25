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
	reasons := map[string]bool{
		telemetry.ReasonNone:               true,
		telemetry.ReasonBadRequest:         true,
		telemetry.ReasonBackendUnavailable: true,
		telemetry.ReasonResourceExhausted:  true,
		telemetry.ReasonTimeout:            true,
		telemetry.ReasonInternal:           true,
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

// TestOutcomeOK is the shorthand used by call sites with no HTTP status
// to classify.
func TestOutcomeOK(t *testing.T) {
	if got, want := telemetry.OutcomeOK(), telemetry.ClassifyStatus(http.StatusOK); got != want {
		t.Errorf("OutcomeOK() = %+v; want %+v", got, want)
	}
}
