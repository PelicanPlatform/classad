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

## Opening a store is O(segments), not O(records)

`collections` opens an archive or table by reading per-segment METADATA. Nothing on the open
path may read the store's records one at a time, except the single unsealed (active) segment,
which has no sidecar and must be derived from its records. One breach is still open and known:
without a clean Close there is no directory snapshot, and `rebuildDir` / `rebuildAppendLog` then
walk the whole store -- fixable the same way, by persisting each sealed segment's record count
and sequence range.

This keeps regressing because the per-record work is cheap per record: a header read, no
decompress, so it surfaces as an open that is merely slow. Three separate phases had it. Each
computed a fact about a sealed segment, held it in RAM, and threw it away at close, so every
start rediscovered it, forever. On a 545 MiB archive that was ~90% of a cold open.

- The remedy is to **persist the fact into the segment's sidecar and adopt it on reopen** --
  what `persistSegZonesUpgrade` does for zone maps and `persistSegTrailerHintsUpgrade` for the
  offsets of the dictionary and columnar-payload records. Include the write-back for sidecars
  that predate the field, or existing stores pay the walk for the rest of their lives; count
  whether it landed, because a write-back that silently fails looks exactly like no fix at all.
- Making the work **lazy** -- on first use rather than at open -- is the other acceptable
  answer, and the right one where the data is often not needed at all. The sealed-segment key
  Bloom is built that way: a store nobody looks keys up in never builds one.
- **Check it with `OpenTiming`** (`OpenIndexDiag.Timing`, via `OpenIndexDiagHook`), which breaks
  an open into its phases and counts `RecordsWalked`. Measure before attributing: of the phases
  that have been the culprit, no two were the same one. `TestOpenDoesNotScaleWithRecordCount`
  holds the bound; it asserts on `RecordsWalked`, not elapsed time, because the regression is
  not slow enough to fail a timing test reliably.
