package chopt

// CancellationGap records ClickHouse functions whose single call ignored
// KILL QUERY and max_execution_time before a given build. ClickHouse checks a
// query's cancellation between pipeline blocks; a function that did not also
// check inside its own loop ran to completion once started, so a query that
// timed out or was cancelled mid-call kept a server thread busy after the
// client had already been answered and its admission released. Cerberus emits
// these functions itself: arrayFold in range-window folds (for example
// double_exponential_smoothing) and in LogQL pattern filters, and
// replaceRegexpAll / replaceRegexpOne in every PromQL label-name normalization
// and LogQL label conversion.
type CancellationGap struct {
	// Functions are the ClickHouse functions the fix made interruptible.
	Functions []string
	// Fixed is the first build carrying the in-function cancellation check.
	Fixed Version
	// Defect names the upstream change.
	Defect string
}

// cancellationGaps are the verified gaps. Each boundary is the upstream merge
// build; test/chserver's real-server cancellation test reproduces the gap
// on the last release before it and bounded cancellation on a release after it.
var cancellationGaps = []CancellationGap{
	{
		Functions: []string{"arrayFold"},
		Fixed:     Version{Major: 26, Minor: 7, Patch: 1, Build: 446},
		Defect:    "ClickHouse#108192",
	},
	{
		Functions: []string{"replaceAll", "replaceOne", "replaceRegexpAll", "replaceRegexpOne"},
		Fixed:     Version{Major: 26, Minor: 8, Patch: 1, Build: 581},
		Defect:    "ClickHouse#112483",
	},
}

// CancellationGaps returns the gaps server still carries: on such a build a
// cerberus timeout or cancellation answers the client on time, but the server
// may keep evaluating one in-flight call of a listed function until it
// finishes. The supported-version policy for bounded server-side cancellation
// of cerberus-emitted expressions is a build with no remaining gap.
func CancellationGaps(server Version) []CancellationGap {
	var out []CancellationGap
	for _, g := range cancellationGaps {
		if !server.AtLeast(g.Fixed) {
			out = append(out, g)
		}
	}
	return out
}
