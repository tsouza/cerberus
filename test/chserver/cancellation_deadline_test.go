package chserver

import (
	"errors"
	"fmt"
	"testing"
	"time"
)

// The modelled array_fold query the request-deadline tests drive: the
// calibration of array_fold at size 200000 on 26.8.10.6 in a GitHub Actions
// run, entering the call after about 0.8s of a 3.4s run whose loop performs
// 1199989 function executions at an even rate.
const (
	modelEntered = 800 * time.Millisecond
	modelCall    = 2600 * time.Millisecond
	modelLoop    = 1_199_989
	// modelBase is the function executions the query has performed when the
	// fold's input arrives: one per block for every function ahead of it.
	modelBase = 300
	// modelInterruptLag is how long an interrupted loop runs on past the
	// cancellation before it checks for it.
	modelInterruptLag = pollInterval
)

// modelQuery is a request-deadline dispatch's query without a server. Each
// dispatch enters the call after the next of entries (the last repeats); the
// loop then performs modelLoop executions at an even rate over modelCall. A
// deadline before the entry stops the query ahead of the call. A deadline
// inside it stops the loop after modelInterruptLag when interruptible, and
// otherwise the loop runs out.
type modelQuery struct {
	t             *testing.T
	entries       []time.Duration
	interruptible bool
	deadlines     []time.Duration
	landings      []deadlineLanding
	discards      int
	final         uint64
}

func (q *modelQuery) executionsAt(entry, at time.Duration) uint64 {
	if at < entry {
		return uint64(float64(modelBase) * float64(at) / float64(entry))
	}
	return modelBase + min(uint64(float64(modelLoop)*float64(at-entry)/float64(modelCall)), modelLoop)
}

func (q *modelQuery) dispatch(deadline time.Duration) []callProgress {
	entry := q.entries[min(len(q.deadlines), len(q.entries)-1)]
	q.deadlines = append(q.deadlines, deadline)
	if deadline >= entry+modelCall {
		// The query would finish and the request succeed, so the real probe
		// would fail on the answer rather than judge a cancellation.
		q.t.Fatalf("deadline %s passes after the query entering at %s has finished", deadline, entry)
	}
	var seen []callProgress
	for at := pollInterval; at < deadline; at += pollInterval {
		p := callProgress{elapsed: at, functionExecutions: q.executionsAt(entry, at)}
		if at >= entry {
			p.filterPassedRows = 1
		}
		seen = append(seen, p)
	}
	switch {
	case deadline < entry:
		q.final = q.executionsAt(entry, deadline)
	case q.interruptible:
		q.final = q.executionsAt(entry, deadline+modelInterruptLag)
	default:
		q.final = modelBase + modelLoop
	}
	q.landings = append(q.landings, modelWant(true).landing(foldLoopRunning, seen))
	return seen
}

func (q *modelQuery) discard() { q.discards++ }

func modelWant(bounded bool) cancelExpectation {
	return cancelExpectation{
		bounded:        bounded,
		meter:          loopExecutions,
		naturalProfile: naturalProfile{natural: modelEntered + modelCall, entered: modelEntered, loopExecutions: modelLoop},
	}
}

// probeModel runs the request-deadline probe's logic against q with the
// build table's expectation bounded, and returns the verdict on the dispatch
// it judged.
func probeModel(t *testing.T, q *modelQuery, bounded bool) loopJudgement {
	t.Helper()
	want := modelWant(bounded)
	_, seen, err := want.landRequestDeadline(foldLoopRunning, q.dispatch, q.discard)
	if err != nil {
		t.Fatalf("landRequestDeadline: %v", err)
	}
	if q.discards != len(q.deadlines)-1 {
		t.Errorf("discarded %d of %d dispatches; want every one but the judged one", q.discards, len(q.deadlines))
	}
	j, err := want.judgeLoop(seen, q.final)
	if err != nil {
		t.Fatalf("judgeLoop: %v", err)
	}
	return j
}

// A query that reaches the call later than the calibrated deadline is
// judged on a re-armed deadline, and the verdict still separates the builds:
// an interruptible call passes only where the build table says it is
// interrupted, and a call that cannot be interrupted fails there.
func TestLandRequestDeadline_DelayedEntryKeepsTheVerdict(t *testing.T) {
	want := modelWant(true)
	step := want.deadlineStep()
	maxDelay := (requestDeadlineAttempts - 2) * step
	lateDelays := 0
	for delay := time.Duration(0); delay <= maxDelay; delay += pollInterval {
		entries := []time.Duration{modelEntered + delay}
		t.Run(fmt.Sprintf("delay_%s", delay), func(t *testing.T) {
			var deadlines []time.Duration
			for _, tc := range []struct {
				interruptible, bounded, pass bool
			}{
				{interruptible: true, bounded: true, pass: true},
				{interruptible: false, bounded: true, pass: false},
				{interruptible: false, bounded: false, pass: true},
				{interruptible: true, bounded: false, pass: false},
			} {
				q := &modelQuery{t: t, entries: entries, interruptible: tc.interruptible}
				j := probeModel(t, q, tc.bounded)
				if j.ok != tc.pass {
					t.Errorf("interruptible=%v bounded=%v: verdict ok=%v (left %d, after %d, bound %.0f); want %v",
						tc.interruptible, tc.bounded, j.ok, j.left, j.after, j.bound, tc.pass)
				}
				// Where the deadlines land is read before each cancellation,
				// so it cannot depend on what the call does after one.
				if deadlines == nil {
					deadlines = q.deadlines
				} else if fmt.Sprint(q.deadlines) != fmt.Sprint(deadlines) {
					t.Errorf("interruptible=%v bounded=%v dispatched deadlines %v; another model dispatched %v",
						tc.interruptible, tc.bounded, q.deadlines, deadlines)
				}
				if q.landings[0] == landedBeforeCall {
					lateDelays++
				}
			}
		})
	}
	// The sweep must reach the defect: entries the calibrated deadline
	// passes ahead of, which a single dispatch could not judge.
	if lateDelays == 0 {
		t.Fatalf("no delay up to %s made the calibrated deadline %s pass before the call", maxDelay, want.requestDeadline())
	}
}

// A dispatch whose deadline lands too late in the call is re-armed from the
// entry it observed, not stepped further.
func TestLandRequestDeadline_PastFloorRearmsFromObservedEntry(t *testing.T) {
	step := modelWant(true).deadlineStep()
	// The first query enters a step and a half late; the next ones half a
	// step early, so the stepped deadline passes past the floor.
	entries := []time.Duration{modelEntered + step + step/2, modelEntered - step/2}
	for _, interruptible := range []bool{true, false} {
		q := &modelQuery{t: t, entries: entries, interruptible: interruptible}
		j := probeModel(t, q, true)
		wantLandings := []deadlineLanding{landedBeforeCall, landedPastFloor, landedInWindow}
		if fmt.Sprint(q.landings) != fmt.Sprint(wantLandings) {
			t.Fatalf("interruptible=%v: landings %v; want %v", interruptible, q.landings, wantLandings)
		}
		if j.ok != interruptible {
			t.Errorf("interruptible=%v: verdict ok=%v on a build the table says interrupts the call", interruptible, j.ok)
		}
	}
}

// A query that never reaches the call within the attempts fails the probe
// rather than passing unjudged.
func TestLandRequestDeadline_NeverInCallFails(t *testing.T) {
	want := modelWant(true)
	// Later than the last deadline the attempts can arm.
	never := want.requestDeadline() + requestDeadlineAttempts*want.deadlineStep()
	q := &modelQuery{t: t, entries: []time.Duration{never}, interruptible: true}
	_, _, err := want.landRequestDeadline(foldLoopRunning, q.dispatch, q.discard)
	if err == nil {
		t.Fatal("landRequestDeadline judged a query that never reached the call")
	}
	if len(q.deadlines) != requestDeadlineAttempts || q.discards != requestDeadlineAttempts {
		t.Errorf("dispatched %d and discarded %d; want %d of each", len(q.deadlines), q.discards, requestDeadlineAttempts)
	}
}

// A calibration that leaves the call no window fails before any dispatch.
func TestLandRequestDeadline_NoWindowFails(t *testing.T) {
	want := modelWant(true)
	want.natural = want.entered
	q := &modelQuery{t: t, entries: []time.Duration{modelEntered}}
	if _, _, err := want.landRequestDeadline(foldLoopRunning, q.dispatch, q.discard); err == nil || len(q.deadlines) != 0 {
		t.Errorf("landRequestDeadline = %v after %d dispatches; want an error before any", err, len(q.deadlines))
	}
}

// judgeLoop refuses a statement cancelled with less than the floor left.
func TestJudgeLoop_PastFloorIsUnjudgeable(t *testing.T) {
	want := modelWant(true)
	seen := []callProgress{
		{elapsed: modelEntered, filterPassedRows: 1, functionExecutions: modelBase},
		{elapsed: modelEntered + modelCall, filterPassedRows: 1, functionExecutions: modelBase + modelLoop},
	}
	if _, err := want.judgeLoop(seen, modelBase+modelLoop); !errors.Is(err, errLoopUnjudgeable) {
		t.Errorf("judgeLoop = %v; want errLoopUnjudgeable", err)
	}
}

// The modelled regex_replace query: it reads its one label for
// modelRegexRead, then runs replaceRegexpAll as a single function execution
// until modelRegexNatural.
const (
	modelRegexRead    = time.Second
	modelRegexNatural = 14 * time.Second
	modelRegexRows    = 1
)

// regexReadings are the readings a regex_replace query whose start is
// delayed by delay shows before deadline: its read progresses and executes
// one function per reading, and then the call holds the count still.
func regexReadings(delay, deadline time.Duration) []callProgress {
	var seen []callProgress
	for at := pollInterval; at < deadline; at += pollInterval {
		run := at - delay
		p := callProgress{elapsed: at}
		if run > 0 {
			read := min(run, modelRegexRead)
			p.totalRows = modelRegexRows
			p.readRows = uint64(float64(modelRegexRows) * float64(read) / float64(modelRegexRead))
			p.functionExecutions = uint64(read / pollInterval)
		}
		seen = append(seen, p)
	}
	return seen
}

// Under wallClock a delayed entry is re-armed past as well, and the deadline
// it lands still leaves the minRemaining of server time the verdict judges.
func TestLandRequestDeadline_WallClockDelayedEntry(t *testing.T) {
	entered, ok := callEntry(regexCallRunning, regexReadings(0, modelRegexNatural))
	if !ok {
		t.Fatal("the undelayed modelled query never entered its call")
	}
	want := cancelExpectation{
		bounded: true, meter: wallClock,
		naturalProfile: naturalProfile{natural: modelRegexNatural, entered: entered},
	}
	step := want.deadlineStep()
	lateDelays := 0
	for delay := time.Duration(0); delay <= step; delay += pollInterval {
		var landings []deadlineLanding
		dispatch := func(deadline time.Duration) []callProgress {
			if deadline >= delay+modelRegexNatural {
				t.Fatalf("delay %s: deadline %s passes after the query has finished", delay, deadline)
			}
			seen := regexReadings(delay, deadline)
			landings = append(landings, want.landing(regexCallRunning, seen))
			return seen
		}
		deadline, _, err := want.landRequestDeadline(regexCallRunning, dispatch, func() {})
		if err != nil {
			t.Fatalf("delay %s: %v", delay, err)
		}
		if landings[0] == landedBeforeCall {
			lateDelays++
		}
		if remaining := delay + modelRegexNatural - deadline; remaining < minRemaining {
			t.Errorf("delay %s: deadline %s leaves %s of the run; want at least %s", delay, deadline, remaining, minRemaining)
		}
	}
	if lateDelays == 0 {
		t.Fatalf("no delay up to %s made the calibrated deadline %s pass before the call", step, want.requestDeadline())
	}
}
