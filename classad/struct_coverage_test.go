package classad

import (
	"reflect"
	"testing"
)

// Test unmarshalValue with map target
func TestUnmarshalToMap(t *testing.T) {
	classadStr := `[
		Name = "test";
		Value = 42;
		Active = true;
		Score = 3.14
	]`

	var result map[string]interface{}
	err := Unmarshal(classadStr, &result)
	if err != nil {
		t.Fatalf("Unmarshal to map failed: %v", err)
	}

	if result["Name"] != "test" {
		t.Errorf("Name: expected 'test', got %v", result["Name"])
	}

	if result["Value"] != int64(42) {
		t.Errorf("Value: expected 42, got %v", result["Value"])
	}

	if result["Active"] != true {
		t.Errorf("Active: expected true, got %v", result["Active"])
	}
}

// Test unmarshalValue with pointer to struct
func TestUnmarshalToPointerStruct(t *testing.T) {
	type TestStruct struct {
		Name  string
		Value int
	}

	classadStr := `[Name = "test"; Value = 42]`

	var result *TestStruct
	err := Unmarshal(classadStr, &result)
	if err != nil {
		t.Fatalf("Unmarshal to pointer struct failed: %v", err)
	}

	if result == nil {
		t.Fatal("Expected non-nil result")
	}

	if result.Name != "test" {
		t.Errorf("Name: expected 'test', got %s", result.Name)
	}

	if result.Value != 42 {
		t.Errorf("Value: expected 42, got %d", result.Value)
	}
}

// Test unmarshalValueInto with various types
func TestUnmarshalValueIntoTypes(t *testing.T) {
	ad := New()

	// Test with bool
	ad.InsertAttrBool("BoolVal", true)
	val := ad.EvaluateAttr("BoolVal")

	var b bool
	err := unmarshalValueInto(val, valueRef(&b))
	if err != nil {
		t.Errorf("unmarshalValueInto bool failed: %v", err)
	}
	if !b {
		t.Error("Expected true")
	}

	// Test with int
	ad.InsertAttr("IntVal", 42)
	val = ad.EvaluateAttr("IntVal")

	var i int
	err = unmarshalValueInto(val, valueRef(&i))
	if err != nil {
		t.Errorf("unmarshalValueInto int failed: %v", err)
	}
	if i != 42 {
		t.Errorf("Expected 42, got %d", i)
	}

	// Test with float
	ad.InsertAttrFloat("FloatVal", 3.14)
	val = ad.EvaluateAttr("FloatVal")

	var f float64
	err = unmarshalValueInto(val, valueRef(&f))
	if err != nil {
		t.Errorf("unmarshalValueInto float failed: %v", err)
	}
	if f != 3.14 {
		t.Errorf("Expected 3.14, got %f", f)
	}

	// Test with string
	ad.InsertAttrString("StringVal", "hello")
	val = ad.EvaluateAttr("StringVal")

	var s string
	err = unmarshalValueInto(val, valueRef(&s))
	if err != nil {
		t.Errorf("unmarshalValueInto string failed: %v", err)
	}
	if s != "hello" {
		t.Errorf("Expected 'hello', got %q", s)
	}

	// Test with list
	InsertAttrList(ad, "ListVal", []int64{1, 2, 3})
	val = ad.EvaluateAttr("ListVal")

	var list []int
	err = unmarshalValueInto(val, valueRef(&list))
	if err != nil {
		t.Errorf("unmarshalValueInto list failed: %v", err)
	}
	if len(list) != 3 {
		t.Errorf("Expected list length 3, got %d", len(list))
	}

	// Test with nested ClassAd
	nested := New()
	nested.InsertAttr("X", 10)
	ad.InsertAttrClassAd("NestedVal", nested)
	val = ad.EvaluateAttr("NestedVal")

	var nestedPtr *ClassAd
	err = unmarshalValueInto(val, valueRef(&nestedPtr))
	if err != nil {
		t.Errorf("unmarshalValueInto ClassAd failed: %v", err)
	}
	if nestedPtr == nil {
		t.Error("Expected non-nil ClassAd")
	}
}

// Helper function to get reflect.Value
func valueRef(v interface{}) reflect.Value {
	return reflect.ValueOf(v).Elem()
}

// Test valueToInterface function
func TestValueToInterface(t *testing.T) {
	ad := New()

	// Test undefined value
	val := NewUndefinedValue()
	result := valueToInterface(val)
	if result != nil {
		t.Errorf("Expected nil for undefined, got %v", result)
	}

	// Test bool value
	ad.InsertAttrBool("Bool", true)
	val = ad.EvaluateAttr("Bool")
	result = valueToInterface(val)
	if result != true {
		t.Errorf("Expected true, got %v", result)
	}

	// Test int value
	ad.InsertAttr("Int", 42)
	val = ad.EvaluateAttr("Int")
	result = valueToInterface(val)
	if result != int64(42) {
		t.Errorf("Expected 42, got %v", result)
	}

	// Test float value
	ad.InsertAttrFloat("Float", 3.14)
	val = ad.EvaluateAttr("Float")
	result = valueToInterface(val)
	if result != 3.14 {
		t.Errorf("Expected 3.14, got %v", result)
	}

	// Test string value
	ad.InsertAttrString("String", "hello")
	val = ad.EvaluateAttr("String")
	result = valueToInterface(val)
	if result != "hello" {
		t.Errorf("Expected 'hello', got %v", result)
	}

	// Test list value
	InsertAttrList(ad, "List", []int64{1, 2, 3})
	val = ad.EvaluateAttr("List")
	result = valueToInterface(val)
	list, ok := result.([]interface{})
	if !ok {
		t.Errorf("Expected []interface{}, got %T", result)
	} else if len(list) != 3 {
		t.Errorf("Expected list length 3, got %d", len(list))
	}

	// Test nested ClassAd
	nested := New()
	nested.InsertAttr("X", 10)
	nested.InsertAttrString("Y", "test")
	ad.InsertAttrClassAd("Nested", nested)
	val = ad.EvaluateAttr("Nested")
	result = valueToInterface(val)
	m, ok := result.(map[string]interface{})
	if !ok {
		t.Errorf("Expected map[string]interface{}, got %T", result)
	} else {
		if m["X"] != int64(10) {
			t.Errorf("Expected X=10, got %v", m["X"])
		}
		if m["Y"] != "test" {
			t.Errorf("Expected Y='test', got %v", m["Y"])
		}
	}
}

// Test unmarshal with nested structures
func TestUnmarshalNestedStructures(t *testing.T) {
	type Inner struct {
		Value int
		Name  string
	}

	type Outer struct {
		ID    int
		Inner Inner
	}

	classadStr := `[ID = 1; Inner = [Value = 42; Name = "test"]]`

	var result Outer
	err := Unmarshal(classadStr, &result)
	if err != nil {
		t.Fatalf("Unmarshal nested failed: %v", err)
	}

	if result.ID != 1 {
		t.Errorf("ID: expected 1, got %d", result.ID)
	}

	if result.Inner.Value != 42 {
		t.Errorf("Inner.Value: expected 42, got %d", result.Inner.Value)
	}

	if result.Inner.Name != "test" {
		t.Errorf("Inner.Name: expected 'test', got %s", result.Inner.Name)
	}
}

// Test unmarshal with slices of different types
func TestUnmarshalSliceTypes(t *testing.T) {
	type TestStruct struct {
		IntList    []int
		StringList []string
		BoolList   []bool
		FloatList  []float64
	}

	classadStr := `[
		IntList = {1, 2, 3};
		StringList = {"a", "b", "c"};
		BoolList = {true, false, true};
		FloatList = {1.1, 2.2, 3.3}
	]`

	var result TestStruct
	err := Unmarshal(classadStr, &result)
	if err != nil {
		t.Fatalf("Unmarshal slices failed: %v", err)
	}

	if len(result.IntList) != 3 || result.IntList[0] != 1 {
		t.Errorf("IntList: expected [1 2 3], got %v", result.IntList)
	}

	if len(result.StringList) != 3 || result.StringList[0] != "a" {
		t.Errorf("StringList: expected [a b c], got %v", result.StringList)
	}

	if len(result.BoolList) != 3 || !result.BoolList[0] {
		t.Errorf("BoolList: expected [true false true], got %v", result.BoolList)
	}

	if len(result.FloatList) != 3 {
		t.Errorf("FloatList: expected length 3, got %d", len(result.FloatList))
	}
}

// Test marshal with pointers and nil values
func TestMarshalPointers(t *testing.T) {
	type TestStruct struct {
		IntPtr    *int
		StringPtr *string
	}

	intVal := 42
	strVal := "hello"

	s := TestStruct{
		IntPtr:    &intVal,
		StringPtr: &strVal,
	}

	result, err := Marshal(s)
	if err != nil {
		t.Fatalf("Marshal pointers failed: %v", err)
	}

	if result == "" {
		t.Error("Expected non-empty result")
	}

	// Parse it back
	var s2 TestStruct
	err = Unmarshal(result, &s2)
	if err != nil {
		t.Fatalf("Unmarshal pointers failed: %v", err)
	}

	if s2.IntPtr == nil || *s2.IntPtr != 42 {
		t.Errorf("IntPtr: expected 42, got %v", s2.IntPtr)
	}

	if s2.StringPtr == nil || *s2.StringPtr != "hello" {
		t.Errorf("StringPtr: expected 'hello', got %v", s2.StringPtr)
	}
}

// Test unmarshal with map of different value types
func TestUnmarshalMapTypes(t *testing.T) {
	classadStr := `[
		A = 1;
		B = "text";
		C = 3.14;
		D = true
	]`

	var result map[string]interface{}
	err := Unmarshal(classadStr, &result)
	if err != nil {
		t.Fatalf("Unmarshal to map failed: %v", err)
	}

	if result["A"] != int64(1) {
		t.Errorf("A: expected 1, got %v (%T)", result["A"], result["A"])
	}

	if result["B"] != "text" {
		t.Errorf("B: expected 'text', got %v", result["B"])
	}

	if result["C"] != 3.14 {
		t.Errorf("C: expected 3.14, got %v", result["C"])
	}

	if result["D"] != true {
		t.Errorf("D: expected true, got %v", result["D"])
	}
}

// Test error handling in unmarshal
func TestUnmarshalErrors(t *testing.T) {
	// Test unmarshal to non-struct/non-map
	var i int
	err := Unmarshal("[A = 1]", &i)
	if err == nil {
		t.Error("Expected error unmarshaling to int")
	}

	// Test unmarshal with invalid ClassAd string
	type TestStruct struct {
		Value int
	}
	var s TestStruct
	err = Unmarshal("[A =]", &s)
	if err == nil {
		t.Error("Expected error with invalid ClassAd")
	}

	// Test unmarshal map with non-string keys (not directly testable through Unmarshal)
	// This path is tested through internal function coverage
}

// Test marshal with complex nested structures
func TestMarshalComplexNested(t *testing.T) {
	type Address struct {
		Street string
		City   string
		Zip    int
	}

	type Person struct {
		Name    string
		Age     int
		Address Address
		Tags    []string
	}

	p := Person{
		Name: "Alice",
		Age:  30,
		Address: Address{
			Street: "123 Main St",
			City:   "Boston",
			Zip:    0o2101,
		},
		Tags: []string{"developer", "golang"},
	}

	classadStr, err := Marshal(p)
	if err != nil {
		t.Fatalf("Marshal complex nested failed: %v", err)
	}

	// Unmarshal it back
	var p2 Person
	err = Unmarshal(classadStr, &p2)
	if err != nil {
		t.Fatalf("Unmarshal complex nested failed: %v", err)
	}

	if p2.Name != p.Name {
		t.Errorf("Name mismatch: expected %s, got %s", p.Name, p2.Name)
	}

	if p2.Address.City != p.Address.City {
		t.Errorf("City mismatch: expected %s, got %s", p.Address.City, p2.Address.City)
	}

	if len(p2.Tags) != len(p.Tags) {
		t.Errorf("Tags length mismatch: expected %d, got %d", len(p.Tags), len(p2.Tags))
	}
}

// Test all integer types in unmarshal
func TestUnmarshalIntegerTypes(t *testing.T) {
	type IntTypes struct {
		Int8   int8
		Int16  int16
		Int32  int32
		Int64  int64
		Uint8  uint8
		Uint16 uint16
		Uint32 uint32
	}

	classadStr := `[
		Int8 = 127;
		Int16 = 32767;
		Int32 = 2147483647;
		Int64 = 9223372036854775807;
		Uint8 = 255;
		Uint16 = 65535;
		Uint32 = 4294967295
	]`

	var result IntTypes
	err := Unmarshal(classadStr, &result)
	if err != nil {
		t.Fatalf("Unmarshal integer types failed: %v", err)
	}

	if result.Int8 != 127 {
		t.Errorf("Int8: expected 127, got %d", result.Int8)
	}

	if result.Uint8 != 255 {
		t.Errorf("Uint8: expected 255, got %d", result.Uint8)
	}

	if result.Int64 != 9223372036854775807 {
		t.Errorf("Int64: expected max value, got %d", result.Int64)
	}
}
