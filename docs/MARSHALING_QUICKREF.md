# Marshaling Quick Reference

Quick reference for marshaling Go structs to/from ClassAd and JSON formats.

## Struct Tags

```go
type Job struct {
    ID       int      `classad:"JobId"`           // Custom ClassAd name
    Owner    string   `json:"owner"`              // Falls back to json tag
    CPUs     int                                   // Uses field name "CPUs"
    Optional string   `classad:"Optional,omitempty"` // Omit if zero value
    Tags     []string `json:"tags,omitempty"`     // json tag with omitempty
    Internal string   `classad:"-"`               // Skip this field
}
```

## ClassAd Format

### Marshal
```go
import "github.com/PelicanPlatform/classad/classad"

job := Job{ID: 123, Owner: "alice", CPUs: 4}
classadStr, err := classad.Marshal(job)
// [JobId = 123; owner = "alice"; CPUs = 4]
```

### Unmarshal
```go
var job Job
err := classad.Unmarshal(`[JobId = 123; owner = "alice"; CPUs = 4]`, &job)
```

## JSON Format

### Marshal ClassAd to JSON
```go
import "encoding/json"

ad, _ := classad.Parse(`[x = 5; y = x + 3]`)
jsonBytes, err := json.Marshal(ad)
// {"x":5,"y":"\/Expr(x + 3)\/"}
```

### Unmarshal JSON to ClassAd
```go
var ad classad.ClassAd
err := json.Unmarshal(jsonBytes, &ad)
```

## Type Mapping

| Go Type | ClassAd | JSON |
|---------|---------|------|
| `int`, `int64`, etc. | `123` | `123` |
| `float64` | `3.14` | `3.14` |
| `string` | `"text"` | `"text"` |
| `bool` | `true`/`false` | `true`/`false` |
| `[]T` | `{a, b, c}` | `["a","b","c"]` |
| `struct` | `[...]` | `{...}` |
| `map[string]T` | `[...]` | `{...}` |
| `*classad.ClassAd` | `[...]` | `{...}` |
| `*classad.Expr` | unevaluated expr | `"\/Expr(...)\/")` |
| Complex expr | N/A | `"\/Expr(...)\/")` |

## Supported Operations

| Operation | ClassAd Format | JSON Format |
|-----------|---------------|-------------|
| Struct → String | `classad.Marshal(v)` | `json.Marshal(ad)` |
| String → Struct | `classad.Unmarshal(s, &v)` | `json.Unmarshal(b, &ad)` |
| Parse to ClassAd | `classad.Parse(s)` | `json.Unmarshal(b, &ad)` |
| ClassAd → String | `ad.String()` | `json.Marshal(ad)` |

## Expression Handling

### In ClassAd Format
Expressions are evaluated:
```go
classadStr := `[x = 5; y = x + 3]`
ad, _ := classad.Parse(classadStr)
y := ad.EvaluateAttr("y")  // Returns 8
```

### In JSON Format
Expressions are serialized with `/Expr(...)/)` format:
```go
ad, _ := classad.Parse(`[x = 5; y = x + 3]`)
jsonBytes, _ := json.Marshal(ad)
// {"x":5,"y":"\/Expr(x + 3)\/"}

var ad2 classad.ClassAd
json.Unmarshal(jsonBytes, &ad2)
y := ad2.EvaluateAttr("y")  // Returns 8 (evaluated in context)
```

## Common Patterns

### Round-trip: Struct → ClassAd → JSON
```go
// 1. Struct to ClassAd
job := Job{ID: 123, Owner: "alice"}
classadStr, _ := classad.Marshal(job)

// 2. Parse ClassAd
ad, _ := classad.Parse(classadStr)

// 3. ClassAd to JSON
jsonBytes, _ := json.Marshal(ad)

// 4. JSON back to ClassAd
var ad2 classad.ClassAd
json.Unmarshal(jsonBytes, &ad2)

// 5. ClassAd to Struct
classadStr2 := ad2.String()
var job2 Job
classad.Unmarshal(classadStr2, &job2)
```

### Work with map[string]interface{}
```go
// Marshal map to ClassAd
data := map[string]interface{}{
    "id": 123,
    "name": "test",
}
classadStr, _ := classad.Marshal(data)

// Unmarshal ClassAd to map
var restored map[string]interface{}
classad.Unmarshal(classadStr, &restored)
```

### Nested Structs
```go
type Config struct {
    Timeout int
    Server  string
}

type Job struct {
    ID     int
    Config Config
}

job := Job{ID: 123, Config: Config{Timeout: 30, Server: "host"}}
classadStr, _ := classad.Marshal(job)
// [ID = 123; Config = [Timeout = 30; Server = "host"]]
```

### ClassAd Fields
```go
type Container struct {
    Name   string
    Config *classad.ClassAd  // Flexible nested ClassAd
}

config := classad.New()
config.InsertAttr("timeout", 30)
config.InsertAttrString("server", "example.com")

container := Container{Name: "test", Config: config}
classadStr, _ := classad.Marshal(container)
// [Name = "test"; Config = [timeout = 30; server = "example.com"]]

// Unmarshal back
var restored Container
classad.Unmarshal(classadStr, &restored)
timeout, _ := restored.Config.EvaluateAttrInt("timeout")
```

### Expr Fields (Unevaluated Expressions)
```go
type Job struct {
    Name    string
    CPUs    int
    Formula *classad.Expr  // Preserved as expression
}

formula, _ := classad.ParseExpr("CPUs * 2 + 8")
job := Job{Name: "test", CPUs: 4, Formula: formula}

// Marshal - formula is NOT evaluated
classadStr, _ := classad.Marshal(job)
// [Name = "test"; CPUs = 4; Formula = CPUs * 2 + 8]

// Unmarshal - formula preserved
var restored Job
classad.Unmarshal(classadStr, &restored)

// Evaluate later with context
context := classad.New()
context.InsertAttr("CPUs", 4)
result := restored.Formula.Eval(context)  // Evaluates to 16
```

## Tag Options Summary

| Tag | Effect | Example |
|-----|--------|---------|
| `classad:"name"` | Custom name | `JobId int \`classad:"ClusterId"\`` |
| `json:"name"` | Fallback name | `CPUs int \`json:"cpus"\`` |
| `,omitempty` | Skip if zero | `Tags []string \`classad:"Tags,omitempty"\`` |
| `"-"` | Always skip | `Secret string \`classad:"-"\`` |
| (no tag) | Use field name | `CPUs int` → `CPUs` |

## Zero Values Behavior

Without `omitempty`:
```go
type Job struct {
    ID   int      // 0 will be marshaled
    Name string   // "" will be marshaled
    Tags []string // nil/empty will be marshaled as {}
}
```

With `omitempty`:
```go
type Job struct {
    ID   int      `classad:",omitempty"` // 0 will be omitted
    Name string   `classad:",omitempty"` // "" will be omitted
    Tags []string `classad:",omitempty"` // nil/empty will be omitted
}
```

## Error Handling

```go
// Always check errors
classadStr, err := classad.Marshal(job)
if err != nil {
    log.Fatal(err)
}

// Unmarshal requires pointer
var job Job
err = classad.Unmarshal(classadStr, &job) // Note: &job, not job
if err != nil {
    log.Fatal(err)
}

// Validate after unmarshal
if job.ID == 0 {
    return errors.New("ID is required")
}
```

## See Also

- [MARSHALING.md](MARSHALING.md) - Complete marshaling guide
- [EVALUATION_API.md](EVALUATION_API.md) - Evaluation API reference
- [examples/struct_demo/](../examples/struct_demo/) - Struct marshaling examples
- [examples/json_demo/](../examples/json_demo/) - JSON marshaling examples
