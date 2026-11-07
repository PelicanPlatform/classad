package classad

import (
	"fmt"
	"math"
	"strings"
	"testing"

	"github.com/PelicanPlatform/classad/ast"
	"github.com/PelicanPlatform/classad/fuzz/gen"
	"github.com/PelicanPlatform/classad/parser"
)

// Differential testing of the predicate fast path (predicate.go) against the
// ordinary evaluator.
//
// The fast path is a second implementation of a subset of the comparison semantics,
// and a second implementation is only worth having if it is provably the same as the
// first on every input it accepts. Hand-written cases prove that for the inputs
// someone thought of; what is needed is the property, over generated inputs, that
// fastpath(expr, ad) == slowpath(expr, ad) -- including on the inputs nobody thought
// of, which for this evaluator are the ones that decide whether undefined or error
// wins, whether a boolean compares as an integer, and whether a string comparison
// folds case.
//
// There are two levels here because they fail in different ways.
//
//   - TestPredicateOperandDifferential drives the fast path's own pieces against the
//     Value path for the same operand expressions. It is where the SEMANTICS are
//     pinned: operands come from generated ads (so their values span the edge cases
//     fuzz/gen already knows to produce -- MaxInt64, MinInt64, reals, numeric-looking
//     strings, undefined, error, lists, function results) crossed with a literal table
//     that adds the ones only a comparison cares about: NaN, both infinities, -0.0,
//     2^53 and 2^53+1, and strings that differ only in case.
//
//   - TestPredicateEndToEndDifferential evaluates whole expressions through the real
//     entry point, twice: once as written, and once "armored" so the fast path cannot
//     fire. It is where the INTEGRATION is pinned -- that hooking the fast path into
//     evaluateBinaryOp did not disturb short-circuiting, the conditional, unary not,
//     or error and undefined propagation through a nest of them.
//
// Exact equality, not canon.Equal. fuzz/canon exists to compare two DIFFERENT engines
// and therefore compares reals with a tolerance, which is right for libclassad's
// transcendental functions and wrong here: both sides of this comparison are the same
// Go code, so anything but bit-exact agreement on a real is a bug, and NaN and -0.0
// are exactly the values a tolerance would hide. valuesIdentical below compares the
// payload bits. fuzz/gen is reused as is -- the generator is the part worth sharing,
// and it already encodes which inputs are worth generating and which (the regex
// family, non-finite string conversions) are not.

// valuesIdentical reports whether two Values are the same value in every respect a
// caller could observe: same type, same payload, same bits for a real. It is
// deliberately stricter than ==: NaN != NaN as a float, but two NaN results from the
// two paths ARE the same answer and must compare equal here, so reals are compared by
// their bit pattern, which also distinguishes -0.0 from 0.0.
func valuesIdentical(a, b Value) bool {
	if a.Type() != b.Type() {
		return false
	}
	switch a.Type() {
	case UndefinedValue, ErrorValue:
		return true
	case BooleanValue, IntegerValue:
		return a.intVal == b.intVal
	case RealValue:
		return math.Float64bits(uint64bitsOf(a)) == math.Float64bits(uint64bitsOf(b))
	case StringValue:
		return a.strVal == b.strVal
	case ListValue:
		av, aerr := a.ListValue()
		bv, berr := b.ListValue()
		if (aerr == nil) != (berr == nil) || len(av) != len(bv) {
			return false
		}
		for i := range av {
			if !valuesIdentical(av[i], bv[i]) {
				return false
			}
		}
		return true
	default: // ClassAdValue
		return a.String() == b.String()
	}
}

// uint64bitsOf returns a real Value's float64. Named rather than inlined so the bit
// comparison above reads as "same bits", which is the property that matters.
func uint64bitsOf(v Value) float64 { return v.real() }

func describeValue(v Value) string {
	switch v.Type() {
	case UndefinedValue:
		return "undefined"
	case ErrorValue:
		return "error"
	case RealValue:
		return fmt.Sprintf("real(%x)", math.Float64bits(v.real()))
	default:
		return fmt.Sprintf("%v(%s)", v.Type(), v.String())
	}
}

// predicateOps are the operators the fast path claims, in their source spelling.
var predicateOps = []string{"<", "<=", ">", ">=", "==", "!=", "=?=", "=!="}

// literalOperands are literal AST nodes chosen for what they expose about the
// comparison rules rather than for variety: the two integers that collide when
// promoted to double, the signed zeroes and non-finite reals whose ordering and
// equality are IEEE rules rather than obvious ones, strings differing only in case
// (== folds, =?= does not), and the two non-values whose precedence over each other
// is the rule most easily got backwards.
func literalOperands() []ast.Expr {
	return []ast.Expr{
		&ast.IntegerLiteral{Value: 0},
		&ast.IntegerLiteral{Value: 1},
		&ast.IntegerLiteral{Value: -1},
		&ast.IntegerLiteral{Value: 1 << 53},
		&ast.IntegerLiteral{Value: (1 << 53) + 1},
		&ast.IntegerLiteral{Value: math.MaxInt64},
		&ast.IntegerLiteral{Value: math.MinInt64},
		&ast.RealLiteral{Value: 0},
		&ast.RealLiteral{Value: math.Copysign(0, -1)},
		&ast.RealLiteral{Value: 1},
		&ast.RealLiteral{Value: 1.5},
		&ast.RealLiteral{Value: math.NaN()},
		&ast.RealLiteral{Value: math.Inf(1)},
		&ast.RealLiteral{Value: math.Inf(-1)},
		&ast.RealLiteral{Value: float64(1 << 53)},
		&ast.StringLiteral{Value: ""},
		&ast.StringLiteral{Value: "alice"},
		&ast.StringLiteral{Value: "ALICE"},
		&ast.StringLiteral{Value: "AlIcE"},
		&ast.StringLiteral{Value: "alice2"},
		&ast.StringLiteral{Value: "0"},
		&ast.BooleanLiteral{Value: true},
		&ast.BooleanLiteral{Value: false},
		&ast.UndefinedLiteral{},
		&ast.ErrorLiteral{},
	}
}

// generatedAds builds n ads from fuzz/gen, wired into the scope shapes the fast path
// has to resolve through: each ad gets a PARENT and a TARGET drawn from the same
// population, so MY, TARGET, PARENT, absolute and unqualified references (including
// the old-ClassAd fallthrough from MY to TARGET) all resolve against something real.
func generatedAds(t *testing.T, n int) []*ClassAd {
	t.Helper()
	g := gen.New(20260101, gen.DefaultConfig())
	ads := make([]*ClassAd, 0, n)
	for len(ads) < n {
		src := g.ClassAd()
		ad, err := Parse(src)
		if err != nil {
			// The generator is grammar-directed, so this should not happen; if it
			// ever does, the generator changed and the test should say so rather
			// than quietly sample fewer ads.
			t.Fatalf("generated ad does not parse: %v\n%s", err, src)
		}
		ads = append(ads, ad)
	}
	// Parent chains are built in groups of three and left OPEN at the end. A ring
	// of parents would hang the evaluator rather than exercise it: an unqualified
	// reference to an absent attribute walks `for ad := e.classad; ad != nil; ad =
	// ad.parent` and a ring never reaches nil. That is a property of SetParent, not
	// of anything this test is about, and a test that wires one up measures nothing
	// because it never returns.
	//
	// Targets may form a ring safely: the target chain is followed one hop at a time
	// through evalAttrExpr, which is bounded by the cyclic-reference guard.
	for i, ad := range ads {
		if i%3 != 2 && i+1 < len(ads) {
			ad.SetParent(ads[i+1])
		}
		ad.SetTarget(ads[(i+2)%len(ads)])
	}
	return ads
}

// cyclicAds are ads whose attributes reference each other, directly and through a
// chain, plus ones mixing a cycle with a literal. The fast path must never answer a
// comparison that touches one -- a cycle necessarily runs through a non-literal
// expression, so every such reference declines -- and these make that a tested fact
// rather than an argument.
func cyclicAds(t *testing.T) []*ClassAd {
	t.Helper()
	srcs := []string{
		`[ a0 = a0; a1 = 1; a2 = "s" ]`,
		`[ a0 = a1; a1 = a0; a2 = 3 ]`,
		`[ a0 = a1 + 1; a1 = a2 + 1; a2 = a0 + 1 ]`,
		`[ a0 = TARGET.a0; a1 = MY.a1; a2 = .a2 ]`,
		`[ a0 = (5); a1 = (undefined); a2 = ( "x" ) ]`,
	}
	ads := make([]*ClassAd, 0, len(srcs))
	for _, s := range srcs {
		ad, err := Parse(s)
		if err != nil {
			t.Fatalf("parse %s: %v", s, err)
		}
		ads = append(ads, ad)
	}
	for i, ad := range ads {
		ad.SetTarget(ads[(i+1)%len(ads)])
	}
	return ads
}

// refOperands are attribute references in every scope the resolver distinguishes,
// naming attributes that are present-and-literal, present-and-computed, and absent.
func refOperands() []ast.Expr {
	names := []string{"a0", "a1", "a2", "result", "x0", "CurrentTime"}
	scopes := []ast.AttributeScope{ast.NoScope, ast.MyScope, ast.TargetScope, ast.ParentScope, ast.RootScope}
	var out []ast.Expr
	for _, n := range names {
		for _, s := range scopes {
			out = append(out, ast.NewAttributeReference(n, s))
		}
	}
	return out
}

// evalSafe evaluates expr, converting the cyclic-reference panic into the error value
// the top-level entry points produce, so a divergence is reported rather than
// crashing the test binary.
func evalSafe(e *Evaluator, expr ast.Expr) (result Value) {
	defer recoverCyclic(&result)
	return e.Evaluate(expr)
}

// The overlay dimension.
//
// Every generated input below is run under each of these configurations of the
// evaluator's two resolver hooks, applied identically to both arms. Without this
// dimension the differential covers only evaluators with nothing installed, and the
// fast path's resolution of a reference is a SECOND implementation of
// resolveAttributeReference -- so anything that path learns and this one does not is
// a wrong answer that nothing here would notice. That is not hypothetical: the fast
// path shipped reading the ad underneath an installed partial resolver, and what
// caught it was a downstream module's own tests rather than anything in this file.
//
// The two hooks are not variations on one thing and fail differently:
//
//   - SetResolver replaces scope resolution outright. The fast path cannot reproduce
//     its contract and declines whenever one is installed, so these arms assert the
//     decline holds over the whole input space rather than on one hand-written case.
//   - SetPartialResolver OVERLAYS it and may decline per name, which is the harder
//     shape: a name it answers has a Value already, and a name it declines must
//     resolve exactly as it would with nothing installed. Both halves are wrong
//     silently -- an overlay the fast path skips reads the ad's value, and a
//     comparison against it comes out the other way with nothing raised.
type resolverOverlay struct {
	name    string
	install func(*Evaluator)
}

// overlayValue is the value an overlay answers a name with: deterministic in the
// name, and drawn from the same semantic edge cases as the literal table so that an
// overlaid operand can be undefined, error, a NaN, or a string that differs from the
// ad's only in case.
func overlayValue(name string) Value {
	table := []Value{
		NewIntValue(1),
		NewIntValue((1 << 53) + 1),
		NewRealValue(math.NaN()),
		NewRealValue(1.5),
		NewStringValue("ALICE"),
		NewStringValue("overlaid"),
		NewBoolValue(true),
		NewUndefinedValue(),
		NewErrorValue(),
	}
	// FNV-1a over the folded name: names match case-insensitively, so two spellings
	// of one attribute must be answered with one value or the two arms would differ
	// for a reason that is the test's fault.
	var h uint32 = 2166136261
	for _, c := range strings.ToLower(name) {
		h = (h ^ uint32(c)) * 16777619
	}
	return table[h%uint32(len(table))]
}

// overlayAnswers decides which names an overlay claims, by the same hash, so that a
// single run reaches both the answered and the declined branch rather than needing
// one configuration for each.
func overlayAnswers(name string) bool {
	var h uint32 = 2166136261
	for _, c := range strings.ToLower(name) {
		h = (h ^ uint32(c)) * 16777619
	}
	return h%2 == 0
}

const (
	overlayNone       = "no overlay"
	overlayDeclines   = "partial declines everything"
	overlayAnswersAll = "partial answers everything"
)

func resolverOverlays() []resolverOverlay {
	return []resolverOverlay{
		{name: overlayNone, install: func(*Evaluator) {}},
		{
			// The configuration that pins the fall-through: an overlay is installed
			// and answers nothing, so every reference must resolve exactly as it
			// does with no overlay at all -- same answers AND the same fast-path
			// share, which the caller asserts.
			name: overlayDeclines,
			install: func(e *Evaluator) {
				e.SetPartialResolver(func(string, ast.AttributeScope) (Value, bool) {
					return NewUndefinedValue(), false
				})
			},
		},
		{
			name: overlayAnswersAll,
			install: func(e *Evaluator) {
				e.SetPartialResolver(func(name string, _ ast.AttributeScope) (Value, bool) {
					return overlayValue(name), true
				})
			},
		},
		{
			// The realistic shape: some names bound, the rest left to the ad, both
			// reached within a single evaluation.
			name: "partial answers some",
			install: func(e *Evaluator) {
				e.SetPartialResolver(func(name string, _ ast.AttributeScope) (Value, bool) {
					if !overlayAnswers(name) {
						return NewUndefinedValue(), false
					}
					return overlayValue(name), true
				})
			},
		},
		{
			// Scope-sensitive: the overlay claims MY.x and not x, so an arm that
			// passed the wrong scope (or dropped it) would answer the wrong half of
			// the references and diverge.
			name: "partial answers MY only",
			install: func(e *Evaluator) {
				e.SetPartialResolver(func(name string, scope ast.AttributeScope) (Value, bool) {
					if scope != ast.MyScope {
						return NewUndefinedValue(), false
					}
					return overlayValue(name), true
				})
			},
		},
		{
			name: "full resolver",
			install: func(e *Evaluator) {
				e.SetResolver(func(name string, _ ast.AttributeScope) Value {
					return overlayValue(name)
				})
			},
		},
		{
			// Both installed, which is the composition SetPartialResolver documents:
			// a declined name falls to the full resolver and never to the ad. An
			// implementation that fell to the ad instead would pass every arm above.
			name: "partial over full resolver",
			install: func(e *Evaluator) {
				e.SetPartialResolver(func(name string, _ ast.AttributeScope) (Value, bool) {
					if !overlayAnswers(name) {
						return NewUndefinedValue(), false
					}
					return overlayValue(name), true
				})
				e.SetResolver(func(name string, _ ast.AttributeScope) Value {
					return NewStringValue("from-resolver-" + strings.ToLower(name))
				})
			},
		},
	}
}

// evaluatorFor builds an evaluator over ad with ov installed. Both arms of every
// differential go through it, so neither can be configured differently from the
// other by accident.
func evaluatorFor(ad *ClassAd, ov resolverOverlay) *Evaluator {
	e := NewEvaluator(ad)
	ov.install(e)
	return e
}

// TestPredicateOperandDifferential asserts that every comparison the fast path
// accepts produces exactly what the ordinary Value path produces for the same
// operands, over generated ads crossed with the literal table, in every scope, under
// every overlay configuration.
//
// It reports the share of attempts that took the fast path. That number is the point
// of the exercise and also the test's own health check: a decline rate of 100% would
// make everything below pass while proving nothing, so the test fails if the fast
// path turns out never to fire or never to decline.
//
// The shares across overlays are themselves asserted, because agreement alone cannot
// tell a fast path that consults an overlay from one that stands down whenever a
// hook is installed -- both answer correctly. See the checks after the loop.
func TestPredicateOperandDifferential(t *testing.T) {
	ads := append(generatedAds(t, 60), cyclicAds(t)...)
	operands := append(refOperands(), literalOperands()...)

	shares := make(map[string]float64)
	for _, ov := range resolverOverlays() {
		t.Run(ov.name, func(t *testing.T) {
			attempts, fast := operandDifferential(t, ads, operands, ov)
			if fast == 0 {
				t.Fatal("the fast path never fired: this subtest proved nothing")
			}
			if fast == attempts {
				t.Fatal("the fast path never declined: the decline cases are untested")
			}
			shares[ov.name] = float64(fast) / float64(attempts)
			t.Logf("operand differential: %d comparisons attempted, %d took the fast path (%.1f%%), %d declined (%.1f%%)",
				attempts, fast, 100*shares[ov.name],
				attempts-fast, 100*float64(attempts-fast)/float64(attempts))
		})
	}
	if t.Failed() {
		return
	}

	// An overlay that answers NOTHING must leave the fast path exactly where it was.
	// This is the assertion that a coarse "stand down whenever a partial resolver is
	// installed" guard fails: it is correct, so every divergence check above passes
	// under it, and only the share shows that the overlay cost the fast path every
	// reference it was supposed to cost it none of.
	if shares[overlayDeclines] != shares[overlayNone] {
		t.Errorf("an overlay that answers nothing changed the fast-path share: %.1f%% with it, %.1f%% without; a declined name must resolve as it does with no overlay installed",
			100*shares[overlayDeclines], 100*shares[overlayNone])
	}
	// An overlay that answers EVERYTHING must cost the fast path every reference:
	// the overlay hands back a Value, which is the ordinary path by definition. If
	// this share did not drop, the fast path is reading the ad instead of asking.
	if shares[overlayAnswersAll] >= shares[overlayNone] {
		t.Errorf("an overlay that answers everything did not reduce the fast-path share (%.1f%% vs %.1f%%): references are being answered from the ad rather than from the overlay",
			100*shares[overlayAnswersAll], 100*shares[overlayNone])
	}
}

// operandDifferential is the body of the operand differential for one overlay: it
// returns how many comparisons it attempted and how many the fast path decided, and
// fails on the first divergence.
func operandDifferential(t *testing.T, ads []*ClassAd, operands []ast.Expr, ov resolverOverlay) (attempts, fast int) {
	t.Helper()
	for _, ad := range ads {
		for _, opStr := range predicateOps {
			// The parser normalizes =?= / =!= to "is"/"isnt"; comparisonKind and
			// applyBinaryValues both speak the normalized spelling, so normalize
			// once here the way the parser would.
			normOp := opStr
			switch opStr {
			case "=?=":
				normOp = "is"
			case "=!=":
				normOp = "isnt"
			}
			c := comparisonKind(normOp)
			if c == cmpNone {
				t.Fatalf("comparisonKind(%q) = cmpNone; the operator table and the test disagree", normOp)
			}
			for _, lhs := range operands {
				for _, rhs := range operands {
					attempts++
					got, ok := fastCompare(evaluatorFor(ad, ov), c, lhs, rhs)
					if !ok {
						continue
					}
					fast++

					slowEv := evaluatorFor(ad, ov)
					lv := evalSafe(slowEv, lhs)
					rv := evalSafe(slowEv, rhs)
					want := slowEv.applyBinaryValues(normOp, lv, rv)
					if !valuesIdentical(got, want) {
						t.Fatalf("predicate divergence\n  overlay: %s\n  ad:    %s\n  expr:  %s %s %s\n  fast:  %s\n  slow:  %s\n  left:  %s\n  right: %s",
							ov.name, ad.String(), lhs.String(), opStr, rhs.String(),
							describeValue(got), describeValue(want),
							describeValue(lv), describeValue(rv))
					}
				}
			}
		}
	}
	return attempts, fast
}

// armor rewrites an expression so the predicate fast path cannot fire anywhere in it,
// by wrapping both operands of every comparison in parentheses.
//
// A ParenExpr is transparent to evaluation -- evalNode evaluates its Inner and
// nothing else -- but operandLiteral declines it, exactly as it declines any node
// that is neither a literal nor an attribute reference. So the armored expression is
// the SAME expression evaluated entirely by the ordinary path, which is what makes it
// usable as the control arm. assertArmored below checks the suppression structurally,
// so the control cannot silently stop being a control.
//
// It recurses into EVERY sub-expression, function arguments and list elements
// included. An earlier version stopped at those two, and it quietly broke the
// measurement in ratio_metrics_test.go rather than this test: a comparison inside a
// function argument then took the fast path in BOTH arms, so the control arm counted
// work it was supposed to be the baseline for. The one thing the wrapping is visible
// to is unparsing -- a lazy list value and unparse() carry their SOURCE expressions,
// so `string({A == 1})` reads back with the added parentheses. No expression this
// test or that measurement builds unparses a comparison, which is why a total armor
// is safe here; an expression that did would have to be excluded rather than armored.
func armor(e ast.Expr) ast.Expr {
	switch v := e.(type) {
	case *ast.ParenExpr:
		return &ast.ParenExpr{Inner: armor(v.Inner)}
	case *ast.BinaryOp:
		l, r := armor(v.Left), armor(v.Right)
		if comparisonKind(v.Op) != cmpNone {
			l = &ast.ParenExpr{Inner: l}
			r = &ast.ParenExpr{Inner: r}
		}
		return &ast.BinaryOp{Op: v.Op, Left: l, Right: r}
	case *ast.UnaryOp:
		return &ast.UnaryOp{Op: v.Op, Expr: armor(v.Expr)}
	case *ast.ConditionalExpr:
		return &ast.ConditionalExpr{
			Condition: armor(v.Condition),
			TrueExpr:  armor(v.TrueExpr),
			FalseExpr: armor(v.FalseExpr),
		}
	case *ast.ElvisExpr:
		return &ast.ElvisExpr{Left: armor(v.Left), Right: armor(v.Right)}
	case *ast.FunctionCall:
		args := make([]ast.Expr, len(v.Args))
		for i, a := range v.Args {
			args[i] = armor(a)
		}
		return &ast.FunctionCall{Name: v.Name, Args: args}
	case *ast.ListLiteral:
		if v.Elements == nil {
			return e
		}
		elems := make([]ast.Expr, len(v.Elements))
		for i, el := range v.Elements {
			elems[i] = armor(el)
		}
		return &ast.ListLiteral{Elements: elems}
	case *ast.SelectExpr:
		return &ast.SelectExpr{Record: armor(v.Record), Attr: v.Attr}
	case *ast.SubscriptExpr:
		return &ast.SubscriptExpr{Container: armor(v.Container), Index: armor(v.Index)}
	case *ast.RecordLiteral:
		if v.ClassAd == nil {
			return e
		}
		attrs := make([]*ast.AttributeAssignment, len(v.ClassAd.Attributes))
		for i, a := range v.ClassAd.Attributes {
			attrs[i] = &ast.AttributeAssignment{Name: a.Name, Value: armor(a.Value)}
		}
		return &ast.RecordLiteral{ClassAd: &ast.ClassAd{Attributes: attrs}}
	default:
		return e
	}
}

// assertArmored walks an armored expression and fails unless every comparison in it
// has both operands wrapped, which is the condition under which operandLiteral
// provably declines: operandLiteral accepts only a literal node or an attribute
// reference, and a ParenExpr is neither.
func assertArmored(t *testing.T, e ast.Expr) {
	t.Helper()
	switch v := e.(type) {
	case *ast.ParenExpr:
		assertArmored(t, v.Inner)
	case *ast.BinaryOp:
		if comparisonKind(v.Op) != cmpNone {
			if _, ok := v.Left.(*ast.ParenExpr); !ok {
				t.Fatalf("armor left an unwrapped left operand in %s", e.String())
			}
			if _, ok := v.Right.(*ast.ParenExpr); !ok {
				t.Fatalf("armor left an unwrapped right operand in %s", e.String())
			}
		}
		assertArmored(t, v.Left)
		assertArmored(t, v.Right)
	case *ast.UnaryOp:
		assertArmored(t, v.Expr)
	case *ast.ConditionalExpr:
		assertArmored(t, v.Condition)
		assertArmored(t, v.TrueExpr)
		assertArmored(t, v.FalseExpr)
	case *ast.ElvisExpr:
		assertArmored(t, v.Left)
		assertArmored(t, v.Right)
	case *ast.FunctionCall:
		for _, a := range v.Args {
			assertArmored(t, a)
		}
	case *ast.ListLiteral:
		for _, el := range v.Elements {
			assertArmored(t, el)
		}
	case *ast.SelectExpr:
		assertArmored(t, v.Record)
	case *ast.SubscriptExpr:
		assertArmored(t, v.Container)
		assertArmored(t, v.Index)
	case *ast.RecordLiteral:
		if v.ClassAd != nil {
			for _, a := range v.ClassAd.Attributes {
				assertArmored(t, a.Value)
			}
		}
	}
}

// predicateExprs generates whole expressions in the grammar the fast path lives in:
// comparisons over attribute references and literals, composed with && , || , ! , the
// conditional and the Elvis operator, and with non-literal operands (arithmetic, a
// function call, a list, a nested comparison) mixed in so declines are reached inside
// compositions rather than only at the top.
//
// The ad population comes from fuzz/gen; the composition grammar is written here
// because the general generator also produces string() and lazy lists, whose values
// carry their SOURCE expressions, and an expression that unparses itself cannot be
// compared against a parenthesized rewrite of itself. Narrowing the grammar to the
// predicate domain is what keeps the control arm honest instead of needing a skip.
func predicateExprs(t *testing.T) []ast.Expr {
	t.Helper()
	refs := []string{"a0", "a1", "a2", "result", "x0", "MY.a0", "TARGET.a0", "TARGET.a1",
		"PARENT.a0", ".a0", "CurrentTime", "TARGET.CurrentTime"}
	lits := []string{"0", "1", "-1", `"alice"`, `"ALICE"`, "true", "false", "undefined",
		"error", "1.5", "0.0", "9007199254740993"}
	computed := []string{"a0 + 1", "a1 * 2", "size({1,2})", "(a0)", "strcat(a0, \"x\")",
		"{1, 2}", "a0 == a1", "-a0"}

	var leaves []string
	leaves = append(leaves, refs...)
	leaves = append(leaves, lits...)
	leaves = append(leaves, computed...)

	var cmps []string
	for i, l := range leaves {
		for j, op := range predicateOps {
			r := leaves[(i*len(predicateOps)+j*7+3)%len(leaves)]
			cmps = append(cmps, "("+l+" "+op+" "+r+")")
		}
	}

	var srcs []string
	srcs = append(srcs, cmps...)
	for i := range cmps {
		a := cmps[i]
		b := cmps[(i*5+1)%len(cmps)]
		c := cmps[(i*11+7)%len(cmps)]
		srcs = append(srcs,
			"("+a+" && "+b+")",
			"("+a+" || "+b+")",
			"(!"+a+")",
			"("+a+" ? "+b+" : "+c+")",
			"("+a+" ?: "+b+")",
			"(("+a+" && "+b+") || !"+c+")",
			"("+a+" && ("+b+" || "+c+"))",
		)
	}

	out := make([]ast.Expr, 0, len(srcs))
	for _, s := range srcs {
		e, err := parser.ParseExpr(s)
		if err != nil {
			t.Fatalf("predicate source does not parse: %q: %v", s, err)
		}
		out = append(out, e)
	}
	return out
}

// TestPredicateEndToEndDifferential evaluates each generated predicate expression
// through the real entry point against each generated ad, twice: as written (the fast
// path may fire anywhere inside it) and armored (it cannot fire at all). The two must
// produce identical values.
//
// This is what covers the composition the fast path does not implement itself:
// short-circuiting past a comparison that would have errored, an undefined conjunct
// poisoning a conjunction but not a disjunction, a conditional whose condition is a
// fast-path comparison, and unary not over one.
//
// Each overlay configuration is applied to both arms, so a reference overlaid in the
// middle of a conjunction has to compose the same way whether or not the comparison
// around it took the fast path.
func TestPredicateEndToEndDifferential(t *testing.T) {
	ads := append(generatedAds(t, 25), cyclicAds(t)...)
	exprs := predicateExprs(t)
	// Armor once, not per evaluation: the rewrite allocates a parallel AST, and
	// rebuilding it inside the loop would make the control arm's cost swamp the
	// evaluation being compared.
	armored := make([]ast.Expr, len(exprs))
	for i, e := range exprs {
		armored[i] = armor(e)
		assertArmored(t, armored[i])
	}

	for _, ov := range resolverOverlays() {
		t.Run(ov.name, func(t *testing.T) {
			var checked int
			for _, ad := range ads {
				for i, e := range exprs {
					got := evalSafe(evaluatorFor(ad, ov), e)
					want := evalSafe(evaluatorFor(ad, ov), armored[i])
					if !valuesIdentical(got, want) {
						t.Fatalf("end-to-end divergence\n  overlay: %s\n  ad:      %s\n  expr:    %s\n  armored: %s\n  fast:    %s\n  slow:    %s",
							ov.name, ad.String(), e.String(), armored[i].String(),
							describeValue(got), describeValue(want))
					}
					checked++
				}
			}
			t.Logf("end-to-end differential: %d (ad, expression) pairs agreed", checked)
		})
	}
}

// TestPredicateMatchmakingDifferential runs the same comparison under a MatchClassAd,
// where TARGET is supplied by an evaluation-scoped binding rather than by the ad's own
// target field. attrLiteral resolves TARGET through targetOf for exactly this reason,
// and a binding is the shape that would expose it reading ad.target directly.
func TestPredicateMatchmakingDifferential(t *testing.T) {
	job, err := Parse(`[ Owner = "alice"; RequestCpus = 4; RequestMemory = 2048;
		Requirements = TARGET.Arch == "X86_64" && TARGET.Cpus >= RequestCpus &&
			TARGET.Memory >= RequestMemory && TARGET.Owner =?= Owner ]`)
	if err != nil {
		t.Fatal(err)
	}
	machines := []string{
		`[ Arch = "X86_64"; Cpus = 8; Memory = 16384; Owner = "alice"; Requirements = true ]`,
		`[ Arch = "x86_64"; Cpus = 8; Memory = 16384; Owner = "ALICE"; Requirements = true ]`,
		`[ Arch = "AARCH64"; Cpus = 2; Memory = 1024; Requirements = true ]`,
		`[ Arch = "X86_64"; Cpus = Memory / 2048; Memory = 16384; Owner = "alice"; Requirements = true ]`,
		`[ Arch = undefined; Cpus = error; Memory = 16384.0; Owner = "alice"; Requirements = true ]`,
	}
	reqExpr, ok := job.Lookup("Requirements")
	if !ok {
		t.Fatal("job has no Requirements")
	}
	for _, src := range machines {
		machine, perr := Parse(src)
		if perr != nil {
			t.Fatal(perr)
		}
		m := NewMatchClassAd(job, machine)
		got := m.EvaluateExprLeft(reqExpr.expr)
		want := m.EvaluateExprLeft(armor(reqExpr.expr))
		if !valuesIdentical(got, want) {
			t.Fatalf("match divergence for %s: fast %s, slow %s", src, describeValue(got), describeValue(want))
		}
	}

	// EvaluateExprAgainst is the binding that is NOT written onto either ad: it
	// pairs them for one evaluation, leaving whatever target each ad already had in
	// place. That is the shape in which reading ad.target directly instead of going
	// through targetOf gives a wrong answer rather than the same one, so the ad here
	// is deliberately given a DIFFERENT standing target first -- and one whose
	// attributes would make the expression come out the other way.
	decoy, err := Parse(`[ Arch = "AARCH64"; Cpus = 1; Memory = 1; Owner = "mallory" ]`)
	if err != nil {
		t.Fatal(err)
	}
	for _, src := range machines {
		machine, perr := Parse(src)
		if perr != nil {
			t.Fatal(perr)
		}
		job.SetTarget(decoy)
		got := job.EvaluateExprAgainst(reqExpr.expr, machine)
		job.SetTarget(decoy)
		want := job.EvaluateExprAgainst(armor(reqExpr.expr), machine)
		if !valuesIdentical(got, want) {
			t.Fatalf("bound-target divergence for %s: fast %s, slow %s", src, describeValue(got), describeValue(want))
		}
	}

	// The binding must be what DECIDED those answers, not a standing target that
	// happens to agree with it. An implementation reading ad.target directly would
	// return the decoy's answer for every machine and still pass everything above,
	// so this pins that the decoy and the bound target give DIFFERENT answers --
	// without which the loop could not detect a binding being ignored at all.
	job.SetTarget(decoy)
	matching, err := Parse(machines[0])
	if err != nil {
		t.Fatal(err)
	}
	bound := job.EvaluateExprAgainst(reqExpr.expr, matching)
	job.SetTarget(decoy)
	viaDecoy := job.EvaluateExprAgainst(reqExpr.expr, decoy)
	if valuesIdentical(bound, viaDecoy) {
		t.Fatalf("the decoy target gives the same answer as the bound one (%s); this test cannot detect a binding being ignored",
			describeValue(bound))
	}
}

// TestPredicateResolverDeclines pins the decline for a custom resolver. A resolver
// replaces scope resolution entirely and speaks only in Values, so the fast path has
// nothing to read; answering from the ad underneath it would silently ignore the
// resolver, which is the kind of wrong answer that would show up as a query returning
// the wrong rows rather than as a failure.
func TestPredicateResolverDeclines(t *testing.T) {
	ad, err := Parse(`[ Owner = "alice" ]`)
	if err != nil {
		t.Fatal(err)
	}
	e := NewEvaluator(ad)
	e.SetResolver(func(name string, _ ast.AttributeScope) Value {
		if strings.EqualFold(name, "Owner") {
			return NewStringValue("bob")
		}
		return NewUndefinedValue()
	})
	expr, err := parser.ParseExpr(`Owner == "bob"`)
	if err != nil {
		t.Fatal(err)
	}
	v := e.Evaluate(expr)
	if b, err := v.BoolValue(); err != nil || !b {
		t.Fatalf("resolver ignored: %s", describeValue(v))
	}
	if _, ok := fastCompare(e, cmpEQ, ast.NewAttributeReference("Owner", ast.NoScope), &ast.StringLiteral{Value: "bob"}); ok {
		t.Fatal("the fast path answered while a resolver was installed")
	}
}

// TestPredicateCurrentTimeDeclines pins the CurrentTime decline. The magic is that
// whether the scope DEFINES CurrentTime changes the answer and not only its value:
// an ad that defines it as undefined is not the same as one that omits it, which
// reads as the clock. The fast path cannot see that distinction, so it must not try.
func TestPredicateCurrentTimeDeclines(t *testing.T) {
	for _, src := range []string{`[ a = 1 ]`, `[ CurrentTime = undefined ]`, `[ CurrentTime = 5 ]`} {
		ad, err := Parse(src)
		if err != nil {
			t.Fatal(err)
		}
		e := NewEvaluator(ad)
		for _, name := range []string{"CurrentTime", "currenttime", "CURRENTTIME"} {
			ref := ast.NewAttributeReference(name, ast.NoScope)
			if _, ok := fastCompare(e, cmpEQ, ref, &ast.IntegerLiteral{Value: 5}); ok {
				t.Fatalf("the fast path answered a %s reference in %s", name, src)
			}
		}
	}
}

// TestPredicateDepthMarginDeclines pins the depth decline. The ordinary path charges
// one unit per AST node and this one charges none, so near maxEvalDepth the two could
// disagree about whether an expression is too deep to evaluate at all; the margin
// makes the fast path stand down before they can.
func TestPredicateDepthMarginDeclines(t *testing.T) {
	ad, err := Parse(`[ a = 1 ]`)
	if err != nil {
		t.Fatal(err)
	}
	e := NewEvaluator(ad)
	ref := ast.NewAttributeReference("a", ast.NoScope)
	lit := &ast.IntegerLiteral{Value: 1}
	if _, ok := fastCompare(e, cmpEQ, ref, lit); !ok {
		t.Fatal("the fast path declined at depth 0")
	}
	e.depth = maxEvalDepth - predicateDepthMargin
	if _, ok := fastCompare(e, cmpEQ, ref, lit); ok {
		t.Fatal("the fast path answered inside the depth margin")
	}
}

// TestPredicateSemanticsTable states the rules in the file's doc comment as cases, so
// a reader can see what the fast path claims without reconstructing it from the
// generated runs: == folds case while =?= does not, a boolean coerces to an integer
// under == but is a different TYPE under =?=, error beats undefined, a number against
// a string is an error and not false, and integers near 2^53 do not collapse.
func TestPredicateSemanticsTable(t *testing.T) {
	ad, err := Parse(`[ s = "Alice"; i = 9007199254740993; r = 1.0; b = true; u = undefined; e = error ]`)
	if err != nil {
		t.Fatal(err)
	}
	cases := []struct {
		expr string
		want string
	}{
		{`s == "alice"`, "B:true"},
		{`s =?= "alice"`, "B:false"},
		{`s =?= "Alice"`, "B:true"},
		{`b == 1`, "B:true"},
		{`b =?= 1`, "B:false"},
		{`i > 9007199254740992`, "B:true"},
		{`i == 9007199254740992.0`, "B:true"}, // the real side collapses, as doComparison does
		{`s == 3`, "E"},
		{`s < 3`, "E"},
		{`u == 1`, "U"},
		{`u =?= undefined`, "B:true"},
		{`e == 1`, "E"},
		{`e =?= error`, "B:true"},
		{`u =?= error`, "B:false"},
		{`missing == 1`, "U"},
		{`missing =?= undefined`, "B:true"},
		{`r == 1`, "B:true"},
		{`r != 1`, "B:false"},
		{`u != 1`, "U"},
	}
	for _, tc := range cases {
		expr, err := parser.ParseExpr(tc.expr)
		if err != nil {
			t.Fatalf("%s: %v", tc.expr, err)
		}
		e := NewEvaluator(ad)
		got := evalSafe(e, expr)
		slow := evalSafe(NewEvaluator(ad), armor(expr))
		if !valuesIdentical(got, slow) {
			t.Fatalf("%s: fast %s, slow %s", tc.expr, describeValue(got), describeValue(slow))
		}
		var desc string
		switch got.Type() {
		case UndefinedValue:
			desc = "U"
		case ErrorValue:
			desc = "E"
		case BooleanValue:
			b, _ := got.BoolValue()
			desc = fmt.Sprintf("B:%v", b)
		default:
			desc = describeValue(got)
		}
		if desc != tc.want {
			t.Errorf("%s = %s, want %s", tc.expr, desc, tc.want)
		}
	}
}

// fastCompare asks whether the fast path would decide a comparison, and with what
// result, by composing the two production pieces evaluateComparison composes:
// operandLiteral for each side, then compareLiterals. It is a TEST composition rather
// than a second production entry point kept alive for tests -- production has exactly
// one hook, evaluateComparison, and the end-to-end tests go through it.
//
// operandLiteral evaluates an operand it cannot read as a literal, so a decline here
// has already done the ordinary work and may have raised the cyclic sentinel on the
// way. Both are reported simply as "declined", which is all a caller asks of it.
func fastCompare(e *Evaluator, c cmpKind, lhs, rhs ast.Expr) (result Value, ok bool) {
	defer func() {
		if r := recover(); r != nil {
			if _, isCyclic := r.(cyclicEvalError); isCyclic {
				result, ok = Value{}, false
				return
			}
			panic(r)
		}
	}()
	if !e.predicateEligible() {
		return Value{}, false
	}
	var l, r litOperand
	var lv, rv Value
	if !e.operandLiteral(lhs, &l, &lv) {
		return Value{}, false
	}
	if !e.operandLiteral(rhs, &r, &rv) {
		return Value{}, false
	}
	return mapState(compareLiterals(c, l, r)), true
}

// TestPredicateOverlayConsultedOnce pins the two properties of the overlay consult
// that the differential above cannot see, because both arms agree under either.
//
// First, an overlay is asked once per reference, not once per attempt. fn is the
// caller's code; nothing in SetPartialResolver's contract makes it cheap or free of
// effects, and asking twice is the shape a fast path falls into when it tests the
// overlay and then hands the reference to Evaluate to do the work again.
//
// Second, a name the overlay declines resolves through the FULL resolver when one is
// installed, never from the ad underneath it. The ad here holds a value that would
// decide the comparison the other way, so an implementation that read it answers
// false where the composition requires true.
func TestPredicateOverlayConsultedOnce(t *testing.T) {
	ad, err := Parse(`[ Owner = "from-the-ad"; Cpus = 1 ]`)
	if err != nil {
		t.Fatal(err)
	}
	var partialCalls, resolverCalls int
	e := NewEvaluator(ad)
	e.SetPartialResolver(func(string, ast.AttributeScope) (Value, bool) {
		partialCalls++
		return NewUndefinedValue(), false
	})
	e.SetResolver(func(name string, _ ast.AttributeScope) Value {
		resolverCalls++
		if strings.EqualFold(name, "Owner") {
			return NewStringValue("from-the-resolver")
		}
		return NewUndefinedValue()
	})

	expr, err := parser.ParseExpr(`Owner == "from-the-resolver"`)
	if err != nil {
		t.Fatal(err)
	}
	got := evalSafe(e, expr)
	if b, berr := got.BoolValue(); berr != nil || !b {
		t.Fatalf("a declined name did not reach the full resolver: %s", describeValue(got))
	}
	if partialCalls != 1 {
		t.Errorf("the overlay was consulted %d times for one reference, want 1", partialCalls)
	}
	if resolverCalls != 1 {
		t.Errorf("the resolver was consulted %d times for one reference, want 1", resolverCalls)
	}

	// The same expression with the fast path suppressed: the ordinary path is the
	// definition of how often each hook is asked, so this is what the count above is
	// being held to rather than a number chosen here.
	partialCalls, resolverCalls = 0, 0
	slow := evalSafe(e, armor(expr))
	if !valuesIdentical(got, slow) {
		t.Fatalf("fast %s, slow %s", describeValue(got), describeValue(slow))
	}
	if partialCalls != 1 || resolverCalls != 1 {
		t.Errorf("the ordinary path asked the overlay %d times and the resolver %d times; the fast path must match it, not a fixed number",
			partialCalls, resolverCalls)
	}
}
