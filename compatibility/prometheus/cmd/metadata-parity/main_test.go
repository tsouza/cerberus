package main

import (
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
	"time"
)

// statusServer starts a test server that always answers the given
// status code, ignoring the request entirely — the driver only cares
// about the status class it gets back.
func statusServer(t *testing.T, status int) *httptest.Server {
	t.Helper()
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.WriteHeader(status)
		_, _ = w.Write([]byte(`{"status":"error","message":"boom"}`))
	}))
	t.Cleanup(srv.Close)
	return srv
}

// TestMatchShapes_LockstepWithUnitTests pins the driver's shape table
// against the exact set internal/api/prom/metadata_test.go's
// matchSelectorRejectedShapes pins — a range-vector call, offset, @,
// and an aggregation — plus the empty {} selector and one accepted
// selector, so a future edit can't silently let the two tables drift
// apart.
func TestMatchShapes_LockstepWithUnitTests(t *testing.T) {
	wantRejected := map[string]bool{
		"rate(http_requests_total[5m])": true,
		"foo offset 1h":                 true,
		"foo @ 1600000000":              true,
		"sum(foo)":                      true,
		"{}":                            true,
	}

	shapes := matchShapes()
	gotRejected := map[string]bool{}
	sawAccepted := false
	for _, sh := range shapes {
		switch sh.expect {
		case http.StatusBadRequest:
			gotRejected[sh.value] = true
		case http.StatusOK:
			sawAccepted = true
		default:
			t.Errorf("shape %q: unexpected expect=%d, want 200 or 400", sh.desc, sh.expect)
		}
	}
	if !sawAccepted {
		t.Error("expected at least one shape asserting a 200 (a syntactically valid selector must be accepted)")
	}
	for v := range wantRejected {
		if !gotRejected[v] {
			t.Errorf("expected match[]=%q among the rejected shapes, got %v", v, gotRejected)
		}
	}
	for v := range gotRejected {
		if !wantRejected[v] {
			t.Errorf("unexpected rejected shape %q not present in the unit-test lockstep set", v)
		}
	}
}

// TestMetadataEndpoints_MatchesFixedPromEndpoints pins the three routes
// #1487 fixed match[] parsing on.
func TestMetadataEndpoints_MatchesFixedPromEndpoints(t *testing.T) {
	want := map[string]string{
		"series":       "/api/v1/series",
		"labels":       "/api/v1/labels",
		"label_values": "/api/v1/label/job/values",
	}
	got := metadataEndpoints()
	if len(got) != len(want) {
		t.Fatalf("expected %d endpoints, got %d: %+v", len(want), len(got), got)
	}
	for _, ep := range got {
		if want[ep.desc] != ep.path {
			t.Errorf("endpoint %q: path=%q, want %q", ep.desc, ep.path, want[ep.desc])
		}
	}
}

// TestRunCase_BothMatchExpectation_Passes is the case this driver
// exists to make representable: both backends answer the shape's
// expected status, and it's recorded as parity.
func TestRunCase_BothMatchExpectation_Passes(t *testing.T) {
	ref := statusServer(t, http.StatusBadRequest)
	test := statusServer(t, http.StatusBadRequest)

	client := &http.Client{Timeout: 5 * time.Second}
	sh := matchShape{desc: "empty {} selector", value: "{}", expect: http.StatusBadRequest}
	ep := metadataEndpoint{desc: "labels", path: "/api/v1/labels"}

	res := runCase(client, ref.URL, test.URL, ep, sh)
	if res.Verdict != "parity" {
		t.Fatalf("expected parity, got %+v", res)
	}
}

// TestRunCase_MismatchCaught is the exact blind spot #1729 describes:
// one backend accepts a shape the other rejects. Both divergence
// directions, and a both-wrong-status case, must surface as
// "mismatch", never a silent pass.
func TestRunCase_MismatchCaught(t *testing.T) {
	tests := []struct {
		name       string
		refStatus  int
		testStatus int
		wantSubstr string
	}{
		{"reference accepts, cerberus rejects", http.StatusOK, http.StatusBadRequest, "reference diverged"},
		{"reference rejects, cerberus accepts", http.StatusBadRequest, http.StatusOK, "cerberus diverged"},
		{"both wrong status", http.StatusNotFound, http.StatusOK, "both diverged"},
	}
	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			ref := statusServer(t, tc.refStatus)
			test := statusServer(t, tc.testStatus)

			client := &http.Client{Timeout: 5 * time.Second}
			sh := matchShape{desc: "empty {} selector", value: "{}", expect: http.StatusBadRequest}
			ep := metadataEndpoint{desc: "labels", path: "/api/v1/labels"}

			res := runCase(client, ref.URL, test.URL, ep, sh)
			if res.Verdict != "mismatch" {
				t.Fatalf("expected mismatch, got: %+v", res)
			}
			if !strings.Contains(res.Detail, tc.wantSubstr) {
				t.Fatalf("Detail = %q, want substring %q", res.Detail, tc.wantSubstr)
			}
		})
	}
}

// TestRunCase_FetchFailureDistinctFromMismatch pins that an unreachable
// backend is attributed to the side that failed, never silently
// reinterpreted as a status-0 mismatch.
func TestRunCase_FetchFailureDistinctFromMismatch(t *testing.T) {
	test := statusServer(t, http.StatusBadRequest)

	client := &http.Client{Timeout: 2 * time.Second}
	sh := matchShape{desc: "empty {} selector", value: "{}", expect: http.StatusBadRequest}
	ep := metadataEndpoint{desc: "labels", path: "/api/v1/labels"}

	res := runCase(client, "http://127.0.0.1:1", test.URL, ep, sh)
	if res.Verdict != "hard_error" {
		t.Fatalf("expected hard_error on unreachable reference, got: %+v", res)
	}
	if !strings.Contains(res.Detail, "reference") {
		t.Fatalf("Detail should name the reference side: %q", res.Detail)
	}
}

// TestReport_Fatal pins the exit-semantics contract: only a mismatch
// fails the driver, a hard_error alone does not.
func TestReport_Fatal(t *testing.T) {
	cases := []struct {
		name string
		rep  Report
		want bool
	}{
		{"all parity", Report{Total: 3, Parity: 3}, false},
		{"one hard_error only", Report{Total: 3, Parity: 2, HardError: 1}, false},
		{"one mismatch", Report{Total: 3, Parity: 2, Mismatch: 1}, true},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			if got := tc.rep.Fatal(); got != tc.want {
				t.Errorf("Fatal() = %v, want %v", got, tc.want)
			}
		})
	}
}
