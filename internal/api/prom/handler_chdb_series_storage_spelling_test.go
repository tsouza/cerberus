//go:build chdb

package prom_test

import (
	"encoding/json"
	"fmt"
	"net/http"
	"net/http/httptest"
	"net/url"
	"sort"
	"strings"
	"testing"
	"time"

	"github.com/tsouza/cerberus/internal/api/prom"
	"github.com/tsouza/cerberus/internal/chclienttest"
	"github.com/tsouza/cerberus/internal/schema"
)

// This file is the correctness proof for the #1528 retirement of the
// metadata-side matcher-string fan-out over dotted-storage candidates
// (`expandUnderscoredMetricNameMatcher`). That helper expanded one
// `match[]` selector into one matcher STRING per candidate spelling; the
// predicate-level fan-out in internal/promql.metricNamePredicate resolves
// the identical candidate set inside a single arm, as a flat
// `MetricName IN (?,…)`. The retirement is only sound if the surviving
// layer reaches every stored spelling the retired one did.
//
// A shape-level assertion cannot establish that: the two layers put the
// candidate set in different places (matcher text vs. bound predicate),
// so their emitted SQL differs by construction. The proof has to be
// value-level — seed real rows under each competing storage spelling and
// assert WHICH ROWS come back. That is what these tests do, against chDB
// through the real in-process handler.
//
// The pinned sets below were captured from the pre-retirement (two-layer)
// handler and are asserted unchanged after it. The one spelling that
// deliberately moved is called out in
// TestSeriesStorageSpellings_InterleavedSeparatorMatchesQueryPath.

// storageSpellingProbeLabel is the attribute each seeded row carries so a
// returned series identifies the stored spelling it came from. `__name__`
// cannot serve: format.WithMetricName normalises every stored spelling
// back to the same Prom-grammar name on the wire, which is exactly the
// aliasing the fan-out exists to resolve — so without this label every
// spelling's series would be indistinguishable in the response.
const storageSpellingProbeLabel = "probe"

// storageSpellings is the universe of MetricName spellings a single
// Prom-grammar name (`http_server_request_body_size`) could plausibly be
// stored under, one row seeded per spelling:
//
//   - the Prom-grammar form itself (a producer that already normalised);
//   - the fully OTel-dotted form (the OTel-CH collector default);
//   - a partially dotted form (namespace dotted, leaf underscored);
//   - a contiguous dot/slash zone form (the GCP Cloud Monitoring shape
//     `domain.parts/path/leaf_name`);
//   - a NON-contiguous interleaved form, which
//     format.PromLabelToOTelCandidates documents as out of scope.
var storageSpellings = []string{
	"http_server_request_body_size",
	"http.server.request.body.size",
	"http.server.request_body_size",
	"http.server/request/body_size",
	"http.server/request.body.size",
}

// interleavedSpelling is the one member of storageSpellings that no single
// fan-out layer reaches — see the test that pins its behaviour.
const interleavedSpelling = "http.server/request.body.size"

// promGrammarName is the name a Grafana metric picker shows for every
// spelling above, and the name it sends back in `match[]`.
const promGrammarName = "http_server_request_body_size"

func storageSpellingSeed() string {
	var b strings.Builder
	b.WriteString(`CREATE OR REPLACE TABLE otel_metrics_gauge (
	  ServiceName String, MetricName String, Attributes Map(String,String),
	  ResourceAttributes Map(String,String) DEFAULT map(),
	  TimeUnix DateTime64(9), Value Float64
	) ENGINE = MergeTree() ORDER BY (ServiceName, MetricName, toUnixTimestamp64Nano(TimeUnix));`)
	b.WriteString(`CREATE OR REPLACE TABLE otel_metrics_sum (
	  ServiceName String, MetricName String, Attributes Map(String,String),
	  ResourceAttributes Map(String,String) DEFAULT map(),
	  TimeUnix DateTime64(9), Value Float64
	) ENGINE = MergeTree() ORDER BY (ServiceName, MetricName, toUnixTimestamp64Nano(TimeUnix));`)
	b.WriteString(`CREATE OR REPLACE TABLE otel_metrics_histogram (
	  ServiceName String, MetricName String, Attributes Map(String,String),
	  ResourceAttributes Map(String,String) DEFAULT map(),
	  TimeUnix DateTime64(9), Count UInt64, Sum Float64,
	  BucketCounts Array(UInt64), ExplicitBounds Array(Float64)
	) ENGINE = MergeTree() ORDER BY (ServiceName, MetricName, toUnixTimestamp64Nano(TimeUnix));`)
	for _, n := range storageSpellings {
		b.WriteString(fmt.Sprintf(
			`INSERT INTO otel_metrics_gauge (ServiceName, MetricName, Attributes, TimeUnix, Value) `+
				`VALUES ('svc','%s', map('%s','%s'), now64(9), 1);`,
			n, storageSpellingProbeLabel, n,
		))
		b.WriteString(fmt.Sprintf(
			`INSERT INTO otel_metrics_histogram (ServiceName, MetricName, Attributes, TimeUnix, `+
				`Count, Sum, BucketCounts, ExplicitBounds) `+
				`VALUES ('svc','%s', map('%s','%s'), now64(9), 10, 5.0, [1,2,3],[0.1,0.5]);`,
			n, storageSpellingProbeLabel, n,
		))
	}
	return b.String()
}

func storageSpellingServer(t *testing.T) *httptest.Server {
	t.Helper()
	client := chclienttest.NewChDB(t)
	client.Seed(t, storageSpellingSeed())
	h := prom.New(client, schema.DefaultOTelMetrics(), nil)
	mux := http.NewServeMux()
	h.Mount(mux)
	srv := httptest.NewServer(mux)
	t.Cleanup(srv.Close)
	return srv
}

// seriesSpellings runs /api/v1/series for the given selector and returns the
// distinct set of `<stored spelling>|<wire __name__>` pairs it reports.
func seriesSpellings(t *testing.T, srv *httptest.Server, matcher string) []string {
	t.Helper()
	reqURL := fmt.Sprintf("%s/api/v1/series?match[]=%s&start=%d&end=%d",
		srv.URL, url.QueryEscape(matcher),
		time.Now().Add(-time.Hour).Unix(), time.Now().Add(time.Hour).Unix())
	resp, err := http.Get(reqURL)
	if err != nil {
		t.Fatalf("GET /series: %v", err)
	}
	defer resp.Body.Close()
	var body struct {
		Status string              `json:"status"`
		Error  string              `json:"error"`
		Data   []map[string]string `json:"data"`
	}
	if err := json.NewDecoder(resp.Body).Decode(&body); err != nil {
		t.Fatalf("decode /series: %v", err)
	}
	if resp.StatusCode != http.StatusOK || body.Status != "success" {
		t.Fatalf("/series status=%d body.status=%s err=%s", resp.StatusCode, body.Status, body.Error)
	}
	return distinctSorted(t, body.Data, "/series")
}

// querySpellings runs /api/v1/query for the given PromQL and returns the same
// `<stored spelling>|<wire __name__>` pairs, so the two surfaces are compared
// in identical terms.
func querySpellings(t *testing.T, srv *httptest.Server, promQL string) []string {
	t.Helper()
	reqURL := fmt.Sprintf("%s/api/v1/query?query=%s&time=%d",
		srv.URL, url.QueryEscape(promQL), time.Now().Unix())
	resp, err := http.Get(reqURL)
	if err != nil {
		t.Fatalf("GET /query: %v", err)
	}
	defer resp.Body.Close()
	var body struct {
		Status string `json:"status"`
		Error  string `json:"error"`
		Data   struct {
			Result []struct {
				Metric map[string]string `json:"metric"`
			} `json:"result"`
		} `json:"data"`
	}
	if err := json.NewDecoder(resp.Body).Decode(&body); err != nil {
		t.Fatalf("decode /query: %v", err)
	}
	if resp.StatusCode != http.StatusOK || body.Status != "success" {
		t.Fatalf("/query status=%d body.status=%s err=%s", resp.StatusCode, body.Status, body.Error)
	}
	sets := make([]map[string]string, 0, len(body.Data.Result))
	for _, r := range body.Data.Result {
		sets = append(sets, r.Metric)
	}
	return distinctSorted(t, sets, "/query")
}

func distinctSorted(t *testing.T, sets []map[string]string, surface string) []string {
	t.Helper()
	seen := make(map[string]struct{}, len(sets))
	out := make([]string, 0, len(sets))
	for _, l := range sets {
		spelling, ok := l[storageSpellingProbeLabel]
		if !ok {
			t.Fatalf("%s returned a series with no %q label: %v — the seeded rows all carry "+
				"one, so this series did not come from the probe corpus and the comparison "+
				"would be measuring something else", surface, storageSpellingProbeLabel, l)
		}
		key := spelling + "|" + l["__name__"]
		if _, dup := seen[key]; dup {
			continue
		}
		seen[key] = struct{}{}
		out = append(out, key)
	}
	sort.Strings(out)
	return out
}

// TestSeriesStorageSpellings_UnchangedByFanoutRetirement is the before/after
// value-level pin. The expected set was captured from the PRE-retirement
// two-layer handler; every entry in it is a stored row the metadata surface
// resolved then and must still resolve now.
//
// It covers both halves of the old composition:
//
//   - the bare arm (`G:` rows) — a gauge row stored under a dotted spelling,
//     which the retired layer reached by rewriting the matcher string and the
//     surviving layer reaches through `MetricName IN (?,…)`;
//   - the classic-histogram companion arms (`H:` rows) — a histogram row
//     stored under a DOTTED base name, reported as the three synthetic
//     `_bucket` / `_count` / `_sum` companions. This is the composition the
//     retired helper's docstring called load-bearing (the
//     `iterate-metrics-explorer` label-chip failure on
//     `http_server_request_body_size`), and the exact case a naive deletion
//     would have regressed. It survives because the lowering strips the
//     companion suffix BEFORE expanding the base name, so the candidate set
//     is applied to the base either way.
func TestSeriesStorageSpellings_UnchangedByFanoutRetirement(t *testing.T) {
	srv := storageSpellingServer(t)

	got := seriesSpellings(t, srv, `{__name__="`+promGrammarName+`"}`)

	want := []string{
		// Gauge rows: one per resolvable stored spelling, all reported under
		// the single Prom-grammar wire name.
		"http.server.request.body.size|http_server_request_body_size",
		"http.server.request_body_size|http_server_request_body_size",
		"http.server/request/body_size|http_server_request_body_size",
		"http_server_request_body_size|http_server_request_body_size",
		// Histogram rows: each resolvable stored spelling surfaces its three
		// classic-histogram companions.
		"http.server.request.body.size|http_server_request_body_size_bucket",
		"http.server.request.body.size|http_server_request_body_size_count",
		"http.server.request.body.size|http_server_request_body_size_sum",
		"http.server.request_body_size|http_server_request_body_size_bucket",
		"http.server.request_body_size|http_server_request_body_size_count",
		"http.server.request_body_size|http_server_request_body_size_sum",
		"http.server/request/body_size|http_server_request_body_size_bucket",
		"http.server/request/body_size|http_server_request_body_size_count",
		"http.server/request/body_size|http_server_request_body_size_sum",
		"http_server_request_body_size|http_server_request_body_size_bucket",
		"http_server_request_body_size|http_server_request_body_size_count",
		"http_server_request_body_size|http_server_request_body_size_sum",
	}
	sort.Strings(want)

	if diff := setDiff(want, got); diff != "" {
		t.Fatalf("/series no longer reports the same stored rows for %q:\n%s\n"+
			"This set was captured from the two-layer handler. A MISSING entry means the "+
			"predicate-level `__name__` fan-out does not in fact subsume the retired "+
			"matcher-string fan-out and the retirement lost coverage; an EXTRA entry means "+
			"the metadata surface started advertising a row it did not before.",
			promGrammarName, diff)
	}
}

// TestSeriesStorageSpellings_MatchQueryPath is the invariant that makes the
// retirement correct rather than merely equivalent-enough: the set of stored
// rows /series advertises for a name must be exactly the set an actual PromQL
// query for that name can return. A metadata surface that resolves MORE
// spellings than the query path advertises series that render empty in
// Grafana.
//
// The comparison is restricted to the gauge rows because that is where the
// two surfaces are directly comparable — /series reports a classic histogram
// as its three synthetic companions, while an instant query for the bare base
// name selects the gauge table (the companions are queried under their own
// suffixed names).
func TestSeriesStorageSpellings_MatchQueryPath(t *testing.T) {
	srv := storageSpellingServer(t)

	series := gaugeOnly(seriesSpellings(t, srv, `{__name__="`+promGrammarName+`"}`))
	query := gaugeOnly(querySpellings(t, srv, `{__name__="`+promGrammarName+`"}`))

	if diff := setDiff(series, query); diff != "" {
		t.Fatalf("/series and /query disagree on which stored spellings of %q resolve:\n%s\n"+
			"Whichever side is wider is resolving storage spellings the other cannot: a wider "+
			"/series advertises series that query empty; a wider /query means the metadata "+
			"surface hides series that are queryable.", promGrammarName, diff)
	}
	if len(series) == 0 {
		t.Fatal("both surfaces returned nothing — the seed or the window is wrong and the " +
			"agreement above is vacuous")
	}
}

// TestSeriesStorageSpellings_InterleavedSeparatorMatchesQueryPath pins the one
// spelling whose treatment the retirement deliberately changed.
//
// `http.server/request.body.size` mixes separators non-contiguously. It is not
// in format.PromLabelToOTelCandidates' output for the Prom-grammar name — that
// function emits the dot powerset and the contiguous dot/slash/underscore zone
// forms, and documents arbitrary interleavings as out of scope. The retired
// two-layer composition nonetheless reached it: the outer layer produced the
// zone arm `http.server/request_body_size`, and the predicate-level fan-out
// then re-expanded THAT arm's remaining underscores, landing on the
// interleaving. Neither layer reaches it alone.
//
// The PromQL query path has only ever had the single layer, so it never
// matched this row — /series was advertising a series no query could fetch.
// The test pins the corrected state: both surfaces agree the row is not
// resolvable. If interleaved storage spellings are ever to be supported, the
// fix belongs in PromLabelToOTelCandidates, where BOTH surfaces would pick it
// up — not in a metadata-only string expansion.
func TestSeriesStorageSpellings_InterleavedSeparatorMatchesQueryPath(t *testing.T) {
	srv := storageSpellingServer(t)

	series := seriesSpellings(t, srv, `{__name__="`+promGrammarName+`"}`)
	query := querySpellings(t, srv, `{__name__="`+promGrammarName+`"}`)

	for _, surface := range []struct {
		name string
		got  []string
	}{{"/series", series}, {"/query", query}} {
		for _, entry := range surface.got {
			if strings.HasPrefix(entry, interleavedSpelling+"|") {
				t.Errorf("%s resolved the interleaved storage spelling %q for %q. Only one "+
					"surface can gain this, and only via PromLabelToOTelCandidates; a "+
					"metadata-only expansion that reaches it re-creates the /series-wider-"+
					"than-/query divergence #1528 removed.",
					surface.name, interleavedSpelling, promGrammarName)
			}
		}
	}

	// Guard the guard: the row must actually be seeded, or the assertion above
	// passes for the wrong reason.
	var seeded bool
	for _, n := range storageSpellings {
		if n == interleavedSpelling {
			seeded = true
		}
	}
	if !seeded {
		t.Fatalf("%q is not in the seeded corpus — the assertion above is vacuous",
			interleavedSpelling)
	}
}

// setDiff renders the symmetric difference of two sorted string sets, or ""
// when they are equal.
func setDiff(want, got []string) string {
	inWant := make(map[string]struct{}, len(want))
	for _, w := range want {
		inWant[w] = struct{}{}
	}
	inGot := make(map[string]struct{}, len(got))
	for _, g := range got {
		inGot[g] = struct{}{}
	}
	var b strings.Builder
	for _, w := range want {
		if _, ok := inGot[w]; !ok {
			fmt.Fprintf(&b, "  MISSING: %s\n", w)
		}
	}
	for _, g := range got {
		if _, ok := inWant[g]; !ok {
			fmt.Fprintf(&b, "  EXTRA:   %s\n", g)
		}
	}
	return b.String()
}

// gaugeOnly keeps the entries whose wire `__name__` is the bare Prom-grammar
// name — i.e. the gauge-table rows, dropping the synthetic histogram
// companions.
func gaugeOnly(entries []string) []string {
	out := make([]string, 0, len(entries))
	for _, e := range entries {
		if strings.HasSuffix(e, "|"+promGrammarName) {
			out = append(out, e)
		}
	}
	return out
}
