package chsql

import (
	"go/ast"
	"go/parser"
	"go/token"
	"os"
	"strconv"
	"strings"
	"testing"

	"github.com/tsouza/cerberus/internal/chplan"
)

// TestEmittersSpellSharedOutputNamesThroughChplan pins that an output name a
// node's RowType() claims is spelled in this package only through its chplan
// constant, never as a string literal of its own. A literal is a second
// spelling, and two spellings of one name is how a RowType() claim and the
// alias an emitter renders drift apart. Comments are free to quote the name;
// the walk looks at string literals in code only.
//
// The names checked are the ones with a chplan constant whose text is
// distinctive enough to have no other legitimate meaning in this package.
// Four constants are NOT checked, and their spelling is therefore reviewer
// discipline rather than this test's: DefaultSampleTimestampColumn ("TimeUnix")
// is also the OTel-CH physical timestamp column and the Exemplars nested
// field (query_exemplars.go reads `Exemplars.TimeUnix`); DefaultSampleValueColumn
// ("Value"), MetricsCompareAttrColumn ("attr") and MetricsCompareValColumn ("val") likewise
// name physical columns and exemplar fields. A literal of any of those is not
// necessarily a second spelling of the output name, so the walk cannot tell
// a drift from a physical reference.
func TestEmittersSpellSharedOutputNamesThroughChplan(t *testing.T) {
	t.Parallel()

	shared := map[string]string{
		chplan.RangeWindowAnchorColumn:       "chplan.RangeWindowAnchorColumn",
		chplan.MetricsBucketColumn:           "chplan.MetricsBucketColumn",
		chplan.MetricsMultiQuantilePhiColumn: "chplan.MetricsMultiQuantilePhiColumn",
		chplan.MetricsCompareSelectionColumn: "chplan.MetricsCompareSelectionColumn",
		chplan.RangeLWRSampleTimestampColumn: "chplan.RangeLWRSampleTimestampColumn",
	}
	entries, err := os.ReadDir(".")
	if err != nil {
		t.Fatal(err)
	}
	fset := token.NewFileSet()
	var offenders []string
	for _, entry := range entries {
		name := entry.Name()
		if entry.IsDir() || !strings.HasSuffix(name, ".go") || strings.HasSuffix(name, "_test.go") {
			continue
		}
		file, err := parser.ParseFile(fset, name, nil, 0)
		if err != nil {
			t.Fatalf("parse %s: %v", name, err)
		}
		ast.Inspect(file, func(n ast.Node) bool {
			lit, ok := n.(*ast.BasicLit)
			if !ok || lit.Kind != token.STRING {
				return true
			}
			text, err := strconv.Unquote(lit.Value)
			if err != nil {
				return true
			}
			if constant, shared := shared[text]; shared {
				offenders = append(offenders, fset.Position(lit.Pos()).String()+" spells "+lit.Value+" instead of "+constant)
			}
			return true
		})
	}
	if len(offenders) > 0 {
		t.Fatalf("%d literal(s) re-spell an output name chplan already names:\n  %s",
			len(offenders), strings.Join(offenders, "\n  "))
	}
}
