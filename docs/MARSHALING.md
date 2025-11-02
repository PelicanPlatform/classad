# Struct Marshaling and Unmarshaling Guide

This guide explains how to marshal and unmarshal Go structs to/from ClassAd format and JSON format using the `classad` package.

## Table of Contents

- [Overview](#overview)
- [ClassAd Format Marshaling](#classad-format-marshaling)
- [JSON Format Marshaling](#json-format-marshaling)
- [Struct Tags](#struct-tags)
- [Advanced Features](#advanced-features)
- [Examples](#examples)
- [Best Practices](#best-practices)

## Overview

The `classad` package provides two complementary marshaling systems:

1. **ClassAd Format**: Native HTCondor ClassAd text format
2. **JSON Format**: Standard JSON with special handling for expressions

Both systems support the same struct tag conventions for a consistent API.

## ClassAd Format Marshaling

### Basic Usage

```go
import "github.com/PelicanPlatform/classad/classad"

// Define your struct
type Job struct {
    ID       int
    Name     string
    CPUs     int
    Priority float64
}

// Marshal to ClassAd format
job := Job{ID: 123, Name: "test", CPUs: 4, Priority: 10.5}
classadStr, err := classad.Marshal(job)
// Result: [ID = 123; Name = "test"; CPUs = 4; Priority = 10.5]

// Unmarshal from ClassAd format
var job2 Job
err = classad.Unmarshal(classadStr, &job2)
```

### Supported Types

| Go Type | ClassAd Representation |
|---------|----------------------|
| `int`, `int8`, `int16`, `int32`, `int64` | Integer literal |
| `uint`, `uint8`, `uint16`, `uint32`, `uint64` | Integer literal |
| `float32`, `float64` | Real literal |
| `string` | String literal (quoted) |
| `bool` | Boolean literal (`true`/`false`) |
| `[]T`, `[N]T` | List literal `{...}` |
| `struct` | Nested ClassAd `[...]` |
| `map[string]T` | ClassAd record `[...]` |
| `*classad.ClassAd` | Nested ClassAd `[...]` (flexible) |
| `*classad.Expr` | Unevaluated expression |
| `*T` | Dereferences pointer, `undefined` if nil |

## JSON Format Marshaling

### Basic Usage

```go
import (
    "encoding/json"
    "github.com/PelicanPlatform/classad/classad"
)

type Job struct {
    ID   int
    Name string
}

job := Job{ID: 123, Name: "test"}

// ClassAd implements json.Marshaler interface
ad, _ := classad.Parse(`[ID = 123; Name = "test"]`)
jsonBytes, err := json.Marshal(ad)
// Result: {"ID":123,"Name":"test"}

// ClassAd implements json.Unmarshaler interface
var ad2 classad.ClassAd
err = json.Unmarshal(jsonBytes, &ad2)
```

### Expression Handling in JSON

Complex expressions are serialized with a special format: `/Expr(...)/`

```go
ad, _ := classad.Parse(`[x = 5; y = x + 3]`)
jsonBytes, _ := json.Marshal(ad)
// Result: {"x":5,"y":"\/Expr(x + 3)\/"}
```

When unmarshaling, the JSON library automatically converts `\/` to `/`, so the expression is correctly recognized.

## Struct Tags

### Tag Priority

The package uses struct tags to control field names and behavior:

1. **Primary**: `classad:"name"` - Used for both ClassAd and JSON marshaling
2. **Fallback**: `json:"name"` - Used if no `classad` tag is present
3. **Default**: Field name - Used if no tags are present

### Tag Syntax

```go
type Job struct {
    // Use custom name in ClassAd/JSON
    JobID int `classad:"ClusterId"`

    // Falls back to json tag
    Memory int `json:"request_memory"`

    // Uses field name "CPUs"
    CPUs int

    // Skip this field entirely
    Internal string `classad:"-"`

    // Omit if zero value
    Optional string `classad:"Optional,omitempty"`

    // Can also use json tag with omitempty
    Tags []string `json:"tags,omitempty"`
}
```

### Tag Options

| Option | Description | Example |
|--------|-------------|---------|
| Custom name | Sets the field name | `classad:"ClusterId"` |
| `-` | Skip field entirely | `classad:"-"` |
| `omitempty` | Omit zero values | `classad:"name,omitempty"` |

## Advanced Features

### Nested Structs

Both systems support nested structs:

```go
type Resources struct {
    CPUs   int
    Memory int
}

type Job struct {
    ID        int
    Resources Resources
}

job := Job{
    ID: 123,
    Resources: Resources{CPUs: 4, Memory: 8192},
}

// ClassAd format
str, _ := classad.Marshal(job)
// [ID = 123; Resources = [CPUs = 4; Memory = 8192]]

// JSON format
ad, _ := classad.Parse(str)
jsonBytes, _ := json.Marshal(ad)
// {"ID":123,"Resources":{"CPUs":4,"Memory":8192}}
```

### Lists and Slices

```go
type Job struct {
    ID   int
    Tags []string
    Nums []int
}

job := Job{
    ID:   123,
    Tags: []string{"prod", "critical"},
    Nums: []int{1, 2, 3},
}

// ClassAd: [ID = 123; Tags = {"prod", "critical"}; Nums = {1, 2, 3}]
// JSON: {"ID":123,"Tags":["prod","critical"],"Nums":[1,2,3]}
```

### Map Support

```go
// Map with interface{} values
data := map[string]interface{}{
    "id":   123,
    "name": "test",
    "cpus": 4,
}

// ClassAd format
str, _ := classad.Marshal(data)
// [id = 123; name = "test"; cpus = 4]

// Can also unmarshal into map
var restored map[string]interface{}
classad.Unmarshal(str, &restored)
```

### ClassAd and Expr Fields

You can include `*classad.ClassAd` and `*classad.Expr` fields in your structs. This is useful for:
- Storing nested ClassAds without defining intermediate struct types
- Preserving unevaluated expressions for later evaluation
- Building dynamic configurations

#### Using *ClassAd Fields

`*ClassAd` fields allow you to embed arbitrary ClassAds within your struct:

```go
type Container struct {
    Name   string
    Config *classad.ClassAd
}

// Create a ClassAd for the Config field
config := classad.New()
config.InsertAttr("timeout", 30)
config.InsertAttrString("server", "example.com")
config.InsertAttr("retries", 3)

container := Container{
    Name:   "mycontainer",
    Config: config,
}

// Marshal to ClassAd format
str, _ := classad.Marshal(container)
// Result: [Name = "mycontainer"; Config = [timeout = 30; server = "example.com"; retries = 3]]

// Unmarshal back
var restored Container
classad.Unmarshal(str, &restored)

// Access the nested ClassAd
timeout, _ := restored.Config.EvaluateAttrInt("timeout")
server, _ := restored.Config.EvaluateAttrString("server")
```

#### Using *Expr Fields

`*Expr` fields store unevaluated expressions. This is important when:
- The expression references attributes not available at marshal time
- You want to preserve the formula for evaluation in different contexts
- Building templates that will be evaluated later

```go
type Job struct {
    Name     string
    CPUs     int
    Memory   *classad.Expr  // Unevaluated expression
    Formula  *classad.Expr  // Unevaluated expression
}

// Create expressions
memExpr, _ := classad.ParseExpr("RequestMemory * 1024")
formulaExpr, _ := classad.ParseExpr("CPUs * 2 + 8")

job := Job{
    Name:    "test-job",
    CPUs:    4,
    Memory:  memExpr,
    Formula: formulaExpr,
}

// Marshal - expressions are preserved
str, _ := classad.Marshal(job)
// Result: [Name = "test-job"; CPUs = 4; Memory = RequestMemory * 1024; Formula = CPUs * 2 + 8]

// Unmarshal - expressions remain unevaluated
var restored Job
classad.Unmarshal(str, &restored)

// Expression is stored, not evaluated
fmt.Println(restored.Memory.String())  // "(RequestMemory * 1024)"

// Evaluate later with a context
context := classad.New()
context.InsertAttr("RequestMemory", 2048)
context.InsertAttr("CPUs", 4)

memVal := restored.Memory.Eval(context)     // Evaluates to 2048 * 1024 = 2097152
formulaVal := restored.Formula.Eval(context) // Evaluates to 4 * 2 + 8 = 16
```

#### Nil ClassAd and Expr Fields

Nil `*ClassAd` and `*Expr` fields are marshaled as `undefined`:

```go
type Container struct {
    Name    string
    Config  *classad.ClassAd
    Formula *classad.Expr
}

container := Container{
    Name:    "test",
    Config:  nil,  // Will be undefined
    Formula: nil,  // Will be undefined
}

str, _ := classad.Marshal(container)
// Result: [Name = "test"; Config = undefined; Formula = undefined]
```

### Type Conversions

The package handles automatic type conversions during unmarshal:

```go
type Job struct {
    IntField   int     // Can read from integer or real
    FloatField float64 // Can read from integer or real
}

// Integer to float
classad.Unmarshal(`[IntField = 10; FloatField = 20]`, &job)
// FloatField becomes 20.0

// Real to integer (truncates)
classad.Unmarshal(`[IntField = 10.7; FloatField = 20.3]`, &job)
// IntField becomes 10
```

## Examples

### Example 1: HTCondor Job Description

```go
type HTCondorJob struct {
    ClusterID     int      `classad:"ClusterId"`
    ProcID        int      `classad:"ProcId"`
    Owner         string   `classad:"Owner"`
    RequestCPUs   int      `classad:"RequestCpus"`
    RequestMemory int      `classad:"RequestMemory"`
    Executable    string   `classad:"Executable"`
    Arguments     string   `classad:"Arguments,omitempty"`
    Requirements  []string `classad:"Requirements,omitempty"`
}

job := HTCondorJob{
    ClusterID:     100,
    ProcID:        0,
    Owner:         "alice",
    RequestCPUs:   4,
    RequestMemory: 8192,
    Executable:    "/usr/bin/python",
    Requirements:  []string{`OpSys == "LINUX"`, `Arch == "X86_64"`},
}

// Marshal to ClassAd format
classadStr, _ := classad.Marshal(job)

// Can also convert to JSON for REST APIs
ad, _ := classad.Parse(classadStr)
jsonBytes, _ := json.Marshal(ad)
```

### Example 2: Configuration File

```go
type ServerConfig struct {
    Host    string `classad:"host"`
    Port    int    `classad:"port"`
    Timeout int    `classad:"timeout"`
    TLS     bool   `classad:"tls"`
    Options struct {
        MaxConns int `classad:"max_connections"`
        KeepAlive bool `classad:"keep_alive"`
    } `classad:"options"`
}

// Read from ClassAd config file
configData, _ := os.ReadFile("config.classad")
var config ServerConfig
classad.Unmarshal(string(configData), &config)
```

### Example 3: Round-trip with Both Formats

```go
type Job struct {
    ID       int      `classad:"JobId"`
    Name     string   `classad:"Name"`
    Tags     []string `classad:"Tags"`
}

original := Job{ID: 123, Name: "test", Tags: []string{"a", "b"}}

// 1. Marshal to ClassAd
classadStr, _ := classad.Marshal(original)
// [JobId = 123; Name = "test"; Tags = {"a", "b"}]

// 2. Parse as ClassAd
ad, _ := classad.Parse(classadStr)

// 3. Convert to JSON
jsonBytes, _ := json.Marshal(ad)
// {"JobId":123,"Name":"test","Tags":["a","b"]}

// 4. Parse JSON back
var ad2 classad.ClassAd
json.Unmarshal(jsonBytes, &ad2)

// 5. Marshal back to struct
jsonBytes2, _ := json.Marshal(map[string]interface{}{
    "JobId": 123,
    "Name": "test",
    "Tags": []string{"a", "b"},
})
ad3 := &classad.ClassAd{}
json.Unmarshal(jsonBytes2, ad3)

// 6. Extract to struct (manual, or re-marshal to ClassAd first)
var restored Job
classadStr2 := ad3.String()
classad.Unmarshal(classadStr2, &restored)
```

### Example 4: Dynamic Job Template with Expressions

This example shows how to use `*Expr` fields to create job templates with formulas that evaluate based on runtime context:

```go
type JobTemplate struct {
    Name           string         `classad:"Name"`
    BaseCPUs       int            `classad:"BaseCPUs"`
    BaseMemory     int            `classad:"BaseMemory"`
    ComputedCPUs   *classad.Expr  `classad:"ComputedCPUs"`   // Formula
    ComputedMemory *classad.Expr  `classad:"ComputedMemory"` // Formula
    Priority       *classad.Expr  `classad:"Priority"`       // Formula
}

// Create a template with formulas
cpuExpr, _ := classad.ParseExpr("BaseCPUs * ScaleFactor")
memExpr, _ := classad.ParseExpr("BaseMemory * ScaleFactor")
priorityExpr, _ := classad.ParseExpr("BaseCPUs * 10 + BaseMemory / 1024")

template := JobTemplate{
    Name:           "batch-job",
    BaseCPUs:       2,
    BaseMemory:     4096,
    ComputedCPUs:   cpuExpr,
    ComputedMemory: memExpr,
    Priority:       priorityExpr,
}

// Save template to file
templateStr, _ := classad.Marshal(template)
os.WriteFile("job-template.classad", []byte(templateStr), 0644)

// Later, load and evaluate with different contexts
data, _ := os.ReadFile("job-template.classad")
var loaded JobTemplate
classad.Unmarshal(string(data), &loaded)

// Evaluate for small job
smallContext := classad.New()
smallContext.InsertAttr("ScaleFactor", 1)
smallCPUs := loaded.ComputedCPUs.Eval(smallContext)  // 2 * 1 = 2
smallMem := loaded.ComputedMemory.Eval(smallContext)  // 4096 * 1 = 4096

// Evaluate for large job
largeContext := classad.New()
largeContext.InsertAttr("ScaleFactor", 4)
largeCPUs := loaded.ComputedCPUs.Eval(largeContext)  // 2 * 4 = 8
largeMem := loaded.ComputedMemory.Eval(largeContext)  // 4096 * 4 = 16384
```

### Example 5: Nested Configuration with ClassAd Fields

This example shows using `*ClassAd` fields for flexible nested configurations:

```go
type ServiceConfig struct {
    Name     string         `classad:"Name"`
    Enabled  bool           `classad:"Enabled"`
    Database *classad.ClassAd `classad:"Database"`
    Cache    *classad.ClassAd `classad:"Cache"`
    Auth     *classad.ClassAd `classad:"Auth,omitempty"`
}

// Build database config
dbConfig := classad.New()
dbConfig.InsertAttrString("host", "db.example.com")
dbConfig.InsertAttr("port", 5432)
dbConfig.InsertAttrString("name", "myapp")
dbConfig.InsertAttr("pool_size", 10)

// Build cache config
cacheConfig := classad.New()
cacheConfig.InsertAttrString("type", "redis")
cacheConfig.InsertAttrString("host", "cache.example.com")
cacheConfig.InsertAttr("port", 6379)
cacheConfig.InsertAttr("ttl", 3600)

// Create service config
config := ServiceConfig{
    Name:     "myservice",
    Enabled:  true,
    Database: dbConfig,
    Cache:    cacheConfig,
    Auth:     nil, // Optional, will be undefined
}

// Marshal to file
configStr, _ := classad.Marshal(config)
os.WriteFile("service.config", []byte(configStr), 0644)
// Result:
// [Name = "myservice"; Enabled = true;
//  Database = [host = "db.example.com"; port = 5432; name = "myapp"; pool_size = 10];
//  Cache = [type = "redis"; host = "cache.example.com"; port = 6379; ttl = 3600];
//  Auth = undefined]

// Load and use
data, _ := os.ReadFile("service.config")
var loaded ServiceConfig
classad.Unmarshal(string(data), &loaded)

// Access nested values
dbHost, _ := loaded.Database.EvaluateAttrString("host")
dbPort, _ := loaded.Database.EvaluateAttrInt("port")
cacheType, _ := loaded.Cache.EvaluateAttrString("type")
cacheTTL, _ := loaded.Cache.EvaluateAttrInt("ttl")

// Check if optional field is present
if loaded.Auth != nil {
    // Auth config is available
}
```

### Example 6: REST API Integration

```go
// API handler that accepts both formats
func CreateJob(w http.ResponseWriter, r *http.Request) {
    var job Job

    contentType := r.Header.Get("Content-Type")

    if contentType == "application/json" {
        // Handle JSON
        var ad classad.ClassAd
        json.NewDecoder(r.Body).Decode(&ad)
        classad.Unmarshal(ad.String(), &job)
    } else {
        // Handle ClassAd format
        body, _ := io.ReadAll(r.Body)
        classad.Unmarshal(string(body), &job)
    }

    // Process job...

    // Return in requested format
    accept := r.Header.Get("Accept")
    if accept == "application/json" {
        // Return as JSON
        classadStr, _ := classad.Marshal(job)
        ad, _ := classad.Parse(classadStr)
        json.NewEncoder(w).Encode(ad)
    } else {
        // Return as ClassAd
        classadStr, _ := classad.Marshal(job)
        w.Write([]byte(classadStr))
    }
}
```

## Best Practices

### 1. Use Struct Tags Consistently

Choose one tagging convention and stick to it:

```go
// Good: Consistent use of classad tags
type Job struct {
    ID   int    `classad:"JobId"`
    Name string `classad:"Name"`
    CPUs int    `classad:"RequestCpus"`
}

// Acceptable: Consistent use of json tags
type Job struct {
    ID   int    `json:"job_id"`
    Name string `json:"name"`
    CPUs int    `json:"cpus"`
}

// Avoid: Mixed tags without reason
type Job struct {
    ID   int    `classad:"JobId"`
    Name string `json:"name"`  // Why different?
    CPUs int                   // And no tag here?
}
```

### 2. Use omitempty for Optional Fields

```go
type Job struct {
    ID       int    `classad:"JobId"`
    Name     string `classad:"Name"`
    // Optional fields
    Email    string   `classad:"Email,omitempty"`
    Tags     []string `classad:"Tags,omitempty"`
    Metadata map[string]string `classad:"Metadata,omitempty"`
}
```

### 3. Document ClassAd-Specific Field Names

When using custom field names for HTCondor compatibility:

```go
type Job struct {
    // ClusterId is the HTCondor cluster ID (ClusterId in ClassAd)
    ClusterID int `classad:"ClusterId"`

    // ProcId is the process ID within the cluster (ProcId in ClassAd)
    ProcID int `classad:"ProcId"`

    // Owner is the job owner (Owner in ClassAd)
    Owner string `classad:"Owner"`
}
```

### 4. Validate After Unmarshal

```go
var job Job
if err := classad.Unmarshal(input, &job); err != nil {
    return fmt.Errorf("unmarshal failed: %w", err)
}

// Validate required fields
if job.ID == 0 {
    return errors.New("job ID is required")
}
if job.Name == "" {
    return errors.New("job name is required")
}
```

### 5. Handle Unknown Fields Gracefully

The unmarshal process ignores unknown fields by default, which is usually desirable:

```go
// Struct with subset of fields
type JobSummary struct {
    ID   int    `classad:"JobId"`
    Name string `classad:"Name"`
    // Other fields in ClassAd are ignored
}

// Full ClassAd with many fields
fullClassAd := `[
    JobId = 123;
    Name = "test";
    Owner = "alice";
    RequestCpus = 4;
    ExtraField = "ignored";
]`

var summary JobSummary
classad.Unmarshal(fullClassAd, &summary)
// Only ID and Name are populated
```

### 6. Use Pointers for Optional Structs

```go
type Job struct {
    ID       int
    Metadata *Metadata `classad:"Metadata,omitempty"`
}

type Metadata struct {
    Created  string
    Modified string
}

// If Metadata is nil, it won't be marshaled
job := Job{ID: 123, Metadata: nil}
```

### 7. Combine Both Formats When Appropriate

```go
// Store in ClassAd format (native HTCondor)
classadStr, _ := classad.Marshal(job)
os.WriteFile("job.classad", []byte(classadStr), 0644)

// Expose via JSON API
ad, _ := classad.Parse(classadStr)
jsonBytes, _ := json.Marshal(ad)
w.Header().Set("Content-Type", "application/json")
w.Write(jsonBytes)
```

## Performance Considerations

### Marshaling Performance

- **ClassAd marshaling** uses reflection and is comparable to `json.Marshal`
- **JSON marshaling** of ClassAds converts to intermediate map structure
- For hot paths, consider caching marshaled results

### Memory Usage

- Both systems create intermediate representations during marshaling
- For large structures, consider streaming or chunking if possible
- Reuse struct instances when processing many items

### Tips for Better Performance

```go
// Pre-allocate slices with known capacity
type Job struct {
    Tags []string `classad:"Tags"`
}

job := Job{
    Tags: make([]string, 0, 10), // Pre-allocate
}

// Reuse structs in loops
var job Job
for _, input := range inputs {
    if err := classad.Unmarshal(input, &job); err != nil {
        continue
    }
    // Process job...
    // job will be reused in next iteration
}
```

## Error Handling

Common errors and how to handle them:

```go
// Unmarshal into wrong type
var job Job
err := classad.Unmarshal(`[ID = "not-a-number"]`, &job)
// Error: expected integer, got string

// Unmarshal into non-pointer
err := classad.Unmarshal(input, job) // Wrong!
// Error: unmarshal target must be a pointer

// Unmarshal nil pointer
var job *Job
err := classad.Unmarshal(input, job) // Wrong!
// Error: unmarshal target cannot be nil

// Correct usage
var job Job
err := classad.Unmarshal(input, &job) // Correct!
```

## See Also

- [Evaluation API Documentation](EVALUATION_API.md)
- [examples/struct_demo/](../examples/struct_demo/) - Working examples
- [examples/json_demo/](../examples/json_demo/) - JSON marshaling examples
- [HTCondor ClassAd Documentation](https://htcondor.readthedocs.io/en/latest/misc/classad-mechanism.html)
