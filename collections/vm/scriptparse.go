package vm

import (
	"fmt"
	"strings"

	"github.com/PelicanPlatform/classad/ast"
	"github.com/PelicanPlatform/classad/parser"
)

// THE SURFACE SYNTAX FOR A SCRIPT, and the one rule that shapes all of it: an expression is a
// ClassAd expression, parsed by the ClassAd parser, unchanged.
//
// The builder in script.go settled what a name means before anything could be written down, and this
// file does not get to revisit any of it. A bare bound name is a local; anything else is an
// attribute; `x = x + 1` reads the ATTRIBUTE x and binds the LOCAL x; `MY.x` is always the attribute.
// Everything here is a transcription into Let/If/While/Result calls, which is deliberate: the binding
// model is already tested and fuzzed against classad.Eval, and a parser that reimplemented any part
// of it would be a second model to keep in agreement with the first.
//
// WHAT THE LANGUAGE IS:
//
//	want = MY.RequestMemory         # bind a local; the right-hand side is any ClassAd expression
//	want += 512                     # sugar for `want = want + 512`
//	if want is undefined:           # `is` is ClassAd's =?=, so this is an ordinary expression
//	    want = 2048
//	elif want < 512:
//	    want = 512
//	else:
//	    pass
//	while want > 65536:
//	    if want is error:
//	        break                   # leave the innermost enclosing 'while'
//	    want = want / 2
//	for size in MY.Sizes:           # one trip per element of a list
//	    want += size
//	result = want                   # the script's value; top level, last, exactly once
//
// `break` leaves the innermost enclosing `while` and `continue` starts its next iteration, which
// re-tests the condition exactly as falling off the end of the body does. Both take no operand, and
// both are refused with a line and column when no loop encloses them.
//
// FUNCTIONS:
//
//	def over_cap(requested, cap):   # top level only, and visible to the WHOLE script, so a call
//	    if requested is undefined:  # may appear above the def that names it
//	        return cap
//	    return requested
//
// A call is expanded where it is written, which is where the rules that surprise people come from.
// Every argument is evaluated in the caller, left to right, exactly once, before the body runs --
// including an argument whose parameter the body never reads. The body is function-scoped: a bare
// name in it is a parameter, a name the body itself binds, or an ATTRIBUTE of the subject ad, never
// a local of the code that called it. A function is therefore not pure, and one with no parameters
// is still useful: `def over(): return RequestMemory > 4096` reads the ad. `return` with no operand,
// and falling off the end, both give UNDEFINED. Recursion is refused, directly or in a cycle,
// because an expansion that contains itself has no end; see scriptfunc.go.
//
// A `break` or a `continue` in a body is bound by a loop IN THAT BODY, and refused when there is
// none -- including when the call itself sits inside a loop. The expansion puts the body's
// instructions inside the caller's loop, so a `break` bound by the call site would make the same
// function mean one thing where it is called from a loop and nothing at all where it is not.
//
// SOFT KEYWORDS, which is a requirement and not a style choice. ClassAd attribute names are
// case-insensitive, so `For`, `IF` and `Result` have to stay usable as attributes. A word is a
// keyword only when it is spelled in lowercase AND leads a statement; everywhere else it lexes as an
// ordinary identifier, because that is all the ClassAd parser will ever see of it. The residual case
// -- wanting a local named exactly lowercase `for` -- is covered by ClassAd's own quoted-identifier
// syntax, which is accepted on the left of an assignment for that reason: `'for' = 1`.
//
// NO ONE-LINE SUITES, and this is what makes the block colon findable with no lookahead. `:` is
// already a ClassAd token -- the ternary's -- so a header's expression may contain colons:
// `if a ? b : c:` has two. The rule that saves it is that no valid ClassAd expression ends with a
// bare `:`. The ternary requires an expression after its colon, and the Elvis `?:` is one token that
// requires a right operand. So the LAST token of a header line is the block colon, always. Allowing
// `if x: y = 1` would destroy that: the line would end in `1`, and finding the block colon would
// require backtracking over an arbitrary expression.
//
// WHAT IS DELIBERATELY NOT HERE. Ad writes (`MY.X = ...`, `self[e] = ...`, `del`), `fail` and
// `defer` are all refused by name with a message saying so. None of them has a machine behind it --
// the compiler has no opcode that mutates the scope -- and a parser that accepted them would have
// to invent one. Refusing a word the language is going to grow is a better failure than parsing it
// into something that is not what it says.
//
// `for NAME in SEQUENCE:` IS A COUNTED `while`, and the parser spells it rather than implements it:
// the sequence is bound once, its length is bound once, and an index reads one element per trip.
// NAME is an ordinary local, function-scoped like every other, and the body is an ordinary suite. A
// sequence that is not a list is a FAULT rather than an empty loop, for the same reason a
// non-boolean `if` condition is. See Script.For, which holds the lowering and the reasoning.

// ScriptSyntaxError is a syntax error with the position of the thing that caused it.
//
// It carries the offending source line so a caller that prints the error gets the caret for free. A
// parser whose errors say only "syntax error" is a parser people route around, and the script is a
// place where the mistake is usually an indent or a stray colon -- exactly the mistakes a column
// pins down and a prose message does not.
type ScriptSyntaxError struct {
	Line   int    // 1-based
	Column int    // 1-based, counted in runes
	Msg    string // what went wrong, and where possible what was expected instead
	Source string // the text of Line, for the caret
}

func (e *ScriptSyntaxError) Error() string {
	s := fmt.Sprintf("line %d, column %d: %s", e.Line, e.Column, e.Msg)
	if e.Source == "" {
		return s
	}
	return s + "\n" + e.Source + "\n" + strings.Repeat(" ", max(e.Column-1, 0)) + "^"
}

// ParseScript parses ClassAd Script source and compiles it.
//
// The returned program is the same thing NewScript()...Compile() returns and runs identically; there
// is no second execution path for parsed scripts, and no part of the compiler knows which way a
// script arrived.
func ParseScript(src string) (*ScriptProgram, error) {
	s, err := ParseScriptBuilder(src)
	if err != nil {
		return nil, err
	}
	return s.Compile()
}

// ParseScriptBuilder parses source into an uncompiled Script, for a caller that wants to inspect or
// extend it before compiling.
func ParseScriptBuilder(src string) (*Script, error) {
	p := &scriptParser{pos: newPosIndex(src)}
	lines, clean, err := scanLines(src, p.pos)
	if err != nil {
		return nil, err
	}
	p.clean = clean
	p.lines = lines
	stmts, err := p.parseTop()
	if err != nil {
		return nil, err
	}
	return emitScript(stmts, p.pos)
}

// ---------------------------------------------------------------------------
// The statement tree
//
// Statements are built into a tree first and lowered into builder calls afterwards, rather than
// emitted as they are parsed. The reason is If: the builder takes its branches as closures, so a
// one-pass parser would have to parse the `then` block from inside the callback that emits it, and
// the parser's position would be buried in a stack of closures the moment anything went wrong.
// ---------------------------------------------------------------------------

// stmt is one statement. Every one carries the source offset of the line it came from, because a
// statement's expression can still be refused when it is LOWERED rather than when it is parsed --
// the compiler is the only thing that knows which of its subtrees it delegates -- and an error
// discovered then has to come back out with a position on it like every other.
type stmt interface{ at() int }

// stmtAt is embedded in every statement to carry that position and to be the thing that makes it a
// stmt. A separate empty marker method would say the same and be unreachable code, since the
// position every statement has to answer for already distinguishes them.
type stmtAt struct{ off int }

func (s stmtAt) at() int { return s.off }

type letStmt struct {
	stmtAt
	name string
	expr ast.Expr
}

type ifStmt struct {
	stmtAt
	cond ast.Expr
	then []stmt
	els  []stmt
}

type whileStmt struct {
	stmtAt
	cond ast.Expr
	body []stmt
}

// forStmt is `for name in seq:` over body. It is kept whole rather than desugared here, so the
// shape the author wrote is the shape a diagnostic names; the lowering into a counted loop is the
// builder's, in scriptfor.go.
type forStmt struct {
	stmtAt
	name string
	seq  ast.Expr
	body []stmt
}

type resultStmt struct {
	stmtAt
	expr ast.Expr
}

type defStmt struct {
	stmtAt
	name   string
	params []string
	body   []stmt
}

// returnStmt carries a nil expr for a bare `return`, which is a return of UNDEFINED and not a
// statement with a missing part.
type returnStmt struct {
	stmtAt
	expr ast.Expr
}

// breakStmt and continueStmt carry nothing but their position. Which loop each one names is settled
// by the builder, which is the only thing that knows the loop's addresses; the parser does not even
// track whether one encloses the statement.
type breakStmt struct{ stmtAt }

type continueStmt struct{ stmtAt }

// ---------------------------------------------------------------------------
// Parsing
// ---------------------------------------------------------------------------

type scriptParser struct {
	pos   *posIndex
	clean []byte
	lines []scriptLine
	idx   int
	// levels is the stack of indentation columns currently open. A dedent has to land on one of
	// them; landing between two is the error Python calls "unindent does not match any outer
	// indentation level", and it is worth detecting because the alternative -- silently treating it
	// as the nearest enclosing block -- puts a statement somewhere the author did not write it.
	levels []int

	// inFunc says whether a function body is being parsed, which is what makes `return` a statement
	// and `result` still not one.
	inFunc bool
}

func (p *scriptParser) parseTop() ([]stmt, error) {
	if len(p.lines) == 0 {
		return nil, p.pos.errorAt(0, "the script is empty: it needs at least a 'result' statement")
	}
	if p.lines[0].indent != 0 {
		return nil, p.at(p.lines[0], "unexpected indent: the first statement must start at column 1")
	}
	p.levels = []int{0}
	stmts, err := p.statements(0)
	if err != nil {
		return nil, err
	}
	return stmts, p.checkResult(stmts)
}

// parseModuleTop parses a MODULE: top-level `def` statements and nothing else.
//
// It is parseTop's sibling rather than a mode of it, because the two differ in exactly one place --
// what a top-level statement may be -- and a flag threaded through parseTop would put that question
// somewhere a reader of either language has to answer it.
//
// Everything below the top level is identical: a `def` body is parsed by the same code whichever
// entry point reached it, so there is no possibility of a statement meaning one thing in a script
// and another in a module.
func (p *scriptParser) parseModuleTop() ([]stmt, error) {
	if len(p.lines) == 0 {
		return nil, fmt.Errorf("%w: the source is empty", ErrNoFunctions)
	}
	if p.lines[0].indent != 0 {
		return nil, p.at(p.lines[0], "unexpected indent: the first statement must start at column 1")
	}
	p.levels = []int{0}
	stmts, err := p.statements(0)
	if err != nil {
		return nil, err
	}
	for _, st := range stmts {
		if _, isDef := st.(*defStmt); isDef {
			continue
		}
		// A `result` gets its own message because it is the mistake an author of scripts makes
		// first, and because "this is not a def" does not say what to do about it.
		if _, isResult := st.(*resultStmt); isResult {
			return nil, p.pos.errorAt(st.at(), "a module has no 'result': it defines functions "+
				"rather than computing a value, and each function's value is its 'return'")
		}
		// Everything else, a binding included. A top-level binding has no moment at which it would
		// be computed: a module is never run, only the functions it exports are, each with its own
		// registers. Bind inside the function that needs the value.
		return nil, p.pos.errorAt(st.at(), "a module holds only 'def' statements at the top level: "+
			"a module is not run, so there is no point at which a statement outside a function "+
			"would execute")
	}
	if len(stmts) == 0 {
		return nil, fmt.Errorf("%w: the source holds no statements", ErrNoFunctions)
	}
	return stmts, nil
}

// checkResult enforces that `result` is present, top level, and last.
//
// Compile would reject a missing result on its own, but with no position and no hint. The ordering
// rule is this parser's: the builder requires Result to be the final call, and a script that reads
// as though statements run after its answer has been computed is a script whose author has the wrong
// model of it.
func (p *scriptParser) checkResult(stmts []stmt) error {
	for i, s := range stmts {
		if _, ok := s.(*resultStmt); !ok {
			continue
		}
		for _, later := range stmts[i+1:] {
			// A `def` is a declaration rather than a step, so it may sit below the result it is
			// called from -- the same freedom a call above the definition has. Anything else is
			// blamed on the statement that cannot run, not on the result, because that is the line
			// the author has to delete or move.
			if _, isDef := later.(*defStmt); isDef {
				continue
			}
			return p.pos.errorAt(later.at(), "no statements may follow 'result'")
		}
		return nil
	}
	return p.pos.errorAt(len(p.pos.src), "the script has no 'result' statement")
}

// statements parses every statement at one indentation column, stopping at the first line that
// belongs to an enclosing block.
func (p *scriptParser) statements(indent int) ([]stmt, error) {
	var out []stmt
	for p.idx < len(p.lines) {
		ln := p.lines[p.idx]
		if ln.indent > indent {
			return nil, p.at(ln, "unexpected indent")
		}
		if ln.indent < indent {
			if !p.knownLevel(ln.indent) {
				return nil, p.at(ln, "unindent does not match any outer indentation level")
			}
			return out, nil
		}
		s, err := p.statement(indent)
		if err != nil {
			return nil, err
		}
		if s != nil {
			out = append(out, s)
		}
	}
	return out, nil
}

// block parses the indented suite that follows a header line.
func (p *scriptParser) block(header scriptLine, outer int, kw string) ([]stmt, error) {
	if p.idx >= len(p.lines) || p.lines[p.idx].indent <= outer {
		return nil, p.atEnd(header, fmt.Sprintf("expected an indented block after '%s'", kw))
	}
	indent := p.lines[p.idx].indent
	p.levels = append(p.levels, indent)
	stmts, err := p.statements(indent)
	p.levels = p.levels[:len(p.levels)-1]
	if err != nil {
		return nil, err
	}
	// An empty result is not an empty block: a suite of nothing but `pass` produces no statements
	// and is exactly what `pass` is for. The block's existence was settled by the indentation above.
	return stmts, nil
}

func (p *scriptParser) knownLevel(indent int) bool {
	for _, l := range p.levels {
		if l == indent {
			return true
		}
	}
	return false
}

// statement parses one logical line and whatever suite belongs to it. It returns a nil stmt for
// `pass`, which is a statement that produces nothing on purpose.
func (p *scriptParser) statement(indent int) (stmt, error) {
	ln := p.lines[p.idx]
	word, after := p.leadingWord(ln)
	switch word {
	case "if":
		p.idx++
		return p.ifChain(ln, after, indent, "if")
	case "elif":
		return nil, p.at(ln, "'elif' without a matching 'if'")
	case "else":
		return nil, p.at(ln, "'else' without a matching 'if'")
	case "while":
		p.idx++
		cond, err := p.headerExpr(ln, after, "while")
		if err != nil {
			return nil, err
		}
		body, err := p.block(ln, indent, "while")
		if err != nil {
			return nil, err
		}
		return &whileStmt{stmtAt: stmtAt{ln.off}, cond: cond, body: body}, nil
	case "for":
		p.idx++
		return p.forLine(ln, after, indent)
	case "pass":
		p.idx++
		if p.text(after, ln.end) != "" {
			return nil, p.pos.errorAt(after, "expected end of line after 'pass'")
		}
		return nil, nil
	case "def":
		p.idx++
		return p.defLine(ln, after, indent)
	case "return":
		p.idx++
		return p.returnLine(ln, after)
	case "break", "continue":
		p.idx++
		if p.text(after, ln.end) != "" {
			return nil, p.pos.errorAt(after, fmt.Sprintf("expected end of line after '%s': it "+
				"takes no operand", word))
		}
		if word == "break" {
			return &breakStmt{stmtAt{ln.off}}, nil
		}
		return &continueStmt{stmtAt{ln.off}}, nil
	case "del", "fail", "defer":
		return nil, p.at(ln, fmt.Sprintf("'%s' is not implemented in ClassAd Script", word))
	case "result":
		p.idx++
		return p.resultLine(ln, after, indent)
	}
	p.idx++
	return p.assignment(ln)
}

// ifChain parses an if/elif/else cascade. An `elif` is an `if` in the else block of the one before
// it, which is what the builder's two-branch If can express and what the language means anyway.
func (p *scriptParser) ifChain(ln scriptLine, after, indent int, kw string) (stmt, error) {
	cond, err := p.headerExpr(ln, after, kw)
	if err != nil {
		return nil, err
	}
	then, err := p.block(ln, indent, kw)
	if err != nil {
		return nil, err
	}
	out := &ifStmt{stmtAt: stmtAt{ln.off}, cond: cond, then: then}
	if p.idx >= len(p.lines) || p.lines[p.idx].indent != indent {
		return out, nil
	}
	next := p.lines[p.idx]
	switch word, nafter := p.leadingWord(next); word {
	case "elif":
		p.idx++
		nested, err := p.ifChain(next, nafter, indent, "elif")
		if err != nil {
			return nil, err
		}
		out.els = []stmt{nested}
	case "else":
		p.idx++
		if t := p.text(nafter, next.end); t != ":" {
			return nil, p.pos.errorAt(nafter, "expected ':' directly after 'else'")
		}
		els, err := p.block(next, indent, "else")
		if err != nil {
			return nil, err
		}
		out.els = els
	}
	return out, nil
}

// defLine parses `def NAME(p1, p2):` and the indented body that follows it.
//
// TOP LEVEL ONLY. A function is a name the whole script can call, so there is nothing for a `def`
// inside an `if` to mean: the branch it sits in decides whether a line runs, not whether a name
// exists, and the call sites were compiled before the branch is reached. Nesting one inside another
// `def` is refused for the same reason and one more -- a nested function would read the enclosing
// body's parameters, which is a closure, and nothing here captures anything.
func (p *scriptParser) defLine(ln scriptLine, after, indent int) (stmt, error) {
	if indent != 0 || p.inFunc {
		return nil, p.at(ln, "'def' must be a top-level statement: a function is visible to the "+
			"whole script, so there is nothing for one nested inside a block to mean")
	}
	end, err := p.blockColon(ln, after, "def")
	if err != nil {
		return nil, err
	}
	name, params, err := p.defHeader(after, end)
	if err != nil {
		return nil, err
	}
	p.inFunc = true
	body, err := p.block(ln, indent, "def")
	p.inFunc = false
	if err != nil {
		return nil, err
	}
	return &defStmt{stmtAt: stmtAt{ln.off}, name: name, params: params, body: body}, nil
}

// defHeader reads `NAME(p1, p2)` out of the range between `def` and the block colon.
func (p *scriptParser) defHeader(from, to int) (string, []string, error) {
	from, to = trimRange(p.clean, from, to)
	text := p.text(from, to)
	open := strings.IndexByte(text, '(')
	if open < 0 || !strings.HasSuffix(text, ")") {
		return "", nil, p.pos.errorAt(from, "expected a function name and a parameter list, as in 'def f(x):'")
	}
	name := strings.TrimSpace(text[:open])
	if !isBareIdent(name) {
		return "", nil, p.pos.errorAt(from, fmt.Sprintf("expected a function name, found %q", name))
	}
	inner := strings.TrimSpace(text[open+1 : len(text)-1])
	if inner == "" {
		// `def now():` -- a function of the ad alone, which is a function like any other here,
		// since a body reads attributes whether or not it has parameters.
		return name, nil, nil
	}
	var params []string
	for _, part := range strings.Split(inner, ",") {
		pname := strings.TrimSpace(part)
		if !isBareIdent(pname) {
			return "", nil, p.pos.errorAt(from+open+1,
				fmt.Sprintf("expected a parameter name, found %q", pname))
		}
		params = append(params, pname)
	}
	return name, params, nil
}

// forLine parses `for NAME in <expr>:` and the indented suite that follows it.
func (p *scriptParser) forLine(ln scriptLine, after, indent int) (stmt, error) {
	end, err := p.blockColon(ln, after, "for")
	if err != nil {
		return nil, err
	}
	name, rest, err := p.forTarget(after, end)
	if err != nil {
		return nil, err
	}
	if strings.TrimSpace(p.text(rest, end)) == "" {
		return nil, p.pos.errorAt(rest, "expected a sequence after 'in', as in 'for x in MY.Sizes:'")
	}
	seq, err := p.expr(rest, end)
	if err != nil {
		return nil, err
	}
	body, err := p.block(ln, indent, "for")
	if err != nil {
		return nil, err
	}
	return &forStmt{stmtAt: stmtAt{ln.off}, name: name, seq: seq, body: body}, nil
}

// forTarget reads `NAME in` from the head of a `for` header, returning the name and the offset just
// past the `in`.
//
// `in` is read as a WORD rather than searched for, which is what keeps it out of the expression: a
// sequence may perfectly well contain the letters -- `for x in Inputs:` -- and a search would cut
// the header in the wrong place. The loop variable is one token, so the word after it is the only
// place `in` can be.
func (p *scriptParser) forTarget(from, to int) (string, int, error) {
	from, to = trimRange(p.clean, from, to)
	if from >= to {
		return "", 0, p.pos.errorAt(from, "expected a loop variable, as in 'for x in MY.Sizes:'")
	}
	if p.clean[from] == '\'' {
		// The quoted form, for a name that is not a bare identifier -- the same escape hatch an
		// assignment target has.
		name, n, ok := unquoteIdent(string(p.clean[from:to]))
		if !ok {
			return "", 0, p.pos.errorAt(from, "expected a quoted attribute name")
		}
		return p.forIn(name, from, from+n, to)
	}
	i := from
	for i < to && identPart(p.clean[i]) {
		i++
	}
	name := string(p.clean[from:i])
	if !isBareIdent(name) {
		if i > from {
			return "", 0, p.pos.errorAt(from, fmt.Sprintf("expected a loop variable name, found %q", name))
		}
		return "", 0, p.pos.errorAt(from,
			fmt.Sprintf("expected a loop variable name, found %q", p.text(from, to)))
	}
	return p.forIn(name, from, i, to)
}

// forIn consumes the `in` that separates a `for` header's variable from its sequence. nameAt is
// where the variable started, for the one diagnostic that is about the variable and not about `in`.
func (p *scriptParser) forIn(name string, nameAt, from, to int) (string, int, error) {
	i := from
	for i < to && (p.clean[i] == ' ' || p.clean[i] == '\t') {
		i++
	}
	j := i
	for j < to && identPart(p.clean[j]) {
		j++
	}
	if string(p.clean[i:j]) != "in" {
		if name == "in" {
			// `for in L:` read `in` as the variable, which is the one case where the complaint is
			// about what is missing in FRONT of it.
			return "", 0, p.pos.errorAt(nameAt,
				"expected a loop variable before 'in', as in 'for x in MY.Sizes:'")
		}
		return "", 0, p.pos.errorAt(i, "expected 'in' after the loop variable, as in 'for x in MY.Sizes:'")
	}
	return name, j, nil
}

// returnLine parses `return` and `return <expr>`.
func (p *scriptParser) returnLine(ln scriptLine, after int) (stmt, error) {
	if !p.inFunc {
		return nil, p.at(ln, "'return' outside a function body: the script's value is 'result'")
	}
	if p.text(after, ln.end) == "" {
		return &returnStmt{stmtAt: stmtAt{ln.off}}, nil
	}
	expr, err := p.expr(after, ln.end)
	if err != nil {
		return nil, err
	}
	return &returnStmt{stmtAt: stmtAt{ln.off}, expr: expr}, nil
}

// resultLine parses `result = <expr>`.
func (p *scriptParser) resultLine(ln scriptLine, after, indent int) (stmt, error) {
	if indent != 0 {
		return nil, p.at(ln, "'result' must be a top-level statement")
	}
	op := findAssign(p.clean, after, ln.end)
	if op.at < 0 || op.aug != 0 {
		return nil, p.pos.errorAt(after, "expected '=' after 'result'")
	}
	expr, err := p.expr(op.at+1, ln.end)
	if err != nil {
		return nil, err
	}
	return &resultStmt{stmtAt: stmtAt{ln.off}, expr: expr}, nil
}

// assignment parses `<name> = <expr>` and the augmented forms.
func (p *scriptParser) assignment(ln scriptLine) (stmt, error) {
	op := findAssign(p.clean, ln.off, ln.end)
	if op.at < 0 {
		return nil, p.at(ln, "expected a statement: an assignment, 'if', 'while', 'for', 'pass' or 'result'")
	}
	lhsEnd := op.at
	if op.aug != 0 {
		lhsEnd--
	}
	name, err := p.localName(ln.off, lhsEnd)
	if err != nil {
		return nil, err
	}
	rhs, err := p.expr(op.at+1, ln.end)
	if err != nil {
		return nil, err
	}
	if op.aug != 0 {
		// `x op= e` is exactly `x = x op e`, including the part that surprises people: the `x` on
		// the right is read BEFORE this statement binds anything, so on a first use it is the
		// attribute. That falls out of emitting the right-hand side before the binding, which is
		// what Let already does, so the sugar needs nothing of its own.
		rhs = &ast.BinaryOp{
			Op:    string(op.aug),
			Left:  ast.NewAttributeReference(name, ast.NoScope),
			Right: &ast.ParenExpr{Inner: rhs},
		}
	}
	return &letStmt{stmtAt: stmtAt{ln.off}, name: name, expr: rhs}, nil
}

// localName reads an assignment target: a bare ClassAd identifier, or a quoted one.
//
// The quoted form is the escape hatch for the soft keywords. It is the same syntax ClassAd already
// uses for an attribute whose name is not a bare identifier, so `'for' = 1` binds a local that
// `'for' + 1` then reads, with no new concept anywhere.
func (p *scriptParser) localName(from, to int) (string, error) {
	from, to = trimRange(p.clean, from, to)
	if from >= to {
		return "", p.pos.errorAt(from, "expected a local name")
	}
	if p.clean[from] == '\'' {
		name, end, ok := unquoteIdent(p.text(from, to))
		if !ok || from+end != to {
			return "", p.pos.errorAt(from, "expected a quoted attribute name")
		}
		return name, nil
	}
	text := p.text(from, to)
	if !isBareIdent(text) {
		switch {
		case strings.ContainsAny(text, ".["):
			return "", p.pos.errorAt(from,
				"the left-hand side of an assignment must be a local name; writing to the ad is not implemented")
		default:
			return "", p.pos.errorAt(from, fmt.Sprintf("expected a local name, found %q", text))
		}
	}
	return text, nil
}

// headerExpr parses the condition of an `if`, `elif` or `while`, which is everything between the
// keyword and the block colon.
func (p *scriptParser) headerExpr(ln scriptLine, after int, kw string) (ast.Expr, error) {
	end, err := p.blockColon(ln, after, kw)
	if err != nil {
		return nil, err
	}
	if strings.TrimSpace(p.text(after, end)) == "" {
		return nil, p.pos.errorAt(after, fmt.Sprintf("expected a condition after '%s'", kw))
	}
	return p.expr(after, end)
}

// blockColon finds the block colon of a header line and returns its offset, which is where the
// header's expression stops.
func (p *scriptParser) blockColon(ln scriptLine, after int, kw string) (int, error) {
	if ln.end <= after || p.clean[ln.end-1] != ':' {
		return 0, p.atEnd(ln, fmt.Sprintf("expected ':' at the end of the '%s' header", kw))
	}
	colon := ln.end - 1
	// The one spelling that puts a colon at the end of a line without it being the block colon is
	// the Elvis operator, whose two characters the ClassAd lexer joins across whitespace. Taking it
	// as the block colon would leave a dangling `?` and produce a baffling expression error instead
	// of the true complaint, which is that the header has no colon of its own.
	for j := colon - 1; j >= after; j-- {
		switch p.clean[j] {
		case ' ', '\t':
			continue
		case '?':
			return 0, p.atEnd(ln, fmt.Sprintf("expected ':' at the end of the '%s' header", kw))
		}
		break
	}
	return colon, nil
}

// expr parses one ClassAd expression out of the cleaned source range.
//
// The range is trimmed BEFORE the offset is remembered, so the position reported for a failure
// inside it is the position of the expression's first character rather than of the `=` or keyword in
// front of it. Trimming afterwards, or passing a trimmed copy while keeping the untrimmed offset,
// would put every such diagnostic a few columns to the left of the thing it is about.
func (p *scriptParser) expr(from, to int) (ast.Expr, error) {
	from, to = trimRange(p.clean, from, to)
	text := string(p.clean[from:to])
	e, err := parser.ParseExpr(text)
	if err == nil {
		return e, nil
	}
	return nil, p.exprError(from, text, err)
}

// exprError lifts the ClassAd parser's diagnostic into the script's coordinates.
//
// ParseExpr reports a line and column relative to the fragment it was handed, which for a statement
// on line 40 of a script would read "line 1, col 7" -- true, useless, and actively misleading next to
// the script's other messages. The fragment's own start position is known exactly, because the
// cleaned buffer has the same length as the source, so the two can simply be added.
func (p *scriptParser) exprError(from int, text string, err error) error {
	msg, line, col, ok := splitParserError(err.Error())
	if !ok {
		return p.pos.errorAt(from, err.Error())
	}
	startLine, startCol := p.pos.lineCol(from)
	if line == 1 {
		col += startCol - 1
	}
	line += startLine - 1
	// THE WORST PIECE OF PYTHON MUSCLE MEMORY IN THIS LANGUAGE. `'...'` is a ClassAd quoted
	// ATTRIBUTE NAME, not a string: `x = 'alice'` binds x to whatever the attribute named alice
	// holds, and `x = 'alice' + 1` is a perfectly well-formed expression that means nothing like
	// what it looks like. The failure that brings a user here is usually two lines later, so the
	// hint goes on any parse error of a fragment containing one.
	if strings.ContainsRune(text, '\'') {
		msg += `; note that '...' is a quoted attribute name -- use "..." for a string`
	}
	return &ScriptSyntaxError{Line: line, Column: col, Msg: msg, Source: p.pos.sourceLine(line)}
}

// splitParserError pulls the position back out of a ClassAd parse error, whose first line reads
// `parse error at line L, col C: message`. Everything after that first line is the parser's own
// caret, which is drawn against the fragment and so is dropped in favour of this parser's, drawn
// against the script.
func splitParserError(s string) (msg string, line, col int, ok bool) {
	if nl := strings.IndexByte(s, '\n'); nl >= 0 {
		s = s[:nl]
	}
	const prefix = "parse error at line "
	if !strings.HasPrefix(s, prefix) {
		return "", 0, 0, false
	}
	rest := s[len(prefix):]
	sep := strings.Index(rest, ": ")
	if sep < 0 {
		return "", 0, 0, false
	}
	if _, err := fmt.Sscanf(rest[:sep], "%d, col %d", &line, &col); err != nil {
		return "", 0, 0, false
	}
	return rest[sep+2:], line, col, true
}

// leadingWord returns the identifier a line starts with, lowercased only if it already was, together
// with the offset just past it. A line that does not start with an identifier yields "".
func (p *scriptParser) leadingWord(ln scriptLine) (string, int) {
	i := ln.off
	if i >= ln.end || !identStart(p.clean[i]) {
		return "", ln.off
	}
	for i < ln.end && identPart(p.clean[i]) {
		i++
	}
	return string(p.clean[ln.off:i]), i
}

func (p *scriptParser) text(from, to int) string {
	if from >= to {
		return ""
	}
	return strings.TrimSpace(string(p.clean[from:to]))
}

func (p *scriptParser) at(ln scriptLine, msg string) error {
	return p.pos.errorAt(ln.off, msg)
}

func (p *scriptParser) atEnd(ln scriptLine, msg string) error {
	return p.pos.errorAt(ln.end, msg)
}

// ---------------------------------------------------------------------------
// Small lexical helpers, all working over the cleaned buffer
// ---------------------------------------------------------------------------

type assignOp struct {
	at  int  // offset of the '='
	aug byte // the operator of an augmented assignment, or 0
}

// findAssign locates the assignment operator of a statement: the first `=` at bracket depth zero
// that is not part of a comparison.
//
// It cannot collide with an augmented form, because no ClassAd production allows a binary arithmetic
// operator immediately followed by `=`: both sides of `+` must be expressions, so `+=` is a syntax
// error in expression position and is free for this.
func findAssign(buf []byte, from, to int) assignOp {
	depth := 0
	for i := from; i < to; i++ {
		switch c := buf[i]; {
		case c == '"' || c == '\'':
			i = skipQuoted(buf, i, to) - 1
		case c == '(' || c == '[' || c == '{':
			depth++
		case c == ')' || c == ']' || c == '}':
			depth--
		case c == '=' && depth == 0:
			// `==`, `=?=` and `=!=` all start here and are not assignments; `<=`, `>=` and `!=` end
			// here and are not either.
			if i+1 < to && (buf[i+1] == '=' || buf[i+1] == '?' || buf[i+1] == '!') {
				continue
			}
			if i > from {
				switch prev := buf[i-1]; prev {
				case '=', '<', '>', '!':
					continue
				case '+', '-', '*', '/', '%':
					return assignOp{at: i, aug: prev}
				}
			}
			return assignOp{at: i}
		}
	}
	return assignOp{at: -1}
}

// skipQuoted returns the offset just past the literal starting at i.
func skipQuoted(buf []byte, i, to int) int {
	quote := buf[i]
	for j := i + 1; j < to; j++ {
		if buf[j] == '\\' {
			j++
			continue
		}
		if buf[j] == quote {
			return j + 1
		}
	}
	return to
}

// unquoteIdent reads a `'...'` quoted attribute name, returning its value and the offset just past
// the closing quote.
func unquoteIdent(s string) (string, int, bool) {
	if s == "" || s[0] != '\'' {
		return "", 0, false
	}
	var b strings.Builder
	for i := 1; i < len(s); i++ {
		switch s[i] {
		case '\\':
			if i+1 >= len(s) {
				return "", 0, false
			}
			i++
			b.WriteByte(s[i])
		case '\'':
			if b.Len() == 0 {
				return "", 0, false
			}
			return b.String(), i + 1, true
		default:
			b.WriteByte(s[i])
		}
	}
	return "", 0, false
}

func trimRange(buf []byte, from, to int) (int, int) {
	for from < to && isLayout(buf[from]) {
		from++
	}
	for to > from && isLayout(buf[to-1]) {
		to--
	}
	return from, to
}

func isLayout(c byte) bool {
	return c == ' ' || c == '\t' || c == '\n' || c == '\r'
}

func isBareIdent(s string) bool {
	if s == "" || !identStart(s[0]) {
		return false
	}
	for i := 1; i < len(s); i++ {
		if !identPart(s[i]) {
			return false
		}
	}
	return true
}

func identStart(c byte) bool {
	return c == '_' || (c >= 'a' && c <= 'z') || (c >= 'A' && c <= 'Z')
}

func identPart(c byte) bool {
	return identStart(c) || (c >= '0' && c <= '9')
}

// ---------------------------------------------------------------------------
// Lowering into the builder
// ---------------------------------------------------------------------------

// emitScript walks the statement tree, making the Let/If/While/Func/Return/Result calls a
// hand-written builder script would have made.
//
// Every `def` is declared before any statement is lowered, so a call may appear above the function
// it names. The builder would allow that anyway -- it compiles nothing until Compile -- but this
// parser flushes after each statement to keep a refusal attached to the line that caused it, and
// the flush is what would otherwise make a forward call fail.
func emitScript(stmts []stmt, pos *posIndex) (*Script, error) {
	s := NewScript()
	e := &emitter{pos: pos}
	for _, st := range stmts {
		d, ok := st.(*defStmt)
		if !ok {
			continue
		}
		before := s.err
		s.Func(d.name, d.params, func(sc *Script) { e.block(sc, d.body) })
		if before == nil && s.err != nil {
			return nil, e.pos.errorAt(d.at(), detailOf(s.err))
		}
	}
	e.block(s, stmts)
	if e.err != nil {
		return nil, e.err
	}
	if s.err != nil {
		return nil, s.err
	}
	return s, nil
}

type emitter struct {
	pos *posIndex
	err error
}

// block lowers a run of statements, checking after each one whether the compiler refused it.
//
// The check belongs here, and it belongs after EACH statement rather than at the end, for one
// reason: this is the only place that still knows which line the compiler was lowering when it
// refused. Compile sees a finished program and a single recorded refusal, by which time the
// statement it came from is gone and the user is told what is wrong without being told where.
//
// The flush is what makes that true now that the builder stages its statements: without it the
// compiler would not have run yet, and every refusal would surface at Compile with no line.
func (e *emitter) block(s *Script, stmts []stmt) {
	for _, st := range stmts {
		if e.err != nil {
			return
		}
		hadCapture := s.c.capture != nil
		hadErr := s.err
		e.one(s, st)
		s.flush()
		switch {
		case e.err != nil:
			// A statement whose lowering expanded a call already blamed the line INSIDE the body
			// that the compiler refused, through this same loop one level down. That line is the
			// one the author edits, so the statement that contained the call does not take it over.
		case !hadCapture && s.c.capture != nil:
			e.err = e.pos.errorAt(st.at(), captureDetail(s.c.capture))
		case hadErr == nil && s.err != nil:
			e.err = e.pos.errorAt(st.at(), detailOf(s.err))
		}
	}
}

// detailOf renders a builder error for a diagnostic that supplies its own position, dropping the
// package prefix the builder's own callers need.
func detailOf(err error) string {
	return strings.TrimPrefix(strings.TrimPrefix(err.Error(), "vm: "), "script: ")
}

// one makes the builder call for a single statement. `pass` reaches here as no statement at all, so
// there is nothing to do for it and nothing to name. A `def` was declared before this walk began.
func (e *emitter) one(s *Script, st stmt) {
	switch v := st.(type) {
	case *letStmt:
		s.Let(v.name, v.expr)
	case *resultStmt:
		s.Result(v.expr)
	case *ifStmt:
		s.If(v.cond, e.suite(v.then), e.suite(v.els))
	case *whileStmt:
		s.While(v.cond, e.suite(v.body))
	case *forStmt:
		s.For(v.name, v.seq, e.suite(v.body))
	case *returnStmt:
		s.Return(v.expr)
	case *breakStmt:
		s.Break()
	case *continueStmt:
		s.Continue()
	case *defStmt:
	}
}

// suite turns a list of statements into the closure the builder's If and While take, or nil when
// there is nothing to run -- which the builder reads as "no branch here" rather than "an empty one",
// and which is the same thing.
func (e *emitter) suite(stmts []stmt) func(*Script) {
	if len(stmts) == 0 {
		return nil
	}
	return func(s *Script) { e.block(s, stmts) }
}
