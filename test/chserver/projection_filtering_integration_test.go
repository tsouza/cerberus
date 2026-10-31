//go:build integration

package chserver

import (
	"context"
	"fmt"
	"net/url"
	"strconv"
	"testing"
	"time"

	"github.com/tsouza/cerberus/internal/chclient"
	"github.com/tsouza/cerberus/internal/chopt"
	"github.com/tsouza/cerberus/internal/chopttest"
	"github.com/tsouza/cerberus/internal/choptwire"
	"github.com/tsouza/cerberus/internal/engine"
	"github.com/tsouza/cerberus/internal/schema"
	"github.com/tsouza/cerberus/internal/schema/ddl"
)

// projectionFilteringBuilds are the pinned builds the proj_trace_id
// projection-filtering reproductions run against, with whether each must lose
// spans there: ClickHouse#120286's negated compound, and a plain TraceId
// lookup, which the 25.11 line's projection-index read also loses rows from.
//
//   - 25.10.7.6: the last line before the projection-index read (25.11).
//   - 25.11.1.558: the first release with it.
//   - 25.12.11.4: the first release whose plain lookup is whole again.
//   - 26.3.17.56, 26.8.36.3, 26.9.25.15: a release of every supported line
//     the fix was not backported to.
//   - 26.10.2.103: the first release of the line the fix merged into.
var projectionFilteringBuilds = []struct {
	image          string
	negatedPoisons bool
	plainPoisons   bool
}{
	{"clickhouse/clickhouse-server:25.10.7.6-alpine", false, false},
	{"clickhouse/clickhouse-server:25.11.1.558-alpine", true, true},
	{"clickhouse/clickhouse-server:25.12.11.4-alpine", true, false},
	{"clickhouse/clickhouse-server:26.3.17.56-alpine", true, false},
	{"clickhouse/clickhouse-server:26.8.36.3-alpine", true, false},
	{"clickhouse/clickhouse-server:26.9.25.15-alpine", true, false},
	{"clickhouse/clickhouse-server:26.10.2.103-alpine", false, false},
}

// projectionFilteringConcurrentBuilds bounds how many pinned builds run at
// once, so their containers' memory limits together stay within a CI runner.
const projectionFilteringConcurrentBuilds = 3

// Seed shape: projectionTraces traces of projectionSpansPerTrace spans each,
// interleaved in time, so a trace's spans are spread across the table's
// granules rather than packed into their own.
const (
	projectionTraces        = 20_000
	projectionSpansPerTrace = 60
	projectionSpans         = projectionTraces * projectionSpansPerTrace
	// projectionTraceIndex is the trace the probe looks up.
	projectionTraceIndex = 7
	// projectionSearchLimit lifts /api/search's trace and spans-per-spanset
	// caps above everything the probe can match.
	projectionSearchLimit = 100_000
)

// projectionTraceID is the hex trace id of trace i. The leading 'a' keeps it
// free of leading zeros, which cerberus strips from a TraceQL trace:id.
func projectionTraceID(i int) string {
	return fmt.Sprintf("a%031x", i)
}

// negatedAndTraceQL selects every span of one trace through a negated
// compound over the projection key: no span carries span.foo = "nope", so the
// negation keeps all of them. It lowers to PREWHERE (TraceId = ?) WHERE … AND
// not((TraceId = ?) AND (SpanAttributes[?] = ?)), the ClickHouse#120286 shape.
var negatedAndTraceQL = fmt.Sprintf(`{ trace:id = "%s" && !(trace:id = "%s" && span.foo = "nope") }`,
	projectionTraceID(projectionTraceIndex), projectionTraceID(projectionTraceIndex))

// plainTraceQL selects every span of the same trace by its id alone.
var plainTraceQL = fmt.Sprintf(`{ trace:id = "%s" }`, projectionTraceID(projectionTraceIndex))

// TestProjectionFiltering_NegatedAndAcrossBuilds ports ClickHouse#120286's
// reproduction to cerberus's own schema, with proj_trace_id installed by
// cerberus's DDL, and runs it through the production Tempo search handler.
// For every pinned build it establishes whether filtering through the
// projection (optimize_use_projection_filtering pinned to 1, against 0 as the
// truth) drops spans the negated compound or a plain lookup selects, requires
// chopt's shape-defect ranges to agree, and requires the production wiring —
// the resolved auto set and its engine settings rules — to return every span
// of both shapes, with the effective optimize_use_projection_filtering read
// back from system.query_log: 0 on the shape a defect of the build drops spans
// from, the server's default on the other, which then reads through the
// projection index wherever trace_id_bitmap_filter resolved.
func TestProjectionFiltering_NegatedAndAcrossBuilds(t *testing.T) {
	slots := make(chan struct{}, projectionFilteringConcurrentBuilds)
	for _, build := range projectionFilteringBuilds {
		t.Run(build.image, func(t *testing.T) {
			t.Parallel()
			slots <- struct{}{}
			defer func() { <-slots }()
			ctx, cancel := context.WithTimeout(context.Background(), 10*time.Minute)
			defer cancel()
			s := startServer(ctx, t, build.image)
			seedProjectionFilteringProbe(ctx, t, s)

			// --- server behaviour, with projection filtering pinned per query ---
			raw := newProbeMux(t, s.client(t, adminUser, adminPassword, chclient.Config{}), engine.SettingsRules{})
			dropped := func(q string) bool {
				truth := raw.searchMatchedSpans(projectionFilteringCtx(ctx, s.version, 0), t, q)
				if truth != projectionSpansPerTrace {
					t.Fatalf("%s with projection filtering off matched %d spans; want every span of the trace (%d)", q, truth, projectionSpansPerTrace)
				}
				got := raw.searchMatchedSpans(projectionFilteringCtx(ctx, s.version, 1), t, q)
				if got != truth {
					t.Logf("%s: %s through projection filtering matched %d of %d spans", s.image, q, got, truth)
				}
				return got != truth
			}
			negatedPoisoned, plainPoisoned := dropped(negatedAndTraceQL), dropped(plainTraceQL)
			if negatedPoisoned != build.negatedPoisons {
				t.Errorf("negated compound: projection filtering dropped spans = %v; want %v", negatedPoisoned, build.negatedPoisons)
			}
			if plainPoisoned != build.plainPoisons {
				t.Errorf("plain lookup: projection filtering dropped spans = %v; want %v", plainPoisoned, build.plainPoisons)
			}

			// --- chopt's recorded ranges agree with the build ---
			for _, c := range []struct {
				defect   string
				poisoned bool
			}{
				{chopt.ShapeDefectNegatedConjunctionProjection, negatedPoisoned},
				{chopt.ShapeDefectTraceIDProjectionRead, plainPoisoned},
			} {
				if got := chopt.KnownShapeDefect(c.defect, s.version); got != c.poisoned {
					t.Errorf("chopt says %s affects %s = %v, but the reproduction dropped spans = %v", c.defect, s.version, got, c.poisoned)
				}
			}
			// The defects gate query shapes, never the features: an explicit
			// trace_id_projection under enforcing still resolves, and auto
			// still selects trace_id_bitmap_filter wherever its floor allows.
			if _, _, err := chopt.Resolve(chopt.Config{
				Optimizations: chopt.FeatureTraceIDProjection, Mode: chopt.Enforcing, Capability: chopt.CapabilityAvailable,
			}, s.version); err != nil {
				t.Errorf("explicit %s under enforcing on %s: %v", chopt.FeatureTraceIDProjection, s.version, err)
			}

			// --- the production wiring answers correctly ---
			set := chopttest.ResolveEnabledSet(ctx, t, s.admin, chopt.SelectionAuto)
			if got, want := set.Has(chopt.FeatureTraceIDBitmapFilter), s.version.AtLeast(projectionIndexSince); got != want {
				t.Errorf("auto resolved %s = %v on %s; want %v", chopt.FeatureTraceIDBitmapFilter, got, s.version, want)
			}
			rules := choptwire.SettingsRules(set, schema.DefaultOTelMetrics(), schema.DefaultOTelTraces(), schema.DefaultOTelLogs())
			if rules.ResultCache {
				t.Fatal("the resolved set enabled result_cache; every probe answer must be computed, not served from cache")
			}
			prodClient := s.client(t, adminUser, adminPassword, chclient.Config{})
			prodClient.SetQueryConditionCacheOverride(conditionCacheOverrideOn(s.version))
			prod := newProbeMux(t, prodClient, rules)

			// The effective optimize_use_projection_filtering each query ran
			// under: off on the shape a defect of the build drops spans from,
			// the server default everywhere else.
			defaultFiltering := s.defaultSetting(ctx, t, settingOptimizeUseProjectionFiltering)
			for _, c := range []struct {
				q     string
				gated bool
				shape string
			}{
				{negatedAndTraceQL, negatedPoisoned || plainPoisoned, "negated"},
				{plainTraceQL, plainPoisoned, "plain"},
			} {
				qid := fmt.Sprintf("proj-filter-probe-%s-%d", c.shape, time.Now().UnixNano())
				if got := prod.searchMatchedSpans(chclient.WithQueryID(ctx, qid), t, c.q); got != projectionSpansPerTrace {
					t.Errorf("production %s search matched %d spans; want %d", c.shape, got, projectionSpansPerTrace)
				}
				want := defaultFiltering
				if c.gated {
					want = "0"
				}
				s.assertEffectiveSetting(ctx, t, qid, settingOptimizeUseProjectionFiltering, want)
				if c.q == plainTraceQL && !c.gated && set.Has(chopt.FeatureTraceIDBitmapFilter) {
					s.assertProjectionIndexUsed(ctx, t, raw, qid)
				}
			}
		})
	}
}

// projectionFilteringCtx pins optimize_use_projection_filtering on the
// request with the query condition cache off, so no cached verdict of another
// defect can stand in for the projection's. Where the server has the
// projection-index read it also lowers that read's row threshold to 0, as
// trace_id_bitmap_filter stamps it: the seed's parts are each below the
// server's default threshold, which a production-sized part clears on its own.
func projectionFilteringCtx(base context.Context, server chopt.Version, on int) context.Context {
	ctx := chclient.WithQuerySetting(cacheCtx(base, 0), settingOptimizeUseProjectionFiltering, on)
	if server.AtLeast(projectionIndexSince) {
		ctx = chclient.WithQuerySetting(ctx, settingMinTableRowsToUseProjectionIndex, 0)
	}
	return ctx
}

// projectionIndexSince is the first release line with the projection-index
// read (ClickHouse#81021) and its row-threshold setting.
var projectionIndexSince = chopt.Version{Major: 25, Minor: 11}

// settingOptimizeUseProjectionFiltering lets a read filter through a
// projection's index; the shape-defect rules stamp it 0.
const settingOptimizeUseProjectionFiltering = "optimize_use_projection_filtering"

// settingMinTableRowsToUseProjectionIndex is the projection-index row
// threshold trace_id_bitmap_filter stamps to 0.
const settingMinTableRowsToUseProjectionIndex = "min_table_rows_to_use_projection_index"

// seedProjectionFilteringProbe applies cerberus's own traces DDL with
// proj_trace_id and seeds the spans the probe reads.
func seedProjectionFilteringProbe(ctx context.Context, t *testing.T, s *server) {
	t.Helper()
	cfg := ddl.Config{TraceIDProjectionEnabled: true}
	if err := ddl.ApplyWithConfig(ctx, s.admin.Conn(), cfg, []ddl.Signal{ddl.Traces}); err != nil {
		t.Fatalf("%s: apply DDL: %v", s.image, err)
	}
	s.exec(ctx, t, fmt.Sprintf(`
INSERT INTO otel_traces
    (Timestamp, TraceId, SpanId, ParentSpanId, SpanName, SpanKind, ServiceName,
     ResourceAttributes, SpanAttributes, Duration, StatusCode, StatusMessage,
     ScopeName, ScopeVersion, TraceState)
SELECT
    toDateTime64(%d, 9) + toIntervalMillisecond(1 + number %% 3599999) AS Timestamp,
    concat('a', leftPad(lower(hex(number %% %d)), 31, '0')) AS TraceId,
    leftPad(hex(number), 16, '0') AS SpanId,
    '' AS ParentSpanId,
    'op' AS SpanName,
    'Server' AS SpanKind,
    concat('s', toString(number %% %d)) AS ServiceName,
    map('service.name', concat('s', toString(number %% %d))) AS ResourceAttributes,
    map('foo', if(number %% 2 = 1, 'bar', 'baz')) AS SpanAttributes,
    %d AS Duration,
    'Unset' AS StatusCode,
    '' AS StatusMessage, '' AS ScopeName, '' AS ScopeVersion, '' AS TraceState
FROM numbers(%d)`,
		probeWindowStart.Unix(), projectionTraces, probeServices, probeServices, probeDurationNs, projectionSpans))
}

// searchMatchedSpans runs a TraceQL /api/search over the probe window and
// returns the number of spans its span sets matched. ctx is the request's
// context.
// profileEventIndexBinarySearch counts the primary-key analyses that ran a
// binary search. The table's own primary key does not lead with TraceId, so a
// TraceId lookup binary-searches only proj_trace_id's key, which is the
// projection index's analysis.
const profileEventIndexBinarySearch = "IndexBinarySearchAlgorithm"

// assertProjectionIndexUsed requires the plain lookup's finished query qid to
// have analysed the projection index, and the same lookup with projection
// filtering off not to, which is what makes the event a witness here.
func (s *server) assertProjectionIndexUsed(ctx context.Context, t *testing.T, raw probeMux, qid string) {
	t.Helper()
	offQID := fmt.Sprintf("proj-filter-probe-off-%d", time.Now().UnixNano())
	raw.searchMatchedSpans(chclient.WithQueryID(projectionFilteringCtx(ctx, s.version, 0), offQID), t, plainTraceQL)
	if got := s.profileEvent(ctx, t, offQID, profileEventIndexBinarySearch); got != 0 {
		t.Fatalf("plain lookup with projection filtering off counted %s = %d; the event does not witness the projection index on %s",
			profileEventIndexBinarySearch, got, s.version)
	}
	if got := s.profileEvent(ctx, t, qid, profileEventIndexBinarySearch); got == 0 {
		t.Errorf("production plain lookup %s counted no %s: it did not read through the projection index", qid, profileEventIndexBinarySearch)
	}
}

// profileEvent is ProfileEvents[name] of finished query qid, summed over its
// query_log rows.
func (s *server) profileEvent(ctx context.Context, t *testing.T, qid, name string) uint64 {
	t.Helper()
	s.flushLogs(ctx, t)
	var n uint64
	if err := s.admin.Conn().QueryRow(ctx,
		"SELECT sum(ProfileEvents[?]) FROM system.query_log WHERE type = 'QueryFinish' AND query_id = ?",
		name, qid).Scan(&n); err != nil {
		t.Fatalf("read %s of %s: %v", name, qid, err)
	}
	return n
}

func (p probeMux) searchMatchedSpans(ctx context.Context, t *testing.T, q string) int {
	t.Helper()
	params := url.Values{}
	params.Set("q", q)
	params.Set("start", strconv.FormatInt(probeWindowStart.Unix(), 10))
	params.Set("end", strconv.FormatInt(probeWindowEnd.Unix(), 10))
	params.Set("limit", strconv.Itoa(projectionSearchLimit))
	params.Set("spss", strconv.Itoa(projectionSearchLimit))
	var resp struct {
		Traces []struct {
			SpanSets []struct {
				Matched int `json:"matched"`
			} `json:"spanSets"`
		} `json:"traces"`
	}
	serveJSON(ctx, t, p.mux, "/api/search?"+params.Encode(), &resp)
	matched := 0
	for _, tr := range resp.Traces {
		for _, ss := range tr.SpanSets {
			matched += ss.Matched
		}
	}
	return matched
}
