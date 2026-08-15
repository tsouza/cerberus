package chplan

// Fn is a symbolic, engine-agnostic function identifier used by FuncCall
// and AggFunc in place of a raw ClickHouse spelling. Each constant names
// what the function DOES, not what CH calls it — the const block below is
// the complete vocabulary chsql's per-dialect resolution table
// (internal/chsql/fnspelling.go) must cover, and it is derived from
// source (see internal/chplan/fn_completeness_test.go and
// internal/chsql/fnspelling_completeness_test.go) rather than restated by
// hand anywhere else.
//
// A doc comment on each constant states its contract as this codebase's
// call sites actually use it: argument shapes and null/error behavior,
// not the full ClickHouse manual entry. Where CH's own name is not
// self-describing (a combinator suffix, an unrelated spelling for a
// PromQL-side concept) the doc comment says so.
//
// This PR (#2060 PR 1) only adds the vocabulary, the resolution table and
// the ratchets — see FuncCall.Fn / AggFunc.Fn for the dual-mode
// contract. Construction sites keep using Name until PRs 2-5 convert
// them per query-language head; nothing observable changes yet.
type Fn string

const (
	// Array functions.
	// array(x, y, ...) — an Array literal built from its positional arguments; the
	// CH element type is the common supertype.
	FnArray Fn = "array"

	// arrayAvg(arr) — the average of a numeric array's elements.
	FnArrayAvg Fn = "arrayAvg"

	// arrayConcat(arr1, arr2, ...) — arrays joined end to end, all operands the
	// same element type.
	FnArrayConcat Fn = "arrayConcat"

	// arrayCumSum(arr) — the running sum of a numeric array, same length as the
	// input.
	FnArrayCumSum Fn = "arrayCumSum"

	// arrayDistinct(arr) — arr with duplicate elements removed, order of first
	// occurrence preserved.
	FnArrayDistinct Fn = "arrayDistinct"

	// arrayElement(arr, i) — the 1-based element at index i; out-of-range returns
	// the element type's zero value, never an error.
	FnArrayElement Fn = "arrayElement"

	// arrayEnumerate(arr) — [1, 2, ..., length(arr)], the 1-based position array
	// for arr.
	FnArrayEnumerate Fn = "arrayEnumerate"

	// arrayExists(lambda, arr) — true if the lambda predicate holds for at least
	// one element.
	FnArrayExists Fn = "arrayExists"

	// arrayFilter(lambda, arr) — the subsequence of arr whose elements satisfy the
	// lambda predicate.
	FnArrayFilter Fn = "arrayFilter"

	// arrayFlatten(arrOfArr) — a nested array flattened by one level.
	FnArrayFlatten Fn = "arrayFlatten"

	// arrayFold(lambda, arr, init) — a left fold over arr, threading the lambda's
	// 2-arg accumulator from init.
	FnArrayFold Fn = "arrayFold"

	// arrayJoin(arr) — explodes arr into one output row per element; changes row
	// cardinality, not just column shape.
	FnArrayJoin Fn = "arrayJoin"

	// arrayMap(lambda, arr, ...) — arr (or several zipped arrays) transformed
	// element-wise by the lambda, same length as the input(s).
	FnArrayMap Fn = "arrayMap"

	// arrayMax(arr) — the maximum element of a numeric array.
	FnArrayMax Fn = "arrayMax"

	// arrayMin(arr) — the minimum element of a numeric array.
	FnArrayMin Fn = "arrayMin"

	// arrayPopBack(arr) — arr with its last element removed; empty input yields
	// empty output.
	FnArrayPopBack Fn = "arrayPopBack"

	// arrayPopFront(arr) — arr with its first element removed; empty input yields
	// empty output.
	FnArrayPopFront Fn = "arrayPopFront"

	// arrayPushBack(arr, x) — arr with x appended.
	FnArrayPushBack Fn = "arrayPushBack"

	// arrayReduce(aggNameString, arr, ...) — arr(s) folded through the CH
	// aggregate named by the first, string-literal argument; the aggregate name
	// itself is data CH interprets, not a chsql-resolved Fn.
	FnArrayReduce Fn = "arrayReduce"

	// arrayReverse(arr) — arr with element order reversed.
	FnArrayReverse Fn = "arrayReverse"

	// arraySlice(arr, offset[, length]) — a 1-based sub-array; a negative offset
	// counts from the end, as in CH.
	FnArraySlice Fn = "arraySlice"

	// arraySort([lambda,] arr) — arr sorted ascending, optionally by a lambda-
	// derived sort key.
	FnArraySort Fn = "arraySort"

	// arrayStringConcat(arr[, sep]) — the elements of a string array joined with
	// sep (default '').
	FnArrayStringConcat Fn = "arrayStringConcat"

	// arraySum(arr) — the sum of a numeric array's elements.
	FnArraySum Fn = "arraySum"

	// arrayZip(arr1, arr2, ...) — same-length arrays zipped into an array of
	// tuples.
	FnArrayZip Fn = "arrayZip"

	// emptyArrayFloat64() — the nullary empty Array(Float64) literal.
	FnEmptyArrayFloat64 Fn = "emptyArrayFloat64"

	// has(arr, x) — true if x is an element of arr (equality, not
	// substring/regex).
	FnArrayHas Fn = "has"

	// range([start,] end[, step]) — the half-open numeric sequence [start, end) as
	// an Array, default start 0 and step 1.
	FnRange Fn = "range"

	// transform(x, from, to[, default]) — x looked up in the parallel from/to
	// arrays and mapped to the matching to element, or default (or x itself with
	// the 3-arg form) if absent.
	FnTransform Fn = "transform"

	// tuple(x, y, ...) — a Tuple literal built from its positional arguments.
	FnTuple Fn = "tuple"

	// tupleElement(t, i) — the 1-based i-th element of tuple t.
	FnTupleElement Fn = "tupleElement"

	// Map functions.
	// map(k1, v1, k2, v2, ...) — a Map literal built from alternating key/value
	// arguments.
	FnMap Fn = "map"

	// mapApply(lambda, m) — m with the lambda applied to each (key, value) pair,
	// producing a new map of the lambda's (key, value) results.
	FnMapApply Fn = "mapApply"

	// mapConcat(m1, m2, ...) — maps merged left to right; a key present in more
	// than one operand keeps the rightmost value.
	FnMapMerge Fn = "mapConcat"

	// mapContains(m, k) — true if k is a key of m.
	FnMapContainsKey Fn = "mapContains"

	// mapFilter(lambda, m) — the sub-map of m whose (key, value) pairs satisfy the
	// 2-arg lambda predicate.
	FnMapFilter Fn = "mapFilter"

	// mapFromArrays(keys, values) — a Map built by pairing the same-length keys
	// and values arrays positionally.
	FnMapFromArrays Fn = "mapFromArrays"

	// mapKeys(m) — m's keys as an Array, in map iteration order.
	FnMapKeys Fn = "mapKeys"

	// mapSort(m) — m with its entries in a canonical key order; establishes the
	// series-identity key-order invariant (see CanonicalMapFunc), whose
	// spelling this constant reuses rather than restates.
	FnMapSort Fn = CanonicalMapFunc

	// mapUpdate(m1, m2) — m1 with every key of m2 overwritten (or inserted) from
	// m2; keys unique to m1 are kept.
	FnMapUpdate Fn = "mapUpdate"

	// mapValues(m) — m's values as an Array, in map iteration order (parallel to
	// mapKeys).
	FnMapValues Fn = "mapValues"

	// String and regex functions.
	// char(code1, code2, ...) — a String built by treating each integer argument
	// as one character code.
	FnChar Fn = "char"

	// coalesce(x, y, ...) — the first non-NULL argument, left to right; NULL if
	// every argument is NULL.
	FnCoalesce Fn = "coalesce"

	// concat(x, y, ...) — string arguments concatenated; non-string operands are
	// not implicitly stringified by this Fn.
	FnConcat Fn = "concat"

	// countSubstrings(haystack, needle) — the number of non-overlapping
	// occurrences of needle in haystack.
	FnCountSubstrings Fn = "countSubstrings"

	// extract(haystack, pattern) — the first regex match, or the first capture
	// group when the pattern has one; '' if no match.
	FnRegexExtractFirst Fn = "extract"

	// extractAll(haystack, pattern) — every regex match (or first-group capture)
	// as Array(String); empty array if no match.
	FnRegexExtractAll Fn = "extractAll"

	// extractAllGroupsHorizontal(haystack, pattern) — every match's capture groups
	// as Array(Array(String)), outer array indexed by match, inner by group.
	FnRegexExtractAllGroupsHorizontal Fn = "extractAllGroupsHorizontal"

	// extractKeyValuePairs(s) — s's `key=value`-shaped substrings parsed into
	// Map(String, String).
	FnExtractKeyValuePairs Fn = "extractKeyValuePairs"

	// isValidJSON(s) — true if s parses as well-formed JSON.
	FnIsValidJSON Fn = "isValidJSON"

	// JSONExtractKeysAndValuesRaw(json) — key/raw-JSON-value pairs of a JSON
	// object as Array(Tuple(String, String)).
	FnJSONExtractKeysAndValuesRaw Fn = "JSONExtractKeysAndValuesRaw"

	// JSONExtractString(json, path...) — a JSON string field, unescaped; returns
	// '' if the path is absent or not a string.
	FnJSONExtractString Fn = "JSONExtractString"

	// leftPad(s, len[, pad]) — s left-padded to len characters with pad (default
	// space); truncated, not padded, if already longer.
	FnLeftPad Fn = "leftPad"

	// length(x) — the element count of an Array/Map, or the byte length of a
	// String.
	FnLength Fn = "length"

	// lower(s) — s with ASCII letters lowercased.
	FnLower Fn = "lower"

	// match(haystack, pattern) — true if pattern (an re2 regular expression)
	// matches anywhere in haystack.
	FnRegexMatch Fn = "match"

	// nullIf(x, y) — NULL when x equals y, else x.
	FnNullIf Fn = "nullIf"

	// parseReadableSize(s) — a human-readable byte-size string ('1.5 MiB') parsed
	// to a Float64 byte count.
	FnParseReadableSize Fn = "parseReadableSize"

	// parseTimeDelta(s) — a human-readable duration string parsed to a Float64
	// second count.
	FnParseTimeDelta Fn = "parseTimeDelta"

	// position(haystack, needle[, startPos]) — the 1-based first occurrence of
	// needle in haystack, or 0 if absent.
	FnStringPosition Fn = "position"

	// replaceAll(haystack, pattern, replacement) — every literal (non-regex)
	// occurrence of pattern replaced.
	FnReplaceAll Fn = "replaceAll"

	// replaceRegexpAll(haystack, pattern, replacement) — every regex match of
	// pattern replaced; replacement may reference capture groups (\1, ...).
	FnRegexReplaceAll Fn = "replaceRegexpAll"

	// replaceRegexpOne(haystack, pattern, replacement) — only the first regex
	// match of pattern replaced.
	FnRegexReplaceFirst Fn = "replaceRegexpOne"

	// startsWith(s, prefix) — true if s begins with prefix.
	FnStartsWith Fn = "startsWith"

	// substring(s, offset[, length]) — a 1-based substring; a negative offset
	// counts from the end, as in CH.
	FnSubstring Fn = "substring"

	// toJSONString(x) — x serialised to a JSON-text String.
	FnToJSONString Fn = "toJSONString"

	// toString(x) — x's default textual representation as a String.
	FnToString Fn = "toString"

	// xxHash64(x) — the 64-bit xxHash of x's byte representation, as UInt64.
	FnXxHash64 Fn = "xxHash64"

	// Control-flow and null-handling functions.
	// greatest(x, y, ...) — the maximum of its arguments; NULL propagates only per
	// CH's usual comparison rules, not specially.
	FnGreatest Fn = "greatest"

	// least(x, y, ...) — the minimum of its arguments.
	FnLeast Fn = "least"

	// if(cond, then, else) — then when cond is true, else otherwise; then and else
	// must share a common type.
	FnIf Fn = "if"

	// ifNull(x, default) — x, or default when x is NULL.
	FnIfNull Fn = "ifNull"

	// isInfinite(x) — true if the Float64 x is +Inf or -Inf.
	FnIsInfinite Fn = "isInfinite"

	// isNaN(x) — true if the Float64 x is NaN.
	FnIsNaN Fn = "isNaN"

	// isNotNull(x) — true unless x is NULL.
	FnIsNotNull Fn = "isNotNull"

	// multiIf(cond1, then1, cond2, then2, ..., else) — the first then whose cond
	// is true, else if none match; all then/else branches share a common type.
	FnMultiIf Fn = "multiIf"

	// not(x) — the boolean negation of x.
	FnNot Fn = "not"

	// throwIf(cond[, message]) — aborts the query with message when cond is true;
	// evaluates to 0 otherwise, so it composes as an expression.
	FnThrowIf Fn = "throwIf"

	// Type-cast and numeric-conversion functions.
	// assumeNotNull(x) — x's Nullable wrapper dropped without a runtime check; a
	// NULL operand renders as the underlying type's default, not an error.
	FnAssumeNotNull Fn = "assumeNotNull"

	// CAST(<expr> AS <type>) — the second argument renders as a bare type token,
	// not a bound value.
	FnCast Fn = "CAST"

	// toDateTime(x) — x (a number or a parseable string) cast to DateTime (second
	// resolution).
	FnToDateTime Fn = "toDateTime"

	// toDateTime64(x, precision) — x cast to DateTime64 at the given fractional-
	// second precision.
	FnToDateTime64 Fn = "toDateTime64"

	// toFloat64(x) — x cast to Float64; aborts the query if x cannot be
	// represented (use ToFloat64OrNull/ToFloat64OrZero to avoid that).
	FnToFloat64 Fn = "toFloat64"

	// toFloat64OrNull(x) — x cast to Nullable(Float64), or NULL if x cannot be
	// parsed/represented.
	FnToFloat64OrNull Fn = "toFloat64OrNull"

	// toFloat64OrZero(x) — x cast to Float64, or 0 if x cannot be
	// parsed/represented.
	FnToFloat64OrZero Fn = "toFloat64OrZero"

	// toInt64(x) — x cast to Int64; aborts the query if x cannot be represented.
	FnToInt64 Fn = "toInt64"

	// toIntervalNanosecond(n) — the Interval value of n nanoseconds, usable in
	// DateTime64 arithmetic.
	FnToIntervalNanosecond Fn = "toIntervalNanosecond"

	// toNullable(x) — x's type wrapped as Nullable, value unchanged.
	FnToNullable Fn = "toNullable"

	// toUInt32(x) — x cast to UInt32; aborts the query if x is negative or out of
	// range.
	FnToUInt32 Fn = "toUInt32"

	// toUInt64(x) — x cast to UInt64; aborts the query if x is negative or out of
	// range.
	FnToUInt64 Fn = "toUInt64"

	// toIPv4(s) — s (dotted-quad string) cast to IPv4; aborts the query if s does
	// not parse.
	FnToIPv4 Fn = "toIPv4"

	// toIPv4OrNull(s) — s cast to Nullable(IPv4), or NULL if s does not parse.
	FnToIPv4OrNull Fn = "toIPv4OrNull"

	// toIPv6(s) — s cast to IPv6; aborts the query if s does not parse.
	FnToIPv6 Fn = "toIPv6"

	// toIPv6OrNull(s) — s cast to Nullable(IPv6), or NULL if s does not parse.
	FnToIPv6OrNull Fn = "toIPv6OrNull"

	// isIPv6String(s) — true if s parses as a valid IPv6 address string.
	FnIsIPv6String Fn = "isIPv6String"

	// Date and time functions.
	// addSeconds(datetime, n) — datetime shifted forward by n seconds (n may be
	// negative).
	FnAddSeconds Fn = "addSeconds"

	// dateDiff(unit, start, end) — end - start expressed as a whole count of unit
	// (a string literal: 'second', 'day', ...).
	FnDateDiff Fn = "dateDiff"

	// fromUnixTimestamp64Nano(n) — the DateTime64(9) instant n nanoseconds after
	// the Unix epoch.
	FnFromUnixNanos Fn = "fromUnixTimestamp64Nano"

	// now() — the current instant as DateTime (second resolution).
	FnNow Fn = "now"

	// now64([precision]) — the current instant as DateTime64, at sub-second
	// precision.
	FnNow64 Fn = "now64"

	// toDayOfMonth(d) — d's day of the calendar month (1-31).
	FnToDayOfMonth Fn = "toDayOfMonth"

	// toDayOfWeek(d) — d's ISO day of the week (1=Monday..7=Sunday).
	FnToDayOfWeek Fn = "toDayOfWeek"

	// toDayOfYear(d) — d's ordinal day within the calendar year (1-366).
	FnToDayOfYear Fn = "toDayOfYear"

	// toHour(dt) — dt's hour-of-day (0-23), in the column's timezone.
	FnToHour Fn = "toHour"

	// toLastDayOfMonth(d) — the Date of the last day of d's calendar month.
	FnToLastDayOfMonth Fn = "toLastDayOfMonth"

	// toMinute(dt) — dt's minute-of-hour (0-59), in the column's timezone.
	FnToMinute Fn = "toMinute"

	// toMonth(d) — d's calendar month (1-12).
	FnToMonth Fn = "toMonth"

	// toUnixTimestamp64Nano(dt) — dt (DateTime64) as an Int64 count of nanoseconds
	// since the Unix epoch.
	FnToUnixNanos Fn = "toUnixTimestamp64Nano"

	// toYear(d) — d's calendar year.
	FnToYear Fn = "toYear"

	// Math functions.
	// abs(x) — the absolute value of a numeric x.
	FnAbs Fn = "abs"

	// bitShiftRight(x, n) — x right-shifted by n bits (integer operand).
	FnBitShiftRight Fn = "bitShiftRight"

	// ceil(x[, n]) — x rounded up to n decimal places (default 0).
	FnCeil Fn = "ceil"

	// exp(x) — e raised to the power x.
	FnExp Fn = "exp"

	// floor(x[, n]) — x rounded down to n decimal places (default 0).
	FnFloor Fn = "floor"

	// log(x) — the natural logarithm of x (CH's spelling for PromQL's ln).
	FnLn Fn = "log"

	// log2(x) — the base-2 logarithm of x.
	FnLog2 Fn = "log2"

	// log10(x) — the base-10 logarithm of x.
	FnLog10 Fn = "log10"

	// pow(base, exp) — base raised to the power exp.
	FnPow Fn = "pow"

	// round(x[, n]) — x rounded to n decimal places (default 0), half-away-from-
	// zero.
	FnRound Fn = "round"

	// sign(x) — -1, 0, or 1 according to the sign of numeric x.
	FnSign Fn = "sign"

	// sqrt(x) — the square root of a non-negative numeric x.
	FnSqrt Fn = "sqrt"

	// acos(x) — the arccosine of x (radians), x in [-1, 1].
	FnAcos Fn = "acos"

	// acosh(x) — the inverse hyperbolic cosine of x, x >= 1.
	FnAcosh Fn = "acosh"

	// asin(x) — the arcsine of x (radians), x in [-1, 1].
	FnAsin Fn = "asin"

	// asinh(x) — the inverse hyperbolic sine of x.
	FnAsinh Fn = "asinh"

	// atan(x) — the arctangent of x (radians).
	FnAtan Fn = "atan"

	// atanh(x) — the inverse hyperbolic tangent of x, x in (-1, 1).
	FnAtanh Fn = "atanh"

	// cos(x) — the cosine of x (radians).
	FnCos Fn = "cos"

	// cosh(x) — the hyperbolic cosine of x.
	FnCosh Fn = "cosh"

	// sin(x) — the sine of x (radians).
	FnSin Fn = "sin"

	// sinh(x) — the hyperbolic sine of x.
	FnSinh Fn = "sinh"

	// tan(x) — the tangent of x (radians).
	FnTan Fn = "tan"

	// tanh(x) — the hyperbolic tangent of x.
	FnTanh Fn = "tanh"

	// degrees(x) — x (radians) converted to degrees.
	FnDegrees Fn = "degrees"

	// radians(x) — x (degrees) converted to radians.
	FnRadians Fn = "radians"

	// Aggregate functions.
	// any(x) aggregate — an arbitrary (implementation-chosen, order-dependent)
	// non-NULL x from the group.
	FnAny Fn = "any"

	// argMax(arg, val) aggregate — the arg value from the row where val is maximal
	// in the group.
	FnArgMax Fn = "argMax"

	// argMin(arg, val) aggregate — the arg value from the row where val is minimal
	// in the group.
	FnArgMin Fn = "argMin"

	// argMinIf(arg, val, cond) aggregate — argMin over only the rows where cond is
	// true; CH's `-If` combinator applied to argMin, spelled as one literal name
	// pending the structural Combinators split (issue #2060 PR 3).
	FnArgMinIf Fn = "argMinIf"

	// avg(x) aggregate — the arithmetic mean of non-NULL x in the group.
	FnAvg Fn = "avg"

	// count([x]) aggregate — row count, or non-NULL count of x when given an
	// argument.
	FnCount Fn = "count"

	// countEqual(arr, x) — the number of elements of arr equal to x.
	FnCountEqual Fn = "countEqual"

	// groupArray(x) aggregate — every x value in the group collected into an
	// Array, in an unspecified (block) order.
	FnGroupArray Fn = "groupArray"

	// max(x) aggregate — the maximum non-NULL x in the group.
	FnMax Fn = "max"

	// min(x) aggregate — the minimum non-NULL x in the group.
	FnMin Fn = "min"

	// quantile(phi)(x) aggregate — the phi-th (0..1) percentile of x in the group,
	// CH's interpolated estimate.
	FnQuantile Fn = "quantile"

	// quantiles(phi1, phi2, ...)(x) aggregate — every requested percentile of x
	// in the group as an Array(Float64), in parameter order.
	FnQuantiles Fn = "quantiles"

	// stddevPop(x) aggregate — the population standard deviation of x in the
	// group.
	FnStddevPop Fn = "stddevPop"

	// sum(x) aggregate — the sum of non-NULL x in the group.
	FnSum Fn = "sum"

	// uniqExact(x) aggregate — the exact count of distinct x values in the group
	// (no sketch approximation).
	FnUniqExact Fn = "uniqExact"

	// varPop(x) aggregate — the population variance of x in the group.
	FnVarPop Fn = "varPop"
)
