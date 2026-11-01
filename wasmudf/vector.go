package wasmudf

import (
	"context"
	"fmt"

	"github.com/PelicanPlatform/classad/classad"
)

// CallBatch runs the UDF over a whole batch of records at once.
//
// args are the argument columns, out is the column to fill. Every column is N
// elements long, or one element with Const set. out must be in the data form and
// its St/I must already be allocated: it is the engine's own result vector, and
// allocating a replacement here would take it out of the pool it is recycled
// from.
//
// ok=false means DECLINED, not failed: the caller should fall back to calling
// Call once per record, which is what the engine does for any built-in it cannot
// vectorize. A decline is not an error and no record gets a value from it. The
// reasons are a module with no vector entry point, a batch wider than the frame
// the region was sized for, a batch containing strings (see Batch), and a
// malformed batch. Each is counted, because "the vector path is off" otherwise
// shows up only as a query being slower than it should be.
//
// A declined batch may have left SOME elements of out written. That matches the
// engine's own rule for a call it cannot complete -- ok=false declines the whole
// evaluation and the vector is discarded, never read as a partial answer -- so
// there is nothing to clean up, but a caller that kept reading out after a false
// would be reading a mixture of two answers.
//
// ok=true means every element of out holds a value, with no exceptions:
//
//   - records the guest computed hold what the guest computed
//   - records the guest DECLINED hold what the scalar entry point computed for
//     them, one at a time, through exactly the path Call uses
//   - if the vector call itself failed -- a trap, the step budget, a bad result
//     slot -- every record holds ERROR
//
// That last case does NOT fall back to the scalar path, deliberately. A guest
// that trapped halfway through a batch would otherwise produce an answer on the
// retry and an ERROR without it, so the same input would give different results
// depending on a failure the caller cannot see. And the decline mask a dead guest
// left behind means nothing, so there is no honest way to know which records it
// claimed.
func (u *UDF) CallBatch(args []Batch, out *Batch) bool {
	if !u.vectored {
		// Not a failure and not counted: a module without a vector entry point
		// is an ordinary scalar UDF, and the caller asking is how it finds out.
		return false
	}
	n, ok := u.batchShape(args, out)
	if !ok {
		// The message does not read anything out of the batch, because a nil or
		// short column is one of the things being refused: formatting out.N here
		// dereferenced a nil result column and panicked out of a function whose
		// whole contract is that it does not.
		u.fail(ReasonBatchShape, fmt.Errorf("malformed batch: %d arguments for arity %d..%d",
			len(args), u.minArgs, u.maxArgs))
		return false
	}
	if n > u.opts.MaxBatch {
		u.fail(ReasonBatchTooLarge, fmt.Errorf("batch of %d exceeds MaxBatch %d", n, u.opts.MaxBatch))
		return false
	}
	if u.abi < abiVersion2 {
		// Version 2 answers the same question inside the uniformity scan, which
		// has to look at every state byte anyway; running both would be two
		// passes over every column to learn one thing.
		for i := range args {
			if args[i].carriesStrings() {
				u.fail(ReasonBatchStrings, nil)
				return false
			}
		}
	}

	// Every argument const means the result is too: one scalar call, and out is
	// left const. The engine's own elementwise call does this (callFuncVec), and
	// doing it here as well keeps a call on literals -- quantize(4, 8) -- off
	// both the vector entry point and the per-record loop. It also means a guest
	// never sees an all-const batch, so it needs no special case for one.
	if allConst(args) {
		if !out.setValue(0, u.Call(constArgs(args))) {
			return false
		}
		out.Const = true
		return true
	}
	out.Const = false

	in, err := u.acquire()
	if err != nil {
		u.batches.Add(1)
		u.fail(ReasonInstance, err)
		return u.errorBatch(out, n)
	}

	// Version 2 decides each column's form before anything is written, because a
	// column with no uniform state to give it declines the batch -- and a decline
	// is not a failure, so it must happen before the batch is counted as one. The
	// scan needs the instance's scratch, which is why it is here and not above
	// the acquire.
	if u.abi >= abiVersion2 {
		if !u.planColumns(in, n, args) {
			u.release(in)
			return false
		}
	}
	u.batches.Add(1)

	// ONE deadline for the whole batch, and for every record escalated out of it.
	//
	// This is the measurement from the scalar path applied: context.WithTimeout
	// is ~290ns, which was 39% of a 747ns per-record call, and amortizing it over
	// 4096 records makes it 0.07ns each. It is also the right SCOPE rather than
	// merely the cheap one -- the engine's unit of work is the batch, so a budget
	// per record would let a batch of 4096 run for 4096 times the deadline the
	// caller set.
	ctx, cancel := in.callContext(u.timeout)
	if cancel != nil {
		defer cancel()
	}

	r, cerr := u.invokeVec(in, ctx, n, args, out)
	if r != ReasonNone {
		if r.fromGuest() {
			u.discard(in)
		} else {
			u.release(in)
		}
		u.fail(r, cerr)
		return u.errorBatch(out, n)
	}

	// The escape hatch. Each declined record goes through the scalar entry point
	// on the SAME instance and under the SAME deadline, with its arguments boxed
	// from the caller's columns rather than read back out of guest memory -- so a
	// guest cannot feed the scalar path values it edited after the host wrote
	// them.
	escalated := 0
	for i := range n {
		if in.declined[i/64]&(uint64(1)<<uint(i%64)) == 0 {
			continue
		}
		escalated++
		// Counted as a scalar call, because it is one: Stats.Calls against
		// Stats.Escalated is how an operator sees the escape hatch firing, and a
		// path that incremented neither would make the escalations invisible in
		// exactly the case where they matter.
		u.calls.Add(1)
		args1 := in.scalarArgsFor(len(args))
		for j := range args {
			args1[j] = args[j].value(i)
		}
		v, sr, serr := u.invoke(in, ctx, args1)
		if sr != ReasonNone {
			u.fail(sr, serr)
			v = classad.NewErrorValue()
			if sr.fromGuest() {
				// The instance is unusable for the rest of the batch. Every
				// record still to come gets ERROR rather than being computed by
				// a guest whose memory is in an unknown state.
				u.discard(in)
				u.escalated.Add(uint64(escalated))
				return u.errorBatchFrom(out, i, n)
			}
		}
		if !out.setValue(i, v) {
			// A string, list or nested ad: a value the column cannot hold, which
			// is the engine's own reason to decline a call result.
			u.release(in)
			u.escalated.Add(uint64(escalated))
			return false
		}
	}
	u.release(in)
	u.escalated.Add(uint64(escalated))
	return true
}

// invokeVec is one vector call. It returns ReasonNone with out and in.declined
// filled, or the reason it could not.
func (u *UDF) invokeVec(in *instance, ctx context.Context, n int, args []Batch, out *Batch) (r Reason, cerr error) {
	defer func() {
		// recover() must be called DIRECTLY by the deferred literal; a helper
		// that calls it leaves recover a frame too deep, where it returns nil.
		if p := recover(); p != nil {
			r, cerr = ReasonPanic, fmt.Errorf("panic in wasm vector call path: %v", p)
		}
	}()

	var f vecFrame
	var fits bool
	if u.abi >= abiVersion2 {
		f, fits = writeColBatchArgs(in.mem, in.base, in.size, n, args, in.plan, in.hostDeclined)
	} else {
		f, fits = writeBatchArgs(in.mem, in.base, in.size, n, args)
	}
	if !fits {
		// The region was sized for MaxBatch at load, so this means the frame is
		// wider than that sizing assumed -- a variadic UDF handed more arguments
		// than MaxBatchArgs. A decline, not a failure: nothing ran.
		return ReasonBatchTooLarge, fmt.Errorf("the %d-byte region cannot hold %d arguments over %d records",
			in.size, len(args), n)
	}

	in.fuel.steps = u.opts.Steps
	in.stack[0] = uint64(n)
	in.stack[1] = uint64(f.args)
	in.stack[2] = uint64(len(args))
	in.stack[3] = uint64(f.result)
	in.stack[4] = uint64(f.decline)
	if err := in.callVec.CallWithStack(ctx, in.stack[:]); err != nil {
		return classifyCallError(err), err
	}
	if status := uint32(in.stack[0]); status != statusOK {
		return ReasonStatus, fmt.Errorf("guest returned status %d from the vector entry point", status)
	}

	// The mask is read only now that the call has reported success. A mask left
	// by a guest that trapped says nothing about which records it handled, so the
	// failure paths above never reach this.
	if !readDeclineMask(in.mem, f.decline, n, in.declined) {
		return ReasonBadResult, fmt.Errorf("cannot read the decline mask at %d", f.decline)
	}
	if u.abi >= abiVersion2 {
		// The host's own declines go back in. They were handed to the guest as the
		// mask's starting value, but version 1 promised a zero mask and so allowed
		// a guest to STORE its bits rather than OR them; one that does would
		// otherwise be credited with records whose value was never of the state its
		// column declared, and the host would read a lane computed from a filler.
		for w := range maskWords(n) {
			in.declined[w] |= in.hostDeclined[w]
		}
		if !readColBatchResult(in.mem, f, n, in.declined, out) {
			return ReasonBadResult, fmt.Errorf("the result column holds a state that is not a value")
		}
		return ReasonNone, nil
	}
	if !readBatchResult(in.mem, f, n, in.declined, out) {
		return ReasonBadResult, fmt.Errorf("the result column holds a state that is not a value")
	}
	return ReasonNone, nil
}

// batchShape validates the columns and returns the record count they agree on.
//
// It is one check rather than trusting the caller because every pointer in the
// vector frame is derived from N: a batch claiming more records than its backing
// arrays hold would have the host copying out of range, and this is the boundary
// where that is still cheap to notice.
func (u *UDF) batchShape(args []Batch, out *Batch) (int, bool) {
	if out == nil || out.Mask || out.N < 0 {
		return 0, false
	}
	if len(args) < u.minArgs || (u.maxArgs >= 0 && len(args) > u.maxArgs) {
		return 0, false
	}
	n := out.N
	if len(out.St) < n || len(out.I) < n {
		return 0, false
	}
	for i := range args {
		if args[i].N != n || !args[i].valid() {
			return 0, false
		}
	}
	return n, true
}

func allConst(args []Batch) bool {
	if len(args) == 0 {
		// No arguments means nothing varies per record, so the result does not
		// either -- the same conclusion as every argument being const, reached
		// without a loop over none of them.
		return true
	}
	for i := range args {
		if !args[i].Const {
			return false
		}
	}
	return true
}

func constArgs(args []Batch) []classad.Value {
	vals := make([]classad.Value, len(args))
	for i := range args {
		vals[i] = args[i].value(0)
	}
	return vals
}

// errorBatch fills the whole column with ERROR and reports that the batch was
// handled. Used where the vector call failed: the records got an answer, and the
// answer is ERROR.
func (u *UDF) errorBatch(out *Batch, n int) bool { return u.errorBatchFrom(out, 0, n) }

// errorBatchFrom fills out[from:n] with ERROR, leaving earlier elements as they
// were -- which is what an instance dying partway through the escape hatch needs:
// the records already escalated have real answers and keep them.
func (u *UDF) errorBatchFrom(out *Batch, from, n int) bool {
	for i := from; i < n; i++ {
		out.St[i], out.I[i] = vsError, 0
	}
	out.Const = false
	return true
}

// scalarArgsFor sizes the instance's boxed argument buffer, so the escape hatch
// allocates once per instance rather than once per escalated record.
func (in *instance) scalarArgsFor(argc int) []classad.Value {
	if cap(in.scalarArgs) < argc {
		in.scalarArgs = make([]classad.Value, argc)
	}
	return in.scalarArgs[:argc]
}
