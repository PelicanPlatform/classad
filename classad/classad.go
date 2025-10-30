// Package classad provides a public API for working with HTCondor ClassAds.
// It mimics the C++ ClassAd library API from HTCondor.
package classad

import (
	"fmt"

	"github.com/PelicanPlatform/classad/ast"
	"github.com/PelicanPlatform/classad/parser"
)

// Expr represents an unevaluated ClassAd expression.
// It provides methods to evaluate the expression in different contexts
// and to inspect its structure.
type Expr struct {
	expr ast.Expr
}

// ParseExpr parses a ClassAd expression string and returns an Expr object.
// This allows you to work with expressions without evaluating them immediately.
//
// Example:
//
//	expr, err := classad.ParseExpr("Cpus * 2 + Memory / 1024")
//	if err != nil {
//	    log.Fatal(err)
//	}
func ParseExpr(input string) (*Expr, error) {
	// Wrap the expression in a temporary ClassAd for parsing
	wrapped := fmt.Sprintf("[__expr__ = %s]", input)
	node, err := parser.Parse(wrapped)
	if err != nil {
		return nil, err
	}

	// Extract the expression from the temporary attribute
	if ad, ok := node.(*ast.ClassAd); ok && len(ad.Attributes) == 1 {
		return &Expr{expr: ad.Attributes[0].Value}, nil
	}

	return nil, fmt.Errorf("unable to extract expression from parsed result")
}

// Quote escapes a string for safe use in ClassAd expressions.
// It adds surrounding quotes and escapes special characters according to ClassAd syntax.
//
// Example:
//
//	quoted := classad.Quote(`value with "quotes"`)
//	// Returns: "value with \"quotes\""
func Quote(s string) string {
	return fmt.Sprintf("%q", s)
}

// Unquote removes ClassAd string quoting and unescapes special characters.
// It expects the input to be a quoted string (with surrounding quotes).
//
// Example:
//
//	original, err := classad.Unquote(`"value with \"quotes\""`)
//	// Returns: value with "quotes"
func Unquote(s string) (string, error) {
	// Use Go's strconv unquoting which handles the same escape sequences
	if len(s) < 2 || s[0] != '"' || s[len(s)-1] != '"' {
		return "", fmt.Errorf("string must be quoted")
	}

	// Parse using Go's string literal rules which match ClassAd escaping
	var result string
	_, err := fmt.Sscanf(s, "%q", &result)
	if err != nil {
		return "", fmt.Errorf("failed to unquote string: %w", err)
	}
	return result, nil
}

// String returns the string representation of the expression.
func (e *Expr) String() string {
	if e.expr == nil {
		return "undefined"
	}
	return e.expr.String()
}

// internal returns the internal ast.Expr representation.
// This is used internally for operations that require the AST node.
func (e *Expr) internal() ast.Expr {
	if e == nil {
		return nil
	}
	return e.expr
}

// Eval evaluates the expression in the context of the given ClassAd.
// This is equivalent to calling classad.EvaluateExpr(expr).
func (e *Expr) Eval(scope *ClassAd) Value {
	if e.expr == nil {
		return NewUndefinedValue()
	}
	evaluator := NewEvaluator(scope)
	return evaluator.Evaluate(e.expr)
}

// EvalWithContext evaluates the expression with explicit MY (scope) and TARGET contexts.
// The scope parameter provides the context for MY.attr references.
// The target parameter provides the context for TARGET.attr references.
// Either parameter may be nil if not needed.
//
// Example:
//
//	expr, _ := classad.ParseExpr("MY.Cpus > TARGET.Cpus")
//	result := expr.EvalWithContext(jobAd, machineAd)
func (e *Expr) EvalWithContext(scope, target *ClassAd) Value {
	if e.expr == nil {
		return NewUndefinedValue()
	}

	// Set up the scope with target for evaluation
	if scope != nil && target != nil {
		// Temporarily set the target
		oldTarget := scope.target
		scope.target = target
		defer func() { scope.target = oldTarget }()
	}

	evaluator := NewEvaluator(scope)
	return evaluator.Evaluate(e.expr)
}

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

// ToOldFormat serializes the ClassAd to old HTCondor format (newline-delimited).
// Old format has one attribute per line without surrounding brackets.
//
// Example:
//
//	ad, _ := classad.Parse("[Cpus = 4; Memory = 8192]")
//	oldFmt := ad.MarshalOld()
//	// Returns: "Cpus = 4\nMemory = 8192"
func (c *ClassAd) MarshalOld() string {
	if c.ad == nil || len(c.ad.Attributes) == 0 {
		return ""
	}

	result := ""
	for i, attr := range c.ad.Attributes {
		if i > 0 {
			result += "\n"
		}
		result += fmt.Sprintf("%s = %s", attr.Name, attr.Value.String())
	}
	return result
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

// InsertExpr inserts an attribute with an Expr value into the ClassAd.
// This allows you to copy expressions between ClassAds without evaluating them.
//
// Example:
//
//	expr, _ := sourceAd.Lookup("Formula")
//	targetAd.InsertExpr("Formula", expr)
func (c *ClassAd) InsertExpr(name string, expr *Expr) {
	if expr == nil {
		return
	}
	c.Insert(name, expr.internal())
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
func (c *ClassAd) InsertAttrString(name, value string) {
	c.Insert(name, &ast.StringLiteral{Value: value})
}

// InsertAttrBool inserts an attribute with a boolean value.
func (c *ClassAd) InsertAttrBool(name string, value bool) {
	c.Insert(name, &ast.BooleanLiteral{Value: value})
}

// InsertAttrClassAd inserts an attribute with a nested ClassAd value.
// The provided ClassAd will be embedded as a record literal.
func (c *ClassAd) InsertAttrClassAd(name string, value *ClassAd) {
	var inner *ast.ClassAd
	if value != nil {
		inner = value.ad
	}
	if inner == nil {
		inner = &ast.ClassAd{Attributes: []*ast.AttributeAssignment{}}
	}
	c.Insert(name, &ast.RecordLiteral{ClassAd: inner})
}

// Lookup returns the unevaluated expression for an attribute.
// Returns nil if the attribute doesn't exist.
// This is useful for inspecting or copying expressions without evaluating them.
//
// Example:
//
//	ad, _ := classad.Parse("[x = 10; y = x * 2]")
//	expr, ok := ad.Lookup("y")  // Returns expression for "x * 2"
//	if ok {
//	    fmt.Println(expr.String())  // Prints: x * 2
//	}
func (c *ClassAd) Lookup(name string) (*Expr, bool) {
	if c.ad == nil {
		return nil, false
	}

	for _, attr := range c.ad.Attributes {
		if attr.Name == name {
			return &Expr{expr: attr.Value}, true
		}
	}
	return nil, false
}

// lookupInternal finds an expression bound to an attribute name.
// Returns nil if the attribute doesn't exist.
// This is the internal version that returns ast.Expr for backward compatibility.
func (c *ClassAd) lookupInternal(name string) ast.Expr {
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
	expr := c.lookupInternal(name)
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

// EvaluateExprWithTarget evaluates an Expr in the context of this ClassAd with a target.
// This ClassAd serves as the MY scope, and the target parameter serves as the TARGET scope.
// This is useful for match-making operations where expressions reference both ClassAds.
//
// Example:
//
//	expr, _ := classad.ParseExpr("MY.Cpus > TARGET.Cpus")
//	result := jobAd.EvaluateExprWithTarget(expr, machineAd)
func (c *ClassAd) EvaluateExprWithTarget(expr *Expr, target *ClassAd) Value {
	if expr == nil {
		return NewUndefinedValue()
	}
	return expr.EvalWithContext(c, target)
}

// ExternalRefs returns a list of attribute names referenced in the expression
// but not defined in this ClassAd. This is useful for validating that a ClassAd
// has all required attributes before evaluation.
//
// Example:
//
//	ad, _ := classad.Parse("[Cpus = 4; Memory = 8192]")
//	expr, _ := classad.ParseExpr("Cpus * 2 + ExternalAttr")
//	external := ad.ExternalRefs(expr)  // Returns: ["ExternalAttr"]
func (c *ClassAd) ExternalRefs(expr *Expr) []string {
	if expr == nil {
		return []string{}
	}

	allRefs := c.collectRefs(expr.expr)
	external := []string{}

	for _, ref := range allRefs {
		if _, ok := c.Lookup(ref); !ok {
			external = append(external, ref)
		}
	}

	return external
}

// InternalRefs returns a list of attribute names referenced in the expression
// that are defined in this ClassAd.
//
// Example:
//
//	ad, _ := classad.Parse("[Cpus = 4; Memory = 8192]")
//	expr, _ := classad.ParseExpr("Cpus * 2 + ExternalAttr")
//	internal := ad.InternalRefs(expr)  // Returns: ["Cpus"]
func (c *ClassAd) InternalRefs(expr *Expr) []string {
	if expr == nil {
		return []string{}
	}

	allRefs := c.collectRefs(expr.expr)
	internal := []string{}

	for _, ref := range allRefs {
		if _, ok := c.Lookup(ref); ok {
			internal = append(internal, ref)
		}
	}

	return internal
}

// collectRefs recursively collects all attribute references from an expression
func (c *ClassAd) collectRefs(expr ast.Expr) []string {
	if expr == nil {
		return []string{}
	}

	refs := make(map[string]bool)
	c.collectRefsHelper(expr, refs)

	// Convert map to slice
	result := make([]string, 0, len(refs))
	for ref := range refs {
		result = append(result, ref)
	}
	return result
}

// collectRefsHelper is a recursive helper for collectRefs
func (c *ClassAd) collectRefsHelper(expr ast.Expr, refs map[string]bool) {
	switch v := expr.(type) {
	case *ast.AttributeReference:
		// Only collect non-scoped references (no MY., TARGET., PARENT.)
		if v.Scope == ast.NoScope {
			refs[v.Name] = true
		}

	case *ast.BinaryOp:
		c.collectRefsHelper(v.Left, refs)
		c.collectRefsHelper(v.Right, refs)

	case *ast.UnaryOp:
		c.collectRefsHelper(v.Expr, refs)

	case *ast.ConditionalExpr:
		c.collectRefsHelper(v.Condition, refs)
		c.collectRefsHelper(v.TrueExpr, refs)
		c.collectRefsHelper(v.FalseExpr, refs)

	case *ast.FunctionCall:
		for _, arg := range v.Args {
			c.collectRefsHelper(arg, refs)
		}

	case *ast.ListLiteral:
		for _, elem := range v.Elements {
			c.collectRefsHelper(elem, refs)
		}

	case *ast.ClassAd:
		for _, attr := range v.Attributes {
			c.collectRefsHelper(attr.Value, refs)
		}

	case *ast.SelectExpr:
		c.collectRefsHelper(v.Record, refs)

	case *ast.SubscriptExpr:
		c.collectRefsHelper(v.Container, refs)
		c.collectRefsHelper(v.Index, refs)

	// Literals don't contain references
	case *ast.IntegerLiteral, *ast.RealLiteral, *ast.StringLiteral,
		*ast.BooleanLiteral, *ast.UndefinedLiteral, *ast.ErrorLiteral:
		// Nothing to do
	}
}

// Flatten partially evaluates an expression in the context of this ClassAd.
// Attributes that are defined in the ClassAd are evaluated and replaced with their values.
// Undefined attributes are left as references. This is useful for optimizing expressions
// by pre-computing constant sub-expressions.
//
// Example:
//
//	ad, _ := classad.Parse("[RequestMemory = 2048]")
//	expr, _ := classad.ParseExpr("RequestMemory * 1024 * 1024")
//	flattened := ad.Flatten(expr)  // Returns expression equivalent to: 2147483648
func (c *ClassAd) Flatten(expr *Expr) *Expr {
	if expr == nil {
		return nil
	}

	flattened := c.flattenExpr(expr.expr)
	return &Expr{expr: flattened}
}

// flattenExpr recursively flattens an AST expression
func (c *ClassAd) flattenExpr(expr ast.Expr) ast.Expr {
	if expr == nil {
		return nil
	}

	switch v := expr.(type) {
	case *ast.AttributeReference:
		// Try to evaluate the reference
		if v.Scope == ast.NoScope {
			if _, ok := c.Lookup(v.Name); ok {
				// Evaluate the attribute
				val := c.EvaluateAttr(v.Name)
				return c.valueToExpr(val)
			}
		}
		// Keep the reference if undefined or scoped
		return expr

	case *ast.BinaryOp:
		// Flatten both sides
		left := c.flattenExpr(v.Left)
		right := c.flattenExpr(v.Right)

		// Try to evaluate if both sides are literals
		leftVal := c.exprToValue(left)
		rightVal := c.exprToValue(right)

		if !leftVal.IsUndefined() && !rightVal.IsUndefined() {
			// Try to compute the operation
			result := c.evaluateBinaryOp(v.Op, leftVal, rightVal)
			if !result.IsUndefined() && !result.IsError() {
				return c.valueToExpr(result)
			}
		}

		// Return with flattened operands
		return &ast.BinaryOp{
			Op:    v.Op,
			Left:  left,
			Right: right,
		}

	case *ast.UnaryOp:
		operand := c.flattenExpr(v.Expr)
		operandVal := c.exprToValue(operand)

		if !operandVal.IsUndefined() {
			result := c.evaluateUnaryOp(v.Op, operandVal)
			if !result.IsUndefined() && !result.IsError() {
				return c.valueToExpr(result)
			}
		}

		return &ast.UnaryOp{
			Op:   v.Op,
			Expr: operand,
		}

	case *ast.ConditionalExpr:
		condition := c.flattenExpr(v.Condition)
		trueExpr := c.flattenExpr(v.TrueExpr)
		falseExpr := c.flattenExpr(v.FalseExpr)

		// If condition is a literal boolean, return the appropriate branch
		condVal := c.exprToValue(condition)
		if condVal.IsBool() {
			if boolVal, _ := condVal.BoolValue(); boolVal {
				return trueExpr
			} else {
				return falseExpr
			}
		}

		return &ast.ConditionalExpr{
			Condition: condition,
			TrueExpr:  trueExpr,
			FalseExpr: falseExpr,
		}

	case *ast.FunctionCall:
		// Flatten arguments
		args := make([]ast.Expr, len(v.Args))
		for i, arg := range v.Args {
			args[i] = c.flattenExpr(arg)
		}
		return &ast.FunctionCall{
			Name: v.Name,
			Args: args,
		}

	case *ast.ListLiteral:
		elements := make([]ast.Expr, len(v.Elements))
		for i, elem := range v.Elements {
			elements[i] = c.flattenExpr(elem)
		}
		return &ast.ListLiteral{
			Elements: elements,
		}

	case *ast.SelectExpr:
		record := c.flattenExpr(v.Record)
		return &ast.SelectExpr{
			Record: record,
			Attr:   v.Attr,
		}

	case *ast.SubscriptExpr:
		container := c.flattenExpr(v.Container)
		index := c.flattenExpr(v.Index)
		return &ast.SubscriptExpr{
			Container: container,
			Index:     index,
		}

	// Literals are already fully evaluated
	default:
		return expr
	}
}

// exprToValue converts a literal expression to a Value, returns undefined for non-literals
func (c *ClassAd) exprToValue(expr ast.Expr) Value {
	switch v := expr.(type) {
	case *ast.IntegerLiteral:
		return NewIntValue(v.Value)
	case *ast.RealLiteral:
		return NewRealValue(v.Value)
	case *ast.StringLiteral:
		return NewStringValue(v.Value)
	case *ast.BooleanLiteral:
		return NewBoolValue(v.Value)
	case *ast.UndefinedLiteral:
		return NewUndefinedValue()
	case *ast.ErrorLiteral:
		return NewErrorValue()
	default:
		return NewUndefinedValue()
	}
}

// valueToExpr converts a Value to an AST expression
func (c *ClassAd) valueToExpr(val Value) ast.Expr {
	switch val.Type() {
	case IntegerValue:
		if intVal, err := val.IntValue(); err == nil {
			return &ast.IntegerLiteral{Value: intVal}
		}
	case RealValue:
		if realVal, err := val.RealValue(); err == nil {
			return &ast.RealLiteral{Value: realVal}
		}
	case StringValue:
		if strVal, err := val.StringValue(); err == nil {
			return &ast.StringLiteral{Value: strVal}
		}
	case BooleanValue:
		if boolVal, err := val.BoolValue(); err == nil {
			return &ast.BooleanLiteral{Value: boolVal}
		}
	case UndefinedValue:
		return &ast.UndefinedLiteral{}
	case ErrorValue:
		return &ast.ErrorLiteral{}
	}
	return &ast.UndefinedLiteral{}
}

// Helper functions for evaluating operations during flattening
func (c *ClassAd) evaluateBinaryOp(op string, left, right Value) Value {
	// Create a temporary evaluator to use its operator logic
	evaluator := NewEvaluator(c)
	tempOp := &ast.BinaryOp{
		Op:    op,
		Left:  c.valueToExpr(left),
		Right: c.valueToExpr(right),
	}
	return evaluator.Evaluate(tempOp)
}

func (c *ClassAd) evaluateUnaryOp(op string, operand Value) Value {
	evaluator := NewEvaluator(c)
	tempOp := &ast.UnaryOp{
		Op:   op,
		Expr: c.valueToExpr(operand),
	}
	return evaluator.Evaluate(tempOp)
}
