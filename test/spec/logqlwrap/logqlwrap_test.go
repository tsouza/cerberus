package logqlwrap

import (
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/tsouza/cerberus/internal/chplan"
	"github.com/tsouza/cerberus/test/spec"
)

func TestReconstructLogStreamWrapPlan(t *testing.T) {
	c := loadCase(t, "-- query.logql --\n{job=\"api\"} |= \"error\"\n")
	plan, ok, err := ReconstructLogStreamWrapPlan(c)
	if err != nil {
		t.Fatalf("ReconstructLogStreamWrapPlan: %v", err)
	}
	if !ok {
		t.Fatal("ReconstructLogStreamWrapPlan declined a log-stream query")
	}
	project, ok := plan.(*chplan.Project)
	if !ok {
		t.Fatalf("wrapped plan = %T, want *chplan.Project", plan)
	}
	want := []string{"Line", "Attributes", "TimeUnix", "Metadata"}
	if len(project.Projections) != len(want) {
		t.Fatalf("projection width = %d, want %d", len(project.Projections), len(want))
	}
	for i, projection := range project.Projections {
		if projection.Alias != want[i] {
			t.Errorf("projection[%d] alias = %q, want %q", i, projection.Alias, want[i])
		}
	}
}

func TestReconstructLogStreamWrapDeclinesMetricQueries(t *testing.T) {
	c := loadCase(t, "-- query.logql --\nrate({job=\"api\"}[5m])\n")
	if _, _, ok, err := ReconstructLogStreamWrap(c); err != nil || ok {
		t.Fatalf("ReconstructLogStreamWrap(metric) = ok %v, err %v; want false, nil", ok, err)
	}
}

func TestReconstructLogStreamWrapThreadsFixtureWindow(t *testing.T) {
	c := loadCase(t, "-- query.logql --\n{job=\"api\"}\n-- start --\n2026-01-01T00:00:00Z\n-- end --\n2026-01-01T00:05:00Z\n")
	sqlStr, _, ok, err := ReconstructLogStreamWrap(c)
	if err != nil {
		t.Fatalf("ReconstructLogStreamWrap: %v", err)
	}
	if !ok {
		t.Fatal("ReconstructLogStreamWrap declined a log-stream query")
	}
	if !strings.Contains(sqlStr, "`Timestamp` >=") || !strings.Contains(sqlStr, "`Timestamp` <=") {
		t.Fatalf("wrapped SQL omitted the fixture window:\n%s", sqlStr)
	}
}

func loadCase(t *testing.T, body string) *spec.Case {
	t.Helper()
	path := filepath.Join(t.TempDir(), "case.txtar")
	if err := os.WriteFile(path, []byte(body), 0o600); err != nil {
		t.Fatalf("write fixture: %v", err)
	}
	c, err := spec.Load(path)
	if err != nil {
		t.Fatalf("spec.Load: %v", err)
	}
	return c
}
