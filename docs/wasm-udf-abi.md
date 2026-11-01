# Writing a WebAssembly user-defined function for ClassAd

**Status:** implemented, `classad/wasmudf` (its own Go module)
**ABI versions:** 1 and 2, both spoken by the host
**Audience:** someone writing a guest module, in any language that compiles to wasm

This describes the contract between the host (the `wasmudf` package) and a guest
module, in prose, so that you do not have to read the host's Go code or
reverse-engineer the example fixtures to know what is required of you. The
fixtures in `wasmudf/testdata/*.wat` are the worked examples; this is the
explanation.

There are **two** calling conventions. The scalar one is called once per record
and every module must implement it. The vector one is called once per batch of
records, is optional, and is where the performance is. The relationship between
them is half the design: a vector guest is allowed to hand individual records
back to its own scalar implementation, so it never has to express every awkward
case in batch-shaped code.

The vector convention comes in two shapes, and the number your module reports
from `classad_udf_abi_version` chooses which one it is handed. **Version 1**
carries a type tag beside every element, so one column can hold five kinds of
value. **Version 2** carries one type for the whole column and escalates the
records that do not fit it. If you are writing a new module, write version 2 —
section 4a says why, and it is a large difference rather than a tidier one.

---

## 1. What a UDF is, and what follows from it

A ClassAd UDF is a **pure function of its argument values**. It is handed values
and it returns a value. It cannot see the record it was called from, cannot look
up an attribute, and cannot ask the host anything.

That is not a rule the host enforces at runtime — it is a consequence of the
interface. The host offers your module exactly **one** import (a step counter,
section 7), so there is nothing else to call. No file system, no clock, no
randomness, no network, no WASI. A module that imports anything else is rejected
when it is loaded, and the error names what it asked for.

This matters to you in one practical way: a function that read the clock would
not be a pure function of its arguments, so it could not be compiled into a query
at all. The purity is what buys the speed.

### What you must export

```wat
(memory (export "memory") 1)                                     ;; required
(func (export "classad_udf_abi_version") (result i32))           ;; required, must return 1 or 2
(func (export "classad_udf_call") (param i32 i32 i32) (result i32))        ;; required
(func (export "classad_udf_call_vec") (param i32 i32 i32 i32 i32) (result i32)) ;; optional
```

The memory must be exported under the name `memory`. The version function must
return `1` or `2`; if it returns anything else the module is refused at load with
your number and the host's range in the message, rather than quietly reading
fields at offsets the host no longer writes.

The version is not merely checked, it is **kept**, and it decides which descriptor
forms your vector entry point is handed for the life of the module. That is the
only thing that decides it: not the batch, not a flag, not anything you can ask
for per call. Reading the wrong one of these two forms is the single mistake this
boundary cannot catch — every byte involved is legal and in bounds — so the choice
is made once, from a number you declare, and never guessed.

Anything else you export is ignored. In particular:

- `_start` is **not** called. wazero's default would call it at instantiation,
  which for a UDF would mean running your `main` once per pooled instance. If your
  module needs initialization, put it in the wasm **start section**, which
  instantiation always runs.
- Nothing you export or declare can grant your module any privilege. The trusted
  path in section 7 is the admin's decision and there is no way to ask for it.

---

## 2. A value: the 16-byte slot (scalar ABI)

Every value crossing the boundary in the scalar ABI is a **slot**: 16 bytes,
little-endian, naturally aligned.

```
offset  0   u32   tag
offset  4   u32   byte length    (strings only; zero otherwise)
offset  8   u64   payload
```

| tag | meaning   | payload                                        |
|-----|-----------|------------------------------------------------|
| 0   | UNDEFINED | unused                                         |
| 1   | ERROR     | unused                                         |
| 2   | boolean   | 0 or 1, and nothing else                       |
| 3   | integer   | the int64, two's complement                    |
| 4   | real      | the float64's IEEE-754 bits                    |
| 5   | string    | a byte offset in the **low 32 bits**; length at +4 |

### Why it is fixed width

Because you must be able to reach argument *k* by arithmetic: it is at
`argsPtr + k*16`. There is no length prefix to walk and no parsing loop in your
module — which is exactly where a hand-written guest is most likely to be wrong.
The host pays 16 bytes for a value that would pack into 9; at one call per record
that is a write that never leaves L1.

### Reading an argument, writing an answer

Your `classad_udf_call(argsPtr, argc, resultPtr)` receives:

- `argsPtr` — the first of `argc` consecutive slots
- `argc` — how many arguments you were given
- `resultPtr` — one slot, which is where your answer goes

Read argument 0's tag with `i32.load` at `argsPtr`, its length at `argsPtr+4`,
its payload with `i64.load` at `argsPtr+8`. Write your answer by storing a tag,
a length and a payload at `resultPtr`, then return `0`.

`testdata/tagecho.wat` is this in about forty lines: it doubles a number,
negates a boolean, echoes a string, passes UNDEFINED and ERROR through unchanged,
and refuses anything else.

### Strings

A string **argument** is bytes the host has written into its own region of your
memory. The slot's payload is the offset and `+4` is the length. Those bytes are
valid **only for the duration of this call** — see section 6.

A string **result** may point anywhere in your own linear memory, including back
at the bytes the host handed you. The host validates the pointer and copies the
bytes out before returning, so nothing of yours has to outlive the call. Three
rules the host enforces, each of which makes your result ERROR rather than a
wrong value:

- the payload's **upper 32 bits must be zero**. wasm32 offsets are 32 bits;
  a host that truncated a 64-bit payload would read a different, in-bounds region
  and return a plausible wrong string.
- the pointer and length must be **inside** your linear memory.
- the length must not exceed the configured cap (64 KiB by default).

A length of zero is the empty string and the pointer is not read.

### UNDEFINED and ERROR are values, not failures

This is the part most worth internalising. In ClassAd, UNDEFINED and ERROR are
**values**. `isUndefined(f(x))` is a question your caller is entitled to ask. So:

- You can be **handed** UNDEFINED or ERROR, and it is **your** decision what your
  answer is. You may return UNDEFINED, return ERROR, or return a number. The host
  does not apply a three-valued rule on your behalf in either direction.
- You can **return** either of them by writing tag 0 or tag 1 and returning status
  0. That is a successful call that produced a value, and the host counts no
  failure for it. An operator reading the failure counters will not be sent
  looking for a broken module that is working as written.

---

## 3. The status return, and why the host poisons your result slot

Your call function returns an `i32` **status**. It means one thing:

- `0` — "I have written a value to the result slot."
- anything else — "I have not; do not read it."

The status is **not** how you report ERROR. You can encode ERROR in the slot. The
status exists so the host knows whether the slot was *written at all*.

Why that needs saying: **instances are reused across records.** The host
instantiates your module once and calls it for record after record through the
same linear memory. So the bytes sitting in the result slot when you are called
are **the previous record's answer**. A guest that returned 0 without writing
would hand back the last record's value for this record, with a success status,
and nothing anywhere would report a problem.

So before every scalar call the host writes an **impossible tag** (`0xFFFFFFFF`)
over the result slot's tag field. If you return 0 and that tag is still there, the
host knows you wrote nothing and your call becomes ERROR. The two halves —
poisoning plus the status — are what make a forgetful guest loud instead of
silently wrong.

A nonzero status is cheap for you: if you cannot produce a value, say so and do
not bother encoding anything.

---

## 4. The vector ABI: one call per batch

If you also export `classad_udf_call_vec`, the host will hand you whole columns:

```
classad_udf_call_vec(n, argsPtr, argc, resPtr, declinePtr) -> status
```

- `n` — the **batch width**: how many records this call covers. It is a parameter,
  not something you have to discover. It is not fixed between calls; a later call
  may have a different `n`.
- `argsPtr` — the first of `argc` consecutive **batch descriptors**
- `argc` — how many argument columns
- `resPtr` — one more descriptor, describing where to write your output column
- `declinePtr` — a bitmask, one bit per record, for records you will not handle
  (section 5)

Which **forms** you are handed depends on the version you reported:

| you report | you may be handed |
|---|---|
| 1 | the data form (0) and the mask form (1) |
| 2 | the **columnar** form (2) and the mask form (1) — never the data form |

The status means the same thing it does in the scalar ABI: `0` says your output
column is written, anything else says it is not.

### A batch descriptor

16 bytes — the same size as a value slot, so you index descriptors the same way:
descriptor *k* is at `argsPtr + k*16`.

```
offset  0   u32   form      0 = data, 1 = mask
offset  4   u32   flags     bit 0 set = const
offset  8   u32   aPtr
offset 12   u32   bPtr
```

What `aPtr` and `bPtr` point at depends on `form`.

### The data form (`form == 0`)

Two parallel arrays:

- `aPtr` → **n state bytes**, one per record
- `bPtr` → **n i64 payloads**, one per record, 8-byte aligned

The state codes are **not the same numbers as the scalar tags**. They are the
query engine's own codes, because a column in this form is passed through
untouched rather than converted:

| state | meaning   | payload                              |
|-------|-----------|--------------------------------------|
| 0     | integer   | the int64                            |
| 1     | real      | the float64's IEEE-754 bits          |
| 2     | boolean   | 0 or 1                               |
| 3     | UNDEFINED | ignore it                            |
| 4     | ERROR     | ignore it                            |
| 5     | string    | **not carried; see below**            |
| 6     | dictionary code | **not carried; see below**      |

Yes, this is a second numbering, and yes it is a nuisance. It exists because the
alternative is a translation pass over every element of every column before your
code gets to run, on data the engine is handing over for free. The scalar slot's
tags are laid out for a single value; these are laid out for a column.

This is an int64 lane array with a parallel byte lane of states — the shape that
vectorizes.

### The mask form (`form == 1`)

Three-valued logic as **two bitplanes**, two bits per record:

- `aPtr` → the **hi** plane: `ceil(n/64)` u64 words
- `bPtr` → the **lo** plane: `ceil(n/64)` u64 words

Record *i*'s bit is bit `i % 64` of word `i / 64`. Read the pair:

| hi | lo | meaning   |
|----|----|-----------|
| 0  | 0  | FALSE     |
| 0  | 1  | TRUE      |
| 1  | 0  | UNDEFINED |
| 1  | 1  | ERROR     |

A **set hi bit obliges you to decide something**: the record is UNDEFINED or
ERROR, and it is your function's business what that means for its answer. A
**clear hi bit** lets you assume the lo bit is the whole value — an ordinary
boolean, no escapes. That is why the engine's own logical kernels branch once per
64 records on `hi == 0`: it is the common case, and it collapses the entire
three-valued apparatus to one bitwise AND.

This is the form a comparison in the engine already produces. A batch of 4096
records in the mask form is 1 KB; the same records in the data form are 36 KB.
`testdata/vecmaskarg.wat` reads it.

#### There is no "absent" plane, and this is deliberate

An attribute a record does not have arrives as **UNDEFINED** — state 3 in the
data form, `hi=1 lo=0` in the mask form. There is no third bitplane, no fourth
state and no flag distinguishing "this record has no such attribute" from "this
record's attribute evaluated to undefined". The engine does not make that
distinction and neither does the evaluator it has to agree with, so inventing one
here would be inventing semantics. If you are looking for an absent mask, this is
the answer: it is the undefined mask.

### 4a. The columnar form (`form == 2`, ABI version 2 only)

This is the data form with the state plane shortened to **one element**.

```
offset  0   u32   form      2
offset  4   u32   flags     bit 0 set = const
offset  8   u32   aPtr      ONE state byte: the state of every record in the column
offset 12   u32   bPtr      n i64 payloads (one, if const)
```

Same 16 bytes, same field offsets, same state codes 0–4. The only difference is
that `aPtr` points at a single byte rather than at `n` of them, so you read the
column's type **once, before your loop**, instead of once per element.

#### Why that is the whole point

A ClassAd column genuinely can hold an integer, a real and an UNDEFINED in three
consecutive records, which is what the data form is honest about. The cost of
that honesty is not the branch itself. It is that a loop with a branch per element
retires **one record per loop back-edge** — it cannot be unrolled, the lanes
cannot be made to agree, and nothing can be hoisted out of it. And a back-edge is
the unit an interruptible runtime charges for: measured on an Apple M2, about
20ns, against about 0.6ns for the arithmetic. So a byte that exists to describe
the rare record sets the price of every record.

With one state for the column there is nothing to branch on. The loop that remains
is a branch-free pass over int64 lanes, which you can unroll, and which you can
write with `v128` if you want to. Measured end to end over a 4096-record batch of
integers, per element:

| guest | untrusted | trusted |
|---|---|---|
| version 1, a branch per element | 25.9 ns | 5.29 ns |
| version 2, branch-free, one record per back-edge | 22.7 ns | 2.18 ns |
| version 2, branch-free, eight records per back-edge | 5.69 ns | 1.90 ns |
| version 2, the same eight as four `i64x2` lanes | 5.23 ns | 1.69 ns |

Read the last two rows together before you reach for SIMD. Most of the untrusted
win is **unrolling**, not vector lanes: the runtime is charging you per back-edge
and a vector instruction's only relevance is that it needs fewer of them. The
vector instructions themselves are worth about 10% on top. They are worth having
and they are not the reason this form exists.

#### The records that do not fit

A column is given one state, so the records whose value is not in that state
cannot be represented. They are not dropped and they do not get a worse answer:
**the host sets their bit in the decline mask before calling you**, and they take
the per-record scalar path, which is the same path version 1's escape hatch sends
them down. The host picks the state that covers the most records, so one UNDEFINED
in a column of integers escalates one record rather than all the others.

That has a consequence worth stating plainly, because it is what makes a
branch-free guest possible at all:

> **You may ignore the decline mask entirely.** Compute every lane, including the
> lanes of records you were never given. The host does not read a declined lane,
> so what you put there cannot be wrong.

So that this is safe rather than merely usual, the host writes a **benign value**
into those lanes before the call — integer 1, real 1.0 — rather than leaving the
bits the engine happened to have there. An operation that can trap on a value, an
integer division above all, cannot be made to trap by a record you were not given.
(It can still trap on a real one. A zero divisor is your problem either way.)

If you would rather skip the work, the mask is there and you may read it. What you
must not do is **clear** a bit in it — see section 5.

#### Your output column, when you report version 2

The result descriptor is also `form == 2`: `aPtr` is one state byte, `bPtr` is `n`
payloads. Write the state **once**, write the payloads, return 0.

```wat
(i32.store8 (i32.load offset=8 (local.get $res)) (i32.const 0))  ;; "this column is integers"
```

Everything the data-form result requires still holds: states 0–4 only, a boolean
payload is 0 or 1, and the host reads **its own** pointers rather than the ones in
the descriptor. One state describes the whole column, so one illegal state byte is
the whole column being a value of no kind, and the batch is refused.

If you declined every record, write nothing. There is no column, and the host does
not ask you to describe one.

#### What this form gives up, honestly

Version 1 poisons `n` result state bytes, so a guest that fills half a column and
declines nothing is caught at the first element it skipped. There is one state byte
to poison here, so that guest is caught **only if it also forgot the state**. In
exchange the host zeroes the payload array before every call, so the part you did
not write is deterministic zeros rather than the previous batch's answers for other
records. That is a smaller net and a stale-value leak closed, not a straight win.
Fill the column you described.

### The const flag

If `flags` bit 0 is set, **only element 0 of that column is meaningful** — every
record has the same value. Read element 0 and use it `n` times. Do not index the
array by record number; for a const column it has exactly one element and
anything past it is not yours.

This is not a micro-optimization the host could skip. A literal argument is the
commonest shape a UDF call in a query has, and materializing one would mean
writing nine identical bytes per record to say the same thing `n` times — the cost
the engine's own const representation exists to remove.

The flag means the same thing in the columnar form, where the column is one state
byte and one payload whatever `n` is.

One case you do **not** have to handle: if *every* argument is const, the host
computes the single answer through your **scalar** entry point and never calls the
vector one. So a const flag on every column is something you will not see.

### Your output column (version 1)

A version 2 guest writes the columnar result described above; this is the version
1 shape. The result descriptor describes the **data form**, with arrays the host
allocated: `aPtr` → `n` state bytes, `bPtr` → `n` i64 payloads. Write a state and
a payload for each record you handled.

You may write only states **0 through 4** (integer, real, boolean, UNDEFINED,
ERROR). States 5 and 6 are refused: this ABI carries no string storage in the
vector direction, so claiming one would be producing a value with nothing behind
it. A boolean element's payload must be **0 or 1**, exactly as in the scalar ABI
— the engine would read any nonzero payload as TRUE while the scalar path calls
it malformed, so allowing it here would let your own two entry points disagree
about the same input.

Every element the host reads is validated, and one bad element makes the whole
batch ERROR — a guest that got one element wrong has no claim to the others.

The result column is **poisoned** before every call, the same discipline as the
scalar slot and for the same reason: `0xFF` in every state byte, which is not a
state the engine has. If you return 0 and leave a poison byte behind for a record
you did not decline, that batch becomes ERROR rather than returning the previous
batch's column.

The host reads **its own** result pointers, not the ones in the descriptor when
your call returns. Rewriting the result descriptor to point at a buffer of your
own has no effect; the host will read the slots it allocated and poisoned. (The
descriptor is the host telling you where to write, not you telling the host where
to read — otherwise the poison would guarantee nothing, since you could point it
at any bytes of your own that happened to look legal.)

### What the vector ABI does not carry

- **Strings**, plain or dictionary-encoded. A column holding them declines the
  whole batch and the engine calls you per record instead. This is not a
  limitation worked around by the escape hatch: a string column is strings for
  *every* record, so escalating them one at a time would be the per-record path
  with a vector call in front of it.
- **Stored-width columns.** The engine sometimes holds a column in the width it
  occupies on disk — two or four bytes, signed or unsigned, and sometimes an odd
  fitted width like six. Exposing that would put four code paths in every guest
  and break them all the next time the block format changed, so the host widens it
  to int64 first. A two-byte column is eight lanes of a 128-bit vector against
  two, which is the argument for carrying it, and the measurements in section 4a
  are the argument against hurrying: the guest's loop is a quarter of a trusted
  call and a seventh of an untrusted one, so four times the lanes inside it moves
  a number that is not the one in front. It would be a new **form**, not a new
  field, so nothing here has a hole shaped like it.

Both may be added later; both would be a new form or an ABI version bump, because
they change what a guest must understand.

---

## 5. The escape hatch: declining records

This is the feature that makes a vectorized UDF practical.

`declinePtr` points at `ceil(n/64)` u64 words, **all zero when you are called**.
Set bit `i % 64` of word `i / 64` to say: *I did not handle record i.* The host
then runs that record through your **scalar** entry point, one at a time, and uses
that answer.

So you can implement the one typed path your data is actually made of — two
integers, say — and hand back everything else: the records with an UNDEFINED
operand, the mixed types, the reals, whatever you would rather not express in
branch-free form. Those records do not get a *worse* answer or a different answer
than your scalar implementation would give them. They get your scalar
implementation's answer, because they are run through it.

This is why the scalar entry point is mandatory even for a vectorized module:
there is no module shape in which a declined record has nowhere to go.

Three rules:

1. **Your output slot for a declined record is never read.** You may leave
   anything in it — scratch space, garbage, an illegal state code, or nothing at
   all. "I declined this record" means "do not look", and the host does not.
   (`testdata/vecdecline.wat` deliberately leaves an illegal state there, so that
   this is a test rather than a promise.)
2. **The mask holds the host's own declines when you are called,** and nothing
   from any previous batch. For a version 1 guest that is always zero, which is
   what version 1 promised. For a version 2 guest it is the records whose value is
   not of the state their column was given (section 4a).

   So: **OR into the mask, never store over it.** A guest that stores would wipe
   bits that are not its to wipe. The host ORs its own declines back in after
   reading yours, so doing it wrong cannot produce a wrong answer — it would mean
   claiming records you were never given, and the host does not let you have them.
   Clearing a bit buys you nothing and the arithmetic for ORing is the same either
   way.
3. **The mask is read only if you return status 0.** If you trap, run out of
   steps, or report a nonzero status, the mask you left behind says nothing about
   which records you handled, so the host escalates nothing and the whole batch
   becomes ERROR.

Declining every record is legal and pointless: it is the per-record path with an
extra call in front of it. The host counts escalations separately
(`Stats.Escalated` against `Stats.Batches`) precisely so that a vector entry point
which is earning nothing is visible.

---

## 6. Ownership and lifetime

This is where reading the example code misleads people, so it is worth being
explicit.

**The host owns a region of your linear memory.** After your module is
instantiated, the host grows your memory by as many pages as it needs and uses
**the pages it added**. Those pages cannot overlap anything you already had: your
data segments and your stack live below your initial memory size, and if you grow
memory later you get pages above the host's. This is why the host needs no
allocator export from you and no convention about which addresses are free — the
placement falls out of how wasm memory grows.

Consequences:

- **Every pointer the host passes you is inside your own memory.** You read and
  write it with ordinary loads and stores. There is no shared memory with the
  host and no address space but yours.
- **You may write to the regions the host passes you.** They are yours to scribble
  on. The host does not read an argument array after your call returns, and it
  reads the result array only at the pointers it chose.
- **Every pointer is valid only for the duration of one call.** Not "until you
  next grow memory", not "until the module is closed" — one call. **Retain
  nothing.** The next call reuses the same region, and in the vector case so do
  the *scalar* calls the host makes immediately afterwards for the records you
  declined: those overwrite the argument and result arrays you were just given.
  A guest that stashed a pointer from a vector call and read it during the
  escalations that followed would be reading a scalar call frame.
- **You may keep your own state below the region**, in your data segments or your
  own heap, across calls — the host does not touch it. Be aware that it persists
  across *records*, which means a UDF that accumulated state there would not be a
  pure function of its arguments. Also be aware that an instance is discarded
  after a trap (section 7), so such state is not durable.
- **The host copies everything out before returning.** A string result is copied,
  a result column is copied. Nothing of yours is aliased by the value the caller
  receives, so you never have to keep anything alive for the host's benefit.

**Concurrency:** one instance serves one call at a time. The host pools several
instances of your module and may call them concurrently, but each has its own
memory and no call is ever concurrent with another in the same instance. You do
not need locks and you do not need to be re-entrant.

---

## 7. Limits, and what failure means

### The step budget, which is the real limit

The host offers exactly one import:

```wat
(import "classad_udf" "fuel" (func $fuel (param i32)))
```

Call it with a cost to declare that you have done that much work. The host debits
a per-call budget and **traps** when it is exhausted.

It is measured in **steps you declare**, not in time, on purpose. A limit measured
in wall clock makes the same input succeed on an idle machine and fail on a busy
one; the fault is not reproducible, so it cannot be tested against anything. With
a declared budget the same input exhausts it at the same iteration everywhere,
which is what makes a function that hits the limit comparable against another
implementation of itself.

Be honest about what this is: it is **cooperative**. A module that never calls
`fuel` is never charged. It bounds instrumented guests and nothing else. There is
no bytecode rewriting that injects the charges for you — that is **not built**.

Two details:

- A cost of **zero is charged as one**. Calling the import is itself a step, so you
  cannot opt out of being metered with `fuel(0)` in a loop.
- The budget is **per call** and is refilled before each one. It also covers your
  start section, so an instrumented module's initialization is metered like
  anything else.
- **A vector call gets ONE budget for the whole batch**, not one per record. If
  you charge a step per record then a batch of 4096 costs 4096 steps; if you
  charge per inner-loop iteration, multiply accordingly. Each record escalated out
  of the batch then gets its own fresh budget for its scalar call, because that is
  a separate call.

### The wall-clock backstop

Because the step budget is cooperative, there is also a deadline — by default one
second — enforced by the runtime against any guest at all. It is what stops a bare
`(loop (br 0))`. It is a backstop, not the contract: the two failures are reported
to operators as different causes so that a module which only ever hits the
deterministic one can be held to that.

For a vector call the deadline is set up **once per batch**, covering the vector
call and every record escalated out of it. Your batch does not get `n` times the
configured limit.

### Memory

Your linear memory is capped (8 MiB by default). The cap is hard and it is the
runtime's: your own `memory.grow` simply fails. A module whose *declared* minimum
exceeds the cap is refused at load.

### What happens when you fail

Every way your module can misbehave becomes either a **load error** or the ClassAd
value **ERROR**. Never a panic, never a crash of the process that called you — a
daemon evaluating an expression runs no top-level recover, so a UDF that could
crash it would take it down one match at a time.

Refused at **load**, so you find out once with a message rather than per record:
invalid wasm; a missing or mistyped export; the wrong ABI version; any import
beyond `classad_udf.fuel`, named individually; a memory that cannot hold the call
frame; a start section that traps.

ERROR at **call** time: a trap (`unreachable`, out-of-bounds, integer divide by
zero, stack overflow); the step budget; the deadline; a nonzero status; a result
the host cannot read as a value.

**A trap or an exhausted budget discards your instance.** You were stopped at an
arbitrary instruction, so anything you keep in linear memory — an allocator's free
list, a half-written buffer, an initialization flag — is in whatever state that
instruction left it. The host throws the instance away and instantiates a fresh
one for the next record, rather than letting a guest whose own invariants are
broken compute the next answer. Mechanically it could reuse it; the plausible
outcome of doing so is a wrong value rather than a second failure.

In the vector case, if an *escalated* record traps, the records already escalated
keep the answers they got and that record and all later ones become ERROR. Nothing
is recomputed on a fresh instance, because the remaining records would then be
served by a guest that had not run the ones before them.

### The trusted path

An administrator may label a module **trusted** in configuration. A trusted module
runs without the wall-clock backstop, on the runtime's non-interruptible code
path. It is a large difference — roughly 350 ns of per-call overhead, and for a
vector guest it is charged per loop back-edge inside your own loop, so it is
closer to per element.

What trust **does not** change, and you should not write a module that assumes
otherwise: the step budget still applies, the memory cap still applies, your
result slot is still poisoned, everything you return is still validated, and a
trapped instance is still discarded.

**You cannot ask for it.** There is no ABI through which a module requests trust.
Nothing you export, no global, no custom section and no name is consulted — a
capability a module can assert is a request, not a capability. The decision
belongs to whoever configured the pool, because what it gives up is liveness: a
trusted module that loops forever hangs the goroutine calling it, with nothing
able to stop it.

---

## 8. To write a UDF, in order

1. **Export a memory called `memory`** with at least one page.
2. **Export `classad_udf_abi_version`** returning `2` for a new module, or `1` to
   be handed the per-element typed forms a version 1 module was written for.
3. **Export `classad_udf_call(argsPtr, argc, resultPtr) -> i32`.** Check `argc`
   first and return nonzero if it is not what you expect. Read each argument's tag
   and dispatch on it. Decide, explicitly, what your answer is for UNDEFINED and
   for ERROR — you will be handed both. Write a tag, a length and a payload to
   `resultPtr` and return 0.
4. **Import nothing**, unless you are declaring steps, in which case import
   `classad_udf.fuel` and nothing else.
5. **Put any initialization in the start section**, not in `_start`.
6. **Stop here if that is enough.** A scalar-only module is a complete, correct
   UDF. The vector entry point is an optimization.
7. **If you add `classad_udf_call_vec`:** check `argc`; check each descriptor's
   `form` and const flag and refuse with a nonzero status if they are not what you
   support; loop over `n` records writing your output column; OR a decline bit in
   for every record you do not handle, and **make sure your scalar implementation
   handles everything you decline** — that is where those records go.
8. **At version 2, read each column's state once and write a loop with no branch
   in it.** Then unroll it. That ordering is deliberate: the unrolling is where
   most of the measured difference is, and it is only available to you because the
   state came out of the loop.
9. **Decline a state you have no loop for by setting every bit,** not by returning
   a nonzero status. A nonzero status makes every record ERROR; a full decline mask
   gives every record its scalar answer.
10. **Keep the two in agreement.** A vectorized module is two implementations of one
   function. Nothing stops them from disagreeing except you testing that they do
   not, over every state and a range of payloads.

---

## 9. The worked example, in words

`testdata/vecdouble.wat` is a UDF that doubles a number. Read it alongside this.

Its **vector** entry point first refuses anything it does not understand: an
`argc` that is not 1, a descriptor that is not the data form, a const flag it did
not expect. Then it resolves four pointers — the argument's state and payload
arrays, and its own output's — and loops over `n` records. For a record whose
state byte is 0 (integer) it stores state 0 and the payload doubled. For anything
else it calls its `$setdecl` helper, which ORs one bit into the decline mask, and
**leaves the output slot alone**. It returns 0.

`$setdecl` is worth reading because it is the one piece of bit arithmetic you
cannot avoid: word address is `declinePtr + (i >> 6) * 8`, bit position is
`i & 63`, and the update is a load, an OR with `1 << bit`, and a store.

Its **scalar** entry point is the complete function: integers double, reals
double, UNDEFINED and ERROR pass through, anything else returns a nonzero status.
So a batch of mixed states comes back entirely correct — the integers from the
vector path, everything else from the scalar path — and the only trace of the
split is in the host's counters.

The other fixtures are each one point of the contract, and several exist to make a
rule testable rather than merely stated:

| fixture | what it demonstrates |
|---|---|
| `tagecho.wat` | the scalar ABI across every tag |
| `identity.wat` | the cheapest possible guest; the round-trip of every value state |
| `concat.wat` | building a string result in memory you own |
| `vecdouble.wat` | the typed vector path plus the escape hatch (version 1) |
| `colvecdouble.wat` | the same function on the columnar form (version 2) |
| `colvecunroll.wat`, `colvecsimd.wat` | eight records per back-edge, scalar and `i64x2` |
| `colvecconst.wat` | a const argument in the columnar form |
| `colvecstomp.wat` | a guest that stores over the decline mask, and gets nothing for it |
| `colvecdeclineall.wat` | declining every record, and a result that describes no column |
| `colvecsilent.wat`, `colvecbadstate.wat`, `colvecbadbool.wat` | the three ways a columnar result fails validation |
| `colvecpartial.wat` | filling half a column, and the tail not being the last batch's |
| `vecmaskarg.wat` | reading the mask-form bitplanes |
| `vecconst.wat` | requiring the const flag, so the host cannot quietly materialize it |
| `vecdecline.wat` | declining by value, leaving an illegal state in the declined slots |
| `vecdeclinetrap.wat` | an escalated record trapping partway through a batch |
| `vecredirect.wat` | rewriting the result descriptor, and it being ignored |
| `vecsilent.wat` | returning success without writing the output column |
| `vecstatus.wat` | a nonzero status with a full column and every decline bit set |
| `vecbadstate.wat`, `vecstring.wat`, `vecbadbool.wat` | the three ways a result element can fail validation |
| `stale.wat` | why the result slot is poisoned |
| `trustclaim.wat` | a module claiming trust three ways and being refused all three |
| `fuelspin.wat`, `fuelzero.wat`, `hardspin.wat` | the step budget, zero-cost charging, and the guest the budget cannot see |

They are committed as both `.wat` (the source, for reading) and `.wasm` (what the
tests load), with `testdata/build.sh` to rebuild them. The tests load the
committed binary, so running the suite needs no wasm toolchain.

If you change a fixture, **run `build.sh`**. It records the hash of every source
it compiled in `testdata/fixtures.sha256`, and `TestFixtureBinariesAreCurrent`
fails if a `.wat` has changed since — otherwise the suite would stay green while
testing the old binary, and the source you were reading would say something
else.

---

## 10. What is stable, and what may change

**Stable within ABI version 1.** Depend on these:

- the export names and their signatures, and the import name `classad_udf.fuel`
- the scalar slot: 16 bytes, the field offsets, and tags 0–5 with their meanings
- the batch descriptor: 16 bytes, the field offsets, `form` 0, 1 and 2, `flags`
  bit 0
- the data-form state codes 0–4, the mask-form `(hi, lo)` pairs, and the columnar
  form's single state byte
- which forms each reported version is handed
- the status convention, the result poisoning, and the decline mask's semantics
  including the three rules in section 5
- the ownership and lifetime rules in section 6

**May change, and will come with a version bump:** new tags or states appended
(so do not treat an unknown one as a particular value — refuse it); new descriptor
forms; strings in the vector ABI.

**May change without a version bump,** because it is configuration rather than
contract: the default step budget, the deadline, the memory cap, the maximum batch
width, the per-call region size, and the number of pooled instances. Do not encode
any of these as constants in your module. In particular `n` is a parameter of
every vector call and is not fixed.

**Version 1 is not deprecated.** It is what a column with a state per element
needs, and the host will keep sending it to a guest that asks for it. What it is
not is the shape to write something new in.

**Not built.** Named here so you do not plan around something that does not exist:

- bytecode rewriting to inject step charges — so the step budget is cooperative
  and only bounds a module that calls `fuel`
- strings or dictionary-encoded columns in either vector form
- stored-width (narrow) columns, which would be a new form rather than a new field
- per-module step and time limits. The budget and the deadline come from the
  loader, so two policies mean two loaders. The trusted label is the one
  per-module decision, and it is a yes or no rather than a number
- any mechanism by which a module influences whether it is trusted, which is not
  a gap to be filled but a property to be preserved

---

## 11. Where this lives in the code

| what | where |
|---|---|
| the scalar slot, encode and decode | `wasmudf/abi.go` |
| the vector descriptors, the decline mask, the poison | `wasmudf/abivec.go` |
| the columnar form, the uniformity scan, the exception mask | `wasmudf/abicol.go` |
| bulk moves between an int64 column and guest memory | `wasmudf/bulk.go` |
| the column representation and the engine's state codes | `wasmudf/batch.go` |
| loading, validation, the two runtimes, the trust label | `wasmudf/loader.go` |
| the scalar call path, the step counter, the instance pool | `wasmudf/udf.go` |
| the batch call path and the escape hatch | `wasmudf/vector.go` |
| what each failure means | `wasmudf/reason.go` |
| guest fixtures | `wasmudf/testdata/` |
