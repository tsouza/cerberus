package qlcommon

import "github.com/tsouza/cerberus/internal/chplan"

// OTelDottedFallbackChain builds the right-associative if-chain that
// resolves a Prom label whose OTel form has multiple spellings (see
// [format.PromLabelToOTelCandidates]) against the label map expression
// m. The chain reads:
//
//	if(mapContains(m, k0), m[k0],
//	  if(mapContains(m, k1), m[k1],
//	    ... m[kN-1]))
//
// so the leftmost candidate (the underscored input) wins when present,
// and the terminal branch is a bare MapAccess against the last
// candidate — when no candidate's key exists the map's value-type
// default (empty string) matches "absent label" semantics.
//
// CH's `m['missing']` returns the value-type default rather than NULL,
// so `coalesce` would short-circuit on the first lookup even when the
// row's actual key is a later candidate; `mapContains` cleanly
// distinguishes "present with empty value" from "absent".
//
// candidates must be non-empty. The PromQL and LogQL heads share this
// so a Grafana dashboard mixing Prom + Loki panels resolves labels
// symmetrically.
func OTelDottedFallbackChain(m chplan.Expr, candidates []string) chplan.Expr {
	last := candidates[len(candidates)-1]
	var chain chplan.Expr = &chplan.MapAccess{
		Map: m,
		Key: &chplan.LitString{V: last},
	}
	for i := len(candidates) - 2; i >= 0; i-- {
		k := candidates[i]
		chain = &chplan.FuncCall{
			Name: "if",
			Args: []chplan.Expr{
				&chplan.FuncCall{
					Name: "mapContains",
					Args: []chplan.Expr{
						m,
						&chplan.LitString{V: k},
					},
				},
				&chplan.MapAccess{
					Map: m,
					Key: &chplan.LitString{V: k},
				},
				chain,
			},
		}
	}
	return chain
}
