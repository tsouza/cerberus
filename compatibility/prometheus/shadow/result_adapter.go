package shadow

import (
	"github.com/prometheus/prometheus/model/labels"

	"github.com/tsouza/cerberus/internal/promshim/local"
)

// ResultToVector adapts a promshim/local Result into the shadow VectorResult
// shape that the differ understands. It is the canonical translation used by
// both the shadow CLI (oracle wiring) and the in-package PromQL shadow tests.
//
// Scalars are surfaced as a single series with an empty label set, mirroring
// the Prometheus HTTP response convention.
func ResultToVector(r local.Result) VectorResult {
	switch r.Kind {
	case local.ResultKindVector:
		out := VectorResult{Series: make([]Series, 0, len(r.Vector))}
		for _, s := range r.Vector {
			out.Series = append(out.Series, Series{
				Labels:  LabelsToMap(s.Metric),
				Samples: []Sample{{TimestampMs: s.T, Value: s.V}},
			})
		}
		return out
	case local.ResultKindMatrix:
		out := VectorResult{Series: make([]Series, 0, len(r.Matrix))}
		for _, m := range r.Matrix {
			samples := make([]Sample, 0, len(m.Points))
			for _, p := range m.Points {
				samples = append(samples, Sample{TimestampMs: p.T, Value: p.V})
			}
			out.Series = append(out.Series, Series{
				Labels:  LabelsToMap(m.Metric),
				Samples: samples,
			})
		}
		return out
	case local.ResultKindScalar:
		if r.Scalar == nil {
			return VectorResult{}
		}
		return VectorResult{Series: []Series{{
			Labels:  map[string]string{},
			Samples: []Sample{{TimestampMs: r.Scalar.T, Value: r.Scalar.V}},
		}}}
	}
	return VectorResult{}
}

// LabelsToMap flattens a labels.Labels into a name→value map keyed by label
// name. Companion to ResultToVector; exported for callers that build their own
// expected VectorResults from raw Prometheus label sets.
func LabelsToMap(ls labels.Labels) map[string]string {
	out := make(map[string]string, ls.Len())
	ls.Range(func(l labels.Label) { out[l.Name] = l.Value })
	return out
}
