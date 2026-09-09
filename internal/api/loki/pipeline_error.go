package loki

import (
	"fmt"
	"net/http"
	"sort"
	"strconv"
	"strings"

	syntax "github.com/tsouza/cerberus/internal/logql/lsyntax"

	"github.com/tsouza/cerberus/internal/chclient"
)

// Reference Loki refuses to ANSWER a metric query whose pipeline stamped
// `__error__` on a sample. The check is
// pkg/logql/evaluator.go's RangeVectorEvaluator.Next (and its
// AbsentRangeVectorEvaluator twin, and quantile_over_time_sketch.go's
// JoinSampleVector):
//
//	// Errors are not allowed in metrics unless they've been specifically requested.
//	if s.Metric.Has(logqlmodel.ErrorLabel) && s.Metric.Get(logqlmodel.PreserveErrorLabel) != trueString {
//		r.err = logqlmodel.NewPipelineErr(s.Metric)
//		return false, 0, SampleVector{}
//	}
//
// and pkg/util/server/error.go maps the resulting ErrPipeline to 400.
//
// Cerberus answered 200 with the error series alongside the good ones, so
// a `sum_over_time({job="api"} | unwrap latency [5m])` over a stream
// holding one unparseable value drew an extra, permanently-flat series
// into the panel and told the user nothing (cerberus issue #3183). The
// error series is still what the SQL produces — that is how the full
// label set the message quotes is obtained, and it is exactly what
// reference's LabelsBuilder.GroupedLabels returns for an error sample —
// but it is now the trigger for the rejection rather than a result row.
//
// A user opts back into an answer the way they do upstream: by filtering
// the errors out (`| __error__=""`), in which case no error sample
// reaches here, or by asking for them explicitly with
// `__preserve_error__="true"`.
const errPreserveErrorTrue = "true"

// pipelineErrorFor returns reference Loki's PipelineError for the first
// error-carrying sample, or nil when none carries one.
//
// "First" is by the sample order the scan produced, matching reference,
// which reports the first sample its iterator reaches. Only metric
// queries call this: a LOG query keeps `__error__` on the stream labels
// and returns it, which is why the upstream check lives in the sample
// evaluators and not in the entry iterators.
func pipelineErrorFor(samples []chclient.Sample) *apiError {
	for _, s := range samples {
		kind, ok := s.Labels[syntax.ErrorLabel]
		if !ok || kind == "" {
			continue
		}
		if s.Labels[syntax.PreserveErrorLabel] == errPreserveErrorTrue {
			continue
		}
		return &apiError{
			Kind:   ErrBadData,
			Err:    fmt.Errorf("%s", pipelineErrorMessage(kind, s.Labels)),
			Status: http.StatusBadRequest,
		}
	}
	return nil
}

// pipelineErrorMessage reproduces logqlmodel.PipelineError.Error()
// byte-for-byte, including its trailing newline: the message is the wire
// contract a client (and the compatibility differ) reads.
func pipelineErrorMessage(kind string, lbls map[string]string) string {
	return fmt.Sprintf(
		"pipeline error: '%s' for series: '%s'.\n"+
			"Use a label filter to intentionally skip this error. (e.g | __error__!=\"%s\").\n"+
			"To skip all potential errors you can match empty errors.(e.g __error__=\"\")\n"+
			"The label filter can also be specified after unwrap. (e.g | unwrap latency | __error__=\"\" )\n",
		kind, promLabelsString(lbls), kind,
	)
}

// promLabelsString renders a label map the way
// prometheus/model/labels.Labels.String() does — `{a="1", b="2"}`, keys
// sorted, values quoted with Go syntax — which is what upstream
// interpolates into the message.
func promLabelsString(lbls map[string]string) string {
	keys := make([]string, 0, len(lbls))
	for k := range lbls {
		keys = append(keys, k)
	}
	sort.Strings(keys)
	var b strings.Builder
	b.WriteByte('{')
	for i, k := range keys {
		if i > 0 {
			b.WriteString(", ")
		}
		b.WriteString(k)
		b.WriteByte('=')
		b.WriteString(strconv.Quote(lbls[k]))
	}
	b.WriteByte('}')
	return b.String()
}
