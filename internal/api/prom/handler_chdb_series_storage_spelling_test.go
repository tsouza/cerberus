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

// capBoundaryBase has exactly maxRewritableUnderscores (6) internal
// underscores, which puts its classic-histogram / counter COMPANION names one
// over the cap. That boundary is the second place the two layers differed, and
// the reason the retirement's justification is about composition rather than
// about interleaved separators alone:
//
//   - The bare base is AT the cap, so
//     [format.PromLabelToOTelCandidates] still enumerates its full dot powerset
//     and zone forms — every stored spelling of the base resolves.
//   - A companion name (`<base>_count`) has 7, so the generator falls back to
//     `{self, all-dots, all-slashes}` and drops the powerset and the zone
//     enumeration entirely.
//
// The companion arm that scans the sum/gauge table under the LITERAL suffixed
// name (internal/promql.buildLiteralNameCompanionArm) therefore cannot reach a
// stored `<dotted base>_count`, while the retired layer reached it by dotting
// the base FIRST and appending the suffix to each candidate. The histogram arms
// are unaffected — they strip the suffix before expanding, so they expand the
// base and stay under the cap.
//
// As with the interleaved spelling, the PromQL query path never reached these
// either, so the surfaces now agree. The family below pins BOTH halves: the
// base spellings resolve on both surfaces, and the over-cap companion spelling
// resolves on neither.
const capBoundaryBase = "deep_a_b_c_d_e_f"

// capBoundaryGaugeSpellings are stored spellings of capBoundaryBase itself —
// all reachable, because the base sits AT the cap.
var capBoundaryGaugeSpellings = []string{
	"deep_a_b_c_d_e_f",
	"deep.a.b.c.d.e.f",
	"deep.a.b.c.d.e_f",
}

// capBoundarySumSpellings are stored spellings of the `_count` COMPANION. The
// underscored one is reachable (it is the literal the companion arm binds);
// the dotted one is over the cap and is not.
var capBoundarySumSpellings = []string{
	"deep_a_b_c_d_e_f_count",
	"deep.a.b.c.d.e.f_count",
}

// overCapCompanionSpelling is the spelling this change stopped reaching, and
// which the query path never reached.
const overCapCompanionSpelling = "deep.a.b.c.d.e.f_count"

// The probe seeds and asserts on the CONTENTS of its tables, so it must not
// share table names with any other test in this package. chDB sessions are
// per-Client, but several chdb-tagged tests here seed the default OTel table
// names, and `CREATE OR REPLACE TABLE` from one of them running concurrently
// would silently empty this probe's rows — the assertions would then compare
// two empty sets and agree for the wrong reason. Pinning probe-private table
// names through the schema makes the probe independent of test ordering and
// parallelism.
const (
	probeGaugeTable     = "probe_spelling_gauge"
	probeSumTable       = "probe_spelling_sum"
	probeHistogramTable = "probe_spelling_histogram"
)

// probeSeedAgeSeconds backdates every seeded row. The instant-query surface
// takes its evaluation instant from the `time` parameter, which carries
// whole-second precision, while `now64(9)` writes a sub-second timestamp: a row
// inserted at T.83 is strictly AFTER an evaluation instant of T, so the
// `TimeUnix <= eval` half of the staleness window drops it. Seeding at
// now64(9) therefore makes the result depend on where in the wall-clock second
// the seed landed — 5 rows, some rows, or none. Backdating well clear of that
// boundary (and well inside the 5m staleness lookback, so every row is still
// the live sample for its series) makes the probe deterministic.
const probeSeedAgeSeconds = 30

func storageSpellingSchema() schema.Metrics {
	s := schema.DefaultOTelMetrics()
	s.GaugeTable = probeGaugeTable
	s.SumTable = probeSumTable
	s.HistogramTable = probeHistogramTable
	return s
}

func storageSpellingSeed() string {
	var b strings.Builder
	b.WriteString(`CREATE OR REPLACE TABLE ` + probeGaugeTable + ` (
	  ServiceName String, MetricName String, Attributes Map(String,String),
	  ResourceAttributes Map(String,String) DEFAULT map(),
	  TimeUnix DateTime64(9), Value Float64
	) ENGINE = MergeTree() ORDER BY (ServiceName, MetricName, toUnixTimestamp64Nano(TimeUnix));`)
	b.WriteString(`CREATE OR REPLACE TABLE ` + probeSumTable + ` (
	  ServiceName String, MetricName String, Attributes Map(String,String),
	  ResourceAttributes Map(String,String) DEFAULT map(),
	  TimeUnix DateTime64(9), Value Float64
	) ENGINE = MergeTree() ORDER BY (ServiceName, MetricName, toUnixTimestamp64Nano(TimeUnix));`)
	b.WriteString(`CREATE OR REPLACE TABLE ` + probeHistogramTable + ` (
	  ServiceName String, MetricName String, Attributes Map(String,String),
	  ResourceAttributes Map(String,String) DEFAULT map(),
	  TimeUnix DateTime64(9), Count UInt64, Sum Float64,
	  BucketCounts Array(UInt64), ExplicitBounds Array(Float64)
	) ENGINE = MergeTree() ORDER BY (ServiceName, MetricName, toUnixTimestamp64Nano(TimeUnix));`)
	for _, n := range storageSpellings {
		fmt.Fprintf(
			&b,
			`INSERT INTO `+probeGaugeTable+` (ServiceName, MetricName, Attributes, TimeUnix, Value) `+
				`VALUES ('svc','%s', map('%s','%s'), now64(9) - INTERVAL %d SECOND, 1);`,
			n, storageSpellingProbeLabel, n, probeSeedAgeSeconds,
		)
		fmt.Fprintf(
			&b,
			`INSERT INTO `+probeHistogramTable+` (ServiceName, MetricName, Attributes, TimeUnix, `+
				`Count, Sum, BucketCounts, ExplicitBounds) `+
				`VALUES ('svc','%s', map('%s','%s'), now64(9) - INTERVAL %d SECOND, 10, 5.0, [1,2,3],[0.1,0.5]);`,
			n, storageSpellingProbeLabel, n, probeSeedAgeSeconds,
		)
	}
	for _, n := range capBoundaryGaugeSpellings {
		fmt.Fprintf(
			&b,
			`INSERT INTO `+probeGaugeTable+` (ServiceName, MetricName, Attributes, TimeUnix, Value) `+
				`VALUES ('svc','%s', map('%s','%s'), now64(9) - INTERVAL %d SECOND, 1);`,
			n, storageSpellingProbeLabel, n, probeSeedAgeSeconds,
		)
	}
	for _, n := range capBoundarySumSpellings {
		fmt.Fprintf(
			&b,
			`INSERT INTO `+probeSumTable+` (ServiceName, MetricName, Attributes, TimeUnix, Value) `+
				`VALUES ('svc','%s', map('%s','%s'), now64(9) - INTERVAL %d SECOND, 1);`,
			n, storageSpellingProbeLabel, n, probeSeedAgeSeconds,
		)
	}
	return b.String()
}

func storageSpellingServer(t *testing.T) *httptest.Server {
	t.Helper()
	client := chclienttest.NewChDB(t)
	client.Seed(t, storageSpellingSeed())
	h := prom.New(client, storageSpellingSchema(), nil)
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

// TestSeriesStorageSpellings_CapBoundaryCompanionMatchesQueryPath pins the
// second place the retired composition reached further than the surviving
// predicate: a stored COMPANION name whose Prom-grammar spelling sits one
// underscore over maxRewritableUnderscores. See capBoundaryBase for the
// mechanism.
//
// Both halves are asserted, because only the pair is meaningful. If just the
// negative half were pinned, an accidental narrowing of the base expansion
// would pass; if just the positive half were, the divergence this documents
// would go unrecorded.
func TestSeriesStorageSpellings_CapBoundaryCompanionMatchesQueryPath(t *testing.T) {
	srv := storageSpellingServer(t)

	// Positive half: the base is AT the cap, so every stored spelling of it
	// resolves, and both surfaces agree on the set.
	baseSeries := spellingsOnly(seriesSpellings(t, srv, `{__name__="`+capBoundaryBase+`"}`), capBoundaryGaugeSpellings)
	baseQuery := spellingsOnly(querySpellings(t, srv, `{__name__="`+capBoundaryBase+`"}`), capBoundaryGaugeSpellings)
	if diff := setDiff(baseSeries, baseQuery); diff != "" {
		t.Fatalf("/series and /query disagree on the stored spellings of the at-cap base %q:\n%s",
			capBoundaryBase, diff)
	}
	if len(baseSeries) != len(capBoundaryGaugeSpellings) {
		t.Fatalf("the at-cap base %q resolved %d of its %d seeded spellings (%v) — the base "+
			"expansion is meant to be unaffected by the cap, since 6 underscores is exactly "+
			"maxRewritableUnderscores; a short count means the candidate powerset degenerated "+
			"one name too early", capBoundaryBase, len(baseSeries), len(capBoundaryGaugeSpellings), baseSeries)
	}

	// Negative half: the over-cap companion spelling resolves on NEITHER
	// surface.
	//
	// The two surfaces must be probed with DIFFERENT matchers, because the
	// divergence only ever appeared on one of them. The retired layer
	// short-circuited on an input name that already carried a companion suffix
	// (its equalNameMatcherValue skipped `_bucket` / `_count` / `_sum` /
	// `_total`), so `match[]={__name__="<base>_count"}` never fanned out. What
	// it DID fan out was the BARE base: it dotted the base first, and the
	// companion layer then appended `_count` to each dotted candidate,
	// producing a literal-name arm that matched the stored dotted companion
	// row. So the /series half must use the bare base to reproduce it.
	//
	// On the query side the reachable spelling of that row is its Prom-grammar
	// wire name — `<base>_count` — which is what /series would advertise, and
	// which is the matcher a Grafana panel built from that listing sends back.
	companionMatcher := `{__name__="` + capBoundaryBase + `_count"}`
	for _, surface := range []struct {
		name string
		got  []string
	}{
		{"/series", seriesSpellings(t, srv, `{__name__="`+capBoundaryBase+`"}`)},
		{"/query", querySpellings(t, srv, companionMatcher)},
	} {
		for _, entry := range surface.got {
			if strings.HasPrefix(entry, overCapCompanionSpelling+"|") {
				t.Errorf("%s resolved the over-cap companion spelling %q. Both surfaces must "+
					"agree here: /series advertising it while a query for the name it advertises "+
					"cannot fetch it is exactly the divergence #1528 removed. If this row is to "+
					"be reachable, the fix is in format.PromLabelToOTelCandidates (raise or "+
					"remove maxRewritableUnderscores), which BOTH surfaces read — not a "+
					"metadata-only string expansion.",
					surface.name, overCapCompanionSpelling)
			}
		}
	}

	// The under-cap companion spelling DOES resolve — without this the
	// negative half above could pass because the companion arm stopped
	// working altogether.
	underCap := spellingsOnly(seriesSpellings(t, srv, companionMatcher), []string{capBoundaryBase + "_count"})
	if len(underCap) == 0 {
		t.Fatalf("/series resolved none of the seeded `%s_count` rows — the companion arm is "+
			"not running at all, so the over-cap assertion above proves nothing",
			capBoundaryBase)
	}

	var seeded bool
	for _, n := range capBoundarySumSpellings {
		if n == overCapCompanionSpelling {
			seeded = true
		}
	}
	if !seeded {
		t.Fatalf("%q is not in the seeded corpus — the assertion above is vacuous",
			overCapCompanionSpelling)
	}
}

// TestLabelValuesStorageSpellings_MatchSeries extends the value-level proof to
// the third matched metadata surface. /series, /labels and
// /label/<name>/values all fan out through the same
// Handler.expandMetadataMatchers, so retiring a layer there changes all three;
// pinning only /series would leave the other two asserted by construction
// rather than by measurement.
//
// The probe label carries the stored spelling, so asking for that label's
// VALUES under the same matcher yields exactly the set of stored rows the
// surface resolved — directly comparable with /series.
func TestLabelValuesStorageSpellings_MatchSeries(t *testing.T) {
	srv := storageSpellingServer(t)

	matcher := `{__name__="` + promGrammarName + `"}`
	fromSeries := map[string]struct{}{}
	for _, entry := range seriesSpellings(t, srv, matcher) {
		fromSeries[strings.SplitN(entry, "|", 2)[0]] = struct{}{}
	}

	reqURL := fmt.Sprintf("%s/api/v1/label/%s/values?match[]=%s&start=%d&end=%d",
		srv.URL, storageSpellingProbeLabel, url.QueryEscape(matcher),
		time.Now().Add(-time.Hour).Unix(), time.Now().Add(time.Hour).Unix())
	resp, err := http.Get(reqURL)
	if err != nil {
		t.Fatalf("GET /label/values: %v", err)
	}
	defer resp.Body.Close()
	var body struct {
		Status string   `json:"status"`
		Error  string   `json:"error"`
		Data   []string `json:"data"`
	}
	if err := json.NewDecoder(resp.Body).Decode(&body); err != nil {
		t.Fatalf("decode /label/values: %v", err)
	}
	if resp.StatusCode != http.StatusOK || body.Status != "success" {
		t.Fatalf("/label/values status=%d body.status=%s err=%s", resp.StatusCode, body.Status, body.Error)
	}

	fromValues := map[string]struct{}{}
	for _, v := range body.Data {
		fromValues[v] = struct{}{}
	}
	if len(fromValues) == 0 {
		t.Fatal("/label/values returned nothing for the seeded probe label — the comparison " +
			"below would agree vacuously")
	}

	var missing, extra []string
	for s := range fromSeries {
		if _, ok := fromValues[s]; !ok {
			missing = append(missing, s)
		}
	}
	for s := range fromValues {
		if _, ok := fromSeries[s]; !ok {
			extra = append(extra, s)
		}
	}
	sort.Strings(missing)
	sort.Strings(extra)
	if len(missing) > 0 || len(extra) > 0 {
		t.Fatalf("/label/%s/values and /series disagree on which stored spellings of %q "+
			"resolve:\n  only in /series: %v\n  only in /label/values: %v\n"+
			"Both fan out through the same Handler.expandMetadataMatchers and lower through the "+
			"same predicate, so a divergence means one surface grew or lost a resolution path "+
			"the other did not.", storageSpellingProbeLabel, promGrammarName, missing, extra)
	}
}

// spellingsOnly keeps the entries whose stored spelling is in `want`.
func spellingsOnly(entries, want []string) []string {
	in := make(map[string]struct{}, len(want))
	for _, w := range want {
		in[w] = struct{}{}
	}
	seen := map[string]struct{}{}
	out := make([]string, 0, len(entries))
	for _, e := range entries {
		spelling := strings.SplitN(e, "|", 2)[0]
		if _, ok := in[spelling]; !ok {
			continue
		}
		if _, dup := seen[spelling]; dup {
			continue
		}
		seen[spelling] = struct{}{}
		out = append(out, spelling)
	}
	sort.Strings(out)
	return out
}
