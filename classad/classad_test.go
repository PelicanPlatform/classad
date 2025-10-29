package classad

import (
	"testing"

	"github.com/bbockelm/golang-classads/ast"
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
	if val := ad.Lookup("x"); val == nil {
		t.Error("Attribute 'x' not found")
	}
	if val := ad.Lookup("y"); val == nil {
		t.Error("Attribute 'y' not found")
	}
	if val := ad.Lookup("name"); val == nil {
		t.Error("Attribute 'name' not found")
	}
	if val := ad.Lookup("flag"); val == nil {
		t.Error("Attribute 'flag' not found")
	}
}

func TestLookup(t *testing.T) {
	ad := New()
	ad.InsertAttr("x", 10)

	expr := ad.Lookup("x")
	if expr == nil {
		t.Fatal("Lookup('x') returned nil")
	}

	intLit, ok := expr.(*ast.IntegerLiteral)
	if !ok {
		t.Fatal("Lookup('x') did not return IntegerLiteral")
	}
	if intLit.Value != 10 {
		t.Errorf("Expected value 10, got %d", intLit.Value)
	}

	// Test non-existent attribute
	expr = ad.Lookup("nonexistent")
	if expr != nil {
		t.Error("Lookup('nonexistent') should return nil")
	}
}

func TestDelete(t *testing.T) {
	ad := New()
	ad.InsertAttr("x", 10)
	ad.InsertAttr("y", 20)

	if !ad.Delete("x") {
		t.Error("Delete('x') should return true")
	}

	if ad.Lookup("x") != nil {
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
