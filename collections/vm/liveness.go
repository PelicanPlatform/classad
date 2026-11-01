package vm

import "math/bits"

// LIVENESS over the register form, and the measurement that decides how much SSA is worth here.
//
// A register is live at a point if some path from that point reads it before writing it. The
// standard backward fixpoint:
//
//	liveOut(n) = union of liveIn(s) over successors s
//	liveIn(n)  = uses(n) u (liveOut(n) \ def(n))
//
// WHY THIS MATTERS MORE HERE THAN IN A SCALAR COMPILER. In the scalar register machine a register
// holds one classad.Value, so the number simultaneously live is a curiosity. In the vector machine a
// register is a whole batch -- about 200 KiB in data form at 8192 records -- so the peak number of
// simultaneously live registers decides cache residency, and a spill moves 200 KiB. Peak live
// registers is therefore the quantity a vector register allocator exists to minimize, and MaxLive is
// how any claim about it gets checked rather than asserted.
//
// A register with one place per local, which is what the lowering emits today, is live from its first
// write to its last read -- most of the program for a script that uses its bindings. That is the cost
// of places, and MaxLive is what makes it a number instead of an argument.

// liveSets holds live-in and live-out sets, one bitset of registers per instruction. Index n is
// instruction n; index len(code) is the exit, whose live-in is the result register alone.
type liveSets struct {
	in, out []uint64
	words   int
	n       int
}

// liveness computes live-in and live-out for every instruction.
func liveness(rp *RegProgram) *liveSets {
	c := regCFG(rp)
	words := (rp.nregs + 63) / 64
	if words == 0 {
		words = 1
	}
	l := &liveSets{
		in:    make([]uint64, c.n*words),
		out:   make([]uint64, c.n*words),
		words: words,
		n:     c.n,
	}
	// The exit reads the result register, which is the use the instruction stream does not show.
	if rp.res >= 0 && int(rp.res) < rp.nregs {
		l.in[(c.n-1)*words+int(rp.res)/64] |= 1 << uint(rp.res%64)
	}
	var buf []int32
	tmp := make([]uint64, words)
	for changed := true; changed; {
		changed = false
		// Backwards, which converges in one pass for forward-only code and still terminates with a
		// back edge.
		for n := c.n - 2; n >= 0; n-- {
			if !c.reach[n] {
				continue
			}
			for i := range tmp {
				tmp[i] = 0
			}
			for _, s := range c.succs[n] {
				row := l.in[s*words : (s+1)*words]
				for i := range tmp {
					tmp[i] |= row[i]
				}
			}
			outRow := l.out[n*words : (n+1)*words]
			for i := range tmp {
				if outRow[i] != tmp[i] {
					outRow[i], changed = tmp[i], true
				}
			}
			// liveIn = uses u (liveOut \ def)
			for i := range tmp {
				tmp[i] = outRow[i]
			}
			if d, ok := regDef(rp.code[n]); ok && int(d) < rp.nregs {
				tmp[d/64] &^= 1 << uint(d%64)
			}
			buf = regUses(rp, rp.code[n], buf)
			for _, u := range buf {
				if int(u) < rp.nregs {
					tmp[u/64] |= 1 << uint(u%64)
				}
			}
			inRow := l.in[n*words : (n+1)*words]
			for i := range tmp {
				if inRow[i] != tmp[i] {
					inRow[i], changed = tmp[i], true
				}
			}
		}
	}
	return l
}

// liveAt reports whether register r is live on entry to instruction n.
func (l *liveSets) liveAt(n int, r int32) bool {
	if n < 0 || n >= l.n || r < 0 {
		return false
	}
	return l.in[n*l.words+int(r)/64]&(1<<uint(r%64)) != 0
}

// countAt returns how many registers are live on entry to instruction n.
func (l *liveSets) countAt(n int) int {
	if n < 0 || n >= l.n {
		return 0
	}
	c := 0
	for _, w := range l.in[n*l.words : (n+1)*l.words] {
		c += bits.OnesCount64(w)
	}
	return c
}

// MaxLive returns the greatest number of registers simultaneously live at any point, which is the
// quantity a vector register allocator minimizes: at 8192 records each one is about 200 KiB in data
// form, so this number times that is the program's peak working set.
//
// It counts live-in sets, so a value defined and immediately consumed by the next instruction is not
// counted twice.
func MaxLive(rp *RegProgram) int {
	l := liveness(rp)
	best := 0
	for n := 0; n < l.n; n++ {
		if c := l.countAt(n); c > best {
			best = c
		}
	}
	return best
}
