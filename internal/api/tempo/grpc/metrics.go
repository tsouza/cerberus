package grpc

import (
	"context"
	"errors"
	"math"
	"time"

	"github.com/grafana/tempo/pkg/tempopb"
	v1 "github.com/grafana/tempo/pkg/tempopb/common/v1"
	"google.golang.org/grpc/codes"
	"google.golang.org/grpc/status"

	"github.com/tsouza/cerberus/internal/api/tempo"
	"github.com/tsouza/cerberus/internal/chclient"
)

// This file implements the two metrics StreamingQuerier RPCs:
// MetricsQueryRange + MetricsQueryInstant. Both are SINGLE-FRAME
// streams per .claude/plans/tempo-grpc-streaming-design.md §6 (range
// mode emits one QueryRangeResponse after eager drain; instant is the
// single-bucket variant of the same shape). Frame batching does not
// apply — the matrix / instant pivot pipelines need the full row set
// in hand before zero-fill + quantile post-processing can produce a
// well-formed series envelope, so the streaming-friendly behaviour is
// to flush exactly one frame on completion.
//
// Both methods shadow the embedded
// tempopb.UnimplementedStreamingQuerierServer in service.go via Go's
// promoted-method-shadowing — implementing them here replaces the
// codes.Unimplemented default for these two RPCs only. Admission
// control is handled by Limiter.StreamInterceptor at the gRPC server
// level (server.go); the per-RPC code below does NOT re-acquire a
// slot.
//
// Error mapping (matches the design-doc §3 table):
//   - tempo.ErrParseStage / tempo.ErrLowerStage → codes.InvalidArgument
//     (user-facing query errors — bad TraceQL, non-metrics-pipeline
//     expression, missing start/end/step).
//   - chclient.ErrCircuitOpen → codes.Unavailable.
//   - any other engine / CH error → codes.Internal.
//
// Cancellation: stream.Context() flows through Handler.ExecMetrics*
// → engine.QueryPlan → chclient.Client.Query, so a client disconnect
// surfaces as ctx.Err() on the in-flight CH driver call and the gRPC
// transport closes the stream with codes.Canceled.

// MetricsQueryRange implements StreamingQuerier_MetricsQueryRangeServer.
// Mirrors the HTTP /api/metrics/query_range endpoint: evaluate the
// TraceQL metrics-pipeline expression over [Start, End] with the given
// Step, then return one tempopb.QueryRangeResponse carrying the full
// matrix (one TimeSeries per group-by tuple, samples sorted ascending,
// exemplars best-effort). Start / End / Step on the wire are uint64
// nanoseconds (per upstream Tempo's `pkg/tempopb` proto definition);
// Step = 0 is rejected as InvalidArgument.
func (s *Service) MetricsQueryRange(req *tempopb.QueryRangeRequest, stream tempopb.StreamingQuerier_MetricsQueryRangeServer) error {
	if s.Handler == nil {
		return status.Error(codes.Internal, "tempo gRPC: handler not wired")
	}
	if req == nil {
		return status.Error(codes.InvalidArgument, "tempo gRPC: nil QueryRangeRequest")
	}
	if req.Query == "" {
		return status.Error(codes.InvalidArgument, "tempo gRPC: missing 'query' field")
	}
	if req.Start == 0 || req.End == 0 {
		return status.Error(codes.InvalidArgument, "tempo gRPC: 'start' and 'end' are required")
	}
	if req.Step == 0 {
		return status.Error(codes.InvalidArgument, "tempo gRPC: 'step' must be > 0")
	}
	if req.End < req.Start {
		return status.Error(codes.InvalidArgument, "tempo gRPC: 'end' must not be before 'start'")
	}
	if req.Start > math.MaxInt64 || req.End > math.MaxInt64 || req.Step > math.MaxInt64 {
		return status.Error(codes.InvalidArgument, "tempo gRPC: 'start' / 'end' / 'step' must fit in int64 nanoseconds")
	}

	ctx := stream.Context()
	start := nanosToTime(req.Start)
	end := nanosToTime(req.End)
	step := time.Duration(req.Step) //nolint:gosec // bounded above by req.Step <= math.MaxInt64

	out, err := metricsExecRange(ctx, s.Handler, req.Query, start, end, step)
	if err != nil {
		return mapMetricsError(err)
	}
	return mapStreamSendError(stream.Send(out))
}

// MetricsQueryInstant implements StreamingQuerier_MetricsQueryInstantServer.
// Single-bucket evaluation over [Start, End] returning one InstantSeries
// per group-by tuple (each carrying a scalar Value rather than a
// Samples slice). Mirrors HTTP /api/metrics/query — Tempo's
// translateQueryRangeToInstant collapses a range response by setting
// step = end - start; cerberus follows the same rule on the gRPC side.
func (s *Service) MetricsQueryInstant(req *tempopb.QueryInstantRequest, stream tempopb.StreamingQuerier_MetricsQueryInstantServer) error {
	if s.Handler == nil {
		return status.Error(codes.Internal, "tempo gRPC: handler not wired")
	}
	if req == nil {
		return status.Error(codes.InvalidArgument, "tempo gRPC: nil QueryInstantRequest")
	}
	if req.Query == "" {
		return status.Error(codes.InvalidArgument, "tempo gRPC: missing 'query' field")
	}
	if req.Start == 0 || req.End == 0 {
		return status.Error(codes.InvalidArgument, "tempo gRPC: 'start' and 'end' are required")
	}
	if req.End <= req.Start {
		return status.Error(codes.InvalidArgument, "tempo gRPC: 'end' must be after 'start'")
	}
	if req.Start > math.MaxInt64 || req.End > math.MaxInt64 {
		return status.Error(codes.InvalidArgument, "tempo gRPC: 'start' / 'end' must fit in int64 nanoseconds")
	}

	ctx := stream.Context()
	start := nanosToTime(req.Start)
	end := nanosToTime(req.End)

	out, err := metricsExecInstant(ctx, s.Handler, req.Query, start, end)
	if err != nil {
		return mapMetricsError(err)
	}
	return mapStreamSendError(stream.Send(out))
}

// metricsExecRange runs the cerberus metrics-pipeline path for a range
// query and pivots the post-processed MetricsSeries list into the
// tempopb.QueryRangeResponse wire shape. Mirrors PR 2's searchFlusher
// in spirit (separate helper to keep test-side helpers focused) but
// is a single-frame pivot rather than a streaming flusher — range
// mode emits one frame after eager drain (design §6).
func metricsExecRange(ctx context.Context, h *tempo.Handler, query string, start, end time.Time, step time.Duration) (*tempopb.QueryRangeResponse, error) {
	res, err := h.ExecMetricsRange(ctx, query, start, end, step)
	if err != nil {
		return nil, err
	}
	out := &tempopb.QueryRangeResponse{
		Series: make([]*tempopb.TimeSeries, 0, len(res.Series)),
	}
	for _, ms := range res.Series {
		ts := &tempopb.TimeSeries{
			Labels:    metricsLabelsToKeyValues(ms.Labels),
			Samples:   make([]tempopb.Sample, 0, len(ms.Samples)),
			Exemplars: make([]tempopb.Exemplar, 0, len(ms.Exemplars)),
		}
		for _, s := range ms.Samples {
			ts.Samples = append(ts.Samples, tempopb.Sample{
				TimestampMs: s.TimestampMs,
				Value:       s.Value,
			})
		}
		for _, ex := range ms.Exemplars {
			ts.Exemplars = append(ts.Exemplars, tempopb.Exemplar{
				Labels:      metricsLabelsToKeyValues(ex.Labels),
				Value:       ex.Value,
				TimestampMs: ex.Timestamp,
			})
		}
		out.Series = append(out.Series, ts)
	}
	return out, nil
}

// metricsExecInstant runs the cerberus metrics-pipeline path for an
// instant query and pivots the post-processed MetricsInstantSeries
// list into the tempopb.QueryInstantResponse wire shape. Each series
// carries a scalar Value rather than a Samples slice.
func metricsExecInstant(ctx context.Context, h *tempo.Handler, query string, start, end time.Time) (*tempopb.QueryInstantResponse, error) {
	res, err := h.ExecMetricsInstant(ctx, query, start, end)
	if err != nil {
		return nil, err
	}
	out := &tempopb.QueryInstantResponse{
		Series: make([]*tempopb.InstantSeries, 0, len(res.Series)),
	}
	for _, ms := range res.Series {
		out.Series = append(out.Series, &tempopb.InstantSeries{
			Labels: metricsLabelsToKeyValues(ms.Labels),
			Value:  ms.Value,
		})
	}
	return out, nil
}

// metricsLabelsToKeyValues pivots cerberus's MetricsLabel slice into
// the tempopb v1.KeyValue + AnyValue wire shape (string variant only —
// TraceQL group-by keys + intrinsic labels are always stringified on
// the SQL side, so other AnyValue variants don't apply). Mirrors the
// HTTP JSON path's MetricsLabel.MarshalJSON envelope (metricsLabelWire)
// so the streaming + HTTP responses canonicalise identically.
func metricsLabelsToKeyValues(in []tempo.MetricsLabel) []v1.KeyValue {
	out := make([]v1.KeyValue, 0, len(in))
	for _, l := range in {
		out = append(out, v1.KeyValue{
			Key: l.Key,
			Value: &v1.AnyValue{
				Value: &v1.AnyValue_StringValue{StringValue: l.Value},
			},
		})
	}
	return out
}

// mapMetricsError converts an Engine / pipeline error into the gRPC
// status the metrics RPCs return. Mirrors classifyMetricsQueryRangeErr's
// HTTP-status mapping while adopting the gRPC error vocabulary:
//
//   - parse + lower (tempo.ErrParseStage / ErrLowerStage) →
//     codes.InvalidArgument (user-facing query errors).
//   - chclient circuit-open (errors.Is against chclient.ErrCircuitOpen) →
//     codes.Unavailable (downstream saturation, retry-after).
//   - everything else (emit / execute / unexpected) → codes.Internal.
func mapMetricsError(err error) error {
	if err == nil {
		return nil
	}
	switch {
	case errors.Is(err, chclient.ErrCircuitOpen):
		return status.Errorf(codes.Unavailable, "%v", err)
	case errors.Is(err, tempo.ErrParseStage), errors.Is(err, tempo.ErrLowerStage):
		return status.Errorf(codes.InvalidArgument, "%v", err)
	}
	return status.Errorf(codes.Internal, "%v", err)
}

// nanosToTime converts a tempopb-wire-format nanosecond timestamp
// (uint64; per upstream Tempo's QueryRangeRequest / QueryInstantRequest
// proto definition) into the time.Time the engine pipeline consumes.
// The MetricsQueryRange / MetricsQueryInstant entry points pre-check
// that the input fits in int64 (the gosec bound) before calling this
// helper, so the int64 cast is safe by construction; the //nolint:gosec
// directive keeps the linter from re-flagging the proven-safe cast.
func nanosToTime(n uint64) time.Time {
	return time.Unix(0, int64(n)).UTC() //nolint:gosec // caller pre-checks n <= math.MaxInt64
}

// mapStreamSendError preserves an existing gRPC status when the send
// failed mid-flight (the gRPC runtime usually returns status-wrapped
// errors here) and wraps a non-status error as codes.Internal so the
// caller never gets a bare transport error. Mirrors PR 2's
// mapStreamError for the search path.
func mapStreamSendError(err error) error {
	if err == nil {
		return nil
	}
	if _, ok := status.FromError(err); ok {
		return err
	}
	return status.Errorf(codes.Internal, "stream send: %v", err)
}
