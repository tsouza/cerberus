package promql_test

import (
	"strings"
	"testing"
	"time"

	"github.com/prometheus/prometheus/promql/parser"

	"github.com/tsouza/cerberus/internal/promql"
	"github.com/tsouza/cerberus/internal/schema"
)

// TestLower_Modifiers_ErrorsWithoutRange covers the modifier paths that
// require a query range context (`@ start()` / `@ end()`) but were
// invoked via the plain Lower entrypoint that doesn't carry one.
func TestLower_Modifiers_ErrorsWithoutRange(t *testing.T) {
	t.Parallel()

	s := schema.DefaultOTelMetrics()
	p := parser.NewParser(parser.Options{})

	cases := []struct {
		name    string
		query   string
		wantErr string
	}{
		{
			name:    "at start() without range context",
			query:   `up @ start()`,
			wantErr: "`@ start()` modifier requires query range context",
		},
		{
			name:    "at end() in range vector without range context",
			query:   `rate(http_requests_total[5m] @ end())`,
			wantErr: "`@ end()` modifier requires query range context",
		},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			t.Parallel()
			expr, err := p.ParseExpr(tc.query)
			if err != nil {
				t.Fatalf("ParseExpr: %v", err)
			}
			_, err = promql.Lower(expr, s)
			if err == nil {
				t.Fatalf("expected error containing %q, got nil", tc.wantErr)
			}
			if !strings.Contains(err.Error(), tc.wantErr) {
				t.Fatalf("error %q does not contain %q", err.Error(), tc.wantErr)
			}
		})
	}
}

// TestLowerAt_StartEndModifiers verifies the time-aware lowering
// resolves `@ start()` / `@ end()` against the threaded range.
func TestLowerAt_StartEndModifiers(t *testing.T) {
	t.Parallel()

	s := schema.DefaultOTelMetrics()
	p := parser.NewParser(parser.Options{})

	start := time.Unix(100, 0).UTC()
	end := time.Unix(500, 0).UTC()

	cases := []string{
		`up @ start()`,
		`up @ end()`,
		`rate(http_requests_total[5m] @ start())`,
		`rate(http_requests_total[5m] @ end())`,
	}
	for _, q := range cases {
		q := q
		t.Run(q, func(t *testing.T) {
			t.Parallel()
			expr, err := p.ParseExpr(q)
			if err != nil {
				t.Fatalf("ParseExpr(%q): %v", q, err)
			}
			if _, err := promql.LowerAt(expr, s, start, end); err != nil {
				t.Fatalf("LowerAt(%q): %v", q, err)
			}
		})
	}
}
