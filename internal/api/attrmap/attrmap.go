// Package attrmap holds the typed chsql fragments the three HTTP heads' label
// and tag metadata endpoints build over the OTel attribute Map columns
// (`Attributes`, `ResourceAttributes`, `SpanAttributes`, …): a subscript, its
// not-empty predicate, and the one-scan projection of every candidate
// spelling of a label. It exists so the heads compose ONE set of fragments
// rather than each carrying its own copy — the Prometheus head collapsed its
// per-candidate scans into one (cerberus issue #3168) while the Loki head
// kept fanning out, which is the drift a shared fragment closes.
package attrmap

import "github.com/tsouza/cerberus/internal/chsql"

// At emits "`<col>`[?]" with key bound as a positional argument — the bare
// Map subscript every other fragment here builds on. The key is never
// spliced into the SQL text.
func At(col, key string) chsql.Frag {
	return func(b *chsql.Builder) { b.MapAt(col, key) }
}

// DistinctAt emits "DISTINCT `<col>`[?]" — the projection for "the distinct
// values stored under one Map key".
func DistinctAt(col, key string) chsql.Frag {
	return chsql.Distinct(At(col, key))
}

// NotEmpty emits "`<col>`[?] != ?", binding both the key and the
// empty-string sentinel ClickHouse returns for an absent Map key, so a row
// that does not carry the key is dropped rather than surfaced as "".
func NotEmpty(col, key string) chsql.Frag {
	return chsql.Neq(At(col, key), chsql.Lit(""))
}

// CollapsedValues emits the projection that surfaces every candidate
// spelling's value out of col in ONE scan:
//
//	arrayJoin(arrayFilter(v -> v != '', [<col>[k0], <col>[k1], …]))
//
// arrayFilter drops the empty-string sentinel ClickHouse returns for an
// absent Map key BEFORE arrayJoin explodes the survivors into rows, so a row
// missing every candidate contributes zero rows (arrayJoin on an empty
// array yields none) and no separate not-empty predicate is needed. One
// scan per table replaces one scan per candidate: a label with two
// rewritable underscores expands to seven spellings, and seven full scans of
// a fact table for one Grafana variable is the 120 s timeout cerberus issue
// #3168 measured.
func CollapsedValues(col string, keys []string) chsql.Frag {
	lookups := make([]chsql.Frag, len(keys))
	for i, k := range keys {
		lookups[i] = At(col, k)
	}
	notEmpty := chsql.Lambda1("v", chsql.Neq(chsql.BareIdent("v"), chsql.Lit("")))
	return chsql.Call("arrayJoin", chsql.Call("arrayFilter", notEmpty, chsql.Array(lookups...)))
}
