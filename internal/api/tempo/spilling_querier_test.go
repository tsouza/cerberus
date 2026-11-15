package tempo_test

import (
	"context"
	"fmt"
	"net/http"
	"net/http/httptest"
	"net/url"
	"slices"
	"strconv"
	"strings"
	"sync"
	"testing"

	"github.com/tsouza/cerberus/internal/api/tempo"
	"github.com/tsouza/cerberus/internal/chclient"
	"github.com/tsouza/cerberus/internal/chopt"
	"github.com/tsouza/cerberus/internal/chsql"
	"github.com/tsouza/cerberus/internal/engine"
	"github.com/tsouza/cerberus/internal/schema"
)

// spillCapBytes is the per-statement memory cap the recording client
// reports; every spill threshold is half of it.
const spillCapBytes int64 = 3 << 30

// recordedStatement is one statement as it reached the client.
type recordedStatement struct {
	method   string
	sql      string
	settings map[string]any
}

// spillSettingsRecorder is a Querier that records the SQL and per-query
// settings of every statement it receives, and answers each with no rows.
type spillSettingsRecorder struct {
	mu    sync.Mutex
	calls []recordedStatement
}

func (r *spillSettingsRecorder) record(ctx context.Context, method, sql string) {
	r.mu.Lock()
	defer r.mu.Unlock()
	r.calls = append(r.calls, recordedStatement{method: method, sql: sql, settings: chclient.QuerySettingsFromContext(ctx)})
}

func (r *spillSettingsRecorder) taken() []recordedStatement {
	r.mu.Lock()
	defer r.mu.Unlock()
	calls := r.calls
	r.calls = nil
	return calls
}

func (r *spillSettingsRecorder) EffectiveMaxQueryMemoryBytes() int64 { return spillCapBytes }

func (r *spillSettingsRecorder) Query(ctx context.Context, sql string, _ ...any) ([]chclient.Sample, error) {
	r.record(ctx, "Query", sql)
	return nil, nil
}

func (r *spillSettingsRecorder) QueryStrings(ctx context.Context, sql string, _ ...any) ([]string, error) {
	r.record(ctx, "QueryStrings", sql)
	return nil, nil
}

// tempoSpillHandler is the handler configuration one case of
// TestHandlerDispatches_CarryEngineSpillThresholds runs under.
type tempoSpillHandler int

const (
	// defaultHandler serves the upstream OTel-CH traces schema.
	defaultHandler tempoSpillHandler = iota
	// materializedHandler adds the default materialized span and resource
	// attribute columns.
	materializedHandler
	// scopeAttrsHandler adds an instrumentation-scope attribute map.
	scopeAttrsHandler
	// jsonSpanAttrsHandler reads SpanAttributes under the JSON strategy, so
	// span tag names come from JSON paths rather than Map keys.
	jsonSpanAttrsHandler
)

// newTempoSpillHandler builds a Tempo handler on rec under variant, with
// both version-gated spill features resolved in, and mounts it.
func newTempoSpillHandler(rec *spillSettingsRecorder, variant tempoSpillHandler) *http.ServeMux {
	s := schema.DefaultOTelTraces()
	switch variant {
	case materializedHandler:
		s.MaterializedSpanAttributeColumns = schema.DefaultMaterializedSpanAttributeColumns()
		s.MaterializedResourceAttributeColumns = schema.DefaultMaterializedResourceAttributeColumns()
	case scopeAttrsHandler:
		s.ScopeAttributesColumn = "ScopeAttributes"
	}
	h := tempo.New(rec, s, "test", nil)
	if variant == jsonSpanAttrsHandler {
		h.Strategies = chsql.NewSchemaStrategiesCell(chsql.ResolvedSchemaStrategies{Traces: chsql.SchemaStrategies{
			Attrs: chsql.AttrStrategies{s.AttributesColumn: chsql.AttrStrategyJSON},
		}})
	}
	h.Engine.SetSettings(engine.SettingsRules{JoinSpill: true, DistinctSpill: true})
	mux := http.NewServeMux()
	h.Mount(mux)
	return mux
}

// TestHandlerDispatches_CarryEngineSpillThresholds drives every Tempo
// tag-discovery route, and the data-plane statements that dispatch through
// the handler's own client rather than the engine: a metrics range query's
// exemplars statement, which runs after the engine's matrix statement over
// the same closure, and a structural search's phase-A ranking statement,
// which runs before the engine's phase B. Each must reach the client with
// the engine's spill thresholds; without them it aborts at the memory cap
// where the engine's statement over the same data spills.
//
// A statement whose DISTINCT a spill harms (harmful) carries every threshold
// but the DISTINCT one: the name, kind and status intrinsic values, the
// values of a LowCardinality materialized span or resource column, and the
// span and resource tag names read off a Map column. Every other statement
// carries all four, among them the event and link tag names, the plain-map,
// auto-scope, event, link and numeric-materialized tag values, the
// instrumentation-scope and JSON-path tag names, and the data-plane
// statements, whose phase-A DISTINCT must be among them so a data-plane
// DISTINCT keeping its threshold is checked rather than vacuously absent.
func TestHandlerDispatches_CarryEngineSpillThresholds(t *testing.T) {
	window := "&start=1700000000&end=1700003600"
	values := func(tag string) string { return "/api/v2/search/tag/" + tag + "/values?" + window[1:] }
	for _, tc := range []struct {
		path    string
		variant tempoSpillHandler
		// bypassing selects the statements that dispatch outside the engine.
		bypassing func([]recordedStatement) []recordedStatement
		harmful   bool
		// distinct: the selected statements include a DISTINCT the test
		// must see.
		distinct bool
	}{
		{path: "/api/search/tags?scope=span" + window, bypassing: everyStatement, harmful: true},
		{path: "/api/search/tags?scope=resource" + window, bypassing: everyStatement, harmful: true},
		{path: "/api/search/tags?scope=event" + window, bypassing: everyStatement},
		{path: "/api/search/tags?scope=link" + window, bypassing: everyStatement},
		{path: "/api/search/tags?" + window[1:], bypassing: flatMapKeyStatements, harmful: true},
		{path: "/api/search/tags?" + window[1:], bypassing: nestedKeyStatements},
		{path: "/api/v2/search/tags?scope=none" + window, bypassing: flatMapKeyStatements, harmful: true},
		{path: "/api/v2/search/tags?scope=none" + window, bypassing: nestedKeyStatements},
		{path: "/api/v2/search/tags?scope=instrumentation" + window, variant: scopeAttrsHandler, bypassing: everyStatement},
		{path: "/api/v2/search/tags?scope=span" + window, variant: jsonSpanAttrsHandler, bypassing: everyStatement},
		{path: "/api/search/tag/name/values?" + window[1:], bypassing: everyStatement, harmful: true},
		{path: values("name"), bypassing: everyStatement, harmful: true},
		{path: values("kind"), bypassing: everyStatement, harmful: true},
		{path: values("status"), bypassing: everyStatement, harmful: true},
		{path: values("statusMessage"), bypassing: everyStatement},
		{path: values("duration"), bypassing: everyStatement},
		{path: values("span.rpc.method"), variant: materializedHandler, bypassing: everyStatement, harmful: true},
		{path: values("resource.k8s.namespace.name"), variant: materializedHandler, bypassing: everyStatement, harmful: true},
		{path: values("span.http.status_code"), variant: materializedHandler, bypassing: everyStatement},
		{path: values(".rpc.method"), variant: materializedHandler, bypassing: everyStatement},
		{path: values("span.http.method"), bypassing: everyStatement},
		{path: values("resource.k8s.namespace.name"), bypassing: everyStatement},
		{path: values(".http.method"), bypassing: everyStatement},
		{path: values("event.exception.type"), bypassing: everyStatement},
		{path: values("link.peer.service"), bypassing: everyStatement},
		{path: "/api/search/tag/http.method/values?" + window[1:], bypassing: everyStatement},
		{
			path:      "/api/v2/search/tag/span.http.method/values?q=" + url.QueryEscape(`{ kind = server }`) + window,
			bypassing: everyStatement,
		},
		{
			path:      "/api/v2/search/tag/name/values?q=" + url.QueryEscape(`{ kind = server }`) + window,
			bypassing: everyStatement, harmful: true,
		},
		{
			path:      "/api/metrics/query_range?step=60s&q=" + url.QueryEscape(`{ kind = server } > { kind = client } | rate()`) + window,
			bypassing: exemplarsStatement,
		},
		{
			path:      "/api/search?limit=20&q=" + url.QueryEscape(`{ kind = server } >> { kind = client }`) + window,
			bypassing: phaseAStatement, distinct: true,
		},
	} {
		t.Run(fmt.Sprintf("%d %s", tc.variant, tc.path), func(t *testing.T) {
			rec := &spillSettingsRecorder{}
			mux := newTempoSpillHandler(rec, tc.variant)
			w := httptest.NewRecorder()
			mux.ServeHTTP(w, httptest.NewRequest(http.MethodGet, tc.path, nil))
			calls := tc.bypassing(rec.taken())
			if len(calls) == 0 {
				t.Fatalf("GET %s dispatched no bypassing statement (HTTP %d: %s)", tc.path, w.Code, w.Body.String())
			}
			if tc.distinct && !slices.ContainsFunc(calls, func(c recordedStatement) bool { return strings.Contains(c.sql, "DISTINCT") }) {
				t.Fatalf("GET %s dispatched no DISTINCT, so no data-plane DISTINCT is checked", tc.path)
			}
			checkSpillThresholds(t, "GET "+tc.path, calls, tc.harmful)
		})
	}
}

// checkSpillThresholds requires every statement in calls to carry the spill
// thresholds wantSpillThresholds(harmful) names, and a statement whose
// DISTINCT a spill harms to carry no DISTINCT one.
func checkSpillThresholds(t *testing.T, what string, calls []recordedStatement, harmful bool) {
	t.Helper()
	want := wantSpillThresholds(harmful)
	for _, got := range calls {
		for name, value := range want {
			if v, ok := got.settings[name].(int64); !ok || strconv.FormatInt(v, 10) != value {
				t.Errorf("%s: %s = %v; want %s, on %s", what, name, got.settings[name], value, got.sql)
			}
		}
		if v, stamped := got.settings[chopt.SettingMaxBytesBeforeExternalDistinct]; harmful && stamped {
			t.Errorf("%s: %s = %v on a DISTINCT a spill harms; want it withheld, on %s",
				what, chopt.SettingMaxBytesBeforeExternalDistinct, v, got.sql)
		}
	}
}

// everyStatement selects every statement: the endpoint dispatches only
// outside the engine.
func everyStatement(calls []recordedStatement) []recordedStatement { return calls }

// nestedKeysRead is how a tag-name statement over the event or link
// attributes reads them: Events.Attributes or Links.Attributes, one nesting
// level below a flat attribute map.
var nestedKeysRead = []string{"`Events`.`Attributes`", "`Links`.`Attributes`"}

// readsNestedKeys reports whether c enumerates the event or link tag names.
func readsNestedKeys(c recordedStatement) bool {
	return slices.ContainsFunc(nestedKeysRead, func(read string) bool { return strings.Contains(c.sql, read) })
}

// flatMapKeyStatements selects the tag-name statements of a request over
// every scope that read a flat attribute map's keys — the span and resource
// tag names.
func flatMapKeyStatements(calls []recordedStatement) []recordedStatement {
	return slices.DeleteFunc(slices.Clone(calls), readsNestedKeys)
}

// nestedKeyStatements selects the tag-name statements of a request over
// every scope that read the event and link tag names. A request over every
// scope sends one statement per bucket, so it selects none unless both
// nested buckets ran.
func nestedKeyStatements(calls []recordedStatement) []recordedStatement {
	const eventAndLink = 2
	out := slices.DeleteFunc(slices.Clone(calls), func(c recordedStatement) bool { return !readsNestedKeys(c) })
	if len(out) != eventAndLink {
		return nil
	}
	return out
}

// exemplarsStatement selects the final statement, the exemplars statement a
// metrics range query runs after the engine's matrix statement. With fewer
// than two statements there is no exemplars statement to check, and it
// selects none.
func exemplarsStatement(calls []recordedStatement) []recordedStatement {
	const matrixAndExemplars = 2
	if len(calls) < matrixAndExemplars {
		return nil
	}
	return calls[len(calls)-1:]
}

// phaseAStatement selects the phase-A ranking statement of a structural
// search: the one statement a search dispatches through QueryStrings, ahead
// of the engine's phase B, which a phase A that ranks no trace skips.
func phaseAStatement(calls []recordedStatement) []recordedStatement {
	var out []recordedStatement
	for _, c := range calls {
		if c.method == "QueryStrings" {
			out = append(out, c)
		}
	}
	return out
}

// wantSpillThresholds is every spill threshold the engine stamps with both
// version-gated features resolved in, at half spillCapBytes, as the string
// the settings map renders. The DISTINCT one is withheld from a statement
// whose DISTINCT a spill harms (harmful).
func wantSpillThresholds(harmful bool) map[string]string {
	half := strconv.FormatInt(spillCapBytes/2, 10)
	want := map[string]string{
		"max_bytes_before_external_group_by":    half,
		"max_bytes_before_external_sort":        half,
		chopt.SettingMaxBytesBeforeExternalJoin: half,
	}
	if !harmful {
		want[chopt.SettingMaxBytesBeforeExternalDistinct] = half
	}
	return want
}
