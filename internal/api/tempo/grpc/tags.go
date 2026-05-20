package grpc

import (
	"context"
	"errors"
	"time"

	"github.com/grafana/tempo/pkg/tempopb"
	"google.golang.org/grpc/codes"
	"google.golang.org/grpc/status"

	"github.com/tsouza/cerberus/internal/api/tempo"
)

// This file implements the four tag-list StreamingQuerier RPCs:
// SearchTags, SearchTagsV2, SearchTagValues, SearchTagValuesV2. All
// four are SINGLE-FRAME streams — tag lists are small (a few hundred
// entries at most for any realistic CH schema) so the streaming-
// friendly behaviour is to compute the full result eagerly and emit
// exactly one frame carrying the entire list. The gRPC client
// (Grafana's Tempo datasource) treats a one-frame stream identically
// to a multi-frame one; this strategy minimises Send-call overhead
// and matches the design recorded in .claude/plans/tempo-grpc-
// streaming-design.md §3.
//
// The four methods shadow the embedded
// tempopb.UnimplementedStreamingQuerierServer in service.go via Go's
// promoted-method-shadowing — implementing them here automatically
// replaces the codes.Unimplemented default for these RPCs only.
// Admission control is handled by Limiter.StreamInterceptor at the
// gRPC server level (server.go), so the per-RPC code below does NOT
// re-acquire a slot; the interceptor already short-circuited a
// saturated head before dispatch.
//
// Error mapping policy:
//   - bad input (unrecognised scope, malformed tag name) →
//     codes.InvalidArgument with the validator's error string.
//   - missing required RPC field (e.g. TagName for the values RPCs) →
//     codes.InvalidArgument.
//   - ClickHouse / driver failure → codes.Internal.
//   - context cancellation / stream-context cancel surfaces
//     naturally via the CH driver (which honours the ctx); the gRPC
//     transport then closes the stream with codes.Canceled.

// SearchTags implements StreamingQuerier_SearchTagsServer. Mirrors
// the HTTP /api/search/tags endpoint: returns the union of every
// dynamic span- + resource-attribute key seen in the time window,
// sorted ascending. Intrinsics are excluded by default (parity with
// upstream Tempo); only the explicit `scope=intrinsic` request emits
// the static intrinsic inventory.
func (s *Service) SearchTags(req *tempopb.SearchTagsRequest, stream tempopb.StreamingQuerier_SearchTagsServer) error {
	if s.Handler == nil {
		return status.Error(codes.Internal, "tempo gRPC service not wired to handler")
	}
	ctx := stream.Context()
	scope, err := tempo.ParseTagScope(req.GetScope())
	if err != nil {
		return status.Error(codes.InvalidArgument, err.Error())
	}
	start, end := tempoTagsBounds(req.GetStart(), req.GetEnd())
	resource, span, err := s.collectTagKeys(ctx, scope, start, end)
	if err != nil {
		return status.Error(codes.Internal, err.Error())
	}
	all := make([]string, 0, len(resource)+len(span)+len(tempo.IntrinsicTags()))
	all = append(all, resource...)
	all = append(all, span...)
	// Default + scope=resource/span carve out intrinsics; only an
	// explicit scope=intrinsic surfaces them on the V1 envelope
	// (matches the HTTP handler's respondTags policy).
	if scope == tempo.TagScopeIntrinsic {
		all = append(all, tempo.IntrinsicTags()...)
	}
	return stream.Send(&tempopb.SearchTagsResponse{TagNames: tempo.SortedUnique(all)})
}

// SearchTagsV2 implements StreamingQuerier_SearchTagsV2Server. Same
// data as SearchTags, partitioned into resource / span / intrinsic
// scope buckets so Grafana's autocomplete can surface each prefix
// separately. The intrinsic bucket is always emitted on a `none`
// request (the default); scoped requests filter to one bucket.
func (s *Service) SearchTagsV2(req *tempopb.SearchTagsRequest, stream tempopb.StreamingQuerier_SearchTagsV2Server) error {
	if s.Handler == nil {
		return status.Error(codes.Internal, "tempo gRPC service not wired to handler")
	}
	ctx := stream.Context()
	scope, err := tempo.ParseTagScope(req.GetScope())
	if err != nil {
		return status.Error(codes.InvalidArgument, err.Error())
	}
	start, end := tempoTagsBounds(req.GetStart(), req.GetEnd())
	resource, span, err := s.collectTagKeys(ctx, scope, start, end)
	if err != nil {
		return status.Error(codes.Internal, err.Error())
	}
	scopes := make([]*tempopb.SearchTagsV2Scope, 0, 3)
	if scope == tempo.TagScopeNone || scope == tempo.TagScopeResource {
		scopes = append(scopes, &tempopb.SearchTagsV2Scope{Name: tempo.TagScopeResource, Tags: tempo.SortedUnique(resource)})
	}
	if scope == tempo.TagScopeNone || scope == tempo.TagScopeSpan {
		scopes = append(scopes, &tempopb.SearchTagsV2Scope{Name: tempo.TagScopeSpan, Tags: tempo.SortedUnique(span)})
	}
	if scope == tempo.TagScopeNone || scope == tempo.TagScopeIntrinsic {
		scopes = append(scopes, &tempopb.SearchTagsV2Scope{Name: tempo.TagScopeIntrinsic, Tags: tempo.IntrinsicTags()})
	}
	return stream.Send(&tempopb.SearchTagsV2Response{Scopes: scopes})
}

// SearchTagValues implements StreamingQuerier_SearchTagValuesServer.
// Returns every distinct value observed for one attribute key.
// Intrinsic names (`name`, `kind`, …) route to dedicated CH columns;
// scoped names (`resource.x` / `span.x`) route to one attribute
// map; auto-scope names (`.x`, bare dotted) union both maps. Empty
// values are filtered out (the arrayJoin([]) shape can synthesise
// them for rows where only one map carries the key).
func (s *Service) SearchTagValues(req *tempopb.SearchTagValuesRequest, stream tempopb.StreamingQuerier_SearchTagValuesServer) error {
	if s.Handler == nil {
		return status.Error(codes.Internal, "tempo gRPC service not wired to handler")
	}
	name := req.GetTagName()
	if name == "" {
		return status.Error(codes.InvalidArgument, "missing tag name")
	}
	values, _, err := s.lookupTagValues(stream.Context(), name, req.GetStart(), req.GetEnd())
	if err != nil {
		return err
	}
	return stream.Send(&tempopb.SearchTagValuesResponse{TagValues: values})
}

// SearchTagValuesV2 implements
// StreamingQuerier_SearchTagValuesV2Server. Same query path as
// SearchTagValues but wraps each value in a tempopb.TagValue
// {Type, Value} pair: intrinsic types echo Tempo's vocabulary
// ("duration" / "status" / "kind"), dynamic attributes report
// "string" (cerberus stores SpanAttributes/ResourceAttributes as
// CH Map(String, String)).
func (s *Service) SearchTagValuesV2(req *tempopb.SearchTagValuesRequest, stream tempopb.StreamingQuerier_SearchTagValuesV2Server) error {
	if s.Handler == nil {
		return status.Error(codes.Internal, "tempo gRPC service not wired to handler")
	}
	name := req.GetTagName()
	if name == "" {
		return status.Error(codes.InvalidArgument, "missing tag name")
	}
	values, typ, err := s.lookupTagValues(stream.Context(), name, req.GetStart(), req.GetEnd())
	if err != nil {
		return err
	}
	out := make([]*tempopb.TagValue, 0, len(values))
	for _, v := range values {
		out = append(out, &tempopb.TagValue{Type: typ, Value: v})
	}
	return stream.Send(&tempopb.SearchTagValuesV2Response{TagValues: out})
}

// collectTagKeys runs the two attribute-map lookups (one per scope
// bucket) the V1 + V2 tag-list endpoints both share. The boolean
// branches mirror respondTags in search_tags.go so the gRPC and
// HTTP surfaces agree on what data each scope returns: a scoped
// request runs only one query, a `none` (default) request runs both.
func (s *Service) collectTagKeys(ctx context.Context, scope string, start, end time.Time) (resource, span []string, err error) {
	if scope == tempo.TagScopeNone || scope == tempo.TagScopeResource {
		resource, err = s.Handler.FetchTagKeys(ctx, s.Handler.Schema.ResourceAttributesColumn, start, end)
		if err != nil {
			return nil, nil, err
		}
	}
	if scope == tempo.TagScopeNone || scope == tempo.TagScopeSpan {
		span, err = s.Handler.FetchTagKeys(ctx, s.Handler.Schema.AttributesColumn, start, end)
		if err != nil {
			return nil, nil, err
		}
	}
	return resource, span, nil
}

// lookupTagValues encapsulates the parse-name + resolve-scope +
// fetch-values + map-errors-to-grpc-codes pipeline shared by
// SearchTagValues and SearchTagValuesV2. The returned error is
// already a *grpc/status error (mapped to InvalidArgument /
// Internal) so callers can return it as-is.
func (s *Service) lookupTagValues(ctx context.Context, name string, startSec, endSec uint32) (values []string, valueType string, err error) {
	resolved, parseErr := s.Handler.ResolveTagName(name)
	// resolveTagName returns a non-nil error only on a parser-
	// rejected bare dotted form. V1 historically accepted that; the
	// grpc surface stays equally permissive — the parser error
	// surfaces only as a debug log (not exposed today), the bare
	// name is treated as auto-scope. An empty Key after the
	// fallback would be a programmer error, not a client error.
	_ = parseErr
	if !resolved.IsIntrinsic && resolved.Key == "" {
		return nil, "", status.Error(codes.InvalidArgument, "unresolved tag name")
	}
	start, end := tempoTagsBounds(startSec, endSec)
	vals, typ, err := s.Handler.FetchTagValues(ctx, resolved, start, end)
	if err != nil {
		if errors.Is(err, context.Canceled) || errors.Is(err, context.DeadlineExceeded) {
			// Let the gRPC transport propagate the cancellation
			// status the client expects; wrapping into Internal
			// would mask it as a server fault.
			return nil, "", err
		}
		return nil, "", status.Error(codes.Internal, err.Error())
	}
	return vals, typ, nil
}

// tempoTagsBounds converts the SearchTagsRequest / SearchTagValuesRequest
// uint32 Start/End fields (Unix seconds; zero meaning "no bound") into
// the time.Time pair the SQL builder consumes. A zero uint32 maps to
// the zero time.Time, which the QueryBuilder treats as "predicate
// omitted" (see internal/api/tempo/search_tags.go buildSearchTagsSQL).
func tempoTagsBounds(start, end uint32) (time.Time, time.Time) {
	var s, e time.Time
	if start != 0 {
		s = time.Unix(int64(start), 0).UTC()
	}
	if end != 0 {
		e = time.Unix(int64(end), 0).UTC()
	}
	return s, e
}
