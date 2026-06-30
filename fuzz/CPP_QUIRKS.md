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
than being a parse error or a saturated/error value. Reported upstream
previously. The Go engine rejects it at parse time and is intentionally **not**
changed to match.

---

## 2. Infinite loop on a cyclic self-reference reached through a lazy operand — likely bug

```
classad_eval -quiet '[ A0 = 0 ? e : A0 ]' A0        # hangs forever
classad_eval -quiet '[ a = 0 ? e : a ]' a           # hangs forever
```

When an attribute's value reaches a reference to *itself* through a lazily
evaluated operand — the taken branch of a `?:` ternary here — libclassad's
cycle guard never fires and evaluation does not terminate. Contrast with the
direct self-reference, which is detected:

```
classad_eval -quiet '[ A0 = A0 ]' A0                # exception: "failed to evaluate"
```

The Go engine detects all of these as a cyclic reference and returns `error`.
Because libclassad hangs, the differential harness caps each C++ evaluation and
reports a timeout rather than hanging (see `fuzz/README.md`).

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

## 5. Elvis `?:` precedence depends on whitespace between `?` and `:` — surprising

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

Since `" "` and `""` both have size 0, the empty list `" "` ought to be a subset
of `{ "a" }` just like `""` is. The reference disagrees only for the
whitespace-only (but non-empty) string. This is internally inconsistent in
libclassad (`size` says 0, `subsetMatch` iterates as if 1), so the Go engine
deliberately does **not** mirror it — `stringListSubsetMatch(" ", "a")` is
`true` in Go.

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
