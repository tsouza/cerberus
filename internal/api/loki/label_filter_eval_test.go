package loki

import (
	"testing"

	syntax "github.com/tsouza/cerberus/internal/logql/lsyntax"
)

// TestPostProcess_UnpackThenErrorLabelFilter_DropsMismatches pins the
// fix for the loki-unpack-corpus-coverage compat gap (#1611): a
// `| __error__=""` filter following `| unpack` used to have zero
// effect end-to-end (the lowering pushed a SQL predicate that
// silently matched everything — see internal/logql/lower.go's
// dynamicLabels gate). It must now actually drop rows whose unpacked
// payload errored.
func TestPostProcess_UnpackThenErrorLabelFilter_DropsMismatches(t *testing.T) {
	t.Parallel()

	expr, err := syntax.ParseExpr(`{job="api"} | unpack | __error__=""`)
	if err != nil {
		t.Fatalf("parse: %v", err)
	}
	tx, err := postProcessExtract(expr)
	if err != nil {
		t.Fatalf("extract: %v", err)
	}
	if tx == nil {
		t.Fatalf("expected non-nil transform")
	}

	// A well-formed packed payload: keep.
	_, _, keep := tx(`{"_entry":"hello","category":"auth"}`, 0, map[string]string{"job": "api"})
	if !keep {
		t.Errorf("expected a well-formed packed line to be kept")
	}

	// Not a JSON object at all: unpack stamps __error__="JSONParserErr",
	// so `__error__=""` must drop it.
	_, _, keep = tx(`["not","an","object"]`, 0, map[string]string{"job": "api"})
	if keep {
		t.Errorf("expected a non-object payload to be dropped by __error__=\"\"")
	}

	// Malformed JSON: same story.
	_, _, keep = tx(`{"_entry":"truncated`, 0, map[string]string{"job": "api"})
	if keep {
		t.Errorf("expected a malformed payload to be dropped by __error__=\"\"")
	}
}

// TestPostProcess_UnpackThenErrorLabelFilter_KeepsOnlyErrors is the
// mirror image: `| __error__="JSONParserErr"` must keep ONLY the rows
// unpack rejected — previously it matched none, end-to-end.
func TestPostProcess_UnpackThenErrorLabelFilter_KeepsOnlyErrors(t *testing.T) {
	t.Parallel()

	expr, err := syntax.ParseExpr(`{job="api"} | unpack | __error__="JSONParserErr"`)
	if err != nil {
		t.Fatalf("parse: %v", err)
	}
	tx, err := postProcessExtract(expr)
	if err != nil {
		t.Fatalf("extract: %v", err)
	}

	_, _, keep := tx(`{"_entry":"hello","category":"auth"}`, 0, map[string]string{"job": "api"})
	if keep {
		t.Errorf("expected a well-formed packed line to be dropped")
	}

	_, labels, keep := tx(`["not","an","object"]`, 0, map[string]string{"job": "api"})
	if !keep {
		t.Errorf("expected a non-object payload to be kept")
	}
	if labels[syntax.ErrorLabel] != "JSONParserErr" {
		t.Errorf("expected __error__=JSONParserErr, got %q", labels[syntax.ErrorLabel])
	}
}

// TestPostProcess_OrdinaryLabelFilterAfterUnpack_StillPushedToSQL pins
// that only the __error__ family is re-evaluated in Go — an ordinary
// label name keeps the existing structured-metadata SQL fallback, so
// postProcessExtract must NOT add a Go-side step for it (adding one
// would double-apply the filter against post-unpack labels, changing
// behaviour for a path with pinned SQL-emission fixtures — see
// internal/logql/unpack_pattern_test.go's TestLowerUnpackPattern_NoSQLImpact).
func TestPostProcess_OrdinaryLabelFilterAfterUnpack_StillPushedToSQL(t *testing.T) {
	t.Parallel()

	expr, err := syntax.ParseExpr(`{job="api"} | unpack | level="error"`)
	if err != nil {
		t.Fatalf("parse: %v", err)
	}
	tx, err := postProcessExtract(expr)
	if err != nil {
		t.Fatalf("extract: %v", err)
	}
	if tx == nil {
		t.Fatalf("expected a non-nil transform (unpackStep itself still runs)")
	}

	// A row that would fail `level="error"` still comes through kept —
	// the filter is SQL's job here, not Go's.
	_, _, keep := tx(`{"_entry":"hello","category":"auth"}`, 0, map[string]string{"job": "api", "level": "info"})
	if !keep {
		t.Errorf("expected postProcessExtract to leave ordinary label filters to SQL, not drop rows itself")
	}
}

// TestLabelFiltererEval_String covers the String filter kind directly
// — equality, negation, and regex, including the absent-label ("")
// default.
func TestLabelFiltererEval_String(t *testing.T) {
	t.Parallel()

	cases := []struct {
		name   string
		query  string
		labels map[string]string
		keep   bool
	}{
		{"equal match", `{job="api"} | v="x"`, map[string]string{"v": "x"}, true},
		{"equal mismatch", `{job="api"} | v="x"`, map[string]string{"v": "y"}, false},
		{"equal absent is empty", `{job="api"} | v=""`, map[string]string{}, true},
		{"not-equal", `{job="api"} | v!="x"`, map[string]string{"v": "y"}, true},
		{"regex", `{job="api"} | v=~"x.*"`, map[string]string{"v": "xyz"}, true},
		{"regex mismatch", `{job="api"} | v=~"x.*"`, map[string]string{"v": "abc"}, false},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			t.Parallel()
			lf := parseLabelFilter(t, tc.query)
			keep, _ := labelFiltererEval(lf, tc.labels)
			if keep != tc.keep {
				t.Errorf("labelFiltererEval(%q) = %v, want %v", tc.query, keep, tc.keep)
			}
		})
	}
}

// TestLabelFiltererEval_Binary covers `and`/`or` composition.
func TestLabelFiltererEval_Binary(t *testing.T) {
	t.Parallel()

	andLF := parseLabelFilter(t, `{job="api"} | a="1" , b="2"`)
	if keep, _ := labelFiltererEval(andLF, map[string]string{"a": "1", "b": "2"}); !keep {
		t.Errorf("expected AND to keep when both sides match")
	}
	if keep, _ := labelFiltererEval(andLF, map[string]string{"a": "1", "b": "3"}); keep {
		t.Errorf("expected AND to drop when one side mismatches")
	}

	orLF := parseLabelFilter(t, `{job="api"} | a="1" or b="2"`)
	if keep, _ := labelFiltererEval(orLF, map[string]string{"a": "0", "b": "2"}); !keep {
		t.Errorf("expected OR to keep when either side matches")
	}
	if keep, _ := labelFiltererEval(orLF, map[string]string{"a": "0", "b": "0"}); keep {
		t.Errorf("expected OR to drop when neither side matches")
	}
}

// TestLabelFiltererEval_Numeric covers NumericLabelFilter's three-way
// contract: absent drops, unparseable keeps-with-error-stamp,
// otherwise compares.
func TestLabelFiltererEval_Numeric(t *testing.T) {
	t.Parallel()

	lf := parseLabelFilter(t, `{job="api"} | n > 5`)

	if keep, _ := labelFiltererEval(lf, map[string]string{}); keep {
		t.Errorf("expected an absent numeric label to drop the row")
	}
	if keep, _ := labelFiltererEval(lf, map[string]string{"n": "10"}); !keep {
		t.Errorf("expected 10 > 5 to keep the row")
	}
	if keep, _ := labelFiltererEval(lf, map[string]string{"n": "1"}); keep {
		t.Errorf("expected 1 > 5 to drop the row")
	}
	keep, labels := labelFiltererEval(lf, map[string]string{"n": "not-a-number"})
	if !keep {
		t.Errorf("expected an unparseable numeric value to KEEP the row (error-stamped, not dropped)")
	}
	if labels[syntax.ErrorLabel] != "LabelFilterErr" {
		t.Errorf("expected __error__=LabelFilterErr, got %q", labels[syntax.ErrorLabel])
	}
}

// TestLabelFiltererEval_Duration and TestLabelFiltererEval_Bytes cover
// the other two ordered-comparison filter kinds using the same
// three-way contract as Numeric.
func TestLabelFiltererEval_Duration(t *testing.T) {
	t.Parallel()

	lf := parseLabelFilter(t, `{job="api"} | d > 5s`)
	if keep, _ := labelFiltererEval(lf, map[string]string{}); keep {
		t.Errorf("expected an absent duration label to drop the row")
	}
	if keep, _ := labelFiltererEval(lf, map[string]string{"d": "10s"}); !keep {
		t.Errorf("expected 10s > 5s to keep the row")
	}
	if keep, _ := labelFiltererEval(lf, map[string]string{"d": "1s"}); keep {
		t.Errorf("expected 1s > 5s to drop the row")
	}
	if keep, labels := labelFiltererEval(lf, map[string]string{"d": "not-a-duration"}); !keep || labels[syntax.ErrorLabel] != "LabelFilterErr" {
		t.Errorf("expected an unparseable duration to keep + error-stamp; got keep=%v labels=%v", keep, labels)
	}
}

func TestLabelFiltererEval_Bytes(t *testing.T) {
	t.Parallel()

	lf := parseLabelFilter(t, `{job="api"} | b > 5MB`)
	if keep, _ := labelFiltererEval(lf, map[string]string{}); keep {
		t.Errorf("expected an absent bytes label to drop the row")
	}
	if keep, _ := labelFiltererEval(lf, map[string]string{"b": "10MB"}); !keep {
		t.Errorf("expected 10MB > 5MB to keep the row")
	}
	if keep, _ := labelFiltererEval(lf, map[string]string{"b": "1MB"}); keep {
		t.Errorf("expected 1MB > 5MB to drop the row")
	}
}

// TestLabelFiltererEval_IP covers IPLabelFilter's distinct contract:
// error-carrying rows always kept, absent label always dropped (even
// for !=), otherwise a scan of the label value.
func TestLabelFiltererEval_IP(t *testing.T) {
	t.Parallel()

	lf := parseLabelFilter(t, `{job="api"} | addr = ip("10.0.0.0/8")`)

	if keep, _ := labelFiltererEval(lf, map[string]string{}); keep {
		t.Errorf("expected an absent ip label to drop the row")
	}
	if keep, _ := labelFiltererEval(lf, map[string]string{"addr": "10.1.2.3"}); !keep {
		t.Errorf("expected 10.1.2.3 inside 10.0.0.0/8 to keep the row")
	}
	if keep, _ := labelFiltererEval(lf, map[string]string{"addr": "192.168.1.1"}); keep {
		t.Errorf("expected 192.168.1.1 outside 10.0.0.0/8 to drop the row")
	}
	if keep, _ := labelFiltererEval(lf, map[string]string{syntax.ErrorLabel: "SomeErr", "addr": "192.168.1.1"}); !keep {
		t.Errorf("expected a row already carrying __error__ to be kept unconditionally")
	}
}

// parseLabelFilter parses q and returns the LabelFilterer of its
// (single) label-filter stage.
func parseLabelFilter(t *testing.T, q string) syntax.LabelFilterer {
	t.Helper()
	expr, err := syntax.ParseExpr(q)
	if err != nil {
		t.Fatalf("parse(%q): %v", q, err)
	}
	pipe, ok := expr.(*syntax.PipelineExpr)
	if !ok {
		t.Fatalf("parse(%q): expected *syntax.PipelineExpr, got %T", q, expr)
	}
	for _, st := range pipe.MultiStages {
		if lf, ok := st.(*syntax.LabelFilterExpr); ok {
			return lf.LabelFilterer
		}
	}
	t.Fatalf("parse(%q): no label filter stage found", q)
	return nil
}
