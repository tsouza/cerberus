package tempo

import (
	"context"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"net/url"
	"reflect"
	"strconv"
	"strings"
	"testing"
	"time"

	"github.com/tsouza/cerberus/internal/chclient"
	"github.com/tsouza/cerberus/internal/schema"
)

// This file is the ratchet behind #1484 and the four instances of the same
// class before it (#1435, #1487, #1593, #1626): the Tempo head has FIVE
// request entrypoints that must agree on which TraceQL metrics pipelines
// they evaluate, and every one of those issues was one entrypoint quietly
// lacking a branch the others had.
//
// The table below is the single enumeration of those entrypoints. Every
// accepted pipeline form is driven through EVERY entry, so a form that
// works on /api/metrics/query_range and nowhere else fails here rather than
// in a Grafana panel. A sixth entrypoint that re-derives the accept/lower
// decision by hand instead of joining the shared core (metrics_pipeline.go)
// is visible as a missing row in this list.

// The compile-time census of the shared core's implementations: the two
// serving routers (each shared by an HTTP handler and its gRPC sibling) and
// the offline explain adapter. These are not runtime assertions — their job
// is to fail to COMPILE when metricsPipelineRouter grows a method for a new
// plan kind and any implementation has not grown the matching branch, which
// is the guarantee metrics_pipeline.go exists to provide.
var (
	_ metricsPipelineRouter = (*metricsRangeRouter)(nil)
	_ metricsPipelineRouter = (*metricsInstantRouter)(nil)
	_ metricsPipelineRouter = (*explainRouter)(nil)
)

// pipelineStubQuerier answers every query with the same single matrix row.
// The entrypoint table asserts acceptance and rejection, not response
// shaping (each kind's wire shape has its own dedicated test), so one row
// that decodes under any of the three Sample projections is enough.
type pipelineStubQuerier struct{}

func (pipelineStubQuerier) Query(context.Context, string, ...any) ([]chclient.Sample, error) {
	return []chclient.Sample{{
		Labels:    map[string]string{"__name__": "rate", "__bucket": "0.25", "__meta_type": "baseline"},
		Timestamp: pipelineFixtureStart,
		Value:     1,
	}}, nil
}

func (pipelineStubQuerier) QueryStrings(context.Context, string, ...any) ([]string, error) {
	return nil, nil
}

// The window every entrypoint in the table is driven over. Matches the
// package's usual 2026-05-12T10:00:00Z anchor.
var (
	pipelineFixtureStart = time.Date(2026, 5, 12, 10, 0, 0, 0, time.UTC)
	pipelineFixtureEnd   = pipelineFixtureStart.Add(3 * time.Minute)
)

const pipelineFixtureStep = time.Minute

// metricsEntrypoint is one of the five request paths that turns a TraceQL
// metrics query into an answer. invoke drives it and normalises the outcome
// onto (HTTP status, message) — the gRPC exports and the offline explain
// adapter return an error rather than a response, so they report the status
// their shared classification (ClassifyErr) assigns it.
type metricsEntrypoint struct {
	name   string
	invoke func(t *testing.T, h *Handler, query string) (int, string)
}

// errOutcome is the (status, message) normalisation for the entrypoints
// that return an error instead of writing a response.
func errOutcome(err error) (int, string) {
	if err != nil {
		return httpErrStatus(err), err.Error()
	}
	return http.StatusOK, ""
}

// httpOutcome drives an HTTP handler against a GET for the given path and
// decodes the Tempo error envelope out of a non-200 body.
func httpOutcome(t *testing.T, handle http.HandlerFunc, target string) (int, string) {
	t.Helper()
	rec := httptest.NewRecorder()
	handle(rec, httptest.NewRequest(http.MethodGet, target, nil))
	if rec.Code == http.StatusOK {
		return rec.Code, ""
	}
	var body ErrorResponse
	if err := json.Unmarshal(rec.Body.Bytes(), &body); err != nil {
		t.Fatalf("decode error envelope (status=%d body=%q): %v", rec.Code, rec.Body.String(), err)
	}
	return rec.Code, body.Message
}

// metricsQueryURL builds `path?q=…&start=…&end=…&step=…` with the escaping
// a TraceQL query always needs (braces, quotes, pipes).
func metricsQueryURL(path, query string) string {
	vals := url.Values{}
	vals.Set("q", query)
	vals.Set("start", strconv.FormatInt(pipelineFixtureStart.Unix(), 10))
	vals.Set("end", strconv.FormatInt(pipelineFixtureEnd.Unix(), 10))
	vals.Set("step", pipelineFixtureStep.String())
	return path + "?" + vals.Encode()
}

// metricsEntrypoints enumerates every Tempo request path that classifies a
// metrics pipeline. Adding one means adding a row here.
var metricsEntrypoints = []metricsEntrypoint{
	{
		name: "http /api/metrics/query_range",
		invoke: func(t *testing.T, h *Handler, query string) (int, string) {
			return httpOutcome(t, h.handleMetricsQueryRange,
				metricsQueryURL("/api/metrics/query_range", query))
		},
	},
	{
		name: "http /api/metrics/query",
		invoke: func(t *testing.T, h *Handler, query string) (int, string) {
			return httpOutcome(t, h.handleMetricsQueryInstant,
				metricsQueryURL("/api/metrics/query", query))
		},
	},
	{
		name: "grpc MetricsQueryRange",
		invoke: func(_ *testing.T, h *Handler, query string) (int, string) {
			_, err := h.ExecMetricsRange(context.Background(), query,
				pipelineFixtureStart, pipelineFixtureEnd, pipelineFixtureStep)
			return errOutcome(err)
		},
	},
	{
		name: "grpc MetricsQueryInstant",
		invoke: func(_ *testing.T, h *Handler, query string) (int, string) {
			_, err := h.ExecMetricsInstant(context.Background(), query,
				pipelineFixtureStart, pipelineFixtureEnd)
			return errOutcome(err)
		},
	},
	{
		name: "explain offline preview",
		invoke: func(t *testing.T, h *Handler, query string) (int, string) {
			// The entrypoint is explain's METRICS arm. explainLang.Parse
			// routes here whenever the parsed expression carries a metrics
			// pipeline or a second stage, and falls through to the search
			// preview otherwise — a different surface with its own plan
			// shape. Driving parseMetrics directly is what makes the row
			// comparable to the four serving entrypoints: it asks the same
			// arm the same question, including for the negative control.
			lang, ok := NewExplainLang(h.Schema, pipelineFixtureStep).(*explainLang)
			if !ok {
				t.Fatalf("NewExplainLang no longer returns *explainLang")
			}
			ctx := ExplainContext(context.Background(),
				pipelineFixtureStart, pipelineFixtureEnd, DefaultSearchLimit)
			expr, perr := parseExpr(ctx, query)
			if perr != nil {
				return errOutcome(perr)
			}
			plan, meta, err := lang.parseMetrics(ctx, expr)
			if err != nil {
				return errOutcome(err)
			}
			if plan == nil || !meta.IsMetric {
				t.Fatalf("explain accepted %q but produced plan=%v isMetric=%v", query, plan, meta.IsMetric)
			}
			return http.StatusOK, ""
		},
	},
}

func newPipelineHandler() *Handler {
	return New(pipelineStubQuerier{}, schema.DefaultOTelTraces(), "v1.0.0-test", nil)
}

// TestMetricsEntrypointsAcceptEveryPipelineKind is the #1484 regression in
// its general form: each metrics-pipeline kind cerberus can evaluate must
// be accepted by EVERY entrypoint, not just the one whose handler happened
// to grow the branch first. `| histogram_over_time(...)` rendered over
// /api/metrics/query_range while /api/metrics/query, the gRPC
// MetricsQueryInstant RPC and the offline explain preview all rejected it
// as "not a TraceQL metrics-pipeline expression".
func TestMetricsEntrypointsAcceptEveryPipelineKind(t *testing.T) {
	t.Parallel()

	// One query per metricsPipelineRouter method. A router method with no
	// query here is an evaluable kind nothing proves the entrypoints agree
	// on.
	queries := map[string]string{
		"scalar/rate":         "{} | rate()",
		"histogram_over_time": "{} | histogram_over_time(duration)",
		"compare":             "{} | compare({status = error})",
	}
	if len(queries) != len(metricsPipelineRouterMethods()) {
		t.Fatalf("queries cover %d kinds but metricsPipelineRouter has %d methods — "+
			"a new pipeline kind needs a query here", len(queries), len(metricsPipelineRouterMethods()))
	}

	for kind, query := range queries {
		t.Run(kind, func(t *testing.T) {
			t.Parallel()
			for _, ep := range metricsEntrypoints {
				t.Run(ep.name, func(t *testing.T) {
					t.Parallel()
					status, msg := ep.invoke(t, newPipelineHandler(), query)
					if status != http.StatusOK {
						t.Fatalf("%s rejected %q: status=%d msg=%s", ep.name, query, status, msg)
					}
				})
			}
		})
	}
}

// TestMetricsEntrypointsRejectNonPipelineIdentically is the negative
// control. A plain search expression is not a metrics query on ANY of the
// five surfaces, and all five must say so with the same status and the same
// accepted-forms sentence — that sentence used to be hand-copied per
// handler, and the copies disagreed: metrics_query_range.go listed
// `histogram_over_time` while the other three omitted it, so a caller was
// told a form was unsupported on one route and unavailable on another.
func TestMetricsEntrypointsRejectNonPipelineIdentically(t *testing.T) {
	t.Parallel()

	const query = `{ span.http.status_code = 500 }`

	// The shared tail of the rejection: everything from "requires" on is
	// owned by acceptedMetricsPipelineForms, so only the surface name may
	// differ between entrypoints.
	wantTail := "requires " + acceptedMetricsPipelineForms

	for _, ep := range metricsEntrypoints {
		t.Run(ep.name, func(t *testing.T) {
			t.Parallel()
			status, msg := ep.invoke(t, newPipelineHandler(), query)
			// 400, not 422: the query is well-formed TraceQL sent to the
			// wrong endpoint, which is what upstream Tempo answers 400 for.
			if status != http.StatusBadRequest {
				t.Errorf("status: got %d want %d (msg=%s)", status, http.StatusBadRequest, msg)
			}
			if !strings.Contains(msg, errNotMetricsPipeline.Error()) {
				t.Errorf("message %q does not name the shared sentinel %q", msg, errNotMetricsPipeline)
			}
			if !strings.HasSuffix(msg, wantTail) {
				t.Errorf("message %q does not end with the shared accepted-forms sentence %q", msg, wantTail)
			}
		})
	}
}

// TestAcceptedMetricsPipelineFormsCoversEveryRouterMethod pins the other
// half of the drift: the accepted-forms sentence is what a caller is told,
// and metricsPipelineRouter is what the code actually evaluates. A router
// method whose form is missing from the sentence tells the caller a working
// query is unsupported; a form in the sentence with no router method
// advertises one that cannot run.
func TestAcceptedMetricsPipelineFormsCoversEveryRouterMethod(t *testing.T) {
	t.Parallel()

	// The form each router method evaluates, keyed by method name.
	forms := map[string]string{
		"Scalar":    "`| rate()`",
		"Histogram": "`| histogram_over_time(...)`",
		"Compare":   "`| compare({...}, N)`",
	}
	for _, method := range metricsPipelineRouterMethods() {
		form, ok := forms[method]
		if !ok {
			t.Fatalf("metricsPipelineRouter method %q has no advertised form — "+
				"add it to acceptedMetricsPipelineForms and to this table", method)
		}
		if !strings.Contains(acceptedMetricsPipelineForms, form) {
			t.Errorf("acceptedMetricsPipelineForms omits %s (%s): %q",
				method, form, acceptedMetricsPipelineForms)
		}
	}
}

// metricsPipelineRouterMethods reflects the shared core's method set so the
// tests above fail when a plan kind is added without extending their
// tables. reflect is confined to this test file; production code never
// inspects the interface.
func metricsPipelineRouterMethods() []string {
	t := reflect.TypeOf((*metricsPipelineRouter)(nil)).Elem()
	out := make([]string, 0, t.NumMethod())
	for i := range t.NumMethod() {
		out = append(out, t.Method(i).Name)
	}
	return out
}
