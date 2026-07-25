package main

import (
	"testing"

	"github.com/tsouza/cerberus/internal/config"
	"github.com/tsouza/cerberus/internal/logql"
	"github.com/tsouza/cerberus/internal/logql/lsyntax"
	"github.com/tsouza/cerberus/internal/migrateverify"
)

// TestRouteQuery_MatchesLogqlIsMetricQuery pins that the parity gate's offline
// LogQL classifier and the Loki head's own matrix-vs-streams pivot can never
// drift.
//
// The gate must replay exactly the queries cerberus answers with a matrix: a
// classifier that called a log-stream query "metric" would diff a streams body
// against a matrix baseline, and one that called a metric query "log-stream" would
// silently drop a judgeable panel into the out-of-scope bucket. The two predicates
// live in different packages (migrateverify may not import internal/logql), so
// this cross-package test is the only place the equality can be asserted.
func TestRouteQuery_MatchesLogqlIsMetricQuery(t *testing.T) {
	exprs := []string{
		`{job="api"}`,
		`{job="api"} |= "boom"`,
		`{job="api"} | json | line_format "{{.msg}}"`,
		`rate({job="api"}[5m])`,
		`count_over_time({job="api"}[1m])`,
		`sum(rate({job="api"}[5m]))`,
		`sum by (level) (count_over_time({job="api"}[5m]))`,
		`sum(rate({job="api"}[5m])) / sum(rate({job="db"}[5m]))`,
		`label_replace(rate({job="api"}[5m]), "dst", "$1", "job", "(.*)")`,
		`1+1`,
		`vector(1)`,
		`sum(rate({service.name="api"}[5m]))`,
		`quantile_over_time(0.99, {job="api"} | json | unwrap latency [5m])`,
	}
	for _, expr := range exprs {
		parsed, err := lsyntax.ParseExprWithoutValidation(lsyntax.NormalizeDottedLabels(expr))
		if err != nil {
			t.Fatalf("fixture %q must parse: %v", expr, err)
		}
		wantMetric := logql.IsMetricQuery(parsed)
		got := migrateverify.RouteQuery("logql", expr)
		if got.Replayable != wantMetric {
			t.Errorf("RouteQuery(%q).Replayable = %v, but the loki head's IsMetricQuery = %v: the gate would judge a shape cerberus does not serve as a matrix (routing = %+v)",
				expr, got.Replayable, wantMetric, got)
		}
	}
}

// TestHeadTokensMatchConfig pins that the parity gate's head tokens are the same
// strings the runtime uses for CERBERUS_ENABLED_HEADS, so an operator reads one
// vocabulary across the config, the flags, and the report. migrateverify may not
// import internal/config (the architecture gate keeps it off the runtime layers),
// so the constants are duplicated and this cross-package test is the pin.
func TestHeadTokensMatchConfig(t *testing.T) {
	cases := []struct {
		verify string
		cfg    config.Head
	}{
		{migrateverify.HeadProm, config.HeadProm},
		{migrateverify.HeadLoki, config.HeadLoki},
		{migrateverify.HeadTempo, config.HeadTempo},
	}
	for _, tc := range cases {
		if tc.verify != string(tc.cfg) {
			t.Errorf("migrateverify head token %q != config head %q: the report and CERBERUS_ENABLED_HEADS must speak one vocabulary", tc.verify, tc.cfg)
		}
	}
}
