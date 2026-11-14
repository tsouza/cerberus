package chclient

import (
	"context"
	"errors"
	"fmt"
	"maps"
	"regexp"
	"sync/atomic"

	"github.com/ClickHouse/clickhouse-go/v2"
	"go.opentelemetry.io/otel"
	"go.opentelemetry.io/otel/attribute"
	"go.opentelemetry.io/otel/metric"

	"github.com/tsouza/cerberus/internal/chopt"
)

// chCodeUnknownFunction is ClickHouse's UNKNOWN_FUNCTION: the server does not
// know a function the statement calls, and rejects the whole query.
const chCodeUnknownFunction = 46

// chCodeSettingConstraintViolation is ClickHouse's
// SETTING_CONSTRAINT_VIOLATION: the server refuses a value the query sets.
const chCodeSettingConstraintViolation = 452

// The two kinds of name a node can be too old for.
const (
	unknownNameSetting  = "setting"
	unknownNameFunction = "function"
)

// The patterns that pull the rejected name out of a ClickHouse exception.
// The code says what was rejected; only the message says which name, and its
// wording differs by protocol and release: the native protocol and releases
// before 26.6 answer "Unknown setting 'x'", HTTP on 26.6 and later "Setting x
// is neither a builtin setting ...", the analyzer "Function with name `f` does
// not exist" (quoted with ' before 25.3) and the legacy analyzer "Unknown
// function f". Every pattern anchors on the phrase that introduces the
// rejected name, never on a bare quoted name: the same message lists the
// server's spelling suggestions, quoted, after it.
var (
	unknownSettingPatterns = []*regexp.Regexp{
		regexp.MustCompile(`Unknown setting '([^']+)'`),
		regexp.MustCompile(`Setting ([A-Za-z0-9_]+) is neither a builtin setting`),
	}
	unknownFunctionPatterns = []*regexp.Regexp{
		regexp.MustCompile("Function with name [`']([^`']+)[`'] does not exist"),
		regexp.MustCompile(`Unknown function ([A-Za-z0-9_]+)`),
	}
	// analyzerMandatoryPattern reads the setting a node on a build whose
	// analyzer is mandatory (chopt.AnalyzerMandatorySince) refuses to set to
	// 0, off its SETTING_CONSTRAINT_VIOLATION. 26.9 answers it with the same
	// wording over both protocols: "Setting 'enable_analyzer' cannot be
	// disabled: the analyzer is mandatory since v26.9 and the old query
	// analysis is no longer supported. ..." (allow_experimental_analyzer, its
	// alias, is named the same way).
	analyzerMandatoryPattern = regexp.MustCompile(`Setting '([^']+)' cannot be disabled: the analyzer is mandatory`)
	// serverVersionPattern reads the build ClickHouse appends to an exception
	// it answers over HTTP, "(version 26.6.8.7 (official build))". The native
	// protocol carries no build in the exception.
	serverVersionPattern = regexp.MustCompile(`\(version ([0-9][0-9.]*[0-9])`)
)

// unknownName is a rejection parsed off a ClickHouse exception: the kind and
// name the node did not know, and the build it reported, when it reported one.
type unknownName struct {
	kind        string
	name        string
	nodeVersion string
}

// parseUnknownName reports the setting or function a ClickHouse exception
// rejected the query over. ok is false for every other error.
func parseUnknownName(err error) (unknownName, bool) {
	var ex *clickhouse.Exception
	if err == nil || !errors.As(err, &ex) {
		return unknownName{}, false
	}
	var kind string
	var patterns []*regexp.Regexp
	switch ex.Code {
	case chCodeUnknownSetting:
		kind, patterns = unknownNameSetting, unknownSettingPatterns
	case chCodeUnknownFunction:
		kind, patterns = unknownNameFunction, unknownFunctionPatterns
	default:
		return unknownName{}, false
	}
	for _, p := range patterns {
		if m := p.FindStringSubmatch(ex.Message); m != nil {
			u := unknownName{kind: kind, name: m[1]}
			if v := serverVersionPattern.FindStringSubmatch(ex.Message); v != nil {
				u.nodeVersion = v[1]
			}
			return u, true
		}
	}
	return unknownName{}, false
}

// parseAnalyzerMandatory reports the setting a ClickHouse exception refused
// because the node's analyzer is mandatory, with the node's build when the
// exception carries one. ok is false for every other error.
func parseAnalyzerMandatory(err error) (unknownName, bool) {
	var ex *clickhouse.Exception
	if err == nil || !errors.As(err, &ex) || ex.Code != chCodeSettingConstraintViolation {
		return unknownName{}, false
	}
	m := analyzerMandatoryPattern.FindStringSubmatch(ex.Message)
	if m == nil {
		return unknownName{}, false
	}
	u := unknownName{kind: unknownNameSetting, name: m[1]}
	if v := serverVersionPattern.FindStringSubmatch(ex.Message); v != nil {
		u.nodeVersion = v[1]
	}
	return u, true
}

// ErrNodeTooOld is the sentinel every rejection of a setting or function a
// ClickHouse node does not know unwraps to: *UnknownNameError and
// *GateSettingUnsupportedError.
var ErrNodeTooOld = errors.New("chclient: a ClickHouse node does not know a setting or function cerberus sent")

// UnknownNameError is a query a ClickHouse node rejected because it does not
// know a setting (UNKNOWN_SETTING, code 115) or function (UNKNOWN_FUNCTION,
// code 46) cerberus sent. cerberus sends only names the build its optimization
// set was resolved against knows, so the node runs an older build than that:
// a rolling rollback, or an older node joining the fleet. The rejection
// requests an early capability re-probe (Client.SetUnknownNameHook), after
// which queries stop carrying what the fleet's oldest node lacks.
type UnknownNameError struct {
	// Kind is "setting" or "function".
	Kind string
	Name string
	// NodeVersion is the build the rejecting node reported, or "" when the
	// protocol carried none (the native protocol never does).
	NodeVersion string
	Cause       error
}

func (e *UnknownNameError) Error() string {
	node := "A ClickHouse node"
	if e.NodeVersion != "" {
		node = "ClickHouse node version " + e.NodeVersion
	}
	return fmt.Sprintf(
		"%s does not know %s %s, which the optimization set in force sends because it was resolved against a newer build; "+
			"cerberus re-probes the fleet now and stops sending it once the probe sees that node",
		node, e.Kind, e.Name,
	)
}

// Unwrap exposes the sentinel and the underlying ClickHouse exception.
func (e *UnknownNameError) Unwrap() []error {
	if e.Cause == nil {
		return []error{ErrNodeTooOld}
	}
	return []error{ErrNodeTooOld, e.Cause}
}

// wrapUnknownName converts an UNKNOWN_SETTING or UNKNOWN_FUNCTION exception
// into an *UnknownNameError. A gate-setting rejection, already a
// *GateSettingUnsupportedError, and every other error pass through untouched.
func wrapUnknownName(err error) error {
	if errors.Is(err, ErrGateSettingUnsupported) {
		return err
	}
	u, ok := parseUnknownName(err)
	if !ok {
		return err
	}
	return &UnknownNameError{Kind: u.kind, Name: u.name, NodeVersion: u.nodeVersion, Cause: err}
}

// UnknownNameRejection describes one query a ClickHouse node rejected because
// it does not know a setting or function cerberus sent, or — AnalyzerMandatory
// — because it refuses the enable_analyzer=0 cerberus sent.
type UnknownNameRejection struct {
	// Kind is "setting" or "function".
	Kind string
	Name string
	// NodeVersion is the build the rejecting node reported, or "".
	NodeVersion string
	// Retried reports that the name was a chopt.DroppableSettings entry the
	// query carried, so the query was sent again without the droppable
	// settings released at or after it.
	Retried bool
	// AnalyzerMandatory reports that the node knows the setting Name but
	// refused to disable the analyzer with it: the node runs a build whose
	// analyzer is mandatory (chopt.AnalyzerMandatory), which the set in force
	// does not account for. The query is not retried.
	AnalyzerMandatory bool
}

// Actions an UnknownNameRejection ends in, the action attribute of
// cerberus_ch_unknown_name_rejections_total.
const (
	rejectionActionRetried  = "retried"
	rejectionActionSurfaced = "surfaced"
)

// Attribute keys of cerberus_ch_unknown_name_rejections_total.
const (
	attrRejectionKind   = attribute.Key("kind")
	attrRejectionName   = attribute.Key("name")
	attrRejectionAction = attribute.Key("action")
)

// rejectionReporter fans an UnknownNameRejection out to the counter and the
// hook cmd/cerberus installs. One reporter is shared by every ForHead view of
// a Client, so the hook installed on the root reaches every head.
type rejectionReporter struct {
	hook atomic.Pointer[func(UnknownNameRejection)]
	// rejections is nil on the no-telemetry path.
	rejections metric.Int64Counter
}

// newRejectionReporter builds the reporter's counter off mp, or none when mp
// is nil. The droppable settings' retried series are seeded at zero: they are
// the series a rolling rollback moves, and a rate() over a series that has
// never been written reads "No data" instead of 0.
func newRejectionReporter(mp metric.MeterProvider) *rejectionReporter {
	r := &rejectionReporter{}
	if mp == nil {
		return r
	}
	counter, err := mp.Meter(breakerMeterName).Int64Counter(
		"cerberus_ch_unknown_name_rejections_total",
		metric.WithDescription(
			"Queries a ClickHouse node rejected because it does not know a setting or function cerberus sent: "+
				"the node runs an older build than the optimization set in force was resolved against. "+
				"action=retried: a droppable setting, the query was sent again without it and the droppable settings released after it. "+
				"action=surfaced: the rejection was returned to the client. Each one requests an early capability re-probe.",
		),
		metric.WithUnit("{query}"),
	)
	if err != nil {
		panic("chclient: build unknown-name rejection counter: " + err.Error())
	}
	r.rejections = counter
	for _, s := range chopt.DroppableSettings() {
		counter.Add(context.Background(), 0, metric.WithAttributes(
			attrRejectionKind.String(unknownNameSetting),
			attrRejectionName.String(s.Name),
			attrRejectionAction.String(rejectionActionRetried),
		))
	}
	return r
}

// newGlobalRejectionReporter is the production reporter, on the global
// MeterProvider.
func newGlobalRejectionReporter() *rejectionReporter {
	return newRejectionReporter(otel.GetMeterProvider())
}

// report counts rej and hands it to the hook.
func (r *rejectionReporter) report(rej UnknownNameRejection) {
	if r == nil {
		return
	}
	r.count(rej)
	r.notify(rej)
}

// count adds rej to cerberus_ch_unknown_name_rejections_total.
func (r *rejectionReporter) count(rej UnknownNameRejection) {
	if r.rejections != nil {
		action := rejectionActionSurfaced
		if rej.Retried {
			action = rejectionActionRetried
		}
		r.rejections.Add(context.Background(), 1, metric.WithAttributes(
			attrRejectionKind.String(rej.Kind),
			attrRejectionName.String(rej.Name),
			attrRejectionAction.String(action),
		))
	}
}

// notify hands rej to the hook.
func (r *rejectionReporter) notify(rej UnknownNameRejection) {
	if r == nil {
		return
	}
	if fn := r.hook.Load(); fn != nil {
		(*fn)(rej)
	}
}

// SetUnknownNameHook installs fn, called once for every query a ClickHouse
// node rejects because it does not know a setting or function the query
// carried, whether or not the query was retried, and once for every query a
// node rejects because its analyzer is mandatory and the query set
// enable_analyzer=0 (UnknownNameRejection.AnalyzerMandatory). cmd/cerberus
// installs the capability re-probe's early trigger here. fn runs on the
// query's goroutine and must not block. The hook is shared by every ForHead
// view of the client.
func (c *Client) SetUnknownNameHook(fn func(UnknownNameRejection)) {
	if c.rejections == nil {
		return
	}
	c.rejections.hook.Store(&fn)
}

// openWithDroppableRetry runs open, and when a node rejects the query because
// it does not know a chopt.DroppableSettings entry the query carries, runs it
// again without the droppable settings that node cannot know: those first
// released at or after the rejected one. The droppable settings it does know
// keep riding. Each further attempt carries strictly fewer droppable
// settings, so the attempts end once the node accepts what is left, rejects
// something else, or rejects a setting the attempt no longer carries. Every
// rejection of an unknown name is reported (rejectionReporter.report). A
// rejection of enable_analyzer=0 by a node whose analyzer is mandatory is
// handed to the hook alone (rejectionReporter.notify): it is not a name the
// node does not know, so it is neither retried nor counted.
//
// mayRetry, when non-nil, vetoes another attempt after a rejection: the
// columnar path refuses once the server has answered any packet. open must
// open the statement under the context it is given; the context returned is
// the last attempt's, whose per-query settings are the ones the node took.
func openWithDroppableRetry[T any](c *Client, ctx context.Context, open func(context.Context) (T, error), mayRetry func() bool) (T, context.Context, error) {
	return runDroppableRetry(c.rejections, ctx, open, c.droppableRetryContext, mayRetry)
}

// maxDroppableAttempts bounds the statements one query sends through
// runDroppableRetry: the first, and one retry per droppable setting, since
// every retry drops at least the setting the node rejected.
func maxDroppableAttempts() int {
	return len(chopt.DroppableSettings()) + 1
}

// droppableRetryClassifier is droppableRetryContext's signature: it decides
// whether err is a rejection of an unknown name, and whether and under which
// context the statement is sent again.
type droppableRetryClassifier func(ctx context.Context, err error) (retryCtx context.Context, rej UnknownNameRejection, ok bool)

// runDroppableRetry is openWithDroppableRetry's loop over an injected
// classifier. It sends at most maxDroppableAttempts statements. A rejection
// still marked for retry after the last of them means the classifier broke
// the invariant that every retry carries fewer droppable settings: it is
// reported as surfaced and answered with a *DroppableRetryInvariantError
// instead of another round-trip.
func runDroppableRetry[T any](rep *rejectionReporter, ctx context.Context, open func(context.Context) (T, error), classify droppableRetryClassifier, mayRetry func() bool) (T, context.Context, error) {
	limit := maxDroppableAttempts()
	var (
		res T
		err error
		rej UnknownNameRejection
	)
	for attempt := 1; attempt <= limit; attempt++ {
		res, err = open(ctx)
		retryCtx, classified, ok := classify(ctx, err)
		if !ok {
			if rej, ok := analyzerMandatoryRejection(ctx, err); ok {
				rep.notify(rej)
			}
			return res, ctx, err
		}
		rej = classified
		if rej.Retried && mayRetry != nil && !mayRetry() {
			rej.Retried = false
		}
		if rej.Retried && attempt == limit {
			break
		}
		rep.report(rej)
		if !rej.Retried {
			return res, ctx, err
		}
		ctx = retryCtx
	}
	rej.Retried = false
	rep.report(rej)
	return res, ctx, &DroppableRetryInvariantError{Setting: rej.Name, Attempts: limit, Cause: translateCHGoErr(err)}
}

// ErrDroppableRetryInvariant is the sentinel a *DroppableRetryInvariantError
// unwraps to.
var ErrDroppableRetryInvariant = errors.New("chclient: internal invariant broken: the droppable-setting retry did not converge")

// DroppableRetryInvariantError is a query the droppable-setting retry gave up
// on because the node still rejected a droppable setting after
// maxDroppableAttempts statements. Every retry drops at least the rejected
// setting, so reaching the bound is a cerberus bug, never a node's answer;
// Cause is the node's last rejection.
type DroppableRetryInvariantError struct {
	Setting  string
	Attempts int
	Cause    error
}

func (e *DroppableRetryInvariantError) Error() string {
	return fmt.Sprintf(
		"chclient: internal invariant broken: a ClickHouse node still rejected droppable setting %s after %d statements, "+
			"although every retry drops at least the rejected setting; the query was not sent again",
		e.Setting, e.Attempts,
	)
}

// Unwrap exposes the sentinel and the node's last rejection.
func (e *DroppableRetryInvariantError) Unwrap() []error {
	if e.Cause == nil {
		return []error{ErrDroppableRetryInvariant}
	}
	return []error{ErrDroppableRetryInvariant, e.Cause}
}

// capabilityCanaryKeyType keys the capability-canary mark on a context.
type capabilityCanaryKeyType struct{}

// withCapabilityCanary marks ctx as a capability canary's (the workload,
// result-cache, ts-grid and query-log-union probes). A canary sends a setting
// or reads a table to learn whether the server accepts it, so a rejection is
// the answer it asks for, not evidence of a node older than the set in force:
// it is neither retried nor reported.
func withCapabilityCanary(ctx context.Context) context.Context {
	return context.WithValue(ctx, capabilityCanaryKeyType{}, true)
}

// isCapabilityCanary reports whether ctx carries withCapabilityCanary's mark.
func isCapabilityCanary(ctx context.Context) bool {
	marked, _ := ctx.Value(capabilityCanaryKeyType{}).(bool)
	return marked
}

// analyzerMandatoryRejection classifies err as a node refusing the
// enable_analyzer=0 a statement that is not a capability canary's carried,
// because the node's analyzer is mandatory.
func analyzerMandatoryRejection(ctx context.Context, err error) (UnknownNameRejection, bool) {
	u, ok := parseAnalyzerMandatory(translateCHGoErr(err))
	if !ok || isCapabilityCanary(ctx) {
		return UnknownNameRejection{}, false
	}
	return UnknownNameRejection{Kind: u.kind, Name: u.name, NodeVersion: u.nodeVersion, AnalyzerMandatory: true}, true
}

// droppableRetryContext classifies err. ok is false unless err rejects an
// unknown setting or function on a statement that is not a capability
// canary's. When it does, rej describes the rejection and, if rej.Retried,
// retryCtx is ctx without the droppable settings the rejecting node cannot
// know, its ClickHouse settings re-derived to match.
//
// The rejection is retried when the rejected name is a droppable setting the
// query carried at a droppable value. A node that does not know it predates
// its release, so it knows no droppable setting released at or after it
// either: each one the query carries at a droppable value is dropped. One
// released earlier may be known to the node and stays.
func (c *Client) droppableRetryContext(ctx context.Context, err error) (retryCtx context.Context, rej UnknownNameRejection, ok bool) {
	u, ok := parseUnknownName(translateCHGoErr(err))
	if !ok || isCapabilityCanary(ctx) {
		return nil, UnknownNameRejection{}, false
	}
	rej = UnknownNameRejection{Kind: u.kind, Name: u.name, NodeVersion: u.nodeVersion}
	if u.kind != unknownNameSetting {
		return nil, rej, true
	}
	rejected, droppable := chopt.LookupDroppableSetting(u.name)
	perQuery := querySettingsFromContext(ctx)
	sent := c.querySettings(ctx)
	if _, carried := perQuery[u.name]; !droppable || !carried || !rejected.DroppableAt(sent[u.name]) {
		return nil, rej, true
	}
	kept := maps.Clone(perQuery)
	for _, d := range chopt.DroppableSettings() {
		if _, carried := kept[d.Name]; carried && !d.Since.Less(rejected.Since) && d.DroppableAt(sent[d.Name]) {
			delete(kept, d.Name)
		}
	}
	retryCtx = context.WithValue(ctx, perQuerySettingsKey, kept)
	retryCtx = withDriverSettings(retryCtx, c.querySettings(retryCtx))
	rej.Retried = true
	return retryCtx, rej, true
}
