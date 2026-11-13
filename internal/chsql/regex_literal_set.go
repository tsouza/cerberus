package chsql

import (
	"regexp/syntax"
	"slices"
)

// maxRegexLiteralSet bounds the literal set a regex matcher is rewritten
// to. A pattern whose language is larger — typically a product of
// character classes such as `[a-z][a-z][a-z]` — keeps match(), so the
// rewrite never emits a membership list much longer than the pattern it
// replaces would have been worth.
const maxRegexLiteralSet = 1024

// parseSimplifiedRegex parses pattern as the reference engines' Go regexp
// does ([goRegexpFlags]) and simplifies it; ok is false when it does not
// parse.
func parseSimplifiedRegex(pattern string) (re *syntax.Regexp, ok bool) {
	re, err := syntax.Parse(pattern, goRegexpFlags)
	if err != nil {
		return nil, false
	}
	return re.Simplify(), true
}

// anchoredRegexLiteralSet reports whether the anchored pattern — the
// `^(?:…)$` form [anchoredRegexPattern] produces — matches exactly a
// finite set of strings, and returns that set without duplicates. A
// subject then matches the pattern if and only if it equals a member of
// the set, so `match(subject, anchored)` and `subject IN (set)` answer
// alike on every value, an empty or absent one included.
//
// The analysis accepts a language built from literals, the empty string,
// character classes, alternation, concatenation, `?` and groups. It
// rejects:
//
//   - any case-insensitive node: a folded literal is a set of spellings
//     whose membership follows the engine's case-folding tables, so it
//     stays with the engine;
//   - any node [readsInvalidByte] flags (`.`, a word boundary, U+FFFD or a
//     surrogate): Go's regexp reads an invalid UTF-8 byte of the subject
//     as U+FFFD, so such a pattern can match a byte string no literal
//     equals;
//   - any empty-width assertion other than the outer anchors, and every
//     unbounded repetition;
//   - a language larger than [maxRegexLiteralSet].
//
// What remains is matched rune for rune, which on any subject is byte
// equality with one member of the set.
func anchoredRegexLiteralSet(anchored string) ([]string, bool) {
	re, ok := parseSimplifiedRegex(anchored)
	if !ok || re.Op != syntax.OpConcat || len(re.Sub) < 2 {
		return nil, false
	}
	first, last := re.Sub[0], re.Sub[len(re.Sub)-1]
	if first.Op != syntax.OpBeginText || last.Op != syntax.OpEndText {
		return nil, false
	}
	set, ok := concatLiteralSet(re.Sub[1 : len(re.Sub)-1])
	if !ok {
		return nil, false
	}
	return dedupeStable(set), true
}

// literalSet returns the finite language of re, or ok=false when re is
// outside the shapes [anchoredRegexLiteralSet] accepts.
func literalSet(re *syntax.Regexp) ([]string, bool) {
	if re.Flags&syntax.FoldCase != 0 || readsInvalidByte(re) {
		return nil, false
	}
	switch re.Op {
	case syntax.OpEmptyMatch:
		return []string{""}, true
	case syntax.OpLiteral:
		return []string{string(re.Rune)}, true
	case syntax.OpCharClass:
		return charClassLiterals(re.Rune)
	case syntax.OpCapture:
		return literalSet(re.Sub[0])
	case syntax.OpQuest:
		set, ok := literalSet(re.Sub[0])
		if !ok || len(set) >= maxRegexLiteralSet {
			return nil, false
		}
		return append(set, ""), true
	case syntax.OpConcat:
		return concatLiteralSet(re.Sub)
	case syntax.OpAlternate:
		var set []string
		for _, sub := range re.Sub {
			alt, ok := literalSet(sub)
			if !ok || len(set)+len(alt) > maxRegexLiteralSet {
				return nil, false
			}
			set = append(set, alt...)
		}
		return set, true
	}
	return nil, false
}

// concatLiteralSet is the language of the concatenation of subs: every
// member of the first followed by every member of the rest. An empty subs
// is the language holding only the empty string.
func concatLiteralSet(subs []*syntax.Regexp) ([]string, bool) {
	set := []string{""}
	for _, sub := range subs {
		next, ok := literalSet(sub)
		if !ok || len(set)*len(next) > maxRegexLiteralSet {
			return nil, false
		}
		product := make([]string, 0, len(set)*len(next))
		for _, prefix := range set {
			for _, suffix := range next {
				product = append(product, prefix+suffix)
			}
		}
		set = product
	}
	return set, true
}

// charClassLiterals lists every rune of a parsed character class (lo, hi
// pairs) as a one-rune string.
func charClassLiterals(ranges []rune) ([]string, bool) {
	count := 0
	for i := 0; i < len(ranges); i += 2 {
		count += int(ranges[i+1]-ranges[i]) + 1
		if count > maxRegexLiteralSet {
			return nil, false
		}
	}
	set := make([]string, 0, count)
	for i := 0; i < len(ranges); i += 2 {
		for r := ranges[i]; r <= ranges[i+1]; r++ {
			set = append(set, string(r))
		}
	}
	return set, true
}

// dedupeStable drops repeated members of set, keeping first occurrences
// in order.
func dedupeStable(set []string) []string {
	seen := make(map[string]struct{}, len(set))
	return slices.DeleteFunc(set, func(s string) bool {
		if _, dup := seen[s]; dup {
			return true
		}
		seen[s] = struct{}{}
		return false
	})
}

// literalSetMembership renders `subject IN (set)` — `subject = member`
// for a one-element set — or its negation.
func literalSetMembership(negated bool, subject Frag, set []string) Frag {
	if len(set) == 1 {
		if negated {
			return Paren(Neq(subject, Lit(set[0])))
		}
		return Paren(Eq(subject, Lit(set[0])))
	}
	members := make([]Frag, len(set))
	for i, s := range set {
		members[i] = Lit(s)
	}
	if negated {
		return Paren(NotIn(subject, members...))
	}
	return Paren(In(subject, members...))
}
