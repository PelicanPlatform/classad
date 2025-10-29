package parser

import (
	"testing"

	"github.com/bbockelm/golang-classads/ast"
)

func TestParseOldClassAd(t *testing.T) {
	tests := []struct {
		name    string
		input   string
		wantErr bool
	}{
		{
			name: "simple old ClassAd",
			input: `Foo = 3
Bar = "hello"
Moo = Foo + 5`,
			wantErr: false,
		},
		{
			name: "old ClassAd from HTCondor docs",
			input: `Foo = 3
Bar = "ab\"cd\\ef"
Moo = Foo =!= Undefined`,
			wantErr: false,
		},
		{
			name: "machine ClassAd example",
			input: `MyType = "Machine"
TargetType = "Job"
Machine = "froth.cs.wisc.edu"
Arch = "INTEL"
OpSys = "LINUX"
Disk = 35882
Memory = 128
KeyboardIdle = 173
LoadAvg = 0.1000
Requirements = TARGET.Owner=="smith" || LoadAvg<=0.3 && KeyboardIdle>15*60`,
			wantErr: false,
		},
		{
			name: "old ClassAd with comments",
			input: `// This is a comment
Foo = 3
// Another comment
Bar = "hello"`,
			wantErr: false,
		},
		{
			name: "old ClassAd with block comment",
			input: `/* Block comment */
Foo = 3
/* Multi-line
   comment */
Bar = "hello"`,
			wantErr: false,
		},
		{
			name: "old ClassAd with empty lines",
			input: `Foo = 3

Bar = "hello"

Baz = true`,
			wantErr: false,
		},
		{
			name: "old ClassAd with boolean values",
			input: `IsActive = true
IsIdle = false
HasError = error
MissingValue = undefined`,
			wantErr: false,
		},
		{
			name: "old ClassAd with arithmetic expressions",
			input: `X = 10
Y = 20
Sum = X + Y
Product = X * Y
Quotient = Y / X`,
			wantErr: false,
		},
		{
			name: "old ClassAd with comparison expressions",
			input: `X = 10
Y = 20
IsLess = X < Y
IsEqual = X == 10
IsGreater = Y > X`,
			wantErr: false,
		},
		{
			name: "old ClassAd with logical expressions",
			input: `A = true
B = false
AndResult = A && B
OrResult = A || B
NotResult = !A`,
			wantErr: false,
		},
		{
			name: "old ClassAd with conditional expressions",
			input: `X = 10
Y = 20
Max = (X > Y) ? X : Y
Status = (X > 5) ? "high" : "low"`,
			wantErr: false,
		},
		{
			name: "old ClassAd with string operations",
			input: `FirstName = "John"
LastName = "Doe"
FullName = strcat(FirstName, " ", LastName)
Length = size(FirstName)`,
			wantErr: false,
		},
		{
			name: "old ClassAd with scoped references",
			input: `Cpus = 2
Memory = 2048
Requirements = TARGET.Cpus >= MY.Cpus && TARGET.Memory >= MY.Memory`,
			wantErr: false,
		},
		{
			name: "HTCondor job requirements example",
			input: `Requirements = (Arch == "INTEL") && (OpSys == "LINUX")
Rank = TARGET.Memory + TARGET.Mips`,
			wantErr: false,
		},
		{
			name: "HTCondor machine policy example",
			input: `Friend = Owner == "tannenba" || Owner == "wright"
ResearchGroup = Owner == "jbasney" || Owner == "raman"
Trusted = Owner != "rival" && Owner != "riffraff"
START = Trusted && ( ResearchGroup || LoadAvg < 0.3 && KeyboardIdle > 15*60 )
RANK = Friend + ResearchGroup*10`,
			wantErr: false,
		},
		{
			name:    "empty old ClassAd",
			input:   ``,
			wantErr: false,
		},
		{
			name: "old ClassAd with only comments",
			input: `// Just a comment
// Another comment`,
			wantErr: false,
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			ad, err := ParseOldClassAd(tt.input)
			if (err != nil) != tt.wantErr {
				t.Errorf("ParseOldClassAd() error = %v, wantErr %v", err, tt.wantErr)
				return
			}
			if err == nil && ad == nil {
				t.Errorf("ParseOldClassAd() returned nil ClassAd without error")
			}
		})
	}
}

func TestOldClassAdEquivalence(t *testing.T) {
	// Test that old and new formats produce equivalent ClassAds
	tests := []struct {
		name      string
		oldFormat string
		newFormat string
	}{
		{
			name: "simple equivalence",
			oldFormat: `Foo = 3
Bar = "hello"`,
			newFormat: `[
Foo = 3;
Bar = "hello"
]`,
		},
		{
			name: "HTCondor example equivalence",
			oldFormat: `Foo = 3
Bar = "ab\"cd\\ef"
Moo = Foo =!= Undefined`,
			newFormat: `[
Foo = 3;
Bar = "ab\"cd\\ef";
Moo = Foo =!= Undefined
]`,
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			oldAd, err := ParseOldClassAd(tt.oldFormat)
			if err != nil {
				t.Fatalf("ParseOldClassAd() error = %v", err)
			}

			newAd, err := Parse(tt.newFormat)
			if err != nil {
				t.Fatalf("Parse() error = %v", err)
			}

			// Check that both have the same number of attributes
			oldLen := len(oldAd.Attributes)
			newLen := len(newAd.(*ast.ClassAd).Attributes)
			if oldLen != newLen {
				t.Errorf("Different number of attributes: old=%d, new=%d", oldLen, newLen)
			}

			// Create a map of attribute names from old ClassAd
			oldNames := make(map[string]bool)
			for _, attr := range oldAd.Attributes {
				oldNames[attr.Name] = true
			}

			// Check that all attribute names are present in both
			for _, attr := range newAd.(*ast.ClassAd).Attributes {
				if !oldNames[attr.Name] {
					t.Errorf("Attribute %s present in new format but not in old format", attr.Name)
				}
			}
		})
	}
}

// Helper function to find an attribute by name in a ClassAd
func findAttribute(ad *ast.ClassAd, name string) *ast.AttributeAssignment {
	for _, attr := range ad.Attributes {
		if attr.Name == name {
			return attr
		}
	}
	return nil
}

func TestOldClassAdEvaluation(t *testing.T) {
	// Test that parsed old ClassAds can be evaluated correctly
	oldFormat := `X = 10
Y = 20
Sum = X + Y`

	ad, err := ParseOldClassAd(oldFormat)
	if err != nil {
		t.Fatalf("ParseOldClassAd() error = %v", err)
	}

	// Verify attributes exist
	if findAttribute(ad, "X") == nil {
		t.Errorf("Attribute X not found")
	}
	if findAttribute(ad, "Y") == nil {
		t.Errorf("Attribute Y not found")
	}
	if findAttribute(ad, "Sum") == nil {
		t.Errorf("Attribute Sum not found")
	}

	// Check the expressions are correct types
	xAttr := findAttribute(ad, "X")
	if xAttr != nil && xAttr.Value == nil {
		t.Errorf("Attribute X has nil expression")
	}
	yAttr := findAttribute(ad, "Y")
	if yAttr != nil && yAttr.Value == nil {
		t.Errorf("Attribute Y has nil expression")
	}
	sumAttr := findAttribute(ad, "Sum")
	if sumAttr != nil && sumAttr.Value == nil {
		t.Errorf("Attribute Sum has nil expression")
	}
}

func TestConvertOldToNewFormat(t *testing.T) {
	tests := []struct {
		name     string
		input    string
		expected string
	}{
		{
			name:     "simple conversion",
			input:    "Foo = 3\nBar = \"hello\"",
			expected: "[\nFoo = 3;\nBar = \"hello\";\n]",
		},
		{
			name:     "with empty lines",
			input:    "Foo = 3\n\nBar = \"hello\"",
			expected: "[\nFoo = 3;\nBar = \"hello\";\n]",
		},
		{
			name:     "with comments",
			input:    "// Comment\nFoo = 3\nBar = \"hello\"",
			expected: "[\n// Comment\nFoo = 3;\nBar = \"hello\";\n]",
		},
		{
			name:     "empty input",
			input:    "",
			expected: "[\n]",
		},
		{
			name:     "only whitespace",
			input:    "   \n  \n",
			expected: "[\n]",
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			result := convertOldToNewFormat(tt.input)
			if result != tt.expected {
				t.Errorf("convertOldToNewFormat() = %q, expected %q", result, tt.expected)
			}
		})
	}
}
