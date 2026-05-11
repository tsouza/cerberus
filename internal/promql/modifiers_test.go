package promql_test

import (
	"strings"
	"testing"

	"github.com/prometheus/prometheus/promql/parser"

	"github.com/tsouza/cerberus/internal/promql"
	"github.com/tsouza/cerberus/internal/schema"
)

// TestLower_Modifiers_Errors covers the modifier paths that intentionally
// stay out of scope in M1.5 (the start()/end() variants depend on the API
// layer threading the query range through lowering).
func TestLower_Modifiers_Errors(t *testing.T) {
	t.Parallel()

	s := schema.DefaultOTelMetrics()
	p := parser.NewParser(parser.Options{})

	cases := []struct {
		name    string
		query   string
		wantErr string
	}{
		{
			name:    "at start() deferred",
			query:   `up @ start()`,
			wantErr: "`@ start()` / `@ end()` modifiers are not yet supported",
		},
		{
			name:    "at end() in range vector deferred",
			query:   `rate(http_requests_total[5m] @ end())`,
			wantErr: "`@ start()` / `@ end()` modifiers are not yet supported",
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
