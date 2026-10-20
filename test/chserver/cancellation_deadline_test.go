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

// modelDispatch is one request-deadline dispatch against a modelled server.
// The query's reading after run of server progress is at(run), and it
// finishes uncancelled once run reaches length. Server progress keeps pace
// with the wall clock except while the dispatch holds the server. The query
// starts on the server once run reaches started, and its system.processes
// elapsed counts from then. While run lies in [stallFrom, stallUntil) its
// row is not seen, as when the probe's reads stall behind the server.
type modelDispatch struct {
	at                    func(run time.Duration) callProgress
	started, length       time.Duration
	stallFrom, stallUntil time.Duration
	wall, run             time.Duration
	held                  bool
}

func (m *modelDispatch) sinceSent() time.Duration { return m.wall }

func (m *modelDispatch) await() bool {
	m.wall += pollInterval
	m.run += pollInterval
	return m.run < m.length
}

func (m *modelDispatch) read() (callProgress, bool) {
	p := m.at(m.run)
	p.elapsed = m.wall - m.started
	stalled := m.run >= m.stallFrom && m.run < m.stallUntil
	return p, m.run < m.length && m.run >= m.started && !stalled
}

func (m *modelDispatch) hold(span time.Duration) {
	m.wall += span
	m.held = true
}

// observe runs observeToDeadline on m and returns its readings with the
// server progress the query had made when the deadline cancelled it: where
// the hold stopped it, or the deadline itself when nothing held it.
func (m *modelDispatch) observe(t *testing.T, inCall func([]callProgress) bool, deadline time.Duration) ([]callProgress, time.Duration) {
	t.Helper()
	seen := observeToDeadline(inCall, deadline, m)
	if !m.held {
		if deadline >= m.length {
			// The query would finish and the request succeed, so the real
			// probe would fail on the answer rather than judge a cancellation.
			t.Fatalf("deadline %s passes after the query has finished at %s", deadline, m.length)
		}
		m.run = deadline
	}
	return seen, m.run
}

// modelQuery dispatches the modelled array_fold query. Each dispatch enters
// the call after the next of entries (the last repeats); the loop then
// performs modelLoop executions at an even rate over modelCall of server
// progress. A deadline that cancels the query before the entry stops it ahead
// of the call. One inside the call stops the loop modelInterruptLag later
// when interruptible, and otherwise the loop runs out. The reads of the first
// stalled dispatches stall for stall from just after the entry.
type modelQuery struct {
	t             *testing.T
	entries       []time.Duration
	stall         time.Duration
	stalled       int
	interruptible bool
	deadlines     []time.Duration
	landings      []deadlineLanding
	discards      int
	final         uint64
}

func executionsAt(entry, run time.Duration) uint64 {
	if run < entry {
		return uint64(float64(modelBase) * float64(run) / float64(entry))
	}
	return modelBase + min(uint64(float64(modelLoop)*float64(run-entry)/float64(modelCall)), modelLoop)
}

func (q *modelQuery) dispatch(deadline time.Duration) []callProgress {
	n := len(q.deadlines)
	entry := q.entries[min(n, len(q.entries)-1)]
	q.deadlines = append(q.deadlines, deadline)
	m := &modelDispatch{
		at: func(run time.Duration) callProgress {
			p := callProgress{functionExecutions: executionsAt(entry, run)}
			if run >= entry {
				p.filterPassedRows = 1
			}
			return p
		},
		length: entry + modelCall,
	}
	if n < q.stalled {
		m.stallFrom = entry + pollInterval
		m.stallUntil = m.stallFrom + q.stall
	}
	seen, cancelled := m.observe(q.t, foldLoopRunning, deadline)
	switch {
	case cancelled < entry:
		q.final = executionsAt(entry, cancelled)
	case q.interruptible:
		q.final = executionsAt(entry, cancelled+modelInterruptLag)
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

// verdictCases are the four combinations of a call that is or is not
// interruptible with a build table that does or does not say it is: the
// verdict passes exactly where the two agree.
var verdictCases = []struct {
	interruptible, bounded, pass bool
}{
	{interruptible: true, bounded: true, pass: true},
	{interruptible: false, bounded: true, pass: false},
	{interruptible: false, bounded: false, pass: true},
	{interruptible: true, bounded: false, pass: false},
}

// assertVerdicts runs probeModel over every verdict case with the query
// entering the call after entries, and returns the landings of the first.
// Where the deadlines land is read before each cancellation, so it cannot
// depend on what the call does after one: every case must dispatch the same
// deadlines.
func assertVerdicts(t *testing.T, entries []time.Duration) []deadlineLanding {
	t.Helper()
	var (
		deadlines []time.Duration
		landings  []deadlineLanding
	)
	for _, tc := range verdictCases {
		q := &modelQuery{t: t, entries: entries, interruptible: tc.interruptible}
		j := probeModel(t, q, tc.bounded)
		if j.ok != tc.pass {
			t.Errorf("interruptible=%v bounded=%v: verdict ok=%v (left %d, after %d, bound %.0f); want %v",
				tc.interruptible, tc.bounded, j.ok, j.left, j.after, j.bound, tc.pass)
		}
		if deadlines == nil {
			deadlines, landings = q.deadlines, q.landings
		} else if fmt.Sprint(q.deadlines) != fmt.Sprint(deadlines) {
			t.Errorf("interruptible=%v bounded=%v dispatched deadlines %v; another case dispatched %v",
				tc.interruptible, tc.bounded, q.deadlines, deadlines)
		}
	}
	return landings
}

// A query whose entry into the call alternates, dispatch after dispatch,
// between two times further apart than the call's whole judgeable window —
// so that no single deadline fixed at send time could land inside the window
// of both — still lands, and the verdict still separates the builds: an
// interruptible call passes only where the build table says it is
// interrupted, and a call that cannot be interrupted fails there.
func TestLandRequestDeadline_AlternatingEntryLands(t *testing.T) {
	want := modelWant(true)
	early := pollInterval
	late := early + want.judgeableCall() + pollInterval
	for _, entries := range [][]time.Duration{
		{late, early, late, early, late},
		{early, late, early, late, early},
		// The pattern of the CI run that re-armed five deadlines without
		// landing one: fast, slow, slow, fast, slow.
		{early, late, late, early, late},
	} {
		t.Run(fmt.Sprint(entries), func(t *testing.T) {
			if landings := assertVerdicts(t, entries); len(landings) != 1 {
				t.Errorf("landings %v; want the first dispatch to land", landings)
			}
		})
	}
}

// A query that reaches the call later than the first deadline is judged on
// a re-armed deadline, and the verdict still separates the builds.
func TestLandRequestDeadline_DelayedEntryKeepsTheVerdict(t *testing.T) {
	want := modelWant(true)
	maxDelay := 2 * want.requestDeadline()
	lateDelays := 0
	for delay := time.Duration(0); delay <= maxDelay; delay += pollInterval {
		t.Run(fmt.Sprintf("delay_%s", delay), func(t *testing.T) {
			if landings := assertVerdicts(t, []time.Duration{modelEntered + delay}); landings[0] == landedBeforeCall {
				lateDelays++
			}
		})
	}
	// The sweep must reach a re-armed deadline: entries the first deadline
	// passes ahead of, which a single dispatch could not judge.
	if lateDelays == 0 {
		t.Fatalf("no delay up to %s made the first deadline %s pass before the call", maxDelay, want.requestDeadline())
	}
}

// A dispatch first seen inside the call only past the floor, because its
// readings stalled, is not judged, and the next dispatch keeps its deadline:
// the stall says nothing of where the deadline fell.
func TestLandRequestDeadline_StalledReadingsKeepTheDeadline(t *testing.T) {
	want := modelWant(true)
	for _, interruptible := range []bool{true, false} {
		q := &modelQuery{
			t: t, entries: []time.Duration{modelEntered}, interruptible: interruptible,
			stall: modelCall / minLoopLeftFraction, stalled: 1,
		}
		j := probeModel(t, q, true)
		wantLandings := []deadlineLanding{landedPastFloor, landedInWindow}
		if fmt.Sprint(q.landings) != fmt.Sprint(wantLandings) {
			t.Fatalf("interruptible=%v: landings %v; want %v", interruptible, q.landings, wantLandings)
		}
		if q.deadlines[1] != want.requestDeadline() {
			t.Errorf("interruptible=%v: deadlines %v; want the first one kept", interruptible, q.deadlines)
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
	never := requestDeadlineAttempts * want.requestDeadline()
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

// regexReading is the reading of a regex_replace query after run of server
// progress: its read progresses and executes one function per pollInterval,
// and then the call holds the count still.
func regexReading(run time.Duration) callProgress {
	var p callProgress
	if run > 0 {
		read := min(run, modelRegexRead)
		p.totalRows = modelRegexRows
		p.readRows = uint64(float64(modelRegexRows) * float64(read) / float64(modelRegexRead))
		p.functionExecutions = uint64(read / pollInterval)
	}
	return p
}

// Under wallClock a query that starts late on the server is re-armed past as
// well, and wherever it enters, the cancellation still leaves the
// minRemaining of server time the verdict judges.
func TestLandRequestDeadline_WallClockDelayedEntry(t *testing.T) {
	natural := &modelDispatch{at: regexReading, length: modelRegexNatural}
	seen, _ := natural.observe(t, func([]callProgress) bool { return false }, modelRegexNatural-pollInterval)
	entered, ok := callEntry(regexCallRunning, seen)
	if !ok {
		t.Fatal("the undelayed modelled query never entered its call")
	}
	want := cancelExpectation{
		bounded: true, meter: wallClock,
		naturalProfile: naturalProfile{natural: modelRegexNatural, entered: entered},
	}
	lateDelays := 0
	for delay := time.Duration(0); delay <= want.requestDeadline(); delay += pollInterval {
		var (
			landings  []deadlineLanding
			cancelled time.Duration
		)
		dispatch := func(deadline time.Duration) []callProgress {
			m := &modelDispatch{
				at:      func(run time.Duration) callProgress { return regexReading(run - delay) },
				started: delay,
				length:  delay + modelRegexNatural,
			}
			seen, run := m.observe(t, regexCallRunning, deadline)
			cancelled = run
			landings = append(landings, want.landing(regexCallRunning, seen))
			return seen
		}
		if _, _, err := want.landRequestDeadline(regexCallRunning, dispatch, func() {}); err != nil {
			t.Fatalf("delay %s: %v", delay, err)
		}
		if landings[0] == landedBeforeCall {
			lateDelays++
		}
		if remaining := delay + modelRegexNatural - cancelled; remaining < minRemaining {
			t.Errorf("delay %s: the cancellation leaves %s of the run; want at least %s", delay, remaining, minRemaining)
		}
	}
	if lateDelays == 0 {
		t.Fatalf("no delay up to %s made the first deadline pass before the call", want.requestDeadline())
	}
}
