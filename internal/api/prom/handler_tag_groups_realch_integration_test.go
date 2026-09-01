//go:build integration

// handler_tag_groups_realch_integration_test.go — real-ClickHouse
// differential-parity verification for chopt.FeatureTSGridTagGroups
// (cerberus issue #2750): the instant-mode duplicate-labelset guard's new
// UInt64-group-id path (guardNameDropCollisionByTagGroup, internal/promql/
// duplicate_labelset_guard.go) must answer BYTE-IDENTICAL to the existing
// Map-grouped path it replaces, for every shape the guard covers —
// distinct-labelsets pass-through, the collision reject, and the
// pinned-name no-guard case — because the whole point of this feature is a
// grouping-KEY swap with no observable behaviour change (chopt.
// FeatureTSGridTagGroups's own doc).
//
// Unlike handler_route_b_realch_integration_test.go's route-A/route-B
// comparison (a genuinely different execution strategy expected to answer
// the same result), this is a stricter bar: TagGroups=false and
// TagGroups=true must be TWO DIFFERENT SQL SHAPES answering IDENTICALLY,
// proven against a real server rather than assumed from the two shapes
// "looking equivalent" on paper — the two ClickHouse-analyzer rejections
// this feature's own registry doc records (UNKNOWN_IDENTIFIER,
// ILLEGAL_AGGREGATION) are exactly the kind of divergence that looks fine
// until it is run.
//
// Needs a real ClickHouse >= 26.2 — chopt.FeatureTSGridTagGroups's own
// floor (the higher of timeSeriesTagsToGroup/timeSeriesGroupToTags's 26.1
// and timeSeriesThrowDuplicateSeriesIf's 26.2, even though this feature does
// not yet wire the latter into the guard's throw — see that registry
// entry's own doc). Requires Docker; gated behind the `integration` build
// tag, joining the established real-CH lane family
// (handler_route_b_realch_integration_test.go and siblings) — run locally
// with:
//
//	go test -tags=integration -count=1 -run TestTagGroups ./internal/api/prom/...
package prom_test

import (
	"context"
	"encoding/json"
	"fmt"
	"net/http"
	"net/http/httptest"
	"net/url"
	"sort"
	"testing"
	"time"

	tcclickhouse "github.com/testcontainers/testcontainers-go/modules/clickhouse"

	"github.com/tsouza/cerberus/internal/api/prom"
	"github.com/tsouza/cerberus/internal/chclient"
	"github.com/tsouza/cerberus/internal/schema"
)

// tagGroupsCHImage pins ClickHouse exactly at chopt.FeatureTSGridTagGroups's
// own floor, mirroring tsGridGroupArrayImage / routeBCHImage's own
// per-feature pinning rationale — this test's only dependency on the
// feature is the version its own testcontainers image is pinned to.
const tagGroupsCHImage = "clickhouse/clickhouse-server:26.2-alpine"

// tagGroupsGaugeDDL mirrors handler_native_lowerers_integration_test.go's
// nativeLowererGaugeDDL (unavailable here — build-tag-disjoint from this
// file), the full OTel-exporter gauge-table shape schema.DefaultOTelMetrics
// expects: a bare-minimum table (handler_chdb_test.go's chdb-only gaugeDDL)
// is missing ResourceAttributes/ServiceName, which a real ClickHouse's
// merge() table function across every otel_metrics_* table rejects with
// UNKNOWN_IDENTIFIER the moment a query selects them — chDB is lenient
// about the mismatch (see handler_route_b_realch_integration_test.go's own
// header comment on chDB's leniency), a real server is not.
const tagGroupsGaugeDDL = `CREATE TABLE otel_metrics_gauge (
    ResourceAttributes Map(LowCardinality(String), String),
    ServiceName LowCardinality(String),
    MetricName String,
    MetricDescription String,
    MetricUnit String,
    Attributes Map(LowCardinality(String), String),
    StartTimeUnix DateTime64(9),
    TimeUnix DateTime64(9),
    Value Float64,
    Flags UInt32
) ENGINE = MergeTree() ORDER BY (ServiceName, MetricName, TimeUnix);`

// tagGroupsOtherTablesDDL creates the remaining four schema.DefaultOTelMetrics
// tables (sum, histogram, exponential_histogram, summary) EMPTY — an
// unpinned / regex `__name__` matcher (the only way to reach
// collidesOnNameDrop, and therefore this test's whole subject) fans out
// into one arm per configured metric table regardless of which table the
// matched name actually lives in, so all five must exist for the query to
// resolve at all, even though this test only ever seeds rows into
// otel_metrics_gauge. Column shapes mirror the upstream OTel-CH exporter
// templates (handler_chdb_resource_attrs_test.go's chdb-only
// resourceAttrHistogramDDL / resourceAttrExpHistogramDDL carry the same
// histogram / exp-histogram shapes; unavailable here for the same
// build-tag-disjoint reason as tagGroupsGaugeDDL above). One statement per
// slice entry — client.Exec runs one native-protocol statement per call, so
// this cannot be a single multi-statement string the way a single CREATE
// TABLE can.
var tagGroupsOtherTablesDDL = []string{
	`CREATE TABLE otel_metrics_sum (
    ResourceAttributes Map(LowCardinality(String), String),
    ServiceName LowCardinality(String),
    MetricName String,
    Attributes Map(LowCardinality(String), String),
    StartTimeUnix DateTime64(9),
    TimeUnix DateTime64(9),
    Value Float64,
    Flags UInt32,
    AggregationTemporality Int32,
    IsMonotonic Boolean
) ENGINE = MergeTree() ORDER BY (ServiceName, MetricName, TimeUnix);`,
	`CREATE TABLE otel_metrics_histogram (
    ResourceAttributes Map(LowCardinality(String), String),
    ServiceName LowCardinality(String),
    MetricName String,
    Attributes Map(LowCardinality(String), String),
    StartTimeUnix DateTime64(9),
    TimeUnix DateTime64(9),
    Count UInt64,
    Sum Float64,
    BucketCounts Array(UInt64),
    ExplicitBounds Array(Float64),
    Min Float64,
    Max Float64,
    Flags UInt32,
    AggregationTemporality Int32
) ENGINE = MergeTree() ORDER BY (ServiceName, MetricName, TimeUnix);`,
	`CREATE TABLE otel_metrics_exponential_histogram (
    ResourceAttributes Map(LowCardinality(String), String),
    ServiceName LowCardinality(String),
    MetricName String,
    Attributes Map(LowCardinality(String), String),
    StartTimeUnix DateTime64(9),
    TimeUnix DateTime64(9),
    Count UInt64,
    Sum Float64,
    Scale Int32,
    ZeroCount UInt64,
    PositiveOffset Int32,
    PositiveBucketCounts Array(UInt64),
    NegativeOffset Int32,
    NegativeBucketCounts Array(UInt64),
    Min Float64,
    Max Float64,
    Flags UInt32,
    AggregationTemporality Int32
) ENGINE = MergeTree() ORDER BY (ServiceName, MetricName, TimeUnix);`,
	`CREATE TABLE otel_metrics_summary (
    ResourceAttributes Map(LowCardinality(String), String),
    ServiceName LowCardinality(String),
    MetricName String,
    Attributes Map(LowCardinality(String), String),
    StartTimeUnix DateTime64(9),
    TimeUnix DateTime64(9),
    Count UInt64,
    Sum Float64,
    ValueAtQuantiles Array(Tuple(Float64, Float64)),
    Flags UInt32
) ENGINE = MergeTree() ORDER BY (ServiceName, MetricName, TimeUnix);`,
}

// newTagGroupsHandler wires a *httptest.Server backed by client with
// TagGroups set as requested, sharing the SAME real ClickHouse connection
// (and therefore the same seeded data) the TagGroups=false / TagGroups=true
// comparison runs over.
func newTagGroupsHandler(t *testing.T, client *chclient.Client, tagGroups bool) *httptest.Server {
	t.Helper()
	h := prom.New(client, schema.DefaultOTelMetrics(), nil)
	h.TagGroups = tagGroups
	mux := http.NewServeMux()
	h.Mount(mux)
	srv := httptest.NewServer(mux)
	t.Cleanup(srv.Close)
	return srv
}

// tagGroupsInstantQuery issues an /api/v1/query and returns status + body.
func tagGroupsInstantQuery(t *testing.T, srvURL, query string, at time.Time) (int, string) {
	t.Helper()
	resp, err := http.Get(fmt.Sprintf("%s/api/v1/query?query=%s&time=%d",
		srvURL, url.QueryEscape(query), at.Unix()))
	if err != nil {
		t.Fatalf("GET %s: %v", query, err)
	}
	return resp.StatusCode, readBody(t, resp)
}

func TestTagGroups_MatchesMapGrouping_RealClickHouse(t *testing.T) {
	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Minute)
	defer cancel()

	t.Setenv("TESTCONTAINERS_RYUK_DISABLED", "true")

	container, err := tcclickhouse.Run(
		ctx,
		tagGroupsCHImage,
		tcclickhouse.WithUsername("cerberus"),
		tcclickhouse.WithPassword("cerberus"),
		tcclickhouse.WithDatabase("otel"),
	)
	if err != nil {
		t.Fatalf("start clickhouse: %v", err)
	}
	t.Cleanup(func() { _ = container.Terminate(ctx) })

	host, err := container.Host(ctx)
	if err != nil {
		t.Fatalf("host: %v", err)
	}
	port, err := container.MappedPort(ctx, "9000/tcp")
	if err != nil {
		t.Fatalf("port: %v", err)
	}

	client, err := chclient.New(chclient.Config{
		Addr:     host + ":" + port.Port(),
		Database: "otel",
		Username: "cerberus",
		Password: "cerberus",
	})
	if err != nil {
		t.Fatalf("connect: %v", err)
	}
	t.Cleanup(func() { _ = client.Close() })

	if err := client.Exec(ctx, tagGroupsGaugeDDL); err != nil {
		t.Fatalf("create otel_metrics_gauge: %v", err)
	}
	for _, ddl := range tagGroupsOtherTablesDDL {
		if err := client.Exec(ctx, ddl); err != nil {
			t.Fatalf("create table: %v\nddl=%s", err, ddl)
		}
	}

	// One eval instant; both series' latest sample sits inside the 5m LWR
	// staleness window the bare-selector seam applies.
	evalAt := time.Now().UTC().Truncate(time.Second)
	sampleAt := evalAt.Add(-1 * time.Minute)
	tsStr := sampleAt.Format("2006-01-02 15:04:05.000000000")

	// Two metric NAMES sharing one attribute set: dropping __name__ under
	// `ceil({__name__=~...})` maps both onto the identical label set
	// {host="a"} — the collision the guard must reject on both paths.
	// A third, third-name series with a DISTINCT attribute set proves the
	// distinct-labelset pass-through half at the same time (one seed, two
	// query shapes below, less setup to keep in parity between two servers
	// there are none of here — same server, same data, only the handler's
	// TagGroups flag differs).
	seed := fmt.Sprintf(`INSERT INTO otel_metrics_gauge (MetricName, Attributes, TimeUnix, Value) VALUES
    ('collide_a', map('host', 'x'), toDateTime64('%[1]s', 9), 10.0),
    ('collide_b', map('host', 'x'), toDateTime64('%[1]s', 9), 20.0),
    ('distinct_a', map('host', 'p'), toDateTime64('%[1]s', 9), 30.0),
    ('distinct_b', map('host', 'q'), toDateTime64('%[1]s', 9), 40.0),
    ('pinned_metric', map('host', 'r', 'env', 'prod'), toDateTime64('%[1]s', 9), 50.0);`, tsStr)
	if err := client.Exec(ctx, seed); err != nil {
		t.Fatalf("seed: %v", err)
	}

	srvOld := newTagGroupsHandler(t, client, false)
	srvNew := newTagGroupsHandler(t, client, true)

	for _, tc := range []struct {
		name       string
		query      string
		wantStatus int
	}{
		{"collision_rejected", `ceil({__name__=~"collide_a|collide_b"})`, http.StatusUnprocessableEntity},
		{"distinct_labelsets_pass_through", `ceil({__name__=~"distinct_a|distinct_b"})`, http.StatusOK},
		{"pinned_name_no_guard", `ceil(pinned_metric)`, http.StatusOK},
		{"unary_minus", `-{__name__=~"distinct_a|distinct_b"}`, http.StatusOK},
		{"scalar_times_vector", `2 * {__name__=~"distinct_a|distinct_b"}`, http.StatusOK},
	} {
		t.Run(tc.name, func(t *testing.T) {
			statusOld, bodyOld := tagGroupsInstantQuery(t, srvOld.URL, tc.query, evalAt)
			statusNew, bodyNew := tagGroupsInstantQuery(t, srvNew.URL, tc.query, evalAt)

			if statusOld != tc.wantStatus {
				t.Fatalf("TagGroups=false: status=%d, want %d; body=%s", statusOld, tc.wantStatus, bodyOld)
			}
			if statusNew != tc.wantStatus {
				t.Fatalf("TagGroups=true: status=%d, want %d; body=%s", statusNew, tc.wantStatus, bodyNew)
			}

			normOld, err := normalizeQueryBody(bodyOld)
			if err != nil {
				t.Fatalf("normalize TagGroups=false body: %v; body=%s", err, bodyOld)
			}
			normNew, err := normalizeQueryBody(bodyNew)
			if err != nil {
				t.Fatalf("normalize TagGroups=true body: %v; body=%s", err, bodyNew)
			}
			if normOld != normNew {
				t.Fatalf("%s: TagGroups=false and TagGroups=true diverged:\nfalse=%s\ntrue=%s", tc.query, normOld, normNew)
			}
		})
	}
}

// normalizeQueryBody re-marshals an /api/v1/query response through its
// decoded Go shape (queryResponse, from handler_test.go — untagged, shared
// across every build-tag variant of this package) so a byte-for-byte
// comparison is not sensitive to incidental key-order differences the two
// handlers' independent JSON encodings could otherwise introduce, while
// still catching any REAL divergence in status, error, series set, labels,
// or values.
//
// A vector Result's own SERIES order is additionally normalized by
// sortVectorResult. Neither guardNameDropCollision's Map-grouped Aggregate
// nor guardNameDropCollisionByTagGroup's UInt64-id-grouped one carries an
// ORDER BY — ClickHouse's GROUP BY makes no row-order promise for either
// shape, and switching the grouping key changes which order it happens to
// pick, exactly like handler_route_b_realch_integration_test.go's own
// route-A/route-B comparison already has to tolerate (its byLabels map
// exists for the identical reason). Series order was never part of what
// this feature's "no observable behaviour change" doc comment promises —
// only the series SET and each series' own labels/value are.
func normalizeQueryBody(body string) (string, error) {
	var parsed queryResponse
	if err := json.Unmarshal([]byte(body), &parsed); err != nil {
		return "", err
	}
	if parsed.Data.ResultType == "vector" {
		sorted, err := sortVectorResult(parsed.Data.Result)
		if err != nil {
			return "", err
		}
		parsed.Data.Result = sorted
	}
	out, err := json.Marshal(parsed)
	if err != nil {
		return "", err
	}
	return string(out), nil
}

// sortVectorResult decodes a vector Result (decoded generically by
// queryResponse as `any`) into its typed []prom.VectorSample shape and
// sorts it by each series' marshaled label set. encoding/json canonically
// sorts a Go map's keys when marshaling, so two series sharing a label set
// marshal identically regardless of which grouping key produced them; the
// sort key is otherwise unique because the guard under test has already
// rejected any query where two series would share one label set.
func sortVectorResult(result any) ([]prom.VectorSample, error) {
	raw, err := json.Marshal(result)
	if err != nil {
		return nil, err
	}
	var vec []prom.VectorSample
	if err := json.Unmarshal(raw, &vec); err != nil {
		return nil, err
	}
	// keyed pairs a sample with its own sort key so sort.Slice's swaps move
	// both together — sorting `vec` directly against a same-indexed `keys`
	// slice would desync the two the moment the first swap happened.
	type keyed struct {
		key    string
		sample prom.VectorSample
	}
	pairs := make([]keyed, len(vec))
	for i, sample := range vec {
		key, err := json.Marshal(sample.Metric)
		if err != nil {
			return nil, err
		}
		pairs[i] = keyed{key: string(key), sample: sample}
	}
	sort.Slice(pairs, func(i, j int) bool { return pairs[i].key < pairs[j].key })
	for i, p := range pairs {
		vec[i] = p.sample
	}
	return vec, nil
}
