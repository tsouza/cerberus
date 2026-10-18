package promql

import "github.com/tsouza/cerberus/internal/chplan"

// The constants below are Go's `time` package calendar constants
// (src/time/time.go), named as that file names them. [goCivilTime] is a
// transcription of that file's arithmetic, so each value must stay equal
// to its upstream counterpart.
const (
	goSecondsPerMinute = 60
	goSecondsPerHour   = 3600
	goSecondsPerDay    = 86400

	// goUnixToAbsolute is upstream's `unixToAbsolute`
	// (`unixToInternal + internalToAbsolute`): the seconds from the
	// absolute zero instant, March 1 of year -goAbsoluteYears, to the Unix
	// epoch.
	goUnixToAbsolute = 9223372028741760000

	// goAbsoluteYears is upstream's `absoluteYears`: the year of the
	// absolute zero instant, negated.
	goAbsoluteYears = 292277022400

	goDaysPer400Years = 146097
	goDaysPer4Years   = 1461

	// goMarchThruDecember is upstream's `marchThruDecember`: the days from
	// March 1 to the end of the year. An absolute year starts on March 1,
	// so a day of the year at or past it falls in January or February.
	goMarchThruDecember = 306

	// goJanuaryFirstYday is the 1-based day of the year of March 1 in a
	// common year, upstream's `1 + 31 + 28`.
	goJanuaryFirstYday = 60

	goDaysPerCommonYear = 365
	goMonthsPerYear     = 12
	goDaysPerWeek       = 7

	// goAbsoluteZeroWeekday is the weekday of the absolute zero instant,
	// a Wednesday (upstream's `Wednesday`, Sunday being 0).
	goAbsoluteZeroWeekday = 3

	// goDayOfMonthOverflow is the day-of-month Prometheus's days_in_month
	// asks `time.Date` for: day 32 of a month normalises to day
	// (32 - days in that month) of the next one.
	goDayOfMonthOverflow = 32
)

// Neri–Schneider's divisions over a March-based year, as upstream's
// "Computations on Times" comment writes them: `century := (4 days + 3) /
// 146097`, `cyear := (4 cdays + 3) / 1461`, `amonth := (5 ayday + 461) /
// 153`, `mday := (5 ayday + 461) % 153 / 5`.
const (
	goDaysToQuarterDays  = 4
	goQuarterDayOffset   = 3
	goYdayToMonthScale   = 5
	goYdayToMonthOffset  = 461
	goMonthSpanQuintDays = 153
)

// goCivilTime renders the calendar of `time.Unix(sec, 0).UTC()` in
// ClickHouse integer arithmetic, exactly as Go's `time` package computes
// it, for every int64 sec.
//
// Go counts seconds from an absolute zero instant chosen so every value
// below is non-negative: abs = uint64(sec + goUnixToAbsolute), wrapping
// modulo 2^64. The wrap is part of the answer, not an edge to avoid:
// the int64 seconds below -goUnixToAbsolute — math.MinInt64, which a NaN
// sample becomes, among them — wrap to the top of the absolute range, and
// Go reports the dates found there. The sum is therefore taken in UInt64,
// where ClickHouse addition and multiplication wrap modulo 2^64 as Go's
// do. Everything after the wrap is non-negative, so `intDiv` and `%`
// floor without any sign correction.
type goCivilTime struct {
	abs     chplan.Expr // absolute seconds, UInt64
	days    chplan.Expr // absolute days: abs / goSecondsPerDay
	century chplan.Expr // 400-year-aligned centuries since the absolute zero
	cyear   chplan.Expr // year within the century, March-based
	ayday   chplan.Expr // 0-based day within the March-based year
}

// goCivil builds the calendar of unixSeconds, an Int64 count of Unix
// seconds.
func goCivil(unixSeconds chplan.Expr) goCivilTime {
	abs := civilAdd(
		civilFn(chplan.FnReinterpretAsUInt64, unixSeconds),
		civilFn(chplan.FnToUInt64, civilLit(goUnixToAbsolute)),
	)
	return goCivilDays(abs, civilFn(chplan.FnIntDiv, abs, civilLit(goSecondsPerDay)))
}

// goCivilDays splits days, a count of absolute days, into upstream's
// `absDays.split`: century, cyear and ayday.
func goCivilDays(abs, days chplan.Expr) goCivilTime {
	quarterDays := civilAdd(civilMul(civilLit(goDaysToQuarterDays), days), civilLit(goQuarterDayOffset))
	cdays := civilFn(chplan.FnIntDiv, civilMod(quarterDays, civilLit(goDaysPer400Years)), civilLit(goDaysToQuarterDays))
	cd := civilAdd(civilMul(civilLit(goDaysToQuarterDays), cdays), civilLit(goQuarterDayOffset))
	return goCivilTime{
		abs:     abs,
		days:    days,
		century: civilFn(chplan.FnIntDiv, quarterDays, civilLit(goDaysPer400Years)),
		cyear:   civilFn(chplan.FnIntDiv, cd, civilLit(goDaysPer4Years)),
		ayday:   civilFn(chplan.FnIntDiv, civilMod(cd, civilLit(goDaysPer4Years)), civilLit(goDaysToQuarterDays)),
	}
}

// janFeb is upstream's `absYday.janFeb`: 1 when the day falls in January
// or February, which close a March-based year.
func (c goCivilTime) janFeb() chplan.Expr {
	return &chplan.Binary{Op: chplan.OpGe, Left: c.ayday, Right: civilLit(goMarchThruDecember)}
}

// monthSpan is `5 ayday + 461`, the numerator of upstream's month and
// day-of-month divisions.
func (c goCivilTime) monthSpan() chplan.Expr {
	return civilAdd(civilMul(civilLit(goYdayToMonthScale), c.ayday), civilLit(goYdayToMonthOffset))
}

// year is upstream's `absCentury.year`: `century*100 - absoluteYears +
// cyear + janFeb`.
func (c goCivilTime) year() chplan.Expr {
	const yearsPerCentury = 100
	marchYear := civilAdd(civilAdd(civilMul(civilLit(yearsPerCentury), c.century), c.cyear), c.janFeb())
	return civilSub(marchYear, civilLit(goAbsoluteYears))
}

// month is upstream's `absMonth.month`: the March-based month less 12 in
// January and February.
func (c goCivilTime) month() chplan.Expr {
	amonth := civilFn(chplan.FnIntDiv, c.monthSpan(), civilLit(goMonthSpanQuintDays))
	return civilSub(amonth, civilMul(civilLit(goMonthsPerYear), c.janFeb()))
}

// dayOfMonth is upstream's 1-based `mday` from `absYday.split`.
func (c goCivilTime) dayOfMonth() chplan.Expr {
	const firstDayOfMonth = 1
	return civilAdd(
		civilFn(chplan.FnIntDiv, civilMod(c.monthSpan(), civilLit(goMonthSpanQuintDays)), civilLit(goYdayToMonthScale)),
		civilLit(firstDayOfMonth),
	)
}

// dayOfYear is upstream's `absYday.yday`: `ayday + 60 + leap&^janFeb -
// 365*janFeb`, where leap is upstream's `absCentury.leap`.
func (c goCivilTime) dayOfYear() chplan.Expr {
	const (
		leapCycleYears    = 4
		centuriesPerCycle = 4
	)
	divisible := func(e chplan.Expr, n int64) chplan.Expr {
		return &chplan.Binary{Op: chplan.OpEq, Left: civilMod(e, civilLit(n)), Right: civilLit(0)}
	}
	leap := &chplan.Binary{
		Op:   chplan.OpAnd,
		Left: divisible(c.cyear, leapCycleYears),
		Right: &chplan.Binary{
			Op:    chplan.OpOr,
			Left:  &chplan.Binary{Op: chplan.OpNe, Left: c.cyear, Right: civilLit(0)},
			Right: divisible(c.century, centuriesPerCycle),
		},
	}
	beforeJanuary := &chplan.Binary{Op: chplan.OpLt, Left: c.ayday, Right: civilLit(goMarchThruDecember)}
	leapDay := &chplan.Binary{Op: chplan.OpAnd, Left: leap, Right: beforeJanuary}
	return civilSub(
		civilAdd(civilAdd(c.ayday, civilLit(goJanuaryFirstYday)), leapDay),
		civilMul(civilLit(goDaysPerCommonYear), c.janFeb()),
	)
}

// dayOfWeek is upstream's `absDays.weekday`, Sunday being 0 — PromQL's
// day_of_week convention as well.
func (c goCivilTime) dayOfWeek() chplan.Expr {
	return civilMod(civilAdd(c.days, civilLit(goAbsoluteZeroWeekday)), civilLit(goDaysPerWeek))
}

// daysInMonth is Prometheus's `32 - time.Date(year, month, 32, ...).Day()`.
//
// `time.Date` turns the date back into absolute seconds: day 32 of the
// month is `days - mday + 32` absolute days, times goSecondsPerDay,
// wrapping modulo 2^64 as the upstream int64 arithmetic does. The product
// is taken in UInt64 for that wrap, which a month at the top of the
// absolute range reaches.
func (c goCivilTime) daysInMonth() chplan.Expr {
	overflowDay := civilSub(civilAdd(c.days, civilLit(goDayOfMonthOverflow)), c.dayOfMonth())
	abs := civilMul(civilFn(chplan.FnToUInt64, overflowDay), civilFn(chplan.FnToUInt64, civilLit(goSecondsPerDay)))
	normalised := goCivilDays(abs, civilFn(chplan.FnIntDiv, abs, civilLit(goSecondsPerDay)))
	return civilSub(civilLit(goDayOfMonthOverflow), normalised.dayOfMonth())
}

// hour is upstream's `Time.Hour`: `abs % 86400 / 3600`.
func (c goCivilTime) hour() chplan.Expr {
	return civilFn(chplan.FnIntDiv, civilMod(c.abs, civilLit(goSecondsPerDay)), civilLit(goSecondsPerHour))
}

// minute is upstream's `Time.Minute`: `abs % 3600 / 60`.
func (c goCivilTime) minute() chplan.Expr {
	return civilFn(chplan.FnIntDiv, civilMod(c.abs, civilLit(goSecondsPerHour)), civilLit(goSecondsPerMinute))
}

func civilLit(v int64) chplan.Expr { return &chplan.LitInt{V: v} }

func civilFn(fn chplan.Fn, args ...chplan.Expr) chplan.Expr {
	return &chplan.FuncCall{Fn: fn, Args: args}
}

func civilAdd(l, r chplan.Expr) chplan.Expr {
	return &chplan.Binary{Op: chplan.OpAdd, Left: l, Right: r}
}

func civilSub(l, r chplan.Expr) chplan.Expr {
	return &chplan.Binary{Op: chplan.OpSub, Left: l, Right: r}
}

func civilMul(l, r chplan.Expr) chplan.Expr {
	return &chplan.Binary{Op: chplan.OpMul, Left: l, Right: r}
}

// civilMod is the integer remainder. It is never [chplan.OpMod], which
// renders PromQL's float `%`.
func civilMod(l, r chplan.Expr) chplan.Expr {
	return civilFn(chplan.FnModulo, l, r)
}
