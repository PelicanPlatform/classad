package vm

import "fmt"

// The register form's OPTIMIZER.
//
// Two rules govern everything here, and both come from how this gets validated rather than from
// taste.
//
// EVERY PASS IS INDIVIDUALLY SWITCHABLE. The optimizer's whole validation story is "it agrees with
// the unoptimized execution", and that only localizes a divergence to one pass if each pass can be
// turned off on its own. A pass that cannot be isolated cannot be validated, so it does not ship.
//
// NOTHING CHANGES SEMANTICS. A pass may only remove work whose result is provably unobservable. It
// may not reorder value operations, because ClassAd's operators are not all commutative -- `&&` and
// `||` are left-biased, so `ERROR && FALSE` is ERROR while `FALSE && ERROR` is FALSE -- and it may
// not fold anything the compiler did not already fold, because folding is where semantics drift.
//
// WHAT IS AND IS NOT SSA HERE. Most registers are defined exactly once, which is what makes
// substitution sound. A JOIN destination is not: the short-circuit and combine of a logical operator
// both write it, and so does each path of an Elvis. Those are real phi nodes resolved by
// construction rather than by analysis, and a pass that assumed single definition would silently
// rewrite across them. So every pass here checks the definition count rather than assuming it.

// OptPasses selects which passes run. The zero value runs none, which is the unoptimized program and
// the oracle every pass is checked against.
type OptPasses struct {
	// CopyProp replaces reads of a copied register with the original and removes the copy.
	CopyProp bool

	// DeadCode removes instructions whose destination is never read, where removing them is
	// provably unobservable. See removable.
	DeadCode bool
}

// AllPasses enables every pass.
func AllPasses() OptPasses { return OptPasses{CopyProp: true, DeadCode: true} }

// Optimize returns an optimized copy of rp. rp is not modified, so the unoptimized program stays
// available as the oracle for the optimized one.
func Optimize(rp *RegProgram, passes OptPasses) *RegProgram {
	// Copied as a STRUCT, not field by field. An earlier version listed the fields, and when the
	// conditional branch added a target table it was silently left nil -- the optimized program then
	// indexed an empty slice at run time. A new field now comes along by default, and only the
	// slices a pass mutates are detached below.
	cp := *rp
	out := &cp
	out.code = append([]RegInstr(nil), rp.code...)
	// args stays shared, so copyProp deep-copies the one argument list it rewrites.
	if passes.CopyProp {
		copyProp(out)
	}
	// After CopyProp, so the copies it detaches from their readers are then dead.
	if passes.DeadCode {
		deadCode(out)
	}
	return out
}

// regUses returns the registers instruction in reads. The returned slice is scratch owned by the
// caller's buf, so a caller must not retain it across calls.
//
// Getting this table wrong is the way a pass becomes unsound without failing to compile, so it is
// written once and every pass goes through it.
func regUses(rp *RegProgram, in RegInstr, buf []int32) []int32 {
	buf = buf[:0]
	switch in.Op {
	case RNop, RConst, RTrue, RFalse, RUndef, RError, RLoadRef, REvalNode, RJump, RFault:
		// No register operands.
	case RBinop, RCombineAnd, RCombineOr:
		buf = append(buf, in.A, in.B)
	case RUnop, RMember, RStrListMember, RMove, RShortAnd, RShortOr, RJmpIfDefined, RBranchCond:
		buf = append(buf, in.A)
	case RCallFunc:
		buf = append(buf, rp.args[in.Args]...)
	default:
		// Loudly, because the alternative is a pass that quietly treats a new instruction as reading
		// nothing -- and then deletes what it reads. Every opcode is listed above.
		panic(fmt.Sprintf("vm: optimizer has no use list for opcode %d", in.Op))
	}
	return buf
}

// regDef returns the register instruction in writes, and whether it writes one.
//
// RShortAnd, RShortOr and RJmpIfDefined write their destination only on the path they take, which is
// still a definition: the register's value after the instruction depends on it.
//
// THE CONTROL-FLOW OPCODES WRITE NOTHING, and saying otherwise was a real bug. RJump, RFault and
// RBranchCond carry no destination, so their Dst field holds whatever the zero value or the lowerer
// left there -- usually 0. Reporting that as a definition made register 0 look multiply-defined in
// every script containing a branch, and register 0 is the first local, so copy propagation silently
// refused to touch it. It cost optimization and not correctness: deadCode keeps every instruction
// `removable` declines, which includes all three, so none of them was ever at risk of being removed.
func regDef(in RegInstr) (int32, bool) {
	switch in.Op {
	case RNop, RJump, RFault, RBranchCond:
		return 0, false
	default:
		return in.Dst, true
	}
}

// defCounts returns how many instructions define each register.
func defCounts(rp *RegProgram) []int {
	n := make([]int, rp.nregs)
	for _, in := range rp.code {
		if d, ok := regDef(in); ok && int(d) < len(n) {
			n[d]++
		}
	}
	return n
}

// copyProp replaces reads of a copied register with the copy's source and turns the copy into a nop.
//
// The copies worth removing are not incidental. A script's every read of a local lowers to one
// (OpLoadLocal becomes a move from the binding's register), and an Elvis join emits one per path, so
// an expression-heavy script accumulates them in proportion to how much it reuses its own bindings.
//
// THREE CONDITIONS, each of which is a way to be wrong:
//
//   - The copy's destination and its source must each be defined EXACTLY ONCE. A join destination is
//     written by several instructions, so substituting it would take the value of whichever path the
//     pass happened to look at. Single definition of the SOURCE is what makes the substituted value
//     the same value everywhere, and single definition of the DESTINATION is what makes removing the
//     copy safe.
//   - The copy must DOMINATE every read of its destination: every path to the read has to pass
//     through the copy, or the read could see a register the copy never wrote.
//   - The destination must not be the program's result, which is read after the last instruction and
//     so has a use the instruction stream does not show.
//
// Partial substitution would be sound but pointless: the win is deleting the copy, and the copy can
// only go when nothing reads its destination any more. So a copy that fails any condition is left
// entirely alone.
//
// THE SECOND CONDITION USED TO BE A STRAIGHT-LINE TEST -- no branch between the copy and the read,
// and nothing a jump could land on -- which is sound but much weaker: a script that binds a local,
// branches, and then reads the local got no propagation at all, and that is the ordinary shape of a
// script rather than an edge case. Dominance is the exact question, and it also subsumes the old
// test's accidental protection of join destinations, which the definition count now carries alone.
//
// WHY SUBSTITUTING ACROSS A BRANCH IS SOUND, since this is the step that looks unsafe. If the source
// has exactly one definition and the copy reads it, that definition must dominate the copy -- any
// path reaching the copy without it would read a register nothing wrote, which the lowering does not
// emit. Dominance is transitive, so the source's definition also dominates every read of the
// destination, and the value substituted at the read is the one the copy would have carried there.
func copyProp(rp *RegProgram) {
	defs := defCounts(rp)
	c := regCFG(rp)
	var buf []int32
	var reads []int

	for i, in := range rp.code {
		if in.Op != RMove {
			continue
		}
		d, s := in.Dst, in.A
		if int(d) >= len(defs) || int(s) >= len(defs) || defs[d] != 1 || defs[s] != 1 {
			continue
		}
		if d == rp.res {
			continue
		}
		// Every reachable read of d must be dominated by the copy.
		reads := reads[:0]
		ok := true
		for j := 0; j < len(rp.code) && ok; j++ {
			if j == i || !c.reach[j] {
				continue
			}
			buf = regUses(rp, rp.code[j], buf)
			for _, u := range buf {
				if u != d {
					continue
				}
				if !c.dominates(i, j) {
					ok = false
					break
				}
				reads = append(reads, j)
			}
		}
		if !ok {
			continue
		}
		for _, j := range reads {
			substitute(rp, j, d, s)
		}
		rp.code[i] = RegInstr{Op: RNop}
	}
}

// substitute rewrites reads of `from` to `to` in the instruction at index j. It does not touch the
// instruction's destination: a pass that renamed definitions would be register allocation, which is
// a different thing with a different proof obligation.
func substitute(rp *RegProgram, j int, from, to int32) {
	in := &rp.code[j]
	switch in.Op {
	case RBinop, RCombineAnd, RCombineOr:
		if in.A == from {
			in.A = to
		}
		if in.B == from {
			in.B = to
		}
	case RUnop, RMember, RMove, RShortAnd, RShortOr, RJmpIfDefined, RBranchCond:
		if in.A == from {
			in.A = to
		}
	case RCallFunc:
		// Copied before writing: Optimize shares the argument lists with the program it was given,
		// and rewriting in place would reach back into the unoptimized one -- which is the oracle.
		src := rp.args[in.Args]
		cp := append([]int32(nil), src...)
		changed := false
		for k, r := range cp {
			if r == from {
				cp[k], changed = to, true
			}
		}
		if changed {
			rp.args = append(append([][]int32(nil), rp.args...), cp)
			in.Args = int32(len(rp.args) - 1)
		}
	}
}

// removable reports whether an instruction may be deleted when nothing reads its destination.
//
// The answer is NOT "any instruction with a dead destination", and the reason is specific to this
// evaluator: resolving an attribute reference can be a CYCLIC reference, which panics, and
// RunRegisters turns that panic into an error value. So a load whose result nobody reads still
// decides the program's result:
//
//	t = Cyc          // Cyc = Cyc + 1
//	result = 5       // evaluates to ERROR, not 5
//
// Deleting that load would turn ERROR into 5. A delegated subtree is the same: `Cyc ? 1 : 2` bound
// to an unused local is ERROR, and it compiles to one REvalNode. Both are demonstrated by
// TestDeadCodeKeepsWhatCanPanic.
//
// A built-in call is excluded as a PRECAUTION rather than a demonstrated hazard. Ten of them
// materialize a lazy list, which is how a cycle would be reached, but every case tried --
// sum/size/min/join/member over a self-referential list -- returned a value rather than panicking.
// It stays out of the whitelist because "I could not construct one" is not "there is none", and the
// cost of keeping it out is a dead call left in place.
//
// So this is a whitelist of instructions that compute from already-computed values and cannot fail:
// literals, and register copies. That is enough to clear what copy propagation leaves behind, which
// is what this pass is for. Widening it means proving the wider set cannot panic, not assuming it.
func removable(in RegInstr) bool {
	switch in.Op {
	case RNop, RConst, RTrue, RFalse, RUndef, RError, RMove:
		return true
	}
	return false
}

// deadCode removes instructions whose destination is never read.
//
// Liveness is computed to a fixpoint rather than in one backward sweep, because a jump can carry
// control backwards over an instruction -- not in the shapes the compiler emits today, where every
// jump is forward, but the pass should not be the thing that breaks when a loop arrives.
func deadCode(rp *RegProgram) {
	live := make([]bool, rp.nregs)
	if rp.res >= 0 && int(rp.res) < len(live) {
		live[rp.res] = true
	}
	kept := make([]bool, len(rp.code))
	for i, in := range rp.code {
		// An instruction that cannot be removed is kept regardless of whether its destination is
		// read, and therefore its operands are live.
		if !removable(in) {
			kept[i] = true
		}
	}
	var buf []int32
	for changed := true; changed; {
		changed = false
		for i := len(rp.code) - 1; i >= 0; i-- {
			if !kept[i] {
				if d, ok := regDef(rp.code[i]); ok && int(d) < len(live) && live[d] {
					kept[i], changed = true, true
				}
			}
			if !kept[i] {
				continue
			}
			buf = regUses(rp, rp.code[i], buf)
			for _, u := range buf {
				if int(u) < len(live) && !live[u] {
					live[u], changed = true, true
				}
			}
		}
	}
	for i := range rp.code {
		if !kept[i] {
			rp.code[i] = RegInstr{Op: RNop}
		}
	}
}
