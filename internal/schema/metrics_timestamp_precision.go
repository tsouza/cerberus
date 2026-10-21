package schema

import (
	"fmt"
	"strconv"
	"strings"

	"github.com/open-telemetry/opentelemetry-collector-contrib/exporter/clickhouseexporter/sqltemplates"
)

// MetricsTimestampPrecision is the precision of the OTel-CH metrics tables'
// timestamp columns (TimeUnix, StartTimeUnix and the Nested
// Exemplars.TimeUnix). It is one fact read three ways: schema provisioning
// creates the columns with ColumnType, the boot requirements check
// classifies the deployed type (MetricsTimestampPrecisionOfType), and the
// emitter renders every timestamp read for it (internal/chsql).
//
// The zero value is MetricsTimestampNanos.
type MetricsTimestampPrecision uint8

const (
	// MetricsTimestampNanos is nanosecond precision: the columns are
	// DateTime64(9).
	MetricsTimestampNanos MetricsTimestampPrecision = iota
	// MetricsTimestampSeconds is a precision coarser than nanoseconds.
	// Provisioning creates the columns DateTime, the column type the OTel-CH
	// exporter's own metrics templates use from v0.157; sub-second sample
	// timestamps are truncated on insert. Every coarser deployed type
	// classifies as it (MetricsTimestampPrecisionOfType).
	MetricsTimestampSeconds
)

// ExemplarTimestampField is the timestamp field of the metrics tables'
// Nested Exemplars column (Metrics.ExemplarsColumn), which system.columns
// reports flattened as `Exemplars.TimeUnix`.
const ExemplarTimestampField = "TimeUnix"

// The configuration spellings of the two precisions.
const (
	metricsTimestampNanosName   = "nanos"
	metricsTimestampSecondsName = "seconds"
)

// String returns the precision's configuration spelling.
func (p MetricsTimestampPrecision) String() string {
	if p == MetricsTimestampSeconds {
		return metricsTimestampSecondsName
	}
	return metricsTimestampNanosName
}

// ParseMetricsTimestampPrecision parses a configured precision. The empty
// string is MetricsTimestampNanos.
func ParseMetricsTimestampPrecision(s string) (MetricsTimestampPrecision, error) {
	switch s {
	case "", metricsTimestampNanosName:
		return MetricsTimestampNanos, nil
	case metricsTimestampSecondsName:
		return MetricsTimestampSeconds, nil
	default:
		return MetricsTimestampNanos, fmt.Errorf("invalid metrics timestamp precision %q (want %q or %q)",
			s, metricsTimestampNanosName, metricsTimestampSecondsName)
	}
}

// ClickHouse's two timestamp type names, the DateTime64 scale that holds
// nanoseconds (also the largest scale DateTime64 takes), and the punctuation
// of a parameterised type.
const (
	dateTimeTypeName     = "DateTime"
	dateTime64TypeName   = "DateTime64"
	dateTime64NanosScale = 9
	typeArgsOpen         = "("
	typeArgsClose        = ")"
	typeArgsSeparator    = ","
	timezoneArgQuote     = "'"
)

// ColumnType is the ClickHouse type schema provisioning gives the timestamp
// columns at precision p.
func (p MetricsTimestampPrecision) ColumnType() string {
	if p == MetricsTimestampSeconds {
		return dateTimeTypeName
	}
	return dateTime64TypeName + typeArgsOpen + strconv.Itoa(dateTime64NanosScale) + typeArgsClose
}

// SQLTemplatePrecision is p as the collector-contrib fork's sqltemplates
// selects the metrics CREATE TABLE templates for it.
func (p MetricsTimestampPrecision) SQLTemplatePrecision() sqltemplates.MetricsTimestampPrecision {
	if p == MetricsTimestampSeconds {
		return sqltemplates.MetricsTimestampSeconds
	}
	return sqltemplates.MetricsTimestampNanos
}

// MetricsTimestampPrecisionOfType classifies a deployed timestamp column
// type as system.columns reports it, reporting false for a type that is not
// a ClickHouse timestamp:
//
//   - DateTime64(9), with or without a timezone, is MetricsTimestampNanos.
//   - DateTime64 at any scale below 9, and DateTime, each with or without a
//     timezone, is MetricsTimestampSeconds: every value such a column holds
//     converts exactly to DateTime64(9), and the emitter reads the column
//     through that conversion.
//
// A timezone names the zone a column displays and parses in; the instant a
// value holds, which is what a metrics query reads, is the same in any.
func MetricsTimestampPrecisionOfType(typ string) (MetricsTimestampPrecision, bool) {
	name, args, ok := splitTypeArgs(strings.TrimSpace(typ))
	if !ok {
		return MetricsTimestampNanos, false
	}
	switch name {
	case dateTimeTypeName:
		if len(args) == 0 || (len(args) == 1 && isTimezoneArg(args[0])) {
			return MetricsTimestampSeconds, true
		}
	case dateTime64TypeName:
		if len(args) == 0 || len(args) > 2 || (len(args) == 2 && !isTimezoneArg(args[1])) {
			return MetricsTimestampNanos, false
		}
		scale, err := strconv.Atoi(args[0])
		if err != nil || scale < 0 || scale > dateTime64NanosScale {
			return MetricsTimestampNanos, false
		}
		if scale == dateTime64NanosScale {
			return MetricsTimestampNanos, true
		}
		return MetricsTimestampSeconds, true
	}
	return MetricsTimestampNanos, false
}

// TimestampTimezoneOfType returns the timezone a DateTime or DateTime64
// column type names, unquoted, or "" for a type that names none — a column
// whose calendar functions (toStartOfDay and friends) use the server's
// timezone.
func TimestampTimezoneOfType(typ string) string {
	name, args, ok := splitTypeArgs(strings.TrimSpace(typ))
	if !ok {
		return ""
	}
	var tz string
	switch {
	case name == dateTimeTypeName && len(args) == 1:
		tz = args[0]
	case name == dateTime64TypeName && len(args) == 2:
		tz = args[1]
	}
	if !isTimezoneArg(tz) {
		return ""
	}
	return strings.TrimSuffix(strings.TrimPrefix(tz, timezoneArgQuote), timezoneArgQuote)
}

// splitTypeArgs splits a ClickHouse type `Name` or `Name(a, b)` into its
// name and trimmed arguments. A timezone name holds no comma, so a plain
// split on the separator is exact for the types
// MetricsTimestampPrecisionOfType accepts.
func splitTypeArgs(typ string) (name string, args []string, ok bool) {
	open := strings.Index(typ, typeArgsOpen)
	if open < 0 {
		return typ, nil, typ != ""
	}
	if !strings.HasSuffix(typ, typeArgsClose) {
		return "", nil, false
	}
	for arg := range strings.SplitSeq(typ[open+len(typeArgsOpen):len(typ)-len(typeArgsClose)], typeArgsSeparator) {
		args = append(args, strings.TrimSpace(arg))
	}
	return strings.TrimSpace(typ[:open]), args, true
}

// isTimezoneArg reports whether arg is a quoted, non-empty timezone name.
func isTimezoneArg(arg string) bool {
	return len(arg) > 2*len(timezoneArgQuote) &&
		strings.HasPrefix(arg, timezoneArgQuote) && strings.HasSuffix(arg, timezoneArgQuote)
}
