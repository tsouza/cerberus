package chsql

import (
	"strings"
	"testing"

	"github.com/tsouza/cerberus/internal/chplan"
)

// CanonicalKeySQL is the exact rendering of CanonicalMap over an inner
// Map expression whose rendering is inner and binds args. It is declared
// in a _test.go file of package chsql so the package's external tests
// can read it as chsql.CanonicalKeySQL without it reaching the binary.
func CanonicalKeySQL(inner string, args ...any) string {
	sorted := chplan.CanonicalMapFunc + "(" + inner + ")"
	alias := selfTypedAlias(sorted, args)
	return "CAST((" + sorted + " AS `" + alias + "`), toTypeName(`" + alias + "`))"
}

// CanonicalKeyOpen is the rendering of CanonicalMap up to the start of
// its inner Map expression.
const CanonicalKeyOpen = "CAST((" + chplan.CanonicalMapFunc + "("

func TestCanonicalMap_Rendering(t *testing.T) {
	t.Parallel()

	sql, args := Render(CanonicalMap(Col("Attributes")))
	want := "CAST((mapSort(`Attributes`) AS `_typed_"
	if !strings.HasPrefix(sql, want) {
		t.Fatalf("CanonicalMap rendered %q, want prefix %q", sql, want)
	}
	if len(args) != 0 {
		t.Fatalf("CanonicalMap over a column bound %v, want no args", args)
	}
	if sql != CanonicalKeySQL("`Attributes`") {
		t.Fatalf("CanonicalMap rendered %q, want %q", sql, CanonicalKeySQL("`Attributes`"))
	}
	alias := sql[len("CAST((mapSort(`Attributes`) AS `"):strings.Index(sql, "`), ")]
	if wantTail := "`), toTypeName(`" + alias + "`))"; !strings.HasSuffix(sql, wantTail) {
		t.Fatalf("CanonicalMap's type argument does not read its own alias %q: %q", alias, sql)
	}
}

// The alias names the operand by content: one expression rendered twice
// (a SELECT key and its GROUP BY twin) declares one alias, while two
// expressions that differ only in a bound argument declare two — the
// same alias over two different expressions in one scope is a
// ClickHouse error.
func TestSelfTypedCast_AliasIsContentAddressed(t *testing.T) {
	t.Parallel()

	keep := func(v string) Frag {
		return Call("mapFilter", Lambda2("k", "v", Eq(BareIdent("k"), Lit(v))), Col("Attributes"))
	}
	first, firstArgs := Render(SelfTypedCast(keep("job")))
	again, againArgs := Render(SelfTypedCast(keep("job")))
	other, _ := Render(SelfTypedCast(keep("dc")))

	if first != again || len(firstArgs) != 1 || len(againArgs) != 1 {
		t.Fatalf("one expression rendered twice differs: %q (%v) vs %q (%v)", first, firstArgs, again, againArgs)
	}
	if first == other {
		t.Fatalf("expressions differing only in a bound argument share an alias: %q", first)
	}
	if strings.Count(first, "?") != 1 {
		t.Fatalf("SelfTypedCast repeated its operand's placeholders: %q", first)
	}
}
