// Package classad provides ClassAd matching functionality.
package classad

import (
	"fmt"
	"math"
	"math/rand"
	"regexp"
	"strconv"
	"strings"
	"time"
)

func isHexDigit(b byte) bool {
	return (b >= '0' && b <= '9') || (b >= 'a' && b <= 'f') || (b >= 'A' && b <= 'F')
}

// floatToInt64 converts a real to int64 the way int() does in the reference
// engine: NaN is 0, and a value out of int64 range (including +/-Inf)
// saturates to the nearest bound rather than wrapping (Go's int64(f) is
// undefined out of range). In range, it truncates toward zero.
func floatToInt64(f float64) int64 {
	switch {
	case math.IsNaN(f):
		return 0
	case f >= 9223372036854775807: // >= 2^63-1; +Inf included
		return math.MaxInt64
	case f <= -9223372036854775808: // <= -2^63; -Inf included
		return math.MinInt64
	default:
		return int64(f)
	}
}

// parseLeadingFloat mimics C strtod, which int()/real() use to convert a
// string: skip leading whitespace, then parse the longest valid
// floating-point prefix (so "5abc" yields 5 and " 5 " yields 5). It reports
// ok=false only when no numeric characters are consumed ("abc", ""). It accepts
// the same forms as strtod: decimal, hexadecimal (0x1, 0xff, hex floats like
// 0x1p4 -- with or without the binary exponent that Go's ParseFloat requires),
// and the inf/infinity/nan spellings. So real("0x1") is 1, int("0xff") is 255,
// and int("inf") is +Inf (which builtinInt saturates).
func parseLeadingFloat(s string) (float64, bool) {
	i := 0
	for i < len(s) && (s[i] == ' ' || s[i] == '\t' || s[i] == '\n' || s[i] == '\r' || s[i] == '\v' || s[i] == '\f') {
		i++
	}
	start := i
	if i < len(s) && (s[i] == '+' || s[i] == '-') {
		i++
	}
	rest := s[i:]
	// inf / infinity / nan (case-insensitive), as strtod accepts.
	for _, word := range []string{"infinity", "inf", "nan"} {
		if len(rest) >= len(word) && strings.EqualFold(rest[:len(word)], word) {
			f, err := strconv.ParseFloat(s[start:i+len(word)], 64)
			return f, err == nil
		}
	}
	// Hexadecimal: 0x<hex>[.<hex>][p[+/-]<dec>]. Go's ParseFloat parses hex
	// floats only with a binary 'p' exponent, so synthesize "p0" when absent.
	if i+1 < len(s) && s[i] == '0' && (s[i+1] == 'x' || s[i+1] == 'X') {
		j := i + 2
		mant := false
		for j < len(s) && isHexDigit(s[j]) {
			j++
			mant = true
		}
		if j < len(s) && s[j] == '.' {
			j++
			for j < len(s) && isHexDigit(s[j]) {
				j++
				mant = true
			}
		}
		if mant {
			hasExp := false
			if j < len(s) && (s[j] == 'p' || s[j] == 'P') {
				k := j + 1
				if k < len(s) && (s[k] == '+' || s[k] == '-') {
					k++
				}
				if k < len(s) && s[k] >= '0' && s[k] <= '9' {
					j = k
					for j < len(s) && s[j] >= '0' && s[j] <= '9' {
						j++
					}
					hasExp = true
				}
			}
			tok := s[start:j]
			if !hasExp {
				tok += "p0"
			}
			f, err := strconv.ParseFloat(tok, 64)
			return f, err == nil
		}
		// "0x" with no hex digit: the leading "0" parses as decimal zero.
	}
	digits := false
	for i < len(s) && s[i] >= '0' && s[i] <= '9' {
		i++
		digits = true
	}
	if i < len(s) && s[i] == '.' {
		i++
		for i < len(s) && s[i] >= '0' && s[i] <= '9' {
			i++
			digits = true
		}
	}
	if digits && i < len(s) && (s[i] == 'e' || s[i] == 'E') {
		j := i + 1
		if j < len(s) && (s[j] == '+' || s[j] == '-') {
			j++
		}
		if j < len(s) && s[j] >= '0' && s[j] <= '9' {
			i = j
			for i < len(s) && s[i] >= '0' && s[i] <= '9' {
				i++
			}
		}
	}
	if !digits {
		return 0, false
	}
	f, err := strconv.ParseFloat(s[start:i], 64)
	if err != nil {
		return 0, false
	}
	return f, true
}

// coerceToReal converts a value to a real the way the reference engine's
// convertValueToRealValue does, for the math builtins (floor/ceiling/round/
// pow/quantize): integers, reals and booleans convert directly, and a string
// is parsed via strtod. Undefined, error, lists and classads cannot convert.
// (This is deliberately distinct from numericOperand, which the arithmetic and
// comparison operators use and which does NOT coerce strings.)
func coerceToReal(v Value) (float64, bool) {
	switch v.valueType {
	case IntegerValue:
		return float64(v.intVal), true
	case RealValue:
		return v.realVal, true
	case BooleanValue:
		if v.boolVal {
			return 1, true
		}
		return 0, true
	case StringValue:
		return parseLeadingFloat(v.strVal)
	default:
		return 0, false
	}
}

// Built-in string functions

// builtinStrcat concatenates strings
// classadReal renders a real exactly as the reference unparser does
// (ClassAdUnParser::UnparseReal): zero via %.1f ("0.0"/"-0.0"), the non-finite
// values via the real("...") forms, everything else via %1.15E (e.g.
// 1.5 -> "1.500000000000000E+00").
func classadReal(r float64) string {
	switch {
	case math.IsNaN(r):
		return `real("NaN")`
	case math.IsInf(r, -1):
		return `real("-INF")`
	case math.IsInf(r, 1):
		return `real("INF")`
	case r == 0:
		return fmt.Sprintf("%.1f", r)
	default:
		return fmt.Sprintf("%1.15E", r)
	}
}

// unparseString renders a string in the reference sink form: wrapped in double
// quotes with control characters escaped and non-printable bytes written as
// octal (ClassAdUnParser::UnparseString). Used for string elements nested
// inside a list, where they appear quoted (a top-level string() of a string is
// unquoted, handled by classadScalarString).
func unparseString(s string) string {
	var b strings.Builder
	b.WriteByte('"')
	for i := 0; i < len(s); i++ {
		c := s[i]
		switch c {
		case '"':
			b.WriteString(`\"`)
		case '\\':
			b.WriteString(`\\`)
		case '\a':
			b.WriteString(`\a`)
		case '\b':
			b.WriteString(`\b`)
		case '\f':
			b.WriteString(`\f`)
		case '\n':
			b.WriteString(`\n`)
		case '\r':
			b.WriteString(`\r`)
		case '\t':
			b.WriteString(`\t`)
		case '\v':
			b.WriteString(`\v`)
		default:
			if c < 0x20 || c > 0x7e { // !isprint
				fmt.Fprintf(&b, "\\%03o", c)
			} else {
				b.WriteByte(c)
			}
		}
	}
	b.WriteByte('"')
	return b.String()
}

// unparseValueNested renders a value as it appears inside a list literal: like
// classadScalarString but strings are quoted and undefined/error print as their
// keywords. Returns ok=false for nested classads (not yet supported).
func unparseValueNested(v Value) (string, bool) {
	switch {
	case v.IsUndefined():
		return "undefined", true
	case v.IsError():
		return "error", true
	case v.IsString():
		s, _ := v.StringValue()
		return unparseString(s), true
	case v.IsList():
		return unparseList(v)
	default:
		return classadScalarString(v)
	}
}

// unparseList renders a list value in reference sink form, "{ e1,e2 }" (and
// "{  }" when empty). Returns ok=false if any element cannot be unparsed.
//
// A list from a literal carries its source element expressions, which are
// unparsed directly (so string({1, 1+1}) is "{ 1,1 + 1 }", matching the
// reference engine, which stores a list as its unevaluated ExprList). A list
// built programmatically (e.g. by split()) has no source expressions, so its
// already-evaluated element values are unparsed instead.
func unparseList(v Value) (string, bool) {
	if v.listExprs != nil {
		var b strings.Builder
		b.WriteString("{ ")
		for i, e := range v.listExprs {
			if i > 0 {
				b.WriteByte(',')
			}
			b.WriteString(unparseExprString(e))
		}
		b.WriteString(" }")
		return b.String(), true
	}

	elems, err := v.ListValue()
	if err != nil {
		return "", false
	}
	var b strings.Builder
	b.WriteString("{ ")
	for i, e := range elems {
		s, ok := unparseValueNested(e)
		if !ok {
			return "", false
		}
		if i > 0 {
			b.WriteByte(',')
		}
		b.WriteString(s)
	}
	b.WriteString(" }")
	return b.String(), true
}

// classadString converts a value to the string form used by string(), strcat(),
// strcmp()/stricmp() and toUpper()/toLower(): a bare scalar uses its plain
// string form (a top-level string is unquoted), while a list is unparsed in
// sink form with its elements quoted/escaped. ok=false for values that cannot
// be converted (currently nested classads), which callers turn into an error.
//
// NOTE: lists hold already-evaluated elements, so this matches the reference
// engine for lists of literal/scalar values but not for elements that were
// non-trivial expressions (the reference unparses the original expression,
// e.g. string({1, 1+1}) is "{ 1,1 + 1 }" there but "{ 1,2 }" here).
func classadString(v Value) (string, bool) {
	if v.IsList() {
		return unparseList(v)
	}
	return classadScalarString(v)
}

// classadScalarString converts a scalar value to its reference string form,
// used by string() and strcat(). It reports ok=false for values that are not
// scalars (lists, classads), which those builtins still reject. Callers handle
// undefined/error before calling this.
func classadScalarString(v Value) (string, bool) {
	switch {
	case v.IsString():
		s, _ := v.StringValue()
		return s, true
	case v.IsInteger():
		i, _ := v.IntValue()
		return fmt.Sprintf("%d", i), true
	case v.IsBool():
		b, _ := v.BoolValue()
		if b {
			return "true", true
		}
		return "false", true
	case v.IsReal():
		r, _ := v.RealValue()
		return classadReal(r), true
	default:
		return "", false
	}
}

func builtinStrcat(args []Value) Value {
	var result strings.Builder
	// Arguments are processed left to right; the first error or undefined wins
	// (strcat(error, undefined) is error, strcat(undefined, error) is
	// undefined). Other scalars are coerced to their string form.
	for _, arg := range args {
		if arg.IsError() {
			return NewErrorValue()
		}
		if arg.IsUndefined() {
			return NewUndefinedValue()
		}
		str, ok := classadString(arg)
		if !ok {
			return NewErrorValue()
		}
		result.WriteString(str)
	}
	return NewStringValue(result.String())
}

// builtinSubstr extracts substring(string, offset[, length])
func builtinSubstr(args []Value) Value {
	if len(args) < 2 || len(args) > 3 {
		return NewErrorValue()
	}

	// In the reference engine an undefined argument dominates over an error
	// one (e.g. substr(error, undefined, 1) is undefined), so check all
	// arguments for undefined first, then for error.
	for _, arg := range args {
		if arg.IsUndefined() {
			return NewUndefinedValue()
		}
	}
	for _, arg := range args {
		if arg.IsError() {
			return NewErrorValue()
		}
	}

	if !args[0].IsString() || !args[1].IsInteger() {
		return NewErrorValue()
	}

	str, _ := args[0].StringValue()
	offset64, _ := args[1].IntValue()

	threeArg := len(args) == 3
	var length64 int64 // defaults to 0 for the two-argument form
	if threeArg {
		if !args[2].IsInteger() {
			return NewErrorValue()
		}
		length64, _ = args[2].IntValue()
	}
	// The reference engine reads the offset and length into 32-bit ints, so an
	// out-of-int32-range argument is truncated (e.g. a huge offset can wrap to
	// a small or negative value); mirror that before clamping.
	offset := int64(int32(offset64))
	length := int64(int32(length64))
	origLen := length

	// Perl-like substr (matching subString in fnCall.cpp): a negative offset
	// counts from the end (clamped to 0), an offset past the end is the end; a
	// non-positive length counts from the end of the string (clamped to 0),
	// and a too-large length is clamped to what remains.
	alen := int64(len(str))
	if offset < 0 {
		offset = alen + offset
		if offset < 0 {
			offset = 0
		}
	} else if offset >= alen {
		offset = alen
	}
	if length <= 0 {
		length = alen - offset + length
		if length < 0 {
			length = 0
		}
	} else if length > alen-offset {
		length = alen - offset
	}
	// An explicitly-supplied length of 0 yields the empty string.
	if threeArg && origLen == 0 {
		length = 0
	}

	return NewStringValue(str[offset : offset+length])
}

// builtinSize returns the size of a string or list
func builtinSize(args []Value) Value {
	if len(args) != 1 {
		return NewErrorValue()
	}

	if args[0].IsError() {
		return NewErrorValue()
	}
	if args[0].IsUndefined() {
		return NewUndefinedValue()
	}

	if args[0].IsString() {
		str, _ := args[0].StringValue()
		return NewIntValue(int64(len(str)))
	}

	if args[0].IsList() {
		// Count elements without evaluating them (size({C}) is 1 even when C
		// would cycle), matching the reference engine, which counts the
		// unevaluated ExprList.
		return NewIntValue(int64(args[0].listLen()))
	}

	return NewErrorValue()
}

// builtinToLower converts string to lowercase
func builtinToLower(args []Value) Value {
	if len(args) != 1 {
		return NewErrorValue()
	}

	if args[0].IsError() {
		return NewErrorValue()
	}
	if args[0].IsUndefined() {
		return NewUndefinedValue()
	}
	// Non-string scalars are coerced to their string form first (so
	// toLower(1.5) lowercases "1.500000000000000E+00").
	str, ok := classadString(args[0])
	if !ok {
		return NewErrorValue()
	}
	return NewStringValue(strings.ToLower(str))
}

// builtinToUpper converts string to uppercase
func builtinToUpper(args []Value) Value {
	if len(args) != 1 {
		return NewErrorValue()
	}

	if args[0].IsError() {
		return NewErrorValue()
	}
	if args[0].IsUndefined() {
		return NewUndefinedValue()
	}
	str, ok := classadString(args[0])
	if !ok {
		return NewErrorValue()
	}
	return NewStringValue(strings.ToUpper(str))
}

// Built-in math functions

// builtinFloor returns the floor of a number
func builtinFloor(args []Value) Value {
	if len(args) != 1 {
		return NewErrorValue()
	}

	if args[0].IsError() {
		return NewErrorValue()
	}
	// An integer argument is returned unchanged (matching the reference): a
	// float round-trip would lose precision for large magnitudes.
	if args[0].IsInteger() {
		return args[0]
	}
	// Booleans and numeric strings coerce to a number (floor("2.5") == 2);
	// undefined or any other non-number is an error -- unlike int()/real()
	// which propagate undefined.
	num, ok := coerceToReal(args[0])
	if !ok {
		return NewErrorValue()
	}
	return NewIntValue(int64(math.Floor(num)))
}

// builtinCeiling returns the ceiling of a number
func builtinCeiling(args []Value) Value {
	if len(args) != 1 {
		return NewErrorValue()
	}

	if args[0].IsError() {
		return NewErrorValue()
	}
	if args[0].IsInteger() {
		return args[0]
	}
	num, ok := coerceToReal(args[0])
	if !ok {
		return NewErrorValue()
	}
	return NewIntValue(int64(math.Ceil(num)))
}

// builtinRound rounds a number to nearest integer
func builtinRound(args []Value) Value {
	if len(args) != 1 {
		return NewErrorValue()
	}

	if args[0].IsError() {
		return NewErrorValue()
	}
	if args[0].IsInteger() {
		return args[0]
	}
	num, ok := coerceToReal(args[0])
	if !ok {
		return NewErrorValue()
	}
	// The reference engine rounds half to even (C rint), so round(2.5) == 2
	// and round(3.5) == 4, not half-away-from-zero.
	return NewIntValue(int64(math.RoundToEven(num)))
}

// builtinRandom returns a random real number between 0 and 1 (or up to max if specified)
func builtinRandom(args []Value) Value {
	if len(args) > 1 {
		return NewErrorValue()
	}

	if len(args) == 0 {
		return NewRealValue(rand.Float64())
	}

	if args[0].IsError() {
		return NewErrorValue()
	}
	if args[0].IsUndefined() {
		return NewUndefinedValue()
	}
	if !args[0].IsNumber() {
		return NewErrorValue()
	}

	maxVal, _ := args[0].NumberValue()
	return NewRealValue(rand.Float64() * maxVal)
}

// builtinInt converts to integer
func builtinInt(args []Value) Value {
	if len(args) != 1 {
		return NewErrorValue()
	}

	if args[0].IsError() {
		return NewErrorValue()
	}
	if args[0].IsUndefined() {
		return NewUndefinedValue()
	}

	if args[0].IsInteger() {
		return args[0]
	}

	if args[0].IsReal() {
		num, _ := args[0].RealValue()
		return NewIntValue(floatToInt64(num))
	}

	if args[0].IsBool() {
		b, _ := args[0].BoolValue()
		if b {
			return NewIntValue(1)
		}
		return NewIntValue(0)
	}

	// A string is parsed as a number (via strtod) and truncated toward zero,
	// matching the reference: int("1.9") == 1, int("-3.7") == -3, int("abc")
	// is an error. int("0xff") == 255, int("inf") saturates to MaxInt64.
	if args[0].IsString() {
		s, _ := args[0].StringValue()
		if f, ok := parseLeadingFloat(s); ok {
			return NewIntValue(floatToInt64(f))
		}
		return NewErrorValue()
	}

	return NewErrorValue()
}

// builtinReal converts to real
func builtinReal(args []Value) Value {
	if len(args) != 1 {
		return NewErrorValue()
	}

	if args[0].IsError() {
		return NewErrorValue()
	}
	if args[0].IsUndefined() {
		return NewUndefinedValue()
	}

	if args[0].IsReal() {
		return args[0]
	}

	if args[0].IsInteger() {
		num, _ := args[0].IntValue()
		return NewRealValue(float64(num))
	}

	// Booleans convert to 1.0 / 0.0, matching the reference engine.
	if args[0].IsBool() {
		b, _ := args[0].BoolValue()
		if b {
			return NewRealValue(1)
		}
		return NewRealValue(0)
	}

	// A string is parsed as a real (via strtod): real("3.14") == 3.14,
	// real("x") is an error.
	if args[0].IsString() {
		s, _ := args[0].StringValue()
		if f, ok := parseLeadingFloat(s); ok {
			return NewRealValue(f)
		}
		return NewErrorValue()
	}

	return NewErrorValue()
}

// Built-in type checking functions

// builtinIsUndefined checks if value is undefined
func builtinIsUndefined(args []Value) Value {
	if len(args) != 1 {
		return NewErrorValue()
	}
	return NewBoolValue(args[0].IsUndefined())
}

// builtinIsError checks if value is an error
func builtinIsError(args []Value) Value {
	if len(args) != 1 {
		return NewErrorValue()
	}
	return NewBoolValue(args[0].IsError())
}

// builtinIsString checks if value is a string
func builtinIsString(args []Value) Value {
	if len(args) != 1 {
		return NewErrorValue()
	}
	return NewBoolValue(args[0].IsString())
}

// builtinIsInteger checks if value is an integer
func builtinIsInteger(args []Value) Value {
	if len(args) != 1 {
		return NewErrorValue()
	}
	return NewBoolValue(args[0].IsInteger())
}

// builtinIsReal checks if value is a real number
func builtinIsReal(args []Value) Value {
	if len(args) != 1 {
		return NewErrorValue()
	}
	return NewBoolValue(args[0].IsReal())
}

// builtinIsBoolean checks if value is a boolean
func builtinIsBoolean(args []Value) Value {
	if len(args) != 1 {
		return NewErrorValue()
	}
	return NewBoolValue(args[0].IsBool())
}

// builtinIsList checks if value is a list
func builtinIsList(args []Value) Value {
	if len(args) != 1 {
		return NewErrorValue()
	}
	return NewBoolValue(args[0].IsList())
}

// builtinIsClassAd checks if value is a ClassAd
func builtinIsClassAd(args []Value) Value {
	if len(args) != 1 {
		return NewErrorValue()
	}
	return NewBoolValue(args[0].IsClassAd())
}

// Built-in time functions

// builtinTime returns current Unix timestamp
func builtinTime(args []Value) Value {
	if len(args) != 0 {
		return NewErrorValue()
	}
	return NewIntValue(time.Now().Unix())
}

// Built-in list functions

// builtinMember checks if element is in list
func builtinMember(args []Value) Value {
	if len(args) != 2 {
		return NewErrorValue()
	}

	// An undefined argument dominates over an error one (matching the
	// reference): check undefined first, then error.
	if args[0].IsUndefined() || args[1].IsUndefined() {
		return NewUndefinedValue()
	}
	if args[0].IsError() || args[1].IsError() {
		return NewErrorValue()
	}

	// The list to search must be a list, and the target must be comparable:
	// a list or classad target is an error (matching the reference).
	if !args[1].IsList() || args[0].IsList() || args[0].IsClassAd() {
		return NewErrorValue()
	}

	element := args[0]
	list, _ := args[1].ListValue()

	// Membership uses the == operator's semantics (numeric coercion,
	// case-insensitive strings). A comparison that is not boolean-true -- including
	// one that evaluates to error or undefined -- simply does not match, so a
	// non-comparable element does not abort the search.
	for _, item := range list {
		if eq := valuesEqual(item, element); eq.IsBool() {
			if b, _ := eq.BoolValue(); b {
				return NewBoolValue(true)
			}
		}
	}

	return NewBoolValue(false)
}

// builtinStringListMember checks if a string is a member of a comma-separated string list
// stringListMember(string item, string list [, string options])
// The list is a comma-separated string. Options can contain:
// - "i" or "I": case-insensitive comparison
// stringListStrArg coerces a stringList membership/subset argument to its
// string form, treating undefined as the empty string -- these functions treat
// an undefined item/list as empty rather than propagating undefined, so e.g.
// stringListMember(undefined, "a") is false and stringListSubsetMatch(undefined,
// "a") is true (the empty list is a subset of anything). It reports ok=false for
// an argument that is neither a string nor undefined (error, number, list, ...),
// which the caller turns into an error.
func stringListStrArg(v Value) (s string, ok bool) {
	if v.IsUndefined() {
		return "", true
	}
	if v.IsString() {
		s, _ = v.StringValue()
		return s, true
	}
	return "", false
}

func builtinStringListMember(args []Value) Value {
	if len(args) < 2 || len(args) > 3 {
		return NewErrorValue()
	}

	item, ok0 := stringListStrArg(args[0])
	listStr, ok1 := stringListStrArg(args[1])
	if !ok0 || !ok1 {
		return NewErrorValue()
	}

	// Check for options
	ignoreCase := false
	if len(args) == 3 {
		options, ok2 := stringListStrArg(args[2])
		if !ok2 {
			return NewErrorValue()
		}
		if strings.ContainsAny(options, "iI") {
			ignoreCase = true
		}
	}

	// Split the list by commas and check each element
	elements := strings.Split(listStr, ",")
	for _, elem := range elements {
		// Trim whitespace from each element
		elem = strings.TrimSpace(elem)
		if ignoreCase {
			if strings.EqualFold(elem, item) {
				return NewBoolValue(true)
			}
		} else {
			if elem == item {
				return NewBoolValue(true)
			}
		}
	}

	return NewBoolValue(false)
}

// builtinStringListIMember is a convenience wrapper for stringListMember with case-insensitive matching.
// stringListIMember(string item, string list)
// Returns true if item is in list (case-insensitive), false otherwise
func builtinStringListIMember(args []Value) Value {
	if len(args) != 2 {
		return NewErrorValue()
	}

	// Call stringListMember with "i" option for case-insensitive matching
	return builtinStringListMember([]Value{args[0], args[1], NewStringValue("i")})
}

// builtinRegexp checks if a string matches a regular expression
// regexp(string pattern, string target [, string options])
// Options can contain:
// - "i" or "I": case-insensitive
// - "m" or "M": multi-line mode (^ and $ match line boundaries)
// - "s" or "S": single-line mode (. matches newline)
func builtinRegexp(args []Value) Value {
	if len(args) < 2 || len(args) > 3 {
		return NewErrorValue()
	}

	if args[0].IsError() || args[1].IsError() {
		return NewErrorValue()
	}
	if args[0].IsUndefined() || args[1].IsUndefined() {
		return NewUndefinedValue()
	}

	if !args[0].IsString() || !args[1].IsString() {
		return NewErrorValue()
	}

	pattern, _ := args[0].StringValue()
	target, _ := args[1].StringValue()

	// Check for options
	var flags string
	if len(args) == 3 {
		if args[2].IsError() {
			return NewErrorValue()
		}
		if args[2].IsUndefined() {
			return NewUndefinedValue()
		}
		if !args[2].IsString() {
			return NewErrorValue()
		}
		options, _ := args[2].StringValue()

		// Build Go regex flags
		if strings.ContainsAny(options, "iI") {
			flags += "(?i)"
		}
		if strings.ContainsAny(options, "mM") {
			flags += "(?m)"
		}
		if strings.ContainsAny(options, "sS") {
			flags += "(?s)"
		}
	}

	// Compile the regex with flags prepended
	fullPattern := flags + pattern
	re, err := regexp.Compile(fullPattern)
	if err != nil {
		return NewErrorValue()
	}

	return NewBoolValue(re.MatchString(target))
}

// builtinString converts any value to string
func builtinString(args []Value) Value {
	if len(args) != 1 {
		return NewErrorValue()
	}

	if args[0].IsError() {
		return NewErrorValue()
	}
	// string() propagates undefined (matching the reference).
	if args[0].IsUndefined() {
		return NewUndefinedValue()
	}

	// Convert scalars to their reference string form (reals use %.15E, e.g.
	// 1.5 -> "1.500000000000000E+00"). Lists/classads are not handled here.
	if s, ok := classadString(args[0]); ok {
		return NewStringValue(s)
	}

	return NewErrorValue()
}

// builtinBool converts any value to boolean
func builtinBool(args []Value) Value {
	if len(args) != 1 {
		return NewErrorValue()
	}

	if args[0].IsError() {
		return NewErrorValue()
	}
	// bool() propagates undefined (matching the reference).
	if args[0].IsUndefined() {
		return NewUndefinedValue()
	}

	// Already boolean
	if args[0].IsBool() {
		return args[0]
	}

	// Integer: 0 is false, non-zero is true
	if args[0].IsInteger() {
		val, _ := args[0].IntValue()
		return NewBoolValue(val != 0)
	}

	// Real: 0.0 is false, non-zero is true
	if args[0].IsReal() {
		val, _ := args[0].RealValue()
		return NewBoolValue(val != 0.0)
	}

	// String: "true"/"false" (case-insensitive) convert; any other string is
	// undefined (not error), matching the reference.
	if args[0].IsString() {
		str, _ := args[0].StringValue()
		if strings.EqualFold(str, "true") {
			return NewBoolValue(true)
		}
		if strings.EqualFold(str, "false") {
			return NewBoolValue(false)
		}
		return NewUndefinedValue()
	}

	return NewErrorValue()
}

// builtinPow calculates base^exponent
func builtinPow(args []Value) Value {
	if len(args) != 2 {
		return NewErrorValue()
	}

	if args[0].IsError() || args[1].IsError() {
		return NewErrorValue()
	}
	// pow treats undefined operands as an error (matching the reference),
	// not undefined.
	if args[0].IsUndefined() || args[1].IsUndefined() {
		return NewErrorValue()
	}

	// Integer result only when both operands are genuine integers and the
	// exponent is non-negative (booleans take the real path), matching the
	// reference engine, which rounds the integer result via +0.5.
	if args[0].IsInteger() && args[1].IsInteger() {
		base, _ := args[0].IntValue()
		exp, _ := args[1].IntValue()
		if exp >= 0 {
			return NewIntValue(int64(math.Pow(float64(base), float64(exp)) + 0.5))
		}
	}

	// Real path: coerce base and exponent (int/real/bool) to real.
	base, bn := coerceToReal(args[0])
	exp, en := coerceToReal(args[1])
	if !bn || !en {
		return NewErrorValue()
	}
	return NewRealValue(math.Pow(base, exp))
}

// builtinQuantize computes ceiling(a/b)*b for scalars, or finds first value in list >= a
func builtinQuantize(args []Value) Value {
	if len(args) != 2 {
		return NewErrorValue()
	}

	if args[0].IsError() || args[1].IsError() {
		return NewErrorValue()
	}
	// quantize treats undefined operands as an error (matching the reference).
	if args[0].IsUndefined() || args[1].IsUndefined() {
		return NewErrorValue()
	}

	// arg must coerce to a number (bool counts).
	rval, an := coerceToReal(args[0])
	if !an {
		return NewErrorValue()
	}

	// If the base is a list, find the first element >= arg and return that
	// element unchanged; if none, quantize using the last element as the base.
	// An empty list means "do not quantize" and returns arg unchanged.
	if args[1].IsList() {
		list, _ := args[1].ListValue()
		var last Value
		haveLast := false
		for _, item := range list {
			iv, in := coerceToReal(item)
			if !in {
				return NewErrorValue()
			}
			if iv >= rval {
				return item
			}
			last, haveLast = item, true
		}
		if !haveLast {
			return args[0]
		}
		return quantizeScalar(args[0], rval, last)
	}

	return quantizeScalar(args[0], rval, args[1])
}

// quantizeScalar quantizes arg (whose real value is rval) to the next integral
// multiple of base, matching the reference engine (fnCall.cpp doMath2):
//   - a zero base (integer 0 or |real| <= 1e-8) returns arg unchanged,
//     preserving its type;
//   - an integer base with an integer arg yields an integer via ceil division;
//   - otherwise the result is a real.
func quantizeScalar(arg Value, rval float64, base Value) Value {
	rbase, bn := coerceToReal(base)
	if !bn {
		return NewErrorValue()
	}

	if base.IsInteger() {
		ibase, _ := base.IntValue()
		if ibase == 0 {
			return arg
		}
		if arg.IsInteger() {
			ival, _ := arg.IntValue()
			return NewIntValue(((ival + ibase - 1) / ibase) * ibase)
		}
		return NewRealValue(math.Ceil(rval/float64(ibase)) * float64(ibase))
	}

	const epsilon = 1e-8
	if rbase >= -epsilon && rbase <= epsilon {
		return arg
	}
	return NewRealValue(math.Ceil(rval/rbase) * rbase)
}

// builtinSum sums numeric values in a list
func builtinSum(args []Value) Value {
	if len(args) > 1 {
		return NewErrorValue()
	}

	if len(args) == 0 {
		// 0 arguments is wrong arity (the engine rejects it before
		// evaluating args); the reference engine errors here too.
		return NewErrorValue()
	}

	if args[0].IsError() {
		return NewErrorValue()
	}
	if args[0].IsUndefined() {
		return NewUndefinedValue()
	}
	if !args[0].IsList() {
		return NewErrorValue()
	}

	list, _ := args[0].ListValue()
	if len(list) == 0 {
		return NewIntValue(0)
	}

	var sum float64
	hasReal := false
	allUndefined := true

	for _, item := range list {
		if item.IsError() {
			return NewErrorValue()
		}
		if item.IsUndefined() {
			continue
		}

		allUndefined = false

		if item.IsInteger() {
			val, _ := item.IntValue()
			sum += float64(val)
		} else if item.IsReal() {
			val, _ := item.RealValue()
			sum += val
			hasReal = true
		} else {
			return NewErrorValue()
		}
	}

	if allUndefined {
		return NewUndefinedValue()
	}

	if hasReal {
		return NewRealValue(sum)
	}
	return NewIntValue(int64(sum))
}

// builtinAvg computes average of numeric values in a list
func builtinAvg(args []Value) Value {
	if len(args) > 1 {
		return NewErrorValue()
	}

	if len(args) == 0 {
		// 0 arguments is wrong arity (the engine rejects it before
		// evaluating args); the reference engine errors here too.
		return NewErrorValue()
	}

	if args[0].IsError() {
		return NewErrorValue()
	}
	if args[0].IsUndefined() {
		return NewUndefinedValue()
	}
	if !args[0].IsList() {
		return NewErrorValue()
	}

	list, _ := args[0].ListValue()
	if len(list) == 0 {
		return NewRealValue(0.0)
	}

	var sum float64
	count := 0
	allUndefined := true

	for _, item := range list {
		if item.IsError() {
			return NewErrorValue()
		}
		if item.IsUndefined() {
			continue
		}

		allUndefined = false

		if item.IsInteger() {
			val, _ := item.IntValue()
			sum += float64(val)
			count++
		} else if item.IsReal() {
			val, _ := item.RealValue()
			sum += val
			count++
		} else {
			return NewErrorValue()
		}
	}

	if allUndefined {
		return NewUndefinedValue()
	}

	if count == 0 {
		return NewRealValue(0.0)
	}

	return NewRealValue(sum / float64(count))
}

// builtinMin finds minimum value in a list
func builtinMin(args []Value) Value {
	if len(args) > 1 {
		return NewErrorValue()
	}

	if len(args) == 0 {
		// 0 arguments is wrong arity (the engine rejects it before
		// evaluating args); the reference engine errors here too.
		return NewErrorValue()
	}

	if args[0].IsError() {
		return NewErrorValue()
	}
	if args[0].IsUndefined() {
		return NewUndefinedValue()
	}
	if !args[0].IsList() {
		return NewErrorValue()
	}

	list, _ := args[0].ListValue()
	if len(list) == 0 {
		return NewUndefinedValue()
	}

	var minVal float64
	var minItem Value
	hasValue := false
	hasReal := false

	for _, item := range list {
		if item.IsError() {
			return NewErrorValue()
		}
		if item.IsUndefined() {
			continue
		}

		var val float64
		if item.IsInteger() {
			v, _ := item.IntValue()
			val = float64(v)
		} else if item.IsReal() {
			val, _ = item.RealValue()
			hasReal = true
		} else {
			return NewErrorValue()
		}

		if !hasValue || val < minVal {
			minVal = val
			minItem = item
			hasValue = true
		}
	}

	if !hasValue {
		return NewUndefinedValue()
	}

	if hasReal {
		return NewRealValue(minVal)
	}
	return minItem
}

// builtinMax finds maximum value in a list
func builtinMax(args []Value) Value {
	if len(args) > 1 {
		return NewErrorValue()
	}

	if len(args) == 0 {
		// 0 arguments is wrong arity (the engine rejects it before
		// evaluating args); the reference engine errors here too.
		return NewErrorValue()
	}

	if args[0].IsError() {
		return NewErrorValue()
	}
	if args[0].IsUndefined() {
		return NewUndefinedValue()
	}
	if !args[0].IsList() {
		return NewErrorValue()
	}

	list, _ := args[0].ListValue()
	if len(list) == 0 {
		return NewUndefinedValue()
	}

	var maxVal float64
	var maxItem Value
	hasValue := false
	hasReal := false

	for _, item := range list {
		if item.IsError() {
			return NewErrorValue()
		}
		if item.IsUndefined() {
			continue
		}

		var val float64
		if item.IsInteger() {
			v, _ := item.IntValue()
			val = float64(v)
		} else if item.IsReal() {
			val, _ = item.RealValue()
			hasReal = true
		} else {
			return NewErrorValue()
		}

		if !hasValue || val > maxVal {
			maxVal = val
			maxItem = item
			hasValue = true
		}
	}

	if !hasValue {
		return NewUndefinedValue()
	}

	if hasReal {
		return NewRealValue(maxVal)
	}
	return maxItem
}

// builtinJoin joins strings with a separator
// join(sep, arg1, arg2, ...) or join(sep, list) or join(list)
func builtinJoin(args []Value) Value {
	if len(args) == 0 {
		return NewErrorValue()
	}

	// join(list) - no separator
	if len(args) == 1 {
		if args[0].IsError() {
			return NewErrorValue()
		}
		// join propagates undefined (matching the reference).
		if args[0].IsUndefined() {
			return NewUndefinedValue()
		}
		if !args[0].IsList() {
			return NewErrorValue()
		}

		list, _ := args[0].ListValue()
		var result strings.Builder
		for _, item := range list {
			if item.IsUndefined() {
				continue
			}
			if item.IsString() {
				str, _ := item.StringValue()
				result.WriteString(str)
			} else if item.IsInteger() {
				val, _ := item.IntValue()
				result.WriteString(fmt.Sprintf("%d", val))
			} else if item.IsReal() {
				val, _ := item.RealValue()
				result.WriteString(fmt.Sprintf("%g", val))
			} else if item.IsBool() {
				val, _ := item.BoolValue()
				if val {
					result.WriteString("true")
				} else {
					result.WriteString("false")
				}
			}
		}
		return NewStringValue(result.String())
	}

	// Get separator
	if !args[0].IsString() {
		return NewErrorValue()
	}
	sep, _ := args[0].StringValue()

	// Two-argument form: join(separator, list)
	if len(args) == 2 && args[1].IsList() {
		if args[1].IsError() {
			return NewErrorValue()
		}

		list, _ := args[1].ListValue()
		var parts []string
		for _, item := range list {
			if item.IsUndefined() {
				continue
			}
			if item.IsString() {
				str, _ := item.StringValue()
				parts = append(parts, str)
			} else if item.IsInteger() {
				val, _ := item.IntValue()
				parts = append(parts, fmt.Sprintf("%d", val))
			} else if item.IsReal() {
				val, _ := item.RealValue()
				parts = append(parts, fmt.Sprintf("%g", val))
			} else if item.IsBool() {
				val, _ := item.BoolValue()
				if val {
					parts = append(parts, "true")
				} else {
					parts = append(parts, "false")
				}
			}
		}
		return NewStringValue(strings.Join(parts, sep))
	}

	// join(sep, arg1, arg2, ...)
	var parts []string
	for i := 1; i < len(args); i++ {
		if args[i].IsError() {
			return NewErrorValue()
		}
		if args[i].IsUndefined() {
			continue
		}

		if args[i].IsString() {
			str, _ := args[i].StringValue()
			parts = append(parts, str)
		} else if args[i].IsInteger() {
			val, _ := args[i].IntValue()
			parts = append(parts, fmt.Sprintf("%d", val))
		} else if args[i].IsReal() {
			val, _ := args[i].RealValue()
			parts = append(parts, fmt.Sprintf("%g", val))
		} else if args[i].IsBool() {
			val, _ := args[i].BoolValue()
			if val {
				parts = append(parts, "true")
			} else {
				parts = append(parts, "false")
			}
		}
	}

	return NewStringValue(strings.Join(parts, sep))
}

// builtinSplit splits a string into a list
func builtinSplit(args []Value) Value {
	if len(args) < 1 || len(args) > 2 {
		return NewErrorValue()
	}

	if args[0].IsError() {
		return NewErrorValue()
	}
	// split of a non-string (including undefined) is an error in the reference.
	if !args[0].IsString() {
		return NewErrorValue()
	}

	str, _ := args[0].StringValue()

	// Default delimiter is whitespace
	if len(args) == 1 {
		fields := strings.Fields(str)
		var result []Value
		for _, field := range fields {
			result = append(result, NewStringValue(field))
		}
		return NewListValue(result)
	}

	// Custom delimiter
	if !args[1].IsString() {
		return NewErrorValue()
	}
	delim, _ := args[1].StringValue()

	// Split on any character in delimiter string
	fields := strings.FieldsFunc(str, func(r rune) bool {
		return strings.ContainsRune(delim, r)
	})

	var result []Value
	for _, field := range fields {
		result = append(result, NewStringValue(field))
	}
	return NewListValue(result)
}

// builtinSplitUserName splits "user@domain" into {"user", "domain"}
func builtinSplitUserName(args []Value) Value {
	if len(args) != 1 {
		return NewErrorValue()
	}

	if args[0].IsError() {
		return NewErrorValue()
	}
	// undefined is not propagated: a non-string argument (including undefined)
	// is an error, matching the reference engine.
	if !args[0].IsString() {
		return NewErrorValue()
	}

	name, _ := args[0].StringValue()
	parts := strings.SplitN(name, "@", 2)

	if len(parts) == 2 {
		return NewListValue([]Value{
			NewStringValue(parts[0]),
			NewStringValue(parts[1]),
		})
	}

	return NewListValue([]Value{
		NewStringValue(name),
		NewStringValue(""),
	})
}

// builtinSplitSlotName splits "slot1@machine" into {"slot1", "machine"}
// If no @, returns {"", "name"}
func builtinSplitSlotName(args []Value) Value {
	if len(args) != 1 {
		return NewErrorValue()
	}

	if args[0].IsError() {
		return NewErrorValue()
	}
	// undefined is not propagated: a non-string argument (including undefined)
	// is an error, matching the reference engine.
	if !args[0].IsString() {
		return NewErrorValue()
	}

	name, _ := args[0].StringValue()
	parts := strings.SplitN(name, "@", 2)

	if len(parts) == 2 {
		return NewListValue([]Value{
			NewStringValue(parts[0]),
			NewStringValue(parts[1]),
		})
	}

	return NewListValue([]Value{
		NewStringValue(""),
		NewStringValue(name),
	})
}

// builtinStrcmp compares strings (case-sensitive)
func builtinStrcmp(args []Value) Value {
	if len(args) != 2 {
		return NewErrorValue()
	}

	// An undefined argument dominates over an error one (matching the
	// reference): check undefined first, then error.
	if args[0].IsUndefined() || args[1].IsUndefined() {
		return NewUndefinedValue()
	}
	if args[0].IsError() || args[1].IsError() {
		return NewErrorValue()
	}

	// Coerce both arguments to their string form (matching string()/strcat()).
	str1, ok1 := classadString(args[0])
	str2, ok2 := classadString(args[1])
	if !ok1 || !ok2 {
		return NewErrorValue()
	}

	result := strings.Compare(str1, str2)
	return NewIntValue(int64(result))
}

// builtinStricmp compares strings (case-insensitive)
func builtinStricmp(args []Value) Value {
	if len(args) != 2 {
		return NewErrorValue()
	}

	// An undefined argument dominates over an error one (matching the
	// reference): check undefined first, then error.
	if args[0].IsUndefined() || args[1].IsUndefined() {
		return NewUndefinedValue()
	}
	if args[0].IsError() || args[1].IsError() {
		return NewErrorValue()
	}

	// Coerce both arguments to their string form (matching string()/strcat()).
	str1, ok1 := classadString(args[0])
	str2, ok2 := classadString(args[1])
	if !ok1 || !ok2 {
		return NewErrorValue()
	}

	result := strings.Compare(strings.ToLower(str1), strings.ToLower(str2))
	return NewIntValue(int64(result))
}

// versionCompare implements HTCondor version comparison logic
// Lexicographic except numeric sequences are compared numerically
func versionCompare(left, right string) int {
	i, j := 0, 0

	for i < len(left) && j < len(right) {
		// Check if both are at the start of a numeric sequence
		if left[i] >= '0' && left[i] <= '9' && right[j] >= '0' && right[j] <= '9' {
			// Count leading zeros
			zeros1, zeros2 := 0, 0
			for i < len(left) && left[i] == '0' {
				zeros1++
				i++
			}
			for j < len(right) && right[j] == '0' {
				zeros2++
				j++
			}

			// Extract remaining digits
			numEnd1, numEnd2 := i, j
			for numEnd1 < len(left) && left[numEnd1] >= '0' && left[numEnd1] <= '9' {
				numEnd1++
			}
			for numEnd2 < len(right) && right[numEnd2] >= '0' && right[numEnd2] <= '9' {
				numEnd2++
			}

			// Compare numeric values
			if i < numEnd1 || j < numEnd2 {
				// At least one has non-zero digits
				num1Len := numEnd1 - i
				num2Len := numEnd2 - j

				if num1Len != num2Len {
					return num1Len - num2Len
				}

				// Same length, compare digit by digit
				for k := 0; k < num1Len; k++ {
					if left[i+k] != right[j+k] {
						return int(left[i+k]) - int(right[j+k])
					}
				}

				i = numEnd1
				j = numEnd2
			} else {
				// Both are all zeros - more zeros means smaller
				if zeros1 != zeros2 {
					return zeros2 - zeros1
				}
				i = numEnd1
				j = numEnd2
			}
		} else {
			// Lexicographic comparison
			if left[i] != right[j] {
				return int(left[i]) - int(right[j])
			}
			i++
			j++
		}
	}

	// One string is a prefix of the other
	return len(left) - len(right)
}

// builtinVersioncmp compares version strings
func builtinVersioncmp(args []Value) Value {
	if len(args) != 2 {
		return NewErrorValue()
	}

	if args[0].IsError() || args[1].IsError() {
		return NewErrorValue()
	}
	if args[0].IsUndefined() || args[1].IsUndefined() {
		return NewUndefinedValue()
	}
	if !args[0].IsString() || !args[1].IsString() {
		return NewErrorValue()
	}

	left, _ := args[0].StringValue()
	right, _ := args[1].StringValue()

	result := versionCompare(left, right)
	return NewIntValue(int64(result))
}

// builtinVersionGT checks if left > right
func builtinVersionGT(args []Value) Value {
	result := builtinVersioncmp(args)
	if result.IsError() || result.IsUndefined() {
		return result
	}
	val, _ := result.IntValue()
	return NewBoolValue(val > 0)
}

// builtinVersionGE checks if left >= right
func builtinVersionGE(args []Value) Value {
	result := builtinVersioncmp(args)
	if result.IsError() || result.IsUndefined() {
		return result
	}
	val, _ := result.IntValue()
	return NewBoolValue(val >= 0)
}

// builtinVersionLT checks if left < right
func builtinVersionLT(args []Value) Value {
	result := builtinVersioncmp(args)
	if result.IsError() || result.IsUndefined() {
		return result
	}
	val, _ := result.IntValue()
	return NewBoolValue(val < 0)
}

// builtinVersionLE checks if left <= right
func builtinVersionLE(args []Value) Value {
	result := builtinVersioncmp(args)
	if result.IsError() || result.IsUndefined() {
		return result
	}
	val, _ := result.IntValue()
	return NewBoolValue(val <= 0)
}

// builtinVersionEQ checks if left == right
func builtinVersionEQ(args []Value) Value {
	result := builtinVersioncmp(args)
	if result.IsError() || result.IsUndefined() {
		return result
	}
	val, _ := result.IntValue()
	return NewBoolValue(val == 0)
}

// builtinVersionInRange checks if min <= version <= max
func builtinVersionInRange(args []Value) Value {
	if len(args) != 3 {
		return NewErrorValue()
	}

	// Check version >= min
	minCheck := builtinVersionGE([]Value{args[0], args[1]})
	if minCheck.IsError() || minCheck.IsUndefined() {
		return minCheck
	}
	minOk, _ := minCheck.BoolValue()

	if !minOk {
		return NewBoolValue(false)
	}

	// Check version <= max
	maxCheck := builtinVersionLE([]Value{args[0], args[2]})
	if maxCheck.IsError() || maxCheck.IsUndefined() {
		return maxCheck
	}
	maxOk, _ := maxCheck.BoolValue()

	return NewBoolValue(maxOk)
}

// builtinFormatTime formats a Unix timestamp
func builtinFormatTime(args []Value) Value {
	if len(args) > 2 {
		return NewErrorValue()
	}

	// Get time (default to current time)
	var t time.Time
	if len(args) >= 1 && !args[0].IsUndefined() {
		if args[0].IsError() {
			return NewErrorValue()
		}
		if !args[0].IsInteger() {
			return NewErrorValue()
		}
		timestamp, _ := args[0].IntValue()
		t = time.Unix(timestamp, 0).UTC()
	} else {
		t = time.Now().UTC()
	}

	// Get format string (default to "%c")
	format := "%c"
	if len(args) >= 2 {
		if args[1].IsError() {
			return NewErrorValue()
		}
		if args[1].IsUndefined() {
			return NewErrorValue()
		}
		if !args[1].IsString() {
			return NewErrorValue()
		}
		format, _ = args[1].StringValue()
	}

	// Convert strftime format to Go format
	result := convertStrftimeToGo(t, format)
	return NewStringValue(result)
}

// convertStrftimeToGo converts strftime format codes to Go time format
func convertStrftimeToGo(t time.Time, format string) string {
	var result strings.Builder
	i := 0

	for i < len(format) {
		if format[i] == '%' && i+1 < len(format) {
			switch format[i+1] {
			case '%':
				result.WriteByte('%')
			case 'a':
				result.WriteString(t.Format("Mon"))
			case 'A':
				result.WriteString(t.Format("Monday"))
			case 'b':
				result.WriteString(t.Format("Jan"))
			case 'B':
				result.WriteString(t.Format("January"))
			case 'c':
				result.WriteString(t.Format("Mon Jan 2 15:04:05 2006"))
			case 'd':
				result.WriteString(t.Format("02"))
			case 'H':
				result.WriteString(t.Format("15"))
			case 'I':
				result.WriteString(t.Format("03"))
			case 'j':
				result.WriteString(fmt.Sprintf("%03d", t.YearDay()))
			case 'm':
				result.WriteString(t.Format("01"))
			case 'M':
				result.WriteString(t.Format("04"))
			case 'p':
				result.WriteString(t.Format("PM"))
			case 'S':
				result.WriteString(t.Format("05"))
			case 'U', 'W':
				// Week number - simplified
				_, week := t.ISOWeek()
				result.WriteString(fmt.Sprintf("%02d", week))
			case 'w':
				result.WriteString(fmt.Sprintf("%d", t.Weekday()))
			case 'x':
				result.WriteString(t.Format("01/02/06"))
			case 'X':
				result.WriteString(t.Format("15:04:05"))
			case 'y':
				result.WriteString(t.Format("06"))
			case 'Y':
				result.WriteString(t.Format("2006"))
			case 'Z':
				result.WriteString(t.Format("MST"))
			default:
				result.WriteByte('%')
				result.WriteByte(format[i+1])
			}
			i += 2
		} else {
			result.WriteByte(format[i])
			i++
		}
	}

	return result.String()
}

// builtinInterval formats seconds as "days+hh:mm:ss"
func builtinInterval(args []Value) Value {
	if len(args) != 1 {
		return NewErrorValue()
	}

	if args[0].IsError() {
		return NewErrorValue()
	}
	if args[0].IsUndefined() {
		return NewUndefinedValue()
	}
	if !args[0].IsInteger() {
		return NewErrorValue()
	}

	seconds, _ := args[0].IntValue()

	days := seconds / 86400
	seconds %= 86400
	hours := seconds / 3600
	seconds %= 3600
	minutes := seconds / 60
	seconds %= 60

	if days > 0 {
		return NewStringValue(fmt.Sprintf("%d+%02d:%02d:%02d", days, hours, minutes, seconds))
	}
	if hours > 0 {
		return NewStringValue(fmt.Sprintf("%d:%02d:%02d", hours, minutes, seconds))
	}
	if minutes > 0 {
		return NewStringValue(fmt.Sprintf("%d:%02d", minutes, seconds))
	}
	return NewStringValue(fmt.Sprintf("0:%02d", seconds))
}

// builtinIdenticalMember checks if m is in list using =?= (strict identity)
func builtinIdenticalMember(args []Value) Value {
	if len(args) != 2 {
		return NewErrorValue()
	}

	if args[0].IsError() || args[1].IsError() {
		return NewErrorValue()
	}

	// First arg must be scalar
	if args[0].IsList() || args[0].IsClassAd() {
		return NewErrorValue()
	}

	// Second arg must be list
	if !args[1].IsList() {
		return NewErrorValue()
	}

	list, _ := args[1].ListValue()

	for _, item := range list {
		// Strict identity check - same type and value
		if args[0].valueType != item.valueType {
			continue
		}

		switch args[0].valueType {
		case IntegerValue:
			v1, _ := args[0].IntValue()
			v2, _ := item.IntValue()
			if v1 == v2 {
				return NewBoolValue(true)
			}
		case RealValue:
			v1, _ := args[0].RealValue()
			v2, _ := item.RealValue()
			if v1 == v2 {
				return NewBoolValue(true)
			}
		case StringValue:
			v1, _ := args[0].StringValue()
			v2, _ := item.StringValue()
			if v1 == v2 {
				return NewBoolValue(true)
			}
		case BooleanValue:
			v1, _ := args[0].BoolValue()
			v2, _ := item.BoolValue()
			if v1 == v2 {
				return NewBoolValue(true)
			}
		case UndefinedValue:
			return NewBoolValue(true)
		}
	}

	return NewBoolValue(false)
}

// compareList implements anyCompare/allCompare: it applies the comparison
// operator op (arg0) between each element of the list (arg1) and the target
// (arg2). An error in op/list/target, a non-string op, a non-list second
// argument, or an unrecognized operator is an error; an undefined op or list is
// undefined; but the target may be undefined (a comparison against it just
// yields undefined). Each element comparison uses the engine's three-valued
// semantics: a comparison that errors makes the whole call error; for
// anyCompare a true element wins (else false), for allCompare a non-true
// element (false or undefined) loses (else true, vacuously true for []).
func compareList(args []Value, all bool) Value {
	if len(args) != 3 {
		return NewErrorValue()
	}
	if args[0].IsError() || args[1].IsError() || args[2].IsError() {
		return NewErrorValue()
	}
	if args[0].IsUndefined() || args[1].IsUndefined() {
		return NewUndefinedValue()
	}
	if !args[0].IsString() || !args[1].IsList() {
		return NewErrorValue()
	}
	op, _ := args[0].StringValue()
	if !validCompareOp(op) {
		return NewErrorValue()
	}
	list, _ := args[1].ListValue()
	target := args[2]

	for _, item := range list {
		r := compareValues(op, item, target)
		if r.IsError() {
			return NewErrorValue()
		}
		isTrue := false
		if r.IsBool() {
			isTrue, _ = r.BoolValue()
		}
		if all {
			if !isTrue {
				return NewBoolValue(false)
			}
		} else if isTrue {
			return NewBoolValue(true)
		}
	}
	return NewBoolValue(all)
}

// builtinAnyCompare checks if any element in list satisfies the comparison.
// anyCompare(op, list, target), op one of < <= == != >= > is isnt =?= =!=
func builtinAnyCompare(args []Value) Value {
	return compareList(args, false)
}

// builtinAllCompare checks if all elements in list satisfy the comparison.
func builtinAllCompare(args []Value) Value {
	return compareList(args, true)
}

// compareValues performs comparison based on operator string
// validCompareOp reports whether op is a comparison operator accepted by
// anyCompare/allCompare. An unrecognized operator makes those functions error.
func validCompareOp(op string) bool {
	switch op {
	case "<", "<=", ">", ">=", "==", "!=", "is", "isnt", "=?=", "=!=":
		return true
	}
	return false
}

// compareValues applies a comparison operator to two values using the engine's
// real (three-valued) comparison semantics, so anyCompare/allCompare see the
// same undefined/error/case-insensitive behavior as the corresponding operator
// (e.g. 1 == undefined is undefined, not error). An unrecognized operator is an
// error.
func compareValues(op string, left, right Value) Value {
	e := &Evaluator{}
	switch op {
	case "<":
		return e.evaluateLessThan(left, right)
	case "<=":
		return e.evaluateLessOrEqual(left, right)
	case ">":
		return e.evaluateGreaterThan(left, right)
	case ">=":
		return e.evaluateGreaterOrEqual(left, right)
	case "==":
		return e.evaluateEqual(left, right)
	case "!=":
		return e.evaluateNotEqual(left, right)
	case "is", "=?=":
		return e.evaluateIs(left, right)
	case "isnt", "=!=":
		return e.evaluateIsnt(left, right)
	default:
		return NewErrorValue()
	}
}

// parseStringList splits a string list by delimiter (default comma)
func parseStringList(listStr, delimiter string) []string {
	if delimiter == "" {
		delimiter = ","
	}

	parts := strings.Split(listStr, delimiter)
	var result []string
	for _, part := range parts {
		result = append(result, strings.TrimSpace(part))
	}
	return result
}

// builtinStringListSize returns the number of elements in a string list
// stringListArgs validates the (list [, delimiter]) arguments shared by the
// stringList* functions and returns the list string and delimiter. The
// reference engine requires string arguments and errors on anything else --
// including undefined (these HTCondor functions do not propagate undefined) --
// so a non-string argument yields a non-nil error Value to return.
func stringListArgs(args []Value) (listStr, delim string, bad *Value) {
	e := NewErrorValue()
	if !args[0].IsString() {
		return "", "", &e
	}
	listStr, _ = args[0].StringValue()
	delim = ","
	if len(args) == 2 {
		if !args[1].IsString() {
			return "", "", &e
		}
		delim, _ = args[1].StringValue()
	}
	return listStr, delim, nil
}

// parseNumericStringList parses the non-empty items of a delimited list as
// numbers for the stringListSum/Avg/Min/Max family. A plain decimal integer
// item stays an integer; anything else (a real, or a hex/inf form strtod
// accepts) is parsed via strtod and marks the whole result real (hasReal). A
// non-numeric item makes ok=false, which the reference treats as error.
func parseNumericStringList(listStr, delim string) (vals []float64, hasReal, ok bool) {
	for _, part := range parseStringList(listStr, delim) {
		p := strings.TrimSpace(part)
		if p == "" {
			continue
		}
		if iv, err := strconv.ParseInt(p, 10, 64); err == nil {
			vals = append(vals, float64(iv))
			continue
		}
		f, fok := parseLeadingFloat(p)
		if !fok {
			return nil, false, false
		}
		hasReal = true
		vals = append(vals, f)
	}
	return vals, hasReal, true
}

func builtinStringListSize(args []Value) Value {
	if len(args) < 1 || len(args) > 2 {
		return NewErrorValue()
	}
	listStr, delim, bad := stringListArgs(args)
	if bad != nil {
		return *bad
	}
	// Don't count empty strings.
	count := 0
	for _, part := range parseStringList(listStr, delim) {
		if part != "" {
			count++
		}
	}
	return NewIntValue(int64(count))
}

// builtinStringListSum sums numeric values in a string list
func builtinStringListSum(args []Value) Value {
	if len(args) < 1 || len(args) > 2 {
		return NewErrorValue()
	}

	listStr, delim, bad := stringListArgs(args)
	if bad != nil {
		return *bad
	}
	vals, hasReal, ok := parseNumericStringList(listStr, delim)
	if !ok {
		return NewErrorValue()
	}
	if len(vals) == 0 {
		return NewRealValue(0) // empty sum is real 0.0 in the reference
	}
	var sum float64
	for _, v := range vals {
		sum += v
	}
	if hasReal {
		return NewRealValue(sum)
	}
	return NewIntValue(int64(sum))
}

// builtinStringListAvg computes average of numeric values in a string list
func builtinStringListAvg(args []Value) Value {
	if len(args) < 1 || len(args) > 2 {
		return NewErrorValue()
	}

	listStr, delim, bad := stringListArgs(args)
	if bad != nil {
		return *bad
	}
	vals, hasReal, ok := parseNumericStringList(listStr, delim)
	if !ok {
		return NewErrorValue()
	}
	if len(vals) == 0 {
		return NewRealValue(0) // empty average is real 0.0 in the reference
	}
	var sum float64
	for _, v := range vals {
		sum += v
	}
	// All-integer items use integer division (avg("1,2") is 1, not 1.5),
	// matching the reference; a real item makes the average real.
	if hasReal {
		return NewRealValue(sum / float64(len(vals)))
	}
	return NewIntValue(int64(sum) / int64(len(vals)))
}

// builtinStringListMin finds minimum numeric value in a string list
func builtinStringListMin(args []Value) Value {
	if len(args) < 1 || len(args) > 2 {
		return NewErrorValue()
	}

	listStr, delim, bad := stringListArgs(args)
	if bad != nil {
		return *bad
	}
	vals, hasReal, ok := parseNumericStringList(listStr, delim)
	if !ok {
		return NewErrorValue()
	}
	if len(vals) == 0 {
		return NewUndefinedValue() // empty list has no minimum
	}
	minVal := vals[0]
	for _, v := range vals[1:] {
		if v < minVal {
			minVal = v
		}
	}
	if hasReal {
		return NewRealValue(minVal)
	}
	return NewIntValue(int64(minVal))
}

// builtinStringListMax finds maximum numeric value in a string list
func builtinStringListMax(args []Value) Value {
	if len(args) < 1 || len(args) > 2 {
		return NewErrorValue()
	}

	listStr, delim, bad := stringListArgs(args)
	if bad != nil {
		return *bad
	}
	vals, hasReal, ok := parseNumericStringList(listStr, delim)
	if !ok {
		return NewErrorValue()
	}
	if len(vals) == 0 {
		return NewUndefinedValue() // empty list has no maximum
	}
	maxVal := vals[0]
	for _, v := range vals[1:] {
		if v > maxVal {
			maxVal = v
		}
	}
	if hasReal {
		return NewRealValue(maxVal)
	}
	return NewIntValue(int64(maxVal))
}

// builtinStringListsIntersect checks if two string lists have common elements
func builtinStringListsIntersect(args []Value) Value {
	if len(args) < 2 || len(args) > 3 {
		return NewErrorValue()
	}

	if args[0].IsError() || args[1].IsError() {
		return NewErrorValue()
	}
	if args[0].IsUndefined() || args[1].IsUndefined() {
		return NewUndefinedValue()
	}
	if !args[0].IsString() || !args[1].IsString() {
		return NewErrorValue()
	}

	list1Str, _ := args[0].StringValue()
	list2Str, _ := args[1].StringValue()
	delimiter := ","

	if len(args) == 3 {
		if args[2].IsError() {
			return NewErrorValue()
		}
		if args[2].IsUndefined() {
			return NewUndefinedValue()
		}
		if !args[2].IsString() {
			return NewErrorValue()
		}
		delimiter, _ = args[2].StringValue()
	}

	list1 := parseStringList(list1Str, delimiter)
	list2 := parseStringList(list2Str, delimiter)

	// Create a set from list2 for fast lookup
	set := make(map[string]bool)
	for _, item := range list2 {
		if item != "" {
			set[item] = true
		}
	}

	// Check if any item from list1 is in the set
	for _, item := range list1 {
		if item != "" && set[item] {
			return NewBoolValue(true)
		}
	}

	return NewBoolValue(false)
}

// builtinStringListSubsetMatch checks if list1 is a subset of list2
func builtinStringListSubsetMatch(args []Value) Value {
	if len(args) < 2 || len(args) > 3 {
		return NewErrorValue()
	}

	// undefined is treated as the empty list (so subsetMatch(undefined, "a") is
	// true -- the empty list is a subset of anything), error is an error.
	list1Str, ok0 := stringListStrArg(args[0])
	list2Str, ok1 := stringListStrArg(args[1])
	if !ok0 || !ok1 {
		return NewErrorValue()
	}
	delimiter := ","

	if len(args) == 3 {
		d, ok2 := stringListStrArg(args[2])
		if !ok2 {
			return NewErrorValue()
		}
		delimiter = d
	}

	list1 := parseStringList(list1Str, delimiter)
	list2 := parseStringList(list2Str, delimiter)

	// Create a set from list2
	set := make(map[string]bool)
	for _, item := range list2 {
		if item != "" {
			set[item] = true
		}
	}

	// Check if all items from list1 are in list2
	for _, item := range list1 {
		if item != "" && !set[item] {
			return NewBoolValue(false)
		}
	}

	return NewBoolValue(true)
}

// builtinStringListRegexpMember checks if pattern matches any element in string list
func builtinStringListRegexpMember(args []Value) Value {
	if len(args) < 2 || len(args) > 4 {
		return NewErrorValue()
	}

	if args[0].IsError() || args[1].IsError() {
		return NewErrorValue()
	}
	if args[0].IsUndefined() || args[1].IsUndefined() {
		return NewUndefinedValue()
	}
	if !args[0].IsString() || !args[1].IsString() {
		return NewErrorValue()
	}

	pattern, _ := args[0].StringValue()
	listStr, _ := args[1].StringValue()
	delimiter := ","
	options := ""

	// Parse optional arguments
	if len(args) >= 3 {
		if args[2].IsError() {
			return NewErrorValue()
		}
		if !args[2].IsUndefined() {
			if !args[2].IsString() {
				return NewErrorValue()
			}
			delimiter, _ = args[2].StringValue()
		}
	}

	if len(args) == 4 {
		if args[3].IsError() {
			return NewErrorValue()
		}
		if !args[3].IsUndefined() {
			if !args[3].IsString() {
				return NewErrorValue()
			}
			options, _ = args[3].StringValue()
		}
	}

	// Build regex flags
	var flags string
	if strings.ContainsAny(options, "iI") {
		flags += "(?i)"
	}
	if strings.ContainsAny(options, "mM") {
		flags += "(?m)"
	}
	if strings.ContainsAny(options, "sS") {
		flags += "(?s)"
	}

	fullPattern := flags + pattern
	re, err := regexp.Compile(fullPattern)
	if err != nil {
		return NewErrorValue()
	}

	parts := parseStringList(listStr, delimiter)
	for _, part := range parts {
		if part != "" && re.MatchString(part) {
			return NewBoolValue(true)
		}
	}

	return NewBoolValue(false)
}

// builtinRegexpMember checks if pattern matches any string in a list
func builtinRegexpMember(args []Value) Value {
	if len(args) < 2 || len(args) > 3 {
		return NewErrorValue()
	}

	if args[0].IsError() || args[1].IsError() {
		return NewErrorValue()
	}
	if args[0].IsUndefined() || args[1].IsUndefined() {
		return NewUndefinedValue()
	}
	if !args[0].IsString() || !args[1].IsList() {
		return NewErrorValue()
	}

	pattern, _ := args[0].StringValue()
	list, _ := args[1].ListValue()
	options := ""

	if len(args) == 3 {
		if args[2].IsError() {
			return NewErrorValue()
		}
		if !args[2].IsUndefined() {
			if !args[2].IsString() {
				return NewErrorValue()
			}
			options, _ = args[2].StringValue()
		}
	}

	// Build regex flags
	var flags string
	if strings.ContainsAny(options, "iI") {
		flags += "(?i)"
	}
	if strings.ContainsAny(options, "mM") {
		flags += "(?m)"
	}
	if strings.ContainsAny(options, "sS") {
		flags += "(?s)"
	}

	fullPattern := flags + pattern
	re, err := regexp.Compile(fullPattern)
	if err != nil {
		return NewErrorValue()
	}

	for _, item := range list {
		if item.IsString() {
			str, _ := item.StringValue()
			if re.MatchString(str) {
				return NewBoolValue(true)
			}
		}
	}

	return NewBoolValue(false)
}

// builtinRegexps performs regex substitution and returns the result
func builtinRegexps(args []Value) Value {
	if len(args) < 3 || len(args) > 4 {
		return NewErrorValue()
	}

	if args[0].IsError() || args[1].IsError() || args[2].IsError() {
		return NewErrorValue()
	}
	if args[0].IsUndefined() || args[1].IsUndefined() || args[2].IsUndefined() {
		return NewUndefinedValue()
	}
	if !args[0].IsString() || !args[1].IsString() || !args[2].IsString() {
		return NewErrorValue()
	}

	pattern, _ := args[0].StringValue()
	target, _ := args[1].StringValue()
	substitute, _ := args[2].StringValue()
	options := ""

	if len(args) == 4 {
		if args[3].IsError() {
			return NewErrorValue()
		}
		if !args[3].IsUndefined() {
			if !args[3].IsString() {
				return NewErrorValue()
			}
			options, _ = args[3].StringValue()
		}
	}

	// Build regex flags
	var flags string
	if strings.ContainsAny(options, "iI") {
		flags += "(?i)"
	}
	if strings.ContainsAny(options, "mM") {
		flags += "(?m)"
	}
	if strings.ContainsAny(options, "sS") {
		flags += "(?s)"
	}

	fullPattern := flags + pattern
	re, err := regexp.Compile(fullPattern)
	if err != nil {
		return NewErrorValue()
	}

	result := re.ReplaceAllString(target, substitute)
	return NewStringValue(result)
}

// builtinReplace replaces first match of pattern in target
func builtinReplace(args []Value) Value {
	if len(args) < 3 || len(args) > 4 {
		return NewErrorValue()
	}

	if args[0].IsError() || args[1].IsError() || args[2].IsError() {
		return NewErrorValue()
	}
	if args[0].IsUndefined() || args[1].IsUndefined() || args[2].IsUndefined() {
		return NewUndefinedValue()
	}
	if !args[0].IsString() || !args[1].IsString() || !args[2].IsString() {
		return NewErrorValue()
	}

	pattern, _ := args[0].StringValue()
	target, _ := args[1].StringValue()
	substitute, _ := args[2].StringValue()
	options := ""

	if len(args) == 4 {
		if args[3].IsError() {
			return NewErrorValue()
		}
		if !args[3].IsUndefined() {
			if !args[3].IsString() {
				return NewErrorValue()
			}
			options, _ = args[3].StringValue()
		}
	}

	// Build regex flags
	var flags string
	if strings.ContainsAny(options, "iI") {
		flags += "(?i)"
	}
	if strings.ContainsAny(options, "mM") {
		flags += "(?m)"
	}
	if strings.ContainsAny(options, "sS") {
		flags += "(?s)"
	}

	fullPattern := flags + pattern
	re, err := regexp.Compile(fullPattern)
	if err != nil {
		return NewErrorValue()
	}

	// Replace only first occurrence
	loc := re.FindStringIndex(target)
	if loc != nil {
		result := target[:loc[0]] + substitute + target[loc[1]:]
		return NewStringValue(result)
	}

	return NewStringValue(target)
}

// builtinReplaceAll replaces all matches of pattern in target
func builtinReplaceAll(args []Value) Value {
	// Same as regexps - replaces all occurrences
	return builtinRegexps(args)
}
