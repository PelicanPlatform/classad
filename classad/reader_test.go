package classad

import (
	"strings"
	"testing"
)

func TestNewReader_SingleClassAd(t *testing.T) {
	input := `[Foo = 1; Bar = 2]`
	reader := NewReader(strings.NewReader(input))

	if !reader.Next() {
		t.Fatalf("Expected to find ClassAd, got error: %v", reader.Err())
	}

	ad := reader.ClassAd()
	if ad == nil {
		t.Fatal("Expected ClassAd, got nil")
	}

	foo, ok := ad.EvaluateAttrInt("Foo")
	if !ok || foo != 1 {
		t.Errorf("Expected Foo=1, got %v", foo)
	}

	bar, ok := ad.EvaluateAttrInt("Bar")
	if !ok || bar != 2 {
		t.Errorf("Expected Bar=2, got %v", bar)
	}

	// Should be no more ClassAds
	if reader.Next() {
		t.Error("Expected no more ClassAds")
	}

	if reader.Err() != nil {
		t.Errorf("Unexpected error: %v", reader.Err())
	}
}

func TestNewReader_MultipleClassAds(t *testing.T) {
	input := `
[Foo = 1; Bar = 2]
[Baz = 3; Qux = 4]
[Name = "test"; Value = 99]
`
	reader := NewReader(strings.NewReader(input))

	// First ClassAd
	if !reader.Next() {
		t.Fatalf("Expected first ClassAd, got error: %v", reader.Err())
	}
	ad1 := reader.ClassAd()
	foo, _ := ad1.EvaluateAttrInt("Foo")
	if foo != 1 {
		t.Errorf("Expected Foo=1, got %v", foo)
	}

	// Second ClassAd
	if !reader.Next() {
		t.Fatalf("Expected second ClassAd, got error: %v", reader.Err())
	}
	ad2 := reader.ClassAd()
	baz, _ := ad2.EvaluateAttrInt("Baz")
	if baz != 3 {
		t.Errorf("Expected Baz=3, got %v", baz)
	}

	// Third ClassAd
	if !reader.Next() {
		t.Fatalf("Expected third ClassAd, got error: %v", reader.Err())
	}
	ad3 := reader.ClassAd()
	name, _ := ad3.EvaluateAttrString("Name")
	if name != "test" {
		t.Errorf("Expected Name=test, got %v", name)
	}

	// No more ClassAds
	if reader.Next() {
		t.Error("Expected no more ClassAds")
	}
}

func TestNewReader_WithComments(t *testing.T) {
	input := `
// This is a comment
[Foo = 1; Bar = 2]
/* Block comment */
[Baz = 3]
`
	reader := NewReader(strings.NewReader(input))

	count := 0
	for reader.Next() {
		count++
	}

	if reader.Err() != nil {
		t.Errorf("Unexpected error: %v", reader.Err())
	}

	if count != 2 {
		t.Errorf("Expected 2 ClassAds, got %d", count)
	}
}

func TestNewReader_NestedClassAds(t *testing.T) {
	input := `[Outer = [Inner = 42]; Value = 10]`
	reader := NewReader(strings.NewReader(input))

	if !reader.Next() {
		t.Fatalf("Expected ClassAd, got error: %v", reader.Err())
	}

	ad := reader.ClassAd()
	outerVal := ad.EvaluateAttr("Outer")
	if !outerVal.IsClassAd() {
		t.Error("Expected Outer to be a ClassAd")
	}

	innerAd, _ := outerVal.ClassAdValue()
	inner, _ := innerAd.EvaluateAttrInt("Inner")
	if inner != 42 {
		t.Errorf("Expected Inner=42, got %v", inner)
	}
}

func TestNewReader_MultilineClassAd(t *testing.T) {
	input := `[
Foo = 1;
Bar = 2;
Baz = "hello"
]`
	reader := NewReader(strings.NewReader(input))

	if !reader.Next() {
		t.Fatalf("Expected ClassAd, got error: %v", reader.Err())
	}

	ad := reader.ClassAd()
	foo, _ := ad.EvaluateAttrInt("Foo")
	if foo != 1 {
		t.Errorf("Expected Foo=1, got %v", foo)
	}
}

func TestNewReader_EmptyInput(t *testing.T) {
	reader := NewReader(strings.NewReader(""))

	if reader.Next() {
		t.Error("Expected no ClassAds in empty input")
	}

	if reader.Err() != nil {
		t.Errorf("Unexpected error: %v", reader.Err())
	}
}

func TestNewReader_InvalidClassAd(t *testing.T) {
	input := `[Foo = ]` // Invalid syntax
	reader := NewReader(strings.NewReader(input))

	if reader.Next() {
		t.Error("Expected parsing to fail for invalid ClassAd")
	}

	if reader.Err() == nil {
		t.Error("Expected error for invalid ClassAd")
	}
}

func TestNewOldReader_SingleClassAd(t *testing.T) {
	input := `Foo = 1
Bar = 2`
	reader := NewOldReader(strings.NewReader(input))

	if !reader.Next() {
		t.Fatalf("Expected to find ClassAd, got error: %v", reader.Err())
	}

	ad := reader.ClassAd()
	foo, ok := ad.EvaluateAttrInt("Foo")
	if !ok || foo != 1 {
		t.Errorf("Expected Foo=1, got %v", foo)
	}

	bar, ok := ad.EvaluateAttrInt("Bar")
	if !ok || bar != 2 {
		t.Errorf("Expected Bar=2, got %v", bar)
	}

	if reader.Next() {
		t.Error("Expected no more ClassAds")
	}
}

func TestNewOldReader_MultipleClassAds(t *testing.T) {
	input := `Foo = 1
Bar = 2

Baz = 3
Qux = 4

Name = "test"
Value = 99`
	reader := NewOldReader(strings.NewReader(input))

	// First ClassAd
	if !reader.Next() {
		t.Fatalf("Expected first ClassAd, got error: %v", reader.Err())
	}
	ad1 := reader.ClassAd()
	foo, _ := ad1.EvaluateAttrInt("Foo")
	if foo != 1 {
		t.Errorf("Expected Foo=1, got %v", foo)
	}

	// Second ClassAd
	if !reader.Next() {
		t.Fatalf("Expected second ClassAd, got error: %v", reader.Err())
	}
	ad2 := reader.ClassAd()
	baz, _ := ad2.EvaluateAttrInt("Baz")
	if baz != 3 {
		t.Errorf("Expected Baz=3, got %v", baz)
	}

	// Third ClassAd
	if !reader.Next() {
		t.Fatalf("Expected third ClassAd, got error: %v", reader.Err())
	}
	ad3 := reader.ClassAd()
	name, _ := ad3.EvaluateAttrString("Name")
	if name != "test" {
		t.Errorf("Expected Name=test, got %v", name)
	}

	// No more ClassAds
	if reader.Next() {
		t.Error("Expected no more ClassAds")
	}
}

func TestNewOldReader_WithComments(t *testing.T) {
	input := `// This is a comment
Foo = 1
Bar = 2

# Another comment
Baz = 3`
	reader := NewOldReader(strings.NewReader(input))

	count := 0
	for reader.Next() {
		count++
	}

	if reader.Err() != nil {
		t.Errorf("Unexpected error: %v", reader.Err())
	}

	if count != 2 {
		t.Errorf("Expected 2 ClassAds, got %d", count)
	}
}

func TestNewOldReader_MultipleBlankLines(t *testing.T) {
	input := `Foo = 1



Bar = 2`
	reader := NewOldReader(strings.NewReader(input))

	count := 0
	for reader.Next() {
		count++
	}

	if reader.Err() != nil {
		t.Errorf("Unexpected error: %v", reader.Err())
	}

	if count != 2 {
		t.Errorf("Expected 2 ClassAds, got %d", count)
	}
}

func TestNewOldReader_EmptyInput(t *testing.T) {
	reader := NewOldReader(strings.NewReader(""))

	if reader.Next() {
		t.Error("Expected no ClassAds in empty input")
	}

	if reader.Err() != nil {
		t.Errorf("Unexpected error: %v", reader.Err())
	}
}

func TestNewOldReader_RealWorldExample(t *testing.T) {
	input := `MyType = "Machine"
TargetType = "Job"
Cpus = 4
Memory = 8192

MyType = "Job"
TargetType = "Machine"
RequestCpus = 2
RequestMemory = 2048`
	reader := NewOldReader(strings.NewReader(input))

	// Machine ClassAd
	if !reader.Next() {
		t.Fatalf("Expected first ClassAd, got error: %v", reader.Err())
	}
	machine := reader.ClassAd()
	myType, _ := machine.EvaluateAttrString("MyType")
	if myType != "Machine" {
		t.Errorf("Expected MyType=Machine, got %v", myType)
	}
	cpus, _ := machine.EvaluateAttrInt("Cpus")
	if cpus != 4 {
		t.Errorf("Expected Cpus=4, got %v", cpus)
	}

	// Job ClassAd
	if !reader.Next() {
		t.Fatalf("Expected second ClassAd, got error: %v", reader.Err())
	}
	job := reader.ClassAd()
	myType2, _ := job.EvaluateAttrString("MyType")
	if myType2 != "Job" {
		t.Errorf("Expected MyType=Job, got %v", myType2)
	}
	reqCpus, _ := job.EvaluateAttrInt("RequestCpus")
	if reqCpus != 2 {
		t.Errorf("Expected RequestCpus=2, got %v", reqCpus)
	}

	if reader.Next() {
		t.Error("Expected no more ClassAds")
	}
}

// TestReader_ForLoopPattern tests the idiomatic Go for-loop usage
func TestReader_ForLoopPattern(t *testing.T) {
	input := `[ID = 1]
[ID = 2]
[ID = 3]`
	reader := NewReader(strings.NewReader(input))

	count := 0
	ids := []int64{}

	for reader.Next() {
		ad := reader.ClassAd()
		id, ok := ad.EvaluateAttrInt("ID")
		if !ok {
			t.Error("Expected ID attribute")
		}
		ids = append(ids, id)
		count++
	}

	if err := reader.Err(); err != nil {
		t.Errorf("Unexpected error: %v", err)
	}

	if count != 3 {
		t.Errorf("Expected 3 ClassAds, got %d", count)
	}

	expectedIds := []int64{1, 2, 3}
	for i, id := range ids {
		if id != expectedIds[i] {
			t.Errorf("Expected ID=%d, got %d", expectedIds[i], id)
		}
	}
}

// TestAll tests the Go 1.23-style iterator
func TestAll(t *testing.T) {
	input := `[ID = 1; Name = "first"]
[ID = 2; Name = "second"]
[ID = 3; Name = "third"]`

	count := 0
	ids := []int64{}

	// Use the iterator function
	All(strings.NewReader(input))(func(ad *ClassAd) bool {
		count++
		id, ok := ad.EvaluateAttrInt("ID")
		if !ok {
			t.Error("Expected ID attribute")
		}
		ids = append(ids, id)
		return true // Continue iteration
	})

	if count != 3 {
		t.Errorf("Expected 3 ClassAds, got %d", count)
	}

	expectedIds := []int64{1, 2, 3}
	for i, id := range ids {
		if id != expectedIds[i] {
			t.Errorf("Expected ID=%d, got %d", expectedIds[i], id)
		}
	}
}

// TestAll_EarlyStop tests stopping iteration early
func TestAll_EarlyStop(t *testing.T) {
	input := `[ID = 1]
[ID = 2]
[ID = 3]
[ID = 4]
[ID = 5]`

	count := 0

	All(strings.NewReader(input))(func(ad *ClassAd) bool {
		count++
		id, _ := ad.EvaluateAttrInt("ID")
		return id < 3 // Stop after ID=3
	})

	if count != 3 {
		t.Errorf("Expected to stop at 3 ClassAds, got %d", count)
	}
}

// TestAllOld tests the old-style iterator
func TestAllOld(t *testing.T) {
	input := `ID = 1
Name = "first"

ID = 2
Name = "second"

ID = 3
Name = "third"`

	count := 0
	names := []string{}

	AllOld(strings.NewReader(input))(func(ad *ClassAd) bool {
		count++
		name, ok := ad.EvaluateAttrString("Name")
		if !ok {
			t.Error("Expected Name attribute")
		}
		names = append(names, name)
		return true
	})

	if count != 3 {
		t.Errorf("Expected 3 ClassAds, got %d", count)
	}

	expectedNames := []string{"first", "second", "third"}
	for i, name := range names {
		if name != expectedNames[i] {
			t.Errorf("Expected Name=%s, got %s", expectedNames[i], name)
		}
	}
}

// TestAllWithIndex tests the indexed iterator
func TestAllWithIndex(t *testing.T) {
	input := `[Value = 10]
[Value = 20]
[Value = 30]`

	indexes := []int{}
	values := []int64{}

	AllWithIndex(strings.NewReader(input))(func(i int, ad *ClassAd) bool {
		indexes = append(indexes, i)
		val, _ := ad.EvaluateAttrInt("Value")
		values = append(values, val)
		return true
	})

	if len(indexes) != 3 {
		t.Errorf("Expected 3 indexes, got %d", len(indexes))
	}

	for i, idx := range indexes {
		if idx != i {
			t.Errorf("Expected index %d, got %d", i, idx)
		}
	}

	expectedValues := []int64{10, 20, 30}
	for i, val := range values {
		if val != expectedValues[i] {
			t.Errorf("Expected Value=%d, got %d", expectedValues[i], val)
		}
	}
}

// TestAllWithError tests error handling in iterator
func TestAllWithError(t *testing.T) {
	// Valid input
	input := `[ID = 1]
[ID = 2]`

	var err error
	count := 0

	AllWithError(strings.NewReader(input), &err)(func(ad *ClassAd) bool {
		count++
		return true
	})

	if err != nil {
		t.Errorf("Unexpected error: %v", err)
	}

	if count != 2 {
		t.Errorf("Expected 2 ClassAds, got %d", count)
	}

	// Invalid input
	invalidInput := `[ID = ]` // Invalid syntax
	var err2 error
	count2 := 0

	AllWithError(strings.NewReader(invalidInput), &err2)(func(ad *ClassAd) bool {
		count2++
		return true
	})

	if err2 == nil {
		t.Error("Expected error for invalid ClassAd")
	}

	if count2 != 0 {
		t.Errorf("Expected 0 ClassAds due to error, got %d", count2)
	}
}

// TestAllOldWithIndex tests the old-style indexed iterator
func TestAllOldWithIndex(t *testing.T) {
	input := `Name = "first"

Name = "second"

Name = "third"`

	count := 0

	AllOldWithIndex(strings.NewReader(input))(func(i int, ad *ClassAd) bool {
		if i != count {
			t.Errorf("Expected index %d, got %d", count, i)
		}
		count++
		return true
	})

	if count != 3 {
		t.Errorf("Expected 3 ClassAds, got %d", count)
	}
}

// TestNewReader_ConcatenatedClassAds tests parsing ClassAds that are concatenated
// without whitespace between them (e.g., "]["). This format is used by HTCondor
// when writing ClassAds to plugin input files.
func TestNewReader_ConcatenatedClassAds(t *testing.T) {
	// Test concatenated format without newlines: ][
	input := `[Url = "pelican://example.com/file1"; LocalFileName = "/tmp/file1"][Url = "pelican://example.com/file2"; LocalFileName = "/tmp/file2"][Url = "pelican://example.com/file3"; LocalFileName = "/tmp/file3"]`
	reader := NewReader(strings.NewReader(input))

	count := 0
	urls := []string{}

	for reader.Next() {
		ad := reader.ClassAd()
		url, ok := ad.EvaluateAttrString("Url")
		if !ok {
			t.Error("Expected Url attribute")
		}
		urls = append(urls, url)
		count++
	}

	if reader.Err() != nil {
		t.Errorf("Unexpected error: %v", reader.Err())
	}

	if count != 3 {
		t.Errorf("Expected 3 ClassAds, got %d", count)
	}

	expectedUrls := []string{
		"pelican://example.com/file1",
		"pelican://example.com/file2",
		"pelican://example.com/file3",
	}
	for i, url := range urls {
		if url != expectedUrls[i] {
			t.Errorf("Expected Url=%s, got %s", expectedUrls[i], url)
		}
	}
}

// TestAll_ConcatenatedClassAds tests the iterator pattern with concatenated ClassAds
func TestAll_ConcatenatedClassAds(t *testing.T) {
	// Test concatenated format without newlines: ][
	input := `[ID = 1; Name = "first"][ID = 2; Name = "second"][ID = 3; Name = "third"]`

	count := 0
	ids := []int64{}

	All(strings.NewReader(input))(func(ad *ClassAd) bool {
		count++
		id, ok := ad.EvaluateAttrInt("ID")
		if !ok {
			t.Error("Expected ID attribute")
		}
		ids = append(ids, id)
		return true
	})

	if count != 3 {
		t.Errorf("Expected 3 ClassAds, got %d", count)
	}

	expectedIds := []int64{1, 2, 3}
	for i, id := range ids {
		if id != expectedIds[i] {
			t.Errorf("Expected ID=%d, got %d", expectedIds[i], id)
		}
	}
}
