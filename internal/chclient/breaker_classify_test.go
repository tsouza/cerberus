package chclient

import (
	"context"
	"errors"
	"fmt"
	"log/slog"
	"strings"
	"testing"

	"github.com/ClickHouse/ch-go"
	chproto "github.com/ClickHouse/ch-go/proto"
	"github.com/ClickHouse/clickhouse-go/v2"
	"go.opentelemetry.io/otel/attribute"
	sdkmetric "go.opentelemetry.io/otel/sdk/metric"
	"go.opentelemetry.io/otel/sdk/metric/metricdata"
)

// Breaker scope-classification tests (#2900).
//
// The defect these pin: ~21 queries rejected with ClickHouse code 704
// (QUERY_CACHE_USED_WITH_NONDETERMINISTIC_FUNCTIONS) tripped the breaker,
// which then 503'd everything behind it — 144 LogQL and 871 PromQL compat
// divergences from ~21 real failures. The server was healthy throughout and
// answered every other query at the same instant.
//
// Both directions are pinned, because a guard that can only fail one way is
// half a guard:
//
//   - statement-scoped rejections must NOT trip, however many arrive;
//   - server-health failures must STILL trip.
//
// Each test names, in its own doc comment, the source mutation that turns it
// red — the evidence it is not vacuous.

// rowDialException builds the *clickhouse.Exception shape the clickhouse-go/v2
// row dial hands the breaker.
func rowDialException(code chproto.Error) error {
	return &clickhouse.Exception{
		Code:    int32(code),
		Name:    code.String(),
		Message: "synthetic " + code.String(),
	}
}

// columnarDialException builds the ch-go *Exception shape the columnar dial
// hands the breaker. It is a DIFFERENT Go type carrying the identical wire
// code, and before #2900 the breaker's code checks could not see through it at
// all — every columnar server rejection classified as an outage.
func columnarDialException(code chproto.Error) error {
	return &ch.Exception{
		Code:    code,
		Name:    code.String(),
		Message: "synthetic " + code.String(),
	}
}

// dialShapes returns both wire shapes of the same ClickHouse code, so every
// classification assertion runs against the row dial AND the columnar dial.
func dialShapes(code chproto.Error) map[string]error {
	return map[string]error{
		"row-dial":      rowDialException(code),
		"columnar-dial": columnarDialException(code),
		// The wrapped form the stack produces once an error has travelled a
		// stage or two; classification must survive wrapping.
		"wrapped-row-dial": fmt.Errorf("chclient: query: %w", rowDialException(code)),
	}
}

// chCodeUnallocatedFuture is a ClickHouse error code that does not exist in the
// ch-go generated table — the stand-in for "a code nobody has classified yet",
// which is the exact situation 704 was in before this change. Chosen far above
// the allocated range so a real ClickHouse release cannot quietly give it a
// meaning.
const chCodeUnallocatedFuture chproto.Error = 60000

// TestBreakerClassify_StatementScopedStreamDoesNotTrip is acceptance criterion
// 1: a SUSTAINED stream of per-statement rejections leaves the breaker CLOSED.
// It runs far past the trip threshold for each code, on both dial shapes.
//
// NON-VACUITY: adding any one of these codes to breakerServerHealthCodes (or
// flipping classifyBreakerOutcome's default from breakerScopeStatement to
// breakerScopeServerHealth) turns that subtest red — the stream then counts and
// the breaker reports "open".
func TestBreakerClassify_StatementScopedStreamDoesNotTrip(t *testing.T) {
	t.Parallel()

	codes := []chproto.Error{
		// The code that caused #2895's amplification. This is the regression.
		chproto.ErrQueryCacheUsedWithNondeterministicFunctions,
		// Its sibling, named in #2900 and never yet observed in an incident —
		// the point being that it needs no incident to be classified.
		chproto.ErrQueryCacheUsedWithSystemTable,
		// The malformed-statement classes: a bad query is not backend illness.
		chproto.ErrSyntaxError,
		chproto.ErrUnknownIdentifier,
		chproto.ErrTypeMismatch,
		chproto.ErrUnknownTable,
		chproto.ErrIllegalTypeOfArgument,
		// The three codes that used to have hand-written arms in record. Their
		// behaviour must be byte-identical after the unification.
		chproto.ErrMemoryLimitExceeded,
		chproto.ErrTimeoutExceeded,
		chproto.ErrFunctionThrowIfValueIsNonZero,
		// The unclassified future code: the default, exercised.
		chCodeUnallocatedFuture,
	}

	for _, code := range codes {
		t.Run(code.String(), func(t *testing.T) {
			t.Parallel()
			for shape, err := range dialShapes(code) {
				t.Run(shape, func(t *testing.T) {
					t.Parallel()
					b := &breaker{}
					// Ten times the threshold, with no intervening success —
					// there is no window-rollover escape hatch here.
					for i := 0; i < breakerThreshold*10; i++ {
						_ = b.allow()
						b.record(context.Background(), err)
					}
					if got := b.currentState(); got != "closed" {
						t.Fatalf("%s (%d) via %s: breaker went %q after %d rejections; a statement-scoped "+
							"rejection was counted as backend ill-health — this is the #2895 amplification",
							code.String(), int(code), shape, got, breakerThreshold*10)
					}
				})
			}
		})
	}
}

// TestBreakerClassify_ServerHealthStreamTrips is acceptance criterion 2: the
// breaker still fires. Every code in the curated server-health set, plus the
// no-server-answer shapes a real outage wears, must open the circuit within the
// threshold.
//
// The expectation is an INDEPENDENT list, not a walk of
// breakerServerHealthCodes. A test that iterates the very map it is checking
// cannot notice an entry going missing — deleting the entry merely deletes the
// subtest, and the suite stays green while the breaker quietly stops firing on
// that condition. The map walk survives below as a separate, weaker check (each
// entry must actually trip and must carry a reason), but the guard that can go
// red is this list.
//
// NON-VACUITY: deleting any entry named in mustTripCodes from
// breakerServerHealthCodes turns its subtest red — the code falls through to
// the statement-scoped default, stops being counted, and the breaker stays
// "closed".
func TestBreakerClassify_ServerHealthStreamTrips(t *testing.T) {
	t.Parallel()

	// mustTripCodes are ClickHouse codes that MUST count toward the breaker.
	// Each is a condition of the server or its cluster that outlives the
	// statement: retrying different, well-formed SQL against this backend
	// would fail the same way, which is exactly what shedding load is for.
	mustTripCodes := []chproto.Error{
		chproto.ErrServerOverloaded,
		chproto.ErrTooManySimultaneousQueries,
		chproto.ErrKeeperException,
		chproto.ErrAllConnectionTriesFailed,
		chproto.ErrNoAvailableReplica,
		chproto.ErrAllReplicasLost,
		chproto.ErrNetworkError,
		chproto.ErrSocketTimeout,
		chproto.ErrSystemError,
		chproto.ErrCannotAllocateMemory,
		chproto.ErrCannotScheduleTask,
		chproto.ErrNoFreeConnection,
		chproto.ErrDNSError,
		chproto.ErrAborted,
		chproto.ErrAuthenticationFailed,
		chproto.ErrTableIsBeingRestarted,
	}

	t.Run("must-trip-codes", func(t *testing.T) {
		t.Parallel()
		for _, code := range mustTripCodes {
			t.Run(code.String(), func(t *testing.T) {
				t.Parallel()
				for shape, err := range dialShapes(code) {
					t.Run(shape, func(t *testing.T) {
						t.Parallel()
						b := &breaker{}
						for i := 0; i < breakerThreshold; i++ {
							_ = b.allow()
							b.record(context.Background(), err)
						}
						if got := b.currentState(); got != "open" {
							t.Fatalf("%s (%d) via %s: breaker is %q after %d failures; a server-condition "+
								"code stopped counting and the breaker can no longer protect the backend",
								code.String(), int(code), shape, got, breakerThreshold)
						}
					})
				}
			})
		}
	})

	t.Run("curated-set-entries-are-reasoned-and-live", func(t *testing.T) {
		t.Parallel()
		if len(breakerServerHealthCodes) == 0 {
			t.Fatal("breakerServerHealthCodes is empty: the breaker can no longer fire on any answered code")
		}
		for code, reason := range breakerServerHealthCodes {
			t.Run(code.String(), func(t *testing.T) {
				t.Parallel()
				// Every entry earns its place with a sentence. An unreasoned
				// number is precisely the hand-list this change removed, and a
				// blank reason would let one back in.
				if strings.TrimSpace(reason) == "" {
					t.Fatalf("%s carries no reason; an unreasoned entry is the hand-list this change removed",
						code.String())
				}
				// And every entry is live: being in the set must actually
				// produce the server-health verdict, so a typo'd key cannot
				// sit in the map documenting behaviour it does not cause.
				if got := classifyBreakerOutcome(rowDialException(code)).Scope; got != breakerScopeServerHealth {
					t.Fatalf("%s is in breakerServerHealthCodes but classifies %v", code.String(), got)
				}
			})
		}
	})

	// The shapes a backend that never answered wears. None carries a decoded
	// exception, and every one of them must still count.
	t.Run("no-server-answer", func(t *testing.T) {
		t.Parallel()
		cases := map[string]error{
			"dial-refused":   errors.New("dial tcp 127.0.0.1:9000: connect: connection refused"),
			"eof":            errors.New("unexpected EOF"),
			"broken-pipe":    errors.New("write tcp 127.0.0.1:9000: write: broken pipe"),
			"read-timeout":   errors.New("read tcp 127.0.0.1:9000: i/o timeout"),
			"deadline":       context.DeadlineExceeded,
			"driver-unknown": errors.New("clickhouse: unexpected packet 42 from server"),
		}
		for name, err := range cases {
			t.Run(name, func(t *testing.T) {
				t.Parallel()
				b := &breaker{}
				for i := 0; i < breakerThreshold; i++ {
					_ = b.allow()
					b.record(context.Background(), err)
				}
				if got := b.currentState(); got != "open" {
					t.Fatalf("%s: breaker is %q after %d failures; the breaker no longer fires on a backend "+
						"that never answered", name, got, breakerThreshold)
				}
			})
		}
	})
}

// TestBreakerClassify_UnknownCodeIsStatementScoped pins the DEFAULT direction
// on its own, separately from the sustained-stream test, so a change of default
// cannot hide behind a table entry. An unallocated code is statement-scoped:
// the server answered it, therefore the server is serving.
//
// NON-VACUITY: flipping classifyBreakerOutcome's fall-through return from
// breakerScopeStatement to breakerScopeServerHealth turns this red.
func TestBreakerClassify_UnknownCodeIsStatementScoped(t *testing.T) {
	t.Parallel()
	if _, listed := breakerServerHealthCodes[chCodeUnallocatedFuture]; listed {
		t.Fatalf("code %d is in breakerServerHealthCodes; pick a genuinely unallocated code for this test",
			int(chCodeUnallocatedFuture))
	}
	got := classifyBreakerOutcome(rowDialException(chCodeUnallocatedFuture))
	if got.Scope != breakerScopeStatement {
		t.Fatalf("unknown code %d classified %v, want %v: an unenumerated code must default to "+
			"statement-scoped, or every future ClickHouse code re-arms the #2895 amplification",
			int(chCodeUnallocatedFuture), got.Scope, breakerScopeStatement)
	}
	if got.Code != chCodeUnallocatedFuture {
		t.Fatalf("classified code = %d, want %d", int(got.Code), int(chCodeUnallocatedFuture))
	}
}

// TestBreakerClassify_BothDialsAgree pins that the row dial's
// *clickhouse.Exception and the columnar dial's ch-go *Exception reach the
// SAME verdict for the same wire code. Before #2900 they did not: the breaker's
// code checks only understood *clickhouse.Exception, so every server rejection
// on the columnar dial — code 241 and 159 included — classified as an outage.
//
// NON-VACUITY: deleting the ch.AsException branch from serverExceptionCode
// turns this red on every columnar case (they all collapse to
// no-server-answer / server-health).
func TestBreakerClassify_BothDialsAgree(t *testing.T) {
	t.Parallel()
	codes := []chproto.Error{
		chproto.ErrQueryCacheUsedWithNondeterministicFunctions,
		chproto.ErrMemoryLimitExceeded,
		chproto.ErrTimeoutExceeded,
		chproto.ErrFunctionThrowIfValueIsNonZero,
		chproto.ErrServerOverloaded,
		chproto.ErrKeeperException,
		chCodeUnallocatedFuture,
	}
	for _, code := range codes {
		t.Run(code.String(), func(t *testing.T) {
			t.Parallel()
			row := classifyBreakerOutcome(rowDialException(code))
			columnar := classifyBreakerOutcome(columnarDialException(code))
			if row != columnar {
				t.Fatalf("dial disagreement for %s (%d): row=%+v columnar=%+v; the same server rejection "+
					"must reach the same verdict on both dials", code.String(), int(code), row, columnar)
			}
			if row.Code != code {
				t.Fatalf("%s: classified code = %d, want %d", code.String(), int(row.Code), int(code))
			}
		})
	}
}

// TestBreakerClassify_ClientScopedNeverCounts pins the third scope: failures
// that never became a question ClickHouse was asked. Both are pre-existing
// carve-outs the unification absorbed, kept here so absorbing them cannot
// quietly drop them.
func TestBreakerClassify_ClientScopedNeverCounts(t *testing.T) {
	t.Parallel()
	cases := map[string]error{
		"pool-acquire-timeout":         clickhouse.ErrAcquireConnTimeout,
		"wrapped-pool-acquire-timeout": fmt.Errorf("chclient: query: %w", clickhouse.ErrAcquireConnTimeout),
		"columnar-sample-budget":       errBudgetExceeded,
	}
	for name, err := range cases {
		t.Run(name, func(t *testing.T) {
			t.Parallel()
			if got := classifyBreakerOutcome(err).Scope; got != breakerScopeClient {
				t.Fatalf("%s classified %v, want %v", name, got, breakerScopeClient)
			}
			b := &breaker{}
			for i := 0; i < breakerThreshold*3; i++ {
				_ = b.allow()
				b.record(context.Background(), err)
			}
			if got := b.currentState(); got != "closed" {
				t.Fatalf("%s: breaker went %q; a client-scoped failure was counted as backend ill-health",
					name, got)
			}
		})
	}
}

// TestBreakerTripCause_VocabularyIsClosed pins that every cause
// [breakerOutcome.tripCause] can produce is present in allTripCauses — the
// slice the trip counter zero-initialises from. A cause missing from the slice
// exports no baseline stream and renders "No data" instead of a flat 0 until
// its first trip.
func TestBreakerTripCause_VocabularyIsClosed(t *testing.T) {
	t.Parallel()
	seen := map[string]bool{}
	outcomes := []breakerOutcome{
		{Scope: breakerScopeServerHealth, Code: chCodeNoServerAnswer},
		{Scope: breakerScopeServerHealth, Code: chproto.ErrServerOverloaded},
		{Scope: breakerScopeStatement, Code: chproto.ErrQueryCacheUsedWithNondeterministicFunctions},
		{Scope: breakerScopeClient, Code: chCodeNoServerAnswer},
	}
	for _, o := range outcomes {
		seen[o.tripCause()] = true
	}
	known := map[string]bool{}
	for _, c := range allTripCauses {
		known[c] = true
	}
	for c := range seen {
		if !known[c] {
			t.Fatalf("tripCause %q is not in allTripCauses; its trip stream will have no zero-init baseline", c)
		}
	}
	if len(known) != len(allTripCauses) {
		t.Fatalf("allTripCauses contains duplicates: %v", allTripCauses)
	}
}

// TestBreakerTrip_CarriesCauseContext is acceptance criterion 3: a trip event
// says enough to route the triage. It asserts on BOTH channels a responder
// reaches for — the trip counter's `cause` attribute and the WARN log line —
// and it asserts the two trip flavours are DISTINGUISHABLE, which is the whole
// requirement: during #2895 the only available signal (a collapsed compat
// score) pointed at the wrong subsystem for 32 hours.
//
// NON-VACUITY: dropping the `cause` attribute from recordTrip turns the
// counter half red (both trips land on one stream and the two flavours become
// indistinguishable); dropping the `cause` / ch_code_name fields from the trip
// log turns the log half red.
func TestBreakerTrip_CarriesCauseContext(t *testing.T) {
	// NOT parallel: captureBreakerLogs swaps the package-level breakerLogger,
	// and the trip-line assertion requires exclusive ownership of it.

	t.Run("server-condition-trip-names-the-code", func(t *testing.T) {
		logs := captureBreakerLogs(t)
		reader := sdkmetric.NewManualReader()
		b := &breaker{
			head:    HeadProm,
			metrics: newBreakerMetrics(sdkmetric.NewMeterProvider(sdkmetric.WithReader(reader))),
		}
		for i := 0; i < breakerThreshold; i++ {
			_ = b.allow()
			b.record(context.Background(), rowDialException(chproto.ErrServerOverloaded))
		}
		if got := b.currentState(); got != "open" {
			t.Fatalf("breaker is %q, want open", got)
		}
		if got := tripsByCause(t, reader)[tripCauseServerCondition]; got != 1 {
			t.Fatalf("trips{cause=%s} = %d, want 1; a triager cannot tell WHY the breaker opened",
				tripCauseServerCondition, got)
		}
		if got := tripsByCause(t, reader)[tripCauseNoServerAnswer]; got != 0 {
			t.Fatalf("trips{cause=%s} = %d, want 0; the two trip flavours are not distinguishable",
				tripCauseNoServerAnswer, got)
		}
		line := logs.tripLine(t)
		for _, want := range []string{tripCauseServerCondition, "SERVER_OVERLOADED"} {
			if !strings.Contains(line, want) {
				t.Fatalf("trip log does not mention %q: %s", want, line)
			}
		}
	})

	t.Run("no-answer-trip-reports-neutralised-statements", func(t *testing.T) {
		logs := captureBreakerLogs(t)
		reader := sdkmetric.NewManualReader()
		b := &breaker{
			head:    HeadLoki,
			metrics: newBreakerMetrics(sdkmetric.NewMeterProvider(sdkmetric.WithReader(reader))),
		}
		// The #2895 shape: statement rejections flowing alongside a genuine
		// outage. The rejections must not contribute to the trip, but the trip
		// must SAY they happened so the responder is not left guessing.
		const rejections = 3
		for i := 0; i < rejections; i++ {
			_ = b.allow()
			b.record(context.Background(),
				rowDialException(chproto.ErrQueryCacheUsedWithNondeterministicFunctions))
		}
		for i := 0; i < breakerThreshold; i++ {
			_ = b.allow()
			b.record(context.Background(), errors.New("dial tcp 127.0.0.1:9000: connection refused"))
		}
		if got := b.currentState(); got != "open" {
			t.Fatalf("breaker is %q, want open", got)
		}
		if got := tripsByCause(t, reader)[tripCauseNoServerAnswer]; got != 1 {
			t.Fatalf("trips{cause=%s} = %d, want 1", tripCauseNoServerAnswer, got)
		}
		line := logs.tripLine(t)
		if !strings.Contains(line, tripCauseNoServerAnswer) {
			t.Fatalf("trip log does not name the cause: %s", line)
		}
		if !strings.Contains(line, fmt.Sprintf("statement_rejections_since_last_trip=%d", rejections)) {
			t.Fatalf("trip log does not report the %d neutralised statement rejections: %s", rejections, line)
		}
		// A no-answer trip has no ClickHouse code; logging ch_code=0 would
		// send a responder looking up error 0.
		if strings.Contains(line, "ch_code=") {
			t.Fatalf("no-answer trip log invented a ClickHouse code: %s", line)
		}
	})
}

// TestBreakerMetrics_StatementRejectionsCounted pins the standing "we sent bad
// statements" signal. After this change a rejection storm produces NO trip at
// all, so without its own stream the #2895 defect would be invisible in the
// telemetry exactly as it was for 32 hours.
//
// NON-VACUITY: removing the recordStatementRejection call from record turns
// the first assertion red; counting rejections toward the breaker (the old
// behaviour) turns the second red.
func TestBreakerMetrics_StatementRejectionsCounted(t *testing.T) {
	t.Parallel()
	reader := sdkmetric.NewManualReader()
	b := &breaker{
		head:    HeadProm,
		metrics: newBreakerMetrics(sdkmetric.NewMeterProvider(sdkmetric.WithReader(reader))),
	}
	const rejections = 21 // the #2895 incident's own count
	for i := 0; i < rejections; i++ {
		_ = b.allow()
		b.record(context.Background(),
			rowDialException(chproto.ErrQueryCacheUsedWithNondeterministicFunctions))
	}
	if got := statementRejectionsTotal(t, reader); got != rejections {
		t.Fatalf("statement_rejections_total = %d, want %d; the bad-statement signal is missing",
			got, rejections)
	}
	if got := breakerTripsTotal(t, reader); got != 0 {
		t.Fatalf("trips_total = %d, want 0; statement rejections tripped the breaker", got)
	}
	if got := b.currentState(); got != "closed" {
		t.Fatalf("breaker is %q, want closed", got)
	}
}

// tripsByCause reads cerberus_ch_breaker_trips_total keyed by its `cause`
// attribute, summing across heads.
func tripsByCause(t *testing.T, reader *sdkmetric.ManualReader) map[string]int64 {
	t.Helper()
	return sumCounterBy(t, reader, "cerberus_ch_breaker_trips_total", "cause")
}

// statementRejectionsTotal reads the cumulative
// cerberus_ch_breaker_statement_rejections_total across every head.
func statementRejectionsTotal(t *testing.T, reader *sdkmetric.ManualReader) int64 {
	t.Helper()
	var sum int64
	for _, v := range sumCounterBy(t, reader, "cerberus_ch_breaker_statement_rejections_total", "head") {
		sum += v
	}
	return sum
}

// sumCounterBy collects a monotonic Int64 counter from a manual-reader snapshot
// and sums its data points grouped by the named attribute. It fails the test if
// the metric is absent (an unexported stream is a silent observability hole) or
// if a data point is missing the attribute.
func sumCounterBy(t *testing.T, reader *sdkmetric.ManualReader, name, attr string) map[string]int64 {
	t.Helper()
	var rm metricdata.ResourceMetrics
	if err := reader.Collect(context.Background(), &rm); err != nil {
		t.Fatalf("collect: %v", err)
	}
	out := map[string]int64{}
	var found bool
	for _, sm := range rm.ScopeMetrics {
		for _, m := range sm.Metrics {
			if m.Name != name {
				continue
			}
			found = true
			s, ok := m.Data.(metricdata.Sum[int64])
			if !ok {
				t.Fatalf("%s data: want Sum[int64], got %T", name, m.Data)
			}
			for _, dp := range s.DataPoints {
				v, ok := dp.Attributes.Value(attribute.Key(attr))
				if !ok {
					t.Fatalf("%s data point missing %q attribute: %v", name, attr, dp.Attributes.ToSlice())
				}
				out[v.AsString()] += dp.Value
			}
		}
	}
	if !found {
		t.Fatalf("%s not exported", name)
	}
	return out
}

// breakerLogCapture collects the WARN transition lines the breaker emits, so a
// test can assert on the trip line's fields.
type breakerLogCapture struct{ buf *strings.Builder }

// captureBreakerLogs swaps breakerLogger for a text handler writing into the
// returned capture, restoring the previous logger when the test ends. Tests
// using it must NOT be parallel: breakerLogger is a package var.
func captureBreakerLogs(t *testing.T) *breakerLogCapture {
	t.Helper()
	var buf strings.Builder
	logger := slog.New(slog.NewTextHandler(&buf, &slog.HandlerOptions{Level: slog.LevelWarn}))
	prev := breakerLogger
	breakerLogger = func() *slog.Logger { return logger }
	t.Cleanup(func() { breakerLogger = prev })
	return &breakerLogCapture{buf: &buf}
}

// tripLine returns the single CLOSED->OPEN trip line, failing the test if the
// breaker logged no trip or more than one.
func (c *breakerLogCapture) tripLine(t *testing.T) string {
	t.Helper()
	var hits []string
	for _, line := range strings.Split(c.buf.String(), "\n") {
		if strings.Contains(line, "tripped OPEN") {
			hits = append(hits, line)
		}
	}
	if len(hits) != 1 {
		t.Fatalf("want exactly 1 trip log line, got %d:\n%s", len(hits), c.buf.String())
	}
	return hits[0]
}
