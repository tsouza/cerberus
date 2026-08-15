package chsql

import (
	"fmt"

	"github.com/tsouza/cerberus/internal/chplan"
)

// fnRender renders a chplan.Fn's full call expression — name, parens and
// args together — for a ClickHouse shape the plain `<name>(<args>)`
// writer in exprFunc / resolveAggFuncName cannot produce (a call whose
// argument list isn't a flat positional list, say). None of the 149 Fn
// constants declared in internal/chplan/fn.go need one today — every
// current spelling is a plain CH identifier applied to positional args —
// so no entry below sets it. The hook exists so the first irregular
// shape #2060 PR 2-5 introduces has somewhere to go without another
// resolution-table redesign.
type fnRender func(b *Builder, args []chplan.Expr) error

// fnSpelling is one chplan.Fn's ClickHouse rendering. Exactly one of Name
// (the common case) or Render (see fnRender) is set; nothing in this file
// sets both, and resolveDualMode never reads Render when Name is what a
// caller asked to render (AggFunc has no Render path — see
// resolveAggFuncName).
type fnSpelling struct {
	Name   string
	Render fnRender
}

// fnSpellings is chsql's ClickHouse resolution table for chplan.Fn — the
// dialect-specific half of the vocabulary chplan/fn.go declares
// engine-agnostically. A future second dialect (ducksql, say) owns its
// own table of the same shape against the same chplan.Fn keys.
//
// Completeness in both directions — every declared Fn has an entry here,
// every entry here names a declared Fn — is enforced by
// fnspelling_completeness_test.go via a go/parser scan of fn.go's const
// block, not by hand-matching this table against that one.
var fnSpellings = map[chplan.Fn]fnSpelling{
	// Array functions.
	chplan.FnArray:             {Name: "array"},
	chplan.FnArrayAvg:          {Name: "arrayAvg"},
	chplan.FnArrayConcat:       {Name: "arrayConcat"},
	chplan.FnArrayCumSum:       {Name: "arrayCumSum"},
	chplan.FnArrayDistinct:     {Name: "arrayDistinct"},
	chplan.FnArrayElement:      {Name: "arrayElement"},
	chplan.FnArrayEnumerate:    {Name: "arrayEnumerate"},
	chplan.FnArrayExists:       {Name: "arrayExists"},
	chplan.FnArrayFilter:       {Name: "arrayFilter"},
	chplan.FnArrayFlatten:      {Name: "arrayFlatten"},
	chplan.FnArrayFold:         {Name: "arrayFold"},
	chplan.FnArrayJoin:         {Name: "arrayJoin"},
	chplan.FnArrayMap:          {Name: "arrayMap"},
	chplan.FnArrayMax:          {Name: "arrayMax"},
	chplan.FnArrayMin:          {Name: "arrayMin"},
	chplan.FnArrayPopBack:      {Name: "arrayPopBack"},
	chplan.FnArrayPopFront:     {Name: "arrayPopFront"},
	chplan.FnArrayPushBack:     {Name: "arrayPushBack"},
	chplan.FnArrayReduce:       {Name: "arrayReduce"},
	chplan.FnArrayReverse:      {Name: "arrayReverse"},
	chplan.FnArraySlice:        {Name: "arraySlice"},
	chplan.FnArraySort:         {Name: "arraySort"},
	chplan.FnArrayStringConcat: {Name: "arrayStringConcat"},
	chplan.FnArraySum:          {Name: "arraySum"},
	chplan.FnArrayZip:          {Name: "arrayZip"},
	chplan.FnEmptyArrayFloat64: {Name: "emptyArrayFloat64"},
	chplan.FnArrayHas:          {Name: "has"},
	chplan.FnRange:             {Name: "range"},
	chplan.FnTransform:         {Name: "transform"},
	chplan.FnTuple:             {Name: "tuple"},
	chplan.FnTupleElement:      {Name: "tupleElement"},

	// Map functions.
	chplan.FnMap:            {Name: "map"},
	chplan.FnMapApply:       {Name: "mapApply"},
	chplan.FnMapMerge:       {Name: "mapConcat"},
	chplan.FnMapContainsKey: {Name: "mapContains"},
	chplan.FnMapFilter:      {Name: "mapFilter"},
	chplan.FnMapFromArrays:  {Name: "mapFromArrays"},
	chplan.FnMapKeys:        {Name: "mapKeys"},
	chplan.FnMapSort:        {Name: chplan.CanonicalMapFunc},
	chplan.FnMapUpdate:      {Name: "mapUpdate"},
	chplan.FnMapValues:      {Name: "mapValues"},

	// String and regex functions.
	chplan.FnChar:                            {Name: "char"},
	chplan.FnCoalesce:                        {Name: "coalesce"},
	chplan.FnConcat:                          {Name: "concat"},
	chplan.FnCountSubstrings:                 {Name: "countSubstrings"},
	chplan.FnRegexExtractFirst:               {Name: "extract"},
	chplan.FnRegexExtractAll:                 {Name: "extractAll"},
	chplan.FnRegexExtractAllGroupsHorizontal: {Name: "extractAllGroupsHorizontal"},
	chplan.FnExtractKeyValuePairs:            {Name: "extractKeyValuePairs"},
	chplan.FnIsValidJSON:                     {Name: "isValidJSON"},
	chplan.FnJSONExtractKeysAndValuesRaw:     {Name: "JSONExtractKeysAndValuesRaw"},
	chplan.FnJSONExtractString:               {Name: "JSONExtractString"},
	chplan.FnLeftPad:                         {Name: "leftPad"},
	chplan.FnLength:                          {Name: "length"},
	chplan.FnLower:                           {Name: "lower"},
	chplan.FnRegexMatch:                      {Name: "match"},
	chplan.FnNullIf:                          {Name: "nullIf"},
	chplan.FnParseReadableSize:               {Name: "parseReadableSize"},
	chplan.FnParseTimeDelta:                  {Name: "parseTimeDelta"},
	chplan.FnStringPosition:                  {Name: "position"},
	chplan.FnReplaceAll:                      {Name: "replaceAll"},
	chplan.FnRegexReplaceAll:                 {Name: "replaceRegexpAll"},
	chplan.FnRegexReplaceFirst:               {Name: "replaceRegexpOne"},
	chplan.FnStartsWith:                      {Name: "startsWith"},
	chplan.FnSubstring:                       {Name: "substring"},
	chplan.FnToJSONString:                    {Name: "toJSONString"},
	chplan.FnToString:                        {Name: "toString"},
	chplan.FnXxHash64:                        {Name: "xxHash64"},

	// Control-flow and null-handling functions.
	chplan.FnGreatest:   {Name: "greatest"},
	chplan.FnLeast:      {Name: "least"},
	chplan.FnIf:         {Name: "if"},
	chplan.FnIfNull:     {Name: "ifNull"},
	chplan.FnIsInfinite: {Name: "isInfinite"},
	chplan.FnIsNaN:      {Name: "isNaN"},
	chplan.FnIsNotNull:  {Name: "isNotNull"},
	chplan.FnMultiIf:    {Name: "multiIf"},
	chplan.FnNot:        {Name: "not"},
	chplan.FnThrowIf:    {Name: "throwIf"},

	// Type-cast and numeric-conversion functions.
	chplan.FnAssumeNotNull:        {Name: "assumeNotNull"},
	chplan.FnCast:                 {Name: "CAST"},
	chplan.FnToDateTime:           {Name: "toDateTime"},
	chplan.FnToDateTime64:         {Name: "toDateTime64"},
	chplan.FnToFloat64:            {Name: "toFloat64"},
	chplan.FnToFloat64OrNull:      {Name: "toFloat64OrNull"},
	chplan.FnToFloat64OrZero:      {Name: "toFloat64OrZero"},
	chplan.FnToInt64:              {Name: "toInt64"},
	chplan.FnToIntervalNanosecond: {Name: "toIntervalNanosecond"},
	chplan.FnToNullable:           {Name: "toNullable"},
	chplan.FnToUInt32:             {Name: "toUInt32"},
	chplan.FnToUInt64:             {Name: "toUInt64"},
	chplan.FnToIPv4:               {Name: "toIPv4"},
	chplan.FnToIPv4OrNull:         {Name: "toIPv4OrNull"},
	chplan.FnToIPv6:               {Name: "toIPv6"},
	chplan.FnToIPv6OrNull:         {Name: "toIPv6OrNull"},
	chplan.FnIsIPv6String:         {Name: "isIPv6String"},

	// Date and time functions.
	chplan.FnAddSeconds:       {Name: "addSeconds"},
	chplan.FnDateDiff:         {Name: "dateDiff"},
	chplan.FnFromUnixNanos:    {Name: "fromUnixTimestamp64Nano"},
	chplan.FnNow:              {Name: "now"},
	chplan.FnNow64:            {Name: "now64"},
	chplan.FnToDayOfMonth:     {Name: "toDayOfMonth"},
	chplan.FnToDayOfWeek:      {Name: "toDayOfWeek"},
	chplan.FnToDayOfYear:      {Name: "toDayOfYear"},
	chplan.FnToHour:           {Name: "toHour"},
	chplan.FnToLastDayOfMonth: {Name: "toLastDayOfMonth"},
	chplan.FnToMinute:         {Name: "toMinute"},
	chplan.FnToMonth:          {Name: "toMonth"},
	chplan.FnToUnixNanos:      {Name: "toUnixTimestamp64Nano"},
	chplan.FnToYear:           {Name: "toYear"},

	// Math functions.
	chplan.FnAbs:           {Name: "abs"},
	chplan.FnBitShiftRight: {Name: "bitShiftRight"},
	chplan.FnCeil:          {Name: "ceil"},
	chplan.FnExp:           {Name: "exp"},
	chplan.FnFloor:         {Name: "floor"},
	chplan.FnLn:            {Name: "log"},
	chplan.FnLog2:          {Name: "log2"},
	chplan.FnLog10:         {Name: "log10"},
	chplan.FnPow:           {Name: "pow"},
	chplan.FnRound:         {Name: "round"},
	chplan.FnSign:          {Name: "sign"},
	chplan.FnSqrt:          {Name: "sqrt"},
	chplan.FnAcos:          {Name: "acos"},
	chplan.FnAcosh:         {Name: "acosh"},
	chplan.FnAsin:          {Name: "asin"},
	chplan.FnAsinh:         {Name: "asinh"},
	chplan.FnAtan:          {Name: "atan"},
	chplan.FnAtanh:         {Name: "atanh"},
	chplan.FnCos:           {Name: "cos"},
	chplan.FnCosh:          {Name: "cosh"},
	chplan.FnSin:           {Name: "sin"},
	chplan.FnSinh:          {Name: "sinh"},
	chplan.FnTan:           {Name: "tan"},
	chplan.FnTanh:          {Name: "tanh"},
	chplan.FnDegrees:       {Name: "degrees"},
	chplan.FnRadians:       {Name: "radians"},

	// Aggregate functions.
	chplan.FnAny:        {Name: "any"},
	chplan.FnArgMax:     {Name: "argMax"},
	chplan.FnArgMin:     {Name: "argMin"},
	chplan.FnArgMinIf:   {Name: "argMinIf"},
	chplan.FnAvg:        {Name: "avg"},
	chplan.FnCount:      {Name: "count"},
	chplan.FnCountEqual: {Name: "countEqual"},
	chplan.FnGroupArray: {Name: "groupArray"},
	chplan.FnMax:        {Name: "max"},
	chplan.FnMin:        {Name: "min"},
	chplan.FnQuantile:   {Name: "quantile"},
	chplan.FnStddevPop:  {Name: "stddevPop"},
	chplan.FnSum:        {Name: "sum"},
	chplan.FnUniqExact:  {Name: "uniqExact"},
	chplan.FnVarPop:     {Name: "varPop"},
}

// resolveDualMode resolves a FuncCall/AggFunc's Fn/Name pair to a
// rendering. Exactly one of fn/name may be set — kind names the struct
// ("FuncCall" or "AggFunc") in the error when a caller violates that.
//
//   - fn == "": the legacy path every current construction site still
//     takes — name passes through verbatim, render is nil.
//   - fn != "" and name == "": fn resolves through fnSpellings.
//   - fn != "" and name != "": rejected — a FuncCall/AggFunc identifies
//     its function exactly one way, never both.
func resolveDualMode(fn chplan.Fn, name, kind string) (resolvedName string, render fnRender, err error) {
	if fn == "" {
		return name, nil, nil
	}
	if name != "" {
		return "", nil, fmt.Errorf("chsql: %s has both Fn=%q and Name=%q set; pick one", kind, fn, name)
	}
	sp, ok := fnSpellings[fn]
	if !ok {
		return "", nil, fmt.Errorf("%w: chplan.Fn %q has no chsql resolution — "+
			"fnspelling_completeness_test.go should have caught this before it ever reached "+
			"the emitter", ErrUnsupported, fn)
	}
	return sp.Name, sp.Render, nil
}

// resolveAggFuncName resolves af's rendered ClickHouse aggregate-function
// name via resolveDualMode. AggFunc rendering (aggFuncFrag) has no
// fnRender path — every aggregate shape today is the plain
// `<name>[(<params>)](<args>)` ParamAgg produces — so a Fn resolving to a
// Render hook is itself an error here, not a silently-ignored hook.
func resolveAggFuncName(af chplan.AggFunc) (string, error) {
	name, render, err := resolveDualMode(af.Fn, af.Name, "AggFunc")
	if err != nil {
		return "", err
	}
	if render != nil {
		return "", fmt.Errorf("%w: chplan.Fn %q resolves via a render hook, "+
			"which AggFunc rendering does not support", ErrUnsupported, af.Fn)
	}
	return name, nil
}
