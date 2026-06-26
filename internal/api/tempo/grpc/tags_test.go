package grpc_test

import (
	"context"
	"errors"
	"io"
	"net"
	"strings"
	"sync"
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

// fakeQuerier is a minimal tempo.Querier stub for the gRPC tag tests.
// It routes incoming SQL strings to canned string-result rows via a
// longest-substring match against stringsBySQL — the same pattern the
// HTTP-side handler_test.go uses for /search/tags fixtures so the
// span- vs resource-attribute lookups can return distinct rows even
// though they share one Querier surface.
type fakeQuerier struct {
	mu           sync.Mutex
	stringsBySQL map[string][]string
	strings      []string
	err          error
	lastSQLs     []string
	delay        time.Duration // optional sleep so cancellation tests can race
}

func (f *fakeQuerier) Query(_ context.Context, _ string, _ ...any) ([]chclient.Sample, error) {
	return nil, errors.New("fakeQuerier.Query unused in tag tests")
}

func (f *fakeQuerier) QueryStrings(ctx context.Context, sql string, _ ...any) ([]string, error) {
	f.mu.Lock()
	f.lastSQLs = append(f.lastSQLs, sql)
	delay := f.delay
	err := f.err
	rows := append([]string(nil), f.strings...)
	by := f.stringsBySQL
	f.mu.Unlock()

	if delay > 0 {
		select {
		case <-time.After(delay):
		case <-ctx.Done():
			return nil, ctx.Err()
		}
	}
	if err != nil {
		return nil, err
	}
	for needle, r := range by {
		if strings.Contains(sql, needle) {
			return append([]string(nil), r...), nil
		}
	}
	return rows, nil
}

// newTagsTestServer wires up the bufconn-based gRPC plumbing the four
// tag RPCs need: a fake querier → cerberus tempo.Handler → gRPC
// Service → grpc.Server on bufconn → client. Returns the dialled
// client so each subtest can call the RPC under test against a
// known-good fixture. Cleanup tears everything down on test exit.
func newTagsTestServer(t *testing.T, q tempo.Querier) tempopb.StreamingQuerierClient {
	t.Helper()
	handler := tempo.New(q, schema.DefaultOTelTraces(), "test-grpc", nil)
	service := tempogrpc.NewService(handler, nil, nil)

	lis := bufconn.Listen(1 << 20)
	srv := tempogrpc.NewServer(service)
	go func() {
		if err := srv.Serve(lis); err != nil {
			t.Logf("grpc Serve returned: %v", err)
		}
	}()
	t.Cleanup(srv.GracefulStop)

	conn, err := grpc.NewClient(
		"passthrough://bufnet",
		grpc.WithContextDialer(func(ctx context.Context, _ string) (net.Conn, error) {
			return lis.DialContext(ctx)
		}),
		grpc.WithTransportCredentials(insecure.NewCredentials()),
	)
	if err != nil {
		t.Fatalf("grpc dial bufnet: %v", err)
	}
	t.Cleanup(func() { _ = conn.Close() })

	return tempopb.NewStreamingQuerierClient(conn)
}

// recvAll reads every frame from a server-streaming RPC and returns
// them as a slice plus the terminating error (nil on a clean EOF).
// The helper is parametric over the streaming-client interface shape
// the four RPCs share via Go generics so each per-RPC subtest can
// reuse it without writing a per-type drain loop.
func recvAll[T any](t *testing.T, stream interface{ Recv() (*T, error) }) ([]*T, error) {
	t.Helper()
	var out []*T
	for {
		msg, err := stream.Recv()
		if err != nil {
			if errors.Is(err, io.EOF) {
				return out, nil
			}
			return out, err
		}
		out = append(out, msg)
	}
}

// TestSearchTags_SingleFrame asserts the V1 RPC streams exactly one
// frame whose TagNames slice carries the sorted union of every
// dynamic span- + resource-attribute key (intrinsics excluded —
// parity with the HTTP V1 envelope). After that single frame the
// stream MUST EOF — that's the contract single-frame RPCs honour.
func TestSearchTags_SingleFrame(t *testing.T) {
	t.Parallel()
	q := &fakeQuerier{stringsBySQL: map[string][]string{
		"`ResourceAttributes`": {"service.name", "host"},
		"`SpanAttributes`":     {"http.method", "host"},
	}}
	client := newTagsTestServer(t, q)

	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	t.Cleanup(cancel)
	stream, err := client.SearchTags(ctx, &tempopb.SearchTagsRequest{})
	if err != nil {
		t.Fatalf("open SearchTags stream: %v", err)
	}
	frames, err := recvAll[tempopb.SearchTagsResponse](t, stream)
	if err != nil {
		t.Fatalf("recvAll: %v", err)
	}
	if len(frames) != 1 {
		t.Fatalf("frame count: want 1, got %d (%v)", len(frames), frames)
	}
	got := frames[0].GetTagNames()
	want := []string{"host", "http.method", "service.name"}
	if len(got) != len(want) {
		t.Fatalf("tag count: want %d (%v), got %d (%v)", len(want), want, len(got), got)
	}
	for i, w := range want {
		if got[i] != w {
			t.Errorf("tag[%d]: want %q, got %q", i, w, got[i])
		}
	}
	for _, leaked := range []string{"name", "kind", "duration"} {
		for _, g := range got {
			if g == leaked {
				t.Errorf("intrinsic %q leaked into default V1 envelope: %v", leaked, got)
			}
		}
	}
}

// TestSearchTagsV2_ScopeBuckets asserts the V2 RPC streams one frame
// containing the three scope buckets (resource / span / intrinsic),
// each carrying the sorted attribute keys for that scope. The
// intrinsic bucket comes from cerberus's static inventory so its
// length pins to len(IntrinsicTags()) regardless of what the fake
// querier returns.
func TestSearchTagsV2_ScopeBuckets(t *testing.T) {
	t.Parallel()
	q := &fakeQuerier{stringsBySQL: map[string][]string{
		"`ResourceAttributes`": {"service.name"},
		"`SpanAttributes`":     {"http.method"},
	}}
	client := newTagsTestServer(t, q)

	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	t.Cleanup(cancel)
	stream, err := client.SearchTagsV2(ctx, &tempopb.SearchTagsRequest{})
	if err != nil {
		t.Fatalf("open SearchTagsV2 stream: %v", err)
	}
	frames, err := recvAll[tempopb.SearchTagsV2Response](t, stream)
	if err != nil {
		t.Fatalf("recvAll: %v", err)
	}
	if len(frames) != 1 {
		t.Fatalf("frame count: want 1, got %d", len(frames))
	}
	scopes := frames[0].GetScopes()
	byName := map[string][]string{}
	for _, sc := range scopes {
		byName[sc.GetName()] = sc.GetTags()
	}
	if got := byName["resource"]; len(got) != 1 || got[0] != "service.name" {
		t.Errorf("resource bucket: want [service.name], got %v", got)
	}
	if got := byName["span"]; len(got) != 1 || got[0] != "http.method" {
		t.Errorf("span bucket: want [http.method], got %v", got)
	}
	intr := byName["intrinsic"]
	if want := len(tempo.IntrinsicTags()); len(intr) != want {
		t.Errorf("intrinsic bucket length: want %d, got %d (%v)", want, len(intr), intr)
	}
}

// TestSearchTagsV2_ScopeFilter asserts that an explicit ?scope=
// request narrows the V2 response to the requested bucket only.
// Resource-scoped requests skip the span CH lookup entirely (and
// vice-versa) — that's the cost-saver the per-scope autocomplete
// relies on.
func TestSearchTagsV2_ScopeFilter(t *testing.T) {
	t.Parallel()
	q := &fakeQuerier{stringsBySQL: map[string][]string{
		"`ResourceAttributes`": {"service.name", "host"},
		"`SpanAttributes`":     {"http.method"},
	}}
	client := newTagsTestServer(t, q)

	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	t.Cleanup(cancel)
	stream, err := client.SearchTagsV2(ctx, &tempopb.SearchTagsRequest{Scope: "resource"})
	if err != nil {
		t.Fatalf("open SearchTagsV2 stream: %v", err)
	}
	frames, err := recvAll[tempopb.SearchTagsV2Response](t, stream)
	if err != nil {
		t.Fatalf("recvAll: %v", err)
	}
	if len(frames) != 1 {
		t.Fatalf("frame count: want 1, got %d", len(frames))
	}
	scopes := frames[0].GetScopes()
	if len(scopes) != 1 {
		t.Fatalf("scope count: want 1 (resource only), got %d (%v)", len(scopes), scopes)
	}
	if scopes[0].GetName() != "resource" {
		t.Errorf("scope name: want resource, got %q", scopes[0].GetName())
	}
	// Span CH lookup must NOT have fired — only the resource one.
	q.mu.Lock()
	sqls := append([]string(nil), q.lastSQLs...)
	q.mu.Unlock()
	for _, sql := range sqls {
		if strings.Contains(sql, "`SpanAttributes`") {
			t.Errorf("scope=resource leaked a SpanAttributes lookup: %s", sql)
		}
	}
}

// TestSearchTags_WindowlessDefaultsToRecentBound asserts the gRPC tag
// path applies the same recent-window default as the HTTP handler when
// a request carries no Start/End: the emitted SQL must carry a
// `Timestamp` bound so the mapKeys scan part-prunes otel_traces rather
// than full-scanning + exploding the attribute Map.
func TestSearchTags_WindowlessDefaultsToRecentBound(t *testing.T) {
	t.Parallel()
	q := &fakeQuerier{strings: []string{"service.name"}}
	client := newTagsTestServer(t, q)

	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	t.Cleanup(cancel)
	stream, err := client.SearchTags(ctx, &tempopb.SearchTagsRequest{})
	if err != nil {
		t.Fatalf("open SearchTags stream: %v", err)
	}
	if _, err := recvAll[tempopb.SearchTagsResponse](t, stream); err != nil {
		t.Fatalf("recvAll: %v", err)
	}
	q.mu.Lock()
	sqls := append([]string(nil), q.lastSQLs...)
	q.mu.Unlock()
	if len(sqls) == 0 {
		t.Fatal("no CH lookup fired for windowless SearchTags")
	}
	for _, sql := range sqls {
		if !strings.Contains(sql, "`Timestamp`") || !strings.Contains(sql, "toDateTime64") {
			t.Errorf("windowless gRPC tags lookup must bound on Timestamp, got: %s", sql)
		}
	}
}

// TestSearchTags_InvalidScope_GRPCStatus asserts the V1 RPC rejects
// unrecognised scope values with codes.InvalidArgument. The status
// code matters because Grafana's Tempo datasource maps 4xx-equivalent
// gRPC codes onto a user-visible "bad request" error, while
// codes.Internal would surface as a generic backend failure.
func TestSearchTags_InvalidScope_GRPCStatus(t *testing.T) {
	t.Parallel()
	client := newTagsTestServer(t, &fakeQuerier{})

	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	t.Cleanup(cancel)
	stream, err := client.SearchTags(ctx, &tempopb.SearchTagsRequest{Scope: "garbage"})
	if err != nil {
		t.Fatalf("open SearchTags stream: %v", err)
	}
	_, err = stream.Recv()
	if err == nil {
		t.Fatal("want InvalidArgument, got nil")
	}
	st, ok := status.FromError(err)
	if !ok {
		t.Fatalf("want grpc status, got %v", err)
	}
	if st.Code() != codes.InvalidArgument {
		t.Errorf("code: want InvalidArgument, got %s", st.Code())
	}
}

// TestSearchTagValues_DynamicAttribute asserts the V1 values RPC
// streams one frame with the de-duplicated, sorted union of every
// distinct value of the requested attribute. The HTTP and gRPC
// surfaces share the same SQL so this test pins the response-shape
// translation, not the SQL itself.
func TestSearchTagValues_DynamicAttribute(t *testing.T) {
	t.Parallel()
	q := &fakeQuerier{strings: []string{"foo", "bar", "foo"}}
	client := newTagsTestServer(t, q)

	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	t.Cleanup(cancel)
	stream, err := client.SearchTagValues(ctx, &tempopb.SearchTagValuesRequest{TagName: ".service.name"})
	if err != nil {
		t.Fatalf("open SearchTagValues stream: %v", err)
	}
	frames, err := recvAll[tempopb.SearchTagValuesResponse](t, stream)
	if err != nil {
		t.Fatalf("recvAll: %v", err)
	}
	if len(frames) != 1 {
		t.Fatalf("frame count: want 1, got %d", len(frames))
	}
	got := frames[0].GetTagValues()
	want := []string{"bar", "foo"}
	if len(got) != len(want) {
		t.Fatalf("value count: want %d (%v), got %d (%v)", len(want), want, len(got), got)
	}
	for i, w := range want {
		if got[i] != w {
			t.Errorf("value[%d]: want %q, got %q", i, w, got[i])
		}
	}
}

// TestSearchTagValuesV2_IntrinsicTypeLabel asserts the V2 values RPC
// stamps the right Type label on intrinsic columns: `duration` →
// "duration", `kind` → "kind", `name` → "string" (fallback). The
// Type field is the contract Grafana's autocomplete uses to format
// numeric vs string values differently.
func TestSearchTagValuesV2_IntrinsicTypeLabel(t *testing.T) {
	t.Parallel()
	cases := []struct {
		tag      string
		wantType string
	}{
		{"duration", "duration"},
		{"kind", "kind"},
		{"name", "string"},
	}
	for _, tc := range cases {
		tc := tc
		t.Run(tc.tag, func(t *testing.T) {
			t.Parallel()
			q := &fakeQuerier{strings: []string{"42"}}
			client := newTagsTestServer(t, q)

			ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
			t.Cleanup(cancel)
			stream, err := client.SearchTagValuesV2(ctx, &tempopb.SearchTagValuesRequest{TagName: tc.tag})
			if err != nil {
				t.Fatalf("open SearchTagValuesV2 stream: %v", err)
			}
			frames, err := recvAll[tempopb.SearchTagValuesV2Response](t, stream)
			if err != nil {
				t.Fatalf("recvAll: %v", err)
			}
			if len(frames) != 1 {
				t.Fatalf("frame count: want 1, got %d", len(frames))
			}
			vals := frames[0].GetTagValues()
			if len(vals) != 1 {
				t.Fatalf("value count: want 1, got %d", len(vals))
			}
			if vals[0].GetType() != tc.wantType {
				t.Errorf("type label: want %q, got %q", tc.wantType, vals[0].GetType())
			}
			if vals[0].GetValue() != "42" {
				t.Errorf("value: want %q, got %q", "42", vals[0].GetValue())
			}
		})
	}
}

// TestSearchTagValues_MissingTagName asserts both V1 and V2 reject a
// request with an empty TagName with codes.InvalidArgument — the
// gRPC equivalent of the HTTP handler's 400 "missing tag name".
func TestSearchTagValues_MissingTagName(t *testing.T) {
	t.Parallel()
	client := newTagsTestServer(t, &fakeQuerier{})

	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	t.Cleanup(cancel)

	t.Run("V1", func(t *testing.T) {
		t.Parallel()
		stream, err := client.SearchTagValues(ctx, &tempopb.SearchTagValuesRequest{})
		if err != nil {
			t.Fatalf("open: %v", err)
		}
		_, err = stream.Recv()
		assertCode(t, err, codes.InvalidArgument)
	})
	t.Run("V2", func(t *testing.T) {
		t.Parallel()
		stream, err := client.SearchTagValuesV2(ctx, &tempopb.SearchTagValuesRequest{})
		if err != nil {
			t.Fatalf("open: %v", err)
		}
		_, err = stream.Recv()
		assertCode(t, err, codes.InvalidArgument)
	})
}

// TestSearchTagValues_CHError_MapsToInternal asserts that a CH-side
// failure on the values query surfaces as codes.Internal — not
// InvalidArgument (a 4xx-equivalent that would tell Grafana to retry
// with different input). The HTTP surface returns 502 here; gRPC
// has no exact analogue and Internal is the canonical signal.
func TestSearchTagValues_CHError_MapsToInternal(t *testing.T) {
	t.Parallel()
	q := &fakeQuerier{err: errors.New("ch boom")}
	client := newTagsTestServer(t, q)

	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	t.Cleanup(cancel)
	stream, err := client.SearchTagValues(ctx, &tempopb.SearchTagValuesRequest{TagName: ".service.name"})
	if err != nil {
		t.Fatalf("open: %v", err)
	}
	_, err = stream.Recv()
	assertCode(t, err, codes.Internal)
}

// TestSearchTags_ContextCancellation asserts that cancelling the
// client-side context propagates through the stream context into the
// CH driver call, and the stream returns codes.Canceled. The fake
// querier sleeps long enough for the cancel to race; we verify the
// driver saw ctx.Done() (Recv returns a Canceled status) and the
// server didn't hang.
func TestSearchTags_ContextCancellation(t *testing.T) {
	t.Parallel()
	q := &fakeQuerier{delay: 2 * time.Second, strings: []string{"a"}}
	client := newTagsTestServer(t, q)

	ctx, cancel := context.WithCancel(context.Background())
	stream, err := client.SearchTags(ctx, &tempopb.SearchTagsRequest{})
	if err != nil {
		t.Fatalf("open: %v", err)
	}
	// Cancel mid-flight; the fakeQuerier's select{} branch on
	// ctx.Done() returns immediately with ctx.Err(), which our
	// handler maps to codes.Internal (the wrapper around CH
	// errors). The transport may also surface Canceled directly
	// when the server-side stream context closes first; both
	// outcomes are acceptable signals that cancellation
	// propagated rather than the server hanging.
	go func() {
		time.Sleep(50 * time.Millisecond)
		cancel()
	}()
	_, err = stream.Recv()
	if err == nil {
		t.Fatal("want cancellation error, got nil")
	}
	st, ok := status.FromError(err)
	if !ok {
		t.Fatalf("want grpc status, got %v", err)
	}
	if st.Code() != codes.Canceled && st.Code() != codes.Internal {
		t.Errorf("code: want Canceled or Internal, got %s", st.Code())
	}
}

// assertCode is the per-test gRPC status assertion: extracts the
// status from err, fails the test if it isn't there, and asserts
// the code matches want. Folds five-ish lines of boilerplate into
// one call so the per-RPC tests above stay readable.
func assertCode(t *testing.T, err error, want codes.Code) {
	t.Helper()
	if err == nil {
		t.Fatalf("want %s, got nil", want)
	}
	st, ok := status.FromError(err)
	if !ok {
		t.Fatalf("want grpc status, got %v", err)
	}
	if st.Code() != want {
		t.Fatalf("code: want %s, got %s", want, st.Code())
	}
}
