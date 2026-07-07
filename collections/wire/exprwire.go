package wire

import (
	"bufio"
	"errors"
	"fmt"
	"strings"
	"sync"

	"github.com/PelicanPlatform/classad/parser"
)

// wireParseState is a reusable text->wire parse context: a strings.Reader and
// bufio.Reader feeding a StreamingLexer, plus the NodeEmitter (for its scratch
// buffer). Pooled so ParseExprToWire allocates neither a lexer/reader nor emitter
// scratch per call -- only the output node bytes.
type wireParseState struct {
	sr  *strings.Reader
	br  *bufio.Reader
	lex *parser.StreamingLexer
	p   exprParser
}

var wireParsePool = sync.Pool{
	New: func() any {
		sr := strings.NewReader("")
		br := bufio.NewReader(sr)
		return &wireParseState{sr: sr, br: br, lex: parser.NewExprLexer(br)}
	},
}

// ErrUnsupported signals the wire-native parser hit a construct it does not handle
// (a record literal, an int64-min magnitude, a computed select, ...). The caller
// should fall back to the reference parser, which is the source of truth.
var ErrUnsupported = errors.New("wire: expression construct not handled by the native parser")

// Left binding powers, from the ClassAd grammar's precedence (lowest -> highest).
const (
	bpTernary = 10
	bpOr      = 20
	bpAnd     = 30
	bpBitOr   = 40
	bpBitXor  = 50
	bpBitAnd  = 60
	bpCmpEq   = 70 // == != is isnt
	bpCmpRel  = 80 // < > <= >=
	bpShift   = 90 // << >> >>>
	bpAdd     = 100
	bpMul     = 110
	bpUnary   = 120 // ! ~ - + (prefix, right-assoc)
	bpElvis   = 130
	bpPostfix = 140 // . [
)

// ParseExprToWire parses a standalone ClassAd expression from input and appends its
// wire node bytes to dst (interning names into t), returning the extended buffer.
// Output is byte-identical to encoding the reference-parsed ast.Expr. It returns
// ErrUnsupported for constructs it does not handle (so the caller can fall back to
// parser.ParseExpr), or a syntax error for malformed input.
func ParseExprToWire(input string, t *InternTable, dst []byte) ([]byte, error) {
	return parseExprToWire(input, t, false, dst)
}

// parseExprToWire is ParseExprToWire with an explicit inline flag (inline-name node
// variants, no interning), for the persistent-store encoding.
func parseExprToWire(input string, t *InternTable, inline bool, dst []byte) ([]byte, error) {
	w := wireParsePool.Get().(*wireParseState)
	w.sr.Reset(input)
	w.br.Reset(w.sr)
	w.lex.ResetForExpr()
	p := &w.p
	p.lex = w.lex
	p.err = nil
	p.em.t = t
	p.em.inline = inline
	p.em.buf = dst // scratch retained across pool reuse

	p.advance()
	out, err := dst, p.err
	if err == nil {
		if err = p.parseExpr(0); err == nil {
			err = p.err
		}
		if err == nil && p.cur.Kind != parser.TokEOF {
			err = fmt.Errorf("wire: unexpected trailing token")
		}
		out = p.em.buf
	}
	p.lex = nil // don't retain the shared lexer reference across Put
	wireParsePool.Put(w)
	return out, err
}

type exprParser struct {
	lex *parser.StreamingLexer
	em  NodeEmitter
	cur parser.LexToken
	err error
}

func (p *exprParser) advance() {
	if p.err != nil {
		return
	}
	tok, err := p.lex.Next()
	if err != nil {
		p.err = err
		return
	}
	p.cur = tok
}

// lbp is the left binding power of a token as a binary/postfix operator, or 0 for a
// terminator/non-operator.
func lbp(k int) int {
	switch k {
	case '?':
		return bpTernary
	case parser.OR:
		return bpOr
	case parser.AND:
		return bpAnd
	case '|':
		return bpBitOr
	case '^':
		return bpBitXor
	case '&':
		return bpBitAnd
	case parser.EQ, parser.NE, parser.IS, parser.ISNT:
		return bpCmpEq
	case '<', '>', parser.LE, parser.GE:
		return bpCmpRel
	case parser.LSHIFT, parser.RSHIFT, parser.URSHIFT:
		return bpShift
	case '+', '-':
		return bpAdd
	case '*', '/', '%':
		return bpMul
	case parser.ELVIS:
		return bpElvis
	case '.', '[':
		return bpPostfix
	}
	return 0
}

// binOpString maps a binary-operator token to its canonical wire op string.
func binOpString(k int) (string, bool) {
	switch k {
	case '+', '-', '*', '/', '%', '<', '>', '&', '|', '^':
		return string(rune(k)), true
	case parser.LE:
		return "<=", true
	case parser.GE:
		return ">=", true
	case parser.EQ:
		return "==", true
	case parser.NE:
		return "!=", true
	case parser.IS:
		return "is", true
	case parser.ISNT:
		return "isnt", true
	case parser.LSHIFT:
		return "<<", true
	case parser.RSHIFT:
		return ">>", true
	case parser.URSHIFT:
		return ">>>", true
	case parser.AND:
		return "&&", true
	case parser.OR:
		return "||", true
	}
	return "", false
}

func (p *exprParser) parseExpr(minBP int) error {
	mark := p.em.Mark()
	if err := p.parsePrefix(); err != nil {
		return err
	}
	for p.err == nil {
		k := p.cur.Kind
		bp := lbp(k)
		if bp <= minBP {
			break
		}
		switch {
		case k == '?': // ternary cond ? true : false (right-assoc)
			p.advance()
			if err := p.parseExpr(0); err != nil { // true branch, up to ':'
				return err
			}
			if p.cur.Kind != ':' {
				return fmt.Errorf("wire: expected ':' in conditional")
			}
			p.advance()
			if err := p.parseExpr(bpTernary - 1); err != nil { // false branch grabs nested ternary
				return err
			}
			p.em.WrapCond(mark)
		case k == parser.ELVIS:
			p.advance()
			if err := p.parseExpr(bp); err != nil {
				return err
			}
			p.em.WrapElvis(mark)
		case k == '.': // select record.attr
			p.advance()
			if p.cur.Kind != parser.IDENTIFIER {
				return ErrUnsupported
			}
			attr := p.cur.Str
			p.advance()
			p.em.WrapSelect(mark, attr)
		case k == '[': // subscript container[index]
			p.advance()
			if err := p.parseExpr(0); err != nil {
				return err
			}
			if p.cur.Kind != ']' {
				return fmt.Errorf("wire: expected ']'")
			}
			p.advance()
			p.em.WrapSubscript(mark)
		default: // binary operator
			op, ok := binOpString(k)
			if !ok {
				return ErrUnsupported
			}
			p.advance()
			if err := p.parseExpr(bp); err != nil {
				return err
			}
			p.em.WrapBinOp(mark, op)
		}
	}
	return p.err
}

func (p *exprParser) parsePrefix() error {
	switch k := p.cur.Kind; k {
	case parser.INTEGER_LITERAL:
		p.em.Int(p.cur.Int)
		p.advance()
	case parser.REAL_LITERAL:
		p.em.Real(p.cur.Real)
		p.advance()
	case parser.STRING_LITERAL:
		p.em.Str(p.cur.Str)
		p.advance()
	case parser.BOOLEAN_LITERAL:
		p.em.Bool(p.cur.Bool)
		p.advance()
	case parser.UNDEFINED:
		p.em.Undef()
		p.advance()
	case parser.ERROR:
		p.em.Err()
		p.advance()
	case parser.IDENTIFIER:
		name := p.cur.Str
		p.advance()
		if p.cur.Kind == '(' {
			return p.parseCall(name)
		}
		attr, scope := parser.ParseScopedIdentifier(name)
		p.em.AttrRef(byte(scope), attr)
	case '(':
		mark := p.em.Mark()
		p.advance()
		if err := p.parseExpr(0); err != nil {
			return err
		}
		if p.cur.Kind != ')' {
			return fmt.Errorf("wire: expected ')'")
		}
		p.advance()
		p.em.WrapParen(mark)
	case '{':
		return p.parseList()
	case '[':
		return p.parseRecord()
	case '-', '+', '!', '~':
		p.advance()
		p.em.UnOp(string(rune(k)))
		return p.parseExpr(bpUnary)
	default:
		return ErrUnsupported
	}
	return p.err
}

func (p *exprParser) parseCall(name string) error {
	id := p.em.FuncNameID(name) // intern the name before the args (reference pre-order)
	mark := p.em.Mark()         // before args
	p.advance()                 // consume '('
	argc := 0
	if p.cur.Kind != ')' {
		for {
			if err := p.parseExpr(0); err != nil {
				return err
			}
			argc++
			if p.cur.Kind != ',' {
				break
			}
			p.advance()
		}
	}
	if p.cur.Kind != ')' {
		return fmt.Errorf("wire: expected ')' in call")
	}
	p.advance()
	p.em.WrapFuncID(mark, name, id, argc)
	return p.err
}

func (p *exprParser) parseList() error {
	mark := p.em.Mark() // before elements
	p.advance()         // consume '{'
	count := 0
	if p.cur.Kind != '}' {
		for {
			if err := p.parseExpr(0); err != nil {
				return err
			}
			count++
			if p.cur.Kind != ',' {
				break
			}
			p.advance()
		}
	}
	if p.cur.Kind != '}' {
		return fmt.Errorf("wire: expected '}' in list")
	}
	p.advance()
	p.em.WrapList(mark, count)
	return p.err
}

func (p *exprParser) parseRecord() error {
	mark := p.em.Mark() // before the (key, node) pairs
	p.advance()         // consume '['
	attrCount := 0
	for p.cur.Kind != ']' {
		if p.cur.Kind != parser.IDENTIFIER {
			return ErrUnsupported // quoted/computed key: defer to the reference parser
		}
		key := p.cur.Str
		p.advance()
		if p.cur.Kind != '=' {
			return fmt.Errorf("wire: expected '=' in record")
		}
		p.advance()
		p.em.RecordKey(key) // intern the key before its value (reference pre-order)
		if err := p.parseExpr(0); err != nil {
			return err
		}
		attrCount++
		if p.cur.Kind != ';' {
			break
		}
		p.advance() // ';' separator (also tolerates a trailing ';')
	}
	if p.cur.Kind != ']' {
		return fmt.Errorf("wire: expected ']' in record")
	}
	p.advance()
	p.em.WrapRecord(mark, attrCount)
	return p.err
}
