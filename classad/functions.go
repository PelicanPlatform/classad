// Package classad provides ClassAd matching functionality.
package classad

import (
	"fmt"
	"math"
	"math/rand"
	"regexp"
	"strings"
	"time"
)

// Built-in string functions

// builtinStrcat concatenates strings
func builtinStrcat(args []Value) Value {
	var result strings.Builder
	for _, arg := range args {
		if arg.IsError() {
			return NewErrorValue()
		}
		if arg.IsUndefined() {
			return NewUndefinedValue()
		}
		if !arg.IsString() {
			return NewErrorValue()
		}
		str, _ := arg.StringValue()
		result.WriteString(str)
	}
	return NewStringValue(result.String())
}

// builtinSubstr extracts substring(string, offset[, length])
func builtinSubstr(args []Value) Value {
	if len(args) < 2 || len(args) > 3 {
		return NewErrorValue()
	}

	// Check for error or undefined
	for _, arg := range args {
		if arg.IsError() {
			return NewErrorValue()
		}
		if arg.IsUndefined() {
			return NewUndefinedValue()
		}
	}

	if !args[0].IsString() || !args[1].IsInteger() {
		return NewErrorValue()
	}

	str, _ := args[0].StringValue()
	offset, _ := args[1].IntValue()

	// Handle negative offset (from end)
	if offset < 0 {
		offset = int64(len(str)) + offset
	}

	if offset < 0 || offset >= int64(len(str)) {
		return NewStringValue("")
	}

	if len(args) == 3 {
		if !args[2].IsInteger() {
			return NewErrorValue()
		}
		length, _ := args[2].IntValue()
		if length < 0 {
			return NewErrorValue()
		}
		end := offset + length
		if end > int64(len(str)) {
			end = int64(len(str))
		}
		return NewStringValue(str[offset:end])
	}

	return NewStringValue(str[offset:])
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
		list, _ := args[0].ListValue()
		return NewIntValue(int64(len(list)))
	}

	return NewErrorValue()
}

// builtinLength is an alias for size
func builtinLength(args []Value) Value {
	return builtinSize(args)
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
	if !args[0].IsString() {
		return NewErrorValue()
	}

	str, _ := args[0].StringValue()
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
	if !args[0].IsString() {
		return NewErrorValue()
	}

	str, _ := args[0].StringValue()
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
	if args[0].IsUndefined() {
		return NewUndefinedValue()
	}
	if !args[0].IsNumber() {
		return NewErrorValue()
	}

	num, _ := args[0].NumberValue()
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
	if args[0].IsUndefined() {
		return NewUndefinedValue()
	}
	if !args[0].IsNumber() {
		return NewErrorValue()
	}

	num, _ := args[0].NumberValue()
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
	if args[0].IsUndefined() {
		return NewUndefinedValue()
	}
	if !args[0].IsNumber() {
		return NewErrorValue()
	}

	num, _ := args[0].NumberValue()
	return NewIntValue(int64(math.Round(num)))
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
		return NewIntValue(int64(num))
	}

	if args[0].IsBool() {
		b, _ := args[0].BoolValue()
		if b {
			return NewIntValue(1)
		}
		return NewIntValue(0)
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

	if args[0].IsError() || args[1].IsError() {
		return NewErrorValue()
	}
	if args[0].IsUndefined() || args[1].IsUndefined() {
		return NewUndefinedValue()
	}

	if !args[1].IsList() {
		return NewErrorValue()
	}

	element := args[0]
	list, _ := args[1].ListValue()

	for _, item := range list {
		// Use simple equality check
		if element.Type() == item.Type() {
			if element.IsInteger() {
				e, _ := element.IntValue()
				i, _ := item.IntValue()
				if e == i {
					return NewBoolValue(true)
				}
			} else if element.IsReal() {
				e, _ := element.RealValue()
				i, _ := item.RealValue()
				if e == i {
					return NewBoolValue(true)
				}
			} else if element.IsString() {
				e, _ := element.StringValue()
				i, _ := item.StringValue()
				if e == i {
					return NewBoolValue(true)
				}
			} else if element.IsBool() {
				e, _ := element.BoolValue()
				i, _ := item.BoolValue()
				if e == i {
					return NewBoolValue(true)
				}
			}
		}
	}

	return NewBoolValue(false)
}

// builtinStringListMember checks if a string is a member of a comma-separated string list
// stringListMember(string item, string list [, string options])
// The list is a comma-separated string. Options can contain:
// - "i" or "I": case-insensitive comparison
func builtinStringListMember(args []Value) Value {
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

	item, _ := args[0].StringValue()
	listStr, _ := args[1].StringValue()

	// Check for options
	ignoreCase := false
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

// builtinIfThenElse is the conditional operator as a function
// ifThenElse(condition, trueValue, falseValue)
// This is equivalent to (condition ? trueValue : falseValue)
// Unlike the ternary operator, this is a function so all arguments are evaluated first
func builtinIfThenElse(args []Value) Value {
	if len(args) != 3 {
		return NewErrorValue()
	}

	// Check first argument for error/undefined
	if args[0].IsError() {
		return NewErrorValue()
	}
	if args[0].IsUndefined() {
		return NewUndefinedValue()
	}

	if !args[0].IsBool() {
		return NewErrorValue()
	}

	condition, _ := args[0].BoolValue()

	// Return appropriate value based on condition
	if condition {
		return args[1]
	}
	return args[2]
}

// builtinString converts any value to string
func builtinString(args []Value) Value {
	if len(args) != 1 {
		return NewErrorValue()
	}

	if args[0].IsError() {
		return NewErrorValue()
	}
	if args[0].IsUndefined() {
		return NewErrorValue()
	}

	// Convert based on type
	if args[0].IsString() {
		return args[0]
	}
	if args[0].IsInteger() {
		val, _ := args[0].IntValue()
		return NewStringValue(fmt.Sprintf("%d", val))
	}
	if args[0].IsReal() {
		val, _ := args[0].RealValue()
		return NewStringValue(fmt.Sprintf("%g", val))
	}
	if args[0].IsBool() {
		val, _ := args[0].BoolValue()
		if val {
			return NewStringValue("true")
		}
		return NewStringValue("false")
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
	if args[0].IsUndefined() {
		return NewErrorValue()
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

	// String: "true" is true, "false" is false, others are ERROR
	if args[0].IsString() {
		str, _ := args[0].StringValue()
		if str == "true" {
			return NewBoolValue(true)
		}
		if str == "false" {
			return NewBoolValue(false)
		}
		return NewErrorValue()
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
	if args[0].IsUndefined() || args[1].IsUndefined() {
		return NewUndefinedValue()
	}

	// Get base value as real
	var base float64
	if args[0].IsInteger() {
		val, _ := args[0].IntValue()
		base = float64(val)
	} else if args[0].IsReal() {
		base, _ = args[0].RealValue()
	} else {
		return NewErrorValue()
	}

	// Get exponent value
	var exp float64
	expIsInt := false
	if args[1].IsInteger() {
		val, _ := args[1].IntValue()
		exp = float64(val)
		expIsInt = true
	} else if args[1].IsReal() {
		exp, _ = args[1].RealValue()
	} else {
		return NewErrorValue()
	}

	result := math.Pow(base, exp)

	// Return integer if both inputs were integer and exp >= 0
	if expIsInt && args[0].IsInteger() && exp >= 0 {
		return NewIntValue(int64(result))
	}

	return NewRealValue(result)
}

// builtinQuantize computes ceiling(a/b)*b for scalars, or finds first value in list >= a
func builtinQuantize(args []Value) Value {
	if len(args) != 2 {
		return NewErrorValue()
	}

	if args[0].IsError() || args[1].IsError() {
		return NewErrorValue()
	}
	if args[0].IsUndefined() || args[1].IsUndefined() {
		return NewUndefinedValue()
	}

	// If second arg is a list
	if args[1].IsList() {
		list, _ := args[1].ListValue()

		// Get first numeric value from args[0]
		var a float64
		if args[0].IsInteger() {
			val, _ := args[0].IntValue()
			a = float64(val)
		} else if args[0].IsReal() {
			a, _ = args[0].RealValue()
		} else {
			return NewErrorValue()
		}

		// Find first value in list >= a
		var lastVal Value
		for _, item := range list {
			if item.IsError() {
				return NewErrorValue()
			}
			if item.IsUndefined() {
				continue
			}

			var itemVal float64
			if item.IsInteger() {
				val, _ := item.IntValue()
				itemVal = float64(val)
			} else if item.IsReal() {
				itemVal, _ = item.RealValue()
			} else {
				return NewErrorValue()
			}

			if itemVal >= a {
				return item
			}
			lastVal = item
		}

		// No value >= a, compute integral multiple of last value
		if lastVal.valueType != UndefinedValue {
			var lastFloat float64
			if lastVal.IsInteger() {
				val, _ := lastVal.IntValue()
				lastFloat = float64(val)
			} else if lastVal.IsReal() {
				lastFloat, _ = lastVal.RealValue()
			}

			quotient := a / lastFloat
			result := math.Ceil(quotient) * lastFloat

			if lastVal.IsInteger() {
				return NewIntValue(int64(result))
			}
			return NewRealValue(result)
		}

		return NewUndefinedValue()
	}

	// Scalar case: compute ceiling(a/b)*b
	var a, b float64
	aIsInt := args[0].IsInteger()
	bIsInt := args[1].IsInteger()

	if args[0].IsInteger() {
		val, _ := args[0].IntValue()
		a = float64(val)
	} else if args[0].IsReal() {
		a, _ = args[0].RealValue()
	} else {
		return NewErrorValue()
	}

	if args[1].IsInteger() {
		val, _ := args[1].IntValue()
		b = float64(val)
	} else if args[1].IsReal() {
		b, _ = args[1].RealValue()
	} else {
		return NewErrorValue()
	}

	if b == 0 {
		return NewErrorValue()
	}

	quotient := a / b
	result := math.Ceil(quotient) * b

	if bIsInt && aIsInt {
		return NewIntValue(int64(result))
	}
	return NewRealValue(result)
}

// builtinSum sums numeric values in a list
func builtinSum(args []Value) Value {
	if len(args) > 1 {
		return NewErrorValue()
	}

	if len(args) == 0 {
		return NewIntValue(0)
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
		return NewRealValue(0.0)
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
		return NewUndefinedValue()
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
		return NewUndefinedValue()
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
		if args[0].IsUndefined() {
			return NewErrorValue()
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
	if args[0].IsUndefined() {
		return NewUndefinedValue()
	}
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
	if args[0].IsUndefined() {
		return NewUndefinedValue()
	}
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
	if args[0].IsUndefined() {
		return NewUndefinedValue()
	}
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

	if args[0].IsError() || args[1].IsError() {
		return NewErrorValue()
	}
	if args[0].IsUndefined() || args[1].IsUndefined() {
		return NewErrorValue()
	}

	// Convert to strings
	var str1, str2 string
	if args[0].IsString() {
		str1, _ = args[0].StringValue()
	} else if args[0].IsInteger() {
		val, _ := args[0].IntValue()
		str1 = fmt.Sprintf("%d", val)
	} else if args[0].IsReal() {
		val, _ := args[0].RealValue()
		str1 = fmt.Sprintf("%g", val)
	} else {
		return NewErrorValue()
	}

	if args[1].IsString() {
		str2, _ = args[1].StringValue()
	} else if args[1].IsInteger() {
		val, _ := args[1].IntValue()
		str2 = fmt.Sprintf("%d", val)
	} else if args[1].IsReal() {
		val, _ := args[1].RealValue()
		str2 = fmt.Sprintf("%g", val)
	} else {
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

	if args[0].IsError() || args[1].IsError() {
		return NewErrorValue()
	}
	if args[0].IsUndefined() || args[1].IsUndefined() {
		return NewErrorValue()
	}

	// Convert to strings
	var str1, str2 string
	if args[0].IsString() {
		str1, _ = args[0].StringValue()
	} else if args[0].IsInteger() {
		val, _ := args[0].IntValue()
		str1 = fmt.Sprintf("%d", val)
	} else if args[0].IsReal() {
		val, _ := args[0].RealValue()
		str1 = fmt.Sprintf("%g", val)
	} else {
		return NewErrorValue()
	}

	if args[1].IsString() {
		str2, _ = args[1].StringValue()
	} else if args[1].IsInteger() {
		val, _ := args[1].IntValue()
		str2 = fmt.Sprintf("%d", val)
	} else if args[1].IsReal() {
		val, _ := args[1].RealValue()
		str2 = fmt.Sprintf("%g", val)
	} else {
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

// builtinAnyCompare checks if any element in list satisfies comparison with t
// anyCompare(op, list, target) where op is "<", "<=", "==", "!=", ">=", ">"
func builtinAnyCompare(args []Value) Value {
	if len(args) != 3 {
		return NewErrorValue()
	}

	if args[0].IsError() || args[1].IsError() || args[2].IsError() {
		return NewErrorValue()
	}
	if args[0].IsUndefined() || args[1].IsUndefined() || args[2].IsUndefined() {
		return NewUndefinedValue()
	}

	if !args[0].IsString() || !args[1].IsList() {
		return NewErrorValue()
	}

	op, _ := args[0].StringValue()
	list, _ := args[1].ListValue()
	target := args[2]

	for _, item := range list {
		if item.IsUndefined() {
			continue
		}

		result := compareValues(op, item, target)
		if result.IsBool() {
			match, _ := result.BoolValue()
			if match {
				return NewBoolValue(true)
			}
		}
	}

	return NewBoolValue(false)
}

// builtinAllCompare checks if all elements in list satisfy comparison with t
func builtinAllCompare(args []Value) Value {
	if len(args) != 3 {
		return NewErrorValue()
	}

	if args[0].IsError() || args[1].IsError() || args[2].IsError() {
		return NewErrorValue()
	}
	if args[0].IsUndefined() || args[1].IsUndefined() || args[2].IsUndefined() {
		return NewUndefinedValue()
	}

	if !args[0].IsString() || !args[1].IsList() {
		return NewErrorValue()
	}

	op, _ := args[0].StringValue()
	list, _ := args[1].ListValue()
	target := args[2]

	if len(list) == 0 {
		return NewBoolValue(true) // vacuously true
	}

	for _, item := range list {
		if item.IsUndefined() {
			continue
		}

		result := compareValues(op, item, target)
		if result.IsBool() {
			match, _ := result.BoolValue()
			if !match {
				return NewBoolValue(false)
			}
		} else {
			return NewBoolValue(false)
		}
	}

	return NewBoolValue(true)
}

// compareValues performs comparison based on operator string
func compareValues(op string, left, right Value) Value {
	// Handle numeric comparison
	if left.IsNumber() && right.IsNumber() {
		leftNum, _ := left.NumberValue()
		rightNum, _ := right.NumberValue()

		switch op {
		case "<":
			return NewBoolValue(leftNum < rightNum)
		case "<=":
			return NewBoolValue(leftNum <= rightNum)
		case "==":
			return NewBoolValue(leftNum == rightNum)
		case "!=":
			return NewBoolValue(leftNum != rightNum)
		case ">=":
			return NewBoolValue(leftNum >= rightNum)
		case ">":
			return NewBoolValue(leftNum > rightNum)
		}
	}

	// Handle string comparison
	if left.IsString() && right.IsString() {
		leftStr, _ := left.StringValue()
		rightStr, _ := right.StringValue()
		cmp := strings.Compare(leftStr, rightStr)

		switch op {
		case "<":
			return NewBoolValue(cmp < 0)
		case "<=":
			return NewBoolValue(cmp <= 0)
		case "==":
			return NewBoolValue(cmp == 0)
		case "!=":
			return NewBoolValue(cmp != 0)
		case ">=":
			return NewBoolValue(cmp >= 0)
		case ">":
			return NewBoolValue(cmp > 0)
		}
	}

	// Handle boolean comparison
	if left.IsBool() && right.IsBool() {
		leftBool, _ := left.BoolValue()
		rightBool, _ := right.BoolValue()

		switch op {
		case "==":
			return NewBoolValue(leftBool == rightBool)
		case "!=":
			return NewBoolValue(leftBool != rightBool)
		}
	}

	return NewErrorValue()
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
func builtinStringListSize(args []Value) Value {
	if len(args) < 1 || len(args) > 2 {
		return NewErrorValue()
	}

	if args[0].IsError() {
		return NewErrorValue()
	}
	if args[0].IsUndefined() {
		return NewUndefinedValue()
	}
	if !args[0].IsString() {
		return NewErrorValue()
	}

	listStr, _ := args[0].StringValue()
	delimiter := ","

	if len(args) == 2 {
		if args[1].IsError() {
			return NewErrorValue()
		}
		if args[1].IsUndefined() {
			return NewUndefinedValue()
		}
		if !args[1].IsString() {
			return NewErrorValue()
		}
		delimiter, _ = args[1].StringValue()
	}

	parts := parseStringList(listStr, delimiter)
	// Don't count empty strings
	count := 0
	for _, part := range parts {
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

	if args[0].IsError() {
		return NewErrorValue()
	}
	if args[0].IsUndefined() {
		return NewUndefinedValue()
	}
	if !args[0].IsString() {
		return NewErrorValue()
	}

	listStr, _ := args[0].StringValue()
	delimiter := ","

	if len(args) == 2 {
		if args[1].IsError() {
			return NewErrorValue()
		}
		if args[1].IsUndefined() {
			return NewUndefinedValue()
		}
		if !args[1].IsString() {
			return NewErrorValue()
		}
		delimiter, _ = args[1].StringValue()
	}

	parts := parseStringList(listStr, delimiter)
	var sum float64
	hasReal := false

	for _, part := range parts {
		if part == "" {
			continue
		}

		var val float64
		if strings.Contains(part, ".") {
			_, err := fmt.Sscanf(part, "%f", &val)
			if err != nil {
				continue
			}
			hasReal = true
		} else {
			var intVal int64
			_, err := fmt.Sscanf(part, "%d", &intVal)
			if err != nil {
				continue
			}
			val = float64(intVal)
		}
		sum += val
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

	if args[0].IsError() {
		return NewErrorValue()
	}
	if args[0].IsUndefined() {
		return NewUndefinedValue()
	}
	if !args[0].IsString() {
		return NewErrorValue()
	}

	listStr, _ := args[0].StringValue()
	delimiter := ","

	if len(args) == 2 {
		if args[1].IsError() {
			return NewErrorValue()
		}
		if args[1].IsUndefined() {
			return NewUndefinedValue()
		}
		if !args[1].IsString() {
			return NewErrorValue()
		}
		delimiter, _ = args[1].StringValue()
	}

	parts := parseStringList(listStr, delimiter)
	var sum float64
	count := 0

	for _, part := range parts {
		if part == "" {
			continue
		}

		var val float64
		_, err := fmt.Sscanf(part, "%f", &val)
		if err != nil {
			continue
		}
		sum += val
		count++
	}

	if count == 0 {
		return NewRealValue(0.0)
	}

	return NewRealValue(sum / float64(count))
}

// builtinStringListMin finds minimum numeric value in a string list
func builtinStringListMin(args []Value) Value {
	if len(args) < 1 || len(args) > 2 {
		return NewErrorValue()
	}

	if args[0].IsError() {
		return NewErrorValue()
	}
	if args[0].IsUndefined() {
		return NewUndefinedValue()
	}
	if !args[0].IsString() {
		return NewErrorValue()
	}

	listStr, _ := args[0].StringValue()
	delimiter := ","

	if len(args) == 2 {
		if args[1].IsError() {
			return NewErrorValue()
		}
		if args[1].IsUndefined() {
			return NewUndefinedValue()
		}
		if !args[1].IsString() {
			return NewErrorValue()
		}
		delimiter, _ = args[1].StringValue()
	}

	parts := parseStringList(listStr, delimiter)
	var minVal float64
	hasValue := false
	hasReal := false

	for _, part := range parts {
		if part == "" {
			continue
		}

		var val float64
		if strings.Contains(part, ".") {
			_, err := fmt.Sscanf(part, "%f", &val)
			if err != nil {
				continue
			}
			hasReal = true
		} else {
			var intVal int64
			_, err := fmt.Sscanf(part, "%d", &intVal)
			if err != nil {
				continue
			}
			val = float64(intVal)
		}

		if !hasValue || val < minVal {
			minVal = val
			hasValue = true
		}
	}

	if !hasValue {
		return NewUndefinedValue()
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

	if args[0].IsError() {
		return NewErrorValue()
	}
	if args[0].IsUndefined() {
		return NewUndefinedValue()
	}
	if !args[0].IsString() {
		return NewErrorValue()
	}

	listStr, _ := args[0].StringValue()
	delimiter := ","

	if len(args) == 2 {
		if args[1].IsError() {
			return NewErrorValue()
		}
		if args[1].IsUndefined() {
			return NewUndefinedValue()
		}
		if !args[1].IsString() {
			return NewErrorValue()
		}
		delimiter, _ = args[1].StringValue()
	}

	parts := parseStringList(listStr, delimiter)
	var maxVal float64
	hasValue := false
	hasReal := false

	for _, part := range parts {
		if part == "" {
			continue
		}

		var val float64
		if strings.Contains(part, ".") {
			_, err := fmt.Sscanf(part, "%f", &val)
			if err != nil {
				continue
			}
			hasReal = true
		} else {
			var intVal int64
			_, err := fmt.Sscanf(part, "%d", &intVal)
			if err != nil {
				continue
			}
			val = float64(intVal)
		}

		if !hasValue || val > maxVal {
			maxVal = val
			hasValue = true
		}
	}

	if !hasValue {
		return NewUndefinedValue()
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
