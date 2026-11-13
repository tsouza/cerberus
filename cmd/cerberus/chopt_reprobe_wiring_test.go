package main

import (
	"context"
	"errors"
	"io"
	"net/http"
	"net/http/httptest"
	"strings"
	"sync/atomic"
	"testing"
	"time"

	chproto "github.com/ClickHouse/ch-go/proto"
	"github.com/ClickHouse/clickhouse-go/v2"
	"github.com/ClickHouse/clickhouse-go/v2/lib/column"
	"github.com/ClickHouse/clickhouse-go/v2/lib/proto"

	"github.com/tsouza/cerberus/internal/chclient"
	"github.com/tsouza/cerberus/internal/chopt"
	"github.com/tsouza/cerberus/internal/config"
)

// fakeRolledBackServer answers the ClickHouse HTTP interface as a node
// rolled back below the release that introduced argAndMax: the driver's
// hello query gets the node's build, and every other statement is rejected
// with UNKNOWN_FUNCTION, in the wording and headers ClickHouse uses. The
// hello block is encoded at the protocol revision the driver decodes it at.
func fakeRolledBackServer(t *testing.T, node chopt.Version) *httptest.Server {
	t.Helper()
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		body, _ := io.ReadAll(r.Body)
		query := r.URL.Query().Get("query") + string(body)
		if strings.Contains(query, "version()") {
			hello := &proto.Block{}
			for _, col := range []struct{ name, typ string }{
				{"displayName()", "String"}, {"version()", "String"}, {"revision()", "UInt32"}, {"timezone()", "String"},
			} {
				if err := hello.AddColumn(col.name, column.Type(col.typ)); err != nil {
					t.Errorf("hello column %s: %v", col.name, err)
					return
				}
			}
			if err := hello.Append("rolled-back", node.String(), uint32(clickhouse.ClientTCPProtocolVersion), "UTC"); err != nil {
				t.Errorf("hello row: %v", err)
				return
			}
			var buf chproto.Buffer
			if err := hello.Encode(&buf, clickhouse.ClientTCPProtocolVersion); err != nil {
				t.Errorf("encode hello: %v", err)
				return
			}
			_, _ = w.Write(buf.Buf)
			return
		}
		w.Header().Set("X-ClickHouse-Exception-Code", "46")
		w.WriteHeader(http.StatusNotFound)
		_, _ = io.WriteString(w, "Code: 46. DB::Exception: Function with name `argAndMax` does not exist. "+
			"(UNKNOWN_FUNCTION) (version "+node.String()+" (official build))")
	}))
	t.Cleanup(srv.Close)
	return srv
}

// TestStartCHOptReprobe_RejectionSeenByTheClientProbesTheFleet drives the
// wiring the serving process runs: startCHOptReprobe over a real
// *chclient.Client. A node the set in force is too new for rejects a query
// that client sends, and that rejection alone must bring the fleet probe
// forward — the steady interval outlives the test — so the set is
// re-resolved against the rolled-back node.
func TestStartCHOptReprobe_RejectionSeenByTheClientProbesTheFleet(t *testing.T) {
	t.Parallel()
	node := chopt.Version{Major: 25, Minor: 10, Patch: 7, Build: 6}
	srv := fakeRolledBackServer(t, node)

	client, err := chclient.New(chclient.Config{
		Addr:     strings.TrimPrefix(srv.URL, "http://"),
		Protocol: clickhouse.HTTP,
	})
	if err != nil {
		t.Fatalf("chclient.New: %v", err)
	}
	t.Cleanup(func() { _ = client.Close() })

	cfg := config.Config{CHOptimizations: chopt.SelectionAuto, CHOptimizationsMode: chopt.Permissive}
	bootSet, _, err := chopt.Resolve(chopt.Config{Optimizations: cfg.CHOptimizations, Mode: cfg.CHOptimizationsMode}, rollbackFromBuild)
	if err != nil {
		t.Fatalf("resolve auto against %s: %v", rollbackFromBuild, err)
	}
	live := newCHOptLive(chOptResolution{Set: bootSet, ResolvedVersion: rollbackFromBuild})

	var probes atomic.Int32
	probe := func(context.Context) chclient.FleetVersions {
		probes.Add(1)
		return chclient.FleetVersions{Versions: []chopt.Version{rollbackFromBuild, node}, Complete: true}
	}
	ctx, cancel := context.WithCancel(context.Background())
	t.Cleanup(cancel)
	startCHOptReprobe(ctx, quietLogger(), cfg, live, chOptConsumers{client: client}, "",
		reprobeTiming{steady: time.Hour, earlySpacing: time.Millisecond}, probe)

	_, err = client.QueryStrings(ctx, "SELECT toString(argAndMax(number, number)) FROM numbers(3)")
	if !errors.Is(err, chclient.ErrNodeTooOld) {
		t.Fatalf("query on the rolled-back node: err = %v; want a rejection unwrapping to ErrNodeTooOld", err)
	}

	deadline := time.Now().Add(10 * time.Second)
	for live.get().ResolvedVersion != node {
		if time.Now().After(deadline) {
			t.Fatalf("the rejection did not bring the fleet probe forward (probes=%d): %+v", probes.Load(), live.get())
		}
		time.Sleep(5 * time.Millisecond)
	}
}
