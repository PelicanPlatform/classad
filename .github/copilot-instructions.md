# Golang ClassAds Project Instructions

This project implements a parser and lexer for the HTCondor ClassAds language using goyacc.

## Project Structure
- `parser/` - goyacc grammar file and generated parser
- `lexer/` - Lexer implementation for tokenizing ClassAds
- `ast/` - Abstract Syntax Tree node definitions
- `cmd/` - Command-line tools and examples
- `examples/` - Example ClassAds files
- `tests/` - Unit tests

## Development Workflow
1. Modify `parser/classad.y` for grammar changes
2. Run `go generate ./...` to regenerate parser
3. Build with `go build ./...`
4. Test with `go test ./...`

## ClassAds Language Features
- Attribute definitions: `name = value;`
- Expressions with arithmetic, logical, and comparison operators
- String, integer, real, boolean literals
- List values: `{1, 2, 3}`
- Record values: `[a = 1; b = 2]`
- Attribute references
- Built-in functions

## Code Style
- Follow standard Go conventions
- Use gofmt for formatting
- Document exported functions and types
