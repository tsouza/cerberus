//go:build chdb

// chDB-backed end-to-end coverage for the vector-vector binary operator's
// many-to-many refusal.
//
// The reference engine builds a match group per matching key and fails the
// query outright when the side that must carry exactly one series carries
// several (promql/engine.go: `many-to-many matching not allowed: matching
// labels must be unique on one side`). Cerberus does the matching in SQL,
// where the natural per-side aggregation collapses the ambiguity into one
// arbitrary representative — so without a guard the query answers 200 OK
// with one of the pairs and silently discards the rest.
//
// The guard exists, but until #1734 it rode as an unreferenced SELECT-list
// `throwIf` column that ClickHouse's analyzer pruned, so it never fired.
// These tests pin the observable contract at the wire rather than the SQL
// shape: an ambiguous match is a 422 execution error carrying upstream's
// verbatim wording, and an unambiguous one — including the `group_left`
// shape, where the "many" side is *supposed* to carry several series — is
// still answered.

package prom_test

import (
	"encoding/json"
	"fmt"
	"net/http"
	"net/url"
	"strings"
	"testing"
	"time"

	"github.com/tsouza/cerberus/internal/api/prom"
	"github.com/tsouza/cerberus/internal/chplan"
)

// manyToManySeedTime is the instant both the seed and the query use, so the
// bare-selector staleness window covers every seeded sample.
var manyToManySeedTime = time.Date(2026, 1, 1, 0, 0, 0, 0, time.UTC)

// manyToManySeed writes the `a` and `b` series named by rows. Each row is a
// ready-formatted VALUES tuple minus its timestamp, which is stamped here so
// every sample lands on manyToManySeedTime.
func manyToManySeed(rows ...string) string {
	ts := manyToManySeedTime.Format("2006-01-02 15:04:05.000000000")
	stamped := make([]string, len(rows))
	for i, r := range rows {
		stamped[i] = fmt.Sprintf(r, ts)
	}
	return gaugeDDL + "\nINSERT INTO otel_metrics_gauge VALUES\n    " +
		strings.Join(stamped, ",\n    ") + ";"
}

// queryAt runs an instant query at manyToManySeedTime and returns the status
// code, the decoded envelope and the raw body for failure messages.
func queryAt(t *testing.T, srvURL, q string) (int, prom.Response, string) {
	t.Helper()
	resp, err := http.Get(fmt.Sprintf("%s/api/v1/query?query=%s&time=%d",
		srvURL, url.QueryEscape(q), manyToManySeedTime.Unix()))
	if err != nil {
		t.Fatalf("GET %s: %v", q, err)
	}
	body := readBody(t, resp)
	var parsed prom.Response
	if err := json.Unmarshal([]byte(body), &parsed); err != nil {
		t.Fatalf("unmarshal: %v; body=%s", err, body)
	}
	return resp.StatusCode, parsed, body
}

// manyToManyVector runs the query and returns its vector samples keyed by the
// named label, so an assertion reads as "this label value carries this sum".
func manyToManyVector(t *testing.T, srvURL, q, byLabel string) map[string]string {
	t.Helper()
	got := map[string]string{}
	for _, s := range decodeVectorQuery(t, srvURL, q, manyToManySeedTime.Unix()) {
		got[s.Metric[byLabel]] = fmt.Sprint(s.Value[1])
	}
	return got
}

// TestVectorJoin_ManyToMany_ChDB is the ambiguous case. `on(job)` collapses
// both `a` series onto one match key and both `b` series onto one match key,
// so neither side can supply the unique partner one-to-one matching requires.
// Upstream rejects the query; cerberus must too, rather than answering with an
// arbitrary one of the two pairs.
func TestVectorJoin_ManyToMany_ChDB(t *testing.T) {
	seed := manyToManySeed(
		`('a', map('job', 'x', 'instance', 'i1'), toDateTime64('%s', 9), 1.0)`,
		`('a', map('job', 'x', 'instance', 'i2'), toDateTime64('%s', 9), 2.0)`,
		`('b', map('job', 'x', 'instance', 'i1'), toDateTime64('%s', 9), 10.0)`,
		`('b', map('job', 'x', 'instance', 'i2'), toDateTime64('%s', 9), 20.0)`,
	)
	srv, _ := newChDBServer(t, seed)

	status, parsed, body := queryAt(t, srv.URL, `a + on(job) b`)

	// Upstream classifies this as a query-level execution failure, not a
	// backend one: 422, errorType=execution. A 502 would be cerberus blaming
	// ClickHouse for a fault in the query, and a 200 would be the pre-#1734
	// behaviour of picking a pair and discarding the other.
	if status != http.StatusUnprocessableEntity {
		t.Fatalf("status: got %d, want %d; body=%s", status, http.StatusUnprocessableEntity, body)
	}
	if parsed.Status != "error" {
		t.Fatalf("status: got %q, want %q; body=%s", parsed.Status, "error", body)
	}
	if parsed.ErrorType != prom.ErrExecution {
		t.Fatalf("errorType: got %q, want %q; body=%s", parsed.ErrorType, prom.ErrExecution, body)
	}
	if !strings.Contains(parsed.Error, chplan.ManyToManyMatchMessage) {
		t.Fatalf("error: got %q, want it to name %q; body=%s",
			parsed.Error, chplan.ManyToManyMatchMessage, body)
	}
	// A ClickHouse stack frame leaking to the client would mean the abort was
	// forwarded rather than translated.
	if strings.Contains(parsed.Error, "DB::Exception") {
		t.Errorf("error leaks the ClickHouse exception: %q", parsed.Error)
	}
}

// TestVectorJoin_OneToOne_ChDB is the other half of the contract: a guard that
// rejected everything would pass the test above while breaking every ordinary
// vector-vector binop. One series per side per match key is unambiguous and
// must still be answered.
func TestVectorJoin_OneToOne_ChDB(t *testing.T) {
	seed := manyToManySeed(
		`('a', map('job', 'x', 'instance', 'i1'), toDateTime64('%s', 9), 1.0)`,
		`('a', map('job', 'y', 'instance', 'i2'), toDateTime64('%s', 9), 2.0)`,
		`('b', map('job', 'x', 'instance', 'i1'), toDateTime64('%s', 9), 10.0)`,
		`('b', map('job', 'y', 'instance', 'i2'), toDateTime64('%s', 9), 20.0)`,
	)
	srv, _ := newChDBServer(t, seed)

	byJob := manyToManyVector(t, srv.URL, `a + on(job) b`, "job")

	want := map[string]string{"x": "11", "y": "22"}
	if len(byJob) != len(want) {
		t.Fatalf("result: got %v, want %v", byJob, want)
	}
	for job, w := range want {
		if byJob[job] != w {
			t.Errorf("job=%q: got %q, want %q", job, byJob[job], w)
		}
	}
}

// TestVectorJoin_GroupLeft_ChDB pins the shape the guard must NOT reject: with
// `group_left`, several series on the left matching one on the right is the
// declared intent, so only the right ("one") side carries the guard. Two `a`
// series share job=x and both pair with the single `b`.
func TestVectorJoin_GroupLeft_ChDB(t *testing.T) {
	seed := manyToManySeed(
		`('a', map('job', 'x', 'instance', 'i1'), toDateTime64('%s', 9), 1.0)`,
		`('a', map('job', 'x', 'instance', 'i2'), toDateTime64('%s', 9), 2.0)`,
		`('b', map('job', 'x'), toDateTime64('%s', 9), 10.0)`,
	)
	srv, _ := newChDBServer(t, seed)

	byInstance := manyToManyVector(t, srv.URL, `a + on(job) group_left b`, "instance")

	want := map[string]string{"i1": "11", "i2": "12"}
	if len(byInstance) != len(want) {
		t.Fatalf("result: got %v, want %v (one per left series)", byInstance, want)
	}
	for inst, w := range want {
		if byInstance[inst] != w {
			t.Errorf("instance=%q: got %q, want %q", inst, byInstance[inst], w)
		}
	}
}

// TestVectorJoin_GroupLeft_ManyOnOneSide_ChDB completes the group_left
// contract: the modifier declares which side may be many, it does not disable
// the check on the other. Two `b` series collapsing onto job=x leaves the
// "one" side ambiguous, which upstream rejects with the same message.
func TestVectorJoin_GroupLeft_ManyOnOneSide_ChDB(t *testing.T) {
	seed := manyToManySeed(
		`('a', map('job', 'x', 'instance', 'i1'), toDateTime64('%s', 9), 1.0)`,
		`('b', map('job', 'x', 'shard', 's1'), toDateTime64('%s', 9), 10.0)`,
		`('b', map('job', 'x', 'shard', 's2'), toDateTime64('%s', 9), 20.0)`,
	)
	srv, _ := newChDBServer(t, seed)

	status, parsed, body := queryAt(t, srv.URL, `a + on(job) group_left b`)

	if status != http.StatusUnprocessableEntity {
		t.Fatalf("status: got %d, want %d; body=%s", status, http.StatusUnprocessableEntity, body)
	}
	if !strings.Contains(parsed.Error, chplan.ManyToManyMatchMessage) {
		t.Fatalf("error: got %q, want it to name %q; body=%s",
			parsed.Error, chplan.ManyToManyMatchMessage, body)
	}
}
