package logql

import (
	"fmt"
	"net/netip"

	loglib "github.com/grafana/loki/v3/pkg/logql/log"
	"github.com/grafana/loki/v3/pkg/logqlmodel"
	syntax "github.com/tsouza/cerberus/internal/logql/lsyntax"
	"go4.org/netipx"

	"github.com/tsouza/cerberus/internal/chplan"
)

// ip() filter lowering — LogQL's `|= ip("...")` line filter and
// `| addr = ip("...")` label filter.
//
// Reference semantics live in the pinned loki fork's
// pkg/logql/log/ip.go:
//
//   - The pattern is one of SINGLE-IP ("192.168.0.1"), CIDR
//     ("192.168.0.0/16") or IP-RANGE ("192.168.0.1-192.168.0.23"),
//     resolved in that order by `getMatcher` (netip.ParseAddr →
//     netip.ParsePrefix → netipx.ParseIPRange). Anything else is
//     `ErrIPFilterInvalidPattern` — reference Loki rejects the query
//     at stage-build time (HTTP 400); cerberus rejects at lowering.
//   - Matching scans the subject (the log line for the line filter,
//     the label VALUE for the label filter) for IP-shaped substrings:
//     maximal runs over the IPv4 charset `0123456789.` / IPv6 charset
//     `0123456789abcdefABCDEF:.`, each candidate parsed with
//     netip.ParseAddr and tested for containment — equality for a
//     single IP, Prefix.Contains for CIDR, IPRange.Contains for a
//     range. Candidates that fail to parse simply don't match
//     (`filterFn` skips the span) — an invalid-IP token never errors
//     the row.
//   - Containment is family-exact: a v4 candidate never matches a v6
//     matcher and vice versa (netip's Compare / Contains are
//     family-checked), including the v4-in-v6-mapped form.
//
// The lowering normalises every pattern kind to a closed [lo, hi]
// address interval in its family (single IP → lo == hi; CIDR →
// netipx.RangeOfPrefix; range → netipx.ParseIPRange) and renders the
// candidate test as a typed-IP BETWEEN:
//
//	v4: ifNull((toIPv4OrNull(x) >= toIPv4('<lo>')) AND
//	           (toIPv4OrNull(x) <= toIPv4('<hi>')), 0)
//	v6: isIPv6String(x) AND
//	    ifNull((toIPv6OrNull(x) >= toIPv6('<lo>')) AND
//	           (toIPv6OrNull(x) <= toIPv6('<hi>')), 0)
//
// over `arrayExists(x -> ..., extractAll(<subject>, '<charset>+'))`.
// CH's IPv4 / IPv6 types compare numerically (IPv6 is a big-endian
// FixedString(16) domain, so byte order == numeric order), which is
// exactly netip's ordering — the same interval the reference matcher
// covers. The OrNull conversions make unparseable candidates fall out
// as NULL → ifNull → 0 without aborting the query, mirroring the
// reference "skip the span" behaviour. The v6 arm's isIPv6String gate
// keeps v4-shaped candidates out (toIPv6OrNull would otherwise admit
// them as v4-mapped addresses, which reference treats as a family
// mismatch).
//
// Known narrow divergence: reference's scanner restarts one byte after
// a successfully-parsed-but-unmatched candidate, so an IP embedded as
// a proper SUFFIX of a longer charset run (e.g. `1.2.3.44` inside
// `11.2.3.44`) can also match. The maximal-run extraction here only
// tests the full run. Delimited IPs — every realistic log shape, and
// everything the differential corpus seeds — behave identically.

// ipPatternRange is the lowering-time normal form of an ip() pattern:
// a closed [Lo, Hi] interval within one address family.
type ipPatternRange struct {
	Lo, Hi netip.Addr
	V6     bool
}

// parseIPPattern resolves pattern using the same three-step probe as
// reference Loki's getMatcher (single IP → CIDR prefix → IP range) and
// normalises the match set to a closed interval.
func parseIPPattern(pattern string) (ipPatternRange, error) {
	if addr, err := netip.ParseAddr(pattern); err == nil {
		return ipPatternRange{Lo: addr, Hi: addr, V6: addr.Is6()}, nil
	}
	if pfx, err := netip.ParsePrefix(pattern); err == nil {
		r := netipx.RangeOfPrefix(pfx.Masked())
		return ipPatternRange{Lo: r.From(), Hi: r.To(), V6: pfx.Addr().Is6()}, nil
	}
	if r, err := netipx.ParseIPRange(pattern); err == nil {
		return ipPatternRange{Lo: r.From(), Hi: r.To(), V6: r.From().Is6()}, nil
	}
	// Mirrors reference Loki's ErrIPFilterInvalidPattern wording
	// (pkg/logql/log/ip.go); reference surfaces it as a 400 at
	// stage-build time, cerberus as a 422 at lowering — both 4xx.
	return ipPatternRange{}, fmt.Errorf("logql: ip: invalid pattern %q", pattern)
}

// ipCharsetRegexes are the candidate-extraction regexes per family —
// one maximal run over the reference scanner's charset per match.
const (
	ipv4CandidateRegex = "[0-9.]+"
	ipv6CandidateRegex = "[0-9a-fA-F:.]+"
)

// ipSubjectMatchExpr renders the "subject contains an IP inside the
// pattern's match set" predicate over an arbitrary string expression.
func ipSubjectMatchExpr(subject chplan.Expr, r ipPatternRange) chplan.Expr {
	const tok = "_cerb_ip"
	x := func() chplan.Expr { return &chplan.BareIdent{Name: tok} }

	convFn, castFn, candidateRegex := "toIPv4OrNull", "toIPv4", ipv4CandidateRegex
	if r.V6 {
		convFn, castFn, candidateRegex = "toIPv6OrNull", "toIPv6", ipv6CandidateRegex
	}

	between := &chplan.FuncCall{
		Name: "ifNull",
		Args: []chplan.Expr{
			&chplan.Binary{
				Op: chplan.OpAnd,
				Left: &chplan.Binary{
					Op:    chplan.OpGe,
					Left:  &chplan.FuncCall{Name: convFn, Args: []chplan.Expr{x()}},
					Right: &chplan.FuncCall{Name: castFn, Args: []chplan.Expr{&chplan.LitString{V: r.Lo.String()}}},
				},
				Right: &chplan.Binary{
					Op:    chplan.OpLe,
					Left:  &chplan.FuncCall{Name: convFn, Args: []chplan.Expr{x()}},
					Right: &chplan.FuncCall{Name: castFn, Args: []chplan.Expr{&chplan.LitString{V: r.Hi.String()}}},
				},
			},
			&chplan.LitInt{V: 0},
		},
	}
	body := chplan.Expr(between)
	if r.V6 {
		// Family gate: keep v4-shaped candidates out of the v6 arm —
		// toIPv6OrNull("1.2.3.4") succeeds as a v4-mapped address, but
		// reference netip containment is family-exact.
		body = &chplan.Binary{
			Op:    chplan.OpAnd,
			Left:  &chplan.FuncCall{Name: "isIPv6String", Args: []chplan.Expr{x()}},
			Right: between,
		}
	}

	return &chplan.FuncCall{
		Name: "arrayExists",
		Args: []chplan.Expr{
			&chplan.Lambda{Params: []string{tok}, Body: body},
			&chplan.FuncCall{
				Name: "extractAll",
				Args: []chplan.Expr{subject, &chplan.LitString{V: candidateRegex}},
			},
		},
	}
}

// ipLineFilterExpr lowers `|= ip("...")` / `!= ip("...")`. Reference
// Loki's NewIPLineFilter only admits the two equality match types —
// `|~ ip(...)` / `!~ ip(...)` fail stage-building with
// ErrIPFilterInvalidOperation (HTTP 400); cerberus mirrors with a 422.
func ipLineFilterExpr(lf *syntax.LineFilter, body chplan.Expr) (chplan.Expr, error) {
	switch lf.Ty {
	case loglib.LineMatchEqual, loglib.LineMatchNotEqual:
	default:
		return nil, fmt.Errorf("logql: ip: invalid operation for line filter (only |= and != support ip())")
	}
	r, err := parseIPPattern(lf.Match)
	if err != nil {
		return nil, err
	}
	pred := ipSubjectMatchExpr(body, r)
	if lf.Ty == loglib.LineMatchNotEqual {
		pred = notExpr(pred)
	}
	return pred, nil
}

// ipLabelFilterExpr lowers `| addr = ip("...")` / `| addr != ip("...")`.
//
// Reference per-row contract (pkg/logql/log/ip.go::
// (*IPLabelFilter).filterTy):
//
//   - rows already carrying `__error__` are KEPT unconditionally
//     ("if there's an error only the string matchers can filter out");
//   - label absent → the row is DROPPED, for `!=` too (the negation
//     only applies to the scan result);
//   - otherwise the label VALUE is scanned with the same candidate
//     machinery as the line filter, negated for `!=`.
//
// The grammar only produces the two equality types for ip() label
// filters, and an invalid pattern is rejected like the line filter
// (reference parks it in IPLabelFilter.patError → 400 at stage-build).
func ipLabelFilterExpr(f *loglib.IPLabelFilter, labelsExpr chplan.Expr) (chplan.Expr, error) {
	switch f.Ty {
	case loglib.LabelFilterEqual, loglib.LabelFilterNotEqual:
	default:
		return nil, fmt.Errorf("logql: ip: invalid operation for label filter (only = and != support ip())")
	}
	r, err := parseIPPattern(f.Pattern)
	if err != nil {
		return nil, err
	}

	scan := ipSubjectMatchExpr(&chplan.MapAccess{
		Map: labelsExpr,
		Key: &chplan.LitString{V: f.Label},
	}, r)
	if f.Ty == loglib.LabelFilterNotEqual {
		scan = notExpr(scan)
	}

	hasErr := &chplan.FuncCall{
		Name: "mapContains",
		Args: []chplan.Expr{labelsExpr, &chplan.LitString{V: logqlmodel.ErrorLabel}},
	}
	exists := &chplan.FuncCall{
		Name: "mapContains",
		Args: []chplan.Expr{labelsExpr, &chplan.LitString{V: f.Label}},
	}
	return &chplan.FuncCall{
		Name: "multiIf",
		Args: []chplan.Expr{
			hasErr, &chplan.LitBool{V: true},
			notExpr(exists), &chplan.LitBool{V: false},
			scan,
		},
	}, nil
}
