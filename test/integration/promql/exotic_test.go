//go:build chdb

package promql

import (
	"fmt"
	"net/http"
	"net/http/httptest"
	"testing"

	"github.com/tsouza/cerberus/internal/api/prom"
	"github.com/tsouza/cerberus/internal/chclienttest"
	"github.com/tsouza/cerberus/internal/schema"
	"github.com/tsouza/cerberus/test/property"
	oraclepromql "github.com/tsouza/cerberus/test/property/oracle/promql"
	"github.com/tsouza/cerberus/test/spec/wire"
)

// exoticExtraEscapeChars are the punctuation bytes the exotic matrix's
// richer expressions carry beyond wire's base escape set — arithmetic and
// comparison operators, the `@` timestamp modifier, `:` in duration
// literals, etc.
const exoticExtraEscapeChars = "@:/*%^><!'"

// exoticInstantOpts is the wire.InstantOptions this suite runs every
// request with: the matrix deliberately includes top-level scalar
// expressions (e.g. `-2^2`, `scalar(...)`), so AllowScalar is true.
var exoticInstantOpts = wire.InstantOptions{
	AllowScalar:      true,
	ExtraEscapeChars: exoticExtraEscapeChars,
}

// TestExoticPromQL is the chDB-backed exotic-PromQL integration suite.
//
// It seeds ONE rich, fixed fixture (RichSeed) into an ephemeral chDB
// session, mounts the real prom.Handler behind an httptest server, then for
// every query in ExoticMatrix runs BOTH cerberus (parse -> lower ->
// optimize -> emit -> execute via the HTTP handler) AND the from-scratch
// oracle (test/property/oracle/promql.Evaluate), asserting they agree via
// property.CompareOutcomes (multiset, 1e-9 tol, NaN-equal, __name__-
// stripped).
//
// There is NO golden file and NO GOLDEN_UPDATE: expected results are
// computed at run time by the SUT-independent oracle, so a pass proves
// cerberus matches PromQL SEMANTICS, not that it reproduces a recording.
//
// CAT 1 (binary-op-over-rate) is the durable regression net for the prod
// code-47 break; it only passes once the vector-join code-47 fix
// (fix/vector-join-rate-metricname) is present, which this branch stacks
// on.
func TestExoticPromQL(t *testing.T) {
	ddl, model := RichSeed()

	cli := chclienttest.NewChDB(t)
	cli.Seed(t, ddl) // seed ONCE — every matrix query reads the same tables.

	h := prom.New(cli, schema.DefaultOTelMetrics(), nil)
	mux := http.NewServeMux()
	h.Mount(mux)
	srv := httptest.NewServer(mux)
	t.Cleanup(srv.Close)

	ds := property.Dataset{DDL: ddl, Metrics: model}

	// Seed-correctness self-check (design risk #2): before trusting the
	// matrix, prove the histogram BucketCounts prefix-sum + le synthesis is
	// right by running histogram_quantile against BOTH sides. If the seed's
	// cumulative _bucket{le} model diverges from cerberus's array fan-out,
	// this fails loudly here rather than masquerading as an engine bug
	// downstream.
	t.Run("seed-selfcheck/histogram_quantile", func(t *testing.T) {
		q := property.Query{
			String: "histogram_quantile(0.9, sum by(le)(rate(demo_api_request_duration_seconds_bucket[5m])))",
			EvalTs: EvalTs,
		}
		oracleOut := oraclepromql.Evaluate(ds, q, oraclepromql.Options{})
		cerberusOut := wire.RunInstant(t.Context(), srv.URL, q, exoticInstantOpts)
		if oracleOut.Err != nil {
			t.Fatalf("seed self-check: oracle errored: %v", oracleOut.Err)
		}
		if len(oracleOut.Rows) == 0 {
			t.Fatalf("seed self-check: oracle produced no histogram_quantile rows — seed is wrong")
		}
		zeroTimestamps(&oracleOut)
		zeroTimestamps(&cerberusOut)
		if diff := property.CompareOutcomes(oracleOut, cerberusOut); diff != "" {
			t.Fatalf("seed self-check histogram mismatch:\nquery=%s\n--- diff ---\n%s\n--- dataset ---\n%s",
				q.String, diff, dumpModel(model))
		}
	})

	for _, tc := range ExoticMatrix {
		tc := tc
		t.Run(tc.name, func(t *testing.T) {
			q := property.Query{String: tc.promql, EvalTs: tc.ts()}
			oracleOut := oraclepromql.Evaluate(ds, q, oraclepromql.Options{})
			cerberusOut := wire.RunInstant(t.Context(), srv.URL, q, exoticInstantOpts)
			// Instant queries stamp every row at the single eval instant,
			// so the timestamp carries no information to assert on. The
			// oracle stamps a top-level SCALAR at ts=0 while cerberus
			// surfaces `scalar(...)`-style scalars as an eval-ts-stamped
			// label-less vector row — zero both sides so the comparison is
			// about VALUES + LABELS, not the wire timestamp.
			zeroTimestamps(&oracleOut)
			zeroTimestamps(&cerberusOut)
			if diff := property.CompareOutcomes(oracleOut, cerberusOut); diff != "" {
				t.Fatalf("exotic drift\n--- query ---\n%s\nevalTs=%d\n--- diff (want=oracle got=cerberus) ---\n%s",
					q.String, q.EvalTs, diff)
			}
		})
	}
}

// zeroTimestamps sets every row's TimestampMs to 0 in place. Used to make
// instant-query comparisons timestamp-insensitive (see the call sites).
func zeroTimestamps(o *property.Outcome) {
	for i := range o.Rows {
		o.Rows[i].TimestampMs = 0
	}
}

// dumpModel renders the model's series for a failure log.
func dumpModel(m *property.MetricsModel) string {
	var out []byte
	for _, s := range m.Series {
		out = append(out, fmt.Sprintf("  %s%v points=%d\n", s.MetricName, s.Labels, len(s.Points))...)
	}
	return string(out)
}
