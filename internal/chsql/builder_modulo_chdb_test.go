//go:build chdb

// chDB-backed pin for the PromQL `%` operator emit. PromQL's binary
// modulo follows Go's `math.Mod` (IEEE 754 remainder via Plauger
// iteration), not the naive `x - y*trunc(x/y)` formula. ClickHouse's
// `%` operator and `modulo(...)` / `moduloLegacy(...)` / `fmod(...)`
// (which doesn't exist; see #400 Bucket 2 audit) all implement the
// truncation form — at scale `>=2^30` the rounding error in
// `y*trunc(x/y)` cancels visibly against `x`, producing exactly 0
// where Go's iterative algorithm preserves a small residual. The
// post-E-wave audit (docs/compat-residual-audit-postE.md) flagged
// two compat queries hitting this:
//
//	demo_memory_usage_bytes % 1.2345
//	demo_memory_usage_bytes % (1 * 2 + 4 / 6 - 10)        // == -22/3 = -7.333...
//
// This test pins the chsql emit by:
//
//  1. Building a `chplan.Binary{OpMod, L, R}` over float literals.
//  2. Emitting it through chsql.Builder, capturing the parameterised
//     SQL and the args slice.
//  3. Executing the resulting `SELECT <expr>` against an ephemeral
//     chDB session.
//  4. Asserting bit-exact equality against Go's `math.Mod(L, R)`.
//
// The pin covers the audit's failing pair plus a corpus of random
// (x, y) inputs sampled across a wide dynamic range, exercising the
// special cases (NaN, ±Inf, zero) the algorithm has to short-circuit.
//
// Gated by `//go:build chdb` so the default `check` lane (CGO off,
// no libchdb.so) skips it; the dedicated `chdb` workflow runs it.
package chsql_test

import (
	"database/sql"
	"math"
	"math/rand"
	"testing"

	_ "github.com/chdb-io/chdb-go/chdb/driver"

	"github.com/tsouza/cerberus/internal/chplan"
	"github.com/tsouza/cerberus/internal/chsql"
)

// TestEmitGoModulo_BitExactVsGo runs ~100 (x, y) pairs through the
// chsql modulo emit and compares each result against `math.Mod(x, y)`
// at the bit level. Mismatches in the special-case lanes (NaN/Inf/0)
// are treated as test failures except where both sides are NaN
// (different NaN payloads still mean "math says NaN").
//
// The corpus mixes:
//
//   - The two audit-failure pairs (demo_memory_usage_bytes %
//     {1.2345, -22/3}) at a synthetic 2.15e9 magnitude.
//   - Cancellation-prone cases (`22*N % (-22/3)`) where the naive CH
//     formula gives exactly 0 — these are the "Bucket 2 sub-pattern
//     B" cases the audit called out.
//   - Special-value matrix (0, ±Inf, NaN on either side).
//   - Random (x, y) sampled with mantissa + exponent variation.
func TestEmitGoModulo_BitExactVsGo(t *testing.T) {
	db, err := sql.Open("chdb", "")
	if err != nil {
		t.Fatalf("open chdb: %v", err)
	}
	t.Cleanup(func() { _ = db.Close() })
	if err := db.Ping(); err != nil {
		t.Fatalf("ping chdb: %v", err)
	}

	type pair struct{ x, y float64 }
	cases := []pair{
		// Audit's exact failure pair (#400 Bucket 2). The dividend
		// proxies demo_memory_usage_bytes at its compat-fixture scale.
		{2.15e9, 1.2345},
		{2.15e9, -22.0 / 3.0},

		// Cancellation-to-0 fixtures from the audit: any multiple of 22
		// against y = -22/3 trips the naive `x - y*trunc(x/y)` to
		// exactly 0; Plauger preserves the float64 residual.
		{66, -22.0 / 3.0},
		{726, -22.0 / 3.0},
		{7326, -22.0 / 3.0},
		{73326, -22.0 / 3.0},
		{733326, -22.0 / 3.0},
		{7333326, -22.0 / 3.0},

		// Power-of-2 dividends from the audit's other affected query.
		{2147483648.0, 1.2345},
		{2147483648.0, -22.0 / 3.0},
		{1073741824.0, 1.2345},
		{1073741824.0, -22.0 / 3.0},

		// Special-value matrix.
		{0, 1.0},
		{0, -1.0},
		{1.0, 0},                // NaN per math.Mod
		{math.Inf(+1), 1.0},     // NaN per math.Mod
		{math.Inf(-1), 1.0},     // NaN per math.Mod
		{1.0, math.Inf(+1)},     // = x
		{1.0, math.Inf(-1)},     // = x
		{math.NaN(), 1.0},       // NaN
		{1.0, math.NaN()},       // NaN
		{math.NaN(), math.NaN()}, // NaN

		// Small / common shapes.
		{1.5, 1.0},
		{-1.5, 1.0},
		{1.5, -1.0},
		{-1.5, -1.0},
		{math.Pi, math.E},
		{-math.Pi, math.E},
		{math.Pi, -math.E},
	}

	// Random pairs across a wide dynamic range. Seeded so the corpus
	// is deterministic per CI run.
	r := rand.New(rand.NewSource(0xC0FFEE))
	for i := 0; i < 60; i++ {
		x := (r.Float64() - 0.5) * math.Pow(2, float64(r.Intn(60)-30))
		y := (r.Float64() - 0.5) * math.Pow(2, float64(r.Intn(60)-30))
		cases = append(cases, pair{x, y})
	}

	exprL := &chplan.LitFloat{V: 0}
	exprR := &chplan.LitFloat{V: 0}
	bin := &chplan.Binary{Op: chplan.OpMod, Left: exprL, Right: exprR}

	var mismatches int
	for _, tc := range cases {
		exprL.V = tc.x
		exprR.V = tc.y
		bld := chsql.NewBuilder()
		if err := bld.Expr(bin); err != nil {
			t.Errorf("emit (x=%v, y=%v): %v", tc.x, tc.y, err)
			continue
		}
		sqlStr, args := bld.Build()
		query := "SELECT " + sqlStr
		var v float64
		if err := db.QueryRow(query, args...).Scan(&v); err != nil {
			t.Errorf("query (x=%v, y=%v): %v\nSQL: %s", tc.x, tc.y, err, query)
			continue
		}
		want := math.Mod(tc.x, tc.y)
		bothNaN := math.IsNaN(v) && math.IsNaN(want)
		bitEq := math.Float64bits(v) == math.Float64bits(want)
		if !bothNaN && !bitEq {
			mismatches++
			t.Errorf("x=%v y=%v: chDB=%.20e (bits 0x%016x), math.Mod=%.20e (bits 0x%016x), diff=%g",
				tc.x, tc.y, v, math.Float64bits(v), want, math.Float64bits(want), v-want)
		}
	}
	if mismatches > 0 {
		t.Logf("Plauger emit drift on %d/%d cases — see failures above", mismatches, len(cases))
	}
}
