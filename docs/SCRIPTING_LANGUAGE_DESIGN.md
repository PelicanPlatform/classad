# ClassAd Script — design

**Status:** partly implemented. The scalar register machine, the binding model, `if`/`while`, two
optimizer passes and the O2 reduction law are on `main`; the vector path for scripts, the per-record
fault model and the rest of the optimizer are not. **§11 is the ledger** — what exists, with
citations; the decisions taken while building that are not in §9; and the claims in §§2-6 that
building has since made false. Read it before trusting the prose. §12 says what is left and in what
order, and §13 states the one piece nothing here covers.

This document exists so the semantics and the compilation model are argued before a parser or a code
generator is written — which is still how the work is going: there is no surface syntax yet, by
design (§12, item 7).

**What it is:** a small imperative language whose *expressions* are ClassAd expressions and which
adds statements — assignment, conditionals, iteration, functions. Python-shaped surface, ClassAd
expression grammar underneath. The point is to script ClassAds; this document does not argue for a
particular application of that, and deliberately makes no case against any existing language.

**Two execution contexts, and the second one shapes everything.** A script runs either

- **scalar**: one ad, one result — the degenerate case; or
- **vector**: a batch of *n* records at a time, against a `ColumnSource`, inside the collections
  scan loop (`collections/vm/vecexec.go:252-259`).

Almost every hard decision below — what a fault is, how control flow is represented, what a
register costs — follows from the vector context. Where a reader of an earlier draft would expect
the scalar answer, §2.2 and §3 say why it was rebuilt.

**Citations.** Every claim about existing behavior is cited `path:line`, relative to this repository
(`/Users/bbockelm/projects/golang-classads`). Separately,
[`NOTES_TRANSFORM_LANGUAGE_FINDINGS.md`](NOTES_TRANSFORM_LANGUAGE_FINDINGS.md) records verified
findings about HTCondor's C++ transform and config-macro languages; they are not inputs to this
design.

---

## 1. The shape of the thing

Four properties, in the order they constrain the design.

**1. Expressions stay total; statements do not.** ClassAd has no exceptions and no failures:
`UNDEFINED` and `ERROR` are values that every operator and function accepts and returns
(`classad/evaluator.go:1455-1499`, `:1319-1358`). An `if` cannot be total — it must choose. §2.1
confines partiality to the statement layer and enumerates where it can arise; §2.2 decides what it
*is* in a batch.

**2. In the vector context there is no branch to take.** The vector executor has no branch opcode.
`OpShortAnd`/`OpShortOr` are deliberate no-ops, and `OpJmpIfNotUndef` (Elvis) and `OpEvalNode`
decline the whole evaluation (`collections/vm/vecexec.go:316-323`), because a batch's records
disagree about which way to go. Control flow therefore has to be **predication**, not jumps — the
SIMT model — and the mask representation it needs already exists as two bitplanes
(`collections/vm/vecmask.go:8-14`). §2.3.

**3. The whole script compiles to one instruction stream.** Not a statement walker over
per-expression programs. Statements, control flow and locals are all in the IR, so the optimizer can
see across them. §5.

**4. A register is a whole batch vector, so register allocation is a memory-bandwidth problem.**
This is the fact that justifies a real optimizer — SSA, liveness, register assignment, CSE — in a
system where a single ClassAd expression is "a handful of AST levels deep"
(`collections/vm/compile.go:26-27`). The depth of the expression is irrelevant; the *size of a live
value* is what matters. §6.1 does the arithmetic.

---

## 2. Semantics

### 2.1 Two layers: total expressions, partial statements

The governing rule:

> **ClassAd totality is preserved at the expression layer without exception. Partiality is
> introduced only at the statement layer, in a small enumerated set of places.**

Expression evaluation inside a script produces the *same value the tree-walking evaluator produces
for the same expression in the same scope*, bit for bit, including `UNDEFINED` and `ERROR`
(`classad/evaluator.go:396-470`). No script construct changes how `+`, `&&`, `=?=`, `?:`, a
function call, or a reference resolves. This is not a nicety: it is what lets the expression layer
inherit the existing C++ differential oracle (§7.1) and what keeps "an expression is lowered in
exactly one place" (`collections/vm/opcode.go:4-9`) literally true.

The fault classes:

| # | Class | Trigger |
|---|---|---|
| F1 | `condition` | an `if`/`elif`/`while` condition that is not boolean-equivalent (§2.2) |
| F2 | `target` | a dynamic assignment or `del` target that is not a valid attribute name (§2.4) |
| F3 | `trips` | a record exceeding its loop trip limit or call depth (§3) |
| F4 | `refused` | a construct the host's mode forbids, e.g. a nondeterministic built-in (§3.3) |

Producing an `UNDEFINED` or `ERROR` value never faults, and storing one into an attribute never
faults.

There is a fifth failure that is deliberately *not* a fault: exhausting the **batch element-step
budget** (§3.1) aborts the whole execution and is reported to the host as an error, not as a
per-record result. It is an operational limit, not a property of any record.

### 2.2 Conditions, re-derived for the batch

An earlier draft of this document ruled that an `UNDEFINED` condition is a **fault that stops the
script**, and argued it this way: a *constraint* ranges over many ads, so "this ad does not match"
is a legitimate answer; whereas "a script runs once over one ad, so silence is a transform that
quietly did nothing."

**The vector context is the many-records case that argument contrasted against.** A script in the
scan loop runs per record over a whole block — 8192 of them
(`collections/colblock.go:98`: `colGroupMaxRows = 8192`). Aborting a batch because one record's
condition was `UNDEFINED` would be unusable: a single malformed record would take out 8191 good
ones, and on a real table some records are always missing some attribute. The conclusion inverts.
The *motivation* — never silently treat absence as false — survives, but it has to be expressed
per record rather than per script.

**Decision.** A condition is classified by boolean equivalence, the same `boolEquiv` predicate the
evaluator uses, which accepts a boolean or a nonzero integer/real
(`classad/evaluator.go:1704-1714`). A record whose condition is not boolean-equivalent is
**faulted**: it is cleared from the live mask for the remainder of the script, and its result is
`ERROR`.

| Condition value for record *i* | Record *i* |
|---|---|
| `true`, or nonzero int/real | takes the branch |
| `false`, or zero int/real | does not take the branch (`elif`/`else` considered) |
| `UNDEFINED` | **F1-faulted**: cleared from the live mask, result `ERROR` |
| `ERROR` | **F1-faulted**: cleared from the live mask, result `ERROR` |
| string, list, ad | **F1-faulted**: cleared from the live mask, result `ERROR` |

Four properties this buys, each of which was a requirement:

1. **No silent falsiness.** A faulted record's result is `ERROR`, which is distinguishable from
   `false` by every consumer: `Vec.IsTrue` requires `VsBool` with `I == 1`
   (`collections/vm/vecexec.go:648-661`) and `CountTrue` counts only those (`:668`). A misspelled
   attribute name shows up as a column of `ERROR`, not as a column of non-matches.
2. **It is free.** The representation already exists. In data form the state is `VsError`
   (`collections/vm/vecexec.go:56`); in mask form it is the `(hi,lo) = (1,1)` encoding `mError`
   (`collections/vm/vecmask.go:32-37`). Faulting a record is clearing one bit from the live mask and
   setting that record's state.
3. **It is the existing left-biased logic, not a new rule.** `planeAnd`/`planeOr` are already
   total over all sixteen state pairs and verified against the tree-walking evaluator
   (`collections/vm/vecmask.go:25-29`), so an `ERROR` lane propagates through downstream logic
   without any special case.
4. **Scalar is the degenerate case and stays diagnosable.** With *n* = 1, "fault the record and
   continue" and "fault the script" coincide: the one record is the whole result. So the scalar
   context keeps the strong behavior — the script yields `ERROR` and a diagnostic — without a second
   rule.

**What the host receives.** Not a bare result vector. A vector, a `faulted` mask, and a *fault
report*: for each distinct fault **site** (instruction, hence source position), the class, the count
of records faulted there, and the first faulting record's index. Per-site aggregation rather than
per-record diagnostics is what keeps this affordable at 8192 records, and it is the thing that
preserves the original motivation: an operator sees "F1 at line 14: 8192 records, undefined
condition" rather than a silently empty result.

**What it costs, and the compensation.** A faulting condition is still reachable only by writing one
that is already ambiguous, and the fixes are local, visible, and total:

- `x =?= undefined` — the identity operator is total for scalars, undefined and error, and always
  returns a boolean (`classad/evaluator.go:1319-1358`; it yields `ERROR` only for list and ad
  operands). `is` is a keyword synonym (`parser/bytelexer.go:330-332`), so `MY.X is undefined` reads
  naturally and never faults.
- `isUndefined(x)` / `isError(x)` — `classad/evaluator.go:1749`. Both are strict, so both compile to
  `OpCallFunc` (`collections/vm/opcode.go:51-66`) and vectorize elementwise
  (`collections/vm/vecexec.go:538-581`).
- `x ?: fallback` — the Elvis operator collapses undefined to a default
  (`classad/evaluator.go:781-791`). Note it is currently *not* vectorized
  (`collections/vm/vecexec.go:316-323` declines `OpJmpIfNotUndef`), which §5.2 fixes by giving it a
  predicated lowering rather than a jump.

**What would change my mind:** evidence that consumers want a faulted record to be *excluded* rather
than reported as `ERROR` — i.e. that the useful default is "drop it from the result set". That is a
one-line change in how the host reads the `faulted` mask and does not disturb anything else, so it
can stay a host policy rather than a language decision.

### 2.3 Control flow is predication, not branching

The consequence of §1's point 2, and the architectural heart of the vector path.

**Representation.** Execution carries a **live mask** — one `(hi,lo)` bitplane pair over the batch
(`collections/vm/vecmask.go:8-14`) — plus a stack of enclosing live masks. The mask stack's depth is
the static predication depth, so it is bounded at compile time.

**Lowering.**

- `if c:` — evaluate `c` into a mask register `m`; the then-region runs under `live & m`; the
  else-region under `live & ~m & ~faulted_by_c`; reconvergence pops back to `live`. Records whose
  condition faulted are in neither region.
- `while c:` — a loop header recomputes `c` and ANDs it into the loop's live mask; the loop exits
  when that mask is empty. "Empty" is one popcount per 64 records
  (`collections/vm/vecexec.go:663-667`), not a per-record test. Different records leave at different
  iterations, which is exactly the SIMT behavior.
- `break` clears the record's bit from the loop's live mask; `continue` clears it for the remainder
  of the body; `return` clears it for the remainder of the function and commits the value.

**Assignment under predication is a blend, not a write.** `x = e` inside a predicated region must
leave non-live lanes untouched: `x' = select(live, e, x)`. In SSA this is precisely a phi at the
reconvergence point; in mask form it is two plane blends, and in data form a masked copy. **This is
the one genuinely new runtime primitive the vector executor needs** beyond what exists today, and it
is small.

**Scalar execution is the same IR with a uniform mask.** With *n* = 1 every mask is one bit, so the
optimizer may lower a predicated region back to an actual branch, because skipping is free when the
mask is uniform. One IR, two lowerings — and the scalar lowering is what the differential oracle
compares the vector one against (§7.3), which is the same bargain the current executor already
strikes: the scalar interpreter is "a per-record reference to test this one against on the same
Program" (`collections/vm/vecexec.go:19-23`).

**Where the vector path declines.** It keeps the existing discipline — return `ok=false` and let the
caller fall back to per-record scalar execution, never a partial answer
(`collections/vm/vecexec.go:261-278`, with the per-block fallback at `collections/colvec.go:454-463`).
Declined constructs:

- any ad **mutation** (§2.4), because the vector context has records, not an ad to mutate;
- `for x in <list>` where the list is **per-record**, because the records' lists have different
  lengths and the iteration space is ragged. A `for` over a `Const` list is fine, and is in fact
  fully unrollable at compile time (§6.8);
- anything reaching `OpEvalNode`, which `VecEval` already refuses wholesale
  (`collections/vm/vecexec.go:269-272`).

### 2.4 Binding model, mutation, and case-insensitivity

Unchanged from the earlier draft; the reasoning does not depend on the target.

**Two namespaces, one resolution order.**

1. **Script locals.** Created by assigning a bare name. **Case-insensitive**, folded with the same
   `strings.ToLower` the ad uses for attribute lookup (`classad/classad.go:204-206`).
   Function-scoped, not block-scoped: a name assigned anywhere in a function body is that function's
   local throughout, as in Python. No `global`, no `nonlocal`.
2. **Ad / record attributes.** Reached only through an explicit scope: `MY.Attr`, `TARGET.Attr`, or a
   subscript on the subject handle.

Locals are case-insensitive because the alternative is two rules in one expression: with
case-sensitive locals, `x = Owner` then a read of `X` would be an error while `owner` resolves. One
fold, everywhere.

**A bare-name assignment always writes a local and never writes the ad. Writing the ad always names
a scope.** Reading a bare name finds the local if one exists, and otherwise resolves exactly as the
ClassAd evaluator resolves an unscoped reference — current ad, then up the parent chain, then the
target ad under old-ClassAd matchmaking semantics (`classad/evaluator.go:509-543`). So every
existing expression idiom keeps working unchanged. Shadowing is possible but only by *writing* a
local with that name, which is Python's own rule.

**Mechanism, and the one evaluator change required.** The local frame is installed via the existing
custom-resolver hook, `Evaluator.SetResolver` (`classad/vm_support.go:74-76`): the resolver checks
the local frame first and otherwise delegates to ordinary ClassAd-scope resolution. That hook
currently *replaces* scope resolution entirely (`classad/evaluator.go:483-490`), so there is no way
to delegate back. **The single new addition to `classad/vm_support.go` is a
`ResolveRefIgnoringResolver` hook** — resolve this reference as if no custom resolver were installed.
It adds no semantics and it is the only change to `classad/` the whole design needs. In the vector
context it is not used at all: references come from the `ColumnSource`
(`collections/vm/vecexec.go:296-300`).

**Dynamic attribute access is ordinary ClassAd subscripting.** The scalar subject is bound as an
ad-*valued* local (`self`), so `self["GPUMem_0"]` is already fully specified: subscripting a ClassAd
value with a string key evaluates that attribute (`classad/evaluator.go:930-940`). Dynamic *write*
needs a statement form, `self[<expr>] = <expr>`, because expressions cannot assign. An F2 fault
covers a target that is not a string, is a reserved word, or is empty (`ast/ast.go:236-272`), rather
than a silently-dropped write.

**Mutation is scalar-context-only, in place, on a working copy.** The host hands the interpreter a
copy; the script mutates it; the host commits or discards. A script containing a mutation is simply
declined by the vector executor (§2.3).

### 2.5 The lazy-list aliasing hazard, and why assignment to a local is strict

A concrete soundness trap specific to this engine, and the reason local assignment cannot be lazy.

A list literal evaluates *lazily*: the value retains the element expressions and a **live pointer to
the scope ad**, materializing on demand:

```go
return Value{valueType: ListValue, list: &listData{exprs: exprs, scope: e.classad, depth: e.depth}}
```
— `classad/evaluator.go:793-806`, with `listData` documented at `classad/evaluator.go:63-79` and
on-demand materialization at `:226-240`. Meanwhile `ClassAd.Insert` mutates the ad in place
(`classad/classad.go:498-523`).

So with naive semantics:

```
items = {MY.A, MY.B}      # lazy over the live ad
MY.A = 99
print(items)              # would materialize as {99, ...}
```

The local's value would change because the ad changed underneath it. Wrong under any reading,
invisible, and untestable against anything.

**Decision: assignment to a script local is strict.** Any list value crossing into a local is
materialized to an eager `[]Value` (`classad/evaluator.go:122-125`) at the moment of assignment. An
ad-valued local is deep-copied unless it *is* the subject handle, which aliases deliberately and is
the one documented exception. The cost is zero for scalars, which is what locals overwhelmingly
hold.

Related, and relevant to §6.6: ClassAd reads in the scalar engine are **not pure** — resolution
writes a cycle marker into `ad.evaluating` (`classad/evaluator.go:583-592`) and the attribute list
sorts lazily (`classad/classad.go:327`). Consequences: a `*ClassAd` must never be shared across
goroutines, so a scalar script execution owns its subject ad exclusively; and `attrs(self)` must
return a materialized snapshot, so `for name in attrs(self): del self[name]` is well-defined.

### 2.6 Functions

```
def over_cap(requested, cap):
    if requested is undefined:
        return cap
    if requested > cap:
        return cap
    return requested
```

- Parameters are locals; call is by value, with §2.5's forcing rule applied to each argument.
- `return` with no operand returns `undefined`. Falling off the end returns `undefined`.
- **No closures, no default arguments, no `*args`, no keyword arguments, no nested `def`.**
- Recursion is permitted and bounded per record by the call-depth limit (§3.2), not forbidden. A
  non-recursive function is inlined by the optimizer (§6.8), which is what lets liveness and CSE see
  across the call.
- A function may read and write the subject ad in the scalar context, so it is not required to be
  pure.

**Host-registered functions.** The host may register additional strict built-ins; the registration
must be declared to the compiler so arity checking matches the evaluator's
check-arity-before-evaluating-arguments rule (`classad/evaluator.go:1814-1821`). Two guards:

1. Registering a name already in `functionArity` (`classad/evaluator.go:1744-1778`) is **refused**,
   not shadowing. Shadowing a built-in would fork expression semantics.
2. A registered function must satisfy `classad.StrictFunction`'s contract — a pure function of its
   argument values (`classad/vm_support.go:235-241`) — or it is refused in the vector context,
   because `callFuncVec` applies it elementwise through `classad.ApplyFunction` and a
   scope-dependent or nondeterministic function has no defined meaning there
   (`collections/vm/vecexec.go:516-537`).

**Script-only functions** — `attrs(<ad>)` returning a materialized list of attribute-name strings,
and `exprof(MY.Attr)` returning an attribute's bound expression for splicing — are callable only in
script position. They must be rejected inside a `defer`-quoted expression (§2.7), because such an
expression is stored into an ad and later evaluated by a plain ClassAd evaluator that has never
heard of them, which would silently yield `ERROR`. `exprof` needs no new mechanism: it is the
existing `lookupBoundExpr` (`classad/evaluator.go:1683-1702`).

### 2.7 Storing expressions: `defer` and `${...}`

Two assignment forms, because a ClassAd attribute can hold either a value or an unevaluated
expression:

- **`MY.Attr = <expr>`** evaluates now and stores the value as a literal. The default, because `=`
  evaluating its right-hand side is what every Python programmer expects, and because a stored
  literal cannot silently change meaning later.
- **`MY.Attr = defer <expr>`** stores the expression unevaluated.

Inside a `defer`-quoted expression, **`${ <script-expr> }` evaluates at script time and splices the
result as a single AST node.** Splicing a *tree* is parenthesized by construction, so operator
precedence cannot capture it, and a spliced string value becomes a properly escaped string literal
(`ast/ast.go:49`). Building an expression by string concatenation is never necessary.

```
MY.Requirements = defer (TARGET.HasDocker && ${exprof(MY.Requirements)})
```

`defer` is a soft keyword, recognized only as the first token of an assignment's right-hand side.
*Alternatives considered:* a `setexpr(ad, "Attr", ...)` built-in (hides the distinction in a call and
cannot be checked statically); backtick quotation (reads as shell); making `=` store expressions and
adding `:=` for evaluation (inverts Python's expectation).

Both forms are scalar-context-only, since they are mutations (§2.3).

### 2.8 Everything checkable is checked at compile time

Compiling a script fully resolves, with no ad and no `ColumnSource` present:

- every expression parsed and lowered to instructions;
- every function name and arity checked against `functionArity` (`classad/evaluator.go:1744-1778`)
  plus the host's registrations, using the evaluator's check-arity-before-arguments rule
  (`:1814-1821`);
- every static assignment target checked as a valid attribute name (`ast/ast.go:236-272`);
- `break`/`continue` outside a loop, `return` outside a function, duplicate parameter names rejected;
- script-only functions rejected inside `defer`-quoted expressions (§2.6);
- budgets required to be present (§3);
- in deterministic mode, nondeterministic built-ins rejected (§3.3);
- predication depth and mask-stack depth computed, so neither can overflow at run time.

Compilation deliberately proves nothing about values: it cannot know whether `MY.Owner` will be
defined. F1 is the runtime half of the same contract.

---

## 3. Budgets and determinism, re-derived for the batch

The expression engine is *total*: expressions are finite trees, cycles are detected and resolved to
`ERROR` (`classad/evaluator.go:572-600`), and recursion is capped at `maxEvalDepth = 2000`
(`:368-370`). There is no instruction budget, wall-clock limit, or `context.Context` anywhere in
`collections/vm` — grepping for them returns nothing. Totality is why none were needed. Loops and
recursion destroy that invariant, so bounds are part of the language definition rather than
hardening.

An earlier draft specified a flat per-script step budget. In the vector context "a step" is
ambiguous: one instruction dispatch does *n* records' work. Per-record and per-batch budgets are
different things and both are needed.

### 3.1 Per-batch: an element-step budget (aborts, does not fault)

One unit per instruction executed **times the batch width at that point** — so the unit is an
*element-step*, and compaction (§6.4) genuinely reduces consumption rather than hiding it. This is
the real cost model: it is proportional to work done, it is deterministic, and it is the same number
whether a script is executed scalar or vector, which makes it an oracle-friendly quantity (§7.3).

Exhaustion **aborts the execution** and is reported to the host as an error, not as a per-record
`ERROR`. It is a statement about the script, not about any record. A script that exhausts it would
have produced the same result at any batch size, so a host must not retry it scalar.

### 3.2 Per-record: loop trips and call depth (fault, do not abort)

- **Loop trip limit**, counted per record. A record exceeding it is **F3-faulted** — cleared from the
  loop's live mask with result `ERROR` — rather than killing the batch. The SIMT loop then terminates
  naturally when every record has either finished or faulted, which means a runaway loop costs one
  record's correctness and not the scan's.
- **Call depth**, counted per record for the same reason. Separate from `maxEvalDepth`, which bounds
  expression nesting rather than script recursion.

Making these per-record is what keeps a divergent loop usable: the alternative is that one pathological
record determines the fate of 8191 others.

### 3.3 No wall clock; deterministic mode

**No wall-clock limit.** The reflexive answer to "loops in a scan" is a timeout, and it is refused
for one reason that outweighs the rest: a wall-clock bound makes the language **nondeterministic**.
The same script over the same records would succeed or fault depending on machine load, which
destroys reproducible faults and destroys differential testing (§7), the one discipline this
codebase enforces. The element-step budget is deterministic and is a sound proxy for time, because
every step is a value operation with no I/O and the regex built-ins use Go's `regexp`
(`classad/functions.go:8,907`), which is RE2 and linear. A host that genuinely needs a wall-clock
kill implements it *outside* the interpreter as a supervisory abort, reported as a host error and
never as a script fault.

**Deterministic mode** is the default. The engine already knows which built-ins are not functions of
their arguments: `nonStrictFunctions` lists `random`, `time` and `formatTime` as nondeterministic
and `StrictFunction` refuses exactly that set (`classad/vm_support.go:195-241`). Calling one is an
F4 fault, and `CurrentTime` is folded once at compile time to a single injected "now" — not a new
mechanism, since `vm.Compile` already does precisely this so that "a single now is baked into both
the program and its index probes" (`collections/vm/query.go:17-28`, `classad/vm_support.go:129-139`).

Determinism is a prerequisite for every oracle in §7, not a feature.

---

## 4. Syntax

Python-shaped statements, ClassAd expressions. Blocks are introduced by `:` and delimited by
indentation. The work is in the lexical contract, because ClassAd's token set was not designed to sit
next to significant whitespace; each rule below resolves a specific collision.

### 4.1 Significant whitespace is the right choice here, for a ClassAd-specific reason

Braces are not available. `{...}` is a ClassAd **list literal** (`parser/classad.y:264-265`:
`'{' opt_expr_list '}'` → `ast.ListLiteral`; evaluated at `classad/evaluator.go:793-806`) and
`[...]` is a ClassAd **ad literal** (`parser/classad.y:80-88`: `'[' attr_list ']'`). A
brace-delimited block syntax would overload the one token that already means "list" in every
expression on every line. Indentation uses a lexical channel ClassAd does not use at all: newline is
currently plain whitespace (`parser/bytelexer.go`, `skipTrivia`), so promoting it to a token takes
nothing away.

### 4.2 Lexical rules

1. **Logical lines.** A `NEWLINE` token ends each logical line. This is a change: `skipTrivia`
   currently treats `\n`, `\r`, `\f`, `\v` as whitespace alongside spaces and tabs.

2. **Implicit line joining.** A newline inside an unclosed `(`, `[` or `{` emits no `NEWLINE`. One
   depth counter covers all three, which is necessary because ClassAd's `[` is both
   ad-literal-open and subscript-open — the same token either way (`parser/classad.y:57`:
   `%left '.' '[' '('`). This is what lets a multi-line ad literal, with its `;` separators, appear
   as an assignment's right-hand side:

   ```
   policy = [
       MaxRetries = 3;
       OnFailure  = "hold"
   ]
   ```

3. **Explicit line joining.** A trailing `\` joins lines.

4. **Indentation is spaces only.** A tab in the indentation of a logical line is a lexical error, not
   a width guess. Python 3 rejects *inconsistent* mixing; this rejects tabs outright, because the
   surrounding ecosystem is tab-ambiguous and a silently misread indent changes control flow.
   `INDENT`/`DEDENT` otherwise follow Python.

5. **The block colon is the last token of the header's logical line, and there are no one-line
   suites.** `if MY.Cpus > 4: big = true` is a syntax error; the suite must be an indented block.

   This is the collision that looks fatal and is not. `:` is a ClassAd token at the *lowest*
   precedence — the ternary (`parser/classad.y:41`: `%left '?' ':'`) — so a header's expression can
   contain colons: `if MY.Cpus > 4 ? A : B :` has two. But:

   > **No valid ClassAd expression ends with a bare `:` token.** The only productions containing `:`
   > are the ternary `expr '?' expr ':' expr`, which requires a following expression
   > (`parser/classad.y:41`), and the Elvis operator `?:`, which the lexer emits as a *single* token
   > and which also requires a right operand (`parser/bytelexer.go:59-66`).

   So the header's final `:` is unambiguously the block colon, identifiable with zero lookahead and
   no backtracking. Banning one-line suites is what buys that: with them, `if x: y = a ? b : c` would
   end in `c`, and a "last colon at depth 0" heuristic would pick the ternary's colon instead. One
   small Python deviation removes the entire ambiguity class.

6. **Comments.** `#` to end of line. ClassAd's existing `//` and `/* */` remain legal inside
   expressions (`parser/bytelexer.go`, `skipTrivia`), except that a `/* */` comment may **not** span a
   line break in script context, so it cannot swallow a `NEWLINE` and silently join two statements.

7. **Strings are `"..."` only.** `'...'` is a ClassAd **quoted attribute name**, not a string
   (`parser/bytelexer.go:76-77` → `scanQuotedIdentifier`, which yields an `IDENTIFIER`). This is the
   worst Python-muscle-memory trap in the language, and the parser must emit a dedicated diagnostic —
   *"'...' is a quoted attribute name; use \"...\" for a string"* — wherever a `'`-quoted token
   appears in value position. It is also the escape hatch in rule 8.

8. **Statement keywords are soft keywords.** `if elif else for while def return break continue pass
   del fail in defer` are keywords only in statement-leading position (`in` only inside a `for`
   header, `defer` only leading an assignment's right-hand side), matched **case-sensitively
   lowercase**. Everywhere else they lex as `IDENTIFIER`.

   This is required, not stylistic: ClassAd attribute names are case-insensitive
   (`classad/classad.go:204-206`, `ast/ast.go:415-420`), so `For`, `IF` and `Return` must remain
   usable attribute names. The one residual hazard — an expression statement beginning with an
   attribute named exactly lowercase `for` — is solved by the existing quoted-identifier syntax:
   `'for' + 1`. ClassAd has lived with this shape of problem since `error` became a reserved word
   (`ast/ast.go:252-272`).

9. **No `and` / `or` / `not`.** Use `&&`, `||`, `!`. Adding the word forms would fork the expression
   grammar and break any ad with an attribute so named. Likewise no `None` (`undefined`), no
   `True`/`False` (`true`/`false`, case-insensitive per `ast/ast.go:264-265`), no f-strings, no
   walrus, no comprehensions. `is` keeps its ClassAd meaning, `=?=`
   (`parser/bytelexer.go:330-332`) — close enough to Python's identity operator not to mislead.

   This is the honest cost of "expressions are ClassAd expressions":
   `if MY.Cpus > 4 && !isUndefined(MY.Owner):` is Python-shaped but not Python-flavored.

10. **Augmented assignment** `+= -= *= /= %=` are new tokens. They cannot collide: no ClassAd
    production allows a binary arithmetic operator immediately followed by `=`
    (`parser/classad.y:50-51` require an expression after each), so in expression position they are a
    syntax error, which is correct.

### 4.3 Statements

```
<name> = <expr>              # local assignment (strict, §2.5)
<name> += <expr>             # sugar for <name> = <name> + <expr>
MY.<Attr> = <expr>           # ad write, evaluated            [scalar context only]
MY.<Attr> = defer <expr>     # ad write, expression stored    [scalar context only]
self[<expr>] = <expr>        # dynamic ad write               [scalar context only]
del MY.<Attr>  /  del self[<expr>]                            [scalar context only]
if / elif / else
while <expr>:
for <name>[, <name>...] in <expr>:
break / continue / pass
def <name>(<params>):  /  return [<expr>]
fail <expr>                  # fault this record with a message
```

`fail <expr>` raises a distinguished per-record fault whose payload is the evaluated expression; it
aggregates per site like any other fault (§2.2).

`for` destructuring binds positionally when the element is a list. Iterating a non-list is a fault
reported in its own class (`iterable`); an `UNDEFINED` iterable faults rather than silently
iterating zero times, for the §2.2 reason. A per-record (non-`Const`) iterable is declined by the
vector executor (§2.3).

### 4.4 Examples

Three small scripts, chosen to exercise the features rather than to argue a use case.

**(a) Locals, undefined handling, a derived value.**

```
# Effective memory request, with a floor and an explicit absence case.
want = MY.RequestMemory
if want is undefined:
    want = 2048
if want < 512:
    want = 512
result = want
```

**(b) A loop over a list value, with an accumulator.** Not expressible as a single ClassAd
expression without `sum`/`join` gymnastics, and the shape that makes `while`/`for` worth having:

```
# Largest input file size, ignoring entries that are not numbers.
biggest = 0
for sz in MY.InputSizes:
    if isInteger(sz) && sz > biggest:
        biggest = sz
result = biggest
```

In the vector context this is declined if `InputSizes` is per-record and ragged (§2.3), and unrolled
at compile time if it is `Const` (§6.8).

**(c) Iteration over attribute names, with a dynamic write.** Scalar context only, since it mutates:

```
# Clamp every per-GPU memory attribute to a ceiling.
ceiling = 40000
for name in attrs(self):
    if regexp("^GPUMem_", name):
        if self[name] > ceiling:
            self[name] = ceiling
```

**(d) A function, inlined by the optimizer.**

```
def clamp(x, lo, hi):
    if x is undefined:
        return lo
    if x < lo:
        return lo
    if x > hi:
        return hi
    return x

result = clamp(MY.RequestMemory, 512, 40000)
```

---

## 5. Compilation: one instruction stream for the whole script

### 5.1 One Program, registers rather than a stack

**Decision: the entire script — statements, control flow, locals, function bodies — compiles to a
single instruction stream over a register file.** Not a statement interpreter walking a tree of
per-expression `Program`s.

Reasons, in order:

1. **An optimizer needs to see across statements.** Liveness of a loop-carried local, CSE between
   two statements that both read `MY.Cpus`, and compaction placement across a predicated region are
   all inter-statement properties. A statement walker over opaque expression programs can see none of
   them, which forecloses §6 entirely.
2. **The vector path cannot nest executions cheaply.** Each `VecEval` call resets the scratch stack
   and re-enters dispatch (`collections/vm/vecexec.go:279-281`); a statement walker would pay that
   per statement per batch, and worse, each sub-program's result would have to be copied out of the
   scratch stack before the next one reset it.
3. **Predication is not expressible as a composition of unpredicated programs.** A mask has to be
   threaded through every instruction in a region, which means the region has to be one program.

**Registers, not an operand stack.** The existing stack discipline works because an expression's
stack depth is its own height — 2, for `Owner == "x" && RequestMemory > 2048`. A script has
long-lived locals and loop-carried values with no such bound, and §6.1 shows that *which* values are
simultaneously live is the dominant cost. A stack cannot express "this value is live across the loop
but that one dies here"; registers plus a liveness analysis can.

### 5.2 Expression leaves still lower through the existing compiler

Non-negotiable, and the reason the oracle survives. Every ClassAd sub-expression inside a script is
lowered by the **existing** `compile.go` emitter — the same `OpPushConst`/`OpLoadRef`/`OpBinop`/
`OpUnop`/`OpCallFunc`/`OpEvalNode` repertoire (`collections/vm/opcode.go:17-72`), the same
`StrictFunction` gate for native calls (`collections/vm/compile.go:15-18`), the same
`maxNativeDepth` delegation rule (`:21-31`, `:37-45`). The script compiler adds instructions
*around* expression code; it does not add a second way to compile an expression.

Mechanically: emit expression code into the same instruction buffer with a register destination
rather than a stack push. The cleanest form is an SSA-building pass over the stack-shaped output —
each push defines a fresh virtual register, each pop consumes one — which is a mechanical
stack-to-register conversion and cannot change semantics.

Two existing stack-shaped idioms become explicit IR, and both get better for it:

- `OpShortAnd`/`OpShortOr` + `OpCombineAnd`/`OpCombineOr` (`collections/vm/compile.go:200-213`)
  become a predicated region plus a plane combine. The vector executor's current no-op treatment of
  the short opcodes (`collections/vm/vecexec.go:316-317`) is then no longer a special case: short-
  circuiting is predication, and a `Const` condition makes it a compile-time branch.
- `OpJmpIfNotUndef` (Elvis) becomes `m = isUndefined(left); result = select(m, right, left)`, which
  is why §2.2 can promise `?:` works in the vector context where today `VecEval` declines it
  (`collections/vm/vecexec.go:322`).

### 5.3 The calling convention comes from `callFuncVec`

`callFuncVec` is the closest thing in the tree to the convention the script's `def` calls need
(`collections/vm/vecexec.go:538-581`), and it should be generalized rather than paralleled. What it
establishes:

- **Operands are a contiguous window.** `base := s.depth - f.argc`, args are `s.stack[base:base+argc]`
  (`:539-543`). A register-based call passes a contiguous register range, which keeps that shape.
- **The destination is taken after reserving it**, because `push` may grow the backing slice and
  invalidate an earlier window (`:543-544` — the comment says so explicitly). The register file has
  the same hazard and needs the same discipline.
- **Arguments are forced to data form but `Const` is deliberately not materialized**, because
  `valueData` reads element 0 for a const vector (`:546-555`). §6.5 generalizes this into an
  invariant.
- **All-`Const` arguments collapse the call to one evaluation** (`:556-566`), which for a script
  means a call on literals is constant-folded at run time even when the optimizer did not fold it
  statically.
- **The result is moved into the deepest argument's slot by pointer swap**, "rather than copying the
  payload [to keep] every vector in the pool" (`:573-578`). Under a register file this hand-written
  trick becomes the allocator's job — assigning a result to a dying operand's register is
  **coalescing**, which is the standard thing an SSA allocator does. That the existing code arrived
  at it by hand is evidence the register model is the right generalization.

### 5.4 What a script program carries

Beyond today's `Program` fields — `code`, `consts`, `ops`, `refs`, `nodes`, `funcs`, `readAttrs`,
`expr` (`collections/vm/program.go:19-36`) — a script program adds: the register count and each
register's declared form (§6.5), the local-name to register map (for diagnostics, not for
execution), the predication depth, the fault-site table mapping instruction index to source
position, and the budget trio.

`readAttrs` generalizes to the union over the whole script, which keeps the store's planner
(`collections/vm/compile.go:215-222`) and `Native()` (`collections/vm/wirematch.go:12`) working
unchanged.

---

## 6. The optimizer

### 6.1 The cost model: a register is a batch, so allocation is a bandwidth problem

An earlier draft rejected SSA, liveness, register assignment and CSE on the grounds that ClassAd
expressions are a handful of AST levels deep. **That is the wrong cost model and should be
discarded.** It measured the *depth of an expression* and the cost of *instruction dispatch*, when
the quantity that matters is the *size of a live value*.

The arithmetic. A batch is 8192 records (`collections/colblock.go:98`). A `Vec` in data form carries
`I []int64` (8 B/record), `S []string` (16 B/record — a string header) and `St []uint8` (1 B/record)
(`collections/vm/vecexec.go:64-72`). So:

| Register form | Bytes per record | At *n* = 8192 | Relative |
|---|---|---|---|
| Data | 25 | ~200 KiB | 1x |
| Raw, 2-byte column | 2 | ~16 KiB | 1/12 |
| Mask (two bitplanes) | 0.25 | ~2 KiB | 1/100 |
| `Const` | — (one element) | ~25 B | 1/8000 |

Three consequences that change what the optimizer is *for*:

1. **Spilling a register spills a batch.** A spill is ~200 KiB of traffic, not 8 bytes. Spilling must
   be treated as nearly prohibitive, and **rematerialization is cheap by comparison** — reloading a
   column through the `ColumnSource` is usually cheaper than spilling and reloading a derived vector,
   because the block is already in the cache. This *inverts* the usual scalar-compiler preference for
   spilling over recomputation.
2. **The objective is peak simultaneously-live bytes, not register count.** An allocator that counts
   registers would make exactly the wrong decisions: fifty live mask registers cost a quarter of one
   live data register. Form must be part of the cost function, which means it must be part of the IR
   (§6.5).
3. **The target is cache residency.** At ~200 KiB per data register, five to ten simultaneously-live
   data registers evict the batch itself from a 1–2 MiB L2. The current executor stays small by
   accident — its stack depth is the expression's height — and a script with locals and loop-carried
   values has no such natural bound. That is precisely why allocation becomes necessary rather than
   decorative.

Everything in §6.2–6.7 is designed against this model. The earlier objection — "real ClassAd
expressions are a handful of AST levels deep, so a peephole over five nodes earns nothing" — is
answered rather than ignored: it is true, and irrelevant, because nothing here optimizes instruction
count.

### 6.2 SSA construction and predication

**SSA, because liveness over mutable locals in a loop is otherwise a fixpoint.** A script's locals
are mutable and loop-carried; without SSA, "where does this value's live range begin and end" is a
dataflow fixpoint over a mutable name table, computed per pass. In SSA it is a single backward walk,
and a loop-carried local is one phi at the loop header. Given §6.1, getting live ranges exactly right
is the whole game, so the IR should be the form that makes them cheap and obvious.

Construction is the standard one: build the CFG from the predication structure (which is reducible by
construction — the language has no `goto`), insert phis at join points, rename.

**Predication is in the IR, not in the backend.** Each instruction carries a predicate register
(§2.3), and `select`/blend at reconvergence points *is* the phi. That identity is load-bearing: it
means the SSA machinery and the vector execution model want the same thing, and the scalar lowering
(a real branch, §2.3) is a *de-predication* pass rather than a separate code path.

### 6.3 Liveness and register assignment

- **Liveness:** backward pass over the SSA CFG, with live ranges measured in **bytes × form**, not in
  register slots.
- **Assignment: linear scan over live intervals**, with peak simultaneous bytes as the cost.
  Graph-colouring is overkill for the shape this produces — few, long-lived, very large registers in
  mostly straight-line code with reducible loops — and linear scan handles exactly that shape well.
- **The three decisions that actually matter**, in order of value:
  1. **Which values live in mask form.** A 100x factor. Any boolean-valued intermediate should stay
     in bitplanes; `toData` should appear only where a kernel genuinely indexes elements.
  2. **Which values are rematerialized rather than kept live.** Candidates: any `OpLoadRef` (reload
     the column), any `Const` (recompute from the constant pool — free), and any strict function of
     still-live registers.
  3. **Which values can defer materialization** — stay `Raw`, `Dict` or `Const` (§6.5).
- **Spill policy:** last resort, costed at the register's byte size, and only after rematerialization
  has been tried. A spill slot is a pooled `*Vec`, so spilling is also an allocation the pool has to
  absorb (§6.7).
- **Coalescing** subsumes `callFuncVec`'s hand-written pointer swap (§5.3): a result assigned to a
  dying operand's register needs no move at all.

### 6.4 Compaction: where, when, and how results get back

The pass with the most teeth, and the one with the most ways to be subtly wrong.

**The problem.** After a selective predicate the live mask is sparse, but every downstream kernel
still runs over all *n* lanes. The tree's current answer is to abandon vectorization below 25%
density:

```go
// minLiveNum/minLiveDen is the visible fraction below which a block goes to colScope instead. The vector
// executor evaluates EVERY record in a block, including superseded ones, while colScope evaluates only
// the visible ones -- so on a heavily updated table the vectorized scan can do mostly wasted work. It is
// worth roughly 8x per record, so it stays ahead while more than an eighth of a block is visible; the
// guard trips at a quarter, leaving margin rather than sitting on the crossover.
	minLiveNum = 1
	minLiveDen = 4
```
— `collections/colvec.go:56-63`, checked per block at `:444`. Compaction replaces that bail-out:
compact and stay vectorized.

**Where the pass inserts a compaction — chosen statically.** Candidate sites are the entry of each
basic block *B* whose live-in mask is a strict subset of its predecessor's. A candidate becomes a
site only when all three hold:

1. *B*'s dominated region is long enough to pay — the pass's estimate is
   `instructions × live data registers` in that region versus the gather cost, which is
   `popcount(live) × live registers` element moves;
2. no instruction in the region needs original record positions (nothing position-dependent; the
   scatter at reconvergence restores them, so this is about instructions *inside* the region);
3. every register crossing the site is in a form compaction can move (§6.5).

**When it fires — decided dynamically.** The emitted `COMPACT` instruction reads the live mask's
popcount and compacts only if density is below a threshold. Selectivity is data-dependent, so a
static estimate would be wrong per block; this mirrors the existing design, where the 25% guard is
a per-block run-time check (`collections/colvec.go:444`) rather than a per-query decision. **So:
static site selection, dynamic firing.** That split is the design, not a hedge.

**Threshold.** Start at the existing 1/4 and calibrate, do not guess. Note the compaction threshold
is strictly *looser* than the bail-out threshold it replaces: the bail-out's alternative is
re-evaluating scalar (worth ~8x per the comment above), whereas compaction's cost is a gather —
one `CopyElem`-class move per surviving record per live register. The right default is whatever makes
a synthetic selectivity sweep monotone; a threshold that makes the curve non-monotone is a bug, and
that sweep is a test (§7.4), not a benchmark.

**What it does to the bitplanes.** `COMPACT` sets `n' = popcount(live)`, rewrites every live
register's elements into positions `0..n'-1`, and resets `live` to all-ones over `n'` records with
the tail bits cleared. Tail-clearing is not optional and the existing code says why: bits past *n*
"must be clear, or a popcount over the tail counts records that do not exist"
(`collections/vm/vecexec.go:793-797`).

A dense prefix is also the point, not just a side effect: `planeAnd`'s fast path takes **one branch
per 64 records** when neither operand has an `UNDEFINED` or `ERROR` in that word
(`collections/vm/vecmask.go:52-56`), and compaction maximizes how often that fires by packing the
surviving lanes together.

**How results scatter back.** `COMPACT` records a **selection vector** — the original index of each
surviving lane — and the region's results scatter through it at reconvergence. The primitive exists
and is named as such:

> *"This is the scatter primitive. Boxing through `classad.Value` would work and is what a caller
> without it must do, but it allocates for strings and loses the dictionary encoding, turning a code
> copy into a string materialization per record."*
> — `collections/vm/vecexec.go:1243-1246`, `func (v *Vec) CopyElem(i int, src *Vec, j int) bool` (`:1249`)

So the scatter is `for i, j := range sel { out.CopyElem(j, compacted, i) }`. Lanes absent from the
selection vector keep whatever the enclosing region left there, which is correct because they were
not live — and is exactly why the blend semantics of §2.3 have to be right.

**Nesting.** Selection vectors compose, but a nested one is relative to its parent, so the pass keeps
a stack of them and each reconvergence point scatters through the innermost only. The stack's depth is
the predication depth, known at compile time (§2.8), so it needs no dynamic bound.

**The failure mode to fuzz hardest.** Scatter-back is where off-by-one and wrong-index bugs live, and
they are *silent* — a wrong position produces a plausible wrong answer, not a crash. This codebase has
shipped that shape of bug before (a rewritten segment whose locations were not reconciled; unstable
zone identifiers that gave silently wrong counts). §7.4's compaction-transparency law is the
countermeasure and should be treated as the highest-value test in the plan.

### 6.5 Non-materialized representations: form is part of the IR

`Vec` carries three representations whose entire purpose is to *avoid* materializing, each with a
measured justification:

- **`Raw`** — the column in stored width. "Lanes come from width. A column fitted to two bytes is 8
  lanes of a 128-bit vector against 2 as int64, and widening it at load throws that away before any
  kernel sees it" (`collections/vm/vecexec.go:75-85`).
- **`Dict`** — dictionary codes; a comparison against a string literal becomes an integer range test
  because the dictionary is fold-ordered (`collections/vm/vecexec.go:88-95`, `:240-251`).
- **`Const`** — one value standing for the batch. "A literal in an expression is the dominant case,
  and materializing one across a block cost **9.8% of a scan** in the profile — nine bytes per record
  written per block, to say the same thing 1000 times" (`collections/vm/vecexec.go:97-105`).

**A register holds an unmaterialized vector, and form is part of its type in the IR.** A register is
typed `(form, width)` with form ∈ {`Mask`, `Data`, `Raw`, `Dict`, `Const`}. This is forced by §6.1:
the forms differ by up to 8000x in bytes, so an allocator that could not see form would be costing
fiction.

**Materialization becomes an explicit instruction the optimizer inserts, never a side effect of a
kernel.** Today it is a side effect — `toData`, `toMask`, `ensureInts` and `materialize` are called
from inside `binopVec`, `unopVec` and `callFuncVec` — and the hazard has already been paid once in
this codebase, recorded in the source:

> *"toData converts the vector in place to data form. It deliberately does NOT materialize a Const
> vector: compareVec calls this and then specializes on Const, so materializing here would silently
> disable that specialization — which it did, and cost the whole optimization until the benchmark
> showed no gain."*
> — `collections/vm/vecexec.go:820-823`

That is exactly the trap: an invisible conversion that gives back a measured win, detectable only by
benchmarking. Making conversions explicit instructions means the pass can see them, hoist them, sink
them, and — most importantly — a test can assert that none was inserted where none was needed.
§7.4's representation-transparency law covers correctness; the "no unnecessary conversion" assertion
covers the performance regression that correctness tests cannot see.

**Does compaction force materialization? For `Raw`, yes — so don't compact `Raw`.** `CopyElem` calls
`src.ensureInts()` (`collections/vm/vecexec.go:1253`), because "a raw element means nothing once
separated from its width and signedness" (`:1247-1248`). Compacting a `Raw` register therefore pays
exactly the widening that `Raw` exists to avoid. Rule:

- **`Raw`**: never compacted. A `Raw` register crossing a compaction site is instead
  **rematerialized after it** — reload the column, then compact — which costs a column read rather
  than a widen-then-gather, and keeps the width-native comparison path (`simdCompareRaw`) available
  up to the site. If the downstream region forces materialization anyway, the pass may compact it
  directly; that is a cost comparison it can make statically.
- **`Const`**: never compacted and never materialized by compaction. A `Const` register is
  position-independent, which is also what keeps `callFuncVec`'s all-`Const` fast path
  (`collections/vm/vecexec.go:556-566`) intact across a compaction boundary.
- **`Dict`**: compacts fine — `CopyElem` preserves codes (`collections/vm/vecexec.go:1253-1262`).
  But a `DictResolver` is per-block (`collections/vm/vecexec.go:240-241`), so a `Dict`-form register
  must not outlive its block. State that as an invariant the allocator enforces, because a register
  file that spans blocks (§6.7) makes violating it possible for the first time.
- **`Mask`**: compacts as a bitplane permutation — cheaper than a data gather by the ratio in §6.1,
  and the case compaction most wants to be in.

### 6.6 CSE: re-derived, and sound where it matters

The earlier draft concluded CSE is unsound here. That conclusion was reasoned from the **scalar**
evaluator: `evalAttrExpr` writes `ad.evaluating[norm]` as a cycle marker
(`classad/evaluator.go:583-592`) and the attribute list sorts lazily
(`classad/classad.go:327`), so an attribute read is not a pure function and two textually identical
reads are not interchangeable.

**That hazard does not survive vectorization.** In the vector executor no reference is resolved
through the evaluator at all. `OpLoadRef` calls `src.LoadColumn(r.name, r.scope, s.push())`
(`collections/vm/vecexec.go:296-300`), and the evaluator the executor carries exists only for the
value-operation hooks:

> *"The vector executor never resolves a reference through the evaluator — refs come from the
> ColumnSource — so an empty scope is enough for the value-operation hooks."*
> — `collections/vm/vecexec.go:353-357`, where it is constructed as `classad.NewEvaluator(nil)`

No ad, so no `evaluating` map and no lazy sort. The hazard was real and is scalar-only.

**Where CSE is sound:**

| Candidate | Vector context | Scalar context |
|---|---|---|
| `OpBinop` / `OpUnop` | sound | sound |
| `OpCallFunc` (strict built-ins) | sound | sound |
| `OpLoadRef` | **sound**, given the contract addition below | unsound in general (cycle markers, lazy sort) |
| `OpEvalNode` | n/a — `VecEval` declines the program outright | unsound: opaque |
| across an ad mutation | n/a — mutation is declined | barrier (below) |

- **Operators and strict calls are sound in both contexts.** `ApplyBinaryOp`, `ApplyUnaryOp` and
  `ApplyFunction` are functions of their argument values, and `StrictFunction` is the compile-time
  gate that already excludes exactly the ones that are not — the nine lazy, scope/AST-dependent and
  nondeterministic built-ins (`classad/vm_support.go:210-218`, `:235-241`). The purity split this
  design needs already exists and is already the single source of truth for it; CSE consumes it
  rather than re-deriving it, which was the substance of the original objection and is satisfied.
- **`OpLoadRef` needs one contract addition.** CSE on a column load is sound only if
  `ColumnSource.LoadColumn` is a pure function of `(name, scope)` for the duration of one batch. The
  interface does not say so today (`collections/vm/vecexec.go:252-259`). **Add it to the interface
  documentation**, because otherwise CSE depends on an unstated property of every implementation —
  and `blockVecSource` does satisfy it, but by construction rather than by promise.
- **Mutation is a barrier, in the scalar context only.** A write to `MY.X` invalidates every
  expression whose read set contains `X`. The read set is already computed — `collectReads`
  (`collections/vm/compile.go:215-222`) — so the rule is precise: a mutation instruction is a CSE
  barrier for every available expression whose reads intersect the written attribute, and a dynamic
  target (`self[expr] = v`) is a barrier for everything. Since mutation is scalar-context-only
  (§2.3), **the vector hot loop never hits a barrier** — which is the context where CSE is worth
  anything, because a redundant `OpLoadRef` there costs a whole column load.

**Net: CSE is sound for every expression a vectorized script can contain**, because that set excludes
`OpEvalNode` by construction (`VecEval` declines when `len(prog.nodes) != 0`,
`collections/vm/vecexec.go:269-272`) and excludes mutation. In the scalar context CSE is restricted
to operators and strict calls, which is where it is worth little and can simply be switched off.

### 6.7 The register file is `VecScratch` generalized

**Yes — the register file is the existing pool, addressed by register number instead of stack
depth.** `VecScratch` already holds the vector stack and reuses it across batches and across queries
via `vecScratchPool` (`collections/vm/vecexec.go:337-350`, `collections/colvec.go:54`), growing on
demand and re-slicing to the batch width in `push` (`:371-387`).

**What changes.** Slots are addressed by register number, so `push`'s invariant — the vector at this
depth is free, because we just popped — is gone. Lifetime comes from the allocator's live intervals
instead. That is the point: the stack discipline *is* a trivial allocator, and §6.1 is the argument
that a trivial allocator is no longer good enough.

**What must be preserved, and gets harder:**

1. **The capacity re-check.** `push` reallocates when the batch grew beyond the pooled vector's
   capacity (`collections/vm/vecexec.go:376-379`). A register file sized for one block must re-slice
   per block, not alias a stale length.
2. **`Release()`'s string clearing, which matters more than before.** A string element "aliases a
   block's decompressed string region rather than copying it, so a pooled stack would otherwise pin
   whichever region it last read — for as long as the pool holds it, and even if the table it came
   from has since been dropped" (`collections/vm/vecexec.go:1212-1223`). A whole-script register file
   holds values *longer* than an expression stack does, and across more blocks. **New invariant: the
   register file is `Release`d at every block boundary, not only at query end** — and a `Dict`-form
   register must be dropped there too (§6.5).
3. **Reuse guarantees weaken, deliberately.** The stack gives "O(1) reuse, always, by construction."
   A register file gives "reuse as good as the allocator." That is an acceptable trade only because
   the allocator is testable: §7.4's register-pressure-invariance law sweeps the budget from
   spill-heavy to spill-free and requires identical results, so a bad allocation is a performance
   bug and never a correctness one.
4. **Coalescing replaces the pointer swap.** `callFuncVec`'s manual move of the result into the
   deepest argument's slot (`collections/vm/vecexec.go:573-578`) becomes an allocator decision
   (§6.3).

### 6.8 What the optimizer does and does not include

**In scope, roughly in order of expected value:**

1. **Form selection** — keep values in `Mask`/`Const`/`Raw`/`Dict` where possible; insert explicit
   conversions only where needed (§6.5). Highest value per §6.1's table.
2. **Liveness + linear-scan register assignment with coalescing and rematerialization** (§6.3).
3. **Compaction placement** (§6.4).
4. **CSE / GVN** over the sound set (§6.6), where the prize is a deduplicated column load.
5. **Constant folding and propagation**, reusing `FoldConstants` (`classad/vm_support.go:129-139`) for
   expression-level folding and propagating `Const`-form registers through the IR. A `Const` predicate
   turns a predicated region into a compile-time branch, which is how short-circuiting over literals
   stops costing anything (§5.2).
6. **Inlining of non-recursive script functions**, because it is what lets liveness and CSE see across
   a call. Recursive functions are not inlined and keep a real call frame.
7. **Compile-time unrolling of a `for` over a `Const` list**, which is both the common case and the
   only way such a loop vectorizes at all (§2.3).
8. **Dead code and dead store elimination**, which falls out of SSA and matters here because a dead
   data register is 200 KiB.
9. **Deferred column loads -- sinking a load into the narrowest predicate that needs it.** The
   highest-value pass on this list, and distinct from both 4 and 8: CSE deduplicates a load and DCE
   removes one nothing reads, but neither defers one that is read only under a predicate. A column
   load is a *decompression*, and roughly 78% of mutable read CPU is zstd, so eliding one is worth
   far more than eliding any amount of arithmetic. If `Owner` is read only inside
   `if RequestGpus > 0:`, a block where nothing passes must never touch that column. The
   counterpart to loop-invariant hoisting, in the opposite direction: hoist out of loops, sink into
   predicates.
10. **Tiling and kernel fusion.** Attacks §6.1's cost model head on rather than working around it.
   Peak live bytes is `registers x 8192 x 25B`; tile the batch into L2-sized sub-batches and fuse
   each straight-line region over a tile, and the peak falls by 8-16x -- at which point MORE live
   registers become affordable, not fewer. So tiling and register assignment are substitutes for the
   same problem, and this may belong ahead of 2 rather than after it. Two constraints: a tile
   shortens the region a compaction has to amortize over (§6.4), and `Raw`/`Dict` are block-scoped
   (§6.5), so a tile must not straddle a block.
11. **Early exit on an empty live mask.** A popcount at region entry and a jump past it. §2.3 gives
   `while` this; nothing else has it, and on a selective script it is nearly free.
12. **Loop-invariant code motion.** The motivating case is concrete: there is no regular-expression
   cache anywhere in the library -- five bare `regexp.Compile` call sites in `classad/functions.go`
   -- so `regexp()` with a constant pattern recompiles per element in the vector path and per record
   in the scalar one. Hoisting that out of the element loop is likely the largest constant-factor win
   available on string-heavy work. Half a pass that recognizes `Const` arguments, half a change to
   the built-in.
13. **A definedness (totality) analysis.** Proves a subexpression yields only TRUE or FALSE, never
   UNDEFINED or ERROR, from columnar metadata that already exists -- presence bitmaps and the
   dictionary's `noEscape` flag. It pays twice: it lets the form-selection pass emit the
   one-instruction AND instead of leaving `planeAnd` to discover per 64 records that the
   three-valued apparatus was unnecessary (`collections/vm/vecmask.go:52-56`), and it is the
   *precondition* for the pass in the next paragraph.

**Unsound here, and worth writing down because every database reaches for it:**

**Reordering the operands of `&&` or `||` by selectivity or cost is not legal.** ClassAd's logical
operators are LEFT-BIASED, not commutative: `ERROR && FALSE` is ERROR while `FALSE && ERROR` is
FALSE. The tables say so, and so does the comment above them, which adds the reason this is easy to
get wrong -- a combine "that quietly symmetrised them would still pass any corpus that never put an
ERROR on the left" (`collections/vm/vecmask.go:25-29`). Reordering therefore requires discharging
pass 13 on both operands first; with totality proved, the operator is commutative and the reordering
is sound. Without it, the pass is a silent wrong-answer generator on exactly the records that are
already unusual.

**Out of scope, with reasons:**

- **Instruction scheduling.** The cost is memory traffic, not pipeline hazards; the host CPU reorders
  anyway.
- **Loop unrolling of data-dependent loops.** The batch *is* the unrolling.
- **Autovectorization.** The IR is already vector.
- **Alias analysis beyond the read/write-set barrier rule** (§6.6). Attributes are named; there are no
  pointers.
- **A second expression lowering.** Expression code comes from `compile.go` and nowhere else (§5.2).
- **Any pass that cannot be switched off individually.** §7.4 requires per-pass switches, so a pass
  that cannot be isolated cannot be validated and does not ship.

---

## 7. Testing: the oracle problem

A whole-script register IR with an optimizer is a much larger new surface than a statement walker,
and the parity discipline here demands a differential oracle
(`collections/vm/opcode.go:4-9`). Five layers.

### 7.1 O1 — the expression layer keeps its existing oracle, for free

Every ClassAd sub-expression in a script is lowered by the existing compiler into the existing
opcodes (§5.2), so `FuzzDifferential` (`collections/vm/vm_test.go:263-291`) and the C++ differential
harness against `libclassad` (`fuzz/README.md`, in-process via cgo per `fuzz/README.md:16-25`,
canonical encoding in `fuzz/canon/`) already cover 100% of expression semantics, seeded with the
parity corpus (`collections/vm/parity_corpus_test.go:3-7`). Saying precisely what is *not* newly
tested is the first job of the plan.

### 7.2 O2 — reduce the new parts to the old parts and test the reduction

The answer to "what is the oracle for a language with no reference implementation." The law:

> For a script consisting only of local assignments followed by a single result expression, where
> every right-hand side is a pure expression, the script's result must equal `classad.Eval` of the
> fully-inlined expression.

```
a = <X>
b = <Y>
result = a + b          ===     Eval( (<X>) + (<Y>) )
```

Generate `X`, `Y` with the existing grammar-directed generator (`fuzz/gen/gen.go`) and compare in
canonical form (`fuzz/canon/`). This pins the **binding model** — scoping, case-insensitive folding,
the strict-assignment forcing rule of §2.5 — against the tree-walking evaluator, and therefore
transitively against the C++ engine. It works only because expression semantics are unforked (§2.1),
which is why that rule is not merely aesthetic.

### 7.3 O3 — scalar/vector agreement

The strongest oracle for the vector path, and the same bargain the existing executor already strikes:
the scalar interpreter is "a per-record reference to test this one against on the same Program"
(`collections/vm/vecexec.go:19-23`).

> The same script over the same records must produce **element-identical** results — values, fault
> mask, per-site fault counts, and element-step budget consumption — whether executed scalar
> record-by-record or vector batch-by-batch, at every batch size including 1, including a partial
> final batch, and including batch boundaries that fall inside a loop's divergence.

Three properties make this a real test rather than a tautology: the two executors share only the IR
and the `classad` hooks; the fault model is per-record by construction (§2.2), so the comparison is
elementwise; and the budget is in element-steps (§3.1), so it is batch-size-invariant and a
divergence in consumption localizes a bug that the values alone might hide.

This also covers the predication model: a script whose records take different branches, leave a loop
at different iterations, and fault at different sites is exactly the input that distinguishes correct
predication from plausible predication.

### 7.4 O4 — optimizer parity, with per-pass isolation

> Optimized and unoptimized execution of the same script must agree, and **every pass must be
> individually switchable** so a divergence localizes to one pass.

The specific laws, in descending order of how much I would trust them to find real bugs:

1. **Compaction transparency.** Results with compaction forced at *every* candidate site must equal
   results with it disabled everywhere, and both must equal the dynamic-threshold run. Fuzz hardest
   here: scatter-back is where silent position bugs live (§6.4).
2. **Representation transparency.** Forcing every register to `Data` form at allocation must equal
   letting `Raw`/`Dict`/`Const` survive. This directly tests §6.5's trap — the one this codebase has
   already paid for once — and it is the only correctness test that can catch a conversion inserted
   where none was needed. Pair it with an assertion on the *count* of inserted conversions, since
   correctness alone cannot see that regression.
3. **Register-pressure invariance.** Sweep the register budget from one that forces heavy spilling to
   one that forces none; all results must agree. This is what makes a bad allocation a performance bug
   rather than a correctness one (§6.7).
4. **Pass isolation.** `-O0`, each single pass alone, and all passes together must all agree, over the
   same generated corpus.
5. **Pass idempotence.** Running a pass twice equals running it once. Cheap, and it catches a pass
   whose output violates its own input invariants.
6. **Selectivity monotonicity.** Across a synthetic predicate sweep, element-steps consumed must be
   monotone in selectivity. A non-monotone curve means the compaction threshold is wrong (§6.4), and
   this is a test rather than a benchmark because the monotonicity is a property, not a number.

### 7.5 O5 — metamorphic laws for the rest

- **Desugaring equivalence.** `elif` chains versus nested `if`; `for x in L:` versus `while` over an
  index; `a += b` versus `a = a + b`; an inlined function versus a called one. Same results, same
  faults, same fault sites. Strong because the two forms share almost no path above the IR.
- **Determinism.** In deterministic mode the same `(script, records)` yields bit-identical results
  across runs.
- **Budget monotonicity.** A script completing under element-step budget *N* completes identically
  under any *M* > *N*; a per-record trip fault at limit *N* reproduces at the same record and site for
  every *M* < *N*.
- **Fault locality.** Faulting one record must not change any other record's result. The law that
  makes §2.2's per-record model mean something, and the one a careless mask bug breaks immediately.
- **Mutation/aliasing law.** For every local assigned before an ad mutation, its value after the
  mutation equals its value before. §2.5's hazard as an executable test.

### 7.6 First deliverable

**D1 — `script/`: the statement IR, SSA construction, an unoptimized register-machine interpreter,
and O2. No surface syntax and no optimizer.** Programs are constructed through a Go builder API, not
parsed.

Done when: the O2 reduction fuzzer runs clean against `classad.Eval`; the fault classes of §2.1 are
enumerated with a test each; the budgets of §3 are enforced with deterministic diagnostics; §2.5's
aliasing law holds; and `ResolveRefIgnoringResolver` (§2.4) is the only addition to `classad/`.

Why unoptimized first, and why no syntax: the optimizer's entire validation story (§7.4) is "it agrees
with the unoptimized execution," so the unoptimized executor is the oracle and has to exist and be
trusted first. And every semantic question here is answerable without syntax, while every syntax
question in §4 is answerable without semantics.

Then: **D2** the vector executor for the script IR, predication and blends, validated by O3 —
this is the deliverable that proves the retarget. **D3** the lexer and parser (indentation, implicit
joining, the colon rule, soft keywords) against a golden-file corpus plus a negative corpus per
diagnostic. **D4** the optimizer, one pass at a time, each gated on its O4 law and each individually
switchable; form selection first, then liveness and allocation, then compaction, then CSE. **D5**
`def`/`return` with inlining, `defer`/`${}`, and host function registration.

---

## 8. What this is not

- **Not a general-purpose language.** No modules or imports, no classes, no dict or set types, no
  mutable containers beyond the subject ad, no iterators or generators, no comprehensions, no
  operator overloading, no string formatting beyond ClassAd built-ins.
- **No exceptions and no `try`/`except`.** A fault is a per-record outcome (§2.2) reported to the host,
  not a catchable control transfer. In-script recovery would let a script swallow a trip-limit fault,
  and it has no coherent meaning when different records fault at different sites. The total predicates
  (`is undefined`, `isError`, `?:`) cover the cases a handler would.
- **No I/O of any kind.** No file, network, subprocess, environment or clock access. This is what makes
  the element-step budget a sound time proxy (§3.3) and what makes the language safe in a scan loop.
- **No concurrency within a script.** A `*ClassAd` is not safe for concurrent use — reads write cycle
  markers and sort lazily (`classad/evaluator.go:583-592`, `classad/classad.go:327`). One scalar
  execution owns its subject ad; one register file belongs to one goroutine, as `VecScratch` already
  does.
- **No `eval` of script text.** ClassAd's expression-level `eval()` (`classad/evaluator.go:1545-1584`)
  stays as it is; there is no statement-level counterpart. Dynamic statement construction would be
  unanalyzable, unbudgetable, and unoptimizable.
- **No shadowing or redefinition of ClassAd built-ins** (§2.6).
- **Not a second expression lowering** (§5.2), and not a second IR for the vector executor — the same
  instruction stream runs both ways (§2.3).
- **No instruction scheduling, loop unrolling of data-dependent loops, or autovectorization** (§6.8).
- **No ad mutation in the vector context** — declined, with a scalar fallback (§2.3).

---

## 9. Decisions, and what would change each

| # | Decision | Reason | What would change my mind |
|---|---|---|---|
| D1 | Two execution contexts, one IR; the vector context drives the design | The vector executor has no branch opcode and declines `OpEvalNode` (`vecexec.go:269-272`, `:316-323`), so it constrains everything | Nothing — a scalar-only design would have to be rebuilt to vectorize |
| D2 | A non-boolean condition **faults that record** (cleared from the live mask, result `ERROR`); it does not abort the script | In a batch of 8192 (`colblock.go:98`) one bad record must not take out the rest; `VsError`/`mError` already exist to say so (`vecexec.go:53`, `vecmask.go:30-35`) | Evidence that consumers want faulted records *dropped* rather than reported as `ERROR` — a host policy, not a language change |
| D3 | Expressions stay bit-identically total; partiality only at the statement layer | Makes O1 and O2 possible (§7) | A construct that provably cannot be expressed without expression-level faulting |
| D4 | Control flow is **predication**, not branching; assignment under a predicate is a blend | No branch opcode in the vector executor; the mask form already exists; predication's phi *is* SSA's phi | Nothing for the vector path; the scalar path keeps real branches as a de-predication pass |
| D5 | Bare assignment writes a **local**; ad writes always name a scope; locals are case-insensitive | Cannot mutate by accident; one fold rule; matches Python's local-shadows-global | Evidence operators overwhelmingly want the ad as the default write target |
| D6 | Local assignment is **strict** (forces lazy lists) | `listData` holds a live scope pointer (`evaluator.go:793-806`) while `Insert` mutates in place (`classad.go:498`) | A cheaper fix such as copy-on-write in `Insert` — but that changes the ad layer for everyone |
| D7 | Whole script → **one instruction stream over a register file**; no statement walker | An optimizer must see across statements; predication must thread one region; a stack cannot express long-lived locals | A profile showing the optimizer wins nothing — but that is D9's question, not this one |
| D8 | Expression leaves still lower through the **existing** `compile.go` | Keeps one lowering site (`opcode.go:4-9`), which is what O1 depends on | Nothing |
| D9 | A real optimizer: form selection, SSA, liveness, linear-scan allocation, coalescing, rematerialization, compaction, CSE | **A register is a batch**: ~200 KiB in data form vs ~2 KiB in mask form at *n*=8192 (§6.1), so allocation controls cache residency and a spill moves 200 KiB | A profile showing peak live bytes is not the bottleneck — i.e. that scripts stay at expression-like register pressure after all |
| D10 | **Form is part of a register's type**; materialization is an explicit instruction | The forms differ by up to 8000x in bytes; an invisible conversion already cost a measured 9.8% win once (`vecexec.go:824-829`) | Nothing — this is the trap, documented in the source |
| D11 | Compaction: **static site selection, dynamic firing**; `Raw` rematerialized rather than compacted; `Const` never compacted | Selectivity is data-dependent, so firing must be dynamic, as the existing 25% guard already is (`colvec.go:444`); `CopyElem` widens `Raw` (`vecexec.go:1241`) | A measurement that a static selectivity estimate is accurate enough per block |
| D12 | CSE is **sound in the vector context**, including on `OpLoadRef`, given a purity clause added to `ColumnSource` | The cycle-marker hazard is scalar-only: the vector executor resolves no reference through the evaluator (`vecexec.go:296-300`, `:353-357`) | A `ColumnSource` implementation that cannot promise purity per batch — then CSE drops `OpLoadRef` and keeps the rest |
| D13 | Register file = `VecScratch` generalized; `Release`d per **block**, not per query | It already pools and re-slices (`vecexec.go:337-387`); a longer-lived file pins string regions longer (`:1209-1219`) | Nothing; the invariant is strictly stronger than today's |
| D14 | Budgets: per-batch **element-steps** (aborts), per-record **trips/depth** (fault); no wall clock | Element-steps are batch-size-invariant, which makes them an O3 comparison quantity; a timeout would make faults irreproducible | A host that cannot tolerate the worst case — answered by an external supervisory abort |
| D15 | Significant whitespace; block colon is the header's last token; no one-line suites | `{}` and `[]` are already list and ad literals; the colon rule is then provably unambiguous (§4.2 rule 5) | Nothing — decided |
| D16 | No `and`/`or`/`not`, no f-strings, no comprehensions | Each forks the ClassAd expression grammar | Nothing, given "expressions are ClassAd expressions" |
| D17 | Unoptimized register interpreter ships **first** and is the optimizer's oracle | §7.4's entire story is "the optimizer agrees with it" | Nothing |
| D18 | Every optimizer pass individually switchable | A divergence must localize to one pass | Nothing; a pass that cannot be isolated cannot be validated |

---

## 10. Open questions

1. **Does `ColumnSource.LoadColumn` promise per-batch purity?** §6.6's CSE on `OpLoadRef` needs it and
   the interface does not state it (`collections/vm/vecexec.go:252-259`). `blockVecSource` appears to
   satisfy it; confirm and write it down before CSE ships.
2. **Compaction's real threshold.** The 1/4 inherited from `minLiveNum`/`minLiveDen`
   (`collections/colvec.go:62-63`) is a bail-out threshold, not a gather threshold, and the gather is
   cheaper. Measure with the §7.4 selectivity sweep; do not pick a number.
3. **Does compaction subsume the per-block bail-out?** If a compacted vector path beats `colScope` at
   every density, `minLiveNum`/`minLiveDen` and the `countBlockScoped` fallback
   (`collections/colvec.go:444-459`) become dead code. That would be a significant simplification and
   should be checked explicitly rather than assumed.
4. **Ragged per-record `for` loops.** §2.3 declines them to scalar. A per-record iteration count is
   representable (iterate to the maximum, predicate the tail), so this may be a capability rather than
   a limit — but it costs `max(len)` iterations for every record, so it needs a density argument of its
   own before it is worth building.
5. **Fault report granularity.** Per-site aggregation (§2.2) loses which records faulted beyond the
   first. Is the `faulted` mask enough for consumers, or does a host need per-record sites?
6. **Budget defaults.** The numbers in §3 are unset on purpose; derive them from the O4 corpus.
7. **Naming.** "ClassAd Script" is a placeholder.

---

## 11. Implementation status, 2026-11-01

Everything above §11 was written before any of it existed, and enough now exists that a reader
cannot tell from the prose which parts are code and which are still argument. This section is the
ledger. Every "shipped" claim is on `main` with a citation; §11.3 lists the places where the prose
above is now **wrong**, and should be trusted over it.

The short version: the scalar register machine, its two optimizer passes, the binding model and the
O2 reduction law are real. The vector path for *scripts* is not built at all — what landed in the
vector path is a widening of what an **expression** can contain, which is a prerequisite for it and
is worth more on its own than it looks (§11.1, last row).

### 11.1 Shipped

| From | What | Where |
|---|---|---|
| D7 | Whole program → one instruction stream over a register file, with its own opcode set | `collections/vm/regir.go:49-125`, `:188` |
| D17 | The unoptimized register interpreter, and it is in fact used as the optimizer's oracle | `collections/vm/regir.go:636`, `collections/vm/optimize_test.go:28-51` |
| D8 | Expression leaves still lower through the existing `compile.go`, unforked | `collections/vm/script.go:215-226` |
| D5 | Binding model: bare name is a local, locals fold case-insensitively, a scoped reference is always the attribute, `x = x + 1` reads the attribute and binds the local | `collections/vm/script.go:75-111`, pinned by `collections/vm/script_test.go:134` |
| §2.1 | Statement layer exists: `Let`, `If`/`else`, `While`, `Result` | `collections/vm/script.go:75,113,164,197` |
| §3 | A *deterministic* budget, with the no-wall-clock rule held | `collections/vm/regir.go:651,661` |
| O2 | The reduction law — a script of local bindings plus a result equals `classad.Eval` of the inlined expression — as a corpus test and a fuzzer | `collections/vm/script_test.go:39-63,112` |
| §6.8.8 | Dead code elimination | `collections/vm/optimize.go:290` |
| D18 | Per-pass switches, with the zero value running nothing, and a test that the zero value is inert | `collections/vm/optimize.go:27-37`, `collections/vm/optimize_test.go:178` |
| §2.6 | Host-registered functions, with both guards: a built-in name is refused, and the registration must satisfy the strictness contract | `classad/udf.go:100`, `classad/vm_support.go:277-289` |
| §6.8.12 | The *built-in half* of the regexp LICM case: a compile cache, measured at 7.2x–16.5x on the call | `classad/regexpcache.go` |
| §5.2 | Native calls, `member`, `stringListMember` over a constant list, `ifThenElse`, and Elvis all execute in the **vector** path, where before them a function call forced the whole expression off it | `collections/vm/vecexec.go:346-372` |

That last row is the one with the most leverage and the least to show for itself in this document.
`Query.Native()` gates every columnar accelerator, and before this work *any* function call in a
constraint turned it off. Widening what the vector path accepts therefore buys the accelerators on
expressions that previously fell to the row scan — a bigger effect than making any single opcode
faster, and the reason the UDF question and the script question turned out to be the same question.

### 11.2 Decisions made while building, which belong in §9 and were not in the draft

**S1 — Locals are places, not bindings.** One register per local for the whole program
(`collections/vm/regir.go:146-152`), written by `OpStoreLocal` and read by `OpLoadLocal`
(`collections/vm/opcode.go:104-124`). The alternative, a fresh register per binding, is what SSA
wants and cannot express a binding made on one path of a branch and read after the join without a
phi — and the compiler has no phi to emit there. The consequence is load-bearing for every pass:
**the register form is not SSA for locals**, so a pass must count definitions rather than assume
single assignment, which is what `defCounts` exists for (`collections/vm/optimize.go:98-107`).

**S2 — Expression-level branches vectorize by *blending*, not by predication.** §2.3 says control
flow is predication. What actually shipped for `?:` and `ifThenElse` evaluates **both paths
unconditionally** and selects per record at the join (`collections/vm/vecexec.go:358-372`, blend at
`:443`). That is sound *precisely because* expressions are total and pure (§2.1): evaluating a path
no record took cannot fault, cannot mutate, and costs only time.

It does not generalize, and the distinction needs to be written down before either half is extended.
A `while` body cannot be evaluated unconditionally — it has no bound — and a statement region cannot
be once mutation exists. So §2.3's mask-stack predication remains the design for **statements**, and
blending is an expression-level special case that happens to be cheaper than predication where it
applies. Expect both in the final executor, not one.

**S3 — The shipped budget is an instruction count, not element-steps.** `DefaultStepBudget = 1 << 22`
(`collections/vm/regir.go:651`), decremented per instruction in the scalar register machine, with
exhaustion returning `ok=false` rather than a value. It satisfies §3.3's determinism requirement and
nothing else: there are no per-record trip limits, so a runaway `while` consumes the whole budget and
reports failure for the script instead of faulting the one record (§3.2). §3.1's element-step unit is
still the right unit for the vector path and is still unbuilt.

**S4 — Copy propagation, which §6.8 does not list.** §6.8's list assumed SSA, where a copy is a
renaming rather than an instruction. Under S1 it is a real instruction and the dominant one: every
read of a local lowers to a move, so an expression-heavy script accumulates them in proportion to how
much it reuses its own bindings. The pass is at `collections/vm/optimize.go:167-220` with three
soundness conditions, two of which currently overlap for the shapes the compiler emits — the source
says which and why neither is removable, because an overlap that is a property of today's lowering is
exactly what a later reader deletes.

**S5 — A UDF registry in `classad/`, not in `collections/vm/`.** `classad.RegisterFunction`
(`classad/udf.go:100`) publishes an append-only copy-on-write table through an atomic pointer, so the
per-record read path is one atomic load and the whole check costs nothing when nothing is registered.
Because `StrictFunction` consults it (`classad/vm_support.go:285-287`), a registered UDF
**automatically** compiles to `OpCallFunc` and vectorizes elementwise with no further work — which is
the concrete payoff of §2.6's rule that registrations are declared to the compiler.

### 11.3 Corrections — prose above that is now false

1. **The header.** "Nothing implemented" is no longer true; see §11.1.
2. **§2.2** says `?:` "is currently *not* vectorized". It is, by blending (S2).
3. **§2.2 is incomplete rather than wrong about calls.** It says the total predicates `isUndefined`
   and `isError` compile to `OpCallFunc` and vectorize elementwise, which is still true. A reader will
   reasonably assume `ifThenElse` takes the same route, since it is a function in the ClassAd grammar
   — it does not. It compiles to `OpBranchCond`, a three-way dispatch on `classad.ConditionState`
   (`collections/vm/opcode.go:61-75`), and in the vector path it blends. Which functions are *not*
   calls is now a thing the compiler decides, and §5.2 should say so.
4. **§6.8 item 12** says there is no regular-expression cache anywhere in the library. There is now
   (`classad/regexpcache.go`); the *pass* it was arguing for is still unbuilt, and with the cache in
   place its remaining value is smaller than the item claims.
5. **§2.5's hazard is not yet guarded, and cannot yet fire.** D6 (strict local assignment) is *not*
   implemented: a register holds whatever `classad.Value` the machine produced, lazy lists included.
   It is unreachable today only because no ad mutation exists to change the scope underneath one. The
   guard must land in the same change as the first mutation, not after it.
6. **§2.1's fault table is aspirational.** Only `OpFault` exists (`collections/vm/opcode.go:139-148`),
   which ends the script with `ERROR`. There is no fault mask, no fault-site table, and no fault
   report — so F1–F4 are currently one undifferentiated outcome, and the per-record model of §2.2 is
   entirely unbuilt.

### 11.4 Two findings from building it

Neither is a design change; both are the kind of thing this document exists to keep.

- **A real-arithmetic divergence, found by the parity fuzzer rather than by reading.** A real result
  that overflows to `+Inf` is `ERROR` in the reference while `-Inf` and NaN are not; the vector path
  served `+Inf` (`collections/vm/vecexec.go:653-657`). It reproduced on the parent commit, so it was
  pre-existing — the new fuzzer found it, the new code did not cause it.
- **A static read set is only sound when every read is syntactically visible.** `IsMatchAll` /
  `IsMatchNone` decided a constraint against an empty ad, guarded only by the references the
  expression *mentions*; an `eval()` reads attributes named at run time, so a constraint containing
  one was folded to "matches nothing" and `COUNT(*)` returned 0 where the row scan found 400. Fixed
  by requiring `vm.SelfRefsSafe` (`db/archive.go:658-660`, `:695-697`;
  `collections/vm/plan.go:97-101`). §6.6 should absorb the general form: CSE and every other pass
  that reasons about a read set inherits this precondition, and `eval()` is the one construct that
  violates it.

---

## 12. What is left, and what blocks what

Ordered by dependency rather than by appeal. The notable thing about the order is that **the critical
path to the database payoff does not pass through syntax at all** — D3 is last, and nothing above it
needs it.

1. **The per-record fault model (§2.2).** First, because it is a *representation* decision that
   everything below it encodes: a vector executor for statements has nowhere to put "this record's
   condition was undefined" until the fault mask and the per-site report exist. Cheap — `VsError` and
   `mError` already exist — and it cannot be retrofitted around.
2. **Predication for statements, and the masked blend (§2.3).** The one genuinely new runtime
   primitive. Half of it is already there: `blend` (`collections/vm/vecexec.go:443`) selects per record
   at a join. What it lacks is the mask *stack* and the rule that assignment under a predicate leaves
   non-live lanes untouched. Note S2 — this is in addition to blending, not a replacement for it.
3. **The element-step budget (§3.1), and per-record trip limits (§3.2).** Required *before* O3, not
   after: budget consumption is one of the four quantities scalar and vector execution are supposed to
   agree on elementwise, and it is the one that localizes a bug the values alone would hide.
4. **O3 itself (§7.3).** The deliverable that proves the retarget. Everything above exists to make it
   a real test rather than a tautology.
5. **Form selection (§6.8.1) and deferred column loads (§6.8.9).** The two highest-value passes, and
   both need the vector path to exist before they mean anything — a form is a property of a `Vec`, and
   a deferred load defers a decompression. Measured context for sizing: roughly 78% of mutable read
   CPU is zstd, so a load not performed is worth more than any amount of arithmetic removed.
6. **`for` over a `Const` list, and `def` with inlining (§6.8.6-7).** Language completeness. Neither
   blocks the database use, and `for` over a *per-record* list stays declined (§2.3) until open
   question 4 gets its density argument.
7. **D3, the lexer and parser.** Last on purpose. Every semantic question above is answerable through
   the builder API, and holding syntax back keeps the §4 lexical argument free to change.

Two items on §6.8's list have been partly overtaken and should be re-costed rather than built as
written: item 12 (LICM for regexp) now has its built-in half done, and item 2 (liveness and
allocation) competes with item 10 (tiling), which §6.8 itself flags as possibly belonging first.

---

## 13. The gap this document does not cover: user-defined *aggregates*

§2.6 and the shipped registry (S5) give the host a user-defined **scalar** function: a function of
argument values, compiled to `OpCallFunc`, vectorized elementwise. That covers projections and
predicates. It does not cover aggregation — and aggregation is where this database's query layer
actually spends its time, so a UDF story that stops at scalars stops short of the use that motivated
it. This section states the problem and the one hard constraint, and deliberately does not pick a
design.

### 13.1 Why it is not a small extension

**The selector is a closed byte on the wire.** `db.AggFunc` is a `uint8` enum of six values
(`db/aggregate.go:21-38`), aliased straight into the RPC layer (`dbrpc/aggregate.go:18`) and decoded
as `AggFunc(r.u8())` (`:247`). A user-defined aggregate needs a representation that is not one of six
bytes, which is a wire-compatibility change and not a new case in a switch.

**There are two aggregation paths, and only one of them can take a UDAF.**

- The **row path** accumulates per group in `aggAcc` and defers the arithmetic to the ClassAd
  library's own `Sum`/`Avg`/`Min`/`Max` at the end (`db/aggregate.go:333-365`). A UDAF plugs in here
  cleanly.
- The **columnar path** does not read values at all: it computes the answer from a zone's precomputed
  `NumStats` (`db/colagg.go:124-131`). A UDAF cannot plug in here, because there is nothing to
  accumulate — the point of that path is that the rows are never visited.

So the honest headline for a UDAF is not "slower than a built-in". It is: **a UDAF forfeits the zone
fast path**, and on the archive that is the difference between reading an index and scanning the
rows. Any design must state that up front, and should probably offer the escape hatch rather than
hide the cliff — a UDAF that can declare itself computable from the stats a zone already holds gets
the fast path, and one that cannot says so.

### 13.2 Four operations, and *merge* is the load-bearing one

The interface is init / accumulate / **merge** / finalize, and the temptation is to ship the first,
second and fourth and add merge later. That gets the capability wrong in three specific ways, each
already visible in the tree:

- **Parallel and per-segment scans produce partial states.** Without merge a UDAF is single-threaded
  by construction, in the one place the engine is not.
- **Materialized views are maintained by delta, and the view layer already classifies aggregates by
  whether that is possible.** It admits only COUNT/SUM/AVG, and says why: "MIN/MAX would need a
  rescan when the current extreme is removed" (`db/view.go:28-37`). A UDAF must declare which camp it
  is in, and the view layer must refuse to maintain one that cannot merge under *deletion* — which is
  a strictly stronger property than merging under union.
- **Visit order is not stable.** Zone and segment identity has already produced one silent
  wrong-count bug in this engine, so merge must be required to be **associative and commutative**,
  not merely associative. A UDAF whose merge is order-sensitive would be correct in tests and wrong
  in production, which is the worst available failure mode.

### 13.3 An argument for an explicit state type, from the built-ins' own shape

Worth recording because it inverts the expected direction. `aggAcc` **retains every argument value**
for SUM/AVG/MIN/MAX — `a.vals = append(a.vals, v)` (`db/aggregate.go:363`) — and hands the whole
slice to the library at the end. That is deliberate, and the comment says why: it makes type coercion
(int64-exact sums, int/real promotion, boolean and undefined handling, error propagation) match
HTCondor's `sum()`/`avg()` *exactly* rather than being a re-implementation of it
(`db/aggregate.go:327-331`).

The cost is that built-in aggregate state is O(group size), not O(1) — parity bought with memory. So
a UDAF with an explicit, bounded state type would be **better behaved than the built-in it imitates**.
That is an argument for giving the interface its own state rather than modelling it on `aggAcc`, and
against the obvious shortcut of letting a UDAF receive the accumulated value slice.

### 13.4 Questions a design has to answer

1. **Who may register one?** The scalar registry is open to the host process. A UDAF that forfeits
   zone pruning is a query-shape decision with a server-wide cost, which argues for the same
   admin-labelled trust model the WASM loader needs for its unsandboxed path — configuration, not
   something a module claims about itself.
2. **What is the wire representation**, given §13.1, and does an unknown UDAF name on the server fail
   the query or fall back to a client-side aggregation?
3. **May a UDAF appear in a materialized view at all**, or is the first version query-only? Query-only
   is a defensible place to start and should be stated rather than left to be discovered.
4. **Does a UDAF see values or columns?** The scalar path is elementwise over boxed `classad.Value`s;
   the whole point of the vector engine is not to box. A batch-shaped accumulate is the same design
   question the vectorized WASM ABI is facing, and the two should share an answer rather than invent
   two.
