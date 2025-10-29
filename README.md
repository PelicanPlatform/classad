# Golang ClassAds Parser

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
import "github.com/bbockelm/golang-classads/classad"

// Create a ClassAd programmatically
ad := classad.New()
ad.InsertAttr("Cpus", 4)
ad.InsertAttrFloat("Memory", 8192.0)

// Parse from string
jobAd, err := classad.Parse(`[
    JobId = 1001;
    Owner = "alice";
    Requirements = (Cpus >= 2) && (Memory >= 2048)
]`)

// Evaluate attributes
if cpus, ok := jobAd.EvaluateAttrInt("Cpus"); ok {
    fmt.Printf("Cpus = %d\n", cpus)
}

if requirements, ok := jobAd.EvaluateAttrBool("Requirements"); ok {
    fmt.Printf("Requirements = %v\n", requirements)
}
```

See [docs/EVALUATION_API.md](docs/EVALUATION_API.md) for complete API documentation.

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
│   ├── README.md     # Examples documentation
│   ├── machine.ad
│   ├── job.ad
│   └── expressions.txt
├── docs/             # Documentation
│   └── EVALUATION_API.md
├── generate.go       # go generate directive
├── go.mod
└── README.md
```

## Installation

### Prerequisites

- Go 1.21 or later
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

Using the high-level ClassAd API:

```go
package main

import (
    "fmt"
    "github.com/bbockelm/golang-classads/classad"
)

func main() {
    // Parse a ClassAd
    ad, err := classad.Parse(`[
        Cpus = 4;
        Memory = 8192;
        Requirements = (Cpus >= 2) && (Memory >= 4096)
    ]`)
    if err != nil {
        fmt.Println("Error:", err)
        return
    }
    
    // Evaluate attributes
    if cpus, ok := ad.EvaluateAttrInt("Cpus"); ok {
        fmt.Printf("Cpus: %d\n", cpus)
    }
    
    if req, ok := ad.EvaluateAttrBool("Requirements"); ok {
        fmt.Printf("Requirements: %v\n", req)
    }
}
```

Using the lower-level parser directly:

```go
package main

import (
    "fmt"
    "github.com/bbockelm/golang-classads/parser"
)

func main() {
    input := "[x = 10; y = x + 5]"
    result, err := parser.Parse(input)
    if err != nil {
        fmt.Println("Error:", err)
        return
    }
    fmt.Println(result.String())
}
```

### Examples

Run the comprehensive API demo:

```bash
go run ./examples/api_demo/main.go
```

This demonstrates:
- Creating ClassAds programmatically
- Parsing ClassAd strings
- Evaluating expressions
- Type-safe attribute access
- Arithmetic, comparison, and logical operations
- Conditional expressions
- Modifying ClassAds
- Real-world HTCondor scenarios

Run the advanced features demo:

```bash
go run ./examples/features_demo/main.go
```

This demonstrates:
- Nested ClassAds and lists
- IS/ISNT operators (strict identity checking)
- Built-in string functions (strcat, substr, toUpper, toLower, size)
- Built-in math functions (floor, ceiling, round, int, real, random)
- Built-in type checking functions (isString, isInteger, isList, etc.)
- List operations (member, size)
- HTCondor job matching simulation

See [examples/README.md](examples/README.md) for detailed examples and usage patterns.
- Creating ClassAds programmatically
- Parsing ClassAd strings
- Evaluating expressions
- Type-safe attribute access
- Arithmetic, comparison, and logical operations
- Conditional expressions
- Modifying ClassAds
- Real-world HTCondor scenarios

### Command-Line Tool

Parse a ClassAd:

```bash
./bin/classad-parser '[x = 10; y = x + 5]'
# Output: [x = 10; y = (x + 5)]
```

Parse a more complex example:

```bash
./bin/classad-parser '[Cpus = 4; Memory = 8192; Requirements = (Cpus >= 2) && (Memory >= 4096)]'
# Output: [Cpus = 4; Memory = 8192; Requirements = ((Cpus >= 2) && (Memory >= 4096))]
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

This project implements the ClassAds language specification from HTCondor.

## Contributing

1. Make changes to the appropriate files
2. Regenerate parser if grammar was modified: `go generate ./...`
3. Run tests: `go test ./...`
4. Format code: `go fmt ./...`
5. Submit a pull request

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
- [ ] Support for old ClassAd syntax
