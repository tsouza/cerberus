//go:build agpl_oracle

// A/B oracle: the in-house logpattern package must agree with the
// upstream AGPL grafana/loki pkg/logql/log/pattern on New / ParseLineFilter
// / ParseLiterals (accept/reject + names + literal runs) and on
// Matches / Test output for a line corpus.
//
// This is the ONLY file that imports the AGPL pattern package, gated
// behind the `agpl_oracle` build tag. Run with:
//
//	CGO_ENABLED=1 go test -tags agpl_oracle ./internal/logql/logpattern/
package logpattern

import (
	"bytes"
	"reflect"
	"testing"

	lokipattern "github.com/grafana/loki/v3/pkg/logql/log/pattern"
)

var patternCorpus = []string{
	"<ip> <_> <method> <path>",
	"<_> <method> <_>",
	"<foo>",
	"<_>",
	"GET <path> <status>",
	"<a><b>",           // consecutive captures (invalid for New + line filter)
	"<a> <a>",          // duplicate names (invalid for New)
	"no captures here", // no capture
	"",
	"literal <_> tail",
	"<ip>:<port>",
	"prefix<_>suffix",
	"<1bad>", // digit-leading name -> '<' is literal
	"<a-b>",  // dash -> '<' is literal
	"<unterminated",
	"plain text",
	"a<_>b<_>c",
	"<method> <_> <code>",
}

func TestPattern_New_MatchesLoki(t *testing.T) {
	for _, p := range patternCorpus {
		p := p
		t.Run(p, func(t *testing.T) {
			want, wantErr := lokipattern.New(p)
			got, gotErr := New(p)
			if (wantErr == nil) != (gotErr == nil) {
				t.Fatalf("New(%q) accept/reject mismatch: loki err=%v in-house err=%v", p, wantErr, gotErr)
			}
			if wantErr != nil {
				return
			}
			if !reflect.DeepEqual(want.Names(), got.Names()) {
				t.Fatalf("New(%q) names mismatch: loki=%v in-house=%v", p, want.Names(), got.Names())
			}
		})
	}
}

func TestPattern_ParseLineFilter_MatchesLoki(t *testing.T) {
	for _, p := range patternCorpus {
		p := p
		t.Run(p, func(t *testing.T) {
			_, wantErr := lokipattern.ParseLineFilter([]byte(p))
			_, gotErr := ParseLineFilter([]byte(p))
			if (wantErr == nil) != (gotErr == nil) {
				t.Fatalf("ParseLineFilter(%q) accept/reject mismatch: loki err=%v in-house err=%v", p, wantErr, gotErr)
			}
		})
	}
}

func TestPattern_ParseLiterals_MatchesLoki(t *testing.T) {
	for _, p := range patternCorpus {
		p := p
		t.Run(p, func(t *testing.T) {
			want, wantErr := lokipattern.ParseLiterals(p)
			got, gotErr := ParseLiterals(p)
			if (wantErr == nil) != (gotErr == nil) {
				t.Fatalf("ParseLiterals(%q) accept/reject mismatch: loki=%v in-house=%v", p, wantErr, gotErr)
			}
			if wantErr != nil {
				return
			}
			if len(want) != len(got) {
				t.Fatalf("ParseLiterals(%q) count mismatch: loki=%d in-house=%d", p, len(want), len(got))
			}
			for i := range want {
				if !bytes.Equal(want[i], got[i]) {
					t.Fatalf("ParseLiterals(%q)[%d] mismatch: loki=%q in-house=%q", p, i, want[i], got[i])
				}
			}
		})
	}
}

var lineCorpus = [][]byte{
	[]byte("192.168.0.1 - GET /api/users"),
	[]byte("GET /api/users 200"),
	[]byte("foo bar baz"),
	[]byte(""),
	[]byte("10.0.0.5:8080"),
	[]byte("prefixVALUEsuffix"),
	[]byte("abab"),
	[]byte("no match for this line"),
	[]byte("method GET code 200"),
}

func TestPattern_MatchesAndTest_MatchLoki(t *testing.T) {
	// Patterns valid for New (have captures, no consecutive/dup) — only
	// these build a usable Matcher on both sides.
	// Patterns with at least one NAMED capture (New rejects unnamed-only).
	valid := []string{
		"<ip> <_> <method> <path>",
		"<method> <_> <code>",
		"<foo>",
		"<ip>:<port>",
		"prefix<val>suffix",
		"<a>ab",
	}
	for _, p := range valid {
		lm, err := lokipattern.New(p)
		if err != nil {
			t.Fatalf("loki New(%q) unexpectedly errored: %v", p, err)
		}
		gm, err := New(p)
		if err != nil {
			t.Fatalf("in-house New(%q) unexpectedly errored: %v", p, err)
		}
		for _, line := range lineCorpus {
			line := line
			wantCaps := lm.Matches(line)
			gotCaps := gm.Matches(line)
			if len(wantCaps) != len(gotCaps) {
				t.Fatalf("Matches(%q, %q) count mismatch: loki=%d in-house=%d", p, line, len(wantCaps), len(gotCaps))
			}
			for i := range wantCaps {
				if !bytes.Equal(wantCaps[i], gotCaps[i]) {
					t.Fatalf("Matches(%q, %q)[%d] mismatch: loki=%q in-house=%q", p, line, i, wantCaps[i], gotCaps[i])
				}
			}
			if lm.Test(line) != gm.Test(line) {
				t.Fatalf("Test(%q, %q) mismatch: loki=%v in-house=%v", p, line, lm.Test(line), gm.Test(line))
			}
		}
	}
}
