// Package classad provides a public API for working with HTCondor ClassAds.
// It mimics the C++ ClassAd library API from HTCondor.
package classad

import (
	"fmt"

	"github.com/bbockelm/golang-classads/ast"
	"github.com/bbockelm/golang-classads/parser"
)

// ClassAd represents a ClassAd with attributes that can be evaluated.
// This is the main type for working with ClassAds.
type ClassAd struct {
	ad     *ast.ClassAd
	parent *ClassAd
	target *ClassAd
}

// New creates a new empty ClassAd.
func New() *ClassAd {
	return &ClassAd{
		ad: &ast.ClassAd{
			Attributes: []*ast.AttributeAssignment{},
		},
	}
}

// Parse parses a ClassAd string and returns a ClassAd object.
func Parse(input string) (*ClassAd, error) {
	ad, err := parser.ParseClassAd(input)
	if err != nil {
		return nil, err
	}
	if ad == nil {
		return nil, fmt.Errorf("failed to parse ClassAd")
	}
	return &ClassAd{ad: ad}, nil
}

// ParseOld parses a ClassAd in the "old" HTCondor format and returns a ClassAd object.
// Old ClassAds have attributes separated by newlines without surrounding brackets.
// Example:
//
//	Foo = 3
//	Bar = "hello"
//	Moo = Foo =!= Undefined
func ParseOld(input string) (*ClassAd, error) {
	ad, err := parser.ParseOldClassAd(input)
	if err != nil {
		return nil, err
	}
	if ad == nil {
		return nil, fmt.Errorf("failed to parse old ClassAd")
	}
	return &ClassAd{ad: ad}, nil
}

// String returns the string representation of the ClassAd.
func (c *ClassAd) String() string {
	if c.ad == nil {
		return "[]"
	}
	return c.ad.String()
}

// Insert inserts an attribute with an expression into the ClassAd.
func (c *ClassAd) Insert(name string, expr ast.Expr) {
	if c.ad == nil {
		c.ad = &ast.ClassAd{Attributes: []*ast.AttributeAssignment{}}
	}

	// Check if attribute already exists and update it
	for i, attr := range c.ad.Attributes {
		if attr.Name == name {
			c.ad.Attributes[i].Value = expr
			return
		}
	}

	// Add new attribute
	c.ad.Attributes = append(c.ad.Attributes, &ast.AttributeAssignment{
		Name:  name,
		Value: expr,
	})
}

// InsertAttr inserts an attribute with an integer value.
func (c *ClassAd) InsertAttr(name string, value int64) {
	c.Insert(name, &ast.IntegerLiteral{Value: value})
}

// InsertAttrFloat inserts an attribute with a float value.
func (c *ClassAd) InsertAttrFloat(name string, value float64) {
	c.Insert(name, &ast.RealLiteral{Value: value})
}

// InsertAttrString inserts an attribute with a string value.
func (c *ClassAd) InsertAttrString(name string, value string) {
	c.Insert(name, &ast.StringLiteral{Value: value})
}

// InsertAttrBool inserts an attribute with a boolean value.
func (c *ClassAd) InsertAttrBool(name string, value bool) {
	c.Insert(name, &ast.BooleanLiteral{Value: value})
}

// Lookup finds an expression bound to an attribute name.
// Returns nil if the attribute doesn't exist.
func (c *ClassAd) Lookup(name string) ast.Expr {
	if c.ad == nil {
		return nil
	}

	for _, attr := range c.ad.Attributes {
		if attr.Name == name {
			return attr.Value
		}
	}
	return nil
}

// Delete removes an attribute from the ClassAd.
// Returns true if the attribute was found and deleted.
func (c *ClassAd) Delete(name string) bool {
	if c.ad == nil {
		return false
	}

	for i, attr := range c.ad.Attributes {
		if attr.Name == name {
			c.ad.Attributes = append(c.ad.Attributes[:i], c.ad.Attributes[i+1:]...)
			return true
		}
	}
	return false
}

// Size returns the number of attributes in the ClassAd.
func (c *ClassAd) Size() int {
	if c.ad == nil {
		return 0
	}
	return len(c.ad.Attributes)
}

// Clear removes all attributes from the ClassAd.
func (c *ClassAd) Clear() {
	if c.ad != nil {
		c.ad.Attributes = []*ast.AttributeAssignment{}
	}
}

// GetAttributes returns a list of all attribute names.
func (c *ClassAd) GetAttributes() []string {
	if c.ad == nil {
		return []string{}
	}

	names := make([]string, len(c.ad.Attributes))
	for i, attr := range c.ad.Attributes {
		names[i] = attr.Name
	}
	return names
}

// SetParent sets the parent ClassAd for this ClassAd.
// The parent is used for PARENT.attr references during evaluation.
func (c *ClassAd) SetParent(parent *ClassAd) {
	c.parent = parent
}

// GetParent returns the parent ClassAd, if any.
func (c *ClassAd) GetParent() *ClassAd {
	return c.parent
}

// SetTarget sets the target ClassAd for this ClassAd.
// The target is used for TARGET.attr references during evaluation.
func (c *ClassAd) SetTarget(target *ClassAd) {
	c.target = target
}

// GetTarget returns the target ClassAd, if any.
func (c *ClassAd) GetTarget() *ClassAd {
	return c.target
}

// EvaluateAttr evaluates an attribute and returns its value.
func (c *ClassAd) EvaluateAttr(name string) Value {
	expr := c.Lookup(name)
	if expr == nil {
		return NewUndefinedValue()
	}

	evaluator := NewEvaluator(c)
	return evaluator.Evaluate(expr)
}

// EvaluateAttrInt evaluates an attribute as an integer.
// Returns true if the attribute evaluated to an integer.
func (c *ClassAd) EvaluateAttrInt(name string) (int64, bool) {
	val := c.EvaluateAttr(name)
	if !val.IsInteger() {
		return 0, false
	}
	intVal, err := val.IntValue()
	if err != nil {
		return 0, false
	}
	return intVal, true
}

// EvaluateAttrReal evaluates an attribute as a real number.
// Returns true if the attribute evaluated to a real.
func (c *ClassAd) EvaluateAttrReal(name string) (float64, bool) {
	val := c.EvaluateAttr(name)
	if !val.IsReal() {
		return 0, false
	}
	realVal, err := val.RealValue()
	if err != nil {
		return 0, false
	}
	return realVal, true
}

// EvaluateAttrNumber evaluates an attribute as a number (int or real).
// Returns true if the attribute evaluated to a number.
// Integers are promoted to float64.
func (c *ClassAd) EvaluateAttrNumber(name string) (float64, bool) {
	val := c.EvaluateAttr(name)
	if !val.IsNumber() {
		return 0, false
	}
	numVal, err := val.NumberValue()
	if err != nil {
		return 0, false
	}
	return numVal, true
}

// EvaluateAttrString evaluates an attribute as a string.
// Returns true if the attribute evaluated to a string.
func (c *ClassAd) EvaluateAttrString(name string) (string, bool) {
	val := c.EvaluateAttr(name)
	if !val.IsString() {
		return "", false
	}
	strVal, err := val.StringValue()
	if err != nil {
		return "", false
	}
	return strVal, true
}

// EvaluateAttrBool evaluates an attribute as a boolean.
// Returns true if the attribute evaluated to a boolean.
func (c *ClassAd) EvaluateAttrBool(name string) (bool, bool) {
	val := c.EvaluateAttr(name)
	if !val.IsBool() {
		return false, false
	}
	boolVal, err := val.BoolValue()
	if err != nil {
		return false, false
	}
	return boolVal, true
}

// EvaluateExpr evaluates an arbitrary expression in the context of this ClassAd.
func (c *ClassAd) EvaluateExpr(expr ast.Expr) Value {
	evaluator := NewEvaluator(c)
	return evaluator.Evaluate(expr)
}

// EvaluateExprString parses and evaluates an expression string.
func (c *ClassAd) EvaluateExprString(exprStr string) (Value, error) {
	// Parse the expression
	node, err := parser.Parse(exprStr)
	if err != nil {
		return NewErrorValue(), err
	}

	// Extract expression from parsed result
	var expr ast.Expr
	if ad, ok := node.(*ast.ClassAd); ok && len(ad.Attributes) == 1 {
		// If it's a simple assignment, evaluate the RHS
		expr = ad.Attributes[0].Value
	} else if e, ok := node.(ast.Expr); ok {
		expr = e
	} else {
		return NewErrorValue(), fmt.Errorf("unable to extract expression from parsed result")
	}

	return c.EvaluateExpr(expr), nil
}
