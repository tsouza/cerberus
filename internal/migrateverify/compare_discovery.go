package migrateverify

import (
	"encoding/json"
	"fmt"
	"sort"
)

// missingTagValue is what a first-diff prints on the side that did not return
// a discovered tag NAME or VALUE, mirroring the matrix lane's "<missing series>".
const missingTagValue = "<missing tag>"

// tempoTagScopes is the fixed, canonical order the v2 tag-NAMES surface is
// diffed in. It is not read off the response (a scope a backend omits when it
// has zero tags for it is indistinguishable from a scope it never reports),
// so the comparator always walks all three and treats an absent scope as
// empty, never as "not judged".
var tempoTagScopes = []string{"resource", "span", "intrinsic"}

// lokiDiscoveryPayload is Loki's decoded discovery response: a flat value set,
// used for both the unfiltered label-NAME enumeration and a single label's
// VALUE enumeration — the two Loki surfaces share one envelope shape.
type lokiDiscoveryPayload struct {
	Values []string
}

// lokiDiscoveryEnvelope is the wire shape both backends emit for
// /loki/api/v1/labels and /loki/api/v1/label/{name}/values.
type lokiDiscoveryEnvelope struct {
	Status string   `json:"status"`
	Data   []string `json:"data"`
}

// decodeLokiDiscovery parses a 200 Loki discovery body.
func decodeLokiDiscovery(body []byte) (any, error) {
	var raw lokiDiscoveryEnvelope
	if err := json.Unmarshal(body, &raw); err != nil {
		return nil, fmt.Errorf("decode loki discovery response: %w", err)
	}
	return lokiDiscoveryPayload{Values: raw.Data}, nil
}

// tempoDiscoveryPayload is Tempo's decoded discovery response, holding
// whichever of the four endpoints' fields the body actually carried. It does
// NOT know which Surface fetched it — see tempoTagDiscoveryDialect.Decode —
// so an empty/absent field here is not itself a signal; the comparator reads
// only the field its Query's Surface names.
type tempoDiscoveryPayload struct {
	// TagNames: /api/search/tags (surface tags-v1).
	TagNames []string
	// Scopes: /api/v2/search/tags (surface tags-v2), keyed by scope name.
	Scopes map[string][]string
	// TagValues: /api/search/tag/{name}/values (surface tag-values-v1).
	TagValues []string
	// TagValuesV2: /api/v2/search/tag/{name}/values (surface tag-values-v2),
	// value -> Tempo's type label ("string" / "int" / "float" / …).
	TagValuesV2 map[string]string
	// TotalJobs / CompletedJobs: the reference's OWN completeness report for
	// this probe. Cerberus's discovery endpoints carry no such block (a single
	// ClickHouse query, complete by construction), so these are always 0 on
	// that side — a zero TotalJobs is not itself a partiality claim (see
	// partial()), mirroring traceSearchResult.partial().
	TotalJobs     int64
	CompletedJobs int64
}

// partial reports whether the reference answered this probe with an
// incomplete search, mirroring traceSearchResult.partial().
func (p tempoDiscoveryPayload) partial() bool {
	return p.TotalJobs > 0 && p.CompletedJobs < p.TotalJobs
}

// tempoDiscoveryRawScope is one element of the v2 tags response's "scopes" list.
type tempoDiscoveryRawScope struct {
	Name string   `json:"name"`
	Tags []string `json:"tags"`
}

// tempoDiscoveryRawTagValueV2 is one element of the v2 tag-values response's
// "tagValues" list — TagValue's JSON shape.
type tempoDiscoveryRawTagValueV2 struct {
	Type  string `json:"type"`
	Value string `json:"value"`
}

// tempoDiscoveryRaw is the union of all four Tempo discovery envelopes'
// top-level fields. TagValues is held raw because its element shape (a bare
// string on v1, a {type,value} object on v2) is not knowable without the
// Surface, which Decode does not carry — see decodeTempoDiscovery.
type tempoDiscoveryRaw struct {
	TagNames  []string                 `json:"tagNames"`
	Scopes    []tempoDiscoveryRawScope `json:"scopes"`
	TagValues json.RawMessage          `json:"tagValues"`
	Metrics   struct {
		TotalJobs     traceFlexInt64 `json:"totalJobs"`
		CompletedJobs traceFlexInt64 `json:"completedJobs"`
	} `json:"metrics"`
}

// decodeTempoDiscovery parses a 200 body from any of the four Tempo discovery
// endpoints. It is Surface-agnostic by construction (see
// tempoTagDiscoveryDialect.Decode): TagValues is attempted first as a plain
// string array (v1's shape) and, only if that fails, as the typed {type,value}
// array (v2's shape) — the two are structurally distinct, so at most one ever
// succeeds and there is never a silent misread.
func decodeTempoDiscovery(body []byte) (any, error) {
	var raw tempoDiscoveryRaw
	if err := json.Unmarshal(body, &raw); err != nil {
		return nil, fmt.Errorf("decode tempo discovery response: %w", err)
	}
	out := tempoDiscoveryPayload{
		TagNames:      raw.TagNames,
		TotalJobs:     int64(raw.Metrics.TotalJobs),
		CompletedJobs: int64(raw.Metrics.CompletedJobs),
	}
	if len(raw.Scopes) > 0 {
		out.Scopes = make(map[string][]string, len(raw.Scopes))
		for _, s := range raw.Scopes {
			out.Scopes[s.Name] = s.Tags
		}
	}
	if len(raw.TagValues) > 0 && string(raw.TagValues) != "null" {
		var plain []string
		if err := json.Unmarshal(raw.TagValues, &plain); err == nil {
			out.TagValues = plain
		} else {
			var typed []tempoDiscoveryRawTagValueV2
			if err := json.Unmarshal(raw.TagValues, &typed); err != nil {
				return nil, fmt.Errorf("decode tempo tagValues: neither a v1 string array nor a v2 typed array: %w", err)
			}
			out.TagValuesV2 = make(map[string]string, len(typed))
			for _, tv := range typed {
				out.TagValuesV2[tv.Value] = tv.Type
			}
		}
	}
	return out, nil
}

// compareTagDiscovery judges one tag/label discovery probe.
//
// The parity rule, in the operator's words: discovery is SET EQUALITY per
// endpoint (and, on Tempo's v2 tag-names surface, per SCOPE). Every tag name
// or tag value present on one side and not the other is a divergence. The
// only exception is a reference response the reference ITSELF reports as
// partial — that one is declined by name with both job counts, never diffed
// against cerberus's complete answer (cerberus is never partial: one
// ClickHouse query, complete-or-error by construction).
func compareTagDiscovery(q Query, refAny, cerAny any, _ Params) Outcome {
	switch q.Head {
	case HeadLoki:
		return compareLokiDiscovery(q, refAny, cerAny)
	case HeadTempo:
		return compareTempoDiscovery(q, refAny, cerAny)
	default:
		return Outcome{
			Verdict: VerdictError,
			Detail:  fmt.Sprintf("tag-discovery comparator has no rule for head %q", q.Head),
		}
	}
}

func compareLokiDiscovery(q Query, refAny, cerAny any) Outcome {
	ref, refOK := refAny.(lokiDiscoveryPayload)
	cer, cerOK := cerAny.(lokiDiscoveryPayload)
	if !refOK || !cerOK {
		return Outcome{Verdict: VerdictError, Detail: "loki tag-discovery comparator received a payload it cannot read; the lane is wired to the wrong dialect"}
	}
	reason := ReasonTagMissing
	if q.Surface == SurfaceLokiLabelValues {
		reason = ReasonTagValueMissing
	}
	return diffTagSet(ref.Values, cer.Values, q.TagName, "", reason)
}

func compareTempoDiscovery(q Query, refAny, cerAny any) Outcome {
	ref, refOK := refAny.(tempoDiscoveryPayload)
	cer, cerOK := cerAny.(tempoDiscoveryPayload)
	if !refOK || !cerOK {
		return Outcome{Verdict: VerdictError, Detail: "tempo tag-discovery comparator received a payload it cannot read; the lane is wired to the wrong dialect"}
	}

	var out Outcome
	switch q.Surface {
	case SurfaceTempoTagsV1:
		out = diffTagSet(ref.TagNames, cer.TagNames, "", "", ReasonTagMissing)
	case SurfaceTempoTagsV2:
		out = diffTagScopes(ref.Scopes, cer.Scopes)
	case SurfaceTempoTagValuesV1:
		out = diffTagSet(ref.TagValues, cer.TagValues, q.TagName, "", ReasonTagValueMissing)
	case SurfaceTempoTagValuesV2:
		out = diffTagValuesV2(ref.TagValuesV2, cer.TagValuesV2, q.TagName)
	default:
		return Outcome{Verdict: VerdictError, Detail: fmt.Sprintf("tempo tag-discovery comparator has no rule for surface %q", q.Surface)}
	}

	if ref.partial() {
		out.Limitations = append(out.Limitations, Limitation{
			Code:  LimitTagDiscoveryRefPartial,
			Count: 1,
			Detail: limitationDetails[LimitTagDiscoveryRefPartial] +
				fmt.Sprintf(" The reference completed %d of %d jobs.", ref.CompletedJobs, ref.TotalJobs),
		})
		// Membership is not judgeable when the reference itself is incomplete: a
		// clean diff above still stands (it found no CONFLICT), but it cannot be
		// promoted to a parity claim.
		if out.Verdict == VerdictMatch {
			out.Verdict = VerdictUndecidable
		}
	}
	return out
}

// diffTagSet compares two flat value sets. tag, when non-empty, is the key
// these values belong to (a label-values / tag-values probe); it is empty for
// an unfiltered tag-NAME enumeration, where each element IS its own identity.
// scope, when non-empty, anchors a v2 per-scope diff.
//
// The two directions of "present on one side only" are NOT symmetric. A value
// the reference has but cerberus does not is always a real divergence:
// cerberus's own enumeration is one unbounded ClickHouse DISTINCT that is
// complete-or-errored by construction, never silently capped. A value cerberus
// has but the reference does not is AMBIGUOUS: both Tempo and Loki cap
// discovery cardinality server-side and return 200 with a silently-truncated
// set when the cap trips, with no field in either wire response naming that it
// happened, so this direction cannot be told apart from a real cerberus
// over-report. It is therefore never diffed as a hard divergence; it is named
// and counted via LimitTagDiscoveryRefCardinalityUnknown instead — the same
// "declared, never silent" treatment every other undecidable dimension in this
// package gets.
func diffTagSet(ref, cer []string, tag, scope, reasonCode string) Outcome {
	refSet := toStringSet(ref)
	cerSet := toStringSet(cer)
	union := unionKeys(refSet, cerSet)
	out := Outcome{Verdict: VerdictMatch}
	ambiguous := 0
	for _, v := range union {
		_, inRef := refSet[v]
		_, inCer := cerSet[v]
		if !inRef && inCer {
			ambiguous++
			continue
		}
		out.Compared++
		if inRef && inCer {
			continue
		}
		// Only the reference-only case reaches here: inRef && !inCer.
		if out.Verdict == VerdictDiverge {
			continue
		}
		anchorTag := tag
		if anchorTag == "" {
			anchorTag = v
		}
		out.Verdict = VerdictDiverge
		out.FirstDiff = &FirstDiff{
			Kind: KindTagDiscovery, Scope: scope, Tag: anchorTag,
			RefValue: v, CerberusValue: missingTagValue,
			Reason: fmt.Sprintf("tag %q present in reference only", v), ReasonCode: reasonCode,
		}
	}
	if ambiguous > 0 {
		out.Limitations = append(out.Limitations, limitation(LimitTagDiscoveryRefCardinalityUnknown, ambiguous))
		if out.Verdict == VerdictMatch {
			out.Verdict = VerdictUndecidable
		}
	}
	return out
}

// diffTagScopes compares the v2 tag-NAMES response scope by scope, over the
// fixed canonical scope order (see tempoTagScopes) so a scope neither
// response mentions is treated as empty rather than skipped, and the first
// divergence reported is stable across runs.
func diffTagScopes(ref, cer map[string][]string) Outcome {
	out := Outcome{Verdict: VerdictMatch}
	for _, scope := range tempoTagScopes {
		scopeOut := diffTagSet(ref[scope], cer[scope], "", scope, ReasonTagMissing)
		out.Compared += scopeOut.Compared
		out.Limitations = mergeLimitations(out.Limitations, scopeOut.Limitations)
		switch {
		case scopeOut.Verdict == VerdictDiverge && out.Verdict != VerdictDiverge:
			out.Verdict, out.FirstDiff = scopeOut.Verdict, scopeOut.FirstDiff
		case scopeOut.Verdict == VerdictUndecidable && out.Verdict == VerdictMatch:
			out.Verdict = VerdictUndecidable
		}
	}
	return out
}

// diffTagValuesV2 compares the v2 tag-values response: value-set equality
// exactly as diffTagSet, PLUS an exact type check on every value present on
// both sides. A type mismatch on a value both sides agree exists is its own
// divergence, reported before the plain missing-value scan so a genuine
// type-encoding bug is never masked by a coincidental membership match.
func diffTagValuesV2(ref, cer map[string]string, tag string) Outcome {
	refVals := make([]string, 0, len(ref))
	for v := range ref {
		refVals = append(refVals, v)
	}
	cerVals := make([]string, 0, len(cer))
	for v := range cer {
		cerVals = append(cerVals, v)
	}
	sort.Strings(refVals)
	sort.Strings(cerVals)

	both := make([]string, 0, len(ref))
	for _, v := range refVals {
		if _, ok := cer[v]; ok {
			both = append(both, v)
		}
	}
	for _, v := range both {
		if rt, ct := ref[v], cer[v]; rt != ct {
			return Outcome{
				Verdict:  VerdictDiverge,
				Compared: len(unionKeys(toStringSet(refVals), toStringSet(cerVals))),
				FirstDiff: &FirstDiff{
					Kind: KindTagDiscovery, Tag: tag,
					RefValue: rt, CerberusValue: ct,
					Reason: fmt.Sprintf("tag value %q type differs", v), ReasonCode: ReasonTagTypeDiffers,
				},
			}
		}
	}
	return diffTagSet(refVals, cerVals, tag, "", ReasonTagValueMissing)
}

// toStringSet builds a lookup set from a value slice, dropping the empty
// string (neither backend's discovery surface returns it as a real value).
func toStringSet(vs []string) map[string]struct{} {
	out := make(map[string]struct{}, len(vs))
	for _, v := range vs {
		if v == "" {
			continue
		}
		out[v] = struct{}{}
	}
	return out
}

// unionKeys returns the sorted union of two string sets, so a diff scan
// (and the Compared count) is deterministic across runs.
func unionKeys(a, b map[string]struct{}) []string {
	seen := make(map[string]struct{}, len(a)+len(b))
	out := make([]string, 0, len(a)+len(b))
	for v := range a {
		seen[v] = struct{}{}
		out = append(out, v)
	}
	for v := range b {
		if _, ok := seen[v]; !ok {
			out = append(out, v)
		}
	}
	sort.Strings(out)
	return out
}
