//go:build chdb

// Behavioural coverage for /index/volume's ranking against real
// ClickHouse semantics (chDB). The bug pinned here is a WRONG ANSWER
// that only a real engine can exhibit: the endpoint truncated to `limit`
// under `ORDER BY bytes DESC` alone, which is not a total order, so
// which of several equal-volume streams survived the cap was whatever
// the aggregation happened to emit first — a choice ClickHouse does not
// promise to repeat.
//
// A stub querier hands back a fixed slice and can never show it. Only
// executing the emitted SQL against an engine that actually groups,
// orders and truncates can.

package loki_test

import (
	"encoding/json"
	"fmt"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
	"time"

	"github.com/tsouza/cerberus/internal/api/loki"
	"github.com/tsouza/cerberus/internal/chclienttest"
	"github.com/tsouza/cerberus/internal/schema"
)

// volumeTieBase frames the seeded rows.
var volumeTieBase = time.Date(2026, 5, 14, 12, 0, 0, 0, time.UTC)

// tiedVolumePods are the twelve streams seeded with an IDENTICAL byte
// volume. They are inserted in an order that is neither ascending nor
// descending by name, so nothing about the seed nudges the engine toward
// the ranking the assertions require.
//
// Twelve is not decoration. The cap below keeps two of them, so an
// engine picking an arbitrary pair would have to hit the one correct
// subset out of the sixty-six possible ones for this test to pass by
// accident — and it does not: the observed pre-fix answers are recorded
// on [TestIndexVolume_ChDB_TiedVolumesTruncateDeterministically].
var tiedVolumePods = []string{
	"delta", "alpha", "echo", "charlie", "bravo", "foxtrot",
	"golf", "hotel", "india", "juliet", "kilo", "lima",
}

// volumeTieSeed writes fourteen streams under one `job="api"` selector:
//
//   - `pod="zulu"`   — 30 body bytes, the single largest volume. Its name
//     sorts LAST, so a ranking that forgot the volume key entirely would
//     put it at the bottom.
//   - the twelve [tiedVolumePods] — 10 body bytes each, a twelve-way tie
//     that straddles the cap.
//   - `pod="aaa"`    — 5 body bytes, the smallest volume. Its name sorts
//     FIRST, so a ranking that ordered by name alone would keep it.
func volumeTieSeed() string {
	rows := []string{
		volumeTieRow("zulu", 30),
		volumeTieRow("aaa", 5),
	}
	for _, pod := range tiedVolumePods {
		rows = append(rows, volumeTieRow(pod, 10))
	}
	return `CREATE TABLE otel_logs (
    Timestamp DateTime64(9),
    Body String,
    ServiceName String,
    ResourceAttributes Map(String, String)
) ENGINE = Memory;
INSERT INTO otel_logs (Timestamp, Body, ServiceName, ResourceAttributes) VALUES
    ` + strings.Join(rows, ",\n    ") + ";"
}

// volumeTieRow writes one log line for `pod` whose Body is exactly
// `size` bytes long, so the group's `sum(length(Body))` is `size`.
func volumeTieRow(pod string, size int) string {
	return fmt.Sprintf(
		"(toDateTime64('2026-05-14 12:00:00.000', 9), '%s', 'svc', map('job','api','pod','%s'))",
		strings.Repeat("x", size), pod,
	)
}

// queryVolume issues one /index/volume request and returns the samples
// in the order the response carries them.
func queryVolume(t *testing.T, srvURL string, limit int) []loki.VectorSample {
	t.Helper()
	var parsed struct {
		Data loki.QueryData `json:"data"`
	}
	getJSON(t, fmt.Sprintf(
		`%s/loki/api/v1/index/volume?query=%%7Bjob%%3D%%22api%%22%%7D&start=%d&end=%d&limit=%d`,
		srvURL,
		volumeTieBase.Add(-time.Minute).Unix(),
		volumeTieBase.Add(time.Minute).Unix(),
		limit,
	), &parsed)

	raw, err := json.Marshal(parsed.Data.Result)
	if err != nil {
		t.Fatalf("re-marshal result: %v", err)
	}
	var samples []loki.VectorSample
	if err := json.Unmarshal(raw, &samples); err != nil {
		t.Fatalf("decode vector: %v", err)
	}
	return samples
}

// podOrder reduces a response to the `pod` label of each sample, in
// response order, so an assertion can name the whole ranking at once.
func podOrder(samples []loki.VectorSample) []string {
	out := make([]string, 0, len(samples))
	for _, s := range samples {
		out = append(out, s.Metric["pod"])
	}
	return out
}

// newVolumeServer seeds one chDB session and mounts the Loki handler over
// it, returning the base URL [queryVolume] asks against.
func newVolumeServer(t *testing.T, seed string) string {
	t.Helper()
	c := chclienttest.NewChDB(t)
	c.Seed(t, seed)
	h := loki.New(c, schema.DefaultOTelLogs(), nil)
	mux := http.NewServeMux()
	h.Mount(mux)
	srv := httptest.NewServer(mux)
	t.Cleanup(srv.Close)
	return srv.URL
}

// TestIndexVolume_ChDB_TiedVolumesTruncateDeterministically is the
// wrong-answer regression, and the half that only the SQL can fix.
// Upstream Loki establishes a TOTAL order before it truncates —
// `MapToVolumeResponse`
// (pkg/storage/stores/index/seriesvolume/volume.go) sorts by volume
// descending, falls back to the entry's own Name ascending, and only
// then slices to `limit` — so its top-N is a function of the data alone.
// Cerberus truncated under `ORDER BY bytes DESC` with no second key, so
// the twelve-way tie seeded here was cut wherever the aggregation
// happened to emit its groups.
//
// The assertion is which rows SURVIVE the cap, which no Go-side re-sort
// can repair: the rows it would have to reorder are already gone. With
// `limit=3` upstream keeps zulu (the largest volume) and then the two
// alphabetically-first members of the tie.
//
// It discriminates. Measured against origin/main's single-key ORDER BY
// on this seed, the surviving tie members were `{bravo, delta}` at
// limit=3 and `{delta, alpha, golf}` at limit=5 — neither the correct
// subset, and not even the same subset as each other, which is the
// nondeterminism itself: the same rows and the same query, cut
// differently because `limit` changed how much of the tie the engine
// bothered to order.
func TestIndexVolume_ChDB_TiedVolumesTruncateDeterministically(t *testing.T) {
	srvURL := newVolumeServer(t, volumeTieSeed())

	// zulu plus the two alphabetically-first members of the tie.
	const keptRows = 3
	got := podOrder(queryVolume(t, srvURL, keptRows))
	want := []string{"zulu", "alpha", "bravo"}
	if !equalStrings(got, want) {
		t.Fatalf("limit=%d ranking: got %v, want %v — the twelve-way volume tie must be cut "+
			"by label-set name ascending, the fallback key upstream's MapToVolumeResponse "+
			"applies before it slices", keptRows, got, want)
	}
}

// TestIndexVolume_ChDB_UntruncatedTieOrderIsUpstreams pins the SERVING
// half of the same rule. `toPrometheusData`
// (pkg/querier/queryrange/volume.go) re-applies volume-descending /
// name-ascending to the vector it writes out, so a caller reading the
// response top-down sees the same ranking whether or not a cap trimmed
// it. Asking for every seeded row (a limit above the row count) isolates
// that ordering from the truncation the sibling test covers.
//
// It fails on origin/main too, and for the complementary reason: nothing
// re-ranked what ClickHouse returned, so the twelve tied streams arrived
// in the aggregation's own order (`foxtrot, echo, delta, alpha, bravo,
// charlie, …` on this seed) rather than in name order.
func TestIndexVolume_ChDB_UntruncatedTieOrderIsUpstreams(t *testing.T) {
	srvURL := newVolumeServer(t, volumeTieSeed())

	// One above the fourteen seeded streams, so nothing is cut.
	const keptRows = 15
	got := podOrder(queryVolume(t, srvURL, keptRows))
	want := []string{
		"zulu",
		"alpha", "bravo", "charlie", "delta", "echo", "foxtrot",
		"golf", "hotel", "india", "juliet", "kilo", "lima",
		"aaa",
	}
	if !equalStrings(got, want) {
		t.Fatalf("limit=%d ranking: got %v, want %v — volume descending first (zulu ahead of "+
			"every tied stream, aaa behind them), then label-set name ascending inside the tie",
			keptRows, got, want)
	}
}

// equalStrings reports element-wise equality, so a ranking assertion can
// fail with both slices printed rather than with a reflect dump.
func equalStrings(got, want []string) bool {
	if len(got) != len(want) {
		return false
	}
	for i := range got {
		if got[i] != want[i] {
			return false
		}
	}
	return true
}

// dottedKeySeed is two equal-volume streams whose RAW OTel attribute keys
// collate in the opposite order to the Prometheus label names the response
// serves them under.
//
// `a.b` and `aZ` sort `a.b` first as stored (`.` is 0x2E, `Z` is 0x5A).
// [format.NormalizeLabelMap] rewrites the dotted key to `a_b` on the way
// out, and `a_b` sorts AFTER `aZ` (`_` is 0x5F). Upstream compares the
// names it serves — `seriesLabels.String()` over the stream's own labels —
// so `aZ` is the one that must come first.
const dottedKeySeed = `CREATE TABLE otel_logs (
    Timestamp DateTime64(9),
    Body String,
    ServiceName String,
    ResourceAttributes Map(String, String)
) ENGINE = Memory;
INSERT INTO otel_logs (Timestamp, Body, ServiceName, ResourceAttributes) VALUES
    (toDateTime64('2026-05-14 12:00:00.000', 9), 'xxxxxxxxxx', 'svc', map('job','api','a.b','1')),
    (toDateTime64('2026-05-14 12:00:00.000', 9), 'xxxxxxxxxx', 'svc', map('job','api','aZ','1'));`

// TestIndexVolume_ChDB_TieOrderUsesServedLabelNames pins that the tie is
// broken on the label names the RESPONSE carries, not on the raw storage
// keys the GROUP BY ran over.
//
// The SQL's second ORDER BY key cannot answer this on its own: it sees
// `ResourceAttributes` as stored, before the OTel-to-Prometheus name
// rewrite, and on this seed that collates the two streams the wrong way
// round. Ranking the returned rows with upstream's own comparator over
// the served names is what settles it — which is why this test fails when
// that re-rank is removed even with the SQL key in place, the exact
// converse of [TestIndexVolume_ChDB_TiedVolumesTruncateDeterministically].
func TestIndexVolume_ChDB_TieOrderUsesServedLabelNames(t *testing.T) {
	srvURL := newVolumeServer(t, dottedKeySeed)

	const keptRows = 2
	samples := queryVolume(t, srvURL, keptRows)
	got := make([]string, 0, len(samples))
	for _, s := range samples {
		for k := range s.Metric {
			if k != "job" {
				got = append(got, k)
			}
		}
	}
	want := []string{"aZ", "a_b"}
	if !equalStrings(got, want) {
		t.Fatalf("tie order by served label name: got %v, want %v — upstream ranks on "+
			"seriesLabels.String(), which is built from the names the response carries", got, want)
	}
}
