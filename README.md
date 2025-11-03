# Golang ClassAds Parser

[![Tests](https://github.com/PelicanPlatform/classad/actions/workflows/test.yml/badge.svg)](https://github.com/PelicanPlatform/classad/actions/workflows/test.yml)
[![License](https://img.shields.io/badge/License-Apache%202.0-blue.svg)](https://opensource.org/licenses/Apache-2.0)
[![Go Report Card](https://goreportcard.com/badge/github.com/PelicanPlatform/classad)](https://goreportcard.com/report/github.com/PelicanPlatform/classad)

A Go implementation of a parser, lexer, and evaluator for the HTCondor ClassAds language, built using goyacc.

## Overview

This project provides a complete parser and evaluation engine for the ClassAds language used by HTCondor (High-Throughput Computing). ClassAds are an attribute-based language for representing and querying structured data, supporting expressions with operators, functions, and nested structures.

## Features

- **Complete Lexer**: Tokenizes ClassAd syntax including:
  - Literals (integers, reals, strings, booleans, undefined, error)
  - String escape sequences per HTCondor spec (`\b`, `\t`, `\n`, `\f`, `\r`, `\\`, `\"`, `\'`, octal)
  - Operators (arithmetic, logical, comparison, bitwise, shift, is/isnt, =?=/=!=)
  - Attribute references and assignments
  - Scoped attribute references (MY., TARGET., PARENT.)
  - Attribute selection (`record.field`)
  - Subscript expressions (`list[index]`, `record["key"]`)
  - Lists and nested records
  - Function calls
  - Comments (line and block)

- **goyacc-based Parser**: Generates efficient parser from grammar specification
- **AST Representation**: Clean Abstract Syntax Tree structures for all ClassAd constructs
- **Evaluation Engine**: Evaluates ClassAd expressions with:
  - Type safety and automatic coercion
  - Nested ClassAds and lists
  - Strict identity operators (`is`/`isnt`, `=?=`/`=!=`)
  - Scoped attribute references for parent/target relationships
  - Attribute selection and subscripting
  - 25+ built-in functions (string, math, type checking, list operations, conditional)
- **ClassAd Matching**: MatchClassAd type for symmetric matching (job/machine matching)
- **Public API**: High-level API mimicking the C++ HTCondor ClassAd library
- **Go Generate Support**: Easy regeneration of parser from grammar

## Quick Start

```go
import "github.com/PelicanPlatform/classad/classad"

// Create a ClassAd programmatically with generic Set() API
ad := classad.New()
ad.Set("Cpus", 4)
ad.Set("Memory", 8192.0)
ad.Set("Name", "worker-01")
ad.Set("Tags", []string{"production", "gpu"})

// Parse from string (new format)
jobAd, err := classad.Parse(`[
    JobId = 1001;
    Owner = "alice";
    Requirements = (Cpus >= 2) && (Memory >= 2048)
]`)

// Parse old ClassAd format (newline-delimited, no brackets)
oldAd, err := classad.ParseOld(`MyType = "Machine"
TargetType = "Job"
Cpus = 4
Memory = 8192`)

// Read multiple ClassAds from a file (traditional pattern)
file, _ := os.Open("jobs.classads")
defer file.Close()

reader := classad.NewReader(file)
for reader.Next() {
    ad := reader.ClassAd()
    // Use generic GetAs[T]() for type-safe retrieval
    if owner, ok := classad.GetAs[string](ad, "Owner"); ok {
        fmt.Printf("Owner: %s\n", owner)
    }
}
if err := reader.Err(); err != nil {
    log.Fatal(err)
}

// Or use Go 1.23+ range-over-function:
for ad := range classad.All(file) {
    if owner, ok := classad.GetAs[string](ad, "Owner"); ok {
        fmt.Printf("Owner: %s\n", owner)
    }
}

// Get attributes with type-safe generic API
if cpus, ok := classad.GetAs[int](jobAd, "Cpus"); ok {
    fmt.Printf("Cpus = %d\n", cpus)
}

// GetOr() provides defaults for missing values
owner := classad.GetOr(jobAd, "Owner", "unknown")
fmt.Printf("Owner: %s\n", owner)

// Complex expressions are evaluated automatically
if requirements, ok := jobAd.EvaluateAttrBool("Requirements"); ok {
    fmt.Printf("Requirements = %v\n", requirements)
}
```

See [docs/EVALUATION_API.md](docs/EVALUATION_API.md) for complete API documentation.

## Generic Get/Set API

The library provides a modern, idiomatic API using Go generics for type-safe attribute access:

```go
ad := classad.New()

// Set() accepts any type
ad.Set("Cpus", 4)
ad.Set("Memory", 8192.0)
ad.Set("Owner", "alice")
ad.Set("Tags", []string{"prod", "gpu"})
ad.Set("IsActive", true)

// GetAs[T]() for type-safe retrieval with two-value return
if cpus, ok := classad.GetAs[int](ad, "Cpus"); ok {
    fmt.Printf("Cpus: %d\n", cpus)
}

if memory, ok := classad.GetAs[float64](ad, "Memory"); ok {
    fmt.Printf("Memory: %.0f MB\n", memory)
}

// GetOr[T]() provides defaults for missing or wrong-type values
owner := classad.GetOr(ad, "Owner", "unknown")
priority := classad.GetOr(ad, "Priority", 10)  // Uses default if missing

// Works with slices
tags := classad.GetOr(ad, "Tags", []string{"default"})

// Type conversions happen automatically where safe
cpusFloat := classad.GetOr(ad, "Cpus", 0.0)  // int -> float64
```

See [examples/generic_api_demo](examples/generic_api_demo/) for comprehensive examples.

## Struct Marshaling

The library supports marshaling Go structs to/from both ClassAd and JSON formats:

```go
// Define a struct with tags
type Job struct {
    ID       int      `classad:"JobId"`
    Owner    string   `classad:"Owner"`
    CPUs     int      `json:"cpus"`        // Falls back to json tag
    Tags     []string `classad:"Tags,omitempty"`
}

// Marshal to ClassAd format
job := Job{ID: 123, Owner: "alice", CPUs: 4, Tags: []string{"prod"}}
classadStr, _ := classad.Marshal(job)
// Result: [JobId = 123; Owner = "alice"; cpus = 4; Tags = {"prod"}]

// Unmarshal from ClassAd format
var job2 Job
classad.Unmarshal(classadStr, &job2)

// JSON marshaling with expression support
ad, _ := classad.Parse(`[x = 5; y = x + 3]`)
jsonBytes, _ := json.Marshal(ad)
// Result: {"x":5,"y":"\/Expr(x + 3)\/"}

// JSON unmarshaling
var ad2 classad.ClassAd
json.Unmarshal(jsonBytes, &ad2)
```

Features:
- Struct tags: `classad:"name"` or falls back to `json:"name"`
- Options: `omitempty`, `-` (skip field)
- Nested structs and slices
- Map support: `map[string]T`, `map[string]interface{}`
- JSON expressions with `/Expr(...))/` format

See [docs/MARSHALING.md](docs/MARSHALING.md) for complete marshaling documentation.

## Project Structure

```
golang-classads/
├── ast/              # Abstract Syntax Tree definitions
│   ├── ast.go
│   └── ast_test.go
├── classad/          # Public ClassAd API and evaluator
│   ├── classad.go
│   ├── classad_test.go
│   ├── evaluator.go
│   ├── evaluator_test.go
│   ├── features_test.go  # Tests for advanced features
│   └── functions.go      # Built-in functions
├── parser/           # Parser and lexer (generated parser from .y file)
│   ├── classad.y     # goyacc grammar specification
│   ├── lexer.go      # Lexer implementation
│   ├── lexer_test.go # Lexer tests
│   ├── parser.go     # Parser API
│   └── y.go          # Generated parser (created by goyacc)
├── cmd/              # Command-line tools
│   └── classad-parser/
│       └── main.go
├── examples/         # Example ClassAd files and demos
│   ├── api_demo/     # Basic API examples
│   ├── features_demo/    # Advanced features demo
│   ├── reader_demo/      # Reader/iterator demo
│   ├── range_demo/       # Go 1.23+ range-over-function demo
│   ├── struct_demo/      # Struct marshaling examples
│   ├── json_demo/        # JSON marshaling examples
│   ├── simple_reader/    # CLI tool for reading ClassAd files
│   ├── README.md     # Examples documentation
│   ├── machine.ad
│   ├── job.ad
│   └── expressions.txt
├── docs/             # Documentation
│   ├── EVALUATION_API.md       # API reference for evaluation
│   ├── MARSHALING.md           # Struct marshaling guide (ClassAd & JSON)
│   └── MARSHALING_QUICKREF.md  # Quick reference card
├── generate.go       # go generate directive
├── go.mod
└── README.md
```

## Installation

### Prerequisites

- Go 1.23 or later (required for range-over-function iterators)
- goyacc (for generating parser)

### Install goyacc

```bash
go install golang.org/x/tools/cmd/goyacc@latest
```

### Build the Project

1. Clone or navigate to the project directory:
```bash
cd ~/projects/golang-classads
```

2. Download dependencies:
```bash
go mod download
```

3. Generate the parser using goyacc:
```bash
# Make sure goyacc is in your PATH (typically ~/go/bin)
export PATH=$PATH:$HOME/go/bin
goyacc -o parser/y.go -p yy parser/classad.y
```

Or use go generate (requires goyacc in PATH):
```bash
go generate ./...
```

4. Build the project:
```bash
go build ./...
```

5. Build the command-line tool:
```bash
go build -o bin/classad-parser ./cmd/classad-parser
```

## Usage

### As a Library

Using the modern generic API (recommended):

```go
package main

import (
    "fmt"
    "github.com/PelicanPlatform/classad/classad"
)

func main() {
    // Create a ClassAd with Set()
    ad := classad.New()
    ad.Set("Cpus", 4)
    ad.Set("Memory", 8192.0)
    ad.Set("Owner", "alice")
    ad.Set("Tags", []string{"prod", "gpu"})

    // Type-safe retrieval with GetAs[T]()
    if cpus, ok := classad.GetAs[int](ad, "Cpus"); ok {
        fmt.Printf("Cpus: %d\n", cpus)
    }

    // Get with defaults using GetOr[T]()
    owner := classad.GetOr(ad, "Owner", "unknown")
    priority := classad.GetOr(ad, "Priority", 10)

    fmt.Printf("Owner: %s, Priority: %d\n", owner, priority)
}
```

### Working with Expressions

The library provides a powerful Expression API for working with unevaluated expressions:

```go
// Parse an expression directly
expr, err := classad.ParseExpr("Cpus * 2 + Memory / 1024")
if err != nil {
    log.Fatal(err)
}

// Evaluate it in a ClassAd context
ad := classad.New()
ad.Set("Cpus", 8)
ad.Set("Memory", 16384)

result := expr.Eval(ad)
if value, ok := result.IntValue(); ok {
    fmt.Printf("Result: %d\n", value)  // Result: 32
}

// Look up unevaluated expressions from ClassAds
sourceAd, _ := classad.Parse("[Formula = Cpus * 2]")
if formula, ok := sourceAd.Lookup("Formula"); ok {
    // Copy expression to another ClassAd
    targetAd := classad.New()
    targetAd.Set("Cpus", 16)
    targetAd.InsertExpr("Computation", formula)

    if value, ok := classad.GetAs[int](targetAd, "Computation"); ok {
        fmt.Printf("Computed: %d\n", value)  // Computed: 32
    }
}

// Scoped evaluation with MY and TARGET contexts
job := classad.New()
job.Set("RequestCpus", 4)
job.Set("RequestMemory", 8192)

machine := classad.New()
machine.Set("Cpus", 8)
machine.Set("Memory", 16384)

// Parse requirements with MY and TARGET references
reqExpr, _ := classad.ParseExpr("MY.RequestCpus <= TARGET.Cpus && MY.RequestMemory <= TARGET.Memory")

// Evaluate with job as MY scope, machine as TARGET scope
result := reqExpr.EvalWithContext(job, machine)
if matches, ok := result.BoolValue(); ok {
    fmt.Printf("Match: %v\n", matches)  // Match: true
}

// Or use the ClassAd method
result = job.EvaluateExprWithTarget(reqExpr, machine)
```

See [examples/expr_demo](examples/expr_demo/main.go) for comprehensive Expression API examples.

### Expression Introspection and Utilities

The library provides powerful introspection and utility methods for analyzing and optimizing expressions:

**Quote/Unquote** - String escaping helpers:
```go
// Quote strings with proper escaping
quoted := classad.Quote(`Hello "World"`)  // Returns: "Hello \"World\""

// Unquote escaped strings
original, err := classad.Unquote(quoted)  // Returns: Hello "World"
```

**MarshalOld** - Convert to old HTCondor format:
```go
ad, _ := classad.Parse(`[Cpus = 4; Memory = 8192]`)
oldFormat := ad.MarshalOld()
// Returns:
// Cpus = 4
// Memory = 8192
```

**ExternalRefs** - Find undefined attribute dependencies:
```go
expr, _ := classad.ParseExpr("RequestCpus * 1000 + Memory / 1024")
job := classad.New()
job.Set("RequestCpus", 4)

missing := job.ExternalRefs(expr)
// Returns: ["Memory"]
// Useful for validation, debugging, and dependency tracking
```

**InternalRefs** - Find defined attribute dependencies:
```go
defined := job.InternalRefs(expr)
// Returns: ["RequestCpus"]
// Useful for change tracking and cache invalidation
```

**Flatten** - Partial evaluation for optimization:
```go
expr, _ := classad.ParseExpr("RequestCpus * 1000 + Memory / 1024 + Unknown")
job := classad.New()
job.Set("RequestCpus", 4)
job.Set("Memory", 8192)

flattened := job.Flatten(expr)
// Returns expression: (4008 + Unknown)
// Known values replaced with literals, unknown values preserved
```

See [examples/introspection_demo](examples/introspection_demo/main.go) for comprehensive introspection examples including:
- Dependency analysis and validation
- Query optimization with partial evaluation
- Cache key computation
- Old format compatibility

### Examples

The `examples/` directory contains several demonstration programs showcasing different aspects of the library:

#### Getting Started Examples

**Generic API Demo** - Modern idiomatic Go API:
```bash
go run ./examples/generic_api_demo/main.go
```
Demonstrates the recommended `Set()`, `GetAs[T]()`, and `GetOr[T]()` APIs with type safety.

**API Demo** - Comprehensive API overview:
```bash
go run ./examples/api_demo/main.go
```
Shows creating ClassAds, parsing, evaluating expressions, type-safe attribute access, arithmetic/logical operations, and real-world scenarios.

**Reader Demo** - Reading multiple ClassAds:
```bash
go run ./examples/reader_demo/main.go
```
Demonstrates reading ClassAds from various sources using the Reader API and filtering.

**Range Demo** - Go 1.23+ iterators:
```bash
go run ./examples/range_demo/main.go
```
Shows modern range-over-function patterns for iterating ClassAds.

#### Advanced Examples

**Features Demo** - Advanced ClassAd features:
```bash
go run ./examples/features_demo/main.go
```
Demonstrates nested ClassAds, IS/ISNT operators, built-in functions (string, math, type checking), list operations, and job matching.

**Expression Demo** - Working with unevaluated expressions:
```bash
go run ./examples/expr_demo/main.go
```
Shows parsing expressions, evaluation in contexts, copying expressions between ClassAds, and scoped evaluation with MY/TARGET.

**Introspection Demo** - Expression analysis and optimization:
```bash
go run ./examples/introspection_demo/main.go
```
Demonstrates dependency analysis, partial evaluation (Flatten), and expression utilities.

**Struct Demo** - Marshaling Go structs:
```bash
go run ./examples/struct_demo/main.go
```
Shows marshaling/unmarshaling between Go structs and ClassAd format using struct tags.

**JSON Demo** - JSON serialization:
```bash
go run ./examples/json_demo/main.go
```
Demonstrates JSON marshaling/unmarshaling with special `/Expr(...)/` syntax for expressions.

See [examples/README.md](examples/README.md) for detailed documentation of all examples.

### Command-Line Tool

Parse a ClassAd (new format):

```bash
./bin/classad-parser '[x = 10; y = x + 5]'
# Output: [x = 10; y = (x + 5)]
```

Parse a more complex example:

```bash
./bin/classad-parser '[Cpus = 4; Memory = 8192; Requirements = (Cpus >= 2) && (Memory >= 4096)]'
# Output: [Cpus = 4; Memory = 8192; Requirements = ((Cpus >= 2) && (Memory >= 4096))]
```

Parse old ClassAd format:

```bash
./bin/classad-parser -old $'Foo = 3\nBar = "hello"\nMoo = Foo + 2'
# Output: [Foo = 3; Bar = "hello"; Moo = (Foo + 2)]
```

View help:

```bash
./bin/classad-parser -help
```

## ClassAds Language Features

### Literals

- **Integers**: `42`, `0`, `-10`
- **Reals**: `3.14`, `2.5e10`, `1.5E-5`
- **Strings**: `"hello"`, `"hello\nworld"`
- **Booleans**: `true`, `false`
- **Special**: `undefined`, `error`

### Operators

- **Arithmetic**: `+`, `-`, `*`, `/`, `%`
- **Comparison**: `<`, `>`, `<=`, `>=`, `==`, `!=`
- **Logical**: `&&`, `||`, `!`
- **Bitwise**: `&`, `|`, `^`, `~`
- **Shift**: `<<`, `>>`, `>>>`
- **Strict Identity**: `is`, `isnt`, `=?=`, `=!=` (type and value must match exactly)
  - `=?=` is an alias for `is` (meta-equal)
  - `=!=` is an alias for `isnt` (meta-not-equal)

### Expressions

- **Attribute Assignment**: `name = value`
- **Attribute Reference**: `TARGET.Memory`
- **Conditional**: `x > 0 ? "positive" : "negative"`
- **Function Call**: `strcat("hello", " ", "world")`, `floor(3.14)`, `member(x, list)`
- **List**: `{1, 2, 3, 4, 5}`
- **Nested Record**: `[a = 1; b = [x = 10; y = 20]]`
- **Selection**: `record.field`, `a.b.c` (chaining supported)
- **Subscript**: `list[0]`, `record["key"]`, `matrix[1][2]` (chaining supported)

### Built-in Functions

The evaluator supports 25+ built-in functions:

**String Functions:**
- `strcat(s1, s2, ...)` - Concatenate strings
- `substr(str, offset, length)` - Extract substring
- `size(str)` / `length(str)` - String/list length
- `toUpper(str)` / `toLower(str)` - Case conversion
- `stringListMember(str, list, options)` - Test membership in comma-separated list
- `regexp(pattern, target, options)` - Regular expression matching

**Math Functions:**
- `floor(x)`, `ceiling(x)`, `round(x)` - Rounding
- `int(x)`, `real(x)` - Type conversion
- `random(max)` - Random number generation

**Type Checking:**
- `isUndefined(x)`, `isError(x)` - Special value checks
- `isString(x)`, `isInteger(x)`, `isReal(x)`, `isBoolean(x)` - Type checks
- `isList(x)`, `isClassAd(x)` - Structure checks

**List Operations:**
- `member(item, list)` - Check list membership

**Conditional:**
- `ifThenElse(cond, trueVal, falseVal)` - Functional conditional expression

**Time:**
- `time()` - Current Unix timestamp

See [docs/EVALUATION_API.md](docs/EVALUATION_API.md) for complete function documentation.

### Example ClassAd

**New Format:**
```classad
[
  Machine = "execute.example.com";
  Cpus = 4;
  Memory = 8192;
  Disk = 1000000;
  Requirements = (TARGET.Cpus >= 2) && (TARGET.Memory >= 4096);
  Rank = TARGET.Memory;
  LoadAvg = 0.5;
  IsAvailable = LoadAvg < 1.0;
]
```

**Old Format:**
```classad
Machine = "execute.example.com"
Cpus = 4
Memory = 8192
Disk = 1000000
Requirements = (TARGET.Cpus >= 2) && (TARGET.Memory >= 4096)
Rank = TARGET.Memory
LoadAvg = 0.5
IsAvailable = LoadAvg < 1.0
```

### Old vs New ClassAd Format

This library supports both the old and new ClassAd formats used by HTCondor:

**New Format (default):**
- Enclosed in square brackets `[ ]`
- Attributes separated by semicolons `;`
- Standard in HTCondor 7.5.1 and later
- Supports all ClassAd features (lists, nested ClassAds, etc.)

**Old Format:**
- No surrounding brackets
- Attributes separated by newlines
- Used in HTCondor versions before 7.5.1
- Compatible with older HTCondor tools
- Use `ParseOld()` API or `-old` flag in CLI

The library automatically converts old format to new format internally, ensuring full compatibility and feature support.

## Development

### Modifying the Grammar

1. Edit `parser/classad.y`
2. Regenerate the parser:
```bash
export PATH=$PATH:$HOME/go/bin
goyacc -o parser/y.go -p yy parser/classad.y
```

3. Rebuild:
```bash
go build ./...
```

### Running Tests

Run all tests:
```bash
go test ./...
```

Run tests with verbose output:
```bash
go test -v ./...
```

Run tests with coverage:
```bash
go test -cover ./...
```

Test packages include:
- `ast` - AST node tests
- `parser` - Lexer and parser tests
- `classad` - Evaluation API tests:
  - classad_test.go: ClassAd CRUD operations
  - evaluator_test.go: Expression evaluation, type coercion, error handling
  - features_test.go: Nested structures, is/isnt operators, built-in functions

### Code Formatting

Format code:
```bash
go fmt ./...
```

### Linting

```bash
go vet ./...
```

## Language Reference

The ClassAds language is documented in:
- [ClassAd Language Reference Manual](https://htcondor.org/classad/refman/)
- [ClassAd Reference Manual PDF](https://htcondor.org/classad/refman.V2.2/refman.pdf)

## License

This project is licensed under the Apache License 2.0 - see the [LICENSE](LICENSE) file for details.

This project implements the ClassAds language specification from HTCondor.

## Contributing

### Development Setup

1. Install [pre-commit](https://pre-commit.com/) for automatic code quality checks:
   ```bash
   pip install pre-commit
   # or: brew install pre-commit
   ```

2. Install the git hooks:
   ```bash
   pre-commit install
   ```

3. (Optional) Run pre-commit against all files:
   ```bash
   pre-commit run --all-files
   ```

See [.pre-commit-setup.md](.pre-commit-setup.md) for detailed setup instructions.

### Making Changes

1. Make changes to the appropriate files
2. Regenerate parser if grammar was modified: `go generate ./...`
3. Run tests: `go test ./...`
4. Format code: `go fmt ./...`
5. Pre-commit hooks will run automatically on `git commit`
6. Submit a pull request

The CI pipeline will automatically run tests on your PR for Go versions 1.21, 1.22, and 1.23.

## Troubleshooting

### Parser Generation Fails

Make sure goyacc is installed and in your PATH:
```bash
go install golang.org/x/tools/cmd/goyacc@latest
export PATH=$PATH:$HOME/go/bin
```

Then regenerate the parser:
```bash
goyacc -o parser/y.go -p yy parser/classad.y
```

### Import Errors

Run:
```bash
go mod tidy
```

### Tests Fail

Make sure the parser has been generated:
```bash
export PATH=$PATH:$HOME/go/bin
goyacc -o parser/y.go -p yy parser/classad.y
```

## TODO

- [ ] Additional built-in functions as needed
