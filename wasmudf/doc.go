// Package wasmudf runs user-defined ClassAd functions compiled to WebAssembly.
//
// It is a separate Go module so the wazero dependency is opt-in: a program that
// imports classad but never wants a sandboxed UDF does not link a wasm compiler.
//
// Integrating a module -- loading it, choosing its sandbox, deciding whether to
// trust it -- is documented in docs/user-defined-functions.md at the repository
// root, which covers all four ways to extend this engine and treats wasm as one
// of them.
//
// Writing a guest module is documented in prose, for someone who is not reading
// this code and may not write Go, in docs/wasm-udf-abi.md at the repository root.
// It covers both calling conventions, the ownership and lifetime rules, and what
// is stable across versions. The comments here and in abivec.go explain why the
// host is built the way it is; that file explains what a module author has to do.
//
// # What a UDF is here
//
// Exactly what classad's registry says it is: a pure function of its argument
// VALUES. It receives no evaluator, so it cannot reach the calling scope, cannot
// resolve an attribute, and cannot materialize a lazy list -- those are structural
// consequences of the registry's signature rather than rules this package
// enforces, which is why they cannot be worked around from inside a guest.
//
// That purity is what makes a UDF safe to COMPILE INTO a query: StrictFunction
// answers "can this call be lowered to a call on pre-evaluated argument values",
// and the answer for a registered UDF is yes for the same reason it is yes for
// pow(). A UDF that could see its caller's scope would have to be delegated to
// the tree-walking evaluator on every record, which is the cost this design is
// built to avoid.
//
// # The ABI
//
// One call passes argument values in and takes one value out, through the guest's
// linear memory. A VALUE SLOT is 16 bytes, little-endian, naturally aligned:
//
//	offset  0  u32  tag
//	offset  4  u32  byte length (strings only; zero otherwise)
//	offset  8  u64  payload
//
//	tag 0  UNDEFINED        payload unused
//	tag 1  ERROR            payload unused
//	tag 2  boolean          payload 0 or 1, and nothing else
//	tag 3  integer          payload is the int64, two's complement
//	tag 4  real             payload is the float64's IEEE-754 bits
//	tag 5  string           payload is a u32 offset in its LOW word, length at +4
//
// A guest exports:
//
//	(memory (export "memory") ...)
//	(func (export "classad_udf_abi_version") (result i32))
//	(func (export "classad_udf_call") (param i32 i32 i32) (result i32))
//
// and classad_udf_call(argsPtr, argc, resultPtr) returns 0 when it has written a
// value to the result slot, or any nonzero value to say it could not.
//
// Fixed-width tagged slots, rather than a packed or self-describing encoding,
// because the guest must be able to reach argument k at a constant offset
// (argsPtr + k*16). That removes the parsing loop from every guest -- the place
// where a hand-written module is most likely to be wrong -- and it means the host
// writes a call frame with no per-argument branching beyond the tag switch it
// already needs. The cost is 16 bytes per argument for values that would pack
// into 9; at one call per record that is a memory write that never leaves L1.
//
// UNDEFINED and ERROR have TAGS, not out-of-band signalling, because in ClassAd
// they are values: isUndefined(f(x)) is a question a UDF's caller is entitled to
// ask, and a UDF handed UNDEFINED must be able to decide for itself whether its
// answer is UNDEFINED, ERROR or a number. A host that mapped them onto a status
// code would make them failures of the call instead, and the distinction between
// "this argument was missing" and "this function broke" would be gone.
//
// The status return is NOT how a guest reports ERROR -- it can encode ERROR in
// the slot. It exists so the host knows whether the slot was WRITTEN at all. The
// host poisons the result slot with an impossible tag before every call, so a
// guest that returns 0 without writing produces a defined failure instead of the
// PREVIOUS record's answer. Those two halves (poison plus status) are what make a
// forgetful guest loud rather than silently wrong.
//
// # The call frame and where it lives
//
// The host owns a region of the guest's linear memory and lays each call out in
// it: argc slots, then the result slot, then an arena holding the bytes of any
// string arguments.
//
// The region is placed in pages the host ADDS to the memory, after instantiation,
// with memory.grow. Those pages cannot overlap anything the guest already has:
// its data segments and stack live below the initial memory size, and a guest
// that grows memory later gets pages above the host's. So the host needs no
// allocator export from the guest and no convention about free addresses -- the
// placement is a consequence of how wasm memory grows. (A guest is of course free
// to scribble on the region; the worst it achieves is a wrong value for its own
// call, which the result validation turns into ERROR.)
//
// String bytes the host writes are valid only for the duration of the call. A
// string the GUEST returns may point anywhere in its own memory, including at the
// host's arena; the host validates the pointer and copies the bytes out, so no
// guest address outlives the call either.
//
// # What this refuses
//
// Lists and nested ads are not in the tag space, as arguments or as results. A
// list argument is refused before the guest runs: the registry hands a UDF values
// it cannot materialize a lazy list from, and an ABI that flattened one would be
// re-introducing the materialization the registry's signature exists to prevent.
// Nested ads would need the scope machinery a UDF deliberately does not get.
//
// No I/O, no clock, no randomness, no WASI. This is not a policy applied to
// guests, it is the absence of anything to call: the host offers exactly one
// import (the step counter below), so a module wanting more cannot be
// instantiated and is rejected at load, naming what it asked for. A UDF that
// could read the clock would also not be a pure function of its arguments, and
// could not be compiled into a query at all -- see classad's nonStrictFunctions,
// where time and random sit for exactly this reason.
//
// # Two entry points
//
// A module always exports the SCALAR entry point, called once per record. It may
// also export a VECTOR entry point, called once per batch, which is what gets a
// UDF into the vector engine's hot loop instead of pulling the whole query off
// it. CallBatch uses the vector one and falls back to the scalar one for anything
// it cannot do; see Batch and abivec.go for the batch representation and the
// escape hatch by which a vectorized guest declines individual records.
//
// Requiring the scalar one is what makes the escape hatch unconditional: there is
// no module shape in which a declined record has nowhere to go.
//
// The measured difference, on an Apple M2 Pro, per element, for a UDF that
// doubles a number:
//
//	scalar, untrusted                 782 ns
//	scalar, trusted                   106 ns
//	batch of 4096, untrusted         27.9 ns
//	batch of 4096, trusted            6.3 ns
//
// Batching is worth 28x on its own and 17x on top of trust, so the two are not
// substitutes: trust removes a per-call cost, batching removes a per-call cost
// AND amortizes the marshalling, and what is left at 6.3 ns is the work.
//
// # Trust
//
// An admin may label a module trusted (LoadOptions.Trusted). A trusted module is
// compiled into a runtime that cannot be interrupted and runs with no wall-clock
// deadline, which is where almost all of the scalar path's overhead was.
//
// It does NOT relax the deterministic step budget, the memory cap, the poisoned
// result slot, the validation of what a guest returns, or discarding an instance
// that trapped. The measurement is why: the budget is not where the time goes,
// and it is the only bound whose failures are reproducible, so relaxing it would
// buy nothing and cost the one property that makes a trusted module and an
// untrusted one comparable on the same input.
//
// Trust is CONFIGURATION. Nothing in a module is consulted -- not an export, not
// a custom section, not a name -- and there is deliberately no ABI through which a
// guest could ask, because a capability a module can assert is a request rather
// than a capability. What a trusted module gives up is liveness: one that loops
// forever hangs its goroutine with nothing able to stop it, which is precisely
// why the decision belongs to the party that configured the pool.
//
// # Bounds
//
// Memory is capped by the runtime (a hard bound; the guest's grow simply fails).
//
// Run length is bounded two ways, and the difference between them matters:
//
//   - A STEP BUDGET, in units the guest declares by calling the one import this
//     host offers, classad_udf.fuel(cost). It is deterministic: the same arguments
//     exhaust it at the same point on a loaded machine and an idle one, which is
//     what makes a UDF that hits the limit reproducible and therefore
//     differentially testable against a non-wasm implementation of the same
//     function. It is also COOPERATIVE -- a module that never calls fuel is never
//     charged -- so it bounds instrumented guests and nothing else.
//
//   - A WALL-CLOCK deadline, enforced by the runtime against any guest at all.
//     This is the one that stops a bare `(loop (br 0))`.
//
// The step budget is the contract and the deadline is the backstop, in that order
// and not the other way around, because a limit measured in wall clock makes the
// same input succeed on one machine and fail on another: the fault is not
// reproducible, so it cannot be tested against anything. The deadline exists only
// because the step budget cannot be made involuntary without rewriting the guest's
// bytecode to inject the charges, which this increment does not do. The two
// failures are reported as different reasons (ReasonStepLimit, ReasonTimeout) so a
// test can assert that a given module only ever hits the deterministic one.
//
// The deadline is set up ONCE per CallBatch, covering the vector call and every
// record escalated out of it, rather than once per record. That is the right scope
// as well as the cheap one: the engine's unit of work is the batch, so a deadline
// per record would let a batch of 4096 run for 4096 times the configured limit.
//
// # Aggregates
//
// A guest can also supply an AGGREGATE, which the database engine reduces with
// exactly as it reduces with SUM. It is a separate export set under its own
// version -- a module is one or the other -- because the two have nothing in
// common at the boundary except the value slot above.
//
//	(memory (export "memory") ...)
//	(func (export "classad_udaf_abi_version") (result i32))
//	(func (export "classad_udaf_init")   (param i32 i32) (result i32))
//	(func (export "classad_udaf_fold")   (param i32 i32 i32 i32 i32 i32) (result i32))
//	(func (export "classad_udaf_merge")  (param i32 i32 i32 i32 i32 i32) (result i32))
//	(func (export "classad_udaf_result") (param i32 i32 i32) (result i32))
//
// Four operations: produce the identity state, fold a batch of value slots into a
// state, merge two states, render a state as one value slot. init and fold and
// merge return the length of the state they wrote, or a negative number to refuse;
// result returns a status like a scalar call.
//
// THE STATE IS BYTES THE HOST HOLDS, not a handle into guest memory, and the
// reason is lifetime rather than speed. An accumulator holding a handle would pin
// the instance that minted it, and the engine's accumulator interface has no
// close -- worse, when the columnar pass declines, every worker's accumulator is
// dropped with neither merge nor result ever called, so there is no moment at
// which guest memory could even be inferred to be free. With the state in the
// host, an accumulator is an ordinary Go value, instances are borrowed per call as
// the scalar path borrows them, and merging two states works whichever instances
// they were folded in. The copy that costs is amortized by folding a BATCH of
// values per call.
//
// MERGE IS NOT DECORATION. The engine's columnar aggregate pass fans out across
// segment windows: one state per worker, folded once every worker has returned.
// Which worker draws which segment is a race, so merge must be associative and
// commutative and the aggregate must not depend on the order values arrive in. A
// guest that gets this wrong is wrong only sometimes, and plausibly.
//
// The OPTIONAL export classad_udaf_fold_num, with fold's signature and slot
// layout, is how a guest says it may be fed from a column: that its answer does
// not depend on arrival order, and that it does not count what it was never
// offered -- the column pass yields only a column's numeric values, so a record
// whose argument is absent or a string is not offered at all. A guest for which
// both hold exports the same function under both names. Without it the aggregate
// is served from the record scan, which is correct and slower, never wrong.
// db.CheckAggregate tests all of it against a real sample.
//
// # Failures
//
// Everything a FUNCTION guest can do wrong is either a load error or the ClassAd
// value ERROR. Nothing becomes a panic: a daemon evaluating an expression runs no
// top-level recover, so a panic escaping here would reach the process, and a UDF
// in a Requirements expression would take the daemon down one match at a time.
// See Reason for the full list and UDF.Call for the classification.
//
// AN AGGREGATE IS THE OPPOSITE, deliberately. It reports a failure by panicking
// with an *AggError, because containment for an aggregate is per query rather than
// per row: a record evaluating to ERROR says nothing about the others, where a
// state that stopped being updated part-way is a wrong answer indistinguishable
// from a right one. The engine's aggregate pass recovers it -- on the calling
// goroutine, even for a panic raised on a worker -- and fails the query naming the
// aggregate. Nothing here is left for a caller of this package to recover.
package wasmudf
