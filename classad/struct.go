package classad

import (
	"fmt"
	"reflect"
	"strings"

	"github.com/PelicanPlatform/classad/ast"
)

// Marshal converts a Go value to a ClassAd string representation.
// It works similarly to encoding/json.Marshal but produces ClassAd format.
// Struct fields can use "classad" struct tags to control marshaling behavior.
// If no "classad" tag is present, it falls back to the "json" tag.
//
// Supported struct tag options:
//   - Field name: classad:"custom_name" or json:"custom_name"
//   - Omit field: classad:"-" or json:"-"
//   - Omit if empty: classad:"name,omitempty" or json:"name,omitempty"
//
// Example:
//
//	type Job struct {
//	    ID       int    `classad:"JobId"`
//	    Name     string `classad:"Name"`
//	    CPUs     int    `json:"cpus"`  // falls back to json tag
//	    Priority int    // uses field name "Priority"
//	}
//	job := Job{ID: 123, Name: "test", CPUs: 4, Priority: 10}
//	classadStr, err := classad.Marshal(job)
//	// Result: [JobId = 123; Name = "test"; cpus = 4; Priority = 10]
func Marshal(v interface{}) (string, error) {
	val := reflect.ValueOf(v)
	expr, err := marshalValue(val)
	if err != nil {
		return "", err
	}

	// If it's a ClassAd (struct), format it properly
	if record, ok := expr.(*ast.RecordLiteral); ok {
		ad := &ClassAd{ad: record.ClassAd}
		return ad.String(), nil
	}

	// Otherwise, just return the expression string
	return expr.String(), nil
}

// Unmarshal parses a ClassAd string and stores the result in the value pointed to by v.
// It works similarly to encoding/json.Unmarshal but expects ClassAd format.
// Struct fields can use "classad" struct tags to control unmarshaling behavior.
// If no "classad" tag is present, it falls back to the "json" tag.
//
// Example:
//
//	type Job struct {
//	    ID   int    `classad:"JobId"`
//	    Name string `classad:"Name"`
//	}
//	var job Job
//	err := classad.Unmarshal("[JobId = 123; Name = \"test\"]", &job)
func Unmarshal(data string, v interface{}) error {
	// Parse the ClassAd string
	ad, err := Parse(data)
	if err != nil {
		return err
	}

	// Unmarshal into the provided value
	return unmarshalInto(ad, v)
}

// marshalValue converts a Go reflect.Value to an AST expression
func marshalValue(val reflect.Value) (ast.Expr, error) {
	// Handle pointers - check for special types first
	if val.Kind() == reflect.Ptr {
		if val.IsNil() {
			return &ast.UndefinedLiteral{}, nil
		}
		// Check if it's a *ClassAd or *Expr type
		if classad, ok := val.Interface().(*ClassAd); ok {
			if classad == nil || classad.ad == nil {
				return &ast.UndefinedLiteral{}, nil
			}
			return &ast.RecordLiteral{ClassAd: classad.ad}, nil
		}
		if expr, ok := val.Interface().(*Expr); ok {
			if expr == nil || expr.expr == nil {
				return &ast.UndefinedLiteral{}, nil
			}
			return expr.expr, nil
		}
		val = val.Elem()
	}

	// Handle interfaces
	if val.Kind() == reflect.Interface {
		if val.IsNil() {
			return &ast.UndefinedLiteral{}, nil
		}
		val = val.Elem()
	}

	switch val.Kind() {
	case reflect.Bool:
		return &ast.BooleanLiteral{Value: val.Bool()}, nil

	case reflect.Int, reflect.Int8, reflect.Int16, reflect.Int32, reflect.Int64:
		return &ast.IntegerLiteral{Value: val.Int()}, nil

	case reflect.Uint, reflect.Uint8, reflect.Uint16, reflect.Uint32, reflect.Uint64:
		return &ast.IntegerLiteral{Value: int64(val.Uint())}, nil

	case reflect.Float32, reflect.Float64:
		return &ast.RealLiteral{Value: val.Float()}, nil

	case reflect.String:
		return &ast.StringLiteral{Value: val.String()}, nil

	case reflect.Slice, reflect.Array:
		elements := make([]ast.Expr, val.Len())
		for i := 0; i < val.Len(); i++ {
			elem, err := marshalValue(val.Index(i))
			if err != nil {
				return nil, fmt.Errorf("slice element %d: %w", i, err)
			}
			elements[i] = elem
		}
		return &ast.ListLiteral{Elements: elements}, nil

	case reflect.Map:
		if val.Type().Key().Kind() != reflect.String {
			return nil, fmt.Errorf("maps must have string keys, got %v", val.Type().Key().Kind())
		}
		attributes := make([]*ast.AttributeAssignment, 0, val.Len())
		iter := val.MapRange()
		for iter.Next() {
			key := iter.Key().String()
			value, err := marshalValue(iter.Value())
			if err != nil {
				return nil, fmt.Errorf("map key %q: %w", key, err)
			}
			attributes = append(attributes, &ast.AttributeAssignment{
				Name:  key,
				Value: value,
			})
		}
		return &ast.RecordLiteral{ClassAd: &ast.ClassAd{Attributes: attributes}}, nil

	case reflect.Struct:
		return marshalStruct(val)

	default:
		return nil, fmt.Errorf("unsupported type: %v", val.Type())
	}
}

// marshalStruct converts a struct to a ClassAd record
func marshalStruct(val reflect.Value) (ast.Expr, error) {
	typ := val.Type()
	attributes := make([]*ast.AttributeAssignment, 0)

	for i := 0; i < typ.NumField(); i++ {
		field := typ.Field(i)
		fieldVal := val.Field(i)

		// Skip unexported fields
		if !field.IsExported() {
			continue
		}

		// Get field name and options from struct tags
		name, opts := parseStructTag(field)
		if name == "-" {
			continue // Skip this field
		}

		// Handle omitempty
		if opts.omitEmpty && isEmptyValue(fieldVal) {
			continue
		}

		// Marshal the field value
		expr, err := marshalValue(fieldVal)
		if err != nil {
			return nil, fmt.Errorf("field %s: %w", field.Name, err)
		}

		attributes = append(attributes, &ast.AttributeAssignment{
			Name:  name,
			Value: expr,
		})
	}

	return &ast.RecordLiteral{ClassAd: &ast.ClassAd{Attributes: attributes}}, nil
}

// unmarshalInto unmarshals a ClassAd into a Go value
func unmarshalInto(ad *ClassAd, v interface{}) error {
	val := reflect.ValueOf(v)
	if val.Kind() != reflect.Ptr {
		return fmt.Errorf("unmarshal target must be a pointer, got %v", val.Type())
	}
	if val.IsNil() {
		return fmt.Errorf("unmarshal target cannot be nil")
	}

	return unmarshalValue(ad.ad, val.Elem())
}

// unmarshalValue unmarshals an AST ClassAd into a reflect.Value
func unmarshalValue(node *ast.ClassAd, val reflect.Value) error {
	if node == nil {
		return nil
	}

	// Handle pointers
	if val.Kind() == reflect.Ptr {
		if val.IsNil() {
			val.Set(reflect.New(val.Type().Elem()))
		}
		val = val.Elem()
	}

	switch val.Kind() {
	case reflect.Struct:
		return unmarshalStruct(node, val)

	case reflect.Map:
		if val.Type().Key().Kind() != reflect.String {
			return fmt.Errorf("maps must have string keys")
		}
		if val.IsNil() {
			val.Set(reflect.MakeMap(val.Type()))
		}
		return unmarshalMap(node, val)

	default:
		return fmt.Errorf("can only unmarshal ClassAd into struct or map, got %v", val.Type())
	}
}

// unmarshalStruct unmarshals a ClassAd into a struct
func unmarshalStruct(node *ast.ClassAd, val reflect.Value) error {
	typ := val.Type()

	// Create a map of classad names to struct field indices
	fieldMap := make(map[string]int)
	for i := 0; i < typ.NumField(); i++ {
		field := typ.Field(i)
		if !field.IsExported() {
			continue
		}
		name, _ := parseStructTag(field)
		if name != "-" {
			fieldMap[name] = i
		}
	}

	// Iterate over ClassAd attributes
	for _, attr := range node.Attributes {
		fieldIdx, ok := fieldMap[attr.Name]
		if !ok {
			// Ignore unknown fields
			continue
		}

		field := typ.Field(fieldIdx)
		fieldVal := val.Field(fieldIdx)

		// Special handling for *Expr fields - don't evaluate
		if field.Type == reflect.TypeOf((*Expr)(nil)) {
			expr := &Expr{expr: attr.Value}
			fieldVal.Set(reflect.ValueOf(expr))
			continue
		}

		// Evaluate the attribute in the context of the ClassAd
		ad := &ClassAd{ad: node}
		result := ad.EvaluateAttr(attr.Name)

		// Unmarshal the result into the field
		if err := unmarshalValueInto(result, fieldVal); err != nil {
			return fmt.Errorf("field %s: %w", field.Name, err)
		}
	}

	return nil
}

// unmarshalMap unmarshals a ClassAd into a map[string]T
func unmarshalMap(node *ast.ClassAd, val reflect.Value) error {
	ad := &ClassAd{ad: node}
	elemType := val.Type().Elem()

	for _, attr := range node.Attributes {
		result := ad.EvaluateAttr(attr.Name)

		// Create a new element of the appropriate type
		elemVal := reflect.New(elemType).Elem()

		// Unmarshal into the element
		if err := unmarshalValueInto(result, elemVal); err != nil {
			return fmt.Errorf("attribute %s: %w", attr.Name, err)
		}

		// Set in map
		val.SetMapIndex(reflect.ValueOf(attr.Name), elemVal)
	}

	return nil
}

// unmarshalValueInto unmarshals a Value into a reflect.Value
func unmarshalValueInto(result Value, val reflect.Value) error {
	// Handle pointers - check for special types first
	if val.Kind() == reflect.Ptr {
		// Check if target is *ClassAd
		if val.Type() == reflect.TypeOf((*ClassAd)(nil)) {
			if result.IsClassAd() {
				nestedAd, err := result.ClassAdValue()
				if err != nil {
					return fmt.Errorf("failed to get ClassAd value: %w", err)
				}
				val.Set(reflect.ValueOf(nestedAd))
				return nil
			}
			return fmt.Errorf("expected ClassAd, got %v", result.Type())
		}
		// Check if target is *Expr
		if val.Type() == reflect.TypeOf((*Expr)(nil)) {
			// *Expr fields are handled in unmarshalStruct to preserve the unevaluated expression
			return fmt.Errorf("*Expr fields should be handled in unmarshalStruct")
		}

		if val.IsNil() {
			val.Set(reflect.New(val.Type().Elem()))
		}
		val = val.Elem()
	}

	switch val.Kind() {
	case reflect.Bool:
		if result.IsBool() {
			v, err := result.BoolValue()
			if err != nil {
				return fmt.Errorf("failed to get bool value: %w", err)
			}
			val.SetBool(v)
		} else {
			return fmt.Errorf("expected bool, got %v", result.Type())
		}

	case reflect.Int, reflect.Int8, reflect.Int16, reflect.Int32, reflect.Int64:
		if result.IsInteger() {
			v, err := result.IntValue()
			if err != nil {
				return fmt.Errorf("failed to get int value: %w", err)
			}
			val.SetInt(v)
		} else if result.IsReal() {
			v, err := result.RealValue()
			if err != nil {
				return fmt.Errorf("failed to get real value: %w", err)
			}
			val.SetInt(int64(v))
		} else {
			return fmt.Errorf("expected integer, got %v", result.Type())
		}

	case reflect.Uint, reflect.Uint8, reflect.Uint16, reflect.Uint32, reflect.Uint64:
		if result.IsInteger() {
			v, err := result.IntValue()
			if err != nil {
				return fmt.Errorf("failed to get int value: %w", err)
			}
			val.SetUint(uint64(v))
		} else if result.IsReal() {
			v, err := result.RealValue()
			if err != nil {
				return fmt.Errorf("failed to get real value: %w", err)
			}
			val.SetUint(uint64(v))
		} else {
			return fmt.Errorf("expected integer, got %v", result.Type())
		}

	case reflect.Float32, reflect.Float64:
		if result.IsReal() {
			v, err := result.RealValue()
			if err != nil {
				return fmt.Errorf("failed to get real value: %w", err)
			}
			val.SetFloat(v)
		} else if result.IsInteger() {
			v, err := result.IntValue()
			if err != nil {
				return fmt.Errorf("failed to get int value: %w", err)
			}
			val.SetFloat(float64(v))
		} else {
			return fmt.Errorf("expected real, got %v", result.Type())
		}

	case reflect.String:
		if result.IsString() {
			v, err := result.StringValue()
			if err != nil {
				return fmt.Errorf("failed to get string value: %w", err)
			}
			val.SetString(v)
		} else {
			return fmt.Errorf("expected string, got %v", result.Type())
		}

	case reflect.Slice:
		if result.IsList() {
			listVals, err := result.ListValue()
			if err != nil {
				return fmt.Errorf("failed to get list value: %w", err)
			}
			slice := reflect.MakeSlice(val.Type(), len(listVals), len(listVals))
			for i, item := range listVals {
				if err := unmarshalValueInto(item, slice.Index(i)); err != nil {
					return fmt.Errorf("list element %d: %w", i, err)
				}
			}
			val.Set(slice)
		} else {
			return fmt.Errorf("expected list, got %v", result.Type())
		}

	case reflect.Struct:
		if result.IsClassAd() {
			nestedAd, err := result.ClassAdValue()
			if err != nil {
				return fmt.Errorf("failed to get ClassAd value: %w", err)
			}
			return unmarshalStruct(nestedAd.ad, val)
		} else {
			return fmt.Errorf("expected ClassAd, got %v", result.Type())
		}

	case reflect.Map:
		if result.IsClassAd() {
			nestedAd, err := result.ClassAdValue()
			if err != nil {
				return fmt.Errorf("failed to get ClassAd value: %w", err)
			}
			if val.IsNil() {
				val.Set(reflect.MakeMap(val.Type()))
			}
			return unmarshalMap(nestedAd.ad, val)
		} else {
			return fmt.Errorf("expected ClassAd, got %v", result.Type())
		}

	case reflect.Interface:
		// For interface{}, convert to a concrete Go type
		if val.Type().NumMethod() == 0 { // empty interface
			concreteVal := valueToInterface(result)
			val.Set(reflect.ValueOf(concreteVal))
		} else {
			return fmt.Errorf("cannot unmarshal into non-empty interface")
		}

	default:
		return fmt.Errorf("unsupported type: %v", val.Type())
	}

	return nil
}

// valueToInterface converts a Value to a Go interface{} type
func valueToInterface(result Value) interface{} {
	if result.IsUndefined() {
		return nil
	}
	if result.IsBool() {
		v, err := result.BoolValue()
		if err != nil {
			return nil
		}
		return v
	}
	if result.IsInteger() {
		v, err := result.IntValue()
		if err != nil {
			return nil
		}
		return v
	}
	if result.IsReal() {
		v, err := result.RealValue()
		if err != nil {
			return nil
		}
		return v
	}
	if result.IsString() {
		v, err := result.StringValue()
		if err != nil {
			return nil
		}
		return v
	}
	if result.IsList() {
		listVals, err := result.ListValue()
		if err != nil {
			return nil
		}
		slice := make([]interface{}, len(listVals))
		for i, item := range listVals {
			slice[i] = valueToInterface(item)
		}
		return slice
	}
	if result.IsClassAd() {
		nestedAd, err := result.ClassAdValue()
		if err != nil {
			return nil
		}
		m := make(map[string]interface{})
		for _, attr := range nestedAd.ad.Attributes {
			attrVal := nestedAd.EvaluateAttr(attr.Name)
			m[attr.Name] = valueToInterface(attrVal)
		}
		return m
	}
	return nil
}

// tagOptions represents parsed struct tag options
type tagOptions struct {
	omitEmpty bool
}

// parseStructTag parses a struct field's tags to determine the ClassAd field name and options
func parseStructTag(field reflect.StructField) (string, tagOptions) {
	var opts tagOptions

	// Try classad tag first
	if tag, ok := field.Tag.Lookup("classad"); ok {
		return parseTag(tag, field.Name, &opts)
	}

	// Fall back to json tag
	if tag, ok := field.Tag.Lookup("json"); ok {
		return parseTag(tag, field.Name, &opts)
	}

	// Use field name as-is
	return field.Name, opts
}

// parseTag parses a tag value into name and options
func parseTag(tag, defaultName string, opts *tagOptions) (string, tagOptions) {
	parts := strings.Split(tag, ",")
	name := parts[0]

	// Handle empty name
	if name == "" {
		name = defaultName
	}

	// Parse options
	for i := 1; i < len(parts); i++ {
		if parts[i] == "omitempty" {
			opts.omitEmpty = true
		}
	}

	return name, *opts
}

// isEmptyValue checks if a value is empty (for omitempty)
func isEmptyValue(v reflect.Value) bool {
	switch v.Kind() {
	case reflect.Array, reflect.Map, reflect.Slice, reflect.String:
		return v.Len() == 0
	case reflect.Bool:
		return !v.Bool()
	case reflect.Int, reflect.Int8, reflect.Int16, reflect.Int32, reflect.Int64:
		return v.Int() == 0
	case reflect.Uint, reflect.Uint8, reflect.Uint16, reflect.Uint32, reflect.Uint64:
		return v.Uint() == 0
	case reflect.Float32, reflect.Float64:
		return v.Float() == 0
	case reflect.Interface, reflect.Ptr:
		return v.IsNil()
	}
	return false
}

// MarshalClassAd is an alias for Marshal for clarity
func MarshalClassAd(v interface{}) (string, error) {
	return Marshal(v)
}

// UnmarshalClassAd is an alias for Unmarshal for clarity
func UnmarshalClassAd(data string, v interface{}) error {
	return Unmarshal(data, v)
}

// Marshaler is the interface implemented by types that can marshal themselves into ClassAd format.
// This is similar to json.Marshaler.
type Marshaler interface {
	MarshalClassAd() (string, error)
}

// Unmarshaler is the interface implemented by types that can unmarshal a ClassAd representation of themselves.
// This is similar to json.Unmarshaler.
type Unmarshaler interface {
	UnmarshalClassAd(data string) error
}
