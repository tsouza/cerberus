package logql_test

import (
	"context"
	"strings"
	"testing"

	"github.com/tsouza/cerberus/internal/chplan"
	"github.com/tsouza/cerberus/internal/chsql"
	"github.com/tsouza/cerberus/internal/engine"
	"github.com/tsouza/cerberus/internal/logql"
	syntax "github.com/tsouza/cerberus/internal/logql/lsyntax"
	"github.com/tsouza/cerberus/internal/schema"
)

// log_row_shape_test.go — issue #1430.
//
// A log stream has no numeric value. Before #1430 the log-stream projection
// still emitted a fourth `toFloat64(0) AS Value` column, purely so the row
// was as wide as the shared cursor's fixed positional scan; every log query
// paid for that column on the wire and then discarded it.
//
// These tests pin the shape from the emitter side: the plan-level projection
// list, the alias order, and the SQL text ClickHouse actually receives.

// logStreamProjection lowers q's log-stream wire projection through the same
// Lang.ProjectSamples call engine.QueryPlan makes.
func logStreamProjection(t *testing.T, q string, s schema.Logs) *chplan.Project {
	t.Helper()

	expr, err := syntax.ParseExpr(q)
	if err != nil {
		t.Fatalf("ParseExpr(%q): %v", q, err)
	}
	l := &logql.Lang{Schema: s}
	wrapped := l.ProjectSamples(&chplan.Scan{Table: s.LogsTable}, engine.Meta{
		IsMetric: false,
		Extra:    map[string]any{"expr": expr},
	})
	proj, ok := wrapped.(*chplan.Project)
	if !ok {
		t.Fatalf("ProjectSamples(%q) returned %T, want *chplan.Project", q, wrapped)
	}
	return proj
}

// projectionAliases is the projection's alias list in emission order — the
// positional contract the chclient cursor's scan binds against.
func projectionAliases(proj *chplan.Project) []string {
	out := make([]string, len(proj.Projections))
	for i, p := range proj.Projections {
		out[i] = p.Alias
	}
	return out
}

// TestLogStreamProjection_HasNoValueColumn pins the alias list of both
// log-stream layouts: the four-wide (Line, Attributes, TimeUnix, Metadata)
// shape a schema WITH a structured-metadata column emits, and the three-wide
// (Line, Attributes, TimeUnix) shape a schema without one emits. Neither
// carries a `Value` alias.
//
// The `Value` assertion is the issue-#1430 regression checkpoint: it fails
// the moment a placeholder column is reintroduced anywhere in the log-stream
// projection, regardless of which expression produces it.
func TestLogStreamProjection_HasNoValueColumn(t *testing.T) {
	t.Parallel()

	withMetadata := schema.DefaultOTelLogs()
	noMetadata := schema.DefaultOTelLogs()
	// A schema whose structured-metadata column is blank skips the Metadata
	// projection entirely, leaving the bare three-column log row.
	noMetadata.AttributesColumn = ""

	cases := []struct {
		name string
		s    schema.Logs
		want []string
	}{
		{
			name: "structured-metadata schema",
			s:    withMetadata,
			want: []string{logql.LogLineColumn, "Attributes", "TimeUnix", "Metadata"},
		},
		{
			name: "schema without a structured-metadata column",
			s:    noMetadata,
			want: []string{logql.LogLineColumn, "Attributes", "TimeUnix"},
		},
	}
	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			t.Parallel()

			proj := logStreamProjection(t, `{job="api"}`, c.s)
			got := projectionAliases(proj)
			if len(got) != len(c.want) {
				t.Fatalf("projection aliases = %v (%d slots), want %v (%d slots)",
					got, len(got), c.want, len(c.want))
			}
			for i := range c.want {
				if got[i] != c.want[i] {
					t.Errorf("projection alias[%d] = %q, want %q (full list %v)", i, got[i], c.want[i], got)
				}
			}
			for i, alias := range got {
				if alias == "Value" {
					t.Errorf("projection slot %d aliases %q: a log stream has no numeric "+
						"value, so the log-stream projection must carry no Value column "+
						"at all — the placeholder issue #1430 removed (full list %v)",
						i, alias, got)
				}
			}
		})
	}
}

// TestLogStreamProjection_NoPlaceholderLiteralInPlan pins the plan-level
// half: no slot of a log-stream projection is a bare numeric literal. The
// removed placeholder was a *chplan.LitFloat{V: 0}, and a literal in this
// projection means the emitter is manufacturing data rather than projecting
// it.
func TestLogStreamProjection_NoPlaceholderLiteralInPlan(t *testing.T) {
	t.Parallel()

	for _, q := range []string{
		`{job="api"}`,
		`{job="api"} |= "error"`,
		`{job="api"} | logfmt`,
		`{job="api"} | unpack`,
	} {
		t.Run(q, func(t *testing.T) {
			t.Parallel()

			proj := logStreamProjection(t, q, schema.DefaultOTelLogs())
			for i, p := range proj.Projections {
				switch p.Expr.(type) {
				case *chplan.LitFloat, *chplan.LitInt:
					t.Errorf("projection slot %d (alias %q) is a numeric literal %T — "+
						"the log-stream shape carries no synthesised numeric column "+
						"(issue #1430)", i, p.Alias, p.Expr)
				}
			}
		})
	}
}

// TestLogStreamSQL_OmitsToFloat64Placeholder pins the same property one layer
// down, in the SQL text ClickHouse receives: the emitted SELECT list neither
// aliases a `Value` column nor wraps anything in the `toFloat64(?)` the
// placeholder literal used to render as.
//
// Pinning the SQL as well as the plan matters because the placeholder was
// invisible at the plan level as anything but a literal — it acquired its
// toFloat64 wrap centrally in the chsql builder, so a plan-only assertion
// would not have described what actually went over the wire.
func TestLogStreamSQL_OmitsToFloat64Placeholder(t *testing.T) {
	t.Parallel()

	proj := logStreamProjection(t, `{job="api"}`, schema.DefaultOTelLogs())
	sqlStr, _, err := chsql.Emit(context.Background(), proj)
	if err != nil {
		t.Fatalf("chsql.Emit: %v", err)
	}
	// Everything before the FROM is the projection list; a `Value` alias
	// anywhere else in the statement (an inner scope, a WHERE) is not this
	// test's business.
	selectList := sqlStr
	if idx := strings.Index(sqlStr, " FROM "); idx > 0 {
		selectList = sqlStr[:idx]
	}
	if strings.Contains(selectList, "AS `Value`") {
		t.Errorf("log-stream SELECT list aliases a `Value` column — the placeholder "+
			"issue #1430 removed:\n%s", sqlStr)
	}
	if strings.Contains(selectList, "toFloat64(") {
		t.Errorf("log-stream SELECT list still contains a toFloat64 wrap — the shape "+
			"the placeholder literal rendered as (issue #1430):\n%s", sqlStr)
	}
}

// TestLogLineColumn_MatchesCursorProbeLiteral pins the emitter side of the
// cross-package pairing the chclient cursor depends on: chclient may not
// import logql (.go-arch-lint.yml gives it no internal dependencies), so it
// duplicates this alias in its own unexported logLineColumn const and keys
// its no-numeric-destination log-row scan off it.
//
// Renaming LogLineColumn without changing that literal would not fail to
// compile — the cursor would simply stop recognising log rows and fall back
// to the four-destination sample scan against a three-column result set,
// surfacing as a scan error on every log query. This assertion turns that
// into a compile-time-adjacent failure at the source of the rename.
func TestLogLineColumn_MatchesCursorProbeLiteral(t *testing.T) {
	t.Parallel()

	const cursorProbeLiteral = "Line" // internal/chclient/cursor.go: logLineColumn
	if logql.LogLineColumn != cursorProbeLiteral {
		t.Errorf("logql.LogLineColumn = %q, but internal/chclient/cursor.go's logLineColumn "+
			"probe constant is %q — the cursor keys its log-row scan off this alias and "+
			"cannot import logql to read it, so the two literals must be changed together",
			logql.LogLineColumn, cursorProbeLiteral)
	}
}
