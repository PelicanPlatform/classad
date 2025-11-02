package classad

import (
	"testing"
)

// Tests for the generic Set(), GetAs(), and GetOr() API

func TestSet_BasicTypes(t *testing.T) {
	ad := New()

	// Test integer types
	if err := ad.Set("int", 42); err != nil {
		t.Fatalf("Set int failed: %v", err)
	}
	if err := ad.Set("int64", int64(100)); err != nil {
		t.Fatalf("Set int64 failed: %v", err)
	}
	if err := ad.Set("uint", uint(50)); err != nil {
		t.Fatalf("Set uint failed: %v", err)
	}

	// Test float
	if err := ad.Set("float", 3.14); err != nil {
		t.Fatalf("Set float failed: %v", err)
	}

	// Test string
	if err := ad.Set("name", "test"); err != nil {
		t.Fatalf("Set string failed: %v", err)
	}

	// Test bool
	if err := ad.Set("enabled", true); err != nil {
		t.Fatalf("Set bool failed: %v", err)
	}

	// Verify values
	if val, ok := ad.EvaluateAttrInt("int"); !ok || val != 42 {
		t.Errorf("Expected int=42, got %d", val)
	}
	if val, ok := ad.EvaluateAttrInt("int64"); !ok || val != 100 {
		t.Errorf("Expected int64=100, got %d", val)
	}
	if val, ok := ad.EvaluateAttrReal("float"); !ok || val != 3.14 {
		t.Errorf("Expected float=3.14, got %f", val)
	}
	if val, ok := ad.EvaluateAttrString("name"); !ok || val != "test" {
		t.Errorf("Expected name='test', got %q", val)
	}
	if val, ok := ad.EvaluateAttrBool("enabled"); !ok || !val {
		t.Errorf("Expected enabled=true, got %v", val)
	}
}

func TestSet_Slices(t *testing.T) {
	ad := New()

	// Test string slice
	if err := ad.Set("tags", []string{"a", "b", "c"}); err != nil {
		t.Fatalf("Set string slice failed: %v", err)
	}

	// Test int slice
	if err := ad.Set("numbers", []int{1, 2, 3}); err != nil {
		t.Fatalf("Set int slice failed: %v", err)
	}

	// Verify slices
	tagsVal := ad.EvaluateAttr("tags")
	if !tagsVal.IsList() {
		t.Errorf("Expected tags to be a list")
	}

	numbersVal := ad.EvaluateAttr("numbers")
	if !numbersVal.IsList() {
		t.Errorf("Expected numbers to be a list")
	}
}

func TestSet_ClassAdAndExpr(t *testing.T) {
	ad := New()

	// Test *ClassAd
	nested := New()
	nested.InsertAttr("x", 10)
	nested.InsertAttrString("y", "hello")

	if err := ad.Set("config", nested); err != nil {
		t.Fatalf("Set *ClassAd failed: %v", err)
	}

	configVal := ad.EvaluateAttr("config")
	if !configVal.IsClassAd() {
		t.Errorf("Expected config to be a ClassAd")
	}

	// Test *Expr
	expr, _ := ParseExpr("x + 10")
	if err := ad.Set("formula", expr); err != nil {
		t.Fatalf("Set *Expr failed: %v", err)
	}

	formulaExpr, ok := ad.Lookup("formula")
	if !ok {
		t.Errorf("Expected formula attribute to exist")
	}
	if formulaExpr.String() != "(x + 10)" {
		t.Errorf("Expected formula='(x + 10)', got %q", formulaExpr.String())
	}
}

func TestSet_Nil(t *testing.T) {
	ad := New()

	if err := ad.Set("nil_value", nil); err != nil {
		t.Fatalf("Set nil failed: %v", err)
	}

	val := ad.EvaluateAttr("nil_value")
	if !val.IsUndefined() {
		t.Errorf("Expected nil_value to be undefined")
	}
}

func TestSet_Struct(t *testing.T) {
	ad := New()

	type Config struct {
		Timeout int
		Server  string
	}

	config := Config{Timeout: 30, Server: "example.com"}
	if err := ad.Set("config", config); err != nil {
		t.Fatalf("Set struct failed: %v", err)
	}

	configVal := ad.EvaluateAttr("config")
	if !configVal.IsClassAd() {
		t.Errorf("Expected config to be a ClassAd")
	}
}

func TestGetAs_BasicTypes(t *testing.T) {
	ad := New()
	ad.InsertAttr("int", 42)
	ad.InsertAttrFloat("float", 3.14)
	ad.InsertAttrString("name", "test")
	ad.InsertAttrBool("enabled", true)

	// Test int
	intVal, ok := GetAs[int](ad, "int")
	if !ok || intVal != 42 {
		t.Errorf("Expected int=42, got %d (ok=%v)", intVal, ok)
	}

	// Test int64
	int64Val, ok := GetAs[int64](ad, "int")
	if !ok || int64Val != 42 {
		t.Errorf("Expected int64=42, got %d (ok=%v)", int64Val, ok)
	}

	// Test float64
	floatVal, ok := GetAs[float64](ad, "float")
	if !ok || floatVal != 3.14 {
		t.Errorf("Expected float=3.14, got %f (ok=%v)", floatVal, ok)
	}

	// Test string
	strVal, ok := GetAs[string](ad, "name")
	if !ok || strVal != "test" {
		t.Errorf("Expected name='test', got %q (ok=%v)", strVal, ok)
	}

	// Test bool
	boolVal, ok := GetAs[bool](ad, "enabled")
	if !ok || !boolVal {
		t.Errorf("Expected enabled=true, got %v (ok=%v)", boolVal, ok)
	}
}

func TestGetAs_Slice(t *testing.T) {
	ad := New()
	InsertAttrList(ad, "tags", []string{"a", "b", "c"})
	InsertAttrList(ad, "numbers", []int64{1, 2, 3})

	// Test string slice
	tags, ok := GetAs[[]string](ad, "tags")
	if !ok {
		t.Fatal("Expected tags to be retrievable")
	}
	if len(tags) != 3 || tags[0] != "a" || tags[1] != "b" || tags[2] != "c" {
		t.Errorf("Expected tags=[a,b,c], got %v", tags)
	}

	// Test int slice
	numbers, ok := GetAs[[]int](ad, "numbers")
	if !ok {
		t.Fatal("Expected numbers to be retrievable")
	}
	if len(numbers) != 3 || numbers[0] != 1 || numbers[1] != 2 || numbers[2] != 3 {
		t.Errorf("Expected numbers=[1,2,3], got %v", numbers)
	}
}

func TestGetAs_ClassAd(t *testing.T) {
	ad := New()
	nested := New()
	nested.InsertAttr("x", 10)
	nested.InsertAttrString("y", "hello")
	ad.InsertAttrClassAd("config", nested)

	// Test *ClassAd retrieval
	config, ok := GetAs[*ClassAd](ad, "config")
	if !ok {
		t.Fatal("Expected config to be retrievable")
	}

	x, ok := config.EvaluateAttrInt("x")
	if !ok || x != 10 {
		t.Errorf("Expected x=10, got %d", x)
	}

	y, ok := config.EvaluateAttrString("y")
	if !ok || y != "hello" {
		t.Errorf("Expected y='hello', got %q", y)
	}
}

func TestGetAs_Expr(t *testing.T) {
	ad := New()
	expr, _ := ParseExpr("x + 10")
	ad.InsertExpr("formula", expr)

	// Test *Expr retrieval (should return unevaluated)
	formula, ok := GetAs[*Expr](ad, "formula")
	if !ok {
		t.Fatal("Expected formula to be retrievable")
	}

	if formula.String() != "(x + 10)" {
		t.Errorf("Expected formula='(x + 10)', got %q", formula.String())
	}
}

func TestGetAs_Missing(t *testing.T) {
	ad := New()

	// Test missing attribute
	val, ok := GetAs[int](ad, "missing")
	if ok {
		t.Errorf("Expected missing attribute to return false")
	}
	if val != 0 {
		t.Errorf("Expected zero value, got %d", val)
	}

	strVal, ok := GetAs[string](ad, "missing")
	if ok {
		t.Errorf("Expected missing attribute to return false")
	}
	if strVal != "" {
		t.Errorf("Expected empty string, got %q", strVal)
	}
}

func TestGetAs_TypeConversion(t *testing.T) {
	ad := New()
	ad.InsertAttr("value", 42)

	// Test int to float conversion
	floatVal, ok := GetAs[float64](ad, "value")
	if !ok || floatVal != 42.0 {
		t.Errorf("Expected float=42.0, got %f (ok=%v)", floatVal, ok)
	}

	// Test with real value to int conversion
	ad.InsertAttrFloat("real", 3.7)
	intVal, ok := GetAs[int](ad, "real")
	if !ok || intVal != 3 {
		t.Errorf("Expected int=3 (truncated), got %d (ok=%v)", intVal, ok)
	}
}

func TestGetAs_Struct(t *testing.T) {
	ad := New()

	// Create a nested ClassAd for config
	configAd := New()
	configAd.InsertAttr("timeout", 30)
	configAd.InsertAttrString("server", "example.com")
	ad.InsertAttrClassAd("config", configAd)

	type Config struct {
		Timeout int    `classad:"timeout"`
		Server  string `classad:"server"`
	}

	// Get the nested ClassAd and unmarshal it to a struct
	config, ok := GetAs[*ClassAd](ad, "config")
	if !ok {
		t.Fatal("Expected config ClassAd to be retrievable")
	}

	// Now unmarshal the ClassAd into a struct
	var configStruct Config
	err := Unmarshal(config.String(), &configStruct)
	if err != nil {
		t.Fatalf("Failed to unmarshal config: %v", err)
	}

	if configStruct.Timeout != 30 {
		t.Errorf("Expected timeout=30, got %d", configStruct.Timeout)
	}
	if configStruct.Server != "example.com" {
		t.Errorf("Expected server='example.com', got %q", configStruct.Server)
	}
}

func TestGetOr_WithDefault(t *testing.T) {
	ad := New()
	ad.InsertAttr("cpus", 4)
	ad.InsertAttrString("name", "job-1")

	// Test existing values
	cpus := GetOr(ad, "cpus", 1)
	if cpus != 4 {
		t.Errorf("Expected cpus=4, got %d", cpus)
	}

	name := GetOr(ad, "name", "unknown")
	if name != "job-1" {
		t.Errorf("Expected name='job-1', got %q", name)
	}

	// Test missing values (should return defaults)
	timeout := GetOr(ad, "timeout", 300)
	if timeout != 300 {
		t.Errorf("Expected timeout=300 (default), got %d", timeout)
	}

	owner := GetOr(ad, "owner", "unknown")
	if owner != "unknown" {
		t.Errorf("Expected owner='unknown' (default), got %q", owner)
	}

	enabled := GetOr(ad, "enabled", true)
	if !enabled {
		t.Errorf("Expected enabled=true (default), got %v", enabled)
	}
}

func TestGetOr_Slice(t *testing.T) {
	ad := New()
	InsertAttrList(ad, "tags", []string{"a", "b"})

	// Test existing slice
	tags := GetOr(ad, "tags", []string{"default"})
	if len(tags) != 2 || tags[0] != "a" || tags[1] != "b" {
		t.Errorf("Expected tags=[a,b], got %v", tags)
	}

	// Test missing slice (should return default)
	labels := GetOr(ad, "labels", []string{"x", "y"})
	if len(labels) != 2 || labels[0] != "x" || labels[1] != "y" {
		t.Errorf("Expected labels=[x,y] (default), got %v", labels)
	}
}

func TestGetOr_ZeroValue(t *testing.T) {
	ad := New()
	ad.InsertAttr("zero", 0)
	ad.InsertAttrString("empty", "")

	// Zero value exists, should return it (not default)
	zero := GetOr(ad, "zero", 999)
	if zero != 0 {
		t.Errorf("Expected zero=0, got %d", zero)
	}

	// Empty string exists, should return it (not default)
	empty := GetOr(ad, "empty", "default")
	if empty != "" {
		t.Errorf("Expected empty='', got %q", empty)
	}
}

func TestRoundTrip_GenericAPI(t *testing.T) {
	ad := New()

	// Set values using generic API
	ad.Set("cpus", 4)
	ad.Set("memory", 8192)
	ad.Set("name", "test-job")
	ad.Set("enabled", true)
	ad.Set("price", 0.05)
	ad.Set("tags", []string{"prod", "critical"})

	// Get values using generic API
	cpus := GetOr(ad, "cpus", 1)
	memory := GetOr(ad, "memory", 1024)
	name := GetOr(ad, "name", "unknown")
	enabled := GetOr(ad, "enabled", false)
	price := GetOr(ad, "price", 0.0)
	tags := GetOr(ad, "tags", []string{})

	// Verify
	if cpus != 4 {
		t.Errorf("Expected cpus=4, got %d", cpus)
	}
	if memory != 8192 {
		t.Errorf("Expected memory=8192, got %d", memory)
	}
	if name != "test-job" {
		t.Errorf("Expected name='test-job', got %q", name)
	}
	if !enabled {
		t.Errorf("Expected enabled=true, got %v", enabled)
	}
	if price != 0.05 {
		t.Errorf("Expected price=0.05, got %f", price)
	}
	if len(tags) != 2 || tags[0] != "prod" || tags[1] != "critical" {
		t.Errorf("Expected tags=[prod,critical], got %v", tags)
	}
}

func TestGenericAPI_WithExpressionEvaluation(t *testing.T) {
	ad := New()
	ad.Set("base", 10)

	// Set an expression
	expr, _ := ParseExpr("base * 2")
	ad.Set("computed", expr)

	// Get the unevaluated expression
	computedExpr, ok := GetAs[*Expr](ad, "computed")
	if !ok {
		t.Fatal("Expected computed expression to be retrievable")
	}

	// Evaluate in context
	result := computedExpr.Eval(ad)
	resultInt, _ := result.IntValue()
	if resultInt != 20 {
		t.Errorf("Expected computed=20 (10*2), got %d", resultInt)
	}
}
