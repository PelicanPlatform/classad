# User-defined functions and aggregates

**Status:** implemented
**Audience:** someone adding a function or an aggregate to this engine, and the
admin deciding whether to allow one

ClassAd ships a fixed set of built-in functions, and the database layer a fixed
set of aggregates (`COUNT`, `COUNT(DISTINCT)`, `SUM`, `AVG`, `MIN`, `MAX`).
This page is about the four ways to add your own, what each one costs, and the rules all four
share — because the rules come from the engine rather than from the language you
write the extension in, and they are the part that is easy to get wrong.

Every code sample below is an excerpt from a file that `go test` runs. They are
listed with the rest of the links at the bottom; if a sample here disagrees with
the file, the file is right and this page has rotted.

---

## 1. The four registries

|  | scalar function | aggregate |
|---|---|---|
| **Go** | `classad.RegisterFunction` | `db.RegisterAggregate` |
| **WebAssembly** | `wasmudf.Register` | `wasmudf.RegisterAggregate` |

A **scalar function** is called once per value: it appears in a `Requirements`
expression, a projection, a constraint — anywhere a built-in like `pow()` can.

An **aggregate** reduces a group of rows to one answer, named in an `AggSpec`
as `AggUser` plus its registered name, and reduced by the same `GROUP BY` core
that reduces with `SUM`.

The WebAssembly rows are not a second mechanism. `wasmudf` loads a guest module,
wraps it in the Go signature the registry above it already takes, and registers
it there. Everything in sections 3 and 4 is therefore true of a wasm extension
too; sections 5 and 6 are only what the sandbox adds.

## 2. Choosing between Go and WebAssembly

The question is not performance. It is **whose code is this**.

| | Go | WebAssembly |
|---|---|---|
| who writes it | you, in your own binary | anyone; a site admin, a user |
| what it can reach | everything your process can | nothing — no I/O, clock, randomness or WASI |
| how it fails | a bug in your daemon | a bounded failure, classified and counted |
| when it is loaded | compiled in | at run time, from bytes |
| cost per call | a Go call | see the table below |

Write it in Go when it is part of the system you are building. Reach for
WebAssembly when the function comes from outside the trust boundary — then the
isolation is the whole point, and the overhead is what it costs.

Measured per element on an Apple M2 Pro, for a guest that doubles a number:

| path | ns/element |
|---|---|
| scalar, untrusted | 782 |
| scalar, trusted | 106 |
| batch of 4096, untrusted | 27.9 |
| batch of 4096, trusted | 6.3 |

Batching is worth 28x on its own and 17x on top of trust, so the two are not
substitutes. A Go function has none of this overhead at all.

## 3. Rules every extension follows

**A function is a pure function of its argument values.** It is handed values and
nothing else: no evaluator, no scope, no AST. It therefore cannot read an
attribute, resolve a reference, or materialize a lazy list. This is not a rule
the engine polices — it is the shape of the signature, and it is what lets a
compiled program call your function natively instead of pulling the whole
expression back onto the tree-walking evaluator. An expression that gets
delegated loses every columnar accelerator with it.

The price is honest: a function handed a list cannot look inside it, because
realizing a list literal's elements means evaluating them in the scope they were
written in. Such a function should return ERROR.

**UNDEFINED and ERROR are values, and which one to return is your decision.**
There is no error return. An attribute the ad does not carry arrives as
UNDEFINED, and a function that answered ERROR for it would make a `Requirements`
expression *reject* the machine where it meant to ignore the term. A string where
a number belongs is a different thing and deserves ERROR.

**Report failure by returning, not by panicking.** A panic from a scalar function
is contained — it becomes ERROR — but containment is a backstop, not an
interface. The containment exists because this library runs inside daemons with
no top-level recover, where one record's arithmetic would otherwise take down a
process serving everything else.

An aggregate is the opposite, deliberately: a failing aggregate fails the whole
**query**. One record evaluating to ERROR says nothing about the others, but a
group's answer is a function of every row in it, so a state that stopped being
updated part-way is a wrong answer indistinguishable from a right one.

**Registration is append-only. There is no unregister.** Register during start-up,
before anything compiles. For scalar functions the reason is structural:
compiling an expression captures the decision that a name was a UDF, programs are
cached and shared, and withdrawing a registration would leave cached programs
calling something that is gone. Registering a name twice fails rather than
overwriting, so what a name means cannot depend on package initialization order,
and no registration may shadow a built-in.

**Names fold case,** like every function name in this language.

## 4. A scalar function in Go

```go
err := classad.RegisterFunction("pctUsed", 2, 2, func(args []classad.Value) classad.Value {
	// Undefined in, undefined out.
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

`2, 2` is the arity range; a negative maximum means variadic. After that,
`pctUsed(Used, Total)` is an expression like any other:

```
pctUsed(Used, Total)     37.5
PCTUSED(Used, Total)     37.5
pctUsed(Used, Absent)    undefined
pctUsed(Used, Label)     error
```

`classad.StrictFunction("pctUsed", 2)` reports `true` — that is the property the
whole registration exists for, and the thing to check if a query that uses your
function is unexpectedly slow.

Registration failures come back as sentinels, so a loader handling a batch of
them can tell apart the three cases that need different responses:
`ErrBuiltinName` (a mistake in what it was asked to load), `ErrFunctionExists`
(usually a double load it can ignore), and `ErrBadFunction` (a bug in whatever
produced the registration).

## 5. An aggregate in Go

An `Aggregate` is a factory; the `Accumulator` it makes is one group's state.

```go
type Accumulator interface {
	Add(v classad.Value)        // fold one row's value
	Merge(other Accumulator)    // fold another partial state into this one
	Result() classad.Value      // render the answer; repeatable, non-mutating
}
```

This is the shape a user-defined aggregate is *for*. The built-in `SUM` and `AVG`
retain every value in the group so the final answer can be computed by the
reference implementation — their state is O(group size). Yours can be O(1). The
worked example is a standard deviation by Welford's method, which keeps three
floats whatever the group holds.

`NewAccumulator` returns the **identity**: the answer for a group that
contributed no rows. For a mean or a standard deviation that answer is UNDEFINED,
not zero — there is no mean of nothing, and zero invents one.

`Add` is handed **every** row in the group, including rows whose attribute is
absent or is a string. Skipping those is your decision, and it is the same one
`SUM` makes.

### The merge laws are not advice

`Merge` must be **associative and commutative**. The engine fans the aggregate
pass out across segment windows — one state per worker, folded once every worker
has returned — and which worker draws which segment is a race. Two storage groups
that render to the same label are also merged. An order-sensitive merge is
correct in every test its author writes, because an author tests the order their
own code produces, and wrong in production, plausibly and only sometimes.

`db.CheckAggregate` tests those laws over a sample **you** supply, because only
you know what your aggregate eats:

```go
fmt.Println(db.CheckAggregateEqual(stdDev{}, samples, closeEnough))
```

Pass at least four values, of the shape the aggregate will really see, and
include the awkward ones — UNDEFINED, an error, a string where a number belongs,
duplicates — since that is where merge and identity usually part company. A pass
is evidence, not proof: the laws are universally quantified and this is one finite
sample.

The comparison is yours because **the laws cannot be exact for floating-point
arithmetic.** Float addition is not associative, so a variance or a mean gives
answers differing in the last bits depending on how the rows were divided. That
*is* the order dependence the law is about, and it cannot be removed; an
aggregate needing slack says so here, in its own tests, where a reader can see
how much. `CheckAggregate` is `CheckAggregateEqual` with an exact comparison,
which is right for an integer aggregate and wrong for a float one.

### Opting into the columnar pass

By default an aggregate is served from the **record scan**: every matching record
decoded, its argument evaluated into a `classad.Value`, one `Add` per row. The
built-in numeric aggregates are not — they read the argument as a contiguous
column, with whole blocks ruled out by zone maps before any value is touched, and
no decode and no boxing for what survives.

Implement `ColumnAccumulator` and you are on that pass too:

```go
type ColumnAccumulator interface {
	Accumulator
	AddNum(v NumValue)   // one value straight from a numeric column
}
```

Measured on 50k records, the gap this closes is 8.4x ungrouped and 6.4x grouped.

**Implementing it is a declaration of two things no signature can check**, and
getting either wrong gives one query two answers that both look right:

1. **Order.** The pass walks segments, then blocks, skipping blocks a zone map
   rules out. That is not the record scan's order and not insertion order. Which
   rung serves a query depends on what is sealed and what the constraint is, so
   it changes under a caller who changed nothing.
2. **What is offered.** The pass yields only the column's **numeric** values. A
   row whose argument is absent, undefined, a string or an expression is never
   offered — where the record scan calls `Add` for it. An aggregate that counts
   what it was given, or that treats UNDEFINED as a contribution, must not
   implement this interface: it would count different things on the two rungs.

An aggregate that already skips non-numbers — which is what `SUM`, `AVG`, `MIN`
and `MAX` all do — loses nothing. `Result` must also work from `AddNum` alone,
since a query served by the columnar pass never calls `Add`.

An aggregate that cannot make those promises stays on the record scan, which is
correct and slower, never wrong.

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

Naming an aggregate that is not registered **fails the query**. It is not
answered as undefined: a query asking for `p95` and receiving a column of
"undefined" has been answered wrongly, where a query that fails says what
happened.

### What a user-defined aggregate still cannot do

It cannot reach the archive's index-resident `GROUP BY` count, which attributes a
whole segment's postings without visiting a value. (`SUM`, `AVG`, `MIN` and `MAX`
do not use that one either, so it is not part of the gap.)

It cannot cross the RPC boundary. No aggregate frame has a field for a *name*, so
`dbrpc` refuses `AggUser` at both ends rather than sending a frame that would
arrive as a lookup of the empty name. A user-defined aggregate is for a caller
embedding this engine, or for the server side of one.

The materialized-view layer cannot name it either.

## 6. A scalar function in WebAssembly

### The host side

```go
l, err := wasmudf.NewLoader(ctx, wasmudf.Options{
	MemoryLimitPages: 16,                     // 1 MiB of linear memory
	Steps:            1 << 20,                // the deterministic budget
	Timeout:          100 * time.Millisecond, // the backstop
})
defer l.Close(ctx)

u, err := wasmudf.Register(ctx, l, wasmudf.ClassAdRegistry(), "pctUsed", 2, 2, wasm)
```

That is the whole integration. `pctUsed(Used, Total)` now evaluates in the guest,
and the expression cannot tell the difference — the outputs in section 4 are the
same function written in `.wat`, and they match.

Loading comes first and registration second, which is the order that matters:
every static problem with the module is a returned error from the load, so a name
is only ever registered against a module that compiled, exports what it must,
speaks the ABI, instantiated, and has been called once. Neither registry has an
unregister, so registering something unproven cannot be taken back.

`Options` is the sandbox, and every field is the admin's. Zero means the default:

| field | default | what it bounds |
|---|---|---|
| `MemoryLimitPages` | 128 (8 MiB) | the guest's linear memory; a hard cap, its `grow` simply fails |
| `Steps` | 10,000,000 | the deterministic step budget (see below) |
| `Timeout` | 1s | the wall-clock backstop |
| `RegionBytes` | 128 KiB | the host-owned call frame: slots plus string arguments |
| `MaxResultBytes` | 64 KiB | the longest string a guest may return |
| `MaxBatch` | 4096 | records per vector call |
| `MaxBatchArgs` | 4 | argument columns per vector call |
| `MaxStateBytes` | 16 KiB | an aggregate's serialized state |
| `AggBatch` | 1024 | values folded per aggregate call |
| `MaxInstances` | `GOMAXPROCS` | concurrent instances of one module |
| `OnFailure` | nil | a callback per failure, for logging and metrics |

The module cannot influence any of it, and cannot ask for more than the sandbox
offers: the host exposes exactly **one** import, so a guest wanting a clock or a
file cannot be instantiated and is rejected at load, naming what it asked for.
That is not a policy applied to guests — it is the absence of anything to call.
A function that could read the clock would also not be pure, and so could not be
compiled into a query at all.

### The guest side

A module exports a `memory`, an ABI version, and one call:

```wat
(memory (export "memory") 1)
(func (export "classad_udf_abi_version") (result i32))
(func (export "classad_udf_call") (param i32 i32 i32) (result i32))
```

Values cross as 16-byte tagged slots, little-endian, naturally aligned — tag,
byte length (strings only), payload. UNDEFINED and ERROR have **tags**, not
out-of-band signalling, for the reason in section 3: they are values, and a host
that mapped them onto a status code would turn "this argument was missing" into
"this function broke".

The status return is not how a guest reports ERROR — it can encode ERROR in the
slot. It exists so the host knows whether the slot was **written at all**. The
host poisons the result slot with an impossible tag before every call, so a guest
that returns success without writing produces a defined failure instead of the
*previous* record's answer.

The complete worked guest is
[`wasmudf/testdata/examplepct.wat`](../wasmudf/testdata/examplepct.wat) — the
same `pctUsed` as section 4, about 50 lines of `.wat`, with the undefined and
error cases handled the same way for the same reasons.

**[docs/wasm-udf-abi.md](wasm-udf-abi.md) is the contract**, in prose, for
someone writing a guest in any language that compiles to wasm: both calling
conventions, ownership and lifetime, what is stable. Read it before writing a
module. What follows here is only the part an integrator and an admin need.

### The vector entry point is where the performance is

A module always exports the scalar entry point, called once per record. It **may**
also export a vector one, called once per batch, which is what gets a guest into
the vector engine's hot loop instead of pulling the query off it — the 28x in
section 2.

The relationship between the two is half the design: a vector guest may hand
individual records back to its own scalar implementation, so it never has to
express every awkward case in batch-shaped code. Requiring the scalar one is what
makes that escape hatch unconditional — there is no module shape in which a
declined record has nowhere to go.

`UDF.Vectored()` reports whether a loaded module has one.

### Trust

`LoadOptions.Trusted` compiles a module into a runtime that cannot be interrupted
and runs with no wall-clock deadline, which is where almost all of the scalar
path's overhead was.

It does **not** relax the step budget, the memory cap, the poisoned result slot,
the validation of what a guest returns, or discarding an instance that trapped.
The measurement is why: the budget is not where the time goes, and it is the only
bound whose failures are reproducible, so relaxing it would buy nothing and cost
the one property that makes a trusted module and an untrusted one comparable on
the same input.

Trust is **configuration**. Nothing in a module is consulted — not an export, not
a custom section, not a name — and there is deliberately no ABI through which a
guest could ask, because a capability a module can assert is a request rather
than a capability. What a trusted module gives up is liveness: one that loops
forever hangs its goroutine with nothing able to stop it, which is exactly why
the decision belongs to the party that configured the pool.

### Two limits, and why they are not one

A **step budget**, in units the guest declares by calling the one import the host
offers, `classad_udf.fuel(cost)`. It is deterministic: the same arguments exhaust
it at the same point on a loaded machine and an idle one, which is what makes a
guest that hits the limit reproducible and therefore testable against a non-wasm
implementation of the same function. It is also **cooperative** — a module that
never calls `fuel` is never charged — so it bounds instrumented guests and
nothing else.

A **wall-clock deadline**, enforced against any guest at all. This is what stops
a bare `(loop (br 0))`.

The step budget is the contract and the deadline is the backstop, in that order.
A limit measured in wall clock makes the same input succeed on one machine and
fail on another, so the fault is not reproducible and cannot be tested against
anything. The two are reported as different reasons so a test can assert that a
given module only ever hits the deterministic one.

## 7. An aggregate in WebAssembly

An aggregate is a **different module, not a different function** — a separate
export set under its own version. A module is one or the other, because the two
have nothing in common at the boundary except the value slot.

```wat
(func (export "classad_udaf_abi_version") (result i32))
(func (export "classad_udaf_init")   (param i32 i32) (result i32))
(func (export "classad_udaf_fold")   (param i32 i32 i32 i32 i32 i32) (result i32))
(func (export "classad_udaf_merge")  (param i32 i32 i32 i32 i32 i32) (result i32))
(func (export "classad_udaf_result") (param i32 i32 i32) (result i32))
```

Four operations: produce the identity state, fold a batch of value slots into a
state, merge two states, render a state as one value slot.

```go
a, err := wasmudf.RegisterAggregate(ctx, l, wasmudf.DBRegistry(), "wasmMean", wasm)
```

**The state is bytes the host holds**, not a handle into guest memory, and the
reason is lifetime rather than speed. An accumulator holding a handle would pin
the instance that minted it, and the accumulator interface has no close — worse,
when the columnar pass declines, every worker's accumulator is dropped with
neither merge nor result ever called, so there is no moment at which guest memory
could even be inferred to be free. With the state in the host an accumulator is
an ordinary Go value, instances are borrowed per call, and merging two states
works whichever instances they were folded in. The copy that costs is amortized
by folding a **batch** of values per call.

The optional export `classad_udaf_fold_num`, with `fold`'s signature, is how a
guest opts into the columnar pass — the wasm spelling of `ColumnAccumulator`, and
the same two promises in section 5. A guest for which both hold exports the same
function under both names; `UDAF.Columnar()` reports whether it did.

A guest reports failure by refusing: `init`, `fold` and `merge` return the length
of the state they wrote or a negative number, and `result` returns a status. The
host turns that into a failed query, as section 3 says it must.

The complete worked guest is
[`wasmudf/testdata/examplemean.wat`](../wasmudf/testdata/examplemean.wat) — an
arithmetic mean over a 16-byte state. It is a mean rather than a sum because a
mean cannot be reconstructed from another group's answer: a merge that forgot the
count would be *wrong* rather than merely incomplete, which is the mistake the
laws exist to catch.

`db.CheckAggregate` works on a wasm aggregate exactly as on a Go one — pass
`a.Aggregate()`.

## 8. When something goes wrong

Everything a wasm **function** guest can do wrong is either a load error or the
ClassAd value ERROR. Nothing becomes a panic; a daemon evaluating an expression
runs no top-level recover, and a function in a `Requirements` expression would
take the daemon down one match at a time.

"The UDF returned ERROR" is not one event, so each call carries a `Reason`:

| reason | what happened |
|---|---|
| `ReasonNone` | not a failure — the guest computed its answer, and its answer is ERROR |
| `ReasonArity` | argc outside the registered range |
| `ReasonUnsupportedArg` | a list or nested ad; refused before the guest runs |
| `ReasonArgsTooLarge` | slots plus string bytes exceed `RegionBytes` |
| `ReasonInstance` | no instance could be created |
| `ReasonTrap` | the guest trapped |
| `ReasonStepLimit` | the declared step budget was spent — **deterministic** |
| `ReasonTimeout` | the wall-clock backstop fired — **not** deterministic |
| `ReasonStatus` | the guest declined to produce a value |
| `ReasonBadResult` | success reported, but the slot does not hold a value |
| `ReasonPanic` | the host call path panicked; unreachable by design, counted anyway |
| `ReasonBatchTooLarge`, `ReasonBatchStrings`, `ReasonBatchShape` | the vector path **declined**: no record got ERROR, it simply did not run |
| `ReasonAggState` | an aggregate's state does not fit the buffer it was given |

The three batch reasons are counted, and `OnFailure` fires for them, even though
nothing is wrong with the answers — a vector path that has quietly turned itself
off is otherwise visible only as a query being slower than it ought to be, which
is the hardest kind of problem to find.

`UDF.Stats()` and `UDAF.Stats()` return counters, including a per-reason
breakdown. A guest that is failing every call is visible there and nowhere else.

## 9. Where to look next

| | |
|---|---|
| [docs/wasm-udf-abi.md](wasm-udf-abi.md) | the guest-side ABI contract, in prose, for a module author |
| `wasmudf/testdata/*.wat` | one fixture per point of the contract, with `build.sh` to rebuild them |
| [docs/EVALUATION_API.md](EVALUATION_API.md) | the evaluation API and the built-in functions a UDF sits beside |

The samples on this page:

| | |
|---|---|
| `classad/example_udf_test.go` | the Go scalar function |
| `db/example_udaf_test.go` | the Go aggregate, and `CheckAggregateEqual` |
| `wasmudf/example_test.go` | loading and registering both kinds of wasm module |
| `wasmudf/testdata/examplepct.wat` | the wasm scalar guest |
| `wasmudf/testdata/examplemean.wat` | the wasm aggregate guest |
