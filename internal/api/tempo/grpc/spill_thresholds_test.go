package grpc_test

import (
	"context"
	"strconv"
	"sync"
	"testing"
	"time"

	"github.com/tsouza/cerberus/internal/thirdparty/tempopb"

	"github.com/tsouza/cerberus/internal/api/tempo"
	tempogrpc "github.com/tsouza/cerberus/internal/api/tempo/grpc"
	"github.com/tsouza/cerberus/internal/chclient"
	"github.com/tsouza/cerberus/internal/chopt"
	"github.com/tsouza/cerberus/internal/engine"
	"github.com/tsouza/cerberus/internal/schema"
)

// spillCapBytes is the per-statement memory cap spillSettingsRecorder
// reports; every spill threshold is half of it.
const spillCapBytes int64 = 3 << 30

// spillRPCBudget bounds one RPC of TestRPCs_CarryEngineSpillThresholds.
const spillRPCBudget = 5 * time.Second

// spillSettingsRecorder is a tempo.Querier that records the per-query
// settings every statement reaches the client with, and answers each with no
// rows.
type spillSettingsRecorder struct {
	mu    sync.Mutex
	calls []map[string]any
}

func (r *spillSettingsRecorder) record(ctx context.Context) {
	r.mu.Lock()
	defer r.mu.Unlock()
	r.calls = append(r.calls, chclient.QuerySettingsFromContext(ctx))
}

func (r *spillSettingsRecorder) taken() []map[string]any {
	r.mu.Lock()
	defer r.mu.Unlock()
	calls := r.calls
	r.calls = nil
	return calls
}

func (r *spillSettingsRecorder) EffectiveMaxQueryMemoryBytes() int64 { return spillCapBytes }

func (r *spillSettingsRecorder) Query(ctx context.Context, _ string, _ ...any) ([]chclient.Sample, error) {
	r.record(ctx)
	return nil, nil
}

func (r *spillSettingsRecorder) QueryStrings(ctx context.Context, _ string, _ ...any) ([]string, error) {
	r.record(ctx)
	return nil, nil
}

// TestRPCs_CarryEngineSpillThresholds drives the four tag-discovery RPCs and
// a metrics range RPC, and requires every statement each sends through the
// handler's own client to carry the engine's spill thresholds, as the HTTP
// routes' statements do. A statement whose DISTINCT a spill harms (the span
// and resource tag names, the name intrinsic's values) carries every
// threshold but the DISTINCT one; the event and link tag names, the plain-map
// tag values and the exemplars statement, which runs after the engine's
// matrix statement, carry all four.
func TestRPCs_CarryEngineSpillThresholds(t *testing.T) {
	rec := &spillSettingsRecorder{}
	handler := tempo.New(rec, schema.DefaultOTelTraces(), "test-grpc", nil)
	handler.Engine.SetSettings(engine.SettingsRules{JoinSpill: true, DistinctSpill: true})
	client, cleanup := newServiceTestServer(t, tempogrpc.NewService(handler, nil, nil))
	t.Cleanup(cleanup)

	const (
		start = uint32(metricsFixtureStartNs / uint64(time.Second))
		end   = uint32(metricsFixtureEndNs / uint64(time.Second))
	)
	for _, tc := range []struct {
		name    string
		harmful bool
		run     func(ctx context.Context) error
	}{
		{"SearchTags", true, func(ctx context.Context) error {
			stream, err := client.SearchTags(ctx, &tempopb.SearchTagsRequest{Scope: "span", Start: start, End: end})
			if err != nil {
				return err
			}
			_, err = recvAll[tempopb.SearchTagsResponse](t, stream)
			return err
		}},
		{"SearchTagsV2 resource", true, func(ctx context.Context) error {
			stream, err := client.SearchTagsV2(ctx, &tempopb.SearchTagsRequest{Scope: "resource", Start: start, End: end})
			if err != nil {
				return err
			}
			_, err = recvAll[tempopb.SearchTagsV2Response](t, stream)
			return err
		}},
		{"SearchTagsV2 event", false, func(ctx context.Context) error {
			stream, err := client.SearchTagsV2(ctx, &tempopb.SearchTagsRequest{Scope: "event", Start: start, End: end})
			if err != nil {
				return err
			}
			_, err = recvAll[tempopb.SearchTagsV2Response](t, stream)
			return err
		}},
		{"SearchTags link", false, func(ctx context.Context) error {
			stream, err := client.SearchTags(ctx, &tempopb.SearchTagsRequest{Scope: "link", Start: start, End: end})
			if err != nil {
				return err
			}
			_, err = recvAll[tempopb.SearchTagsResponse](t, stream)
			return err
		}},
		{"SearchTagValues", false, func(ctx context.Context) error {
			stream, err := client.SearchTagValues(ctx, &tempopb.SearchTagValuesRequest{TagName: "http.method", Start: start, End: end})
			if err != nil {
				return err
			}
			_, err = recvAll[tempopb.SearchTagValuesResponse](t, stream)
			return err
		}},
		{"SearchTagValuesV2", false, func(ctx context.Context) error {
			stream, err := client.SearchTagValuesV2(ctx, &tempopb.SearchTagValuesRequest{
				TagName: "span.http.method", Query: `{ kind = server }`, Start: start, End: end,
			})
			if err != nil {
				return err
			}
			_, err = recvAll[tempopb.SearchTagValuesV2Response](t, stream)
			return err
		}},
		{"SearchTagValuesV2 intrinsic name", true, func(ctx context.Context) error {
			stream, err := client.SearchTagValuesV2(ctx, &tempopb.SearchTagValuesRequest{TagName: "name", Start: start, End: end})
			if err != nil {
				return err
			}
			_, err = recvAll[tempopb.SearchTagValuesV2Response](t, stream)
			return err
		}},
	} {
		t.Run(tc.name, func(t *testing.T) {
			ctx, cancel := context.WithTimeout(context.Background(), spillRPCBudget)
			defer cancel()
			if err := tc.run(ctx); err != nil {
				t.Fatalf("%s: %v", tc.name, err)
			}
			calls := rec.taken()
			if len(calls) == 0 {
				t.Fatalf("%s dispatched no statement", tc.name)
			}
			for i, got := range calls {
				checkSpillThresholds(t, tc.name, i, got, tc.harmful)
			}
		})
	}

	t.Run("MetricsQueryRange exemplars", func(t *testing.T) {
		ctx, cancel := context.WithTimeout(context.Background(), spillRPCBudget)
		defer cancel()
		stream, err := client.MetricsQueryRange(ctx, &tempopb.QueryRangeRequest{
			Query: `{ kind = server } > { kind = client } | rate()`,
			Start: metricsFixtureStartNs,
			End:   metricsFixtureEndNs,
			Step:  metricsFixtureStepNs,
		})
		if err != nil {
			t.Fatalf("open stream: %v", err)
		}
		drainRangeFrames(t, stream)
		calls := rec.taken()
		const matrixAndExemplars = 2
		if len(calls) < matrixAndExemplars {
			t.Fatalf("MetricsQueryRange ran %d statements; want the matrix, then the exemplars", len(calls))
		}
		checkSpillThresholds(t, "MetricsQueryRange exemplars", len(calls)-1, calls[len(calls)-1], false)
	})
}

// checkSpillThresholds requires statement i of what to carry every spill
// threshold the engine stamps with both version-gated features resolved in,
// at half spillCapBytes, but the DISTINCT one when a spill harms its DISTINCT
// (harmful), which it must then not carry at all.
func checkSpillThresholds(t *testing.T, what string, i int, got map[string]any, harmful bool) {
	t.Helper()
	half := spillCapBytes / 2
	want := []string{
		"max_bytes_before_external_group_by",
		"max_bytes_before_external_sort",
		chopt.SettingMaxBytesBeforeExternalJoin,
	}
	if !harmful {
		want = append(want, chopt.SettingMaxBytesBeforeExternalDistinct)
	} else if v, stamped := got[chopt.SettingMaxBytesBeforeExternalDistinct]; stamped {
		t.Errorf("statement %d of %s: %s = %v on a DISTINCT a spill harms; want it withheld",
			i, what, chopt.SettingMaxBytesBeforeExternalDistinct, v)
	}
	for _, name := range want {
		if v, ok := got[name].(int64); !ok || v != half {
			t.Errorf("statement %d of %s: %s = %v; want %s", i, what, name, got[name], strconv.FormatInt(half, 10))
		}
	}
}
