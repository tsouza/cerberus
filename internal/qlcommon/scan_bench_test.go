package qlcommon

import (
	"strings"
	"testing"
)

// The four loops the single-advance reshape touches are scanners over text
// lifted straight out of a user's query, so their cost is measured rather
// than assumed — the same treatment tsouza/cerberus#2977 gave the lexer
// loop when it added the progress invariants these benchmarks now measure
// the removal of.
//
// Each shape is a separate benchmark because the arms they exercise differ:
// a pattern of ordinary bytes runs almost entirely on the default advance,
// a group-dense one runs on classifyGroupOpen, and a class-dense one spends
// its time inside skipCharClass.

var benchScanShapes = []struct {
	name string
	src  string
}{
	{"plain", strings.Repeat("abcdefgh", 64)},
	{"groups", strings.Repeat("(?P<n>ab)(?:cd)|", 32)},
	{"classes", strings.Repeat("[a-z][[:alpha:]][^]x]", 24)},
	{"escapes", strings.Repeat(`a\(b\[c\Qz\E`, 32)},
}

func BenchmarkScanSourceGroups(b *testing.B) {
	for _, shape := range benchScanShapes {
		b.Run(shape.name, func(b *testing.B) {
			b.ReportAllocs()
			for b.Loop() {
				if _, ok := scanSourceGroups(shape.src); !ok {
					b.Fatalf("scanSourceGroups declined the %s shape", shape.name)
				}
			}
		})
	}
}

func BenchmarkSkipCharClass(b *testing.B) {
	const src = "[" + "abcdefghijklmnopqrstuvwxyz0123456789" + "[:alpha:]" + "]"
	b.ReportAllocs()
	for b.Loop() {
		if _, ok := skipCharClass(src, 0); !ok {
			b.Fatal("skipCharClass declined its own fixture")
		}
	}
}

var benchTemplateShapes = []struct {
	name string
	repl string
}{
	{"literal", strings.Repeat("abcdefgh", 64)},
	{"refs", strings.Repeat("x$1y${2}z$name", 32)},
	{"dollars", strings.Repeat("$$a$-b${unclosed", 32)},
}

func BenchmarkResolveSegments(b *testing.B) {
	groups := newCaptureGroups(nineCaptureRegex, withoutCaptureProbes)
	for _, shape := range benchTemplateShapes {
		b.Run(shape.name, func(b *testing.B) {
			b.ReportAllocs()
			for b.Loop() {
				if _, err := resolveSegments(shape.repl, groups); err != nil {
					b.Fatalf("resolveSegments rejected the %s shape: %v", shape.name, err)
				}
			}
		})
	}
}

func BenchmarkEmptyCapturesReplacement(b *testing.B) {
	for _, shape := range benchTemplateShapes {
		b.Run(shape.name, func(b *testing.B) {
			b.ReportAllocs()
			for b.Loop() {
				if EmptyCapturesReplacement(shape.repl) == "\x00" {
					b.Fatal("unreachable, and here only so the call is not optimised away")
				}
			}
		})
	}
}
