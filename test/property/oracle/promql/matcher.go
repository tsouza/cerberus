package promql

import (
	"github.com/prometheus/prometheus/model/labels"
)

// matchSeries reports whether the series satisfies every matcher.
// The matcher walk uses labels.Matcher.Matches, which already handles
// the four Prom matcher kinds (=, !=, =~, !~) correctly. We just feed
// it the label value the matcher names (or "" if absent — the empty
// string is the Prom convention for "label not present").
func matchSeries(s *Series, matchers []*labels.Matcher) bool {
	for _, m := range matchers {
		v := s.Labels[m.Name]
		if !m.Matches(v) {
			return false
		}
	}
	return true
}
