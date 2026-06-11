package logql

import (
	"fmt"
	"regexp"
	"strings"
	"testing"
	"time"

	"github.com/grafana/loki/v3/pkg/logqlmodel"

	"github.com/tsouza/cerberus/internal/chplan"
	"github.com/tsouza/cerberus/internal/schema"
)

// goDurationCorpus is the shared input set for the Go-parity tests:
// every accepted unit (including both non-ASCII spellings of µs),
// fractional / compound / signed shapes, the bare-zero special case,
// the dot-edge shapes Go accepts but CH parseTimeDelta rejects, and
// one representative per Go error class. Overflow shapes (>292y) are
// deliberately absent — Go rejects them at integer-multiply time,
// which a regex can't see; the SQL lowering treats them as valid and
// returns the float seconds (documented divergence in duration.go).
var goDurationCorpus = []string{
	// valid — one per unit
	"1ns", "12us", "291.792µs", "5μs", "200ms", "1s", "1m", "1h",
	// valid — fractional / compound / signed / zero / dot edges
	"1.5s", "1m30s", "1h2m3.5s", "-1.5h", "+2m", "-1s", "0", "-0", "+0",
	".5s", "1.s", "1m.5s", "0.5s", "0s",
	// invalid — one per Go error class plus edge shapes
	"", "-", "+", "abc", "x5s", ".s", "-.s", "..5s", "1..5s",
	"5", "00", "5.5.5s", "5..", "1m5",
	"5x", "5µx", "5sx", "5msx", "1m5x", "1m x", "5.x", "5 s", "5S", "5MS",
	"infinity", "1e3s", "+-5s", "--5s",
}

// stripSign mirrors the SQL-side `replaceRegexpAll(v, '^[+-]', ”)`.
func stripSign(s string) string {
	if s != "" && (s[0] == '-' || s[0] == '+') {
		return s[1:]
	}
	return s
}

// goSideValid mirrors the SQL validity expression: bare "0" after sign
// strip, or a full match of goDurationValidRe. Go's regexp package is
// RE2 — the same engine ClickHouse's match() uses — so a pass here is
// a pass CH-side byte-for-byte (the chdb spec fixtures double-check
// the end-to-end SQL against live execution).
func goSideValid(raw string) bool {
	stripped := stripSign(raw)
	if stripped == "0" {
		return true
	}
	return regexp.MustCompile(goDurationValidRe).MatchString(stripped)
}

// goSideDetails mirrors the SQL classification multiIf: missing-unit,
// then unknown-unit, then invalid.
func goSideDetails(raw string) string {
	stripped := stripSign(raw)
	if regexp.MustCompile(goDurationMissingUnitRe).MatchString(stripped) {
		return fmt.Sprintf("time: missing unit in duration %q", raw)
	}
	if regexp.MustCompile(goDurationUnknownUnitRe).MatchString(stripped) {
		m := regexp.MustCompile(goDurationUnknownUnitExtractRe).FindStringSubmatch(stripped)
		if len(m) == 2 {
			return fmt.Sprintf("time: unknown unit %q in duration %q", m[1], raw)
		}
	}
	return fmt.Sprintf("time: invalid duration %q", raw)
}

// TestGoDurationRegexParity pins the validity regex against the
// reference implementation itself: reference Loki's duration label
// filters and `unwrap duration(...)` both call Go's time.ParseDuration
// (pkg/logql/log/label_filter.go, pkg/logql/log/metrics_extraction.go),
// so the SQL-side gate must accept exactly what it accepts.
func TestGoDurationRegexParity(t *testing.T) {
	t.Parallel()
	for _, in := range goDurationCorpus {
		_, err := time.ParseDuration(in)
		if got, want := goSideValid(in), err == nil; got != want {
			t.Errorf("validity mismatch for %q: regex gate says %v, time.ParseDuration says %v (err: %v)", in, got, want, err)
		}
	}
}

// TestGoDurationErrorDetailsParity pins the three-way error
// classification against Go's actual error strings. Reference Loki
// surfaces err.Error() verbatim in the `__error_details__` label
// (lbs.SetErrorDetails(err.Error())), so the SQL-side message must be
// byte-identical for the classes it claims to replicate. Inputs whose
// quoted form Go hex-escapes (non-ASCII bytes, time.quote's `\xc2\xb5`
// shape) are exempted — duration.go documents that divergence.
func TestGoDurationErrorDetailsParity(t *testing.T) {
	t.Parallel()
	isASCII := func(s string) bool {
		for i := 0; i < len(s); i++ {
			if s[i] >= 0x80 {
				return false
			}
		}
		return true
	}
	for _, in := range goDurationCorpus {
		_, err := time.ParseDuration(in)
		if err == nil || !isASCII(in) {
			continue
		}
		if got, want := goSideDetails(in), err.Error(); got != want {
			t.Errorf("details mismatch for %q:\n  regex classification: %s\n  time.ParseDuration:   %s", in, got, want)
		}
	}
}

// TestGoDurationRegexParity_GeneratedValid sweeps generated VALID
// durations — every unit crossed with integer / fractional / signed
// shapes and two-component compounds — through the same parity check,
// so a regex regression can't hide behind the hand-picked corpus.
func TestGoDurationRegexParity_GeneratedValid(t *testing.T) {
	t.Parallel()
	units := []string{"ns", "us", "µs", "μs", "ms", "s", "m", "h"}
	numbers := []string{"1", "0", "37", "1.5", "0.25", ".5", "2."}
	signs := []string{"", "-", "+"}
	for _, sign := range signs {
		for _, n1 := range numbers {
			for _, u1 := range units {
				one := sign + n1 + u1
				if _, err := time.ParseDuration(one); err != nil {
					t.Fatalf("generator bug: %q should be Go-valid, got %v", one, err)
				}
				if !goSideValid(one) {
					t.Errorf("regex gate rejects Go-valid %q", one)
				}
				compound := one + "30s"
				if _, err := time.ParseDuration(compound); err != nil {
					t.Fatalf("generator bug: %q should be Go-valid, got %v", compound, err)
				}
				if !goSideValid(compound) {
					t.Errorf("regex gate rejects Go-valid %q", compound)
				}
			}
		}
	}
}

// TestDurationLabelFilterExpr_ReferenceSemantics pins the lowered
// predicate / mark structure for `| dur > 5s` against the reference
// per-row contract (pkg/logql/log/label_filter.go):
//
//   - multiIf(NOT exists, false, NOT valid, true, compare) — absent
//     label drops the row, unparseable value KEEPS it.
//   - the mark fires exactly when the label exists and the value is
//     unparseable, and stamps LabelFilterErr.
func TestDurationLabelFilterExpr_ReferenceSemantics(t *testing.T) {
	t.Parallel()
	expr, err := ParseExprPermissive(`{job="api"} | logfmt | duration > 5s`)
	if err != nil {
		t.Fatalf("parse: %v", err)
	}
	labels, err := PipelineLabelsExpr(expr, schema.DefaultOTelLogs())
	if err != nil {
		t.Fatalf("PipelineLabelsExpr: %v", err)
	}
	// The final labels map must carry the conditional __error__ stamp:
	// a mapConcat whose second arg is the marks branch.
	outer, ok := labels.(*chplan.FuncCall)
	if !ok || outer.Name != "mapConcat" {
		t.Fatalf("final labels = %T (%v); want mapConcat(<labels>, <marks branch>)", labels, labels)
	}
	branch, ok := outer.Args[1].(*chplan.FuncCall)
	if !ok || (branch.Name != "if" && branch.Name != "multiIf") {
		t.Fatalf("marks branch = %T; want if/multiIf FuncCall", outer.Args[1])
	}
	errMap, ok := branch.Args[1].(*chplan.FuncCall)
	if !ok || errMap.Name != "map" {
		t.Fatalf("marks branch then-arm = %T; want map(...)", branch.Args[1])
	}
	wantKeys := []string{logqlmodel.ErrorLabel, errLabelFilterKind, logqlmodel.ErrorDetailsLabel}
	for i, want := range wantKeys {
		lit, ok := errMap.Args[i].(*chplan.LitString)
		if !ok || lit.V != want {
			t.Fatalf("error map arg %d = %#v; want LitString %q", i, errMap.Args[i], want)
		}
	}
	// The details slot is the three-way Go error classification —
	// a multiIf whose branch literals carry the `time: …` prefixes.
	details, ok := errMap.Args[3].(*chplan.FuncCall)
	if !ok || details.Name != "multiIf" {
		t.Fatalf("error map details arm = %T; want multiIf classification", errMap.Args[3])
	}
	var prefixes []string
	for _, arg := range details.Args {
		if call, ok := arg.(*chplan.FuncCall); ok && call.Name == "concat" {
			if lit, ok := call.Args[0].(*chplan.LitString); ok {
				prefixes = append(prefixes, lit.V)
			}
		}
	}
	for _, want := range []string{
		`time: missing unit in duration "`,
		`time: unknown unit "`,
		`time: invalid duration "`,
	} {
		found := false
		for _, p := range prefixes {
			if strings.HasPrefix(p, want) || p == want {
				found = true
			}
		}
		if !found {
			t.Errorf("details classification missing branch with prefix %q (got %v)", want, prefixes)
		}
	}
}
