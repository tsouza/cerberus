package logql

import (
	"fmt"
	"net/netip"
	"strings"

	loglib "github.com/grafana/loki/v3/pkg/logql/log"
	"go4.org/netipx"
)

// From-scratch implementations of the `ip(...)` and `|>` / `!>`
// pattern line filters, written off the reference semantics
// (pkg/logql/log/ip.go and pkg/logql/log/pattern/pattern.go Test) —
// deliberately NOT sharing code with internal/logql's SQL lowering so
// the property diff is a genuine two-implementation comparison.

// ipLineFilterPredicate compiles `|= ip("<pattern>")` / `!= ip(...)`.
//
// Reference: pkg/logql/log/ip.go. The pattern resolves to single-IP /
// CIDR / IP-range (in that probe order); the line is scanned for
// IP-shaped charset runs, each candidate parsed and tested for
// containment. The generator only produces delimited IP tokens, where
// the maximal-run scan below agrees with the reference scanner
// exactly.
func ipLineFilterPredicate(pattern string, ty loglib.LineMatchType) (func(string) bool, error) {
	if ty != loglib.LineMatchEqual && ty != loglib.LineMatchNotEqual {
		return nil, fmt.Errorf("oracle/logql: ip line filter only supports |= and != (got %s)", ty)
	}
	contains, err := ipContainsFunc(pattern)
	if err != nil {
		return nil, err
	}
	match := func(line string) bool {
		for _, tok := range ipCharsetRuns(line, contains.v6) {
			addr, err := netip.ParseAddr(tok)
			if err != nil {
				continue
			}
			if contains.fn(addr) {
				return true
			}
		}
		return false
	}
	if ty == loglib.LineMatchNotEqual {
		return func(line string) bool { return !match(line) }, nil
	}
	return match, nil
}

type ipContains struct {
	fn func(netip.Addr) bool
	v6 bool
}

// ipContainsFunc mirrors reference getMatcher's probe order:
// netip.ParseAddr → netip.ParsePrefix → netipx.ParseIPRange. The
// containment closures are family-exact like netip's own Compare /
// Contains.
func ipContainsFunc(pattern string) (ipContains, error) {
	if addr, err := netip.ParseAddr(pattern); err == nil {
		return ipContains{
			fn: func(a netip.Addr) bool { return addr.Compare(a) == 0 },
			v6: addr.Is6(),
		}, nil
	}
	if pfx, err := netip.ParsePrefix(pattern); err == nil {
		return ipContains{
			fn: func(a netip.Addr) bool { return pfx.Contains(a) },
			v6: pfx.Addr().Is6(),
		}, nil
	}
	if r, err := netipx.ParseIPRange(pattern); err == nil {
		return ipContains{
			fn: func(a netip.Addr) bool { return r.Contains(a) },
			v6: r.From().Is6(),
		}, nil
	}
	return ipContains{}, fmt.Errorf("oracle/logql: ip: invalid pattern %q", pattern)
}

// ipCharsetRuns returns the maximal runs of the per-family IP charset
// (reference IPv4Charset / IPv6Charset) in line.
func ipCharsetRuns(line string, v6 bool) []string {
	charset := "0123456789."
	if v6 {
		charset = "0123456789abcdefABCDEF:."
	}
	var out []string
	start := -1
	for i := 0; i <= len(line); i++ {
		in := i < len(line) && strings.IndexByte(charset, line[i]) >= 0
		switch {
		case in && start < 0:
			start = i
		case !in && start >= 0:
			out = append(out, line[start:i])
			start = -1
		}
	}
	return out
}

// patternLinePredicate compiles a `|>` / `!>` pattern line filter.
//
// Reference: pkg/logql/log/pattern.Matcher.Test —
//
//   - empty line matches only the empty pattern (and vice versa);
//   - literals are searched first-occurrence left-to-right; every
//     literal except a pattern-leading one must land strictly after
//     the cursor (a zero-length gap is an empty `<_>`, which never
//     matches); a pattern-leading literal floats (Test never anchors
//     it at offset 0);
//   - a pattern ending on a literal requires the cursor to finish
//     exactly at end-of-line; one ending on `<_>` requires a
//     non-empty remainder.
//
// The structure derivation splits on the literal token `<_>` — the
// only capture form the line-filter grammar admits (named captures
// are rejected upstream; the generator never produces them). An
// interior empty segment would mean consecutive captures (also
// rejected upstream), surfaced as an error here so a generator
// widening can't silently change semantics.
func patternLinePredicate(pattern string, negated bool) (func(string) bool, error) {
	test, err := patternTestFunc(pattern)
	if err != nil {
		return nil, err
	}
	if negated {
		return func(line string) bool { return !test(line) }, nil
	}
	return test, nil
}

func patternTestFunc(pattern string) (func(string) bool, error) {
	if pattern == "" {
		return func(line string) bool { return line == "" }, nil
	}
	parts := strings.Split(pattern, "<_>")
	for i, p := range parts {
		if p == "" && i > 0 && i < len(parts)-1 {
			return nil, fmt.Errorf("oracle/logql: consecutive captures in pattern %q", pattern)
		}
	}
	var lits []string
	for _, p := range parts {
		if p != "" {
			lits = append(lits, p)
		}
	}
	firstIsLiteral := parts[0] != ""
	endsWithCapture := parts[len(parts)-1] == ""

	return func(line string) bool {
		if line == "" {
			return false
		}
		off := 0
		for i, lit := range lits {
			j := strings.Index(line[off:], lit)
			if j < 0 {
				return false
			}
			if j == 0 && !(i == 0 && firstIsLiteral) {
				// Empty wildcard between the cursor and this literal.
				return false
			}
			off += j + len(lit)
		}
		if endsWithCapture {
			return off != len(line)
		}
		return off == len(line)
	}, nil
}
