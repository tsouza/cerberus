package chsql

import (
	"context"
	"errors"
	"fmt"
	"strings"
	"testing"
	"time"

	"github.com/tsouza/cerberus/internal/chplan"
)

// A grid about 328 years wide: every instant inside DateTime64(9)'s range,
// the span further than a time.Duration or Int64 nanoseconds hold.
var (
	wideGridStart = time.Date(1698, 1, 1, 0, 0, 0, 0, time.UTC)
	wideGridEnd   = time.Date(2025, 11, 8, 0, 0, 0, 0, time.UTC)
)

const wideGridStep = 100 * 24 * time.Hour

// wideGridAnchors returns the end-inclusive anchor count of the wide grid,
// counted in whole days so no time.Duration is involved.
func wideGridAnchors(t *testing.T) int64 {
	t.Helper()
	const secondsPerDay = 24 * 60 * 60
	return (wideGridEnd.Unix()-wideGridStart.Unix())/secondsPerDay/100 + 1
}

func TestNewAnchorGrid_PicksUnit(t *testing.T) {
	t.Parallel()
	step, reach := wideGridStep.Nanoseconds(), time.Hour.Nanoseconds()
	for _, tc := range []struct {
		name               string
		stepNS, reachNS, n int64
		want               anchorGrid
	}{
		{"span fits", step, reach, 100, anchorGrid{unit: 1, step: step}},
		{"span exceeds Int64 nanoseconds", step, reach, wideGridAnchors(t), anchorGrid{unit: nanosPerMilli, step: step / nanosPerMilli}},
		{"reach tips it over", 4e18, 6e18, 2, anchorGrid{unit: nanosPerMilli, step: 4e18 / nanosPerMilli}},
		{"step not whole milliseconds", step + 1, reach, wideGridAnchors(t), anchorGrid{unit: 1, step: step + 1}},
		{"reach not whole milliseconds", step, reach + 1, wideGridAnchors(t), anchorGrid{unit: 1, step: step}},
	} {
		if got := newAnchorGrid(tc.stepNS, tc.reachNS, tc.n); got != tc.want {
			t.Errorf("%s: newAnchorGrid = %+v, want %+v", tc.name, got, tc.want)
		}
	}
}

func TestAnchorGrid_Rendering(t *testing.T) {
	t.Parallel()
	base, idx, ts := BareIdent("base"), BareIdent("i"), BareIdent("ts")
	ns := anchorGrid{unit: 1, step: 60_000_000_000}
	ms := anchorGrid{unit: nanosPerMilli, step: 60_000}
	for _, tc := range []struct {
		name string
		got  Frag
		want string
	}{
		{"ns back", ns.back(base, idx), "base - toIntervalNanosecond(i * 60000000000)"},
		{"ns ahead", ns.ahead(base, idx), "base + toIntervalNanosecond(i * 60000000000)"},
		{"ns dist", ns.dist(ts, base), "dateDiff('nanosecond', ts, base)"},
		{"ns earliest", ns.earliest(base, 4), "base - toIntervalNanosecond(180000000000)"},
		{"ms back", ms.back(base, idx), "fromUnixTimestamp64Nano((toUnixTimestamp64Milli(base) - i * 60000) * 1000000)"},
		{"ms ahead", ms.ahead(base, idx), "fromUnixTimestamp64Nano((toUnixTimestamp64Milli(base) + i * 60000) * 1000000)"},
		{"ms dist", ms.dist(ts, base), "(toUnixTimestamp64Milli(base) - toUnixTimestamp64Milli(ts))"},
		{"ms earliest", ms.earliest(base, 4), "fromUnixTimestamp64Nano((toUnixTimestamp64Milli(base) - 180000) * 1000000)"},
	} {
		if got := renderFragToSQL(tc.got); got != tc.want {
			t.Errorf("%s:\n got %s\nwant %s", tc.name, got, tc.want)
		}
	}
	if ms.units(5*nanosPerMilli) != 5 || ns.units(5) != 5 {
		t.Errorf("units: ms %d, ns %d; want 5, 5", ms.units(5*nanosPerMilli), ns.units(5))
	}
}

func TestGridAnchorCount(t *testing.T) {
	t.Parallel()
	if got, err := gridAnchorCount(wideGridStart, wideGridEnd, wideGridStep, "Node"); err != nil || got != wideGridAnchors(t) {
		t.Errorf("wide grid: gridAnchorCount = %d, %v; want %d", got, err, wideGridAnchors(t))
	}
	if got, err := gridAnchorCount(time.Time{}, wideGridEnd, wideGridStep, "Node"); err != nil || got != 1 {
		t.Errorf("unset start: gridAnchorCount = %d, %v; want 1", got, err)
	}
	if _, err := gridAnchorCount(wideGridEnd, wideGridStart, wideGridStep, "Node"); !errors.Is(err, ErrUnsupported) || !strings.Contains(err.Error(), "Node.Start > End") {
		t.Errorf("start after end: err = %v; want ErrUnsupported naming Node", err)
	}
}

func TestOuterAnchorCount_WideGrid(t *testing.T) {
	t.Parallel()
	r := &chplan.RangeWindow{Start: wideGridStart, End: wideGridEnd, Step: wideGridStep, OuterRange: wideGridEnd.Sub(wideGridStart)}
	if got := outerAnchorCount(r); got != wideGridAnchors(t) {
		t.Errorf("outerAnchorCount = %d, want %d", got, wideGridAnchors(t))
	}
	if got, err := rangeWindowGridAnchorCount(r); err != nil || got != wideGridAnchors(t) {
		t.Errorf("rangeWindowGridAnchorCount (outer range) = %d, %v; want %d", got, err, wideGridAnchors(t))
	}
	r.OuterRange = 0
	if got, err := rangeWindowGridAnchorCount(r); err != nil || got != wideGridAnchors(t) {
		t.Errorf("rangeWindowGridAnchorCount (bounds) = %d, %v; want %d", got, err, wideGridAnchors(t))
	}
}

// TestStepAlignedAnchorCountFor_WideSpan pins the epoch-aligned count of a
// window wider than a time.Duration: the whole steps, plus one when the
// remainder passes the phase offset.
func TestStepAlignedAnchorCountFor_WideSpan(t *testing.T) {
	t.Parallel()
	const step = time.Minute
	end := time.Date(2026, time.January, 1, 1, 4, 41, 0, time.UTC) // δ = 41s
	const steps = int64(200_000_000)                               // about 380 years of minutes
	if got := stepAlignedAnchorCountFor(end, 0, steps, 42*time.Second, step.Nanoseconds(), 7); got != steps+1 {
		t.Errorf("remainder past δ: got %d, want %d", got, steps+1)
	}
	if got := stepAlignedAnchorCountFor(end, 0, steps, 41*time.Second, step.Nanoseconds(), 7); got != steps {
		t.Errorf("remainder at δ: got %d, want %d", got, steps)
	}
}

func TestStartAnchoredGridEnd_WideGrid(t *testing.T) {
	t.Parallel()
	n := wideGridAnchors(t)
	want := wideGridStart.AddDate(0, 0, int((n-1)*100))
	if got := startAnchoredGridEnd(wideGridStart, wideGridEnd, wideGridStep.Nanoseconds(), n); !got.Equal(want) {
		t.Errorf("startAnchoredGridEnd = %s, want %s", got, want)
	}
}

// TestEmitWideGrid_CountsEveryAnchor emits each grid shape over the wide
// grid and checks it fans out to every anchor in millisecond arithmetic.
func TestEmitWideGrid_CountsEveryAnchor(t *testing.T) {
	t.Parallel()
	n := wideGridAnchors(t)
	variants := fusedTestWindow()
	variants.Start, variants.End, variants.Step = wideGridStart, wideGridEnd, wideGridStep
	variants.OuterRange = wideGridEnd.Sub(wideGridStart)
	for _, tc := range []struct {
		name string
		node chplan.Node
		fan  string
	}{
		{"StepGrid", &chplan.StepGrid{Start: wideGridStart, End: wideGridEnd, Step: wideGridStep}, fmt.Sprintf("range(0, %d)", n)},
		{"AbsentOverTime", &chplan.AbsentOverTime{
			Input:            closedRoleProject(chplan.Column{Name: "TimeUnix", Role: chplan.RoleTimestamp}),
			Range:            time.Hour,
			Start:            wideGridStart,
			End:              wideGridEnd,
			Step:             wideGridStep,
			TimestampColumn:  "TimeUnix",
			MetricNameColumn: "MetricName",
			AttributesColumn: "Attributes",
			ValueColumn:      "Value",
		}, fmt.Sprintf("range(0, %d)", n)},
		{"variants RangeWindow", variants, fmt.Sprintf("least(%d, ", n)},
	} {
		sql, _, err := Emit(context.Background(), tc.node)
		if err != nil {
			t.Errorf("%s: Emit: %v", tc.name, err)
			continue
		}
		if !strings.Contains(sql, tc.fan) {
			t.Errorf("%s: want fan-out %s\nSQL: %s", tc.name, tc.fan, sql)
		}
		if !strings.Contains(sql, "toUnixTimestamp64Milli(") || strings.Contains(sql, "toIntervalNanosecond(i *") {
			t.Errorf("%s: want millisecond grid arithmetic\nSQL: %s", tc.name, sql)
		}
	}
}
