package steps

import (
	"testing"

	"github.com/tsouza/cerberus/internal/migrate"
	"github.com/tsouza/cerberus/test/e2e/migration/lib"
)

// leakedEnv is what a regression in run()'s environment wiring would hand a
// child process: no blackholed proxy, no closed bypass list — a route out.
// It carries just enough to be a plausible child environment (PATH) while
// failing every one of lib.RequireOffline's checks.
var leakedEnv = []string{"PATH=/usr/bin"}

// TestThenHarvestRanOfflineDetectsALeakedEnv proves the "contacts no network
// endpoint" step asserts against the environment the harvest command for that
// archetype actually ran under (w.envUsed), not a freshly re-derived
// lib.OfflineEnv() that would agree with itself regardless of what run()
// wired into the subprocess. Before threading w.envUsed through, this step
// passed unconditionally no matter what the command actually received.
func TestThenHarvestRanOfflineDetectsALeakedEnv(t *testing.T) {
	w := NewWorld(t.TempDir(), "")
	w.archetypes = []string{"a"}
	w.corpus = map[string]migrate.Corpus{
		"a": {Queries: []migrate.CorpusQuery{
			{Expr: "up", Source: "rule:a.yml/g/n", Kind: migrate.KindRecord, Lang: migrate.LangPromQL},
		}},
	}

	w.envUsed = map[string][]string{"a": leakedEnv}
	if err := w.thenHarvestRanOffline(); err == nil {
		t.Fatal("thenHarvestRanOffline passed against an environment that never blackholed the network")
	}

	w.envUsed = map[string][]string{"a": lib.OfflineEnv()}
	if err := w.thenHarvestRanOffline(); err != nil {
		t.Fatalf("thenHarvestRanOffline rejected a genuinely offline environment: %v", err)
	}
}

// TestThenExplainRanOfflineDetectsALeakedEnv is the explain-side twin of
// TestThenHarvestRanOfflineDetectsALeakedEnv: the MIG-05 "contacts no network
// endpoint" step must fail against the environment the explain command for
// that archetype actually ran under, not a self-consistent freshly-built one.
func TestThenExplainRanOfflineDetectsALeakedEnv(t *testing.T) {
	w := NewWorld(t.TempDir(), "")
	w.archetypes = []string{"a"}
	w.explain = map[string]explainReport{
		"a": {Queries: []explainQuery{
			{Kind: migrate.KindRecord, Source: "rule:a.yml/g/n", Expr: "up", SQL: "SELECT 1"},
		}},
	}

	w.envUsed = map[string][]string{"a": leakedEnv}
	if err := w.thenExplainRanOffline(); err == nil {
		t.Fatal("thenExplainRanOffline passed against an environment that never blackholed the network")
	}

	w.envUsed = map[string][]string{"a": lib.OfflineEnv()}
	if err := w.thenExplainRanOffline(); err != nil {
		t.Fatalf("thenExplainRanOffline rejected a genuinely offline environment: %v", err)
	}
}

// TestResultCarriesTheEnvItActuallyRan proves lib.Run echoes back spec.Env on
// its Result rather than only using it to start the process, which is what
// lets a caller assert lib.RequireOffline against what a completed command
// actually received.
func TestResultCarriesTheEnvItActuallyRan(t *testing.T) {
	want := []string{"PATH=/usr/bin", "FOO=bar"}
	res, err := lib.Run(lib.RunSpec{Bin: "/bin/true", Env: want})
	if err != nil {
		t.Fatalf("run /bin/true: %v", err)
	}
	if len(res.Env) != len(want) {
		t.Fatalf("Result.Env = %v, want %v", res.Env, want)
	}
	for i := range want {
		if res.Env[i] != want[i] {
			t.Fatalf("Result.Env = %v, want %v", res.Env, want)
		}
	}
}
