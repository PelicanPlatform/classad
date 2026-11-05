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
//	    want = want / 2
//	result = want                   # the script's value; top level, last, exactly once
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
// WHAT IS DELIBERATELY NOT HERE. Ad writes (`MY.X = ...`, `self[e] = ...`, `del`), functions (`def`,
// `return`), `break`, `continue`, `fail` and `defer` are all refused by name with a message saying
// so. None of them has a machine behind it -- the compiler has no opcode that mutates the scope, no
// call frame and no way out of a loop -- and a parser that accepted them would have to invent one.
// Refusing a word the language is going to grow is a better failure than parsing it into something
// that is not what it says.
//
// `for` IS REFUSED FOR A MORE SPECIFIC REASON, and it is worth writing down because the shape of it
// is not obvious. A `for` over a list desugars cleanly into a counted `while` -- bind the sequence,
// bind its length, bind an index, and read element i each trip -- and every piece of that except one
// compiles natively today. The exception is `seq[i]`. A subscript is delegated to the tree-walking
// evaluator, which resolves names against the ad and cannot see a register, so the desugared loop
// would read the ATTRIBUTE named seq and the ATTRIBUTE named i, both absent, on every trip. (The
// compiler now refuses that rather than computing it; see LocalCaptureError.) `for` is therefore one
// native opcode away -- an element load with the semantics of evaluateSubscriptExpr -- and that
// opcode is a change to the instruction set, with its own vector decline, step cost and parity
// tests. It is not parser work, and guessing at it here to make a keyword appear would be the
// expensive half of the job done badly.

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
type stmt interface {
	stmtNode()
	at() int
}

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

type resultStmt struct {
	stmtAt
	expr ast.Expr
}

func (*letStmt) stmtNode()    {}
func (*ifStmt) stmtNode()     {}
func (*whileStmt) stmtNode()  {}
func (*resultStmt) stmtNode() {}

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
		if i != len(stmts)-1 {
			// Blamed on the statement that cannot run, not on the result, because that is the line
			// the author has to delete or move.
			return p.pos.errorAt(stmts[i+1].at(), "no statements may follow 'result'")
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
		return nil, p.at(ln, "'for' is not implemented in ClassAd Script: "+
			"a loop over a list needs an element load the compiler does not have yet; "+
			"use 'while' with an explicit index once it does")
	case "pass":
		p.idx++
		if p.text(after, ln.end) != "" {
			return nil, p.pos.errorAt(after, "expected end of line after 'pass'")
		}
		return nil, nil
	case "break", "continue", "def", "return", "del", "fail", "defer":
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
	if from < 0 {
		from = 0
	}
	if to > len(p.clean) {
		to = len(p.clean)
	}
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

// emitScript walks the statement tree, making the Let/If/While/Result calls a hand-written builder
// script would have made.
func emitScript(stmts []stmt, pos *posIndex) (*Script, error) {
	s := NewScript()
	e := &emitter{pos: pos}
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
func (e *emitter) block(s *Script, stmts []stmt) {
	for _, st := range stmts {
		if e.err != nil {
			return
		}
		had := s.c.capture != nil
		e.one(s, st)
		if !had && s.c.capture != nil {
			e.err = e.pos.errorAt(st.at(), captureDetail(s.c.capture))
		}
	}
}

// one makes the builder call for a single statement. `pass` reaches here as no statement at all, so
// there is nothing to do for it and nothing to name.
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
