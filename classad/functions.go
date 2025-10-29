package classad

import (
	"math"
	"math/rand"
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

	max, _ := args[0].NumberValue()
	return NewRealValue(rand.Float64() * max)
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
