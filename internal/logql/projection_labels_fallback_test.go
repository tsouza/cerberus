package logql_test

import (
	"context"
	"strings"
	"testing"

	"github.com/tsouza/cerberus/internal/chsql"
	"github.com/tsouza/cerberus/internal/logql"
	"github.com/tsouza/cerberus/internal/schema"
)

// TestProjectSamples_LabelsWalkRejectionFallsBackToTheBareAttributes pins the
// error arm of [logql.Lang.ProjectSamples]'s projection swap.
//
// The swap consults [logql.PipelineLabelsExpr], which re-walks EVERY pipeline
// stage. The lowering does not: [lowerPipelineWithLabels]'s dynamicLabels gate
// skips SQL lowering for an `__error__` label filter that follows a `| pattern`
// stage, because those markers only exist after the Go-side transform runs. So
// a query can lower cleanly and still be rejected by the projection walk — here
// an `ip()` filter whose pattern the walk rejects at that later moment — and the
// projection must fall back to the bare ResourceAttributes column rather than
// project the walk's empty result.
//
// The two queries differ only in the ip() pattern's validity, which isolates
// the arm: the accepted one projects `| logfmt`'s extraction, the rejected one
// does not. Drop the error check and the rejected query projects nothing.
func TestProjectSamples_LabelsWalkRejectionFallsBackToTheBareAttributes(t *testing.T) {
	t.Parallel()

	const (
		walkAccepts = `{a="b"} | logfmt | pattern "<x>" | __error__ = ip("1.2.3.4")`
		walkRejects = `{a="b"} | logfmt | pattern "<x>" | __error__ = ip("zzz")`
	)

	emit := func(q string) string {
		t.Helper()
		l := &logql.Lang{Schema: schema.DefaultOTelLogs()}
		plan, meta, err := l.Parse(context.Background(), q)
		if err != nil {
			t.Fatalf("Parse(%q): %v", q, err)
		}
		sqlStr, _, err := chsql.Emit(context.Background(), l.ProjectSamples(plan, meta))
		if err != nil {
			t.Fatalf("chsql.Emit(%q): %v", q, err)
		}
		return sqlStr
	}

	const logfmtExtraction = "extractKeyValuePairs"

	if got := emit(walkAccepts); !strings.Contains(got, logfmtExtraction) {
		t.Fatalf("a clean labels walk must project `| logfmt`'s %s extraction; got:\n%s", logfmtExtraction, got)
	}
	if got := emit(walkRejects); strings.Contains(got, logfmtExtraction) {
		t.Fatalf("a rejected labels walk must fall back to the bare attributes column, "+
			"not project %s from a result the walk never produced; got:\n%s", logfmtExtraction, got)
	}
}
