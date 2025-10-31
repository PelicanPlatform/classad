package classad

import (
	"testing"

	"github.com/PelicanPlatform/classad/ast"
)

func TestNewClassAd(t *testing.T) {
	ad := New()
	if ad == nil {
		t.Fatal("New() returned nil")
	}
	if ad.Size() != 0 {
		t.Errorf("New ClassAd should be empty, got size %d", ad.Size())
	}
}

func TestParseClassAd(t *testing.T) {
	tests := []struct {
		name    string
		input   string
		wantErr bool
	}{
		{"empty classad", "[]", false},
		{"simple classad", "[x = 1]", false},
		{"multiple attrs", "[x = 1; y = 2]", false},
		{"invalid syntax", "[x = ", true},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			ad, err := Parse(tt.input)
			if (err != nil) != tt.wantErr {
				t.Errorf("Parse() error = %v, wantErr %v", err, tt.wantErr)
				return
			}
			if !tt.wantErr && ad == nil {
				t.Error("Parse() returned nil ClassAd")
			}
		})
	}
}

func TestParseOldClassAd(t *testing.T) {
	tests := []struct {
		name    string
		input   string
		wantErr bool
	}{
		{"empty old classad", "", false},
		{"simple old classad", "x = 1", false},
		{"multiple attrs old", "x = 1\ny = 2", false},
		{
			"HTCondor machine example",
			`MyType = "Machine"
TargetType = "Job"
Machine = "froth.cs.wisc.edu"
Arch = "INTEL"
OpSys = "LINUX"`,
			false,
		},
		{
			"old classad with expressions",
			`X = 10
Y = 20
Sum = X + Y
Max = (X > Y) ? X : Y`,
			false,
		},
		{
			"old classad with scoped refs",
			`Cpus = 2
Requirements = TARGET.Cpus >= MY.Cpus`,
			false,
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			ad, err := ParseOld(tt.input)
			if (err != nil) != tt.wantErr {
				t.Errorf("ParseOld() error = %v, wantErr %v", err, tt.wantErr)
				return
			}
			if !tt.wantErr && ad == nil {
				t.Error("ParseOld() returned nil ClassAd")
			}
		})
	}
}

func TestOldAndNewFormatEquivalence(t *testing.T) {
	// Test that the same data parsed in old and new format produces equivalent results
	oldFormat := `X = 10
Y = 20
Sum = X + Y`

	newFormat := `[
X = 10;
Y = 20;
Sum = X + Y
]`

	oldAd, err := ParseOld(oldFormat)
	if err != nil {
		t.Fatalf("ParseOld() error = %v", err)
	}

	newAd, err := Parse(newFormat)
	if err != nil {
		t.Fatalf("Parse() error = %v", err)
	}

	// Check that both have the same number of attributes
	if oldAd.Size() != newAd.Size() {
		t.Errorf("Different sizes: old=%d, new=%d", oldAd.Size(), newAd.Size())
	}

	// Check that evaluation produces same results
	oldSum, oldOk := oldAd.EvaluateAttrInt("Sum")
	newSum, newOk := newAd.EvaluateAttrInt("Sum")

	if oldOk != newOk {
		t.Errorf("Different evaluation results: oldOk=%v, newOk=%v", oldOk, newOk)
	}

	if oldOk && newOk && oldSum != newSum {
		t.Errorf("Different sum values: old=%d, new=%d", oldSum, newSum)
	}
}

func TestInsertAttr(t *testing.T) {
	ad := New()

	ad.InsertAttr("x", 42)
	ad.InsertAttrFloat("y", 3.14)
	ad.InsertAttrString("name", "test")
	ad.InsertAttrBool("flag", true)

	if ad.Size() != 4 {
		t.Errorf("Expected size 4, got %d", ad.Size())
	}

	// Test that attributes were inserted correctly
	if val, ok := ad.Lookup("x"); !ok || val == nil {
		t.Error("Attribute 'x' not found")
	}
	if val, ok := ad.Lookup("y"); !ok || val == nil {
		t.Error("Attribute 'y' not found")
	}
	if val, ok := ad.Lookup("name"); !ok || val == nil {
		t.Error("Attribute 'name' not found")
	}
	if val, ok := ad.Lookup("flag"); !ok || val == nil {
		t.Error("Attribute 'flag' not found")
	}
}

func TestInsertListElement(t *testing.T) {
	ad := New()

	// Test creating a new list
	ad.InsertListElement("items", &ast.StringLiteral{Value: "first"})
	if ad.Size() != 1 {
		t.Errorf("Expected size 1, got %d", ad.Size())
	}

	// Verify it's a list
	expr, ok := ad.Lookup("items")
	if !ok || expr == nil {
		t.Fatal("Attribute 'items' not found")
	}
	if expr.String() != "{\"first\"}" {
		t.Errorf("Expected list {\"first\"}, got %s", expr.String())
	}

	// Test appending to existing list
	ad.InsertListElement("items", &ast.StringLiteral{Value: "second"})
	ad.InsertListElement("items", &ast.IntegerLiteral{Value: 42})

	expr, ok = ad.Lookup("items")
	if !ok || expr == nil {
		t.Fatal("Attribute 'items' not found after append")
	}
	expected := "{\"first\", \"second\", 42}"
	if expr.String() != expected {
		t.Errorf("Expected list %s, got %s", expected, expr.String())
	}

	// Test that size didn't change (still one attribute)
	if ad.Size() != 1 {
		t.Errorf("Expected size 1, got %d", ad.Size())
	}

	// Test replacing non-list attribute with list
	ad.InsertAttr("x", 100)
	ad.InsertListElement("x", &ast.IntegerLiteral{Value: 200})

	expr, ok = ad.Lookup("x")
	if !ok || expr == nil {
		t.Fatal("Attribute 'x' not found")
	}
	if expr.String() != "{200}" {
		t.Errorf("Expected list {200}, got %s", expr.String())
	}
}

func TestInsertAttrList(t *testing.T) {
	ad := New()

	// Test InsertAttrList with mixed types
	elements := []ast.Expr{
		&ast.StringLiteral{Value: "hello"},
		&ast.IntegerLiteral{Value: 42},
		&ast.BooleanLiteral{Value: true},
		&ast.RealLiteral{Value: 3.14},
	}
	ad.InsertAttrList("mixed", elements)

	expr, ok := ad.Lookup("mixed")
	if !ok || expr == nil {
		t.Fatal("Attribute 'mixed' not found")
	}
	expected := "{\"hello\", 42, true, 3.14}"
	if expr.String() != expected {
		t.Errorf("Expected list %s, got %s", expected, expr.String())
	}

	// Test empty list
	ad.InsertAttrList("empty", []ast.Expr{})
	expr, ok = ad.Lookup("empty")
	if !ok || expr == nil {
		t.Fatal("Attribute 'empty' not found")
	}
	if expr.String() != "{}" {
		t.Errorf("Expected empty list {}, got %s", expr.String())
	}
}

func TestInsertAttrListInt(t *testing.T) {
	ad := New()
	ad.InsertAttrListInt("numbers", []int64{1, 2, 3, 4, 5})

	expr, ok := ad.Lookup("numbers")
	if !ok || expr == nil {
		t.Fatal("Attribute 'numbers' not found")
	}
	expected := "{1, 2, 3, 4, 5}"
	if expr.String() != expected {
		t.Errorf("Expected list %s, got %s", expected, expr.String())
	}

	// Test empty list
	ad.InsertAttrListInt("emptyInts", []int64{})
	expr, ok = ad.Lookup("emptyInts")
	if !ok || expr == nil {
		t.Fatal("Attribute 'emptyInts' not found")
	}
	if expr.String() != "{}" {
		t.Errorf("Expected empty list {}, got %s", expr.String())
	}
}

func TestInsertAttrListFloat(t *testing.T) {
	ad := New()
	ad.InsertAttrListFloat("values", []float64{1.5, 2.7, 3.14})

	expr, ok := ad.Lookup("values")
	if !ok || expr == nil {
		t.Fatal("Attribute 'values' not found")
	}
	expected := "{1.5, 2.7, 3.14}"
	if expr.String() != expected {
		t.Errorf("Expected list %s, got %s", expected, expr.String())
	}
}

func TestInsertAttrListString(t *testing.T) {
	ad := New()
	ad.InsertAttrListString("names", []string{"Alice", "Bob", "Charlie"})

	expr, ok := ad.Lookup("names")
	if !ok || expr == nil {
		t.Fatal("Attribute 'names' not found")
	}
	expected := "{\"Alice\", \"Bob\", \"Charlie\"}"
	if expr.String() != expected {
		t.Errorf("Expected list %s, got %s", expected, expr.String())
	}
}

func TestInsertAttrListBool(t *testing.T) {
	ad := New()
	ad.InsertAttrListBool("flags", []bool{true, false, true})

	expr, ok := ad.Lookup("flags")
	if !ok || expr == nil {
		t.Fatal("Attribute 'flags' not found")
	}
	expected := "{true, false, true}"
	if expr.String() != expected {
		t.Errorf("Expected list %s, got %s", expected, expr.String())
	}
}

func TestLookup(t *testing.T) {
	ad := New()
	ad.InsertAttr("x", 10)

	expr, ok := ad.Lookup("x")
	if !ok || expr == nil {
		t.Fatal("Lookup('x') should return expression")
	}

	// Verify the expression string representation
	if expr.String() != "10" {
		t.Errorf("Expected expression string '10', got '%s'", expr.String())
	}

	// Test non-existent attribute
	expr, ok = ad.Lookup("nonexistent")
	if ok || expr != nil {
		t.Error("Lookup('nonexistent') should return false")
	}
}

func TestDelete(t *testing.T) {
	ad := New()
	ad.InsertAttr("x", 10)
	ad.InsertAttr("y", 20)

	if !ad.Delete("x") {
		t.Error("Delete('x') should return true")
	}

	if _, ok := ad.Lookup("x"); ok {
		t.Error("Attribute 'x' should be deleted")
	}

	if ad.Size() != 1 {
		t.Errorf("Expected size 1 after delete, got %d", ad.Size())
	}

	// Try deleting non-existent attribute
	if ad.Delete("nonexistent") {
		t.Error("Delete('nonexistent') should return false")
	}
}

func TestClear(t *testing.T) {
	ad := New()
	ad.InsertAttr("x", 10)
	ad.InsertAttr("y", 20)

	ad.Clear()

	if ad.Size() != 0 {
		t.Errorf("Expected size 0 after Clear(), got %d", ad.Size())
	}
}

func TestGetAttributes(t *testing.T) {
	ad := New()
	ad.InsertAttr("x", 10)
	ad.InsertAttr("y", 20)
	ad.InsertAttr("z", 30)

	names := ad.GetAttributes()
	if len(names) != 3 {
		t.Errorf("Expected 3 attribute names, got %d", len(names))
	}

	expectedNames := map[string]bool{"x": true, "y": true, "z": true}
	for _, name := range names {
		if !expectedNames[name] {
			t.Errorf("Unexpected attribute name: %s", name)
		}
		delete(expectedNames, name)
	}

	if len(expectedNames) > 0 {
		t.Errorf("Missing attribute names: %v", expectedNames)
	}
}

func TestEvaluateAttrInt(t *testing.T) {
	ad := New()
	ad.InsertAttr("x", 42)

	val, ok := ad.EvaluateAttrInt("x")
	if !ok {
		t.Fatal("EvaluateAttrInt('x') failed")
	}
	if val != 42 {
		t.Errorf("Expected 42, got %d", val)
	}

	// Test non-existent attribute
	_, ok = ad.EvaluateAttrInt("nonexistent")
	if ok {
		t.Error("EvaluateAttrInt('nonexistent') should fail")
	}
}

func TestEvaluateAttrReal(t *testing.T) {
	ad := New()
	ad.InsertAttrFloat("pi", 3.14159)

	val, ok := ad.EvaluateAttrReal("pi")
	if !ok {
		t.Fatal("EvaluateAttrReal('pi') failed")
	}
	if val != 3.14159 {
		t.Errorf("Expected 3.14159, got %g", val)
	}
}

func TestEvaluateAttrNumber(t *testing.T) {
	ad := New()
	ad.InsertAttr("x", 10)
	ad.InsertAttrFloat("y", 3.5)

	// Test integer
	val, ok := ad.EvaluateAttrNumber("x")
	if !ok {
		t.Fatal("EvaluateAttrNumber('x') failed")
	}
	if val != 10.0 {
		t.Errorf("Expected 10.0, got %g", val)
	}

	// Test real
	val, ok = ad.EvaluateAttrNumber("y")
	if !ok {
		t.Fatal("EvaluateAttrNumber('y') failed")
	}
	if val != 3.5 {
		t.Errorf("Expected 3.5, got %g", val)
	}
}

func TestEvaluateAttrString(t *testing.T) {
	ad := New()
	ad.InsertAttrString("name", "ClassAd")

	val, ok := ad.EvaluateAttrString("name")
	if !ok {
		t.Fatal("EvaluateAttrString('name') failed")
	}
	if val != "ClassAd" {
		t.Errorf("Expected 'ClassAd', got %s", val)
	}
}

func TestEvaluateAttrBool(t *testing.T) {
	ad := New()
	ad.InsertAttrBool("flag", true)

	val, ok := ad.EvaluateAttrBool("flag")
	if !ok {
		t.Fatal("EvaluateAttrBool('flag') failed")
	}
	if val != true {
		t.Error("Expected true, got false")
	}
}

func TestEvaluateArithmetic(t *testing.T) {
	tests := []struct {
		name     string
		classad  string
		attr     string
		expected int64
	}{
		{"addition", "[x = 2; y = 3; z = x + y]", "z", 5},
		{"subtraction", "[x = 10; y = 3; z = x - y]", "z", 7},
		{"multiplication", "[x = 4; y = 5; z = x * y]", "z", 20},
		{"division", "[x = 20; y = 4; z = x / y]", "z", 5},
		{"modulo", "[x = 17; y = 5; z = x % y]", "z", 2},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			ad, err := Parse(tt.classad)
			if err != nil {
				t.Fatalf("Parse error: %v", err)
			}

			val, ok := ad.EvaluateAttrInt(tt.attr)
			if !ok {
				t.Fatalf("EvaluateAttrInt('%s') failed", tt.attr)
			}
			if val != tt.expected {
				t.Errorf("Expected %d, got %d", tt.expected, val)
			}
		})
	}
}

func TestEvaluateComparison(t *testing.T) {
	tests := []struct {
		name     string
		classad  string
		attr     string
		expected bool
	}{
		{"less than true", "[x = 3; y = 5; z = x < y]", "z", true},
		{"less than false", "[x = 5; y = 3; z = x < y]", "z", false},
		{"greater than", "[x = 5; y = 3; z = x > y]", "z", true},
		{"less or equal", "[x = 3; y = 3; z = x <= y]", "z", true},
		{"greater or equal", "[x = 5; y = 5; z = x >= y]", "z", true},
		{"equal", "[x = 5; y = 5; z = x == y]", "z", true},
		{"not equal", "[x = 5; y = 3; z = x != y]", "z", true},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			ad, err := Parse(tt.classad)
			if err != nil {
				t.Fatalf("Parse error: %v", err)
			}

			val, ok := ad.EvaluateAttrBool(tt.attr)
			if !ok {
				t.Fatalf("EvaluateAttrBool('%s') failed", tt.attr)
			}
			if val != tt.expected {
				t.Errorf("Expected %v, got %v", tt.expected, val)
			}
		})
	}
}

func TestEvaluateLogical(t *testing.T) {
	tests := []struct {
		name     string
		classad  string
		attr     string
		expected bool
	}{
		{"and true", "[x = true; y = true; z = x && y]", "z", true},
		{"and false", "[x = true; y = false; z = x && y]", "z", false},
		{"or true", "[x = true; y = false; z = x || y]", "z", true},
		{"or false", "[x = false; y = false; z = x || y]", "z", false},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			ad, err := Parse(tt.classad)
			if err != nil {
				t.Fatalf("Parse error: %v", err)
			}

			val, ok := ad.EvaluateAttrBool(tt.attr)
			if !ok {
				t.Fatalf("EvaluateAttrBool('%s') failed", tt.attr)
			}
			if val != tt.expected {
				t.Errorf("Expected %v, got %v", tt.expected, val)
			}
		})
	}
}

func TestEvaluateConditional(t *testing.T) {
	ad, err := Parse("[x = 10; y = x > 5 ? 100 : 200]")
	if err != nil {
		t.Fatalf("Parse error: %v", err)
	}

	val, ok := ad.EvaluateAttrInt("y")
	if !ok {
		t.Fatal("EvaluateAttrInt('y') failed")
	}
	if val != 100 {
		t.Errorf("Expected 100, got %d", val)
	}

	ad2, err := Parse("[x = 3; y = x > 5 ? 100 : 200]")
	if err != nil {
		t.Fatalf("Parse error: %v", err)
	}

	val2, ok := ad2.EvaluateAttrInt("y")
	if !ok {
		t.Fatal("EvaluateAttrInt('y') failed for ad2")
	}
	if val2 != 200 {
		t.Errorf("Expected 200, got %d", val2)
	}
}

func TestEvaluateComplexExpression(t *testing.T) {
	// Test a more complex expression from HTCondor documentation
	input := `[
		Cpus = 4;
		Memory = 8192;
		Disk = 1000000;
		Requirements = (Cpus >= 2) && (Memory >= 4096);
		MeetsRequirements = Requirements
	]`

	ad, err := Parse(input)
	if err != nil {
		t.Fatalf("Parse error: %v", err)
	}

	// Verify attributes
	if cpus, ok := ad.EvaluateAttrInt("Cpus"); !ok || cpus != 4 {
		t.Errorf("Cpus evaluation failed: got %d, ok=%v", cpus, ok)
	}

	if memory, ok := ad.EvaluateAttrInt("Memory"); !ok || memory != 8192 {
		t.Errorf("Memory evaluation failed: got %d, ok=%v", memory, ok)
	}

	// Verify the Requirements expression evaluates correctly
	if req, ok := ad.EvaluateAttrBool("Requirements"); !ok || !req {
		t.Errorf("Requirements evaluation failed: got %v, ok=%v", req, ok)
	}

	// Verify the reference to Requirements
	if meets, ok := ad.EvaluateAttrBool("MeetsRequirements"); !ok || !meets {
		t.Errorf("MeetsRequirements evaluation failed: got %v, ok=%v", meets, ok)
	}
}

func TestUpdateAttribute(t *testing.T) {
	ad := New()
	ad.InsertAttr("x", 10)

	// Update the attribute
	ad.InsertAttr("x", 20)

	val, ok := ad.EvaluateAttrInt("x")
	if !ok {
		t.Fatal("EvaluateAttrInt('x') failed")
	}
	if val != 20 {
		t.Errorf("Expected 20 after update, got %d", val)
	}

	// Size should still be 1
	if ad.Size() != 1 {
		t.Errorf("Expected size 1, got %d", ad.Size())
	}
}

func TestUndefinedValue(t *testing.T) {
	ad := New()
	ad.InsertAttr("x", 10)

	// Reference non-existent attribute
	val := ad.EvaluateAttr("nonexistent")
	if !val.IsUndefined() {
		t.Error("Expected undefined value for non-existent attribute")
	}
}

func TestInsertAttrClassAd(t *testing.T) {
	parent := New()

	child := New()
	child.InsertAttr("a", 1)
	child.InsertAttrString("b", "x")

	parent.InsertAttrClassAd("nested", child)

	// Validate type and values via evaluation
	v := parent.EvaluateAttr("nested")
	if !v.IsClassAd() {
		t.Fatal("nested should evaluate to a ClassAd")
	}

	nested, err := v.ClassAdValue()
	if err != nil {
		t.Fatalf("ClassAdValue() error: %v", err)
	}

	if a, ok := nested.EvaluateAttrInt("a"); !ok || a != 1 {
		t.Errorf("Expected nested.a = 1, got %d, ok=%v", a, ok)
	}
	if b, ok := nested.EvaluateAttrString("b"); !ok || b != "x" {
		t.Errorf("Expected nested.b = 'x', got %q, ok=%v", b, ok)
	}

	// String form should contain a nested record
	s := parent.String()
	if s == "" || s == "[]" {
		t.Fatalf("Unexpected empty string for parent: %q", s)
	}
	if want := "[nested = [a = 1; b = \"x\"]]"; s != want {
		t.Errorf("Unexpected parent string. want=%s got=%s", want, s)
	}
}
