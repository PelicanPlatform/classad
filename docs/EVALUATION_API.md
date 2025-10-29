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
- `Parse(input string) (*ClassAd, error)` - Parses a ClassAd from a string (new format)
- `ParseOld(input string) (*ClassAd, error)` - Parses a ClassAd from a string (old format)

#### Reading Multiple ClassAds

The library provides two styles of iterators for parsing multiple ClassAds from an `io.Reader`.

**Traditional Iterator Pattern (Go 1.21+):**

- `NewReader(r io.Reader) *Reader` - Creates a Reader for new-style ClassAds (with brackets)
- `NewOldReader(r io.Reader) *Reader` - Creates a Reader for old-style ClassAds (newline-delimited)
- `Next() bool` - Advances to the next ClassAd, returns true if one was found
- `ClassAd() *ClassAd` - Returns the current ClassAd (call after Next() returns true)
- `Err() error` - Returns any error that occurred during iteration

**Go 1.23+ Range-over-Function Pattern:**

- `All(r io.Reader) Seq` - Iterator for new-style ClassAds
- `AllOld(r io.Reader) Seq` - Iterator for old-style ClassAds
- `AllWithIndex(r io.Reader) Seq2` - Iterator with index for new-style ClassAds
- `AllOldWithIndex(r io.Reader) Seq2` - Iterator with index for old-style ClassAds
- `AllWithError(r io.Reader, errPtr *error) Seq` - Iterator with error capture for new-style
- `AllOldWithError(r io.Reader, errPtr *error) Seq` - Iterator with error capture for old-style

**Example Usage (Traditional Pattern):**
```go
import (
    "os"
    "github.com/bbockelm/golang-classads/classad"
)

// Read new-style ClassAds from file
file, _ := os.Open("jobs.classads")
defer file.Close()

reader := classad.NewReader(file)
for reader.Next() {
    ad := reader.ClassAd()
    // Process ClassAd...
}
if err := reader.Err(); err != nil {
    log.Fatal(err)
}

// Read old-style ClassAds
oldFile, _ := os.Open("machines.classads")
defer oldFile.Close()

oldReader := classad.NewOldReader(oldFile)
for oldReader.Next() {
    ad := oldReader.ClassAd()
    // Process ClassAd...
}
if err := oldReader.Err(); err != nil {
    log.Fatal(err)
}
```

**Example Usage (Go 1.23+ Range-over-Function):**
```go
import (
    "os"
    "strings"
    "github.com/bbockelm/golang-classads/classad"
)

// Simple iteration
for ad := range classad.All(strings.NewReader(input)) {
    // Process ClassAd...
}

// Iteration with index
for i, ad := range classad.AllWithIndex(file) {
    fmt.Printf("ClassAd %d: %v\n", i, ad)
}

// Iteration with error handling
var err error
for ad := range classad.AllWithError(file, &err) {
    // Process ClassAd...
}
if err != nil {
    log.Fatal(err)
}
```

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
- Functional form: `ifThenElse(condition, true_value, false_value)` - Evaluates condition and returns appropriate branch

```go
ad, _ := classad.Parse(`[
    x = 10;
    y = 20;

    // Ternary operator
    maxTernary = (x > y) ? x : y;

    // Functional form (useful in nested expressions)
    maxFunc = ifThenElse(x > y, x, y);

    // Can return different types
    status = ifThenElse(x > 5, "high", 0);

    // Handles undefined and error properly
    safeDiv = ifThenElse(y != 0, x / y, undefined)
]`)
// maxTernary = 20
// maxFunc = 20
// status = "high"
// safeDiv = 0.5
```

**ifThenElse behavior:**
- Evaluates first argument as condition
- If condition is `true`, returns second argument
- If condition is `false`, returns third argument
- If condition is `undefined` or `error`, returns that value
- If condition is not boolean, returns `error`

### Attribute References
- Simple: `Cpus`
- In expressions: `Cpus * 2 + Memory / 1024`

### Scoped Attribute References

ClassAds support scoped attribute references for accessing attributes in related ClassAds:

- `MY.attr` - References an attribute in the current ClassAd
- `TARGET.attr` - References an attribute in the target ClassAd (set via `SetTarget()`)
- `PARENT.attr` - References an attribute in the parent ClassAd (set via `SetParent()`)

```go
// Create a job and machine ClassAd
job := classad.New()
job.InsertAttr("Cpus", 2)
job.InsertAttr("Memory", 2048)
job.InsertAttrString("Requirements", "TARGET.Cpus >= MY.Cpus && TARGET.Memory >= MY.Memory")

machine := classad.New()
machine.InsertAttr("Cpus", 4)
machine.InsertAttr("Memory", 8192)

// Set target to enable TARGET.* references
job.SetTarget(machine)

// Evaluate Requirements with TARGET references
if requirements, ok := job.EvaluateAttrBool("Requirements"); ok {
    fmt.Printf("Match: %v\n", requirements)  // true
}
```

**Scoped Reference API:**
- `SetTarget(target *ClassAd)` - Sets the target ClassAd for TARGET.* references
- `GetTarget() *ClassAd` - Returns the current target ClassAd
- `SetParent(parent *ClassAd)` - Sets the parent ClassAd for PARENT.* references
- `GetParent() *ClassAd` - Returns the current parent ClassAd

**Behavior:**
- `MY.attr` always references the current ClassAd (equivalent to `attr`)
- `TARGET.attr` evaluates to `undefined` if no target is set
- `PARENT.attr` evaluates to `undefined` if no parent is set
- Scoped references work in all expressions (requirements, rank, etc.)

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

See `examples/features_demo/main.go` for advanced features including:
- Scoped attribute references (MY., TARGET., PARENT.)
- ClassAd matching with MatchClassAd

Run the examples with:
```bash
go run ./examples/api_demo/main.go
go run ./examples/features_demo/main.go
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

The `is` and `isnt` operators (and their aliases `=?=` and `=!=`) provide strict identity checking (type and value):

```go
// Unlike ==, 'is' checks type identity
ad, _ := classad.Parse(`[
    sameType = (5 is 5);              // true - same type and value
    diffType = (5 is 5.0);            // false - different types (int vs real)
    equalNotIs = (5 == 5.0);          // true - == allows type coercion
    undefCheck = (undefined is undefined);  // true
    errorCheck = (error is error);          // true

    // Meta-equal operator aliases
    metaEqual = (5 =?= 5);            // true - same as 'is'
    metaNotEqual = (5 =!= 5.0);       // true - same as 'isnt'
]`)
```

**Operator Aliases:**
- `=?=` is an alias for `is` (meta-equal operator)
- `=!=` is an alias for `isnt` (meta-not-equal operator)

**Key differences from `==`:**
- `is`/`=?=` requires exact type match (no coercion)
- `is`/`=?=` can compare `undefined` and `error` values
- `is`/`=?=` compares list elements recursively
- `isnt`/`=!=` is the negation of `is`/`=?=`

## Built-in Functions

### String Functions

- `strcat(str1, str2, ...)` - Concatenates strings
- `substr(string, offset[, length])` - Extracts substring (supports negative offsets)
- `size(string_or_list)` - Returns length of string or list
- `length(string_or_list)` - Alias for `size()`
- `toLower(string)` / `tolower(string)` - Converts to lowercase
- `toUpper(string)` / `toupper(string)` - Converts to uppercase
- `stringListMember(string, string_list[, options])` - Tests if string is in comma-separated list
- `regexp(pattern, target[, options])` - Tests if target matches regular expression pattern

```go
ad, _ := classad.Parse(`[
    greeting = strcat("Hello", " ", "World");
    sub = substr("Hello World", 0, 5);
    len = size("Hello");
    lower = toLower("HELLO");
    upper = toUpper("world");

    // String list membership
    colors = "red,green,blue";
    hasRed = stringListMember("red", colors);           // true
    hasYellow = stringListMember("yellow", colors);     // false
    hasGreen = stringListMember("GREEN", colors, "i");  // true (case-insensitive)

    // Regular expression matching
    email = "user@example.com";
    validEmail = regexp("^[^@]+@[^@]+\\.[^@]+$", email);  // true
    startsWithUser = regexp("^user", email);              // true
    caseMatch = regexp("USER", email, "i");               // true (case-insensitive)
]`)
// greeting = "Hello World"
// sub = "Hello"
// len = 5
// lower = "hello"
// upper = "WORLD"
// hasRed = true
// hasYellow = false
// hasGreen = true
// validEmail = true
// startsWithUser = true
// caseMatch = true
```

**stringListMember options:**
- `"i"` or `"icase"` - Case-insensitive comparison

**regexp options:**
- `"i"` - Case-insensitive matching
- `"m"` - Multiline mode (^ and $ match line boundaries)
- `"s"` - Single-line mode (. matches newlines)
- Options can be combined: `"im"`, `"ims"`, etc.

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

## Attribute Selection Expressions

Access nested ClassAd attributes using dot notation (`record.field`):

```go
ad, _ := classad.Parse(`[
    employee = [
        name = "Alice";
        department = [
            name = "Engineering";
            location = "Building A"
        ]
    ];
    empName = employee.name;
    deptName = employee.department.name;
    deptLoc = employee.department.location
]`)

// Access values
name, _ := ad.EvaluateAttrString("empName")           // "Alice"
dept, _ := ad.EvaluateAttrString("deptName")          // "Engineering"
location, _ := ad.EvaluateAttrString("deptLoc")       // "Building A"
```

**Behavior:**
- Returns `undefined` if attribute doesn't exist
- Returns `error` if left side is not a ClassAd
- Can chain multiple selections: `a.b.c.d`

## Subscript Expressions

Access list elements or ClassAd attributes using subscript notation:

### List Subscripting

Use integer indices (0-based) to access list elements:

```go
ad, _ := classad.Parse(`[
    fruits = {"apple", "banana", "cherry"};
    matrix = {{1, 2, 3}, {4, 5, 6}, {7, 8, 9}};

    first = fruits[0];
    third = fruits[2];
    element = matrix[1][2]
]`)

first, _ := ad.EvaluateAttrString("first")    // "apple"
third, _ := ad.EvaluateAttrString("third")    // "cherry"
element, _ := ad.EvaluateAttrInt("element")   // 6
```

### ClassAd Subscripting

Use string keys to access ClassAd attributes:

```go
ad, _ := classad.Parse(`[
    person = [name = "Bob"; age = 30];
    personName = person["name"];
    personAge = person["age"]
]`)

name, _ := ad.EvaluateAttrString("personName")  // "Bob"
age, _ := ad.EvaluateAttrInt("personAge")       // 30
```

### Combined Selection and Subscripting

Mix selection and subscripting for complex data access:

```go
ad, _ := classad.Parse(`[
    company = [
        employees = {
            [name = "Alice"; salary = 100000],
            [name = "Bob"; salary = 95000]
        }
    ];
    firstEmpName = company.employees[0].name;
    secondSalary = company.employees[1].salary
]`)

name, _ := ad.EvaluateAttrString("firstEmpName")    // "Alice"
salary, _ := ad.EvaluateAttrInt("secondSalary")     // 95000
```

**Subscript Behavior:**
- **Lists:** Index must be integer, returns `undefined` if out of bounds
- **ClassAds:** Key must be string, returns `undefined` if not found
- Returns `error` for type mismatches (e.g., string index on list)

## ClassAd Matching with MatchClassAd

The `MatchClassAd` type provides symmetric matching between two ClassAds, inspired by the HTCondor C++ API. It automatically sets up bidirectional TARGET references to enable requirements like `TARGET.Memory >= MY.Memory`.

### Creating a MatchClassAd

```go
import "github.com/bbockelm/golang-classads/classad"

// Create job and machine ClassAds
job := classad.New()
job.InsertAttr("Cpus", 2)
job.InsertAttr("Memory", 2048)
job.InsertAttrString("Requirements", "TARGET.Cpus >= MY.Cpus && TARGET.Memory >= MY.Memory")

machine := classad.New()
machine.InsertAttr("Cpus", 4)
machine.InsertAttr("Memory", 8192)
machine.InsertAttrString("Requirements", "TARGET.Cpus <= MY.Cpus && TARGET.Memory <= MY.Memory")

// Create MatchClassAd - automatically sets up TARGET references
matchAd := classad.NewMatchClassAd(job, machine)
```

### MatchClassAd API

- `NewMatchClassAd(left, right *ClassAd) *MatchClassAd` - Creates a MatchClassAd with bidirectional TARGET setup
- `GetLeftAd() *ClassAd` - Returns the left ClassAd
- `GetRightAd() *ClassAd` - Returns the right ClassAd
- `ReplaceLeftAd(ad *ClassAd)` - Replaces the left ClassAd and updates TARGET references
- `ReplaceRightAd(ad *ClassAd)` - Replaces the right ClassAd and updates TARGET references

### Symmetric Matching

The `Symmetry()` and `Match()` methods evaluate requirements from both sides:

```go
// Check if both Requirements attributes evaluate to true
match := matchAd.Match()
if match {
    fmt.Println("Job and machine match!")
}

// Or use custom requirement attribute names
leftReq := "JobRequirements"
rightReq := "MachineRequirements"
customMatch := matchAd.Symmetry(leftReq, rightReq)
```

**Match Behavior:**
- `Match()` uses the default "Requirements" attribute
- `Symmetry(leftReq, rightReq)` uses custom attribute names
- Returns `true` only if **both** requirements evaluate to `true`
- Returns `false` if either requirement is `false`, `undefined`, or `error`

### Rank Evaluation

After matching, you can evaluate rank expressions to prioritize matches:

```go
// Evaluate rank from the left side's perspective
job.InsertAttrString("Rank", "TARGET.Memory * 2 + TARGET.Cpus")
leftRank := matchAd.EvaluateRankLeft("Rank")
if leftRank.IsReal() {
    rank, _ := leftRank.RealValue()
    fmt.Printf("Job rank: %.2f\n", rank)
}

// Evaluate rank from the right side's perspective
machine.InsertAttrString("Rank", "1000 / TARGET.Memory")
rightRank := matchAd.EvaluateRankRight("Rank")
```

**Rank Methods:**
- `EvaluateRankLeft(rankName string) Value` - Evaluates rank attribute from left ClassAd
- `EvaluateRankRight(rankName string) Value` - Evaluates rank attribute from right ClassAd
- Rank expressions can reference both MY.* and TARGET.* attributes

### Complete Matching Example

```go
// Job ClassAd
job := classad.New()
job.InsertAttr("Cpus", 2)
job.InsertAttr("Memory", 2048)
job.InsertAttrString("Owner", "alice")
job.InsertAttrString("Requirements", "TARGET.Cpus >= MY.Cpus && TARGET.Memory >= MY.Memory")
job.InsertAttrString("Rank", "TARGET.Memory")  // Prefer more memory

// Machine ClassAd
machine := classad.New()
machine.InsertAttr("Cpus", 4)
machine.InsertAttr("Memory", 8192)
machine.InsertAttrString("Name", "slot1@worker1")
machine.InsertAttrString("Requirements", "TARGET.Cpus <= MY.Cpus")
machine.InsertAttrString("Rank", "1000 - TARGET.Memory")  // Prefer lighter jobs

// Create MatchClassAd and check match
matchAd := classad.NewMatchClassAd(job, machine)

if matchAd.Match() {
    fmt.Println("Match successful!")

    // Evaluate ranks
    jobRank := matchAd.EvaluateRankLeft("Rank")
    machineRank := matchAd.EvaluateRankRight("Rank")

    if jobRank.IsReal() && machineRank.IsReal() {
        jr, _ := jobRank.RealValue()
        mr, _ := machineRank.RealValue()
        fmt.Printf("Job rank: %.2f, Machine rank: %.2f\n", jr, mr)
    }
}
```

### Dynamic Replacement

You can replace ClassAds in a MatchClassAd while preserving the bidirectional TARGET setup:

```go
matchAd := classad.NewMatchClassAd(job1, machine1)

// Replace with new ClassAds - TARGET references automatically updated
matchAd.ReplaceLeftAd(job2)
matchAd.ReplaceRightAd(machine2)

// Check match with new ads
if matchAd.Match() {
    fmt.Println("New match successful!")
}
```

This is useful for:
- Reusing MatchClassAd objects in matching loops
- Testing multiple job-machine combinations
- Implementing HTCondor-style matchmaking algorithms

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
- Conditional expressions (ternary operator and ifThenElse function)
- Nested ClassAds and lists
- IS/ISNT operators (with `=?=` and `=!=` aliases)
- Attribute selection expressions (`record.field`)
- Subscript expressions (`list[index]`, `record["key"]`)
- Scoped attribute references (MY., TARGET., PARENT.)
- ClassAd matching with MatchClassAd
- Old ClassAd format support (newline-delimited, no brackets)
- Built-in functions:
  - String functions (strcat, substr, size, toLower, toUpper, stringListMember, regexp)
  - Math functions (floor, ceiling, round, random, int, real)
  - Type checking functions (isUndefined, isError, isString, etc.)
  - List functions (member)
  - Time functions (time)
  - Conditional function (ifThenElse)
- String escape sequences per HTCondor specification:
  - Standard escapes: `\b`, `\t`, `\n`, `\f`, `\r`, `\\`, `\"`, `\'`
  - Octal sequences: `\0-7` (3 digits for 0-3, 2 digits for 4-7)

🚧 **Future Enhancements:**
- Bitwise operators (&, |, ^, ~)
- Shift operators (<<, >>, >>>)
- Additional built-in functions as needed

## Old ClassAd Format

The library supports both "old" and "new" ClassAd formats used by HTCondor:

### New Format (Default)

```go
ad, err := classad.Parse(`[
    Foo = 3;
    Bar = "hello";
    Moo = Foo =!= Undefined
]`)
```

**Characteristics:**
- Enclosed in square brackets `[ ]`
- Attributes separated by semicolons `;`
- Standard in HTCondor 7.5.1 and later
- Supports all ClassAd features

### Old Format

```go
ad, err := classad.ParseOld(`Foo = 3
Bar = "hello"
Moo = Foo =!= Undefined`)
```

**Characteristics:**
- No surrounding brackets
- Attributes separated by newlines
- Used in HTCondor versions before 7.5.1
- Compatible with older HTCondor tools and output

### Implementation Details

The old ClassAd parser converts the old format to new format internally by:
1. Adding surrounding brackets `[ ]`
2. Adding semicolons `;` after each attribute assignment
3. Preserving comments and empty lines
4. Reusing the existing parser for full feature support

This ensures that old ClassAds have access to all features including:
- Nested ClassAds and lists
- Scoped attribute references
- Built-in functions
- All operators and expressions

### Example: Equivalent Formats

**Old Format:**
```
MyType = "Machine"
TargetType = "Job"
Machine = "froth.cs.wisc.edu"
Arch = "INTEL"
OpSys = "LINUX"
Disk = 35882
Memory = 128
Requirements = TARGET.Owner=="smith" || LoadAvg<=0.3
```

**New Format:**
```
[
MyType = "Machine";
TargetType = "Job";
Machine = "froth.cs.wisc.edu";
Arch = "INTEL";
OpSys = "LINUX";
Disk = 35882;
Memory = 128;
Requirements = TARGET.Owner=="smith" || LoadAvg<=0.3
]
```

Both formats parse to the same internal representation and can be evaluated identically.
