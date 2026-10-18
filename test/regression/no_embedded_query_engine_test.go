package regression

import (
	"os/exec"
	"strings"
	"testing"
)

// ClickHouse computes every answer cerberus gives (CLAUDE.md invariant 21).
// The binary therefore never links an upstream query engine: one embedded
// engine is a second way of answering queries, which reads raw samples out of
// ClickHouse and evaluates them in the cerberus process. The upstream parsers
// are linked on purpose; the engines run only in the test-only parity oracles.
//
// The scan is over `go list -deps ./cmd/cerberus` rather than a source grep,
// as agpl-clean's is: what matters is what the binary reaches, transitively,
// not which file names an import. It sees only upstream engines: an in-house
// evaluator that computes answers in Go breaks the same invariant and is left
// to review.

// queryEngineModule is one upstream module whose query engine the binary must
// not link. Every package under engine is forbidden, except those under an
// allowed prefix (the parser the lowering is built on).
type queryEngineModule struct {
	engine  string
	allowed []string
}

var forbiddenQueryEngines = []queryEngineModule{
	{
		engine:  "github.com/prometheus/prometheus/promql",
		allowed: []string{"github.com/prometheus/prometheus/promql/parser"},
	},
	{engine: "github.com/prometheus/prometheus/rules"},
	{engine: "github.com/grafana/loki/v3/pkg/logql"},
	{engine: "github.com/grafana/tempo/pkg/traceql"},
}

// binaryParserPackage is linked by every build of the binary; seeing it proves
// the dependency scan resolved the real graph rather than an empty one.
const binaryParserPackage = "github.com/prometheus/prometheus/promql/parser"

func underPackage(pkg, prefix string) bool {
	return pkg == prefix || strings.HasPrefix(pkg, prefix+"/")
}

// linksQueryEngine reports the forbidden engine module pkg belongs to, if any.
func linksQueryEngine(pkg string) (string, bool) {
	for _, m := range forbiddenQueryEngines {
		if !underPackage(pkg, m.engine) {
			continue
		}
		allowed := false
		for _, a := range m.allowed {
			if underPackage(pkg, a) {
				allowed = true
			}
		}
		if !allowed {
			return m.engine, true
		}
	}
	return "", false
}

func TestBinaryLinksNoQueryEngine(t *testing.T) {
	t.Parallel()

	cmd := exec.Command("go", "list", "-deps", "./cmd/cerberus")
	cmd.Dir = repoRoot
	out, err := cmd.Output()
	if err != nil {
		t.Fatalf("go list -deps ./cmd/cerberus: %v", err)
	}

	sawParser := false
	for pkg := range strings.SplitSeq(strings.TrimSpace(string(out)), "\n") {
		pkg = strings.TrimSpace(pkg)
		if pkg == binaryParserPackage {
			sawParser = true
		}
		if engine, bad := linksQueryEngine(pkg); bad {
			t.Errorf("./cmd/cerberus links %s, part of the query engine %s. ClickHouse computes every "+
				"answer (CLAUDE.md invariant 21): a query the lowering cannot answer exactly is a "+
				"lowering bug to fix, never a reason to evaluate it in-process.", pkg, engine)
		}
	}
	if !sawParser {
		t.Fatalf("the dependency scan never saw %s, which the binary always links; it inspected nothing", binaryParserPackage)
	}
}

func TestLinksQueryEngineClassifies(t *testing.T) {
	t.Parallel()

	cases := map[string]bool{
		"github.com/prometheus/prometheus/promql":                 true,
		"github.com/prometheus/prometheus/promql/promqltest":      true,
		"github.com/prometheus/prometheus/rules":                  true,
		"github.com/grafana/loki/v3/pkg/logql":                    true,
		"github.com/grafana/loki/v3/pkg/logql/syntax":             true,
		"github.com/grafana/tempo/pkg/traceql":                    true,
		"github.com/prometheus/prometheus/promql/parser":          false,
		"github.com/prometheus/prometheus/promql/parser/posrange": false,
		"github.com/prometheus/prometheus/promqlx":                false,
		"github.com/prometheus/prometheus/storage":                false,
		"github.com/prometheus/prometheus/model/labels":           false,
	}
	for pkg, want := range cases {
		if _, got := linksQueryEngine(pkg); got != want {
			t.Errorf("linksQueryEngine(%q) = %v, want %v", pkg, got, want)
		}
	}
}
