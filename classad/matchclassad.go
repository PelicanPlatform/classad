// Package classad provides ClassAd matching functionality.
package classad

import (
	"github.com/PelicanPlatform/classad/ast"
)

// MatchClassAd represents a pair of ClassAds for matching.
// Inspired by the HTCondor C++ MatchClassAd implementation.
// See: https://github.com/htcondor/htcondor/blob/main/src/classad/classad/matchClassad.h
//
// In HTCondor, matching typically involves a job ClassAd and a machine ClassAd.
// Each can reference the other using TARGET.attr syntax.
type MatchClassAd struct {
	left  *ClassAd // Typically the "job" or requesting ClassAd
	right *ClassAd // Typically the "machine" or offering ClassAd
}

// NewMatchClassAd creates a new MatchClassAd with two ClassAds.
// The left ClassAd can reference the right using TARGET.attr
// The right ClassAd can reference the left using TARGET.attr
func NewMatchClassAd(left, right *ClassAd) *MatchClassAd {
	match := &MatchClassAd{
		left:  left,
		right: right,
	}

	// Set up bidirectional TARGET references
	if left != nil {
		left.SetTarget(right)
	}
	if right != nil {
		right.SetTarget(left)
	}

	return match
}

// GetLeftAd returns the left ClassAd.
func (m *MatchClassAd) GetLeftAd() *ClassAd {
	return m.left
}

// GetRightAd returns the right ClassAd.
func (m *MatchClassAd) GetRightAd() *ClassAd {
	return m.right
}

// EvaluateAttrLeft evaluates an attribute in the left ClassAd.
func (m *MatchClassAd) EvaluateAttrLeft(name string) Value {
	if m.left == nil {
		return NewUndefinedValue()
	}
	return m.left.EvaluateAttr(name)
}

// EvaluateAttrRight evaluates an attribute in the right ClassAd.
func (m *MatchClassAd) EvaluateAttrRight(name string) Value {
	if m.right == nil {
		return NewUndefinedValue()
	}
	return m.right.EvaluateAttr(name)
}

// Symmetry checks if both ClassAds' Requirements evaluate to true.
// This is the core matching operation in HTCondor.
// Returns true only if BOTH:
// - left.Requirements evaluates to true (with TARGET = right)
// - right.Requirements evaluates to true (with TARGET = left)
func (m *MatchClassAd) Symmetry(leftReqAttr, rightReqAttr string) bool {
	// Evaluate left Requirements
	leftReq := m.EvaluateAttrLeft(leftReqAttr)
	if !leftReq.IsBool() {
		return false
	}
	leftReqBool, err := leftReq.BoolValue()
	if err != nil || !leftReqBool {
		return false
	}

	// Evaluate right Requirements
	rightReq := m.EvaluateAttrRight(rightReqAttr)
	if !rightReq.IsBool() {
		return false
	}
	rightReqBool, err := rightReq.BoolValue()
	if err != nil || !rightReqBool {
		return false
	}

	return true
}

// Match checks if the ClassAds match using default "Requirements" attribute.
// This is equivalent to Symmetry("Requirements", "Requirements").
func (m *MatchClassAd) Match() bool {
	return m.Symmetry("Requirements", "Requirements")
}

// EvaluateRankLeft evaluates the left ClassAd's rank expression.
// In HTCondor, Rank is used to prefer certain matches over others.
func (m *MatchClassAd) EvaluateRankLeft() (float64, bool) {
	val := m.EvaluateAttrLeft("Rank")
	if val.IsInteger() {
		intVal, _ := val.IntValue()
		return float64(intVal), true
	}
	if val.IsReal() {
		realVal, _ := val.RealValue()
		return realVal, true
	}
	return 0.0, false
}

// EvaluateRankRight evaluates the right ClassAd's rank expression.
func (m *MatchClassAd) EvaluateRankRight() (float64, bool) {
	val := m.EvaluateAttrRight("Rank")
	if val.IsInteger() {
		intVal, _ := val.IntValue()
		return float64(intVal), true
	}
	if val.IsReal() {
		realVal, _ := val.RealValue()
		return realVal, true
	}
	return 0.0, false
}

// ReplaceLeftAd replaces the left ClassAd.
// Updates TARGET references appropriately.
func (m *MatchClassAd) ReplaceLeftAd(left *ClassAd) {
	m.left = left
	if left != nil {
		left.SetTarget(m.right)
	}
	if m.right != nil {
		m.right.SetTarget(left)
	}
}

// ReplaceRightAd replaces the right ClassAd.
// Updates TARGET references appropriately.
func (m *MatchClassAd) ReplaceRightAd(right *ClassAd) {
	m.right = right
	if right != nil {
		right.SetTarget(m.left)
	}
	if m.left != nil {
		m.left.SetTarget(right)
	}
}

// EvaluateExprLeft evaluates an expression in the context of the left ClassAd.
func (m *MatchClassAd) EvaluateExprLeft(expr ast.Expr) Value {
	if m.left == nil {
		return NewUndefinedValue()
	}
	return m.left.EvaluateExpr(expr)
}

// EvaluateExprRight evaluates an expression in the context of the right ClassAd.
func (m *MatchClassAd) EvaluateExprRight(expr ast.Expr) Value {
	if m.right == nil {
		return NewUndefinedValue()
	}
	return m.right.EvaluateExpr(expr)
}
