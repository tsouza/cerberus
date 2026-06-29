//go:build agpl_oracle

// A/B oracle: the in-house jsonPathParse must agree with the upstream
// AGPL grafana/loki jsonexpr parser on both the parsed segment list and
// accept/reject, for every JSON path in the corpus.
//
// This is the ONLY file that imports the AGPL jsonexpr package, gated
// behind the `agpl_oracle` build tag so neither the production build nor
// the default test run links it. Run explicitly with:
//
//	CGO_ENABLED=1 go test -tags agpl_oracle ./internal/logql/
package logql

import (
	"reflect"
	"testing"

	"github.com/grafana/loki/v3/pkg/logql/log/jsonexpr"
)

func TestJSONPathParse_MatchesLokiJSONExpr(t *testing.T) {
	corpus := []string{
		// bare fields + dot chains
		"foo",
		"foo.bar",
		"foo.bar.baz",
		"response.code",
		"a.b.c.d.e",
		"_underscore",
		"mixed_1.field2",
		// bracket key access
		`foo["bar"]`,
		`["foo"]`,
		`["foo"]["bar"]`,
		`foo["bar"]["baz"]`,
		`["foo"].bar`,
		`foo["bar with spaces"]`,
		`foo["dot.in.key"]`,
		// index access
		"foo[0]",
		"foo[42]",
		"[0]",
		"[0][1]",
		"foo[0].bar",
		"foo[0][1].baz",
		"items[10].name",
		// mixed
		`a["b"][0].c["d"]`,
		`data[0]["key"].value`,
		// invalid — both must reject
		"",
		".foo",
		"foo.",
		"foo..bar",
		"[",
		"]",
		"foo[",
		"foo[]",
		"foo[1.5]",
		"foo[bar]",
		"foo bar",
		"123abc",
		"foo[-1]",
		"@invalid",
	}

	for _, path := range corpus {
		path := path
		t.Run(path, func(t *testing.T) {
			want, wantErr := jsonexpr.Parse(path, false)
			got, gotErr := jsonPathParse(path)

			if (wantErr == nil) != (gotErr == nil) {
				t.Fatalf("accept/reject mismatch for %q: loki err=%v, in-house err=%v (loki=%v in-house=%v)",
					path, wantErr, gotErr, want, got)
			}
			if wantErr != nil {
				return // both rejected — segment comparison not meaningful
			}
			if !reflect.DeepEqual(normalizeSegments(want), normalizeSegments(got)) {
				t.Fatalf("segment mismatch for %q: loki=%#v in-house=%#v", path, want, got)
			}
		})
	}
}

// normalizeSegments coerces both segment lists to a comparable shape:
// loki returns []interface{} with string/int elements, identical to the
// in-house []any, so this only guards against nil-vs-empty differences.
func normalizeSegments(segs []any) []any {
	if len(segs) == 0 {
		return nil
	}
	return segs
}
