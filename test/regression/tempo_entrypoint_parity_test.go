// Cross-transport parity for the Tempo head. The same query, driven
// against the same stub ClickHouse client, must produce the same
// ANSWER over HTTP and over the gRPC StreamingQuerier — both the
// error classification and the search-metrics accounting.
//
// Both pins here are regressions with a real before/after. Before the
// entrypoint unification, four independent error classifiers lived in
// internal/api/tempo (two HTTP, two gRPC) and disagreed: a query that
// failed with context.Canceled came back HTTP 500 but gRPC Canceled,
// and SearchMetrics.InspectedTraces counted DRAINED SPAN ROWS on the
// HTTP path and DISTINCT TRACES on the gRPC path — the same seed gave
// two different numbers under one field name.
//
// The classification pin deliberately declares its own HTTP↔gRPC
// correspondence table rather than importing the production encoders.
// A parity test built on the encoders would only prove they agree with
// themselves.
package regression

import (
	"context"
	"encoding/json"
	"fmt"
	"io"
	"net"
	"net/http"
	"net/http/httptest"
	"strconv"
	"testing"
	"time"

	"github.com/grafana/tempo/pkg/tempopb"
	"google.golang.org/grpc"
	"google.golang.org/grpc/codes"
	"google.golang.org/grpc/credentials/insecure"
	"google.golang.org/grpc/status"
	"google.golang.org/grpc/test/bufconn"

	"github.com/tsouza/cerberus/internal/api/tempo"
	tempogrpc "github.com/tsouza/cerberus/internal/api/tempo/grpc"
	"github.com/tsouza/cerberus/internal/chclient"
	"github.com/tsouza/cerberus/internal/schema"
)

// tempoParityHTTPStatusFor is the HTTP status each gRPC code must pair
// with across the Tempo head. It is the contract this file enforces,
// written out independently of internal/api/tempo/errclass.go and
// internal/api/tempo/grpc/errclass.go so agreement between the two
// production encoders cannot by itself satisfy the test.
//
// 499 is nginx's "Client Closed Request" — the status cerberus already
// uses elsewhere for a cancelled query, deliberately outside the 5xx
// band so a client walking away is not counted as a server fault.
var tempoParityHTTPStatusFor = map[codes.Code]int{
	codes.Canceled:          499,
	codes.InvalidArgument:   http.StatusBadRequest,
	codes.ResourceExhausted: http.StatusUnprocessableEntity,
	codes.Unavailable:       http.StatusServiceUnavailable,
	codes.Internal:          http.StatusInternalServerError,
}

const (
	// Window bracketing tempoParitySeedAnchor. /api/search clamps a
	// windowless request to a now-relative lookback, so both transports
	// get an explicit window or the historical seed filters out.
	tempoParityWindowMargin = time.Hour

	// The seed: distinct traces × spans each. The two must differ, or
	// the accounting pin cannot tell a trace count from a span count.
	tempoParitySeedTraces      = 2
	tempoParitySeedSpansPerTrc = 3
)

func tempoParitySeedAnchor() time.Time {
	return time.Date(2026, 5, 1, 10, 0, 0, 0, time.UTC)
}

// tempoParityQuerier is the stub ClickHouse client both transports run
// against. Either it fails every query with err, or it returns the
// same fixed row set to both — which is what makes any difference in
// the response attributable to the entrypoint rather than the data.
type tempoParityQuerier struct {
	err  error
	rows []chclient.Sample
}

func (q *tempoParityQuerier) Query(context.Context, string, ...any) ([]chclient.Sample, error) {
	if q.err != nil {
		return nil, q.err
	}
	return append([]chclient.Sample(nil), q.rows...), nil
}

func (q *tempoParityQuerier) QueryStrings(context.Context, string, ...any) ([]string, error) {
	if q.err != nil {
		return nil, q.err
	}
	return nil, nil
}

// tempoParitySeedRows builds tempoParitySeedTraces traces of
// tempoParitySeedSpansPerTrc spans each, shaped like the canonical
// search wrap-projection output. Every span carries a distinct
// (SpanName, Timestamp) pair so the summary grouping never collapses
// two rows into one and the trace count is unambiguously the trace
// count.
func tempoParitySeedRows() []chclient.Sample {
	anchor := tempoParitySeedAnchor()
	rows := make([]chclient.Sample, 0, tempoParitySeedTraces*tempoParitySeedSpansPerTrc)
	for tr := 1; tr <= tempoParitySeedTraces; tr++ {
		traceID := fmt.Sprintf("%032x", tr)
		for sp := 0; sp < tempoParitySeedSpansPerTrc; sp++ {
			rows = append(rows, chclient.Sample{
				MetricName: fmt.Sprintf("span-%d-%d", tr, sp),
				Labels: map[string]string{
					"service.name":               "frontend",
					"__cerberus_traceID":         traceID,
					"__cerberus_parentSpanID":    "0",
					"__cerberus_traceDurationNs": "1000000",
				},
				Timestamp: anchor.Add(time.Duration(tr*10+sp) * time.Second),
				Value:     1_000_000,
			})
		}
	}
	return rows
}

func tempoParityHandler(t *testing.T, q *tempoParityQuerier) *tempo.Handler {
	t.Helper()
	return tempo.New(q, schema.DefaultOTelTraces(), "v-regression", nil)
}

// tempoParitySearchHTTP drives GET /api/search and returns the status
// plus the decoded envelope (nil envelope on a non-200).
func tempoParitySearchHTTP(t *testing.T, q *tempoParityQuerier) (int, *tempo.SearchResponse) {
	t.Helper()
	mux := http.NewServeMux()
	tempoParityHandler(t, q).Mount(mux)
	srv := httptest.NewServer(mux)
	t.Cleanup(srv.Close)

	anchor := tempoParitySeedAnchor()
	url := fmt.Sprintf("%s/api/search?q=%%7B%%7D&start=%d&end=%d",
		srv.URL, anchor.Add(-tempoParityWindowMargin).Unix(), anchor.Add(tempoParityWindowMargin).Unix())
	resp, err := http.Get(url) //nolint:noctx // httptest server, test-local lifetime
	if err != nil {
		t.Fatalf("GET /api/search: %v", err)
	}
	defer func() { _ = resp.Body.Close() }()
	body, err := io.ReadAll(resp.Body)
	if err != nil {
		t.Fatalf("read body: %v", err)
	}
	if resp.StatusCode != http.StatusOK {
		return resp.StatusCode, nil
	}
	var sr tempo.SearchResponse
	if err := json.Unmarshal(body, &sr); err != nil {
		t.Fatalf("decode body %q: %v", body, err)
	}
	return resp.StatusCode, &sr
}

// tempoParitySearchGRPC drives StreamingQuerier.Search over bufconn and
// returns the terminal status code plus the last frame's metrics.
func tempoParitySearchGRPC(t *testing.T, q *tempoParityQuerier) (codes.Code, *tempopb.SearchMetrics) {
	t.Helper()
	lis := bufconn.Listen(1 << 20)
	srv := tempogrpc.NewServer(tempogrpc.NewService(tempoParityHandler(t, q), nil, nil))
	go func() { _ = srv.Serve(lis) }()
	t.Cleanup(srv.Stop)

	conn, err := grpc.NewClient("passthrough:///bufnet",
		grpc.WithContextDialer(func(ctx context.Context, _ string) (net.Conn, error) {
			return lis.DialContext(ctx)
		}),
		grpc.WithTransportCredentials(insecure.NewCredentials()))
	if err != nil {
		t.Fatalf("dial bufconn: %v", err)
	}
	t.Cleanup(func() { _ = conn.Close() })

	ctx, cancel := context.WithTimeout(context.Background(), 30*time.Second)
	t.Cleanup(cancel)

	anchor := tempoParitySeedAnchor()
	stream, err := tempopb.NewStreamingQuerierClient(conn).Search(ctx, &tempopb.SearchRequest{
		Query: "{}",
		Start: uint32(anchor.Add(-tempoParityWindowMargin).Unix()), //nolint:gosec // fixed test anchor, no overflow
		End:   uint32(anchor.Add(tempoParityWindowMargin).Unix()),  //nolint:gosec // fixed test anchor, no overflow
	})
	if err != nil {
		t.Fatalf("open Search stream: %v", err)
	}

	var last *tempopb.SearchMetrics
	for {
		frame, rerr := stream.Recv()
		if rerr == io.EOF {
			return codes.OK, last
		}
		if rerr != nil {
			st, ok := status.FromError(rerr)
			if !ok {
				t.Fatalf("non-status stream error: %v", rerr)
			}
			return st.Code(), last
		}
		if frame.Metrics != nil {
			last = frame.Metrics
		}
	}
}

// TestTempoEntrypointParity_CanceledStatus pins that a query failing
// with context.Canceled classifies IDENTICALLY on both transports.
//
// Before the unification the gRPC head answered codes.Canceled while
// the HTTP head fell through its sentinel ladder to 500 — the same
// fault reported as a client-side abort on one transport and a server
// bug on the other, which is also what made it invisible in error-rate
// dashboards.
func TestTempoEntrypointParity_CanceledStatus(t *testing.T) {
	t.Parallel()

	httpStatus, _ := tempoParitySearchHTTP(t, &tempoParityQuerier{err: context.Canceled})
	grpcCode, _ := tempoParitySearchGRPC(t, &tempoParityQuerier{err: context.Canceled})

	if grpcCode != codes.Canceled {
		t.Errorf("gRPC Search on context.Canceled: got %s, want %s", grpcCode, codes.Canceled)
	}
	want, ok := tempoParityHTTPStatusFor[grpcCode]
	if !ok {
		t.Fatalf("gRPC code %s has no declared HTTP counterpart — extend tempoParityHTTPStatusFor", grpcCode)
	}
	if httpStatus != want {
		t.Errorf("HTTP /api/search on context.Canceled: got %d, want %d (the counterpart of gRPC %s)",
			httpStatus, want, grpcCode)
	}
}

// TestTempoEntrypointParity_InspectedTraces pins that SearchMetrics
// means the same thing on both transports for one seed.
//
// Before the unification each entrypoint had its own searchMetrics
// construction: HTTP reported the DRAINED SPAN ROW count and gRPC the
// DISTINCT TRACE count, so this seed produced 6 on one transport and 2
// on the other under a field literally named InspectedTraces. The
// canonical meaning is the trace count — it matches the field name and
// upstream Tempo. The span-drain volume is real information and did not
// disappear: it moved to tempo.HeaderInspectedSpans on HTTP and the
// matching gRPC trailer, which is what the drain-bound regressions in
// internal/api/tempo now read.
func TestTempoEntrypointParity_InspectedTraces(t *testing.T) {
	t.Parallel()

	rows := tempoParitySeedRows()
	if tempoParitySeedTraces == len(rows) {
		t.Fatalf("seed misconfigured: %d traces == %d rows, so this test cannot distinguish a trace count from a span count",
			tempoParitySeedTraces, len(rows))
	}

	httpStatus, httpResp := tempoParitySearchHTTP(t, &tempoParityQuerier{rows: rows})
	if httpStatus != http.StatusOK {
		t.Fatalf("HTTP /api/search: status %d, want 200", httpStatus)
	}
	grpcCode, grpcMetrics := tempoParitySearchGRPC(t, &tempoParityQuerier{rows: rows})
	if grpcCode != codes.OK {
		t.Fatalf("gRPC Search: code %s, want OK", grpcCode)
	}
	if grpcMetrics == nil {
		t.Fatal("gRPC Search: no frame carried SearchMetrics")
	}

	if got := httpResp.Metrics.InspectedTraces; got != tempoParitySeedTraces {
		t.Errorf("HTTP InspectedTraces = %d, want %d (distinct traces; %d is the span-row count)",
			got, tempoParitySeedTraces, len(rows))
	}
	if got := int(grpcMetrics.InspectedTraces); got != tempoParitySeedTraces {
		t.Errorf("gRPC InspectedTraces = %d, want %d (distinct traces; %d is the span-row count)",
			got, tempoParitySeedTraces, len(rows))
	}
	if httpResp.Metrics.InspectedTraces != int(grpcMetrics.InspectedTraces) {
		t.Errorf("InspectedTraces disagrees across transports: HTTP %d vs gRPC %d",
			httpResp.Metrics.InspectedTraces, grpcMetrics.InspectedTraces)
	}
}

// TestTempoEntrypointParity_InspectedSpansHeader pins that the relocated
// span-drain volume is actually reported rather than silently dropped —
// the accounting change moved the number, it did not delete it. A
// missing or non-numeric header is the failure mode that would let the
// drain-bound regressions in internal/api/tempo degrade into no-ops.
func TestTempoEntrypointParity_InspectedSpansHeader(t *testing.T) {
	t.Parallel()

	rows := tempoParitySeedRows()
	q := &tempoParityQuerier{rows: rows}

	mux := http.NewServeMux()
	tempoParityHandler(t, q).Mount(mux)
	srv := httptest.NewServer(mux)
	t.Cleanup(srv.Close)

	anchor := tempoParitySeedAnchor()
	url := fmt.Sprintf("%s/api/search?q=%%7B%%7D&start=%d&end=%d",
		srv.URL, anchor.Add(-tempoParityWindowMargin).Unix(), anchor.Add(tempoParityWindowMargin).Unix())
	resp, err := http.Get(url) //nolint:noctx // httptest server, test-local lifetime
	if err != nil {
		t.Fatalf("GET /api/search: %v", err)
	}
	defer func() { _ = resp.Body.Close() }()
	_, _ = io.Copy(io.Discard, resp.Body)

	raw := resp.Header.Get(tempo.HeaderInspectedSpans)
	got, err := strconv.Atoi(raw)
	if err != nil {
		t.Fatalf("%s = %q, want an integer: %v", tempo.HeaderInspectedSpans, raw, err)
	}
	if got != len(rows) {
		t.Errorf("%s = %d, want %d (the drained span-row count)", tempo.HeaderInspectedSpans, got, len(rows))
	}
}
