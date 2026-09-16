//go:build chdb

// Proof that a range vector selector's window is left-EXCLUSIVE,
// right-INCLUSIVE — (T-range, T] — over a dataset carrying a sample
// exactly ON each boundary, differentially checked against the
// from-scratch Prometheus oracle test/property/instant_window_test.go
// already establishes (oracleInstantWindow).
//
// test/property/instant_window_test.go's own random InstantWindowSweep
// property draws its scrape/range/offset axes from a pool and cannot
// GUARANTEE a boundary-exact sample every run; this file pins the
// boundary-exact case deterministically (gen.RangeBoundaryCase), so a
// mutation that widens the window to closed-left is always caught, not
// merely likely to be.
package property_test

import (
	"context"
	"net/http"
	"net/http/httptest"
	"testing"

	"github.com/tsouza/cerberus/internal/api/prom"
	"github.com/tsouza/cerberus/internal/chclienttest"
	"github.com/tsouza/cerberus/internal/schema"
	"github.com/tsouza/cerberus/test/property"
	"github.com/tsouza/cerberus/test/property/gen"
	"github.com/tsouza/cerberus/test/spec/wire"
)

// TestPromQL_RangeWindowBoundaryExactSample proves cerberus's range
// vector window excludes a sample exactly at t=T-range and includes a
// sample exactly at t=T, over gen.RangeBoundaryCase's boundary-exact
// dataset.
func TestPromQL_RangeWindowBoundaryExactSample(t *testing.T) {
	cli := chclienttest.NewChDB(t)
	h := prom.New(cli, schema.DefaultOTelMetrics(), nil)
	mux := http.NewServeMux()
	h.Mount(mux)
	srv := httptest.NewServer(mux)
	t.Cleanup(srv.Close)

	c := gen.RangeBoundaryCase()
	cli.Seed(t, c.Dataset.DDL)

	want := oracleInstantWindow(c)
	if want.Err != nil || len(want.Rows) != 1 {
		t.Fatalf("oracle produced %+v, want exactly one row (err=%v)", want, want.Err)
	}
	if want.Rows[0].Value != gen.RangeBoundaryExpectedCount {
		t.Fatalf("oracle count_over_time() = %g, want %d (the boundary-exact pin itself is wrong)",
			want.Rows[0].Value, gen.RangeBoundaryExpectedCount)
	}

	got := wire.RunInstant(context.Background(), srv.URL, c.Query, wire.InstantOptions{})
	if diff := property.ValidateOutcomes(want, got); diff != "" {
		t.Fatalf("range-window boundary drift\nquery=%s evalTs=%d\n--- diff ---\n%s",
			c.Query.String, c.Query.EvalTs, diff)
	}
}
