package logql

import (
	"fmt"
	"math"
	"regexp"
	"strconv"
	"strings"
	"testing"
	"time"

	"github.com/tsouza/cerberus/internal/chplan"
	syntax "github.com/tsouza/cerberus/internal/logql/lsyntax"
	"github.com/tsouza/cerberus/internal/schema"
)

// goDurationCorpus is the shared input set for the Go-parity tests:
// every accepted unit (including both non-ASCII spellings of µs),
// fractional / compound / signed shapes, the bare-zero special case,
// the dot-edge shapes Go accepts but CH parseTimeDelta rejects, and
// one representative per Go error class. Overflow shapes (>292y) are
// deliberately absent from THIS corpus — it exercises the validity
// REGEX only ([goSideValid] here checks the regex alone, matching
// [goDurationValidRe]'s own scope); overflow is a separate axis,
// checked by [goDurationOverflowCorpus] and [TestGoDurationOverflowParity]
// against [goDurationOverflows] (cerberus issue #3686).
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

// goDurationOverflowCorpus exercises the int64-nanosecond overflow
// boundary the regex alone cannot see (cerberus issue #3686): values
// from the reported bug report, the exact boundary in both directions,
// per-component overflow via a single huge unit multiple, fractional
// overflow, running-sum overflow across compound components, and an
// integer-part digit run past uint64's own range.
var goDurationOverflowCorpus = []string{
	// from the bug report
	"123456789h", "1h2562047h",
	// the exact int64 nanosecond boundary, both signs
	"2562047h47m16.854775807s", "2562047h47m16.854775808s",
	"-2562047h47m16.854775808s", "-2562047h47m16.854775809s",
	// single-component overflow (integer * unit past 1<<63)
	"9223372036854775808ns", "9223372036854775807ns", "300000h",
	// fractional overflow
	"9223372036854775807.9s",
	// running-sum overflow across otherwise-valid components
	"2562047h47m16s2000000000ns",
	// integer digit run past uint64's own range (20+ digits)
	"99999999999999999999h", "99999999999999999999ns",
}

// goSideOverflows mirrors the SQL-side [goDurationOverflows]: Go's
// time.ParseDuration uint64 accumulator loop, replayed digit group by
// digit group over the same [goDurationComponentRe] captures the SQL
// expression folds over.
func goSideOverflows(raw string) bool {
	stripped := stripSign(raw)
	unitNanos := map[string]uint64{}
	for i, name := range goDurationUnitNames {
		unitNanos[name] = uint64(goDurationUnitNanos[i])
	}
	const pow2_63 = uint64(1) << 63
	var d uint64
	overflow := false
	for _, m := range regexp.MustCompile(goDurationComponentRe).FindAllStringSubmatch(stripped, -1) {
		intPart, fracPart, unitPart := m[1], m[2], m[3]
		unit := unitNanos[unitPart]
		var whole uint64
		if len(intPart) > maxSafeIntegerDigits {
			overflow = true
		} else if intPart != "" {
			whole, _ = strconv.ParseUint(intPart, 10, 64)
		}
		if whole > pow2_63/unit {
			overflow = true
		}
		v := whole * unit

		var fracX uint64
		var scale float64 = 1
		fracOverflow := false
		for _, c := range fracPart {
			if fracOverflow {
				continue
			}
			digit := uint64(c - '0')
			if fracX > goFractionSaturation || (fracX == goFractionSaturation && digit > goFractionSaturationLastDigit) {
				fracOverflow = true
				continue
			}
			fracX = fracX*decimalRadix + digit
			scale *= decimalRadix
		}
		fracNanos := uint64(float64(fracX) * (float64(unit) / scale))
		v += fracNanos
		if v > pow2_63 {
			overflow = true
		}
		d += v
		if d > pow2_63 {
			overflow = true
		}
	}
	if !strings.HasPrefix(raw, "-") && d > math.MaxInt64 {
		overflow = true
	}
	return overflow
}

// TestGoDurationOverflowParity pins [goSideOverflows] against real
// time.ParseDuration over the boundary corpus, and TestDurationSecondsMatchesGo
// (build-tagged chdb) pins the actual emitted SQL the same way.
func TestGoDurationOverflowParity(t *testing.T) {
	t.Parallel()
	for _, in := range goDurationOverflowCorpus {
		stripped := stripSign(in)
		if !regexp.MustCompile(goDurationValidRe).MatchString(stripped) {
			t.Fatalf("corpus bug: %q is not even regex-shaped valid", in)
		}
		_, err := time.ParseDuration(in)
		wantOverflow := err != nil
		if got := goSideOverflows(in); got != wantOverflow {
			t.Errorf("%q: goSideOverflows = %v, time.ParseDuration overflow = %v (err: %v)", in, got, wantOverflow, err)
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
	if !ok || outer.Fn != chplan.FnMapMerge {
		t.Fatalf("final labels = %T (%v); want mapConcat(<labels>, <marks branch>)", labels, labels)
	}
	branch, ok := outer.Args[1].(*chplan.FuncCall)
	if !ok || (branch.Fn != chplan.FnIf && branch.Fn != chplan.FnMultiIf) {
		t.Fatalf("marks branch = %T; want if/multiIf FuncCall", outer.Args[1])
	}
	errMap, ok := branch.Args[1].(*chplan.FuncCall)
	if !ok || errMap.Fn != chplan.FnMap {
		t.Fatalf("marks branch then-arm = %T; want map(...)", branch.Args[1])
	}
	wantKeys := []string{syntax.ErrorLabel, errLabelFilterKind, syntax.ErrorDetailsLabel}
	for i, want := range wantKeys {
		lit, ok := errMap.Args[i].(*chplan.LitString)
		if !ok || lit.V != want {
			t.Fatalf("error map arg %d = %#v; want LitString %q", i, errMap.Args[i], want)
		}
	}
	// The details slot is the three-way Go error classification —
	// a multiIf whose branch literals carry the `time: …` prefixes.
	details, ok := errMap.Args[3].(*chplan.FuncCall)
	if !ok || details.Fn != chplan.FnMultiIf {
		t.Fatalf("error map details arm = %T; want multiIf classification", errMap.Args[3])
	}
	var prefixes []string
	for _, arg := range details.Args {
		if call, ok := arg.(*chplan.FuncCall); ok && call.Fn == chplan.FnConcat {
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
