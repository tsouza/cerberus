package loki

import (
	"github.com/tsouza/cerberus/internal/chplan"
	"github.com/tsouza/cerberus/internal/chsql"
)

// The lambda parameter names the normalisation frags bind. They are all
// distinct because the expression nests four lambdas deep — a shadowed
// name would still parse and would silently bind the wrong scope.
const (
	normStoredKeyParam   = "nk" // one stored attribute key
	normStoredValueParam = "nv" // that key's value
	normByteIndexParam   = "nb" // 1-based byte offset inside a key
	normByteParam        = "nc" // one byte of a key, as a 1-byte String
	normEntryParam       = "ne" // one (stored, value, served) triple
	normPeerParam        = "np" // ditto, while scanning for a peer
	normOrdinalParam     = "ni" // a triple's 1-based position
)

// The 1-based tuple positions of the (stored key, value, served key)
// triple every normalisation stage passes around.
const (
	normStoredKeyPos = 1
	normValuePos     = 2
	normServedKeyPos = 3
)

// promLabelNameFrag renders the ClickHouse expression that rewrites one
// STORED attribute key into the Prometheus label NAME cerberus serves it
// under — the SQL twin of [format.OTelToPromLabel], byte for byte.
//
// Go's rewrite walks the key one BYTE at a time: every byte outside
// `[a-zA-Z0-9_]` becomes `_`, and a leading digit additionally gets a `_`
// prefix. So does this, via `substring` (which is byte-indexed; the UTF-8
// variant is spelled `substringUTF8`) over `range(1, length(k) + 1)`
// (`length` is likewise the byte count).
//
// A regex would be shorter and WRONG. ClickHouse compiles
// `replaceRegexpAll` patterns with RE2's default UTF-8 encoding, so
// `[^a-zA-Z0-9_]` matches one RUNE: measured against chDB 26.5,
// `replaceRegexpAll('aé', '[^a-zA-Z0-9_]', '_')` is `a_` where
// [format.OTelToPromLabel] answers `a__`. That single-byte difference is
// enough to make the served name computed here disagree with the one the
// handler serves, which is the whole property this expression exists to
// guarantee.
//
// The byte classes are written as string comparisons against `'0'`, `'9'`,
// `'A'`, … rather than as the code points they stand for: CH compares
// Strings byte-wise, so on a one-byte String the comparison IS the byte
// comparison, and the grammar reads as the grammar instead of as seven
// numeric constants.
func promLabelNameFrag(key chsql.Frag) chsql.Frag {
	byteAt := func(idx chsql.Frag) chsql.Frag {
		return chsql.Call("substring", key, idx, chsql.InlineLit(1))
	}
	c := chsql.BareIdent(normByteParam)
	inGrammar := chsql.Or(
		chsql.And(chsql.Gte(c, chsql.InlineLit("0")), chsql.Lte(c, chsql.InlineLit("9"))),
		chsql.And(chsql.Gte(c, chsql.InlineLit("A")), chsql.Lte(c, chsql.InlineLit("Z"))),
		chsql.And(chsql.Gte(c, chsql.InlineLit("a")), chsql.Lte(c, chsql.InlineLit("z"))),
		chsql.Eq(c, chsql.InlineLit("_")),
	)
	bytes := chsql.Call(
		"arrayMap",
		chsql.Lambda1(normByteIndexParam, byteAt(chsql.BareIdent(normByteIndexParam))),
		chsql.Call("range", chsql.InlineLit(1),
			chsql.Add(chsql.Call("length", key), chsql.InlineLit(1))),
	)
	rewritten := chsql.Call(
		"arrayStringConcat",
		chsql.Call("arrayMap", chsql.Lambda1(normByteParam, chsql.If(inGrammar, c, chsql.InlineLit("_"))), bytes),
	)
	first := byteAt(chsql.InlineLit(1))
	digitPrefix := chsql.If(
		chsql.And(chsql.Gte(first, chsql.InlineLit("0")), chsql.Lte(first, chsql.InlineLit("9"))),
		chsql.InlineLit("_"), chsql.InlineLit(""),
	)
	return chsql.Call("concat", digitPrefix, rewritten)
}

// normalizedLabelsFrag renders the ClickHouse expression that rewrites a
// whole stored label-set Map into the one cerberus SERVES — the SQL twin
// of [format.NormalizeLabelMap], collision policy included.
//
// It exists because /index/volume's answer is keyed by the SERVED label
// set while its rows are grouped by the STORED one, and the rewrite
// between them is not injective: `a.b` and `a_b` are two stored keys and
// one served name. Grouping on the stored key and normalising afterwards
// therefore produced two vector samples under one identical label set,
// each carrying half of one series' byte volume (issue #3246). Upstream
// has no such split because Loki normalises at INGEST — its OTLP handler
// rewrites attribute names before the stream is written, so two spellings
// are one stream by the time any volume is accumulated — and cerberus,
// which normalises on read, has to reach the same place by grouping on
// the rewritten key.
//
// Normalising in Go after the fact cannot get there. The endpoint's top-N
// cut is a function of the volumes, and merging changes the volumes: a
// group built out of rows that all sit below the cut can outrank a group
// above it, so no superset the SQL can name in terms of the STORED
// volumes is guaranteed to contain the correct top-N. Either the SQL
// learns the served identity or it stops cutting at all.
//
// The shape, given a `(stored, value, served)` triple per entry:
//
//  1. `arraySort` by `(served != stored, stored)` — the entry whose key
//     needed no rewriting sorts ahead of every entry that did, and
//     rewrites sort among themselves by their stored spelling. That is
//     exactly [format.NormalizeLabelMap]'s policy read as an order: the
//     already-Prometheus-shaped form wins a collision, and between two
//     rewrites (`a.b` and `a-b`) the lexically-first stored key wins.
//  2. `arrayFilter` keeping a triple only at the FIRST position its
//     served name appears at — the winner picked in (1) — and dropping a
//     served name that came out empty, which happens only for the empty
//     stored key.
//  3. `CAST(… AS Map(String, String))` over the surviving
//     `(served, value)` pairs, wrapped in [canonicalLabelsFrag]'s own
//     `mapSort`, because the array is in policy order rather than key
//     order and a CH Map compares positionally.
//
// [rankIndexVolumeRows] still runs [format.NormalizeLabelMap] over what
// comes back. That is not belt-and-braces: it is the DEFINITION of the
// served map, and the stub-fed handler tests reach it with stored keys
// that never passed through this expression at all. What this frag owes
// is that the GROUPING agrees with that definition, which
// TestNormalizedLabelsFrag_ChDB_MatchesGoNormalizer adjudicates entry by
// entry against the Go function itself rather than against a transcript
// of it.
func normalizedLabelsFrag(stored chsql.Frag) chsql.Frag {
	entryAt := func(param string, pos int) chsql.Frag {
		return chsql.TupleIndex(chsql.BareIdent(param), pos)
	}
	triples := chsql.Call(
		"arrayMap",
		chsql.Lambda2(normStoredKeyParam, normStoredValueParam, chsql.Tuple(
			chsql.BareIdent(normStoredKeyParam),
			chsql.BareIdent(normStoredValueParam),
			promLabelNameFrag(chsql.BareIdent(normStoredKeyParam)),
		)),
		chsql.Call("mapKeys", stored),
		chsql.Call("mapValues", stored),
	)
	sorted := chsql.Call(
		"arraySort",
		chsql.Lambda1(normEntryParam, chsql.Tuple(
			chsql.Neq(entryAt(normEntryParam, normServedKeyPos), entryAt(normEntryParam, normStoredKeyPos)),
			entryAt(normEntryParam, normStoredKeyPos),
		)),
		triples,
	)
	servedNames := chsql.Call(
		"arrayMap",
		chsql.Lambda1(normPeerParam, entryAt(normPeerParam, normServedKeyPos)),
		sorted,
	)
	winners := chsql.Call(
		"arrayFilter",
		chsql.Lambda2(normEntryParam, normOrdinalParam, chsql.And(
			chsql.Neq(entryAt(normEntryParam, normServedKeyPos), chsql.InlineLit("")),
			chsql.Eq(
				chsql.Call("indexOf", servedNames, entryAt(normEntryParam, normServedKeyPos)),
				chsql.BareIdent(normOrdinalParam),
			),
		)),
		sorted,
		chsql.Call("arrayEnumerate", sorted),
	)
	pairs := chsql.Call(
		"arrayMap",
		chsql.Lambda1(normEntryParam, chsql.Tuple(
			entryAt(normEntryParam, normServedKeyPos),
			entryAt(normEntryParam, normValuePos),
		)),
		winners,
	)
	return chsql.Call(chplan.CanonicalMapFunc, chsql.Cast(pairs, servedLabelMapType))
}

// servedLabelMapType is the CH type the served label set casts to. Every
// Prometheus label name and value is a String, and the handler decodes
// the column straight into a Go map[string]string.
const servedLabelMapType = "Map(String, String)"
