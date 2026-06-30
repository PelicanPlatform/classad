# Differential fuzzing: Go ClassAd vs. C++ libclassad

This directory contains a differential fuzzing harness that finds behavioral
differences between the two ClassAd evaluation engines available in this
container:

- **Go** — the native implementation in [`../classad`](../classad).
- **C++** — HTCondor's reference `libclassad` (the same library the `classad2`
  Python bindings and the `classad_eval` CLI wrap).

Both engines parse the *same* ClassAd, evaluate *every* top-level attribute, and
encode the results into a common [canonical form](canon/canon.go). The harness
then compares the two and reports any divergence.

## Why in-process (cgo), not a subprocess

The C++ engine is reached **in-process via cgo** ([`oracle/cgo`](oracle/cgo)),
not by shelling out to `classad_eval` or a Python worker. `libclassad` is a C++
library, so a small `extern "C"` shim ([`shim.cc`](oracle/cgo/shim.cc)) bridges
the C ABI that cgo speaks to the C++ ClassAd API; cgo compiles and links it
automatically. This means one Go process parses and evaluates in *both* engines
with no IPC or process-startup overhead, which is what makes coverage-guided
fuzzing run at thousands of execs/second.

Trade-off: because `libclassad` shares the process, a hard crash there
(segfault/abort) takes the fuzzer down. The shim catches all C++ exceptions, and
`cafuzz -journal` records the current input before each evaluation so a crash
leaves a reproducer. A C++ crash is itself a finding.

### Build requirements

- `condor-devel` (provides `/usr/include/classad/*.h`) — already in the
  [devcontainer Dockerfile](../.devcontainer/Dockerfile).
- An unversioned `libclassad.so` symlink for the linker (the Dockerfile creates
  it).
- `CGO_ENABLED=1` and a C++20 compiler (the headers use `operator<=>`).

## Components

| Path | Role |
|------|------|
| [`canon/`](canon) | Canonical, engine-independent value encoding + tolerant compare. The lingua franca both engines emit. |
| [`oracle/cgo/`](oracle/cgo) | The C++ engine: `extern "C"` shim over `libclassad` + cgo bindings. |
| [`differ/`](differ) | Runs one ClassAd through both engines and classifies the outcome. |
| [`gen/`](gen) | Grammar-directed random ClassAd generator (deterministic per seed). |
| [`cmd/cafuzz/`](cmd/cafuzz) | Standalone driver: generate/replay ads, bucket & report all divergences. |
| [`cmd/genseeds/`](cmd/genseeds) | Regenerates `corpus/seeds.txt`. |
| [`differential_test.go`](differential_test.go) | `FuzzDifferential`, the Go-native coverage-guided target. |

## The canonical encoding

Direct string comparison of engine output is hopeless — Go prints reals as
`0.5` and lists as `[1 2 3]`, while `libclassad` prints `5.0E-01` and
`{ 1,2,3 }`. Instead both engines encode each evaluated value into a compact,
self-delimiting line (see the doc comment in [`canon/canon.go`](canon/canon.go))
that captures **type and value** but not formatting. Comparison is:

1. exact string match (the common case), then
2. a structural compare with a tight float tolerance (`1e-12`) to absorb
   last-ULP differences from `libm`.

The `int` vs `real` distinction is preserved and significant — `int(1)` and
`real(1.0)` are *not* equal, because conflating them would hide a whole class of
ClassAd bugs.

## Usage

### Bulk survey — `cafuzz`

The primary discovery tool. Generates random ads, evaluates in both engines,
and prints divergences bucketed by signature (so one bug isn't reported a
thousand times):

```sh
# 100k generated ads from seed 1
CGO_ENABLED=1 go run ./fuzz/cmd/cafuzz -n 100000

# focus on evaluation semantics, ignore known parser grammar differences
CGO_ENABLED=1 go run ./fuzz/cmd/cafuzz -n 100000 -ignore-parse

# replay a corpus file, one ad per line
CGO_ENABLED=1 go run ./fuzz/cmd/cafuzz -corpus fuzz/corpus/seeds.txt

# inspect a single ad in both engines
CGO_ENABLED=1 go run ./fuzz/cmd/cafuzz -ad '[ a = 1 / 2 ]'

# crash-reproducer journaling (for hunting libclassad crashes)
CGO_ENABLED=1 go run ./fuzz/cmd/cafuzz -n 1000000 -journal /tmp/last.ad
```

`cafuzz` exits non-zero if any divergence is found.

### Coverage-guided — `go test -fuzz`

```sh
# explore from a matching baseline; saves any divergence to testdata/fuzz/
CGO_ENABLED=1 go test ./fuzz -run='xxx' -fuzz='FuzzDifferential' -fuzztime=60s

# re-run a saved finding
CGO_ENABLED=1 go test ./fuzz -run='FuzzDifferential/<hash>'
```

The fuzz target seeds itself only with inputs the engines currently *agree* on
(Go's fuzzer aborts on a failing seed before it mutates), so mutation explores
from green and reports regressions. It fails on `ValueDivergence` and on a Go
engine panic. Parse-only divergences are left to `cafuzz`.

### Regenerating the seed corpus

```sh
CGO_ENABLED=1 go run ./fuzz/cmd/genseeds > fuzz/corpus/seeds.txt
```

## Divergence categories

- **value-divergence** — both parsed; an attribute evaluated differently. The
  prize.
- **parse-divergence** — exactly one engine accepted the input. Often reflects
  known grammar differences; `-ignore-parse` suppresses these.
- **go-panic** — the Go engine panicked. Always a Go bug (it should return
  `error`/`undefined`, never panic).
- **encoding-error** — an engine emitted something the canonical decoder
  rejected (harness/engine bug).

## Findings from the first runs

These were found within seconds and confirmed independently against the
`classad_eval` reference CLI — they are real engine differences, not harness
artifacts. **All of the rows below have since been fixed in the Go engine** (see
the `classad:` / `parser:` commits and `classad/cpp_parity_test.go`):

| Input | Go (before) | C++ | Nature |
|-------|----|----|--------|
| `1 / 2` | `real(0.5)` | `int(0)` | Integer division for integer operands. |
| `"B" < "a"` | `true` | `false` | Case-insensitive string ordering. **Silent wrong boolean.** |
| `true < false` | `error` | `false` | Booleans are orderable numbers (1/0). |
| `6/3`, `1.0/2.0` | parse error | parses | Lexer dropped a char after `/` adjacent to a digit. |
| `false && error` | `error` | `false` | Three-valued short-circuit logic. |
| `{}[0]` | `undefined` | `error` | Out-of-range list subscript is an error. |
| `1 ? a : b` | `error` | `a` | Ternary condition coerces a number's truthiness. |
| `0.1 + 0.2 == 0.3` | `true` | `false` | Reals compare with exact (not epsilon) equality. |
| `round(2.5)` | `3` | `2` | Round half to even. |
| `string(1.5)` | `"1.5"` | `"1.50…E+00"` | Reals stringify as `%.15E`. |

…and many more found by driving the fuzzer iteratively (see the `classad:`
commits and `classad/cpp_parity_test.go`): boolean numeric coercion, per-function
undefined handling, `ifThenElse`/`?:` condition coercion, exact real equality,
mismatched-type `==` → error, `quantize`/`pow` typing, integer division/modulo
overflow, `int()`/`real()`/`floor()`/`pow()`/… string parsing, `member()`
`==`-semantics, `=?=`/`=!=` on lists → error, undefined-dominates-error argument
precedence (`substr`/`strcmp`/`member`), real division by zero (`+Inf`→error,
`-Inf`/`NaN`→value), large-integer `round`/`floor` precision, list coercion via
the sink unparse format, and removing the non-reference `length()`.

The bucketed `cafuzz` survey drove the semantic-divergence rate (random ads,
`-ignore-parse`) from ~3800/5000 down to ~50/5000 (~98.7%).

### Known remaining divergences

Essentially all of the residual ~1% trace to **one architectural difference**:
the Go engine evaluates list elements **eagerly** and stores evaluated values,
whereas the reference engine keeps a list as its *unevaluated* expression list
and evaluates elements lazily in their defining scope. This surfaces as:

- **String coercion of lists with non-literal elements** —
  `string({1, 1+1})` is `"{ 1,1 + 1 }"` in the reference (the source
  expression) but `"{ 1,2 }"` here (the evaluated value). Lists of literals /
  scalars do match; `string`/`strcat`/`strcmp`/`stricmp`/`quantize` of such
  lists are handled.
- **List/nested-subscript element evaluation** — an element that is a reference
  or compound expression can evaluate to a different undefined/error/value
  under eager vs. lazy/scoped evaluation.
- **`string()`/… of a nested classad** — likewise unparses attribute
  expressions; still left erroring.

Closing this fully would require representing list values as lazy expression
lists in the Go evaluator — a larger change than the targeted semantic fixes
above.

Two further intentional non-fixes:

- **`length(...)`** *was* a Go alias for `size()`; the reference engine has no
  such function, so it has been removed (now an error), matching the reference.
- **Integer-literal overflow** (`9223372036854775808`): the reference engine
  silently evaluates it to `0` (a libclassad bug, reported upstream); Go rejects
  it at parse time. Not mirrored.

## Extending

- **More operators/functions**: extend the generator in [`gen/gen.go`](gen/gen.go)
  (`binops`, `fns1/2/3`). Keep additions deterministic — no `time`/`random`.
- **A target attribute / matchmaking**: the differ currently evaluates a single
  ad's attributes. To fuzz `TARGET.x` matchmaking, add a second ad to the shim
  (`MatchClassAd` is already linked) and to `canon.FromGoClassAd`.
- **New value kinds**: add a `Kind` in [`canon/canon.go`](canon/canon.go) and emit
  it from both `shim.cc` and `canon/govalue.go`.
