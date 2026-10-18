package promql

import (
	"context"
	"time"

	"github.com/prometheus/prometheus/model/labels"
	"github.com/prometheus/prometheus/promql/parser"

	"github.com/tsouza/cerberus/internal/chplan"
	"github.com/tsouza/cerberus/internal/schema"
)

// maxSampleReadSpanMillis is the widest window one sample-read selector
// covers, in milliseconds. A selector's range is a time.Duration, which
// names at most about 292 years, while the storage domain spans about
// 362, so [SampleReads] splits a wider window into consecutive pieces.
const maxSampleReadSpanMillis = int64(100 * 365 * 24 * time.Hour / time.Millisecond)

// maxSampleReads is how many pieces of at most maxSampleReadSpanMillis
// cover the whole storage domain, and so the most [SampleReads] ever
// builds. Bounding the loop by it keeps the piece count finite whatever
// the window arithmetic does.
var maxSampleReads = (timeMillis(StorageDomainMax)-timeMillis(StorageDomainMin))/(maxSampleReadSpanMillis+1) + 1

// SampleReads builds the pinned range selectors that read every stored
// sample matching matchers whose timestamp lies in
// (startMs - 1ms, endMs]: `{matchers}[to-from+1ms] @ to` for consecutive
// pieces [from, to] of the window. Each piece's window starts exactly
// where the previous one's ends, so together they read every sample in
// the window once.
//
// The window is first clipped to the storage domain
// ([StorageDomainMin], [StorageDomainMax]), outside which no sample can
// be stored, so the result is empty when nothing of it remains.
//
// The pieces are lowered with [LowerSampleRead], not
// [LowerAtRangeOpts]: a read reaching the storage domain's edges leaves
// the evaluation domain, but a pinned range selector needs no more of
// the lowering's time arithmetic than DateTime64(9) represents exactly.
// Its pin is a parsed DateTime64(9) literal inside the storage domain,
// and its lower bound is ClickHouse arithmetic on that literal, which is
// exact below 1900 too.
func SampleReads(matchers []*labels.Matcher, startMs, endMs int64) []*parser.MatrixSelector {
	startMs = max(startMs, timeMillis(StorageDomainMin))
	endMs = min(endMs, timeMillis(StorageDomainMax))
	var reads []*parser.MatrixSelector
	from := startMs
	for range maxSampleReads {
		if from <= endMs {
			to := min(endMs, from+maxSampleReadSpanMillis)
			pin := to
			reads = append(reads, &parser.MatrixSelector{
				VectorSelector: &parser.VectorSelector{
					Name:          metricNameFromMatchers(matchers),
					LabelMatchers: matchers,
					Timestamp:     &pin,
				},
				Range: time.Duration(to-from+1) * time.Millisecond,
			})
			from = to + 1
		}
	}
	return reads
}

// LowerSampleRead lowers one selector [SampleReads] built. It is the one
// lowering entry point that does not apply the evaluation-domain check —
// see [SampleReads] for why the read is exact anyway.
func LowerSampleRead(ctx context.Context, sel *parser.MatrixSelector, s schema.Metrics, opts LowerOpts) (chplan.Node, error) {
	vs, _ := sel.VectorSelector.(*parser.VectorSelector)
	pin := atModifierInstant(*vs.Timestamp)
	return lowerAtRangeOpts(ctx, sel, s, pin, pin, 0, opts)
}
