package corpus

import (
	"strings"

	"github.com/tsouza/cerberus/internal/logql/lsyntax"
)

// KindInvalid is what [QueryKind] reports for a query cerberus's LogQL
// parser rejects.
const KindInvalid = "invalid"

// QueryKind classifies a concrete LogQL query by parsing it with cerberus's
// own parser: KindMetric for a sample expression, KindLog for a log
// selector, KindInvalid when it does not parse.
func QueryKind(query string) string {
	expr, err := lsyntax.ParseExpr(query)
	if err != nil {
		return KindInvalid
	}
	if _, ok := expr.(lsyntax.SampleExpr); ok {
		return KindMetric
	}
	return KindLog
}

// templateStandIns replaces each template placeholder with a fragment that
// parses in its position, so a definition can be classified before any
// placeholder is resolved. The stand-in values never reach a backend.
var templateStandIns = strings.NewReplacer(
	placeholderSelector, `{kind_probe="x"}`,
	placeholderRange, "1m",
	placeholderLabelName, "kind_probe",
	placeholderLabelValue, "x",
)

// InferKind classifies a still-templated definition query. A definition
// that omits `kind:` gets this answer, so a metric-shaped query expands to
// the single forward case a metric query needs instead of being treated as
// a log query. A query that does not parse even with the placeholders
// substituted is classified KindLog.
func InferKind(templated string) string {
	if QueryKind(templateStandIns.Replace(templated)) == KindMetric {
		return KindMetric
	}
	return KindLog
}
