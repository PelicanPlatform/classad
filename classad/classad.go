// Package classad provides a public API for working with HTCondor ClassAds.
// It mimics the C++ ClassAd library API from HTCondor.
package classad

import (
	"bytes"
	"encoding/json"
	"fmt"
	"math"
	"reflect"
	"sort"
	"strings"

	"github.com/PelicanPlatform/classad/ast"
	"github.com/PelicanPlatform/classad/parser"
)

// Expr represents an unevaluated ClassAd expression.
// It provides methods to evaluate the expression in different contexts
// and to inspect its structure.
type Expr struct {
	expr ast.Expr
}

// Equal reports structural equality between two expressions.
func (e *Expr) Equal(other *Expr) bool {
	if e == nil && other == nil {
		return true
	}
	if e == nil || other == nil {
		return false
	}
	return exprEqual(e.internal(), other.internal())
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
	ad         *ast.ClassAd
	parent     *ClassAd
	target     *ClassAd
	index      map[string]*ast.Expr
	attrsDirty bool // true when attributes changed since last sort
}

// Equal reports whether two ClassAds have the same attributes and values, ignoring
// attribute order and casing of attribute names.
func (c *ClassAd) Equal(other *ClassAd) bool {
	if c == nil && other == nil {
		return true
	}
	if c == nil || other == nil {
		return false
	}

	c.ensureSorted()
	other.ensureSorted()

	if len(c.ad.Attributes) != len(other.ad.Attributes) {
		return false
	}

	for i := range c.ad.Attributes {
		left := c.ad.Attributes[i]
		right := other.ad.Attributes[i]
		if normalizeName(left.Name) != normalizeName(right.Name) {
			return false
		}
		if !exprEqual(left.Value, right.Value) {
			return false
		}
	}

	return true
}

// normalizeName returns a case-insensitive key for attribute lookups.
func normalizeName(name string) string {
	return strings.ToLower(name)
}

// rebuildIndex recreates the fast lookup map from the underlying attributes.
func (c *ClassAd) rebuildIndex() {
	if c.ad == nil {
		c.index = nil
		return
	}
	if c.index == nil {
		c.index = make(map[string]*ast.Expr, len(c.ad.Attributes))
	} else {
		for k := range c.index {
			delete(c.index, k)
		}
	}
	for i := range c.ad.Attributes {
		attr := c.ad.Attributes[i]
		c.index[normalizeName(attr.Name)] = &attr.Value
	}
}

// ensureSorted sorts attributes in-place by normalized name when dirty,
// then rebuilds the index so pointers remain valid.
func (c *ClassAd) ensureSorted() {
	if c.ad == nil {
		c.attrsDirty = false
		return
	}
	if !c.attrsDirty {
		c.ensureIndex()
		return
	}

	sort.SliceStable(c.ad.Attributes, func(i, j int) bool {
		iName := normalizeName(c.ad.Attributes[i].Name)
		jName := normalizeName(c.ad.Attributes[j].Name)
		if iName == jName {
			return c.ad.Attributes[i].Name < c.ad.Attributes[j].Name
		}
		return iName < jName
	})
	c.attrsDirty = false
	c.rebuildIndex()
}

func (c *ClassAd) markDirty() {
	c.attrsDirty = true
}

// ensureIndex lazily initializes the lookup map if needed.
func (c *ClassAd) ensureIndex() {
	if c.index == nil {
		c.rebuildIndex()
	}
}

// New creates a new empty ClassAd.
func New() *ClassAd {
	return &ClassAd{
		ad: &ast.ClassAd{
			Attributes: []*ast.AttributeAssignment{},
		},
		index:      map[string]*ast.Expr{},
		attrsDirty: false,
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
	obj := &ClassAd{ad: ad, attrsDirty: true}
	obj.rebuildIndex()
	return obj, nil
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
	obj := &ClassAd{ad: ad, attrsDirty: true}
	obj.rebuildIndex()
	return obj, nil
}

// String returns the string representation of the ClassAd.
func (c *ClassAd) String() string {
	if c.ad == nil {
		return "[]"
	}

	c.ensureSorted()
	var b strings.Builder
	b.WriteByte('[')
	for i, attr := range c.ad.Attributes {
		if i > 0 {
			b.WriteString("; ")
		}
		b.WriteString(attr.Name)
		b.WriteString(" = ")
		b.WriteString(attr.Value.String())
	}
	b.WriteByte(']')
	return b.String()
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

	c.ensureSorted()
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
	c.ensureIndex()
	c.markDirty()

	normalized := normalizeName(name)
	if ptr, ok := c.index[normalized]; ok {
		*ptr = expr
		return
	}

	// Add new attribute
	c.ad.Attributes = append(c.ad.Attributes, &ast.AttributeAssignment{
		Name:  name,
		Value: expr,
	})
	c.index[normalized] = &c.ad.Attributes[len(c.ad.Attributes)-1].Value
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

// InsertAttrList inserts an attribute with a list of values using generics.
// Supported types are: int64, float64, string, bool, *ClassAd, and *Expr.
//
// Example:
//
//	ad := classad.New()
//	InsertAttrList(ad, "numbers", []int64{1, 2, 3, 4, 5})
//	InsertAttrList(ad, "names", []string{"Alice", "Bob", "Charlie"})
//	InsertAttrList(ad, "flags", []bool{true, false, true})
//	InsertAttrList(ad, "values", []float64{1.5, 2.7, 3.14})
//
//	// Also works with ClassAds
//	ad1, ad2 := classad.New(), classad.New()
//	ad1.InsertAttr("x", 1)
//	ad2.InsertAttr("y", 2)
//	InsertAttrList(ad, "items", []*classad.ClassAd{ad1, ad2})
//
//	// And with expressions
//	expr1, _ := classad.ParseExpr("\"hello\"")
//	expr2, _ := classad.ParseExpr("42")
//	InsertAttrList(ad, "mixed", []*classad.Expr{expr1, expr2})
func InsertAttrList[T int64 | float64 | string | bool | *ClassAd | *Expr](c *ClassAd, name string, values []T) {
	elements := make([]ast.Expr, len(values))
	for i, v := range values {
		elements[i] = valueToAstExpr(v)
	}
	c.Insert(name, &ast.ListLiteral{Elements: elements})
}

// valueToAstExpr converts a value to an ast.Expr based on its type.
func valueToAstExpr[T int64 | float64 | string | bool | *ClassAd | *Expr](v T) ast.Expr {
	switch val := any(v).(type) {
	case int64:
		return &ast.IntegerLiteral{Value: val}
	case float64:
		return &ast.RealLiteral{Value: val}
	case string:
		return &ast.StringLiteral{Value: val}
	case bool:
		return &ast.BooleanLiteral{Value: val}
	case *ClassAd:
		var inner *ast.ClassAd
		if val != nil {
			inner = val.ad
		}
		if inner == nil {
			inner = &ast.ClassAd{Attributes: []*ast.AttributeAssignment{}}
		}
		return &ast.RecordLiteral{ClassAd: inner}
	case *Expr:
		if val == nil {
			return &ast.UndefinedLiteral{}
		}
		return val.internal()
	default:
		return &ast.UndefinedLiteral{}
	}
}

// InsertListElement inserts an element into a list attribute.
// If the attribute doesn't exist, it creates a new list with the element.
// If the attribute exists and is a list, it appends the element to the existing list.
// If the attribute exists but is not a list, it replaces it with a new list containing the element.
//
// Example:
//
//	ad := classad.New()
//	expr1, _ := classad.ParseExpr("\"first\"")
//	expr2, _ := classad.ParseExpr("\"second\"")
//	ad.InsertListElement("items", expr1)
//	ad.InsertListElement("items", expr2)
//	// Result: items = {"first", "second"}
func (c *ClassAd) InsertListElement(name string, element *Expr) {
	if c.ad == nil {
		c.ad = &ast.ClassAd{Attributes: []*ast.AttributeAssignment{}}
	}
	c.ensureIndex()
	c.markDirty()

	var astExpr ast.Expr
	if element == nil {
		astExpr = &ast.UndefinedLiteral{}
	} else {
		astExpr = element.internal()
	}

	if ptr, ok := c.index[normalizeName(name)]; ok {
		if list, ok := (*ptr).(*ast.ListLiteral); ok {
			list.Elements = append(list.Elements, astExpr)
			return
		}
		*ptr = &ast.ListLiteral{Elements: []ast.Expr{astExpr}}
		return
	}

	// Add new list attribute
	c.ad.Attributes = append(c.ad.Attributes, &ast.AttributeAssignment{
		Name:  name,
		Value: &ast.ListLiteral{Elements: []ast.Expr{astExpr}},
	})
	c.index[normalizeName(name)] = &c.ad.Attributes[len(c.ad.Attributes)-1].Value
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
	c.ensureIndex()

	if ptr, ok := c.index[normalizeName(name)]; ok {
		return &Expr{expr: *ptr}, true
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
	c.ensureIndex()

	if ptr, ok := c.index[normalizeName(name)]; ok {
		return *ptr
	}
	return nil
}

// Set is a generic method that accepts any Go value and inserts it as an attribute.
// This provides a more idiomatic alternative to the type-specific Insert* methods.
// Supported types: int/int64/etc., float64, string, bool, []T, *ClassAd, *Expr, and structs.
//
// Example:
//
//	ad := classad.New()
//	ad.Set("cpus", 4)                    // int
//	ad.Set("name", "job-1")              // string
//	ad.Set("price", 3.14)                // float64
//	ad.Set("enabled", true)              // bool
//	ad.Set("tags", []string{"a", "b"})   // slice
//	ad.Set("config", nestedClassAd)      // *ClassAd
func (c *ClassAd) Set(name string, value any) error {
	if value == nil {
		c.Insert(name, &ast.UndefinedLiteral{})
		return nil
	}

	val := reflect.ValueOf(value)

	// Handle special types first
	switch v := value.(type) {
	case *ClassAd:
		c.InsertAttrClassAd(name, v)
		return nil
	case *Expr:
		c.InsertExpr(name, v)
		return nil
	case int64:
		c.InsertAttr(name, v)
		return nil
	case float64:
		c.InsertAttrFloat(name, v)
		return nil
	case string:
		c.InsertAttrString(name, v)
		return nil
	case bool:
		c.InsertAttrBool(name, v)
		return nil
	}

	// Handle other integer types
	switch val.Kind() {
	case reflect.Int, reflect.Int8, reflect.Int16, reflect.Int32:
		c.InsertAttr(name, val.Int())
		return nil
	case reflect.Uint, reflect.Uint8, reflect.Uint16, reflect.Uint32, reflect.Uint64:
		c.InsertAttr(name, int64(val.Uint()))
		return nil
	case reflect.Float32:
		c.InsertAttrFloat(name, val.Float())
		return nil
	}

	// Try to marshal the value using the struct marshaling logic
	expr, err := marshalValue(val)
	if err != nil {
		return fmt.Errorf("failed to marshal value of type %T: %w", value, err)
	}
	c.Insert(name, expr)
	return nil
}

// GetAs retrieves and evaluates an attribute, converting it to the specified type.
// This is a type-safe generic getter that handles type conversions automatically.
// Returns the zero value and false if the attribute doesn't exist or conversion fails.
//
// Example:
//
//	cpus, ok := classad.GetAs[int](ad, "cpus")
//	name, ok := classad.GetAs[string](ad, "name")
//	price, ok := classad.GetAs[float64](ad, "price")
//	tags, ok := classad.GetAs[[]string](ad, "tags")
//	config, ok := classad.GetAs[*classad.ClassAd](ad, "config")
func GetAs[T any](c *ClassAd, name string) (T, bool) {
	var zero T

	// Special case for *Expr - return unevaluated expression
	if _, isExpr := any(zero).(*Expr); isExpr {
		expr, ok := c.Lookup(name)
		if !ok {
			return zero, false
		}
		// Safe type assertion since we checked isExpr above
		result, ok := any(expr).(T)
		if !ok {
			return zero, false
		}
		return result, true
	}

	// For other types, evaluate the attribute
	val := c.EvaluateAttr(name)
	if val.IsUndefined() {
		return zero, false
	}

	// Try to unmarshal into the target type
	target := reflect.New(reflect.TypeOf(zero)).Elem()
	if err := unmarshalValueInto(val, target); err != nil {
		return zero, false
	}

	// Safe type assertion since unmarshalValueInto ensures type compatibility
	result, ok := target.Interface().(T)
	if !ok {
		return zero, false
	}
	return result, true
}

// GetOr retrieves and evaluates an attribute with a default value.
// If the attribute doesn't exist or conversion fails, returns the default value.
// This is a type-safe generic getter with fallback.
//
// Example:
//
//	cpus := classad.GetOr(ad, "cpus", 1)           // Defaults to 1
//	name := classad.GetOr(ad, "name", "unknown")   // Defaults to "unknown"
//	timeout := classad.GetOr(ad, "timeout", 300)   // Defaults to 300
func GetOr[T any](c *ClassAd, name string, defaultValue T) T {
	if value, ok := GetAs[T](c, name); ok {
		return value
	}
	return defaultValue
}

// Delete removes an attribute from the ClassAd.
// Returns true if the attribute was found and deleted.
func (c *ClassAd) Delete(name string) bool {
	if c.ad == nil {
		return false
	}
	c.ensureIndex()
	c.markDirty()

	normalized := normalizeName(name)
	ptr, ok := c.index[normalized]
	if !ok {
		return false
	}

	// Find the matching attribute by pointer equality on Value.
	for i := range c.ad.Attributes {
		if &c.ad.Attributes[i].Value == ptr {
			c.ad.Attributes = append(c.ad.Attributes[:i], c.ad.Attributes[i+1:]...)
			delete(c.index, normalized)
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
	c.index = map[string]*ast.Expr{}
	c.attrsDirty = false
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

	case *ast.ElvisExpr:
		c.collectRefsHelper(v.Left, refs)
		c.collectRefsHelper(v.Right, refs)

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

		// Apply boolean short-circuiting when either side is a literal bool.
		if v.Op == "&&" || v.Op == "||" {
			if leftVal.IsBool() {
				boolVal, err := leftVal.BoolValue()
				if err != nil {
					return &ast.ErrorLiteral{}
				}
				if v.Op == "&&" {
					if !boolVal {
						return &ast.BooleanLiteral{Value: false}
					}
					return right
				}
				// v.Op == "||"
				if boolVal {
					return &ast.BooleanLiteral{Value: true}
				}
				return right
			}
			if rightVal.IsBool() {
				boolVal, err := rightVal.BoolValue()
				if err != nil {
					return &ast.ErrorLiteral{}
				}
				if v.Op == "&&" {
					if !boolVal {
						return &ast.BooleanLiteral{Value: false}
					}
					return left
				}
				// v.Op == "||"
				if boolVal {
					return &ast.BooleanLiteral{Value: true}
				}
				return left
			}
		}

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
			if boolVal, err := condVal.BoolValue(); err == nil && boolVal {
				return trueExpr
			}
			return falseExpr
		}

		return &ast.ConditionalExpr{
			Condition: condition,
			TrueExpr:  trueExpr,
			FalseExpr: falseExpr,
		}

	case *ast.ElvisExpr:
		left := c.flattenExpr(v.Left)
		right := c.flattenExpr(v.Right)

		// If left is a literal undefined, return right
		leftVal := c.exprToValue(left)
		if leftVal.IsUndefined() {
			return right
		}

		return &ast.ElvisExpr{
			Left:  left,
			Right: right,
		}

	case *ast.FunctionCall:
		// Flatten arguments
		args := make([]ast.Expr, len(v.Args))
		for i, arg := range v.Args {
			args[i] = c.flattenExpr(arg)
		}

		// Fold ifThenElse when the condition is a literal boolean after flattening.
		if strings.EqualFold(v.Name, "ifThenElse") && len(args) == 3 {
			condVal := c.exprToValue(args[0])
			if condVal.IsBool() {
				boolVal, err := condVal.BoolValue()
				if err != nil {
					return &ast.ErrorLiteral{}
				}
				if boolVal {
					return args[1]
				}
				return args[2]
			}
		}

		return &ast.FunctionCall{Name: v.Name, Args: args}

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
	case ListValue:
		list, err := val.ListValue()
		if err == nil {
			elements := make([]ast.Expr, 0, len(list))
			for _, item := range list {
				elements = append(elements, c.valueToExpr(item))
			}
			return &ast.ListLiteral{Elements: elements}
		}
	case ClassAdValue:
		adVal, err := val.ClassAdValue()
		if err == nil && adVal != nil {
			return &ast.RecordLiteral{ClassAd: adVal.ad}
		}
	case UndefinedValue:
		return &ast.UndefinedLiteral{}
	case ErrorValue:
		return &ast.ErrorLiteral{}
	}
	return &ast.UndefinedLiteral{}
}

// exprEqual compares two ast expressions for structural equality.
func exprEqual(a, b ast.Expr) bool {
	if a == nil && b == nil {
		return true
	}
	if a == nil || b == nil {
		return false
	}

	switch av := a.(type) {
	case *ast.IntegerLiteral:
		bv, ok := b.(*ast.IntegerLiteral)
		return ok && av.Value == bv.Value
	case *ast.RealLiteral:
		bv, ok := b.(*ast.RealLiteral)
		return ok && floatEqual(av.Value, bv.Value)
	case *ast.StringLiteral:
		bv, ok := b.(*ast.StringLiteral)
		return ok && av.Value == bv.Value
	case *ast.BooleanLiteral:
		bv, ok := b.(*ast.BooleanLiteral)
		return ok && av.Value == bv.Value
	case *ast.UndefinedLiteral:
		_, ok := b.(*ast.UndefinedLiteral)
		return ok
	case *ast.ErrorLiteral:
		_, ok := b.(*ast.ErrorLiteral)
		return ok
	case *ast.AttributeReference:
		bv, ok := b.(*ast.AttributeReference)
		return ok && av.Scope == bv.Scope && strings.EqualFold(av.Name, bv.Name)
	case *ast.BinaryOp:
		bv, ok := b.(*ast.BinaryOp)
		return ok && av.Op == bv.Op && exprEqual(av.Left, bv.Left) && exprEqual(av.Right, bv.Right)
	case *ast.UnaryOp:
		bv, ok := b.(*ast.UnaryOp)
		return ok && av.Op == bv.Op && exprEqual(av.Expr, bv.Expr)
	case *ast.ConditionalExpr:
		bv, ok := b.(*ast.ConditionalExpr)
		return ok && exprEqual(av.Condition, bv.Condition) && exprEqual(av.TrueExpr, bv.TrueExpr) && exprEqual(av.FalseExpr, bv.FalseExpr)
	case *ast.ElvisExpr:
		bv, ok := b.(*ast.ElvisExpr)
		return ok && exprEqual(av.Left, bv.Left) && exprEqual(av.Right, bv.Right)
	case *ast.FunctionCall:
		bv, ok := b.(*ast.FunctionCall)
		if !ok || !strings.EqualFold(av.Name, bv.Name) || len(av.Args) != len(bv.Args) {
			return false
		}
		for i := range av.Args {
			if !exprEqual(av.Args[i], bv.Args[i]) {
				return false
			}
		}
		return true
	case *ast.ListLiteral:
		bv, ok := b.(*ast.ListLiteral)
		if !ok || len(av.Elements) != len(bv.Elements) {
			return false
		}
		for i := range av.Elements {
			if !exprEqual(av.Elements[i], bv.Elements[i]) {
				return false
			}
		}
		return true
	case *ast.RecordLiteral:
		bv, ok := b.(*ast.RecordLiteral)
		if !ok {
			return false
		}
		left := &ClassAd{ad: av.ClassAd, attrsDirty: true}
		left.rebuildIndex()
		right := &ClassAd{ad: bv.ClassAd, attrsDirty: true}
		right.rebuildIndex()
		return left.Equal(right)
	case *ast.SelectExpr:
		bv, ok := b.(*ast.SelectExpr)
		return ok && strings.EqualFold(av.Attr, bv.Attr) && exprEqual(av.Record, bv.Record)
	case *ast.SubscriptExpr:
		bv, ok := b.(*ast.SubscriptExpr)
		return ok && exprEqual(av.Container, bv.Container) && exprEqual(av.Index, bv.Index)
	default:
		return false
	}
}

// floatEqual compares two float64 values with a relative tolerance to account for
// floating point rounding. NaN only equals NaN; +Inf/-Inf must match exactly.
func floatEqual(a, b float64) bool {
	if math.IsNaN(a) || math.IsNaN(b) {
		return math.IsNaN(a) && math.IsNaN(b)
	}
	if math.IsInf(a, 0) || math.IsInf(b, 0) {
		return math.IsInf(a, 1) == math.IsInf(b, 1) && math.IsInf(a, -1) == math.IsInf(b, -1)
	}

	const relTol = 1e-9
	diff := math.Abs(a - b)
	if diff == 0 {
		return true
	}
	mag := math.Max(math.Abs(a), math.Abs(b))
	return diff <= relTol*mag
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

// MarshalJSON implements the json.Marshaler interface for ClassAd.
// ClassAds are serialized as JSON objects where:
//   - Attribute names are JSON keys
//   - Simple values (strings, numbers, booleans) are JSON values
//   - Lists are JSON arrays
//   - Nested ClassAds are JSON objects
//   - Expressions are serialized as strings with the format "/Expr(<expression>)/"
//     (which appears as "\/Expr(<expression>)\/" in JSON due to escaping)
//
// Example:
//
//	ad, _ := classad.Parse(`[x = 5; y = x + 3; name = "test"]`)
//	jsonBytes, _ := json.Marshal(ad)
//	// {"name":"test","x":5,"y":"\/Expr(x + 3)\/"}
func (c *ClassAd) MarshalJSON() ([]byte, error) {
	if c.ad == nil {
		return []byte("{}"), nil
	}

	c.ensureSorted()
	var buf bytes.Buffer
	buf.WriteByte('{')
	for i, attr := range c.ad.Attributes {
		if i > 0 {
			buf.WriteByte(',')
		}
		keyBytes, err := json.Marshal(attr.Name)
		if err != nil {
			return nil, err
		}
		value, err := c.marshalValue(attr.Value)
		if err != nil {
			return nil, fmt.Errorf("failed to marshal attribute %s: %w", attr.Name, err)
		}
		valBytes, err := json.Marshal(value)
		if err != nil {
			return nil, err
		}
		buf.Write(keyBytes)
		buf.WriteByte(':')
		buf.Write(valBytes)
	}
	buf.WriteByte('}')

	jsonBytes := buf.Bytes()
	jsonBytes = []byte(strings.ReplaceAll(string(jsonBytes), "\"/Expr(", "\"\\/Expr("))
	jsonBytes = []byte(strings.ReplaceAll(string(jsonBytes), ")/\"", ")\\/\""))

	return jsonBytes, nil
}

// marshalValue converts an AST expression to a JSON-serializable value.
// Simple literals are converted directly, complex expressions are wrapped.
func (c *ClassAd) marshalValue(expr ast.Expr) (interface{}, error) {
	switch v := expr.(type) {
	case *ast.IntegerLiteral:
		return v.Value, nil
	case *ast.RealLiteral:
		return v.Value, nil
	case *ast.StringLiteral:
		return v.Value, nil
	case *ast.BooleanLiteral:
		return v.Value, nil
	case *ast.UndefinedLiteral:
		return nil, nil
	case *ast.ListLiteral:
		list := make([]interface{}, len(v.Elements))
		for i, elem := range v.Elements {
			val, err := c.marshalValue(elem)
			if err != nil {
				return nil, err
			}
			list[i] = val
		}
		return list, nil
	case *ast.RecordLiteral:
		// Nested ClassAd: serialize deterministically and embed as raw JSON.
		nested := &ClassAd{ad: v.ClassAd, attrsDirty: true}
		nested.rebuildIndex()
		nestedBytes, err := nested.MarshalJSON()
		if err != nil {
			return nil, err
		}
		return json.RawMessage(nestedBytes), nil
	default:
		// Complex expression - serialize as string with special markers
		// Format: /Expr(<expression>)/
		exprStr := expr.String()
		return fmt.Sprintf("/Expr(%s)/", exprStr), nil
	}
}

// UnmarshalJSON implements the json.Unmarshaler interface for ClassAd.
// Deserializes JSON into a ClassAd, handling the special expression format.
// Strings matching the pattern "/Expr(<expression>)/" are parsed as expressions.
// Note: When unmarshaling from JSON, the Go json package automatically unescapes
// "\/Expr(...)\/" to "/Expr(...)/" so only the unescaped format is checked.
//
// Example:
//
//	jsonStr := `{"name":"test","x":5,"y":"\/Expr(x + 3)\/"}`
//	var ad ClassAd
//	json.Unmarshal([]byte(jsonStr), &ad)
func (c *ClassAd) UnmarshalJSON(data []byte) error {
	var raw map[string]interface{}
	if err := json.Unmarshal(data, &raw); err != nil {
		return err
	}

	attributes := make([]*ast.AttributeAssignment, 0, len(raw))
	for name, value := range raw {
		expr, err := c.unmarshalValue(value)
		if err != nil {
			return fmt.Errorf("failed to unmarshal attribute %s: %w", name, err)
		}
		attributes = append(attributes, &ast.AttributeAssignment{
			Name:  name,
			Value: expr,
		})
	}
	sortAttributeAssignments(attributes)

	c.ad = &ast.ClassAd{Attributes: attributes}
	c.attrsDirty = true
	c.rebuildIndex()
	return nil
}

// sortAttributeAssignments provides deterministic ordering by case-insensitive name with
// a secondary case-sensitive tie-breaker to preserve stable behavior.
func sortAttributeAssignments(attrs []*ast.AttributeAssignment) {
	sort.SliceStable(attrs, func(i, j int) bool {
		iName := normalizeName(attrs[i].Name)
		jName := normalizeName(attrs[j].Name)
		if iName == jName {
			return attrs[i].Name < attrs[j].Name
		}
		return iName < jName
	})
}

// unmarshalValue converts a JSON value back into an AST expression.
func (c *ClassAd) unmarshalValue(value interface{}) (ast.Expr, error) {
	switch v := value.(type) {
	case nil:
		return &ast.UndefinedLiteral{}, nil
	case bool:
		return &ast.BooleanLiteral{Value: v}, nil
	case float64:
		// JSON numbers are always float64
		// Check if it's actually an integer
		if v == float64(int64(v)) {
			return &ast.IntegerLiteral{Value: int64(v)}, nil
		}
		return &ast.RealLiteral{Value: v}, nil
	case string:
		// Check if it's an expression string.
		if strings.HasPrefix(v, "/Expr(") && strings.HasSuffix(v, ")/") {
			exprStr := v[6 : len(v)-2] // Remove "/Expr(" and ")/"
			return c.parseExpression(exprStr)
		}
		// Regular string literal
		return &ast.StringLiteral{Value: v}, nil
	case []interface{}:
		// JSON array -> ListLiteral
		elements := make([]ast.Expr, len(v))
		for i, elem := range v {
			expr, err := c.unmarshalValue(elem)
			if err != nil {
				return nil, err
			}
			elements[i] = expr
		}
		return &ast.ListLiteral{Elements: elements}, nil
	case map[string]interface{}:
		// JSON object -> RecordLiteral (nested ClassAd)
		attributes := make([]*ast.AttributeAssignment, 0, len(v))
		for name, val := range v {
			expr, err := c.unmarshalValue(val)
			if err != nil {
				return nil, err
			}
			attributes = append(attributes, &ast.AttributeAssignment{
				Name:  name,
				Value: expr,
			})
		}
		sortAttributeAssignments(attributes)
		return &ast.RecordLiteral{
			ClassAd: &ast.ClassAd{Attributes: attributes},
		}, nil
	default:
		return nil, fmt.Errorf("unsupported JSON value type: %T", value)
	}
}

// parseExpression parses an expression string into an AST expression.
func (c *ClassAd) parseExpression(exprStr string) (ast.Expr, error) {
	// Wrap in a temporary ClassAd for parsing
	wrapped := fmt.Sprintf("[__tmp__ = %s]", exprStr)
	node, err := parser.Parse(wrapped)
	if err != nil {
		return nil, fmt.Errorf("failed to parse expression %q: %w", exprStr, err)
	}

	if ad, ok := node.(*ast.ClassAd); ok && len(ad.Attributes) == 1 {
		return ad.Attributes[0].Value, nil
	}

	return nil, fmt.Errorf("unable to extract expression from parsed result")
}
