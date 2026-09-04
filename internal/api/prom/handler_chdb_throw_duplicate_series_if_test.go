//go:build chdb

// chDB-backed wire-level pins for chopt.FeatureTSThrowDuplicateSeriesIf
// (cerberus issue #3038): swapping the duplicate-labelset guard's HAVING
// from the hand-rolled throwIf(uniqExact(MetricName) > 1, <static
// message>) onto ClickHouse's own timeSeriesThrowDuplicateSeriesIf, which
// names the ACTUAL colliding tags in its error message.
//
// handler_chdb_duplicate_labelset_test.go and
// handler_chdb_instant_duplicate_labelset_test.go already pin the guard's
// CORRECTNESS (which shapes must reject, which must pass through) with the
// feature at its default (off) value, asserting the static
// chplan.DuplicateLabelsetMessage. This file is the feature's own
// differential: the SAME collisions, with ThrowDuplicateSeriesIf turned on,
// asserting the response instead names the real colliding tag — the whole
// point of #3038 — while still landing on the identical 422
// errorType=execution wire shape and leaking no ClickHouse internals
// (neither a raw "DB::Exception" stack frame nor the "while executing
// 'FUNCTION ...'" trailer the emitted SQL's own aliases would otherwise
// expose).
//
// Three guard call sites share duplicateLabelsetGuardExpr
// (internal/promql/lower.go) and are each covered once: the instant
// name-drop guard (guardNameDropCollision), the range-vector name-drop
// guard (wrapDropNameCollisionGuard), and the ts_tag_groups variant
// (guardNameDropCollisionByTagGroup) — which passes a DIFFERENT groupID
// shape (the Aggregate's own tag-group-id column, not a freshly derived
// timeSeriesTagsToGroup(Attributes) call) and is exercised with BOTH
// chopt.FeatureTSGridTagGroups and chopt.FeatureTSThrowDuplicateSeriesIf on
// at once.

package prom_test

import (
	"encoding/json"
	"fmt"
	"net/http"
	"net/http/httptest"
	"net/url"
	"strings"
	"testing"

	"github.com/tsouza/cerberus/internal/api/prom"
	"github.com/tsouza/cerberus/internal/chclienttest"
	"github.com/tsouza/cerberus/internal/chplan"
	"github.com/tsouza/cerberus/internal/schema"
)

// newChDBServerWithThrowDuplicateSeriesIf builds a handler with
// ThrowDuplicateSeriesIf (and, optionally, TagGroups) set, then seeds it —
// mirroring newChDBServerWithZeroThreshold's pattern for a Handler field a
// plain newChDBServer leaves at its zero value.
func newChDBServerWithThrowDuplicateSeriesIf(t *testing.T, ddl string, tagGroups bool) *httptest.Server {
	t.Helper()
	c := chclienttest.NewChDB(t)
	c.Seed(t, ddl)
	h := prom.New(c, schema.DefaultOTelMetrics(), nil)
	h.ThrowDuplicateSeriesIf = true
	h.TagGroups = tagGroups
	mux := http.NewServeMux()
	h.Mount(mux)
	srv := httptest.NewServer(mux)
	t.Cleanup(srv.Close)
	return srv
}

// assertDuplicateSeriesTagsRejected pins the collector-backed variant's
// wire contract: the SAME 422 errorType=execution shape
// assertDuplicateLabelsetRejected pins for the static-message path, but
// the message must name the REAL colliding tag (here, `host`) rather than
// carry the static chplan.DuplicateLabelsetMessage — and must still leak
// neither a raw ClickHouse exception frame nor the emitted SQL's own
// internal aliases.
func assertDuplicateSeriesTagsRejected(t *testing.T, body string, status int, query string) {
	t.Helper()
	if status != http.StatusUnprocessableEntity {
		t.Fatalf("%s: status: got %d, want %d; body=%s",
			query, status, http.StatusUnprocessableEntity, body)
	}
	var parsed prom.Response
	if err := json.Unmarshal([]byte(body), &parsed); err != nil {
		t.Fatalf("%s: unmarshal: %v; body=%s", query, err, body)
	}
	if parsed.Status != "error" {
		t.Fatalf("%s: status: got %q, want %q; body=%s", query, parsed.Status, "error", body)
	}
	if parsed.ErrorType != prom.ErrExecution {
		t.Fatalf("%s: errorType: got %q, want %q; body=%s",
			query, parsed.ErrorType, prom.ErrExecution, body)
	}
	if !strings.HasPrefix(parsed.Error, chplan.DuplicateSeriesTagsMessagePrefix) {
		t.Fatalf("%s: error: got %q, want it to start with %q (the collector-backed variant); body=%s",
			query, parsed.Error, chplan.DuplicateSeriesTagsMessagePrefix, body)
	}
	// The whole point of #3038: the message must name the REAL colliding
	// tag, not the static message every call site raises with the feature
	// off (dupLabelsetSeed's collision seeds a shared `host` tag — see
	// this file's own callers).
	if !strings.Contains(parsed.Error, "host") {
		t.Errorf("%s: error %q does not name the actual colliding tag (\"host\") — "+
			"looks like the static message, not timeSeriesThrowDuplicateSeriesIf's own; body=%s",
			query, parsed.Error, body)
	}
	if parsed.Error == chplan.DuplicateLabelsetMessage {
		t.Errorf("%s: error is the OLD static message verbatim — "+
			"ThrowDuplicateSeriesIf did not take effect; body=%s", query, body)
	}
	if strings.Contains(parsed.Error, "DB::Exception") {
		t.Errorf("%s: error leaks the ClickHouse exception: %q", query, parsed.Error)
	}
	if strings.Contains(parsed.Error, "while executing") {
		t.Errorf("%s: error leaks ClickHouse's function-call trailer "+
			"(internal SQL aliases): %q", query, parsed.Error)
	}
}

// TestQuery_InstantNameDrop_DuplicateSeriesTags_ChDB is the instant
// name-drop guard's (guardNameDropCollision, ThrowDuplicateSeriesIf=true,
// TagGroups=false) differential against
// TestQuery_InstantNameDrop_DuplicateLabelset_ChDB: the identical
// collision, but naming the real tag.
func TestQuery_InstantNameDrop_DuplicateSeriesTags_ChDB(t *testing.T) {
	start, end, _ := subqueryNameWindow()
	srv := newChDBServerWithThrowDuplicateSeriesIf(t, dupLabelsetSeed(t, start, end,
		"map('host', 'a')", "map('host', 'a')"), false)

	const query = `ceil({__name__=~"cpu_temp|gpu_temp"})`
	status, body := instantQuery(t, srv.URL, query, end)
	assertDuplicateSeriesTagsRejected(t, body, status, query)
}

// TestQuery_InstantNameDrop_DistinctLabelsets_ThrowDuplicateSeriesIf_ChDB
// is the feature's pass-through control: two series with DIFFERENT
// attribute sets must still answer normally with the feature on, exactly
// as TestQuery_InstantNameDrop_DistinctLabelsets_ChDB pins with it off.
// Erroring here would mean the feature toggled the guard's SHAPE
// (over-firing), not just its message.
func TestQuery_InstantNameDrop_DistinctLabelsets_ThrowDuplicateSeriesIf_ChDB(t *testing.T) {
	start, end, _ := subqueryNameWindow()
	srv := newChDBServerWithThrowDuplicateSeriesIf(t, dupLabelsetSeed(t, start, end,
		"map('host', 'a')", "map('host', 'b')"), false)

	const query = `ceil({__name__=~"cpu_temp|gpu_temp"})`
	status, body := instantQuery(t, srv.URL, query, end)
	assertNamelessSeriesByHost(t, body, status, query, "a", "b")
}

// TestQueryRange_RateMultiName_DuplicateSeriesTags_ChDB is the RANGE-vector
// name-drop guard's (wrapDropNameCollisionGuard, lower.go) own
// differential against TestQuery_RateMultiName_DuplicateLabelset_ChDB —
// the other call site sharing duplicateLabelsetGuardExpr, reached from a
// completely different lowering (a name-dropping range function) rather
// than an instant math fn.
func TestQueryRange_RateMultiName_DuplicateSeriesTags_ChDB(t *testing.T) {
	start, end, _ := subqueryNameWindow()
	srv := newChDBServerWithThrowDuplicateSeriesIf(t, dupLabelsetSeed(t, start, end,
		"map('host', 'a')", "map('host', 'a')"), false)

	const query = `rate({__name__=~"cpu_temp|gpu_temp"}[5m])`
	status, body := getBody(t, fmt.Sprintf("%s/api/v1/query?query=%s&time=%d",
		srv.URL, url.QueryEscape(query), end.Unix()))
	assertDuplicateSeriesTagsRejected(t, body, status, query)
}

// TestQuery_TagGroupsNameDrop_DuplicateSeriesTags_ChDB is the
// ts_tag_groups variant's (guardNameDropCollisionByTagGroup) own
// differential, with BOTH chopt.FeatureTSGridTagGroups and
// chopt.FeatureTSThrowDuplicateSeriesIf on: this call site passes a
// DIFFERENT groupID shape to timeSeriesThrowDuplicateSeriesIf than the
// other two sites above — the Aggregate's own already-computed
// tag-group-id column (groupIDCol), not a freshly derived
// timeSeriesTagsToGroup(Attributes) call — so it needs its own real-CH
// pin rather than inheriting coverage from the plain-Attributes-grouped
// tests above.
func TestQuery_TagGroupsNameDrop_DuplicateSeriesTags_ChDB(t *testing.T) {
	start, end, _ := subqueryNameWindow()
	srv := newChDBServerWithThrowDuplicateSeriesIf(t, dupLabelsetSeed(t, start, end,
		"map('host', 'a')", "map('host', 'a')"), true)

	const query = `ceil({__name__=~"cpu_temp|gpu_temp"})`
	status, body := instantQuery(t, srv.URL, query, end)
	assertDuplicateSeriesTagsRejected(t, body, status, query)
}
