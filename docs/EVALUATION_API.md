# ClassAd Evaluation API

This document describes the public API for creating, parsing, and evaluating HTCondor ClassAds in Go.

## Overview

The `classad` package provides a high-level API for working with ClassAds, including:
- Creating ClassAds programmatically
- Parsing ClassAd expressions
- Evaluating expressions with type safety
- Modifying ClassAd attributes

## Quick Start

```go
import "github.com/bbockelm/golang-classads/classad"

// Create a new ClassAd
ad := classad.New()
ad.InsertAttr("Cpus", 4)
ad.InsertAttrFloat("Memory", 8192.0)
ad.InsertAttrString("Name", "worker-01")

// Parse a ClassAd from string
jobAd, err := classad.Parse(`[
    JobId = 1001;
    Owner = "alice";
    Cpus = 2;
    Requirements = (Cpus >= 2) && (Memory >= 2048)
]`)

// Evaluate attributes with type safety
if cpus, ok := jobAd.EvaluateAttrInt("Cpus"); ok {
    fmt.Printf("Cpus = %d\n", cpus)
}

if requirements, ok := jobAd.EvaluateAttrBool("Requirements"); ok {
    fmt.Printf("Requirements = %v\n", requirements)
}
```

## API Reference

### ClassAd Type

The `ClassAd` type represents a ClassAd object and provides methods for manipulating attributes.

#### Creation and Parsing

- `New() *ClassAd` - Creates a new empty ClassAd
- `Parse(input string) (*ClassAd, error)` - Parses a ClassAd from a string

#### Attribute Manipulation

- `InsertAttr(name string, value interface{})` - Inserts an attribute (auto-detects type)
- `InsertAttrInt(name string, value int64)` - Inserts an integer attribute
- `InsertAttrFloat(name string, value float64)` - Inserts a float attribute
- `InsertAttrString(name string, value string)` - Inserts a string attribute
- `InsertAttrBool(name string, value bool)` - Inserts a boolean attribute
- `Lookup(name string) ast.Expr` - Returns the expression for an attribute (or nil)
- `Delete(name string) bool` - Deletes an attribute
- `Clear()` - Removes all attributes
- `Size() int` - Returns the number of attributes
- `GetAttributes() []string` - Returns a list of all attribute names

#### Evaluation Methods

- `EvaluateAttr(name string) Value` - Evaluates an attribute and returns a Value
- `EvaluateAttrInt(name string) (int64, bool)` - Evaluates as integer
- `EvaluateAttrReal(name string) (float64, bool)` - Evaluates as float
- `EvaluateAttrNumber(name string) (float64, bool)` - Evaluates as number (int or float)
- `EvaluateAttrString(name string) (string, bool)` - Evaluates as string
- `EvaluateAttrBool(name string) (bool, bool)` - Evaluates as boolean
- `EvaluateExpr(expr ast.Expr) Value` - Evaluates an AST expression
- `EvaluateExprString(exprStr string) (Value, error)` - Parses and evaluates an expression string

### Value Type

The `Value` type represents an evaluated ClassAd value and can be one of 9 types:

- `UndefinedValue` - Undefined/missing value
- `ErrorValue` - Error during evaluation
- `BooleanValue` - Boolean (true/false)
- `IntegerValue` - 64-bit integer
- `RealValue` - 64-bit float
- `StringValue` - String
- `ListValue` - List of Values
- `ClassAdValue` - Nested ClassAd

#### Value Construction

- `NewUndefinedValue() Value`
- `NewErrorValue() Value`
- `NewBoolValue(b bool) Value`
- `NewIntValue(i int64) Value`
- `NewRealValue(r float64) Value`
- `NewStringValue(s string) Value`
- `NewListValue(list []Value) Value`
- `NewClassAdValue(ad *ClassAd) Value`

#### Value Type Checking

- `Type() ValueType` - Returns the type
- `IsUndefined() bool`
- `IsError() bool`
- `IsBool() bool`
- `IsInteger() bool`
- `IsReal() bool`
- `IsNumber() bool` - True for integer or real
- `IsString() bool`
- `IsList() bool`
- `IsClassAd() bool`

#### Value Extraction

- `BoolValue() (bool, error)`
- `IntValue() (int64, error)`
- `RealValue() (float64, error)`
- `NumberValue() (float64, error)` - Converts integer to float if needed
- `StringValue() (string, error)`
- `ListValue() ([]Value, error)`
- `ClassAdValue() (*ClassAd, error)`
- `String() string` - Returns string representation

## Expression Evaluation

The evaluator supports:

### Arithmetic Operators
- Addition: `+`
- Subtraction: `-`
- Multiplication: `*`
- Division: `/`
- Modulo: `%`
- Unary plus/minus: `+x`, `-x`

### Comparison Operators
- Less than: `<`
- Greater than: `>`
- Less than or equal: `<=`
- Greater than or equal: `>=`
- Equal: `==`
- Not equal: `!=`

### Logical Operators
- Logical AND: `&&`
- Logical OR: `||`
- Logical NOT: `!`

### Conditional Operator
- Ternary: `condition ? true_value : false_value`

### Attribute References
- Simple: `Cpus`
- In expressions: `Cpus * 2 + Memory / 1024`

### Type Coercion
- Integer + Real → Real
- Comparisons work across numeric types
- String comparisons are lexicographic

### Error Handling
- Undefined attributes evaluate to `UndefinedValue`
- Type mismatches return `ErrorValue`
- Division by zero returns `ErrorValue`
- Errors propagate through expressions

## Examples

See `examples/api_demo/main.go` for comprehensive examples of:
1. Creating ClassAds programmatically
2. Parsing ClassAds from strings
3. Looking up attributes
4. Evaluating with type safety
5. Complex expressions
6. Arithmetic operations
7. Logical expressions
8. Conditional expressions
9. Modifying ClassAds
10. Real-world HTCondor scenarios
11. Handling undefined values

Run the examples with:
```bash
go run ./examples/api_demo/main.go
```

## Testing

Run the test suite:
```bash
go test ./classad/...
```

The test suite includes:
- ClassAd CRUD operations
- Expression evaluation
- Type checking and coercion
- Error handling
- Value operations
- Arithmetic, comparison, and logical operations
- Unary operations
- Complex expressions
- Nested ClassAds and lists
- IS/ISNT operators
- Built-in functions

## Nested ClassAds and Lists

ClassAds support nested structures:

```go
// Lists
ad, _ := classad.Parse(`[numbers = {1, 2, 3, 4, 5}]`)
numbersVal := ad.EvaluateAttr("numbers")
if numbersVal.IsList() {
    list, _ := numbersVal.ListValue()
    // Access list elements
}

// Nested ClassAds
ad, _ := classad.Parse(`[
    server = [host = "example.com"; port = 8080];
    name = "web-server"
]`)
serverVal := ad.EvaluateAttr("server")
if serverVal.IsClassAd() {
    serverAd, _ := serverVal.ClassAdValue()
    host, _ := serverAd.EvaluateAttrString("host")
    port, _ := serverAd.EvaluateAttrInt("port")
}
```

## IS and ISNT Operators

The `is` and `isnt` operators provide strict identity checking (type and value):

```go
// Unlike ==, 'is' checks type identity
ad, _ := classad.Parse(`[
    sameType = (5 is 5);              // true - same type and value
    diffType = (5 is 5.0);            // false - different types (int vs real)
    equalNotIs = (5 == 5.0);          // true - == allows type coercion
    undefCheck = (undefined is undefined);  // true
    errorCheck = (error is error);          // true
]`)
```

Key differences from `==`:
- `is` requires exact type match (no coercion)
- `is` can compare `undefined` and `error` values
- `is` compares list elements recursively
- `isnt` is the negation of `is`

## Built-in Functions

### String Functions

- `strcat(str1, str2, ...)` - Concatenates strings
- `substr(string, offset[, length])` - Extracts substring (supports negative offsets)
- `size(string_or_list)` - Returns length of string or list
- `length(string_or_list)` - Alias for `size()`
- `toLower(string)` / `tolower(string)` - Converts to lowercase
- `toUpper(string)` / `toupper(string)` - Converts to uppercase

```go
ad, _ := classad.Parse(`[
    greeting = strcat("Hello", " ", "World");
    sub = substr("Hello World", 0, 5);
    len = size("Hello");
    lower = toLower("HELLO");
    upper = toUpper("world")
]`)
// greeting = "Hello World"
// sub = "Hello"
// len = 5
// lower = "hello"
// upper = "WORLD"
```

### Math Functions

- `floor(number)` - Returns floor as integer
- `ceiling(number)` / `ceil(number)` - Returns ceiling as integer
- `round(number)` - Rounds to nearest integer
- `random([max])` - Returns random real 0-1 (or 0-max)
- `int(value)` - Converts to integer
- `real(value)` - Converts to real

```go
ad, _ := classad.Parse(`[
    f = floor(3.7);       // 3
    c = ceiling(3.2);     // 4
    r = round(3.5);       // 4
    i = int(3.9);         // 3
    rl = real(5);         // 5.0
    rand = random(100)    // random float 0-100
]`)
```

### Type Checking Functions

- `isUndefined(value)` - Returns true if value is undefined
- `isError(value)` - Returns true if value is an error
- `isString(value)` - Returns true if value is a string
- `isInteger(value)` - Returns true if value is an integer
- `isReal(value)` - Returns true if value is a real number
- `isBoolean(value)` - Returns true if value is a boolean
- `isList(value)` - Returns true if value is a list
- `isClassAd(value)` - Returns true if value is a ClassAd

```go
ad, _ := classad.Parse(`[
    x = 42;
    checkInt = isInteger(x);      // true
    checkStr = isString(x);       // false
    checkUndef = isUndefined(y)   // true (y doesn't exist)
]`)
```

### List Functions

- `member(element, list)` - Returns true if element is in list

```go
ad, _ := classad.Parse(`[
    nums = {1, 2, 3, 4, 5};
    hasThree = member(3, nums);   // true
    hasTen = member(10, nums)     // false
]`)
```

### Time Functions

- `time()` - Returns current Unix timestamp (seconds since epoch)

```go
ad, _ := classad.Parse(`[now = time()]`)
```

## Error Handling

Functions properly propagate undefined and error values:

```go
ad, _ := classad.Parse(`[
    x = undefined;
    result = size(x)  // result is undefined
]`)

ad2, _ := classad.Parse(`[
    x = error;
    result = size(x)  // result is error
]`)
```

## Compatibility

This API is designed to mimic the C++ HTCondor ClassAd library, providing similar functionality:
- `Insert*()` methods for type-safe attribute insertion
- `EvaluateAttr*()` methods for type-safe evaluation
- `Lookup()` for accessing raw expressions
- Value type system matching ClassAd semantics
- Built-in functions matching HTCondor ClassAd functions
- IS/ISNT operators for strict identity checking

## Implementation Status

✅ **Implemented:**
- Complete ClassAd CRUD API
- Expression evaluation (arithmetic, logical, comparison)
- Conditional expressions
- Nested ClassAds and lists
- IS/ISNT operators
- Built-in functions:
  - String functions (strcat, substr, size, toLower, toUpper)
  - Math functions (floor, ceiling, round, random, int, real)
  - Type checking functions (isUndefined, isError, isString, etc.)
  - List functions (member)
  - Time functions (time)

🚧 **Future Enhancements:**
- Select expressions (record.field)
- Subscript expressions (list[index])
- Bitwise operators (&, |, ^, ~)
- Shift operators (<<, >>, >>>)
- Additional built-in functions as needed
