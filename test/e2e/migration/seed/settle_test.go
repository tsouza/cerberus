package seed

import (
	"context"
	"encoding/hex"
	"encoding/json"
	"fmt"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
	"time"
)

// The Tempo gate's contract is that it does not report a settled reference
// until THIS run's traces are searchable. A gate that reads only the live
// store's block counters passes the moment the store cuts its first block,
// which on a loaded runner happens while later traces are still in the WAL —
// and the parity lane then reports a substrate timing artifact as a parity
// diff. These tests hold the gate to the payload.

// tempoGateBudget bounds the gate under test. It spans several poll intervals,
// so a gate that is genuinely waiting gets more than one look at the stub
// before the deadline names the failure.
const tempoGateBudget = 3 * settleInterval

// stubTempo is a reference-Tempo stand-in: it reports whatever live-store
// counters and per-service search results a test hands it.
type stubTempo struct {
	blocksCompleted int
	blocklistLength int
	// searchable is the trace-id set the stub returns per service, in
	// reference-Tempo wire rendering.
	searchable map[string][]string
}

func (s stubTempo) server(t *testing.T) *httptest.Server {
	t.Helper()
	mux := http.NewServeMux()
	mux.HandleFunc("/metrics", func(w http.ResponseWriter, _ *http.Request) {
		fmt.Fprintf(w, "%s %d\n", tempoBlocksCompletedMetric, s.blocksCompleted)
		fmt.Fprintf(w, "%s{tenant=\"single-tenant\"} %d\n", tempoBlocklistLengthMetric, s.blocklistLength)
	})
	mux.HandleFunc("/api/search", func(w http.ResponseWriter, r *http.Request) {
		service := serviceFromTraceQuery(r.URL.Query().Get("q"))
		var body tempoSearchResponse
		for _, id := range s.searchable[service] {
			body.Traces = append(body.Traces, struct {
				TraceID string `json:"traceID"`
			}{TraceID: id})
		}
		w.Header().Set("Content-Type", "application/json")
		if err := json.NewEncoder(w).Encode(body); err != nil {
			t.Errorf("encode stub search response: %v", err)
		}
	})
	srv := httptest.NewServer(mux)
	t.Cleanup(srv.Close)
	return srv
}

// serviceFromTraceQuery reverses TraceServiceQuery so the stub can answer
// per-service, which is what makes a partial-ingest case expressible.
func serviceFromTraceQuery(q string) string {
	const prefix = `{resource.service.name="`
	const suffix = `"}`
	if !strings.HasPrefix(q, prefix) || !strings.HasSuffix(q, suffix) {
		return ""
	}
	return q[len(prefix) : len(q)-len(suffix)]
}

// tempoGateFixture is a two-service, two-traces-per-service fixture. The first
// trace-id byte is zero on every trace, so the reference rendering the gate
// compares against is exercised rather than assumed.
func tempoGateFixture() Fixture {
	anchor := time.Date(2026, 7, 25, 12, 0, 0, 0, time.UTC)
	f := Fixture{
		Window: Window{
			SeedStart: anchor.Add(-30 * time.Minute),
			AnchorEnd: anchor,
			Step:      time.Minute,
		},
	}
	for _, service := range []string{"checkout", "payments"} {
		for i := range 2 {
			var id [16]byte
			id[1] = byte(len(service))
			id[15] = byte(i + 1)
			f.Traces = append(f.Traces, Trace{
				ServiceName: service,
				Spans:       []Span{{TraceID: id, Start: anchor, End: anchor}},
			})
		}
	}
	return f
}

// searchableFromFixture is the complete, correctly rendered result set — the
// state a finished ingest reaches.
func searchableFromFixture(f Fixture) map[string][]string {
	out := map[string][]string{}
	for _, svc := range f.TraceIDsByService() {
		out[svc.Service] = append([]string(nil), svc.TraceIDs...)
	}
	return out
}

func TestWaitTempoSettledPassesOnlyWhenEveryPushedTraceIsSearchable(t *testing.T) {
	t.Parallel()

	f := tempoGateFixture()
	stub := stubTempo{
		blocksCompleted: tempoBlocksNeeded,
		blocklistLength: tempoBlocklistNeeded,
		searchable:      searchableFromFixture(f),
	}
	srv := stub.server(t)

	if err := WaitTempoSettled(context.Background(), srv.URL, f, tempoGateBudget); err != nil {
		t.Fatalf("gate rejected a reference holding the whole fixture: %v", err)
	}
}

func TestWaitTempoSettledFailsWhilePartOfThePushIsStillUningested(t *testing.T) {
	t.Parallel()

	f := tempoGateFixture()
	complete := searchableFromFixture(f)
	// The live store has cut and published a block — the counters both read
	// "ready" — but it holds only the first trace of one service. This is
	// exactly the state a push that outran max_block_duration leaves behind.
	partial := map[string][]string{
		"checkout": complete["checkout"][:1],
		"payments": complete["payments"],
	}
	stub := stubTempo{
		blocksCompleted: tempoBlocksNeeded,
		blocklistLength: tempoBlocklistNeeded,
		searchable:      partial,
	}
	srv := stub.server(t)

	err := WaitTempoSettled(context.Background(), srv.URL, f, tempoGateBudget)
	if err == nil {
		t.Fatal("gate reported a settled reference while one of the two checkout traces was still " +
			"uningested; the parity lane would read the missing trace as a parity diff")
	}
	for _, want := range []string{"checkout", "returned 1 traces, want the 2 this run pushed"} {
		if !strings.Contains(err.Error(), want) {
			t.Fatalf("gate error %q does not name %q", err, want)
		}
	}
}

func TestWaitTempoSettledFailsWhenTheSearchableSetIsADifferentTrace(t *testing.T) {
	t.Parallel()

	f := tempoGateFixture()
	complete := searchableFromFixture(f)
	// Same cardinality, different identity: a count-only gate would pass here.
	var foreign [16]byte
	foreign[15] = 0xff
	stub := stubTempo{
		blocksCompleted: tempoBlocksNeeded,
		blocklistLength: tempoBlocklistNeeded,
		searchable: map[string][]string{
			"checkout": {complete["checkout"][0], RenderTempoTraceID(hex.EncodeToString(foreign[:]))},
			"payments": complete["payments"],
		},
	}
	srv := stub.server(t)

	err := WaitTempoSettled(context.Background(), srv.URL, f, tempoGateBudget)
	if err == nil {
		t.Fatal("gate accepted a reference holding a trace the fixture never pushed")
	}
	if !strings.Contains(err.Error(), "want") {
		t.Fatalf("gate error %q does not name the expected trace id", err)
	}
}

func TestWaitTempoSettledFailsWhileTheLiveStoreHasPublishedNoBlock(t *testing.T) {
	t.Parallel()

	f := tempoGateFixture()
	stub := stubTempo{
		blocksCompleted: 0,
		blocklistLength: 0,
		searchable:      searchableFromFixture(f),
	}
	srv := stub.server(t)

	err := WaitTempoSettled(context.Background(), srv.URL, f, tempoGateBudget)
	if err == nil {
		t.Fatal("gate reported settled while the querier's blocklist was empty")
	}
	if !strings.Contains(err.Error(), "blocklist_length=0") {
		t.Fatalf("gate error %q does not name the unmet live-store signal", err)
	}
}

func TestWaitTempoSettledRejectsAnEmptyFixture(t *testing.T) {
	t.Parallel()

	stub := stubTempo{blocksCompleted: tempoBlocksNeeded, blocklistLength: tempoBlocklistNeeded}
	srv := stub.server(t)

	err := WaitTempoSettled(context.Background(), srv.URL, Fixture{}, tempoGateBudget)
	if err == nil {
		t.Fatal("gate passed over a fixture with no traces; it would have waited for nothing")
	}
	if !strings.Contains(err.Error(), "no traces") {
		t.Fatalf("gate error %q does not name the empty fixture", err)
	}
}
