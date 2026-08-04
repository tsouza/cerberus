package main

import (
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
	"time"
)

// statusServer starts a test server that always answers the given
// status code, echoing the request body so mismatch tests can assert
// on it.
func statusServer(t *testing.T, status int) *httptest.Server {
	t.Helper()
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.WriteHeader(status)
		_, _ = w.Write([]byte(`{"status":"error","message":"boom"}`))
	}))
	t.Cleanup(srv.Close)
	return srv
}

// TestCompareStatusParityOne_BothMatchExpectation_Passes — the case
// this axis exists to make representable: both backends reject with
// the expected status, and the pass records that as a plain success
// rather than folding it into an opaque fetch error.
func TestCompareStatusParityOne_BothMatchExpectation_Passes(t *testing.T) {
	ref := statusServer(t, http.StatusBadRequest)
	test := statusServer(t, http.StatusBadRequest)

	f := flags{addr1: ref.URL, addr2: test.URL}
	c := &http.Client{Timeout: 5 * time.Second}
	sc := statusParityCases()[0]

	end := time.Now().UTC()
	start := end.Add(-time.Hour)
	result := compareStatusParityOne(c, f, sc, start, end)

	if !result.success() {
		t.Fatalf("expected a passing result, got: %+v", result)
	}
}

// TestCompareStatusParityOne_MismatchCaught — the exact blind spot
// #1487 et al. describe: one backend accepts what the other rejects.
// Both directions must surface as UnexpectedFailure, not a silent
// pass or a generic "fetch error" that loses which side diverged.
func TestCompareStatusParityOne_MismatchCaught(t *testing.T) {
	tests := []struct {
		name       string
		refStatus  int
		testStatus int
		wantSubstr string
	}{
		{"reference accepts, test rejects", http.StatusOK, http.StatusBadRequest, "reference"},
		{"reference rejects, test accepts", http.StatusBadRequest, http.StatusOK, "test endpoint"},
		{"both wrong status class", http.StatusInternalServerError, http.StatusOK, "reference status=500 test status=200"},
	}
	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			ref := statusServer(t, tc.refStatus)
			test := statusServer(t, tc.testStatus)

			f := flags{addr1: ref.URL, addr2: test.URL}
			c := &http.Client{Timeout: 5 * time.Second}
			sc := statusParityCases()[0]

			end := time.Now().UTC()
			start := end.Add(-time.Hour)
			result := compareStatusParityOne(c, f, sc, start, end)

			if result.success() {
				t.Fatalf("expected a failing result, got success: %+v", result)
			}
			if !strings.Contains(result.UnexpectedFailure, tc.wantSubstr) {
				t.Fatalf("UnexpectedFailure = %q, want substring %q", result.UnexpectedFailure, tc.wantSubstr)
			}
		})
	}
}

// TestCompareStatusParityOne_FetchFailureDistinctFromMismatch — a
// genuine network failure (server unreachable) must be attributed to
// the side that failed, never silently reinterpreted as a status of 0
// matching (or not matching) the expectation.
func TestCompareStatusParityOne_FetchFailureDistinctFromMismatch(t *testing.T) {
	test := statusServer(t, http.StatusBadRequest)

	f := flags{addr1: "http://127.0.0.1:1", addr2: test.URL} // port 0 refuses connections
	c := &http.Client{Timeout: 2 * time.Second}
	sc := statusParityCases()[0]

	end := time.Now().UTC()
	start := end.Add(-time.Hour)
	result := compareStatusParityOne(c, f, sc, start, end)

	if result.success() {
		t.Fatal("expected a failing result on unreachable reference")
	}
	if !strings.Contains(result.UnexpectedFailure, "reference") {
		t.Fatalf("UnexpectedFailure should name the reference side: %q", result.UnexpectedFailure)
	}
}

// TestStatusParityCases_ParamsWireCorrectly — sanity check that the
// two fixed cases actually send the request shape their description
// promises (missing query / malformed query), so a future edit to the
// case table can't silently drift the params away from the intent.
func TestStatusParityCases_ParamsWireCorrectly(t *testing.T) {
	cases := statusParityCases()
	if len(cases) < 2 {
		t.Fatalf("expected at least 2 status-parity cases, got %d", len(cases))
	}

	start := time.Unix(0, 0)
	end := time.Unix(3600, 0)

	missingQuery := cases[0].params(start, end)
	if missingQuery.Get("query") != "" {
		t.Errorf("case 0 (%s) should carry no query param, got %q", cases[0].desc, missingQuery.Get("query"))
	}
	if missingQuery.Get("start") == "" || missingQuery.Get("end") == "" {
		t.Errorf("case 0 (%s) should still carry start/end so the rejection is unambiguously about query", cases[0].desc)
	}

	badSyntax := cases[1].params(start, end)
	if badSyntax.Get("query") == "" {
		t.Errorf("case 1 (%s) should carry a (malformed) query param", cases[1].desc)
	}

	for _, sc := range cases {
		if sc.expect < 400 || sc.expect > 599 {
			t.Errorf("case %q: expect=%d is not a 4xx/5xx rejection", sc.desc, sc.expect)
		}
	}
}
