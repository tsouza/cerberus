//go:build chdb

// Proof that rate()/increase()'s counter-reset compensation genuinely runs,
// over a dataset carrying a GENUINE mid-window value decrease between two
// adjacent samples (a real counter reset), differentially checked against
// the from-scratch Prometheus oracle test/property/instant_window_test.go
// already establishes (oracleInstantWindow).
//
// This is deliberately NOT test/property/rate_dup_timestamp_test.go: that
// file's own header states it proves duplicate-(Attributes,TimeUnix)
// dedup, explicitly not counter-reset handling — every one of its samples
// is monotonically non-decreasing. A detector that only ran that file
// could pass with counter-reset compensation disabled entirely, because
// nothing in its dataset ever decreases. This file's dataset does.
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

// TestPromQL_CounterResetMidWindowCompensation proves cerberus applies
// Prometheus's counter-reset rule — adding the pre-reset value back into
// the accumulated delta rather than reading a value decrease as a
// negative rate — over gen.CounterResetCase's genuine mid-window reset.
func TestPromQL_CounterResetMidWindowCompensation(t *testing.T) {
	cli := chclienttest.NewChDB(t)
	h := prom.New(cli, schema.DefaultOTelMetrics(), nil)
	mux := http.NewServeMux()
	h.Mount(mux)
	srv := httptest.NewServer(mux)
	t.Cleanup(srv.Close)

	c := gen.CounterResetCase()
	cli.Seed(t, c.Dataset.DDL)

	want := oracleInstantWindow(c)
	if want.Err != nil || len(want.Rows) != 1 {
		t.Fatalf("oracle produced %+v, want exactly one row (err=%v)", want, want.Err)
	}

	got := wire.RunInstant(context.Background(), srv.URL, c.Query, wire.InstantOptions{})
	if diff := property.ValidateOutcomes(want, got); diff != "" {
		t.Fatalf("counter-reset compensation drift\nquery=%s evalTs=%d\n--- diff ---\n%s",
			c.Query.String, c.Query.EvalTs, diff)
	}
}
