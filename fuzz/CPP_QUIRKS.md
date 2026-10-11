# Surprising libclassad behaviors (for upstream reporting)

Differential fuzzing of the Go ClassAd engine against HTCondor's reference C++
`libclassad` (25.11.0, via the `classad_eval` CLI / `classad2` Python bindings)
surfaced the behaviors below. They are recorded here because they are either
likely C++ bugs or surprising-enough semantics that upstream may want to
confirm or change them. The Go engine was *not* changed to mirror the ones
marked **likely bug**; for the others, the Go engine matches C++ (and the
matching is noted).

Each item gives a minimal reproducer for `classad_eval -quiet '<ad>' '<attr>'`.

---

## 1. Integer-literal overflow silently wraps to 0  — likely bug

```
classad_eval -quiet 9223372036854775808
```

An integer literal that overflows `int64` evaluates to `0` (silently), rather
than being a parse error or a saturated/error value: the lexer reads it with
`std::from_chars`, ignores the out-of-range result, and keeps the 0 it started
with. Reported upstream previously. The Go engine now matches it (HTCondor reads
config `$INT`/`$REAL` arguments, submit expressions and job_queue.log values
with this lexer, so `$REAL(10000000000000000000%1)` is `0`). Only a literal with
its `-` lexed as part of it (#14) reaches `INT64_MIN`: `-9223372036854775808`
is `INT64_MIN`, `- 9223372036854775808` and `1-9223372036854775808` are `-0`
and `1`.

---

## 2. Infinite loop on a cyclic self-reference reached through a lazy operand — REPORTED, FIXED UPSTREAM

```
classad_eval -quiet '[ A0 = 0 ? e : A0 ]' A0        # (older libclassad) hangs forever; (fixed) undefined
classad_eval -quiet '[ a = 0 ? e : a ]' a           # ditto
```

When an attribute's value reaches a reference to *itself* through a lazily
evaluated operand — the taken branch of a `?:` ternary here — older libclassad's
cycle guard never fired and evaluation did not terminate. Contrast with the
direct self-reference, which was always detected:

```
classad_eval -quiet '[ A0 = A0 ]' A0                # exception: "failed to evaluate"
```

**Update:** this was reported upstream and fixed — a current libclassad now
terminates and returns `undefined` for the lazy-operand cycle instead of
hanging. The Go engine detects all of these as a cyclic reference and returns
`error`, so the remaining difference is `undefined` (C++) vs `error` (Go) — a
benign spelling difference the differ classifies as a KnownQuirk
(`explainedByCyclicRefQuirk`). On any libclassad that still hangs, the harness
caps each C++ evaluation and reports a CppTimeout instead (see `fuzz/README.md`);
both are non-divergences.

---

## 3. Inconsistent cyclic-reference handling: hang vs. exception vs. error — likely bug

The same logical condition (an attribute that transitively references itself)
produces three different outcomes depending on the syntactic path:

```
classad_eval -quiet '[ A0 = A0 ]' A0                          # exception ("failed to evaluate")
classad_eval -quiet '[ A0 = 0 ? e : A0 ]' A0                  # infinite loop (hang)
classad_eval -quiet '[ A0 = undefined ? {} : A0 ]' A0         # exception
classad_eval -quiet '[ A0 = (A ? pow(t) : 0); t = A0 ]' A0    # undefined (cycle never reached: wrong arity, see below)
```

A single, consistent outcome (cycle ⇒ error) would be less surprising. (Through
the `classad2` bindings / the differential shim, the "exception" cases surface
as an `error` value for the attribute rather than a thrown exception.)

---

## 4. A ternary with an *undefined* condition evaluates BOTH branches — surprising

A `true`/`false` condition evaluates only the taken branch (correct
short-circuit):

```
classad_eval -quiet '[ A0 = 1 ? 5 : A0 ]' A0        # 5  (false branch A0 not evaluated)
classad_eval -quiet '[ A0 = 0 ? A0 : 5 ]' A0        # 5  (true branch A0 not evaluated)
```

But an **undefined** condition evaluates *both* branches. The result is still
`undefined`, and an ordinary `error`-valued branch is absorbed, yet a cyclic
self-reference in either branch surfaces:

```
classad_eval -quiet '[ A0 = undefined ? 1 : error ]' A0     # undefined (error branch absorbed)
classad_eval -quiet '[ A0 = undefined ? {} : A0 ]' A0       # fails to evaluate (cyclic branch evaluated!)
classad_eval -quiet '[ A0 = undefined ? A0 : 5 ]' A0        # fails to evaluate
```

So whether a branch is evaluated depends on whether the condition is
`true`/`false` (one branch) versus `undefined` (both). The Go engine matches
this behavior (a cyclic branch becomes `error`), but it is asymmetric and
surprising.

---

## 5. Elvis `?:` precedence depended on whitespace between `?` and `:` — FIXED UPSTREAM

**Update:** since HTCONDOR-3814 (libclassad 25.14) the lexer skips whitespace
between `?` and `:`, so `10 ? : 2 + 3` is 13 like `10 ?: 2 + 3`; a comment
between them leaves a conditional with no middle, a parse error. The Go engine
follows 25.14. Before it:

An **adjacent** `?:` is a high-precedence postfix operator (it binds tighter
than arithmetic); a **spaced** `? :` binds at low ternary precedence:

```
classad_eval -quiet '[ a = 10 ?: 2 + 3 ]' a        # 13   -> (10 ?: 2) + 3
         # 10   -> 10 ?: (2 + 3)
```

Inserting a space between `?` and `:` changes the parse tree (and the result).
This stems from the lexer fusing only an immediately-adjacent `?:` into a single
`LEX_ELVIS` token. The Go engine matches this, but identical-looking
expressions parsing differently by whitespace is surprising.

---

## 6. version comparison functions are camelCase and case-sensitive — surprising

ClassAd function names are otherwise case-insensitive (`SUBSTR` == `substr`), but
the version-comparison helpers are spelled in camelCase and only match that exact
case:

```
classad_eval -quiet '[ a = versionGT("2.0","1.0") ]' a    # true
classad_eval -quiet '[ a = versiongt("2.0","1.0") ]' a    # error  (lowercase not recognized)
classad_eval -quiet '[ a = version_gt("2.0","1.0") ]' a   # error  (underscore not recognized)
```

So `versionGT`/`versionGE`/`versionLT`/`versionLE`/`versionEQ` work, but only with
that capitalization — inconsistent with every other builtin. (`version_in_range`
and `versioncmp` are the lowercase-friendly ones.)

## 7. `stringListSubsetMatch` treats a whitespace-only list as non-empty — inconsistent with `stringListSize`

A whitespace-only string tokenizes to *zero* elements according to
`stringListSize`, but `stringListSubsetMatch` behaves as though it contains a
(non-matching) element, so the empty-subset shortcut does not apply:

```
classad_eval -quiet '[ a = stringListSize(" ") ]' a                    # 0  (empty list)
classad_eval -quiet '[ a = stringListSubsetMatch("", "a") ]' a         # true  (empty subset)
classad_eval -quiet '[ a = stringListSubsetMatch(" ", "a") ]' a        # false (!!)  -- " " acts non-empty
```

Since `" "` and `""` both have size 0, the empty list `" "` might be expected to
be a subset of `{ "a" }` just like `""` is. The reference disagrees only for a
whitespace-only (or otherwise all-delimiter, e.g. `","`) **non-empty** string,
treating it as a single non-matchable element. This is internally inconsistent
in libclassad (`size` says 0, `subsetMatch` acts as if 1).

The Go engine **mirrors** this behavior for parity: `stringListSubsetMatch` of a
non-empty string that tokenizes to zero elements is `false`, while a genuinely
empty string (or `undefined`) is `true`. The reference value is pinned by
`TestCppQuirks` in `fuzz/oracle/cgo/quirks_test.go`, which evaluates these
inputs in libclassad itself — so if a future libclassad release changes this
(e.g. fixes the inconsistency), that test fails and flags the Go mirror and this
note for revision. It links libclassad, so run it with the build tag:
`CGO_ENABLED=1 go test -tags libclassad ./fuzz/oracle/cgo/`.

## 8. `int()` / `floor()` / `ceiling()` / `round()` of a non-finite or out-of-range real is undefined behavior — likely bug

`convertValueToIntegerValue` (src/classad/value.cpp) turns a real into an
integer with an **unguarded C++ cast**:

```cpp
case Value::REAL_VALUE:
    value.IsRealValue(rvalue);
    integerValue.SetIntegerValue((long long) rvalue);   // UB if !isfinite or |rvalue| >= 2^63

case Value::STRING_VALUE:
    ivalue = (long long) strtod(buf.c_str(), &end);      // same, e.g. int("nan")
```

Casting a `NaN`, `±Inf`, or out-of-`int64`-range `double` to `long long` is
undefined behavior in C++ (C++20 [conv.fpint]/1). The result is **CPU/compiler
dependent**:

| Input | x86-64 (`cvttsd2si`) | AArch64 (`fcvtzs`) |
| --- | --- | --- |
| `int("nan")`  | `-9223372036854775808` (INT64_MIN) | `0` |
| `int("inf")`  | `-9223372036854775808` (INT64_MIN) | `9223372036854775807` (saturates) |
| `int("-inf")` | `-9223372036854775808` (INT64_MIN) | `-9223372036854775808` |
| `int("1e30")` | `-9223372036854775808` (INT64_MIN) | `9223372036854775807` (saturates) |

So the *same* HTCondor source produces different `int("nan")` results on an
x86-64 build vs. an ARM build. It is also reachable through arithmetic
(`int(0.0/0.0)`, `int(pow(10, 1000))`, integer-overflowing products, …), not
just string literals.

The Go engine does **not** mirror this — it cannot portably reproduce UB, and
mirroring one CPU's result would be wrong on another. `floatToInt64` picks a
sane, deterministic, platform-independent behavior (NaN → 0, +Inf → INT64_MAX,
−Inf/underflow → INT64_MIN — i.e. saturation). The differential fuzzer avoids
generating the trigger (the generator no longer emits `"inf"`/`"nan"`/`"1e30"`
literals), because comparing a UB result against any fixed choice is
meaningless. This is a good candidate to fix upstream by guarding the cast
(range-check + `isnan`/`isinf`).

## 9. `=?=` / `=!=` between two lists depends on their internal representation — likely bug

`=?=` (and `=!=`) between two list operands is **error** when both lists have
the same internal representation, but **false** when they differ:

```
classad_eval -quiet '[ a = ({1} =?= {1}) ]' a                              # error
classad_eval -quiet '[ L = splitSlotName("a@b"); a = (L =?= L) ]' a        # error
classad_eval -quiet '[ L = splitSlotName("a@b"); a = ({1} =?= L) ]' a      # false (!!)
classad_eval -quiet '[ a = ({1} =?= split("x")) ]' a                       # false (!!)
```

A `{...}` literal is an `ExprList`, while a list *returned by a function*
(`split`, `splitSlotName`, `splitUserName`, …) is an `SList`. `=?=` compares the
two operands' internal type tags: same tag (two `ExprList`s, or two `SList`s) is
an unsupported list comparison → error, but different tags (`ExprList` vs
`SList`) look like a type mismatch → false. So the result of `X =?= Y` depends
on *how the lists were produced*, not just their contents.

The Go engine has a single list type, so `list =?= list` is consistently
`error` (it never sees a spurious type mismatch). It does **not** mirror this —
reproducing it would mean tagging list values with their origin purely to copy
a representation leak. Go's behavior matches the reference for the same-tag
cases and differs only for the mixed literal-vs-function case.

## 10. The number lexer is strtod-lenient and accepts a doubled leading dot — likely bug

libclassad's numeric-literal scanner is lenient (strtod-based) and accepts a
doubled leading dot that a stricter parser rejects:

```
classad_eval -quiet '[ x = ..5 ]' x     # 0.0   (doubled leading dot)
```

`..5` scans as `0.0`, while `.5.5`, `1..5`, `1.2.3`, `5.`, `..`, and `1.e5` are
all (correctly) rejected. The scanner takes a second `.` after a first one
whatever follows it: before a digit the two begin a real that `strtod` reads as
0, and otherwise the two are a single selection operator, so `a..b` is `a.b`.
The Go lexer now matches both. (Note: `.e5` is *not* a malformed float -- it is
a leading-dot reference to the attribute `e5`, which the Go parser handles.)

## 11. The parser leniently recovers from unbalanced/mismatched brackets — likely bug

libclassad's parser accepts some inputs with unbalanced or mismatched brackets,
recovering to a partial result, where a strict parser reports a syntax error:

```
classad_eval -quiet '[ A = {(0?[0:0)} ]' A     # { 0 }  -- note the mismatched "[" ... ")"
```

The Go parser rejects such input. This is a libclassad leniency, not mirrored;
the differential parser fuzzer allowlists parse disagreements whose input has
unbalanced brackets as CPP_QUIRKS #11.

## 12. Error recovery accepts a dangling binary operator before a ternary ':' — likely bug

Inside a conditional expression, libclassad's parser recovers from a then-branch
whose trailing binary operator has no right operand, treating the malformed
`then` as complete when it reaches the `:`:

```
classad_eval -quiet '[ a = b ? c % : d ]' a    # undefined  -- note the "% :" (modulo with no RHS)
```

The same operator-missing-operand text outside a conditional (`[a = 1 % : 2]`)
is rejected by both engines; only the ternary's error-recovery path accepts it.
A well-formed then-expression never ends in a binary operator, so the Go parser
rejects the input. This is a libclassad leniency, not mirrored; the differential
parser fuzzer allowlists parse disagreements whose input has a binary operator
immediately before a ':' as CPP_QUIRKS #12.

## 13. Old-ClassAd string literals do NO escape processing (backslashes are literal) — surprising

The old-ClassAd wire format daemons advertise (newline-separated, unbracketed
attributes) is lexed by `Lexer::tokenizeStringOld`, which — unlike the new-ClassAd
string lexer (`tokenizeString` / `convert_escapes`) — performs **no** escape
interpretation. Two consequences surprised us, and the *same bytes therefore mean
different things to the old and new lexers*:

- **Every backslash is literal.** In old-ClassAd, `OSIssue = "\S"` (the agetty
  escapes `/etc/issue` carries: `\S \r \l \m`) is the two characters `\` and `S`.
  The new-ClassAd parser instead *drops* an unknown-escape backslash — `[ A = "\S" ]`
  evaluates `A` to `"S"` — and rejects yet others. `\n`, `\t`, and octal `\NNN`
  are likewise kept literal in old-ClassAd, not interpreted.

  ```
  # new-ClassAd: unknown escape drops the backslash
  classad_eval -quiet '[ A = "\S" ]' A          # "S"
  # old-ClassAd (getOldClassAd / SetOldClassAd(true)): kept literal -> "\S"
  ```

- **A backslash immediately before the closing quote escapes it**, so a string
  cannot end in a lone backslash: `"\\"` (backslash, backslash, quote) followed
  by more text is **unterminated** — the second `\` escapes the `"` and the scan
  runs off the end. (`"a\"b"` is `a"b`.) This is the one that caught us out.
  The exception: when that quote is the last character on the line or in the
  input, the string ends there with the backslash as its last byte, so the
  expression `"C:\"` is the string `C:\`. Quoted attribute names (`'...'`) follow
  the same rules with `'`.
- **Adjacent string literals are not concatenated** (`"a" "b"` is a parse
  error), unlike the new-ClassAd lexer, where it is `"ab"`.

This is legacy behavior, not a bug — but a sharp edge. It bit a Go collector: its
ingest lexed forwarded old-ClassAd string values with new-ClassAd escape rules,
so `OSIssue = "\S"` was rejected (`invalid escape sequence \S`), the startd ad was
dropped, and the forward connection reset (`condor_write(): Socket closed`). The
Go engine's old-ClassAd path (`classad.ParseOld` and the `collections` ingest fast
path) now matches `tokenizeStringOld` exactly, and `FuzzParseOldClassAdDifferential`
guards it against regression.

## 14. A `-` before a number is part of the literal, which changes how the number lexes — surprising

Where an operand is expected (not after a value, an identifier, `.` or a
closing bracket), `Lexer::tokenizeNumber` takes a `-` directly followed by a
digit (or by `.` and a digit) as part of the number. The old-ClassAd-semantics
check that rejects a leading zero (octal or hex) looks at the first character,
which is then the `-`, and the text is read in base 10 by `std::from_chars`:

```
classad_eval -quiet '[ a = -00; b = -07; c = -0x10; d = 00 ]'    # 0, -7, 0, and d fails to parse
classad_eval -quiet '[ a = 1 - 00 ]'                             # parse error: binary minus, then 00
```

So `$INT(!-00)` in an HTCondor config is `1`. The signed literal is a primary
expression: `a ?: -1` parses (the elvis right operand is a postfix expression)
and `-5[0]` is `(-5)[0]`. A hex integer followed by `.` and a digit is a real
that `strtod` reads as a hex float (`0x1.8` is 1.5). The Go engine matches all
of this: its lexers return such a `-` as the NEG token.

## 15. Strings are bytes — surprising only next to Go

libclassad never decodes UTF-8: a string literal keeps every byte, comparison
is `strcmp`/`strcasecmp` on bytes, an octal escape (`\351`) is one byte, and
only C `isspace` characters separate tokens (`1\xc2\xa0+1` is a parse error).
The Go engine used to read source as UTF-8 and replace each byte that is not
valid UTF-8 with U+FFFD, so `"\x7f\xb4" > "\x7f\x84"` was `false` (both
strings became `"\x7f\ufffd"`) where libclassad says `true`. It now keeps the
bytes, in literals, in its case-mapping functions and through unparse.
(It still maps non-ASCII letters in `toUpper`/`toLower`/`stricmp`/
`stringListIMember`, where libclassad's C functions leave them unchanged, and
its regexp functions use RE2, which rejects invalid UTF-8.)

## 16. A full-expression parse returns early from a top-level conditional — likely bug

`ClassAdParser::ParseExpression(text, full=true)` (and so HTCondor's
`ParseClassAdRvalExpr`) checks that the input is exhausted only on the path
for an expression that is not a conditional. After `a ? b : c` it returns at
once, ignoring whatever follows -- even a token that does not lex:

```
# HTCondor config (condor_config_val 25.14.1)
X = $INT(1 ? 2 : 3 4)                 # 2
Y = $INT(1 ? 2 : 3 "unterminated)     # 2
Z = $INT(1 + 2 4)                     # error: does not evaluate to an integer
W = $INT(0;)                          # error: does not evaluate to an integer
```

`classad.ParseExprOld`, the Go counterpart of `ParseClassAdRvalExpr`, mirrors
this; `classad.ParseExpr` does not. Both reject a trailing `;`.

## 17. A `[ ]` body of only `;` is a parse error, and an empty name is refused — surprising

`parseClassAd` allows extra `;` before, between and after attributes, but after
a leading `;` it requires an attribute, so `[;a=1]` and `[a=1;;]` parse while
`[;]` and `[ ; ]` do not. `ClassAd::Insert` refuses an empty attribute name, so
`[''=0]` fails to parse. The Go parser matches both.

## Not mirrored: parser and evaluation depth limits (HTCONDOR-3814)

libclassad 25.14 refuses to parse an expression nested deeper than 500
recursive-descent frames (`MAX_PARSER_DEPTH`: each parenthesis, unary operator
and postfix operand counts) and fails an evaluation that nests more than 400
operations, references, calls or lists (`MAX_CLASSAD_RECURSION`) or takes more
than 10,000,000 node evaluations. `!!!...!0` with 498 `!` does not parse; with
about 400, together with a few binary operators, its evaluation fails. The Go
engine has no parser limit and bounds evaluation at depth 2000 (to error), so
such inputs still differ. `FuzzParseExprOldDifferential` skips inputs longer
than 256 bytes for this reason.

## Observed (reasonable) semantics, recorded for completeness

These are not bugs, but were non-obvious and are now matched by the Go engine:

- **Unknown function ⇒ error without evaluating arguments.** `0 =!= A((x))`
  is `true` (the unknown `A` errors before its cyclic/erroring argument is
  evaluated).
- **Wrong-arity call ⇒ error without evaluating arguments.** `pow(x)` is error
  without evaluating `x`; `sum()`, `avg()`, `min()`, `max()` (0 args) are error.
- **Attribute selection over a list projects element-wise.** `{[A=1],[A=2]}.A`
  is `{1, 2}`; a non-ad element projects to `error`, a missing attribute to
  `undefined`.
- **A list value is its unevaluated `ExprList`.** `size({C})` counts elements
  (`1`) without evaluating `C`; string-coercion unparses the source expressions
  (`string({1, 1+1})` is `"{ 1,1 + 1 }"`).
