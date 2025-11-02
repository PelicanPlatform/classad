package classad

import (
	"testing"
)

func TestMarshal_SimpleStruct(t *testing.T) {
	type Job struct {
		ID       int
		Name     string
		CPUs     int
		Priority float64
		Active   bool
	}

	job := Job{
		ID:       123,
		Name:     "test-job",
		CPUs:     4,
		Priority: 10.5,
		Active:   true,
	}

	result, err := Marshal(job)
	if err != nil {
		t.Fatalf("Marshal failed: %v", err)
	}

	// Parse it back to verify
	ad, err := Parse(result)
	if err != nil {
		t.Fatalf("Failed to parse marshaled result: %v", err)
	}

	// Verify values
	id := ad.EvaluateAttr("ID")
	if !id.IsInteger() || id.String() != "123" {
		t.Errorf("Expected ID=123, got %v", id)
	}

	name := ad.EvaluateAttr("Name")
	nameVal, _ := name.StringValue()
	if nameVal != "test-job" {
		t.Errorf("Expected Name='test-job', got %q", nameVal)
	}
}

func TestMarshal_WithClassAdTags(t *testing.T) {
	type Job struct {
		JobID    int    `classad:"JobId"`
		UserName string `classad:"User"`
		CPUs     int    `classad:"RequestCpus"`
	}

	job := Job{
		JobID:    456,
		UserName: "alice",
		CPUs:     8,
	}

	result, err := Marshal(job)
	if err != nil {
		t.Fatalf("Marshal failed: %v", err)
	}

	ad, err := Parse(result)
	if err != nil {
		t.Fatalf("Failed to parse: %v", err)
	}

	// Verify the tags were used
	jobID := ad.EvaluateAttr("JobId")
	if !jobID.IsInteger() || jobID.String() != "456" {
		t.Errorf("Expected JobId=456, got %v", jobID)
	}

	user := ad.EvaluateAttr("User")
	userVal, _ := user.StringValue()
	if userVal != "alice" {
		t.Errorf("Expected User='alice', got %q", userVal)
	}
}

func TestMarshal_WithJSONTagsFallback(t *testing.T) {
	type Config struct {
		Timeout int    `json:"timeout"`
		Server  string `json:"server"`
		Port    int    // No tag, uses field name
	}

	config := Config{
		Timeout: 30,
		Server:  "example.com",
		Port:    8080,
	}

	result, err := Marshal(config)
	if err != nil {
		t.Fatalf("Marshal failed: %v", err)
	}

	ad, err := Parse(result)
	if err != nil {
		t.Fatalf("Failed to parse: %v", err)
	}

	// Verify json tags were used
	timeout := ad.EvaluateAttr("timeout")
	if timeout.String() != "30" {
		t.Errorf("Expected timeout=30, got %v", timeout)
	}

	// Verify field name was used when no tag present
	port := ad.EvaluateAttr("Port")
	if port.String() != "8080" {
		t.Errorf("Expected Port=8080, got %v", port)
	}
}

func TestMarshal_WithOmitEmpty(t *testing.T) {
	type Job struct {
		ID       int
		Name     string
		Optional string   `classad:"Optional,omitempty"`
		Tags     []string `classad:"Tags,omitempty"`
	}

	job := Job{
		ID:   123,
		Name: "test",
		// Optional and Tags are zero values
	}

	result, err := Marshal(job)
	if err != nil {
		t.Fatalf("Marshal failed: %v", err)
	}

	ad, err := Parse(result)
	if err != nil {
		t.Fatalf("Failed to parse: %v", err)
	}

	// Verify omitted fields are not present
	optional := ad.EvaluateAttr("Optional")
	if !optional.IsUndefined() {
		t.Errorf("Expected Optional to be omitted, got %v", optional)
	}

	tags := ad.EvaluateAttr("Tags")
	if !tags.IsUndefined() {
		t.Errorf("Expected Tags to be omitted, got %v", tags)
	}
}

func TestMarshal_SkipField(t *testing.T) {
	type Job struct {
		ID       int
		Internal string `classad:"-"`
		Secret   string `json:"-"`
	}

	job := Job{
		ID:       123,
		Internal: "skip-me",
		Secret:   "secret",
	}

	result, err := Marshal(job)
	if err != nil {
		t.Fatalf("Marshal failed: %v", err)
	}

	ad, err := Parse(result)
	if err != nil {
		t.Fatalf("Failed to parse: %v", err)
	}

	// Verify skipped fields are not present
	internal := ad.EvaluateAttr("Internal")
	if !internal.IsUndefined() {
		t.Errorf("Expected Internal to be skipped, got %v", internal)
	}

	secret := ad.EvaluateAttr("Secret")
	if !secret.IsUndefined() {
		t.Errorf("Expected Secret to be skipped, got %v", secret)
	}
}

func TestMarshal_NestedStruct(t *testing.T) {
	type Config struct {
		Timeout int
		Retries int
	}

	type Job struct {
		ID     int
		Config Config
	}

	job := Job{
		ID: 123,
		Config: Config{
			Timeout: 30,
			Retries: 3,
		},
	}

	result, err := Marshal(job)
	if err != nil {
		t.Fatalf("Marshal failed: %v", err)
	}

	ad, err := Parse(result)
	if err != nil {
		t.Fatalf("Failed to parse: %v", err)
	}

	// Verify nested struct
	config := ad.EvaluateAttr("Config")
	if !config.IsClassAd() {
		t.Fatalf("Expected Config to be a ClassAd, got %v", config.Type())
	}

	configAd, _ := config.ClassAdValue()
	timeout := configAd.EvaluateAttr("Timeout")
	if timeout.String() != "30" {
		t.Errorf("Expected Timeout=30, got %v", timeout)
	}
}

func TestMarshal_Slice(t *testing.T) {
	type Job struct {
		ID   int
		Tags []string
		Nums []int
	}

	job := Job{
		ID:   123,
		Tags: []string{"prod", "batch", "high-priority"},
		Nums: []int{1, 2, 3, 4, 5},
	}

	result, err := Marshal(job)
	if err != nil {
		t.Fatalf("Marshal failed: %v", err)
	}

	ad, err := Parse(result)
	if err != nil {
		t.Fatalf("Failed to parse: %v", err)
	}

	// Verify slices
	tags := ad.EvaluateAttr("Tags")
	if !tags.IsList() {
		t.Fatalf("Expected Tags to be a list, got %v", tags.Type())
	}
	tagsList, _ := tags.ListValue()
	if len(tagsList) != 3 {
		t.Errorf("Expected 3 tags, got %d", len(tagsList))
	}
}

func TestMarshal_Map(t *testing.T) {
	data := map[string]interface{}{
		"id":   123,
		"name": "test",
		"cpus": 4,
	}

	result, err := Marshal(data)
	if err != nil {
		t.Fatalf("Marshal failed: %v", err)
	}

	ad, err := Parse(result)
	if err != nil {
		t.Fatalf("Failed to parse: %v", err)
	}

	id := ad.EvaluateAttr("id")
	if id.String() != "123" {
		t.Errorf("Expected id=123, got %v", id)
	}
}

func TestUnmarshal_SimpleStruct(t *testing.T) {
	type Job struct {
		ID       int
		Name     string
		CPUs     int
		Priority float64
		Active   bool
	}

	classadStr := `[ID = 123; Name = "test-job"; CPUs = 4; Priority = 10.5; Active = true]`

	var job Job
	err := Unmarshal(classadStr, &job)
	if err != nil {
		t.Fatalf("Unmarshal failed: %v", err)
	}

	if job.ID != 123 {
		t.Errorf("Expected ID=123, got %d", job.ID)
	}
	if job.Name != "test-job" {
		t.Errorf("Expected Name='test-job', got %q", job.Name)
	}
	if job.CPUs != 4 {
		t.Errorf("Expected CPUs=4, got %d", job.CPUs)
	}
	if job.Priority != 10.5 {
		t.Errorf("Expected Priority=10.5, got %f", job.Priority)
	}
	if !job.Active {
		t.Errorf("Expected Active=true, got false")
	}
}

func TestUnmarshal_WithClassAdTags(t *testing.T) {
	type Job struct {
		JobID    int    `classad:"JobId"`
		UserName string `classad:"User"`
		CPUs     int    `classad:"RequestCpus"`
	}

	classadStr := `[JobId = 456; User = "alice"; RequestCpus = 8]`

	var job Job
	err := Unmarshal(classadStr, &job)
	if err != nil {
		t.Fatalf("Unmarshal failed: %v", err)
	}

	if job.JobID != 456 {
		t.Errorf("Expected JobID=456, got %d", job.JobID)
	}
	if job.UserName != "alice" {
		t.Errorf("Expected UserName='alice', got %q", job.UserName)
	}
	if job.CPUs != 8 {
		t.Errorf("Expected CPUs=8, got %d", job.CPUs)
	}
}

func TestUnmarshal_WithJSONTagsFallback(t *testing.T) {
	type Config struct {
		Timeout int    `json:"timeout"`
		Server  string `json:"server"`
		Port    int    // No tag
	}

	classadStr := `[timeout = 30; server = "example.com"; Port = 8080]`

	var config Config
	err := Unmarshal(classadStr, &config)
	if err != nil {
		t.Fatalf("Unmarshal failed: %v", err)
	}

	if config.Timeout != 30 {
		t.Errorf("Expected Timeout=30, got %d", config.Timeout)
	}
	if config.Server != "example.com" {
		t.Errorf("Expected Server='example.com', got %q", config.Server)
	}
	if config.Port != 8080 {
		t.Errorf("Expected Port=8080, got %d", config.Port)
	}
}

func TestUnmarshal_NestedStruct(t *testing.T) {
	type Config struct {
		Timeout int
		Retries int
	}

	type Job struct {
		ID     int
		Config Config
	}

	classadStr := `[ID = 123; Config = [Timeout = 30; Retries = 3]]`

	var job Job
	err := Unmarshal(classadStr, &job)
	if err != nil {
		t.Fatalf("Unmarshal failed: %v", err)
	}

	if job.ID != 123 {
		t.Errorf("Expected ID=123, got %d", job.ID)
	}
	if job.Config.Timeout != 30 {
		t.Errorf("Expected Config.Timeout=30, got %d", job.Config.Timeout)
	}
	if job.Config.Retries != 3 {
		t.Errorf("Expected Config.Retries=3, got %d", job.Config.Retries)
	}
}

func TestUnmarshal_Slice(t *testing.T) {
	type Job struct {
		ID   int
		Tags []string
		Nums []int
	}

	classadStr := `[ID = 123; Tags = {"prod", "batch", "high-priority"}; Nums = {1, 2, 3, 4, 5}]`

	var job Job
	err := Unmarshal(classadStr, &job)
	if err != nil {
		t.Fatalf("Unmarshal failed: %v", err)
	}

	if job.ID != 123 {
		t.Errorf("Expected ID=123, got %d", job.ID)
	}
	if len(job.Tags) != 3 {
		t.Errorf("Expected 3 tags, got %d", len(job.Tags))
	}
	if job.Tags[0] != "prod" {
		t.Errorf("Expected first tag='prod', got %q", job.Tags[0])
	}
	if len(job.Nums) != 5 {
		t.Errorf("Expected 5 nums, got %d", len(job.Nums))
	}
}

func TestUnmarshal_IntoMap(t *testing.T) {
	classadStr := `[ID = 123; Name = "test"; CPUs = 4]`

	var data map[string]interface{}
	err := Unmarshal(classadStr, &data)
	if err != nil {
		t.Fatalf("Unmarshal failed: %v", err)
	}

	if len(data) != 3 {
		t.Errorf("Expected 3 entries, got %d", len(data))
	}
}

func TestRoundTrip_ComplexStruct(t *testing.T) {
	type Metadata struct {
		Created  string
		Modified string
	}

	type Job struct {
		ID       int                    `classad:"JobId"`
		Name     string                 `classad:"Name"`
		CPUs     int                    `json:"cpus"`
		Tags     []string               `classad:"Tags"`
		Metadata Metadata               `classad:"Metadata"`
		Extra    map[string]interface{} `classad:"Extra,omitempty"`
	}

	original := Job{
		ID:   789,
		Name: "complex-job",
		CPUs: 16,
		Tags: []string{"prod", "critical"},
		Metadata: Metadata{
			Created:  "2024-01-01",
			Modified: "2024-01-02",
		},
	}

	// Marshal
	classadStr, err := Marshal(original)
	if err != nil {
		t.Fatalf("Marshal failed: %v", err)
	}

	// Unmarshal
	var restored Job
	err = Unmarshal(classadStr, &restored)
	if err != nil {
		t.Fatalf("Unmarshal failed: %v", err)
	}

	// Compare
	if restored.ID != original.ID {
		t.Errorf("ID mismatch: expected %d, got %d", original.ID, restored.ID)
	}
	if restored.Name != original.Name {
		t.Errorf("Name mismatch: expected %q, got %q", original.Name, restored.Name)
	}
	if restored.CPUs != original.CPUs {
		t.Errorf("CPUs mismatch: expected %d, got %d", original.CPUs, restored.CPUs)
	}
	if len(restored.Tags) != len(original.Tags) {
		t.Errorf("Tags length mismatch: expected %d, got %d", len(original.Tags), len(restored.Tags))
	}
	if restored.Metadata.Created != original.Metadata.Created {
		t.Errorf("Metadata.Created mismatch")
	}
}

func TestUnmarshal_IgnoreUnknownFields(t *testing.T) {
	type Job struct {
		ID   int
		Name string
	}

	classadStr := `[ID = 123; Name = "test"; ExtraField = "ignored"; AnotherField = 999]`

	var job Job
	err := Unmarshal(classadStr, &job)
	if err != nil {
		t.Fatalf("Unmarshal failed: %v", err)
	}

	// Should succeed and ignore unknown fields
	if job.ID != 123 {
		t.Errorf("Expected ID=123, got %d", job.ID)
	}
	if job.Name != "test" {
		t.Errorf("Expected Name='test', got %q", job.Name)
	}
}

func TestUnmarshal_TypeConversion(t *testing.T) {
	type Job struct {
		IntField   int
		FloatField float64
		UintField  uint
	}

	// Int to float conversion
	classadStr1 := `[IntField = 10; FloatField = 20; UintField = 30]`
	var job1 Job
	if err := Unmarshal(classadStr1, &job1); err != nil {
		t.Fatalf("Unmarshal failed: %v", err)
	}
	if job1.FloatField != 20.0 {
		t.Errorf("Expected FloatField=20.0, got %f", job1.FloatField)
	}

	// Float to int conversion (truncation)
	classadStr2 := `[IntField = 10.7; FloatField = 20.3; UintField = 30.9]`
	var job2 Job
	if err := Unmarshal(classadStr2, &job2); err != nil {
		t.Fatalf("Unmarshal failed: %v", err)
	}
	if job2.IntField != 10 {
		t.Errorf("Expected IntField=10, got %d", job2.IntField)
	}
}

func TestMarshal_EmptyStruct(t *testing.T) {
	type Empty struct{}

	result, err := Marshal(Empty{})
	if err != nil {
		t.Fatalf("Marshal failed: %v", err)
	}

	// Should produce empty ClassAd
	if result != "[]" {
		t.Errorf("Expected '[]', got %q", result)
	}
}

func TestMarshal_ClassAdField(t *testing.T) {
	type Container struct {
		Name   string
		Config *ClassAd
	}

	config := New()
	config.InsertAttr("timeout", 30)
	config.InsertAttrString("server", "example.com")

	container := Container{
		Name:   "mycontainer",
		Config: config,
	}

	result, err := Marshal(container)
	if err != nil {
		t.Fatalf("Marshal failed: %v", err)
	}

	// Parse it back
	ad, err := Parse(result)
	if err != nil {
		t.Fatalf("Failed to parse marshaled result: %v", err)
	}

	// Verify the nested ClassAd
	configVal := ad.EvaluateAttr("Config")
	if !configVal.IsClassAd() {
		t.Fatalf("Expected Config to be a ClassAd, got %v", configVal.Type())
	}

	configAd, _ := configVal.ClassAdValue()
	timeout, ok := configAd.EvaluateAttrInt("timeout")
	if !ok || timeout != 30 {
		t.Errorf("Expected timeout=30, got %d", timeout)
	}

	server, ok := configAd.EvaluateAttrString("server")
	if !ok || server != "example.com" {
		t.Errorf("Expected server='example.com', got %q", server)
	}
}

func TestUnmarshal_ClassAdField(t *testing.T) {
	type Container struct {
		Name   string
		Config *ClassAd
	}

	input := `[Name = "mycontainer"; Config = [timeout = 30; server = "example.com"]]`

	var container Container
	err := Unmarshal(input, &container)
	if err != nil {
		t.Fatalf("Unmarshal failed: %v", err)
	}

	if container.Name != "mycontainer" {
		t.Errorf("Expected Name='mycontainer', got %q", container.Name)
	}

	if container.Config == nil {
		t.Fatal("Expected Config to be non-nil")
	}

	timeout, ok := container.Config.EvaluateAttrInt("timeout")
	if !ok || timeout != 30 {
		t.Errorf("Expected timeout=30, got %d", timeout)
	}

	server, ok := container.Config.EvaluateAttrString("server")
	if !ok || server != "example.com" {
		t.Errorf("Expected server='example.com', got %q", server)
	}
}

func TestMarshal_ExprField(t *testing.T) {
	type Job struct {
		Name    string
		CPUs    int
		Memory  *Expr
		Formula *Expr
	}

	memExpr, _ := ParseExpr("RequestMemory * 1024")
	formulaExpr, _ := ParseExpr("Cpus * 2 + 8")

	job := Job{
		Name:    "test-job",
		CPUs:    4,
		Memory:  memExpr,
		Formula: formulaExpr,
	}

	result, err := Marshal(job)
	if err != nil {
		t.Fatalf("Marshal failed: %v", err)
	}

	// Parse it back
	ad, err := Parse(result)
	if err != nil {
		t.Fatalf("Failed to parse marshaled result: %v", err)
	}

	// Verify expressions are preserved (not evaluated)
	memoryExpr, ok := ad.Lookup("Memory")
	if !ok {
		t.Fatal("Expected Memory attribute")
	}
	// Parser adds parentheses for operator precedence
	if memoryExpr.String() != "(RequestMemory * 1024)" {
		t.Errorf("Expected Memory='(RequestMemory * 1024)', got %q", memoryExpr.String())
	}

	formulaExpr2, ok := ad.Lookup("Formula")
	if !ok {
		t.Fatal("Expected Formula attribute")
	}
	// Parser adds parentheses for operator precedence
	if formulaExpr2.String() != "((Cpus * 2) + 8)" {
		t.Errorf("Expected Formula='((Cpus * 2) + 8)', got %q", formulaExpr2.String())
	}
}

func TestUnmarshal_ExprField(t *testing.T) {
	type Job struct {
		Name    string
		CPUs    int
		Memory  *Expr
		Formula *Expr
	}

	input := `[Name = "test-job"; CPUs = 4; Memory = RequestMemory * 1024; Formula = CPUs * 2 + 8]`

	var job Job
	err := Unmarshal(input, &job)
	if err != nil {
		t.Fatalf("Unmarshal failed: %v", err)
	}

	if job.Name != "test-job" {
		t.Errorf("Expected Name='test-job', got %q", job.Name)
	}

	if job.CPUs != 4 {
		t.Errorf("Expected CPUs=4, got %d", job.CPUs)
	}

	if job.Memory == nil {
		t.Fatal("Expected Memory to be non-nil")
	}
	// Parser adds parentheses for operator precedence
	if job.Memory.String() != "(RequestMemory * 1024)" {
		t.Errorf("Expected Memory='(RequestMemory * 1024)', got %q", job.Memory.String())
	}

	if job.Formula == nil {
		t.Fatal("Expected Formula to be non-nil")
	}
	// Parser adds parentheses for operator precedence
	if job.Formula.String() != "((CPUs * 2) + 8)" {
		t.Errorf("Expected Formula='((CPUs * 2) + 8)', got %q", job.Formula.String())
	}

	// Test that we can evaluate the expression with context
	testAd := New()
	testAd.InsertAttr("RequestMemory", 2048)
	testAd.InsertAttr("CPUs", 4)

	memVal := job.Memory.Eval(testAd)
	if !memVal.IsInteger() {
		t.Errorf("Expected Memory to evaluate to integer, got %v", memVal.Type())
	}
	memInt, _ := memVal.IntValue()
	if memInt != 2048*1024 {
		t.Errorf("Expected Memory to evaluate to %d, got %d", 2048*1024, memInt)
	}

	formulaVal := job.Formula.Eval(testAd)
	if !formulaVal.IsInteger() {
		t.Errorf("Expected Formula to evaluate to integer, got %v", formulaVal.Type())
	}
	formulaInt, _ := formulaVal.IntValue()
	if formulaInt != 16 {
		t.Errorf("Expected Formula to evaluate to 16, got %d", formulaInt)
	}
}

func TestMarshal_NilClassAdAndExprFields(t *testing.T) {
	type Container struct {
		Name    string
		Config  *ClassAd
		Formula *Expr
	}

	container := Container{
		Name:    "test",
		Config:  nil,
		Formula: nil,
	}

	result, err := Marshal(container)
	if err != nil {
		t.Fatalf("Marshal failed: %v", err)
	}

	// Parse it back
	ad, err := Parse(result)
	if err != nil {
		t.Fatalf("Failed to parse marshaled result: %v", err)
	}

	// Verify nil fields are marshaled as undefined
	configVal := ad.EvaluateAttr("Config")
	if !configVal.IsUndefined() {
		t.Errorf("Expected Config to be undefined, got %v", configVal.Type())
	}

	formulaVal := ad.EvaluateAttr("Formula")
	if !formulaVal.IsUndefined() {
		t.Errorf("Expected Formula to be undefined, got %v", formulaVal.Type())
	}
}
