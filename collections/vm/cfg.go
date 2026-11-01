package vm

// THE CONTROL-FLOW GRAPH of a compiled program, and dominance over it.
//
// It exists because the questions the compiler needs to answer about a script are dominance
// questions, and until now they were answered by a conservative approximation of one. "Does a store
// to this local always run before this read?" is exactly "does the store dominate the read", and
// anything weaker inserts work that is not needed -- see localsNeedingInit, which is this file's
// first caller and the reason it was written.
//
// INSTRUCTION GRANULARITY, not basic blocks. A node is one instruction. Programs here are tens to
// hundreds of instructions, so the O(n^2) bitset iteration below costs nothing at this size, and in
// exchange there is no leader computation to get wrong and no second mapping between instruction
// indices and block indices. A basic-block version is a later optimization of this file and not a
// different design; nothing above it would change.
//
// The graph is built from a SUCCESSOR FUNCTION rather than from an opcode switch, so one
// implementation serves both forms. The stack form's successors are stackSuccs; the register form's
// will be its own function, and the register form is where phi insertion will need this next.

// cfg is a control-flow graph over instruction indices. Node n is instruction n; node len(code) is
// the EXIT, which every path that falls off the end reaches, so a reader does not have to
// special-case the end of the program.
type cfg struct {
	n      int     // nodes, including the exit
	succs  [][]int // successors per node
	preds  [][]int // predecessors per node
	reach  []bool  // reachable from the entry
	domSet []uint64
	words  int
}

// stackSuccs returns the successors of stack-form instruction ip.
//
// This is now the ONLY place the stack form's control flow is written down -- it replaced a separate
// jump-target list that had to agree with it. A successor this misses is a path dominance would not
// know about, which makes a dominance answer WRONG rather than conservative, so there must not be a
// second copy of this knowledge to drift from.
func stackSuccs(p *Program, ip int, buf []int) []int {
	buf = buf[:0]
	in := p.code[ip]
	switch in.Op {
	case OpShortAnd, OpShortOr, OpJmpIfNotUndef:
		// Conditional: taken to A, or fall through.
		return append(buf, int(in.A), ip+1)
	case OpJmp, OpJmpVoid:
		return append(buf, int(in.A))
	case OpBranchCond, OpBranchVoid:
		c := p.conds[in.A]
		// Fallthrough is the true path; the other three are the condition's remaining states.
		return append(buf, ip+1, int(c.onFalse), int(c.onUndef), int(c.onError))
	case OpFault:
		// Terminates the program. No successor, not even the exit: nothing after it runs.
		return buf
	}
	return append(buf, ip+1)
}

// buildCFG builds the graph of n instructions from a successor function.
func buildCFG(n int, succs func(ip int, buf []int) []int) *cfg {
	c := &cfg{n: n + 1, words: (n + 1 + 63) / 64}
	c.succs = make([][]int, c.n)
	c.preds = make([][]int, c.n)
	var buf []int
	for ip := 0; ip < n; ip++ {
		buf = succs(ip, buf)
		for _, s := range buf {
			if s < 0 || s >= c.n {
				continue // a target outside the program cannot be reached; ignore rather than panic
			}
			c.succs[ip] = append(c.succs[ip], s)
			c.preds[s] = append(c.preds[s], ip)
		}
	}
	c.markReachable()
	c.computeDominators()
	return c
}

// markReachable does a depth-first walk from the entry.
//
// Unreachable instructions matter because the dominator iteration below would otherwise leave them
// dominated by every node, which is the conventional answer but makes "is this read dominated by a
// store" meaningless for them. They are excluded instead, and every caller must ask about reachable
// nodes only -- which is sound, because an unreachable read never happens.
func (c *cfg) markReachable() {
	c.reach = make([]bool, c.n)
	if c.n == 0 {
		return
	}
	stack := []int{0}
	c.reach[0] = true
	for len(stack) > 0 {
		x := stack[len(stack)-1]
		stack = stack[:len(stack)-1]
		for _, s := range c.succs[x] {
			if !c.reach[s] {
				c.reach[s] = true
				stack = append(stack, s)
			}
		}
	}
}

// computeDominators fills domSet by the standard iterative fixpoint:
//
//	dom(entry) = {entry}
//	dom(n)     = {n} u (intersection of dom(p) over predecessors p)
//
// Only reachable nodes participate, and an unreachable predecessor is skipped rather than
// intersected -- intersecting with a node that cannot run would remove dominators that genuinely
// dominate.
func (c *cfg) computeDominators() {
	c.domSet = make([]uint64, c.n*c.words)
	all := make([]uint64, c.words)
	for i := range all {
		all[i] = ^uint64(0)
	}
	for n := 0; n < c.n; n++ {
		if !c.reach[n] {
			continue
		}
		if n == 0 {
			c.setBit(0, 0)
			continue
		}
		copy(c.domSet[n*c.words:(n+1)*c.words], all)
	}
	tmp := make([]uint64, c.words)
	for changed := true; changed; {
		changed = false
		for n := 1; n < c.n; n++ {
			if !c.reach[n] {
				continue
			}
			first := true
			for _, p := range c.preds[n] {
				if !c.reach[p] {
					continue
				}
				row := c.domSet[p*c.words : (p+1)*c.words]
				if first {
					copy(tmp, row)
					first = false
					continue
				}
				for i := range tmp {
					tmp[i] &= row[i]
				}
			}
			if first {
				// No reachable predecessor, yet reachable: only the entry, handled above.
				continue
			}
			tmp[n/64] |= 1 << uint(n%64)
			row := c.domSet[n*c.words : (n+1)*c.words]
			for i := range tmp {
				if row[i] != tmp[i] {
					row[i] = tmp[i]
					changed = true
				}
			}
		}
	}
}

func (c *cfg) setBit(n, b int) {
	c.domSet[n*c.words+b/64] |= 1 << uint(b%64)
}

// dominates reports whether every path from the entry to b passes through a. A node dominates
// itself. An unreachable b is dominated by nothing, since asking about it is a caller error.
func (c *cfg) dominates(a, b int) bool {
	if a < 0 || b < 0 || a >= c.n || b >= c.n || !c.reach[b] || !c.reach[a] {
		return false
	}
	return c.domSet[b*c.words+a/64]&(1<<uint(a%64)) != 0
}

// regSuccs returns the successors of register-form instruction ip.
//
// The same knowledge as execReg's dispatch, and it has to stay the same: a successor missing here is
// a path dominance does not know about, and a pass that trusts dominance would then move work across
// it. The conditional's targets are register-form indices already, which is why this needs no
// mapping back through the stack form.
func regSuccs(rp *RegProgram, ip int, buf []int) []int {
	buf = buf[:0]
	in := rp.code[ip]
	switch in.Op {
	case RJump:
		return append(buf, int(in.T))
	case RFault:
		// Returns from the program. No successor, not even the exit.
		return buf
	case RBranchCond:
		t := rp.conds[in.Imm]
		return append(buf, ip+1, int(t.onFalse), int(t.onUndef), int(t.onError))
	case RShortAnd, RShortOr, RJmpIfDefined:
		// Defines Dst and jumps, or falls through without defining it.
		return append(buf, int(in.T), ip+1)
	}
	return append(buf, ip+1)
}

// regCFG builds the graph of a register program.
func regCFG(rp *RegProgram) *cfg {
	return buildCFG(len(rp.code), func(ip int, buf []int) []int { return regSuccs(rp, ip, buf) })
}
