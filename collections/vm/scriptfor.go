package vm

import (
	"errors"
	"fmt"

	"github.com/PelicanPlatform/classad/ast"
)

// `for NAME in SEQUENCE:` -- a loop over the elements of a list, and the counted `while` it is.
//
// WHAT IT COMPILES TO. The sequence is bound once, its length is bound once, and an index walks it:
//
//	for#3.seq = <SEQUENCE>
//	for#3.n   = ifThenElse(isList(for#3.seq), size(for#3.seq), error)
//	for#3.i   = 0
//	while for#3.i < for#3.n:
//	    NAME    = for#3.seq[for#3.i]
//	    for#3.i = for#3.i + 1
//	    <body>
//
// There is no loop opcode and no iterator: every line above is a statement the builder already had,
// and the one that was missing is the element load, OpSubscript. The three registers are allocated
// like a function expansion's -- named for the loop, reachable by nothing the author can write --
// and their names are bound only while the lines above are emitted, so the body sees NAME and the
// enclosing script's locals and nothing else.
//
// THE SEQUENCE IS EVALUATED ONCE. It is bound to a register before the loop header, so a sequence
// that is an expensive call, or one whose value would change between trips, is the same list on
// every trip.
//
// THE INDEX ADVANCES BEFORE THE BODY, not after it. Both orders visit the same elements, but only
// this one survives a jump back to the loop header from inside the body: an increment written after
// the body would be skipped by such a jump and the loop would not advance.
//
// THE LOOP ITSELF IS While's. This builds the three bindings and the condition and hands them to
// the same lowering `while` uses, so there is one back edge, one condition fault and one loop in
// the builder rather than two that have to stay in step. The fault site is relabelled afterwards,
// because the construct the author wrote is the one a diagnostic has to name.
//
// A SEQUENCE THAT IS NOT A LIST IS A FAULT -- the script ends with ERROR -- and that is what the
// `error` arm of the length binding is for: the length is ERROR, so the loop's condition is ERROR,
// which is the same condition fault an `if` on a non-boolean takes. An empty loop would be the other
// answer, and it is the wrong one for the same reason a false branch is the wrong answer to an
// undefined condition: a misspelled attribute would turn the loop into one that quietly does
// nothing, and a script runs once, so there is no second record to notice. A string is not a list
// here either: `size("abc")` is 3, but `"abc"[0]` is ERROR, so a loop over a string would run three
// times producing ERROR rather than say that a string is not a sequence.
//
// TERMINATION is the step budget's, exactly as it is for `while`. The condition is a value
// operation, so no trip is free, and the loop needs no bound of its own.

// For runs body once for each element of seq, with name bound to that element.
//
// name is an ordinary script local: it is function-scoped like any other, it is still bound after
// the loop (holding the last element visited), and it is UNDEFINED if the loop ran zero times and
// nothing else bound it.
//
// A seq that is not a list faults the script. An empty list is not a fault: the body runs zero times.
func (s *Script) For(name string, seq ast.Expr, body func(*Script)) *Script {
	s.stage(func() { s.forNow(name, seq, body) })
	return s
}

func (s *Script) forNow(name string, seq ast.Expr, body func(*Script)) {
	if s.err != nil {
		return
	}
	if s.hasRes {
		s.err = errors.New("vm: For after Result")
		return
	}
	if foldLocal(name) == "" {
		s.err = errors.New("vm: empty loop variable name")
		return
	}

	// Named for the register they start at, which is unique within the program and needs no counter
	// on the builder to stay that way.
	tag := fmt.Sprintf("for#%d.", len(s.c.p.locals))
	seqName, lenName, idxName := tag+"seq", tag+"n", tag+"i"
	seqReg := s.newFuncReg(seqName)
	lenReg := s.newFuncReg(lenName)
	idxReg := s.newFuncReg(idxName)

	// The sequence is emitted BEFORE the loop's own names are bound, so it is an expression of the
	// enclosing scope like any other and cannot accidentally read one of them.
	s.emitExpr(seq)
	s.c.ins(OpStoreLocal, seqReg)

	restore := s.bindLoopNames(map[string]int32{seqName: seqReg, lenName: lenReg, idxName: idxReg})

	seqRef := func() ast.Expr { return ast.NewAttributeReference(seqName, ast.NoScope) }
	idxRef := func() ast.Expr { return ast.NewAttributeReference(idxName, ast.NoScope) }

	// ifThenElse is a native BRANCH (see OpBranchCond), not a call, so it reads these registers.
	s.emitExpr(&ast.FunctionCall{Name: "ifThenElse", Args: []ast.Expr{
		&ast.FunctionCall{Name: "isList", Args: []ast.Expr{seqRef()}},
		&ast.FunctionCall{Name: "size", Args: []ast.Expr{seqRef()}},
		&ast.ErrorLiteral{},
	}})
	s.c.ins(OpStoreLocal, lenReg)

	s.emitExpr(&ast.IntegerLiteral{Value: 0})
	s.c.ins(OpStoreLocal, idxReg)

	// THE LOOP IS While's, not a second one. Everything a loop needs -- the back edge, the condition
	// fault, the loop depth an inlined call checks, and whatever the builder's loops grow next --
	// belongs to one lowering, and a `for` that had its own copy would be the thing that stopped
	// agreeing with it.
	cond := &ast.BinaryOp{Op: "<", Left: idxRef(), Right: ast.NewAttributeReference(lenName, ast.NoScope)}
	s.whileNow(cond, func(sc *Script) {
		// The element, bound through Let so that NAME is a local in every way the rest of the
		// language means one, including a rebinding of a name the enclosing code already bound.
		sc.letNow(name, &ast.SubscriptExpr{Container: seqRef(), Index: idxRef()})
		sc.emitExpr(&ast.BinaryOp{Op: "+", Left: idxRef(), Right: &ast.IntegerLiteral{Value: 1}})
		sc.c.ins(OpStoreLocal, idxReg)
		// From here on the body sees NAME and the enclosing locals, and not the loop's own
		// registers.
		restore()
		if body != nil {
			body(sc)
		}
	})
	s.c.p.renameLastFaultSite("for")
}

// bindLoopNames makes the loop's own registers readable by name while the header is emitted, and
// returns the function that takes them away again.
//
// It saves and restores whatever the name map already held, which matters only for an author who
// spelled one of these names with ClassAd's quoted-identifier syntax -- `'for#3.seq' = 1` -- and is
// cheap enough that the loop does not have to reason about whether anyone would.
func (s *Script) bindLoopNames(names map[string]int32) func() {
	type prior struct {
		idx   int32
		bound bool
	}
	saved := make(map[string]prior, len(names))
	for name, reg := range names {
		folded := foldLocal(name)
		idx, bound := s.byName[folded]
		saved[folded] = prior{idx: idx, bound: bound}
		s.byName[folded] = reg
	}
	s.c.locals = s.byName
	return func() {
		for folded, p := range saved {
			if p.bound {
				s.byName[folded] = p.idx
			} else {
				delete(s.byName, folded)
			}
		}
		s.c.locals = s.byName
	}
}
