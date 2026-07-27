package tempo

import (
	"context"
	"strings"
	"testing"
	"time"

	"github.com/tsouza/cerberus/internal/api/format"
	"github.com/tsouza/cerberus/internal/engine"
	"github.com/tsouza/cerberus/internal/schema"
)

// ExecMetricsRange is the gRPC sibling of handleMetricsQueryRange, and the two
// have to agree on the request-window contract. The HTTP surface aligns the
// window onto the step grid and refuses a matrix wider than
// format.MaxResolutionPoints; when the gRPC export skipped both, it returned
// samples off Tempo's step grid and let an unauthenticated caller ask for an
// unbounded fan-out (compute-DoS) through a path no HTTP test covers.
//
// The cap sits BEFORE the parse stage on purpose — rejecting only after
// parsing would still pay for parsing an arbitrarily large request. The
// unparseable query below is the probe for that ordering: it can only produce
// a cap error if the cap ran first.
func TestExecMetricsRangeBoundsTheMatrixBeforeParsing(t *testing.T) {
	t.Parallel()

	// An empty Engine is enough: ObserveCapRejection short-circuits on a nil
	// QueryObserver, and the cap returns before any engine work happens.
	h := &Handler{Engine: &engine.Engine{}, Schema: schema.DefaultOTelTraces()}

	const step = time.Second
	// A query that cannot parse. If the error mentions the resolution cap, the
	// cap necessarily ran before parseExpr.
	const unparseable = "{ this is not traceql"

	start := time.Unix(0, 0).UTC()
	overCap := start.Add((format.MaxResolutionPoints + 1) * step)

	if _, err := h.ExecMetricsRange(context.Background(), unparseable, start, overCap, step); err == nil {
		t.Fatal("ExecMetricsRange accepted a matrix wider than the resolution cap")
	} else if !strings.Contains(err.Error(), format.ResolutionCapMessage) {
		t.Fatalf("window of %d steps was not rejected by the resolution cap: %v",
			format.MaxResolutionPoints+1, err)
	}

	// Control: the same unparseable query inside the cap must fail at PARSE,
	// not at the cap. Without this the assertion above would also pass if
	// ExecMetricsRange simply reported the cap message for every error.
	underCap := start.Add((format.MaxResolutionPoints - 1) * step)
	_, err := h.ExecMetricsRange(context.Background(), unparseable, start, underCap, step)
	if err == nil {
		t.Fatal("ExecMetricsRange accepted an unparseable query")
	}
	if strings.Contains(err.Error(), format.ResolutionCapMessage) {
		t.Fatalf("a window inside the cap was still rejected by the cap: %v", err)
	}
}

// The gRPC export must anchor the window to the same step grid the HTTP
// handler uses, so a caller driving both surfaces gets samples at identical
// timestamps.
//
// Alignment is not directly observable from ExecMetricsRange's return value,
// so this reads it through the cap boundary instead. alignMetricsWindow only
// ever WIDENS a window (start truncates down, end rounds up), so a window that
// is off-grid and exactly at the cap when raw is over the cap once aligned.
// The cap error therefore proves alignment ran inside ExecMetricsRange —
// without it the raw ratio stays at the limit and the call reaches the parser.
func TestExecMetricsRangeAlignsTheWindowToTheStepGrid(t *testing.T) {
	t.Parallel()

	h := &Handler{Engine: &engine.Engine{}, Schema: schema.DefaultOTelTraces()}

	const step = time.Minute
	// 30s past a step boundary, so both ends move when aligned.
	const offGridSeconds = 30

	rawStart := time.Unix(offGridSeconds, 0).UTC()
	// Exactly MaxResolutionPoints steps wide: `ratio > cap` is false, so the
	// RAW window passes the cap untouched.
	rawEnd := rawStart.Add(format.MaxResolutionPoints * step)
	if rawEnd.Sub(rawStart)/step > format.MaxResolutionPoints {
		t.Fatalf("test setup is wrong: the raw window already busts the cap")
	}

	_, err := h.ExecMetricsRange(context.Background(), "{ this is not traceql", rawStart, rawEnd, step)
	if err == nil {
		t.Fatal("ExecMetricsRange accepted an unparseable query")
	}
	if !strings.Contains(err.Error(), format.ResolutionCapMessage) {
		t.Fatalf("off-grid window was not aligned before the cap — the raw window passed straight "+
			"through to the parser, so gRPC and HTTP disagree on the sample grid: %v", err)
	}
}
