package chserver

// The request-deadline probe's judging logic, which needs no server: the
// readings it takes of a query, the shapes' inCall predicates, where a
// deadline is placed and re-armed, and the separation verdict. The
// integration-tagged probes in cancellation_integration_test.go drive it
// against real servers; cancellation_deadline_test.go drives it against a
// modelled query.

import (
	"errors"
	"fmt"
	"slices"
	"strings"
	"time"
)

// pollInterval is how often a probe reads a running query's system.processes
// row.
const pollInterval = 50 * time.Millisecond

// callProgress is the part of one system.processes row the inCall
// predicates read.
type callProgress struct {
	elapsed             time.Duration
	readRows, totalRows uint64
	// filterPassedRows and functionExecutions are the query's
	// FilterTransformPassedRows and FunctionExecute profile events: the rows
	// its FilterTransforms have passed, and the function executions it has
	// started — once per block for an ordinary function, once per element
	// for a lambda a higher-order function applies element by element.
	filterPassedRows, functionExecutions uint64
}

// workMeter is the unit in which a probe measures how much of a shape's work
// a cancellation left and how much of it the server performed afterwards.
type workMeter int

const (
	// wallClock measures work as server time: what was left is the natural
	// run's duration less the time the query had run, and what was performed
	// is how long after the cancellation the query_log shows the work ending.
	// It suits a call whose own teardown is negligible next to its run.
	wallClock workMeter = iota
	// loopExecutions measures work as the FunctionExecute profile events the
	// call's per-element loop performs. An interrupted arrayFold still frees
	// the per-element slices its loop had not consumed, and that teardown
	// costs time in proportion to the work left, so server time cannot tell
	// it from the loop running on; the loop's own executions can, because
	// teardown executes no function. What was left is the natural run's loop
	// executions less those the query had performed at the cancellation, and
	// what was performed is the query's final count less its count then.
	loopExecutions
)

// foldLoopRunning is array_fold's inCall. The fold is evaluated by the
// ExpressionTransform that follows the emitted `WHERE length(window_vals) >=
// 2`, whose FilterTransform passes the aggregated series row last. By then
// the query has executed its functions once per block, a few hundred times.
// arrayFold first splits its input into per-element arguments, which no
// pinned build interrupts, and then applies its lambda once per element: the
// loop ClickHouse#108192 made check for cancellation. The fold's own
// expression executes only a handful of functions outside that loop, so the
// count passing foldLoopGrowth times its value at the filter's pass is the
// loop running.
func foldLoopRunning(readings []callProgress) bool {
	base, ok := foldLoopBaseline(readings)
	return ok && readings[len(readings)-1].functionExecutions > foldLoopGrowth*base
}

// foldLoopBaseline is array_fold's function-execution count at the filter's
// pass, the first reading at which the fold's expression has its input: the
// count the query had performed before the fold's loop began. ok is false
// while no reading has seen the filter pass.
func foldLoopBaseline(readings []callProgress) (base uint64, ok bool) {
	i := slices.IndexFunc(readings, func(p callProgress) bool { return p.filterPassedRows > 0 })
	if i < 0 {
		return 0, false
	}
	return readings[i].functionExecutions, true
}

// foldLoopGrowth is the factor by which array_fold's function executions
// must grow past their count at the filter's pass; see foldLoopRunning.
const foldLoopGrowth = 2

// cancelExpectation is what one shape must show on one build: whether the
// build interrupts its CPU-bound call, the meter its work is judged in, and
// the shape's natural run (see naturalProfile).
type cancelExpectation struct {
	bounded bool
	meter   workMeter
	naturalProfile
}

// requestDeadline is the request timeout the deadline probe sends first: the
// natural run's whole duration. The deadline is fixed before the query starts,
// and how long the query then takes to reach the call varies from dispatch to
// dispatch with the load on the runner, by more than the call's judgeable
// window. observeToDeadline therefore holds the query where it is first seen
// inside the call until just before the deadline expires on the server, so no
// deadline after the entry is too late, and the deadline only has to outlast
// the entry: a query
// whose work ahead of the call runs at the natural run's pace enters the call
// long before the natural run ended.
func (w cancelExpectation) requestDeadline() time.Duration {
	return w.natural
}

// judgeableCall is how much of the natural run's call lies ahead of the least
// work the probe judges — minRemaining of server time under wallClock,
// 1/minLoopLeftFraction of the loop under loopExecutions — measured in server
// time from the entry into the call. A calibration that leaves none cannot be
// judged wherever a deadline lands.
func (w cancelExpectation) judgeableCall() time.Duration {
	call := w.natural - w.entered
	if w.meter == loopExecutions {
		return call - call/minLoopLeftFraction
	}
	return call - minRemaining
}

// loopLeft is how many of the natural run's loop executions a query whose
// readings up to its cancellation are seen still had to perform. ok is false
// when no reading saw the fold's input arrive.
func (w cancelExpectation) loopLeft(seen []callProgress) (left uint64, ok bool) {
	base, ok := foldLoopBaseline(seen)
	if !ok {
		return 0, false
	}
	done := seen[len(seen)-1].functionExecutions - base
	return w.loopExecutions - min(done, w.loopExecutions), true
}

// Separation, relative to the work left at the cancellation (left) in the
// shape's meter. An interrupted call may perform at most
// left/interruptedFraction of it after the cancellation; an uninterrupted one
// runs the call out and must perform at least left/uninterruptedFraction.
// Both scale with the calibrated workload, so neither is tied to a runner's
// speed, and the band between a quarter and a half of the remainder separates
// them. Under wallClock the work performed is the time the query ran on,
// which on an interrupted build is its own teardown and scheduling noise:
// minRemaining is the least remainder judged, so a quarter of it stands clear
// of both. Under loopExecutions the work performed by an interrupted call is
// the executions between the last reading and the cancellation taking effect
// — measured locally on 26.7.13.12 and 26.8.10.6 at no more than two percent of
// what was left — and minLoopLeftFraction keeps the remainder at least half
// the loop, so that even a quarter of it is many times that, and an
// uninterrupted call's run out is at least a quarter of the whole loop.
const (
	interruptedFraction   = 4
	uninterruptedFraction = 2
	minRemaining          = 6 * time.Second
	minLoopLeftFraction   = 2
)

// separated reports whether after, the work the server performed past the
// cancellation, lies on the side of left, the work the cancellation left,
// that the build must show, and the bound it was held to.
func separated(bounded bool, after, left float64) (ok bool, bound float64) {
	if bounded {
		bound = left / interruptedFraction
		return after <= bound, bound
	}
	bound = left / uninterruptedFraction
	return after >= bound, bound
}

// naturalProfile is a shape's uncancelled run, the yardstick
// assertServerWorkEnds measures a cancellation against: how long the server
// took, how far into that run shape.inCall first held, and how many function
// executions the fold's loop performed in it (zero for a shape whose inCall
// is not foldLoopRunning). A run inCall was never seen to hold in reports
// entered as natural: none of its work can be counted as the call's.
type naturalProfile struct {
	natural, entered time.Duration
	loopExecutions   uint64
}

// inputRead reports that the query's sources have read every row they will
// read.
func (p callProgress) inputRead() bool { return p.totalRows > 0 && p.readRows >= p.totalRows }

// regexCallRunning is regex_replace's inCall. The normalization runs
// replaceRegexpAll once over the block the read produced, so for the whole
// call the query's function-execution count stands still. The actions ahead
// of it in the same expression each run briefly over bytes the read already
// produced; on 26.7.13.12 a cancellation landing after the read's progress
// report but before the call was observed to end the query at once. None of
// those actions takes as long as producing its input took, so the count
// standing still, with the input read, for longer than the query needed to
// read that input is the call running.
func regexCallRunning(readings []callProgress) bool {
	i := slices.IndexFunc(readings, callProgress.inputRead)
	if i < 0 {
		return false
	}
	last := len(readings) - 1
	since := last
	for since > i && readings[since-1].functionExecutions == readings[last].functionExecutions {
		since--
	}
	return readings[last].elapsed-readings[since].elapsed > readings[i].elapsed
}

// deadlineLanding is where a request deadline's cancellation fell, judged
// only from the readings taken before it: whether those readings can stand
// for a cancellation inside the call. Nothing the server did after the
// cancellation enters it, so re-arming on it cannot select for a verdict.
type deadlineLanding int

const (
	// landedBeforeCall: the deadline passed before the query reached the
	// part of the call the build table describes.
	landedBeforeCall deadlineLanding = iota
	// landedInWindow: the deadline passed inside the call, with at least the
	// least work the probe judges still ahead.
	landedInWindow
	// landedPastFloor: the deadline passed inside the call but too late in
	// the loop to leave the 1/minLoopLeftFraction loopExecutions judges.
	landedPastFloor
)

// landing classifies a request deadline's cancellation by seen, the readings
// of the query that completed before the deadline passed. Under wallClock
// the remainder is server time read from query_log, so assertServerWorkEnds
// judges that floor; under loopExecutions it is read from seen itself.
func (w cancelExpectation) landing(inCall func([]callProgress) bool, seen []callProgress) deadlineLanding {
	if !inCall(seen) {
		return landedBeforeCall
	}
	if w.meter == loopExecutions {
		if left, _ := w.loopLeft(seen); left*minLoopLeftFraction < w.loopExecutions {
			return landedPastFloor
		}
	}
	return landedInWindow
}

// callEntry is the server time at which seen first shows the query inside the
// call: the elapsed time of the earliest reading for whose prefix inCall
// holds. ok is false when inCall never holds.
func callEntry(inCall func([]callProgress) bool, seen []callProgress) (entry time.Duration, ok bool) {
	for i := range seen {
		if inCall(seen[:i+1]) {
			return seen[i].elapsed, true
		}
	}
	return 0, false
}

// deadlineDispatch is one request-deadline dispatch in flight, as
// observeToDeadline drives it.
type deadlineDispatch interface {
	// sinceSent is the wall-clock time since the request was sent.
	sinceSent() time.Duration
	// await waits up to pollInterval for the request's answer and reports
	// whether the request is still unanswered.
	await() bool
	// read reads the query's system.processes row; running is false while
	// the query is not executing.
	read() (p callProgress, running bool)
	// hold suspends the server until sinceSent reaches until: no query on it
	// makes progress meanwhile, while the wall clock runs on. It returns the
	// sinceSent by which the server was certainly running again.
	hold(until time.Duration) (resumed time.Duration)
}

// resumeLead is how long before the request deadline can expire on the
// server observeToDeadline ends its hold. The deadline must expire on a
// server that runs normally: one thawed with its max_execution_time already
// past wakes every thread at once, and the timeout can reach the client
// before the worker has stopped and left system.processes. The lead covers an
// unpause taking effect on a loaded runner, and the call's work in it is
// counted as performed after the cancellation, against the build.
const resumeLead = 4 * pollInterval

// deadlineObservation is what observeToDeadline saw of one dispatch.
type deadlineObservation struct {
	// seen holds the readings that completed before the deadline.
	seen []callProgress
	// resumedLate reports a hold that ended no earlier than the deadline
	// could have expired on the server, so the dispatch cannot be judged.
	resumedLate bool
}

// observeToDeadline reads a dispatched query every pollInterval until its
// deadline passes or its request is answered. At the first reading that shows
// the query inside the call it holds the server until resumeLead before the
// deadline can expire on the server, and reads no more: the cancellation the
// deadline makes then lands resumeLead past where that reading left the
// query, however long the query took to get there. The server measures the
// deadline (max_execution_time) from the query's start on the server, which
// the reading's own elapsed time places: no later than the reading was asked
// for, less that elapsed time. A query that has not entered the call by the
// deadline is cancelled ahead of it, which landing reports.
func observeToDeadline(inCall func([]callProgress) bool, deadline time.Duration, d deadlineDispatch) deadlineObservation {
	var obs deadlineObservation
	for d.await() {
		asked := d.sinceSent()
		p, running := d.read()
		if d.sinceSent() >= deadline {
			break
		}
		if !running {
			continue
		}
		if obs.seen = append(obs.seen, p); inCall(obs.seen) {
			// The earliest the deadline can expire on the server.
			expires := asked - p.elapsed + deadline
			if until := expires - resumeLead; until > d.sinceSent() {
				obs.resumedLate = d.hold(until) >= expires
			}
			break
		}
	}
	return obs
}

// requestDeadlineAttempts bounds how many request-deadline dispatches
// landRequestDeadline makes to land one cancellation in the call's judgeable
// window.
const requestDeadlineAttempts = 5

// landRequestDeadline finds a request deadline whose cancellation lands in
// the judgeable window of the call, and returns it with the readings its
// dispatch took before it passed. dispatch sends the request with a deadline
// and returns what observeToDeadline saw; discard waits out a dispatch that
// cannot be judged. Because the dispatch holds the query at its observed
// entry, a deadline misses only by passing before the entry, never by passing
// too late in the call: the first deadline is requestDeadline, and a deadline
// that passed before the call proves the entry came later, so the next one is
// a further natural run later. A dispatch first seen inside the call already
// past the floor was read too late to hold in time, and one whose hold ended
// too late was cancelled on a thawing server; neither says anything of its
// deadline, so the next dispatch keeps it. Landing depends only on the
// readings before each cancellation (see landing), so the verdict on the
// returned dispatch is the same one a single well-placed deadline would face.
// It returns an error when no dispatch lands within requestDeadlineAttempts,
// or the calibration left the call no window.
func (w cancelExpectation) landRequestDeadline(inCall func([]callProgress) bool,
	dispatch func(deadline time.Duration) deadlineObservation, discard func(),
) (time.Duration, []callProgress, error) {
	if w.judgeableCall() <= 0 {
		return 0, nil, fmt.Errorf("the natural run (%s, entering the call after %s) leaves the call no window to place "+
			"a request deadline in", w.natural, w.entered)
	}
	deadline := w.requestDeadline()
	var missed []string
	for range requestDeadlineAttempts {
		obs := dispatch(deadline)
		switch landing := w.landing(inCall, obs.seen); {
		case obs.resumedLate:
			missed = append(missed, fmt.Sprintf("%s: the hold ended too late to let the deadline expire on a running "+
				"server", deadline))
		case landing == landedInWindow:
			return deadline, obs.seen, nil
		case landing == landedBeforeCall:
			missed = append(missed, fmt.Sprintf("%s passed before the call", deadline))
			deadline += w.natural
		case landing == landedPastFloor:
			entry, _ := callEntry(inCall, obs.seen)
			missed = append(missed, fmt.Sprintf("%s: the query was first seen inside the call after %s, already past the "+
				"floor", deadline, entry))
		}
		discard()
	}
	return 0, nil, fmt.Errorf("none of %d request deadlines landed inside the call with the least work the probe "+
		"judges still ahead: %s", requestDeadlineAttempts, strings.Join(missed, "; "))
}

// loopJudgement is assertLoopSeparation's reading of one statement under
// loopExecutions: left is how many of the natural run's loop executions it
// still had to perform at its last reading before the cancellation, after is
// how many it performed from that reading to its end, and ok reports whether
// after lies on the build's side of bound (see separated).
type loopJudgement struct {
	left, after uint64
	bound       float64
	ok          bool
}

// errLoopUnjudgeable marks a statement whose readings cannot be judged under
// loopExecutions at all.
var errLoopUnjudgeable = errors.New("the probe cannot judge the statement")

// judgeLoop judges a statement under loopExecutions from seen, its readings
// up to the cancellation, and final, the FunctionExecute count its query_log
// row reports. It returns an error wrapping errLoopUnjudgeable when no
// reading saw the fold's input arrive, when final is below the last reading,
// or when less than 1/minLoopLeftFraction of the loop was left.
func (w cancelExpectation) judgeLoop(seen []callProgress, final uint64) (loopJudgement, error) {
	left, ok := w.loopLeft(seen)
	if !ok {
		return loopJudgement{}, fmt.Errorf("%w: no reading before the cancellation saw the fold's input arrive", errLoopUnjudgeable)
	}
	atCancel := seen[len(seen)-1].functionExecutions
	if final < atCancel {
		return loopJudgement{}, fmt.Errorf("%w: query_log reports %d function executions, fewer than the %d "+
			"system.processes read before its end", errLoopUnjudgeable, final, atCancel)
	}
	j := loopJudgement{left: left, after: final - atCancel}
	if left*minLoopLeftFraction < w.loopExecutions {
		return j, fmt.Errorf("%w: only %d of the call's %d loop executions were left at the cancellation; the probe "+
			"needs at least 1/%d of the loop to separate an interrupted call from an uninterrupted one",
			errLoopUnjudgeable, left, w.loopExecutions, minLoopLeftFraction)
	}
	j.ok, j.bound = separated(w.bounded, float64(j.after), float64(left))
	return j, nil
}
