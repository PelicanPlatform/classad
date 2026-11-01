package wire

import (
	"errors"
	"fmt"
	"math"

	"github.com/PelicanPlatform/classad/ast"
)

// MaxDepth is how deeply a stored value may nest, and the ONE bound every reader and the write
// side share. Readers stop past it so a crafted deeply nested input cannot overflow the goroutine
// stack; the write side refuses past it so nothing is stored that a reader would then refuse.
//
// Depth is counted the same way by every reader: an attribute's value is at depth 0, whether the
// reader starts from the whole ad (Decode and its variants) or from the value's node (DecodeNode,
// the renderers, the reference walkers, the transcoders); each operand, element, argument,
// condition branch, selected record or subscript part is one deeper than its parent; a record
// literal's body, and with it each member's value, is one deeper than the record. A sealed
// (encrypted) value is transparent: its plaintext is read at the sealed node's depth, so whether an
// attribute is encrypted at rest never changes whether it fits. A value fits when no part of it is
// deeper than MaxDepth.
//
// A chain of one binary operator, `a || b || c || ...`, parses left-nested: N terms reach depth
// N-1. So the longest chain an attribute can hold, with no parentheses at all, is 1001 terms.
const MaxDepth = 1000

// ErrTooDeep reports a value that nests deeper than MaxDepth, refused when it is written because
// no reader could read it back. It is permanent: the same value is refused every time, so a
// caller must drop or change it rather than retry. Match it with errors.Is; the error carrying it
// is a *DepthError naming the attribute.
var ErrTooDeep = errors.New("wire: value nests too deeply to store")

// DepthError is the refusal of an attribute whose value nests deeper than MaxDepth. It unwraps to
// ErrTooDeep.
type DepthError struct {
	Attr  string // the top-level attribute holding the value
	Depth int    // the value's depth, counted as MaxDepth counts it
}

func (e *DepthError) Error() string {
	return fmt.Sprintf("wire: attribute %q nests to depth %d; the limit is %d", e.Attr, e.Depth, MaxDepth)
}

func (e *DepthError) Unwrap() error { return ErrTooDeep }

// CheckDepth reports the first attribute of ad whose value is deeper than MaxDepth, as a
// *DepthError, or nil when every attribute fits. A nil ad fits.
//
// It reads the ad's syntax tree, not its encoding, so a write path can refuse BEFORE it touches
// anything: an encoder has no error to return, and several write paths change state (the delta
// tracker, the shard) before or while they encode.
func CheckDepth(ad *ast.ClassAd) error {
	if ad == nil {
		return nil
	}
	for _, a := range ad.Attributes {
		if err := CheckExprDepth(a.Name, a.Value); err != nil {
			return err
		}
	}
	return nil
}

// CheckExprDepth is CheckDepth for one attribute's value.
func CheckExprDepth(name string, v ast.Expr) error {
	if d := ExprDepth(v); d > MaxDepth {
		return &DepthError{Attr: name, Depth: d}
	}
	return nil
}

// ExprDepth is the depth of v stored as an attribute's value: the deepest depth a reader reaches
// reading it, counted as MaxDepth counts it (a literal is 0). It follows the encoder rather than the
// syntax tree where the two differ -- a minus folded into the literal it negates is stored as that
// literal, depth 0 -- so it is exactly the depth the readers will meet.
func ExprDepth(v ast.Expr) int {
	switch v := v.(type) {
	case *ast.BinaryOp:
		return 1 + max(ExprDepth(v.Left), ExprDepth(v.Right))
	case *ast.UnaryOp:
		if foldsToLiteral(v) {
			return 0
		}
		return 1 + ExprDepth(v.Expr)
	case *ast.ParenExpr:
		return 1 + ExprDepth(v.Inner)
	case *ast.ListLiteral:
		d := 0
		for _, el := range v.Elements {
			d = max(d, 1+ExprDepth(el))
		}
		return d
	case *ast.FunctionCall:
		d := 0
		for _, a := range v.Args {
			d = max(d, 1+ExprDepth(a))
		}
		return d
	case *ast.RecordLiteral:
		// The body is one deeper than the record, and the readers bound it even when it is empty;
		// each member's value sits at the body's depth.
		d := 0
		if v.ClassAd != nil {
			for _, a := range v.ClassAd.Attributes {
				d = max(d, ExprDepth(a.Value))
			}
		}
		return 1 + d
	case *ast.ConditionalExpr:
		return 1 + max(ExprDepth(v.Condition), ExprDepth(v.TrueExpr), ExprDepth(v.FalseExpr))
	case *ast.ElvisExpr:
		return 1 + max(ExprDepth(v.Left), ExprDepth(v.Right))
	case *ast.SelectExpr:
		return 1 + ExprDepth(v.Record)
	case *ast.SubscriptExpr:
		return 1 + max(ExprDepth(v.Container), ExprDepth(v.Index))
	}
	// A literal, an attribute reference, nil (stored as undefined), or a type the encoder stores as
	// undefined: one node with no children.
	return 0
}

// foldsToLiteral reports whether the encoder stores u as the single literal it denotes rather than
// as a minus over an operand: the syntax-tree form of foldNegatedLiteral's rule. The operand as
// emitted is a foldable literal exactly when the syntax-tree operand is one, because a fold never
// produces a literal the rule folds again (see foldNegatedLiteral).
func foldsToLiteral(u *ast.UnaryOp) bool {
	if u.Op != "-" {
		return false
	}
	switch x := u.Expr.(type) {
	case *ast.IntegerLiteral:
		return x.Value > 0
	case *ast.RealLiteral:
		return !math.Signbit(x.Value)
	}
	return false
}

// nodeFits reports whether node, one attribute value's encoding, reads back within MaxDepth. It
// walks the bytes with the readers' own skip, so it agrees with them by construction. For an
// encoder that writes bytes with no syntax tree to measure.
func nodeFits(node []byte, inline bool) bool {
	c := &cursor{b: node, ok: true, inline: inline}
	skipNode(c, 0)
	return c.ok && c.pos == len(node)
}
