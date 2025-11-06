package classad

import (
	"strings"
	"testing"
)

// Test patterns from api_demo
func TestAPIDemoPatterns(t *testing.T) {
	// Creating a ClassAd programmatically with generic Set() API
	ad := New()
	ad.Set("Cpus", 4)
	ad.Set("Memory", 8192.0)
	ad.Set("Name", "worker-01")
	ad.Set("IsAvailable", true)

	if ad.Size() != 4 {
		t.Errorf("Expected 4 attributes, got %d", ad.Size())
	}

	// Parsing ClassAds from strings
	jobAd, err := Parse(`[
		JobId = 1001;
		Owner = "alice";
		Cpus = 2;
		Memory = 4096;
		Requirements = (Cpus >= 2) && (Memory >= 2048);
		Status = "Running"
	]`)
	if err != nil {
		t.Fatalf("Parse failed: %v", err)
	}

	// Looking up attributes
	if expr, ok := jobAd.Lookup("JobId"); !ok {
		t.Error("Expected to find JobId")
	} else if expr.String() == "" {
		t.Error("Expected non-empty expression string")
	}

	// Type-safe retrieval with GetAs[T]()
	if jobId, ok := GetAs[int](jobAd, "JobId"); !ok || jobId != 1001 {
		t.Errorf("Expected JobId=1001, got %d, ok=%v", jobId, ok)
	}

	if owner, ok := GetAs[string](jobAd, "Owner"); !ok || owner != "alice" {
		t.Errorf("Expected Owner='alice', got %q, ok=%v", owner, ok)
	}

	// Using GetOr[T]() with defaults
	status := GetOr(jobAd, "Status", "Unknown")
	if status != "Running" {
		t.Errorf("Expected Status='Running', got %q", status)
	}

	priority := GetOr(jobAd, "Priority", 10) // Missing, uses default
	if priority != 10 {
		t.Errorf("Expected default Priority=10, got %d", priority)
	}

	// Evaluating complex expressions
	if requirements, ok := jobAd.EvaluateAttrBool("Requirements"); !ok {
		t.Error("Expected Requirements to evaluate to bool")
	} else if !requirements {
		t.Error("Expected Requirements to be true")
	}

	// Using EvaluateAttr
	val := jobAd.EvaluateAttr("JobId")
	if !val.IsInteger() {
		t.Error("Expected JobId to be integer")
	}
}

// Test patterns from reader_demo
func TestReaderDemoPatterns(t *testing.T) {
	// Reading new-style ClassAds
	newStyleAds := `
[JobId = 1; Owner = "alice"; Cpus = 2; Memory = 2048]
[JobId = 2; Owner = "bob"; Cpus = 4; Memory = 4096]
[JobId = 3; Owner = "charlie"; Cpus = 1; Memory = 1024]
`
	reader := NewReader(strings.NewReader(newStyleAds))

	count := 0
	for reader.Next() {
		ad := reader.ClassAd()
		jobId := GetOr(ad, "JobId", 0)
		owner := GetOr(ad, "Owner", "unknown")
		cpus := GetOr(ad, "Cpus", 0)
		memory := GetOr(ad, "Memory", 0)

		if jobId == 0 || owner == "unknown" || cpus == 0 || memory == 0 {
			t.Errorf("Unexpected values: JobId=%d, Owner=%s, Cpus=%d, Memory=%d",
				jobId, owner, cpus, memory)
		}
		count++
	}

	if err := reader.Err(); err != nil {
		t.Fatalf("Error reading ClassAds: %v", err)
	}

	if count != 3 {
		t.Errorf("Expected to read 3 ClassAds, got %d", count)
	}

	// Reading old-style ClassAds
	oldStyleAds := `MyType = "Machine"
Name = "worker01.example.com"
Cpus = 8
Memory = 16384
Arch = "X86_64"

MyType = "Machine"
Name = "worker02.example.com"
Cpus = 16
Memory = 32768
Arch = "X86_64"
`
	oldReader := NewOldReader(strings.NewReader(oldStyleAds))

	oldCount := 0
	for oldReader.Next() {
		ad := oldReader.ClassAd()
		name := GetOr(ad, "Name", "unknown")
		cpus := GetOr(ad, "Cpus", 0)
		memory := GetOr(ad, "Memory", 0)
		arch := GetOr(ad, "Arch", "unknown")

		if name == "unknown" || cpus == 0 || memory == 0 || arch == "unknown" {
			t.Errorf("Unexpected values: Name=%s, Cpus=%d, Memory=%d, Arch=%s",
				name, cpus, memory, arch)
		}
		oldCount++
	}

	if err := oldReader.Err(); err != nil {
		t.Fatalf("Error reading old-style ClassAds: %v", err)
	}

	if oldCount != 2 {
		t.Errorf("Expected to read 2 old-style ClassAds, got %d", oldCount)
	}

	// Processing with filtering
	filterAds := `
[JobId = 100; Cpus = 2; Priority = 10]
[JobId = 101; Cpus = 8; Priority = 5]
[JobId = 102; Cpus = 4; Priority = 8]
[JobId = 103; Cpus = 1; Priority = 3]
[JobId = 104; Cpus = 16; Priority = 9]
`
	filterReader := NewReader(strings.NewReader(filterAds))

	highCpuCount := 0
	for filterReader.Next() {
		ad := filterReader.ClassAd()
		cpus, ok := GetAs[int](ad, "Cpus")
		if !ok {
			continue
		}

		if cpus >= 4 {
			highCpuCount++
		}
	}

	if highCpuCount != 3 {
		t.Errorf("Expected 3 high-CPU jobs, got %d", highCpuCount)
	}
}

// Test patterns from struct_demo
func TestStructDemoPatterns(t *testing.T) {
	// Simple struct marshaling
	type Job struct {
		ID       int
		Name     string
		CPUs     int
		Memory   int
		Priority float64
		Active   bool
	}

	job := Job{
		ID:       12345,
		Name:     "data-processing-job",
		CPUs:     8,
		Memory:   16384,
		Priority: 10.5,
		Active:   true,
	}

	classadStr, err := Marshal(job)
	if err != nil {
		t.Fatalf("Marshal failed: %v", err)
	}

	if classadStr == "" {
		t.Error("Expected non-empty ClassAd string")
	}

	// Unmarshal it back
	var jobCopy Job
	err = Unmarshal(classadStr, &jobCopy)
	if err != nil {
		t.Fatalf("Unmarshal failed: %v", err)
	}

	if jobCopy.ID != job.ID {
		t.Errorf("ID mismatch: expected %d, got %d", job.ID, jobCopy.ID)
	}
	if jobCopy.Name != job.Name {
		t.Errorf("Name mismatch: expected %s, got %s", job.Name, jobCopy.Name)
	}

	// Using struct tags
	type HTCondorJob struct {
		JobID         int      `classad:"ClusterId"`
		ProcID        int      `classad:"ProcId"`
		Owner         string   `classad:"Owner"`
		RequestCPUs   int      `classad:"RequestCpus"`
		RequestMemory int      `json:"request_memory"`
		Requirements  []string `classad:"Requirements"`
		Rank          int
	}

	htcJob := HTCondorJob{
		JobID:         100,
		ProcID:        0,
		Owner:         "alice",
		RequestCPUs:   4,
		RequestMemory: 8192,
		Requirements:  []string{"OpSysAndVer == \"RedHat8\"", "Arch == \"X86_64\""},
		Rank:          5,
	}

	classadStr, err = Marshal(htcJob)
	if err != nil {
		t.Fatalf("Marshal with tags failed: %v", err)
	}

	if !strings.Contains(classadStr, "ClusterId") {
		t.Error("Expected ClusterId in marshaled output")
	}

	// Omitempty and skip fields
	type JobWithOptions struct {
		ID       int
		Name     string
		Optional string   `classad:"Optional,omitempty"`
		Tags     []string `classad:"Tags,omitempty"`
		Internal string   `classad:"-"`
	}

	jobOpts := JobWithOptions{
		ID:       123,
		Name:     "test-job",
		Internal: "secret-data",
	}

	classadStr, err = Marshal(jobOpts)
	if err != nil {
		t.Fatalf("Marshal with options failed: %v", err)
	}

	if strings.Contains(classadStr, "Internal") {
		t.Error("Internal field should not be marshaled")
	}
	if strings.Contains(classadStr, "secret-data") {
		t.Error("Internal value should not appear in output")
	}
}

// Test patterns from json_demo
func TestJSONDemoPatterns(t *testing.T) {
	// Marshal to JSON
	ad := New()
	ad.InsertAttr("JobId", 1001)
	ad.InsertAttrString("Owner", "alice")
	ad.InsertAttr("Cpus", 4)
	ad.InsertAttrFloat("Memory", 8192.5)
	ad.InsertAttrBool("Active", true)
	InsertAttrList(ad, "Tags", []string{"production", "high-priority"})

	// Create nested ClassAd
	nested := New()
	nested.InsertAttrString("Type", "Docker")
	nested.InsertAttrString("Image", "alpine:latest")
	ad.InsertAttrClassAd("Container", nested)

	jsonBytes, err := ad.MarshalJSON()
	if err != nil {
		t.Fatalf("MarshalJSON failed: %v", err)
	}

	if len(jsonBytes) == 0 {
		t.Error("Expected non-empty JSON output")
	}

	// Unmarshal from JSON
	var ad2 ClassAd
	err = ad2.UnmarshalJSON(jsonBytes)
	if err != nil {
		t.Fatalf("UnmarshalJSON failed: %v", err)
	}

	// Verify attributes
	if jobId, ok := GetAs[int](&ad2, "JobId"); !ok || jobId != 1001 {
		t.Errorf("JobId mismatch: expected 1001, got %d, ok=%v", jobId, ok)
	}

	if owner, ok := GetAs[string](&ad2, "Owner"); !ok || owner != "alice" {
		t.Errorf("Owner mismatch: expected 'alice', got %q, ok=%v", owner, ok)
	}
}

// Test patterns from generic_api_demo
func TestGenericAPIDemoPatterns(t *testing.T) {
	// Create ClassAd with various types using Set
	ad := New()

	// Basic types
	err := ad.Set("IntValue", 42)
	if err != nil {
		t.Errorf("Set int failed: %v", err)
	}

	err = ad.Set("FloatValue", 3.14)
	if err != nil {
		t.Errorf("Set float failed: %v", err)
	}

	err = ad.Set("StringValue", "hello")
	if err != nil {
		t.Errorf("Set string failed: %v", err)
	}

	err = ad.Set("BoolValue", true)
	if err != nil {
		t.Errorf("Set bool failed: %v", err)
	}

	// Slices
	err = ad.Set("IntList", []int{1, 2, 3, 4, 5})
	if err != nil {
		t.Errorf("Set int slice failed: %v", err)
	}

	err = ad.Set("StringList", []string{"a", "b", "c"})
	if err != nil {
		t.Errorf("Set string slice failed: %v", err)
	}

	// Nested ClassAd
	nested := New()
	nested.Set("X", 10)
	nested.Set("Y", 20)
	err = ad.Set("Nested", nested)
	if err != nil {
		t.Errorf("Set nested ClassAd failed: %v", err)
	}

	// Retrieve with GetAs
	if intVal, ok := GetAs[int](ad, "IntValue"); !ok || intVal != 42 {
		t.Errorf("GetAs int failed: got %d, ok=%v", intVal, ok)
	}

	if floatVal, ok := GetAs[float64](ad, "FloatValue"); !ok || floatVal != 3.14 {
		t.Errorf("GetAs float failed: got %f, ok=%v", floatVal, ok)
	}

	if strVal, ok := GetAs[string](ad, "StringValue"); !ok || strVal != "hello" {
		t.Errorf("GetAs string failed: got %q, ok=%v", strVal, ok)
	}

	if boolVal, ok := GetAs[bool](ad, "BoolValue"); !ok || !boolVal {
		t.Errorf("GetAs bool failed: got %v, ok=%v", boolVal, ok)
	}

	// Retrieve with GetOr (with defaults)
	missingInt := GetOr(ad, "MissingInt", 999)
	if missingInt != 999 {
		t.Errorf("GetOr default failed: got %d", missingInt)
	}

	existingInt := GetOr(ad, "IntValue", 999)
	if existingInt != 42 {
		t.Errorf("GetOr existing value failed: got %d", existingInt)
	}
}

// Test patterns from introspection_demo
func TestIntrospectionDemoPatterns(t *testing.T) {
	ad, err := Parse(`[
		X = 10;
		Y = 20;
		Sum = X + Y;
		Product = X * Y;
		Status = "Active"
	]`)
	if err != nil {
		t.Fatalf("Parse failed: %v", err)
	}

	// Get all attribute names
	attrs := ad.GetAttributes()
	if len(attrs) != 5 {
		t.Errorf("Expected 5 attributes, got %d", len(attrs))
	}

	// Lookup and inspect expressions
	if expr, ok := ad.Lookup("Sum"); !ok {
		t.Error("Expected to find Sum")
	} else {
		exprStr := expr.String()
		if !strings.Contains(exprStr, "+") {
			t.Errorf("Expected '+' in expression, got %q", exprStr)
		}
	}

	// External and internal references
	testExpr, _ := ParseExpr("X + Y + Z")
	external := ad.ExternalRefs(testExpr)
	if len(external) != 1 || external[0] != "Z" {
		t.Errorf("Expected external ref [Z], got %v", external)
	}

	internal := ad.InternalRefs(testExpr)
	if len(internal) != 2 {
		t.Errorf("Expected 2 internal refs, got %d", len(internal))
	}

	// Flatten expression
	flattened := ad.Flatten(testExpr)
	if flattened == nil {
		t.Error("Expected non-nil flattened expression")
	}
}

// Test patterns from features_demo (MatchClassAd)
func TestMatchClassAdDemoPatterns(t *testing.T) {
	// Create job and machine ClassAds
	job, err := Parse(`[
		MyType = "Job";
		RequestCpus = 4;
		RequestMemory = 8192;
		Requirements = TARGET.Cpus >= 4 && TARGET.Memory >= 8192;
		Rank = TARGET.Cpus
	]`)
	if err != nil {
		t.Fatalf("Parse job failed: %v", err)
	}

	machine, err := Parse(`[
		MyType = "Machine";
		Cpus = 8;
		Memory = 16384;
		Requirements = TARGET.RequestCpus <= Cpus;
		Rank = 1
	]`)
	if err != nil {
		t.Fatalf("Parse machine failed: %v", err)
	}

	// Create MatchClassAd
	match := NewMatchClassAd(job, machine)

	// Test symmetry
	symmetric := match.Symmetry("Requirements", "Requirements")
	if !symmetric {
		t.Error("Expected symmetric match")
	}

	// Also test Match() which is equivalent
	if !match.Match() {
		t.Error("Expected Match() to return true")
	}

	// Get left and right
	if match.GetLeftAd() != job {
		t.Error("GetLeftAd returned wrong ad")
	}
	if match.GetRightAd() != machine {
		t.Error("GetRightAd returned wrong ad")
	}

	// Evaluate rank
	leftRank := match.EvaluateAttrLeft("Rank")
	if !leftRank.IsInteger() {
		t.Error("Expected integer rank from left")
	}

	rightRank := match.EvaluateAttrRight("Rank")
	if !rightRank.IsInteger() {
		t.Error("Expected integer rank from right")
	}
}

// Test patterns for expression evaluation
func TestExpressionEvaluationPatterns(t *testing.T) {
	ad := New()
	ad.InsertAttr("A", 10)
	ad.InsertAttr("B", 20)
	ad.InsertAttr("C", 30)

	// Parse and evaluate expressions
	tests := []struct {
		expr     string
		expected int64
	}{
		{"A + B", 30},
		{"A * B", 200},
		{"C - A", 20},
		{"B / 2", 10},
		{"(A + B) * C", 900},
	}

	for _, tt := range tests {
		expr, err := ParseExpr(tt.expr)
		if err != nil {
			t.Errorf("ParseExpr(%q) failed: %v", tt.expr, err)
			continue
		}

		result := expr.Eval(ad)
		if !result.IsInteger() {
			t.Errorf("Expression %q did not evaluate to integer", tt.expr)
			continue
		}

		intVal, _ := result.IntValue()
		if intVal != tt.expected {
			t.Errorf("Expression %q: expected %d, got %d", tt.expr, tt.expected, intVal)
		}
	}
}

// Test patterns for list operations
func TestListOperationPatterns(t *testing.T) {
	ad := New()

	// Create lists of different types
	InsertAttrList(ad, "Numbers", []int64{1, 2, 3, 4, 5})
	InsertAttrList(ad, "Names", []string{"Alice", "Bob", "Charlie"})
	InsertAttrList(ad, "Flags", []bool{true, false, true})
	InsertAttrList(ad, "Reals", []float64{1.1, 2.2, 3.3})

	// Verify list lengths
	if ad.Size() != 4 {
		t.Errorf("Expected 4 attributes, got %d", ad.Size())
	}

	// Use list functions
	expr, _ := ParseExpr("size(Numbers)")
	result := expr.Eval(ad)
	if !result.IsInteger() {
		t.Error("size(Numbers) should return integer")
	} else {
		size, _ := result.IntValue()
		if size != 5 {
			t.Errorf("Expected size 5, got %d", size)
		}
	}

	// Test member function
	expr2, _ := ParseExpr("member(3, Numbers)")
	result2 := expr2.Eval(ad)
	if !result2.IsBool() {
		t.Error("member() should return boolean")
	} else {
		isMember, _ := result2.BoolValue()
		if !isMember {
			t.Error("Expected 3 to be member of Numbers")
		}
	}
}

// Test patterns for string operations
func TestStringOperationPatterns(t *testing.T) {
	ad := New()
	ad.InsertAttrString("Text", "Hello World")
	ad.InsertAttrString("Lower", "hello")
	ad.InsertAttrString("Upper", "HELLO")

	tests := []struct {
		expr     string
		expected string
	}{
		{"strcat(\"Hello\", \" \", \"World\")", "Hello World"},
		{"substr(Text, 0, 5)", "Hello"},
		{"toLower(Text)", "hello world"},
		{"toUpper(Lower)", "HELLO"},
	}

	for _, tt := range tests {
		expr, err := ParseExpr(tt.expr)
		if err != nil {
			t.Errorf("ParseExpr(%q) failed: %v", tt.expr, err)
			continue
		}

		result := expr.Eval(ad)
		if !result.IsString() {
			t.Errorf("Expression %q did not evaluate to string", tt.expr)
			continue
		}

		strVal, _ := result.StringValue()
		if strVal != tt.expected {
			t.Errorf("Expression %q: expected %q, got %q", tt.expr, tt.expected, strVal)
		}
	}
}

// Test old format marshaling
func TestOldFormatPatterns(t *testing.T) {
	ad := New()
	ad.InsertAttr("Cpus", 4)
	ad.InsertAttrString("Name", "worker01")
	ad.InsertAttr("Memory", 8192)

	oldFmt := ad.MarshalOld()
	if oldFmt == "" {
		t.Error("Expected non-empty old format output")
	}

	if !strings.Contains(oldFmt, "Cpus = 4") {
		t.Error("Expected 'Cpus = 4' in old format")
	}
	if !strings.Contains(oldFmt, "Name = \"worker01\"") {
		t.Error("Expected 'Name = \"worker01\"' in old format")
	}

	// Parse it back
	ad2, err := ParseOld(oldFmt)
	if err != nil {
		t.Fatalf("ParseOld failed: %v", err)
	}

	if cpus, ok := GetAs[int](ad2, "Cpus"); !ok || cpus != 4 {
		t.Errorf("Cpus mismatch after parse: got %d, ok=%v", cpus, ok)
	}
}

// Test Delete and Clear operations
func TestDeleteClearPatterns(t *testing.T) {
	ad := New()
	ad.InsertAttr("A", 1)
	ad.InsertAttr("B", 2)
	ad.InsertAttr("C", 3)

	if ad.Size() != 3 {
		t.Errorf("Expected size 3, got %d", ad.Size())
	}

	// Delete attribute
	deleted := ad.Delete("B")
	if !deleted {
		t.Error("Expected Delete to return true")
	}

	if ad.Size() != 2 {
		t.Errorf("Expected size 2 after delete, got %d", ad.Size())
	}

	if _, ok := ad.Lookup("B"); ok {
		t.Error("B should not exist after delete")
	}

	// Clear all
	ad.Clear()
	if ad.Size() != 0 {
		t.Errorf("Expected size 0 after clear, got %d", ad.Size())
	}
}
