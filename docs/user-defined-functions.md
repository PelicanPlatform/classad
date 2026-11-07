# User-defined functions and aggregates

ClassAd ships a fixed set of built-in functions, and the database layer a fixed
set of aggregates (`COUNT`, `COUNT(DISTINCT)`, `SUM`, `AVG`, `MIN`, `MAX`). This
page is about adding your own.

**You need to be embedding the library in a Go program.** Everything below is
for the program that owns the process.

Every sample is an excerpt from a file `go test` runs; section 9 lists them.

## 1. What you can add

|  | scalar function | aggregate |
|---|---|---|
| **Go** | `classad.RegisterFunction` | `db.RegisterAggregate` |
| **WebAssembly** | `wasmudf.Register` | `wasmudf.RegisterAggregate` |
| **ClassAd Script** | `scriptudf.Register` | — |

A **scalar function** is called once per value, anywhere a built-in like `pow()`
can appear. An **aggregate** reduces a group of rows to one answer, named in an
`AggSpec` as `AggUser`, and reduced by the same `GROUP BY` core as `SUM`.

The second and third rows are not separate mechanisms. `wasmudf` loads a guest
module; `scriptudf` compiles a module written in **ClassAd Script**, the statement
language of section 7. Each picks one of the module's exports by name, wraps it in
the Go signature the registry above it takes, and registers it there. Sections 2
and 3 apply to all three.

## 2. Go, WebAssembly or ClassAd Script

Pick by **whose code it is**. Write it in Go when it is part of the system you
are building. Use WebAssembly when the function comes from outside your trust
boundary — a site admin, a user — because then the isolation is the reason you
are here. Use ClassAd Script when the rule is policy someone writes and reads
rather than code someone builds: it needs no toolchain, it is the language your
policy expressions are already written in, and a function that would reach beyond
its arguments is refused at registration rather than trusted.

A wasm guest can reach nothing: no I/O, no clock, no randomness, no WASI. That is
not a policy, it is the absence of anything to call. The host offers exactly one
import, so a module wanting more cannot be instantiated and is rejected at load,
naming what it asked for.

What the isolation costs, per element on an Apple M2 Pro, for a guest that
doubles a number:

| path | ns/element |
|---|---|
| scalar, untrusted | 782 |
| scalar, trusted | 106 |
| batch of 4096, untrusted | 27.9 |
| batch of 4096, trusted | 6.3 |

A Go function has none of this overhead. Batching and trust are independent wins,
so a guest that wants to be fast should do both (sections 5 and 6).

## 3. Rules for everything here

**A function gets values, not the ad.** No evaluator, no scope, no AST. It cannot
read an attribute, resolve a reference, or look inside a list — realizing a list's
elements means evaluating them in the scope they were written in, so a function
handed one should return ERROR.

This is what keeps your function fast. Because the compiler knows a call cannot
read anything the expression did not already name, it compiles the call in place.
An expression it could not trust that way gets handed to the tree-walking
evaluator, and every columnar accelerator goes with it.

**UNDEFINED and ERROR are values, and choosing between them is your job.** There
is no error return. An attribute the ad does not carry arrives as UNDEFINED, and
a function that answered ERROR for it would make a `Requirements` expression
*reject* the machine where it meant to ignore the term. A string where a number
belongs is different, and deserves ERROR.

**Return failures; do not panic.** A panic from a scalar function becomes ERROR
rather than killing the daemon, but that is a backstop, not an interface. A
failing *aggregate* instead fails the whole query: one bad record is an ordinary
outcome, but a group's answer depends on every row in it, so a half-accumulated
state is a wrong answer that looks right.

**Registration is append-only, so do it at start-up.** Registering a name twice
fails rather than overwriting, nothing may shadow a built-in, and there is no
unregister: compiling an expression records that a name was yours, and programs
are cached and shared.

**Names fold case,** like every function name in this language.

## 4. A function in Go

```go
err := classad.RegisterFunction("pctUsed", 2, 2, func(args []classad.Value) classad.Value {
	if args[0].IsUndefined() || args[1].IsUndefined() {
		return classad.NewUndefinedValue()
	}
	used, uerr := args[0].NumberValue()
	total, terr := args[1].NumberValue()
	if uerr != nil || terr != nil || total == 0 {
		return classad.NewErrorValue()
	}
	return classad.NewRealValue(used / total * 100)
})
```

`2, 2` is the arity range; a negative maximum means variadic. After that:

```
pctUsed(Used, Total)     37.5
PCTUSED(Used, Total)     37.5
pctUsed(Used, Absent)    undefined
pctUsed(Used, Label)     error
```

If a query using your function is unexpectedly slow, check
`classad.StrictFunction("pctUsed", 2)`. It reports whether the compiler will take
the call in place, and it is the difference section 3 describes.

Registration returns one of three sentinels so a loader can tell a bad name
(`ErrBuiltinName`) from a double load (`ErrFunctionExists`) from a malformed
registration (`ErrBadFunction`).

## 5. An aggregate in Go

An `Aggregate` is a factory; each `Accumulator` it makes holds one group's state.

```go
type Accumulator interface {
	Add(v classad.Value)        // fold one row's value
	Merge(other Accumulator)    // fold another partial state into this one
	Result() classad.Value      // render the answer; repeatable, non-mutating
}
```

This is what a user-defined aggregate is good at. The built-in `SUM` and `AVG`
keep every value in the group so the reference implementation can compute the
final answer, which makes their state O(group size). Yours can be O(1): the
worked example is a standard deviation by Welford's method, three floats however
large the group.

`NewAccumulator` returns the answer for a group with no rows. For a mean or a
standard deviation that is UNDEFINED, not zero.

`Add` sees **every** row in the group, including rows where the attribute is
missing or is a string. Skip those if you want to; `SUM` does.

**`Merge` must be associative and commutative.** The engine folds one state per
worker across segments in no fixed order, so an aggregate that cares about order
will be wrong in production and right in every test you write.
`db.CheckAggregate` checks this against a sample you supply — pass at least four
values, including the awkward ones.

Floating-point arithmetic cannot satisfy those laws exactly, because float
addition is not associative. Use `db.CheckAggregateEqual` with a tolerance:

```go
db.CheckAggregateEqual(stdDev{}, samples, closeEnough)
```

### Making it fast

By default your aggregate is fed one decoded record at a time. The built-in
numeric aggregates are not: they read the attribute as a column, skipping whole
blocks that cannot contain a match, with no decoding and no boxing. Implement
`ColumnAccumulator` and you get the same treatment — worth 8.4x ungrouped and
6.4x grouped on 50k records.

```go
type ColumnAccumulator interface {
	Accumulator
	AddNum(v NumValue)   // one value straight from a numeric column
}
```

Writing that method is a promise about two things nothing can check for you, and
breaking either gives one query two answers that both look right:

1. **Order does not matter to you.** The column pass visits segments and blocks
   in its own order, which is neither the record order nor insertion order, and
   which rung serves a query changes as data is written.
2. **You only count numbers.** The pass yields a column's numeric values and
   nothing else, so a row whose attribute is missing or is a string never reaches
   you — where `Add` would have been called for it. An aggregate that counts rows
   it was given, or that treats UNDEFINED as a contribution, must not implement
   this.

`Result` must also work from `AddNum` alone, since the column pass never calls
`Add`.

### Using one

```go
rows, err := d.AggregateColsStats("true",
	[]db.GroupCol{{Attr: "Pool"}},
	[]db.AggSpec{
		{Func: db.AggCount, Arg: "*"},
		{Func: db.AggUser, UserFunc: "stddev", Arg: "Memory"},
	}, nil, nil)
```

```
"cm1" count=3 stddev=1277.1523880188386
"cm2" count=3 stddev=0
```

Naming an unregistered aggregate fails the query rather than answering UNDEFINED,
because a column of "undefined" is a wrong answer that does not look like one.

**Over RPC**, a request can name your aggregate, so a remote client reaches one.
What crosses is the **name**: the aggregate has to be registered in the server's
process, since that is where the reduction runs. A name the server does not know
fails the query rather than answering a column of UNDEFINED, and a server too old
to carry names refuses rather than quietly answering a different aggregate.

The materialized view layer still cannot name one.

## 6. WebAssembly

### Loading a module

```go
l, err := wasmudf.NewLoader(ctx, wasmudf.Options{
	MemoryLimitPages: 16,                     // 1 MiB of linear memory
	Steps:            1 << 20,
	Timeout:          100 * time.Millisecond,
})
defer l.Close(ctx)

u, err := wasmudf.Register(ctx, l, wasmudf.ClassAdRegistry(), "pctUsed", 2, 2, wasm)
```

That is the whole integration. `pctUsed(Used, Total)` now runs in the guest, and
the expression cannot tell the difference: the outputs in section 4 are the same
function written in `.wat`, and they match.

Loading happens before registration, so a name is only ever registered against a
module that compiled, exported what it must, and ran once. Since nothing can be
unregistered, that order matters.

`Options` is yours, not the module's. Zero means the default:

| field | default | bounds |
|---|---|---|
| `MemoryLimitPages` | 128 (8 MiB) | the guest's memory; its `grow` simply fails |
| `Steps` | 10,000,000 | the step budget |
| `Timeout` | 1s | wall clock |
| `RegionBytes` | 128 KiB | the call frame: argument slots and string bytes |
| `MaxResultBytes` | 64 KiB | the longest string a guest may return |
| `MaxBatch` | 4096 | records per vector call |
| `MaxBatchArgs` | 4 | argument columns per vector call |
| `MaxStateBytes` | 16 KiB | an aggregate's state |
| `AggBatch` | 1024 | values folded per aggregate call |
| `MaxInstances` | `GOMAXPROCS` | concurrent instances of one module |
| `OnFailure` | nil | a callback per failure, for logs and metrics |

### Two limits on resource use

A **step budget**, charged by the guest itself through the one import the host
offers, `classad_udf.fuel(cost)`. It is deterministic: the same arguments run out
at the same point on a busy machine and an idle one, so a module that hits it
fails reproducibly and can be tested. It is also cooperative — a module that never
calls `fuel` is never charged.

A **wall-clock deadline**, enforced against every guest. This is what stops a bare
`(loop (br 0))`.

The budget is the real limit and the deadline is the backstop. A limit measured in
wall clock makes the same input pass on one machine and fail on another, so the
failure cannot be reproduced or tested. The two are reported as different reasons
so a test can assert a module only ever hits the deterministic one.

### Trusting a module

`LoadOptions.Trusted` compiles the module into a runtime that cannot be
interrupted and drops the wall-clock deadline, which is most of the scalar path's
overhead. It does not relax the step budget, the memory cap, or any validation of
what the guest returns.

Trust is configuration. Nothing in a module is consulted — not an export, not a
custom section, not a name — and there is no way for a guest to ask, because a
capability a module can claim is a request rather than a capability. What you give
up is liveness: a trusted module that loops forever hangs its goroutine with
nothing able to stop it, which is why the decision belongs to whoever configured
the pool.

### Writing a guest

**[docs/wasm-udf-abi.md](wasm-udf-abi.md) is the contract** — both calling
conventions, ownership, lifetime, and what is stable — for a module author in any
language that compiles to wasm. In outline:

```wat
(memory (export "memory") 1)
(func (export "classad_udf_abi_version") (result i32))
(func (export "classad_udf_call") (param i32 i32 i32) (result i32))
```

Values cross as 16-byte tagged slots: tag, byte length for strings, payload.
UNDEFINED and ERROR have tags rather than being signalled out of band, for the
reason in section 3. The status return is not how a guest reports ERROR — it can
put ERROR in the slot — it tells the host whether the slot was written at all. The
host poisons the slot before each call, so a guest that forgets produces a clear
failure instead of the previous record's answer.

A guest may also export a **vector** entry point, called once per batch instead of
once per record. That is the 28x in section 2, and it is the single biggest thing
a module author can do. A vectorized guest can hand awkward records back to its
own scalar implementation, so it never has to express every case in batch-shaped
code.

Complete worked guest:
[`wasmudf/testdata/examplepct.wat`](../wasmudf/testdata/examplepct.wat), the same
`pctUsed` as section 4 in about 50 lines.

### An aggregate in WebAssembly

An aggregate is a different module, not a different function — five exports under
their own version, and a module is one or the other.

```go
a, err := wasmudf.RegisterAggregate(ctx, l, wasmudf.DBRegistry(), "wasmMean", wasm)
```

```wat
(func (export "classad_udaf_abi_version") (result i32))
(func (export "classad_udaf_init")   (param i32 i32) (result i32))
(func (export "classad_udaf_fold")   (param i32 i32 i32 i32 i32 i32) (result i32))
(func (export "classad_udaf_merge")  (param i32 i32 i32 i32 i32 i32) (result i32))
(func (export "classad_udaf_result") (param i32 i32 i32) (result i32))
```

Produce the identity state, fold a batch of values into a state, merge two states,
render a state as a value. The state is **bytes the host holds**, not a pointer
into guest memory: an accumulator holding a pointer would pin the instance that
made it, and when the column pass declines, accumulators are dropped without
`merge` or `result` ever being called, so there would be no moment at which that
memory was known to be free.

The optional export `classad_udaf_fold_num` is the wasm spelling of
`ColumnAccumulator`, with the same two promises from section 5.
`UDAF.Columnar()` reports whether a module made them. `db.CheckAggregate` works on
`a.Aggregate()` exactly as on a Go one.

Complete worked guest:
[`wasmudf/testdata/examplemean.wat`](../wasmudf/testdata/examplemean.wat).

### When a guest misbehaves

A wasm function can only ever produce a load error or the ClassAd value ERROR;
nothing becomes a panic. "It returned ERROR" is not one event, so each call
carries a `Reason`:

| reason | what happened |
|---|---|
| `ReasonNone` | not a failure: the guest computed ERROR on purpose |
| `ReasonArity` | wrong number of arguments |
| `ReasonUnsupportedArg` | a list or nested ad; refused before the guest ran |
| `ReasonArgsTooLarge` | arguments exceed `RegionBytes` |
| `ReasonInstance` | no instance could be created |
| `ReasonTrap` | the guest trapped |
| `ReasonStepLimit` | the step budget ran out — deterministic |
| `ReasonTimeout` | the wall clock ran out — not deterministic |
| `ReasonStatus` | the guest declined to answer |
| `ReasonBadResult` | success reported, but the slot holds no value |
| `ReasonPanic` | the host call path panicked; unreachable, counted anyway |
| `ReasonBatchTooLarge`, `ReasonBatchStrings`, `ReasonBatchShape` | the vector path declined; no record got ERROR, it just did not run |
| `ReasonAggState` | an aggregate's state does not fit |

The three batch reasons are counted, and `OnFailure` fires for them, even though
the answers are fine. A vector path that has quietly turned itself off shows up
only as a query being slower than it should be, which is the hardest kind of
problem to find.

`UDF.Stats()` and `UDAF.Stats()` break the counts down by reason. A guest failing
every call is visible there and nowhere else.

## 7. ClassAd Script

ClassAd Script is a statement language over the same engine: locals, `if`,
`while`, `for`, and a result, compiled to one instruction stream so the optimizer
can see across the whole thing. It exists for what a single expression says badly
— a loop, or a derived value used three times.

```go
sp, err := vm.ParseScript(`
# Effective memory request, with a floor and an explicit absence case.
want = MY.RequestMemory
if want is undefined:
    want = 2048
if want < 512:
    want = 512
result = want
`)
v := sp.Run(ad)
```

```
[RequestMemory = 4096]   -> 4096
[RequestMemory = 64]     -> 512
[Owner = "alice"]        -> 2048
```

Python-shaped statements over ordinary ClassAd expressions: `is` is `=?=`, and
every expression between the statements is parsed by the same parser an
attribute's value is. Blocks are introduced by `:` and delimited by indentation,
which has to be spaces — braces were not available, since `{...}` is already a
list literal and `[...]` an ad literal. `elif`, `else`, `while`, `for`, `break`,
`continue`, `pass`, `def`, `return` and the compound assignments (`+=`, `-=`,
`*=`, `/=`, `%=`) are there; writes back to the ad are not.

**`for NAME in SEQUENCE:`** walks a list:

```
largest = 0
for request in MY.Requests:
    if request > largest:
        largest = request
result = largest
```

The sequence is evaluated once, before the first trip. `NAME` is an ordinary
local — function-scoped like any other, still bound after the loop, holding the
last element visited. An empty list runs the body zero times. A sequence that is
**not a list** is a fault: the script ends with `error`, the same way an `if` on
an undefined condition does, so a misspelled attribute says so rather than
turning the loop into one that quietly does nothing. The step budget bounds the
loop; there is no separate iteration limit.

**`break` and `continue`** work in either loop. `break` leaves the innermost
enclosing loop and `continue` starts its next iteration — re-testing a `while`
condition, or advancing to the next element of a `for`. Both take no operand,
and both are refused with a line and column where no loop encloses them:

```
i = 0
total = 0
while i < 100:
    i = i + 1
    if i % 3 == 0:
        continue        # skip the multiples of three
    if total > 50:
        break           # and stop as soon as we have enough
    total = total + i
result = total
```

`ParseScriptBuilder` returns the script before compilation, for a caller that
wants to add to it. The builder API it returns is also usable directly.

**The binding rule is Python's.** A bare name that has been bound is the local;
anything else is an attribute. So `want = MY.RequestMemory` binds a local, and
every later `want` is that local, while `MY.want` always means the attribute.

**A local can be indexed.** `seq = MY.Sizes` followed by `seq[0]` reads the
local, and `rec["K"]` reads a key out of a nested ad the same way. An index past
the end of a list is `error` rather than `undefined`, which is the reference
engine's answer, so `?:` — which defaults only an `undefined` — does not catch
it.

**A local reads the same everywhere it appears.** A few constructs are handed to
the tree-walking evaluator rather than compiled — a selection, a lazy built-in
such as `strcat` — and they run with the script's locals overlaid on the ad, so a
bare bound name there is the local:

```
x = Cpus + 1
result = strcat(x, " cpus")     # the lazy call reads the LOCAL x
```

`MY.x` is the attribute there as everywhere, which is how you say you meant the
ad.

**Three places still cannot read a local**, and each is refused with a line and
column rather than answered from the ad:

```
x = Cpus
result = {x, 1}                     # a list literal
result = [a = x].a                  # an ad literal
result = countMatches(x, MY.GPUs)   # a per-ad context
```

Each of those evaluates in a scope of its own. A list literal is the clearest
case: its elements are evaluated on demand, in a scope captured when the list was
built, which may be long after the script has finished and its locals are gone.
Bind the whole construct to a local first, or write `MY.x` if the attribute was
what you meant.

**A script can define its own functions**, for a rule it applies more than once:

```go
sp, err := vm.ParseScript(`
def fits(requested, cap):
    if requested is undefined:
        return Optimistic
    return requested <= cap

result = fits(MY.RequestMemory, 8192) && fits(MY.RequestCpus, 16)
`)
```

A `def` goes at the left margin — not inside an `if`, a `while` or another
`def` — and the whole script can call it, including the lines above it. The
parameter list may be empty. `return` with no value, and running off the end of
the body, both give `undefined`.

Inside the body, a bare name is one of three things: a parameter, a name the
body itself assigns, or an attribute of the ad. The locals of the code that
called it are *not* among them, so a function can be moved around the script
without its names colliding with anything. `Optimistic` above is none of the
first two, so it is read from the ad — a function is a function of the ad as
well as of its arguments, and one with no parameters at all is still useful.

Every argument is evaluated, left to right, before the body starts, even where
the body never reads it.

A `break` or a `continue` in a body names a loop **in that body**, and is
refused when there is none — even where the call itself sits inside a loop. The
call is written out where it appears, so a `break` bound by the call site would
leave the caller's loop, and the same function would mean one thing where it is
called from a loop and nothing at all where it is not. A function that wants to
stop a caller's loop returns a value the caller breaks on.

Three things a script function will not do:

- **Recursion**, directly or in a cycle through other functions. A call is
  written out in full where it appears, and something that contains itself
  cannot be written out. The error names the cycle.
- **Shadowing.** A `def` may not take the name of a built-in or of a registered
  function, for the same reason a registration may not: `strcat` has to mean the
  same thing inside the script as outside it. In a *module* only the built-in
  half of that applies, because a module's own functions are the ones being
  registered; whether the name is free is answered by the registry, as
  `ErrFunctionExists`.
- **Being called from a construct that is evaluated rather than compiled** — a
  list or ad literal, a selection, a lazy built-in. That evaluator resolves a
  function name against the built-ins and the registered functions, and a
  script's own function is in neither, so the call would be `error`. The refusal
  carries a line and column. Call the function from a statement and use the
  local, which a ternary or a subscript can read.

A function defined inside a script belongs to that script, and nothing outside it
can call one. A script can also *call* a UDF, which is how one rule is shared
between scripts.

### Registering a script function

A **script module** is source that defines functions and produces no value —
`def` at the left margin and nothing else, no `result`. Register one of its
functions and every expression in the process can call it, exactly as it calls a
built-in:

```go
mod, err := vm.ParseScriptModule(`
def examplePct(used, total):
    if used is undefined:
        return undefined
    if total is undefined:
        return undefined
    if total <= 0:
        return error
    return used * 100.0 / total
`)

f, err := scriptudf.Register(mod, scriptudf.ClassAdRegistry(), "examplePct")
```

```
examplePct(Used, Total)     37.5
EXAMPLEPCT(Used, Total)     37.5
examplePct(Used, Absent)    undefined
examplePct(Used, Label)     error
```

`mod.Functions()` lists what a module exports. There is no arity to supply: the
`def` states how many arguments it takes, and that is the minimum and the maximum.

**A registered script function cannot read the ad.** Inside a script, a bare name
the body does not bind is an attribute of the subject ad, and that is most of what
makes a script function useful there. A registration has no subject ad, so
`def fits(requested, cap): return Optimistic` is refused rather than registered —
along with `MY.RequestMemory`, a call to `random()`, a list literal and a
selection. Pass what the function needs as a parameter.

A body may use the operators and constructs the compiler lowers natively, over
its parameters: arithmetic and comparison, `&&` and `||`, `?:`, a ternary
`c ? t : f`, a subscript, `ifThenElse` and the strict built-ins. The error says which rule refused it
and what to write instead, and `errors.Is(err, vm.ErrReadsAd)` tells that refusal
apart from a function that simply does not compile.

Everything else in section 3 holds. Compilation happens before registration, so a
name is only ever registered against a function that compiled; registration is
append-only; names fold case; and a call that runs too long gives ERROR rather
than hanging the scan, because a script may loop.

The language's design, and what remains to build, are in
[docs/SCRIPTING_LANGUAGE_DESIGN.md](SCRIPTING_LANGUAGE_DESIGN.md).

## 8. Files

| | |
|---|---|
| [docs/wasm-udf-abi.md](wasm-udf-abi.md) | the guest-side ABI, for a module author |
| [docs/SCRIPTING_LANGUAGE_DESIGN.md](SCRIPTING_LANGUAGE_DESIGN.md) | ClassAd Script's design and status |
| [docs/EVALUATION_API.md](EVALUATION_API.md) | the evaluation API and the built-in functions |
| `wasmudf/testdata/*.wat` | one fixture per point of the ABI, with `build.sh` |

The samples above, all run by `go test`:

| | |
|---|---|
| `classad/example_udf_test.go` | the Go function |
| `collections/vm/example_scriptparse_test.go` | the ClassAd Script above |
| `collections/vm/example_script_test.go` | the same script through the builder API |
| `collections/vm/example_scriptfunc_test.go` | the script function above |
| `collections/scriptudf/example_test.go` | registering a script module's function |
| `db/example_udaf_test.go` | the Go aggregate and `CheckAggregateEqual` |
| `wasmudf/example_test.go` | loading and registering both kinds of wasm module |
| `wasmudf/testdata/examplepct.wat` | the wasm function |
| `wasmudf/testdata/examplemean.wat` | the wasm aggregate |
