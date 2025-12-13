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

func TestNewReader_ConcatenatedClassAds(t *testing.T) {
	input := `[ID = 1][ID = 2][ID = 3]`
	reader := NewReader(strings.NewReader(input))

	ids := []int64{}
	for reader.Next() {
		id, ok := reader.ClassAd().EvaluateAttrInt("ID")
		if !ok {
			t.Fatalf("expected ID attribute")
		}
		ids = append(ids, id)
	}

	if err := reader.Err(); err != nil {
		t.Fatalf("Unexpected error: %v", err)
	}

	expected := []int64{1, 2, 3}
	if len(ids) != len(expected) {
		t.Fatalf("Expected %d ClassAds, got %d", len(expected), len(ids))
	}
	for i, v := range expected {
		if ids[i] != v {
			t.Fatalf("Expected ID=%d at position %d, got %d", v, i, ids[i])
		}
	}
}

func TestNewReader_ConcatenatedWithComments(t *testing.T) {
	input := `[ID = 1]/*block*/[ID = 2]// trailing comment
[ID = 3]`
	reader := NewReader(strings.NewReader(input))

	count := 0
	for reader.Next() {
		count++
	}

	if err := reader.Err(); err != nil {
		t.Fatalf("Unexpected error: %v", err)
	}

	if count != 3 {
		t.Fatalf("Expected 3 ClassAds, got %d", count)
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

func TestNewReader_ErrorAfterFirstAd(t *testing.T) {
	input := `[Ok = 1][Broken = ]`
	reader := NewReader(strings.NewReader(input))

	if !reader.Next() {
		t.Fatalf("expected first ClassAd, got error: %v", reader.Err())
	}

	ok, _ := reader.ClassAd().EvaluateAttrInt("Ok")
	if ok != 1 {
		t.Fatalf("expected Ok=1, got %d", ok)
	}

	if reader.Next() {
		t.Fatalf("expected failure on second ClassAd")
	}

	if reader.Err() == nil {
		t.Fatalf("expected error after malformed second ClassAd")
	}

	if reader.Next() {
		t.Fatalf("expected no further ClassAds after error")
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

// TestRangeIterator demonstrates the Go 1.23+ range-over-func syntax for All.
func TestRangeIterator(t *testing.T) {
	input := `[ID = 1]
[ID = 2]
[ID = 3]`

	ids := []int64{}

	for ad := range All(strings.NewReader(input)) {
		id, ok := ad.EvaluateAttrInt("ID")
		if !ok {
			t.Fatalf("expected ID attribute")
		}
		ids = append(ids, id)
	}

	expected := []int64{1, 2, 3}
	if len(ids) != len(expected) {
		t.Fatalf("expected %d ids, got %d", len(expected), len(ids))
	}
	for i, v := range expected {
		if ids[i] != v {
			t.Fatalf("expected ID=%d at index %d, got %d", v, i, ids[i])
		}
	}
}

func TestAllEarlyStop(t *testing.T) {
	input := `[V = 1][V = 2][V = 3]`
	count := 0

	All(strings.NewReader(input))(func(ad *ClassAd) bool {
		count++
		return false
	})

	if count != 1 {
		t.Fatalf("expected to stop after first element, got %d", count)
	}
}

func TestAllWithIndexEarlyStop(t *testing.T) {
	input := `[V = 1][V = 2][V = 3]`
	count := 0

	AllWithIndex(strings.NewReader(input))(func(i int, ad *ClassAd) bool {
		if i != 0 {
			t.Fatalf("expected first index 0, got %d", i)
		}
		count++
		return false
	})

	if count != 1 {
		t.Fatalf("expected to stop after first element, got %d", count)
	}
}

func TestAllWithErrorEarlyStop(t *testing.T) {
	input := `[V = 1][V = 2][V = 3]`
	count := 0
	var err error

	AllWithError(strings.NewReader(input), &err)(func(ad *ClassAd) bool {
		count++
		return false
	})

	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}

	if count != 1 {
		t.Fatalf("expected to stop after first element, got %d", count)
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

func TestAllOldWithError(t *testing.T) {
	input := `A = 1

B = 2`
	var err error
	count := 0

	for ad := range AllOldWithError(strings.NewReader(input), &err) {
		if _, ok := ad.EvaluateAttrInt("A"); ok {
			count++
		}
	}

	if err != nil {
		t.Fatalf("unexpected error on valid input: %v", err)
	}
	if count != 1 {
		t.Fatalf("expected 1 ClassAd from valid input, got %d", count)
	}

	broken := `Good = 1

Broken =`
	count = 0
	err = nil

	for range AllOldWithError(strings.NewReader(broken), &err) {
		count++
	}

	if err == nil {
		t.Fatalf("expected parse error for broken input")
	}
	if count != 1 {
		t.Fatalf("expected one yielded ad before error, got %d", count)
	}

	earliestop := `X = 1

Y = 2`
	err = nil
	count = 0

	for ad := range AllOldWithError(strings.NewReader(earliestop), &err) {
		count++
		_ = ad
		break // stop early to ensure no error is recorded
	}

	if err != nil {
		t.Fatalf("did not expect error when stopping early: %v", err)
	}
	if count != 1 {
		t.Fatalf("expected single iteration before early stop, got %d", count)
	}
}
