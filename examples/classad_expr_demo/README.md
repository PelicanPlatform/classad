# ClassAd and Expr Fields Demo

This example demonstrates how to use `*classad.ClassAd` and `*classad.Expr` fields in Go structs when marshaling and unmarshaling ClassAds.

## Features Demonstrated

1. **`*classad.ClassAd` Fields**: Store nested ClassAds without defining intermediate struct types
2. **`*classad.Expr` Fields**: Preserve unevaluated expressions for later evaluation in different contexts
3. **Nil Handling**: How nil `*ClassAd` and `*Expr` fields are marshaled

## Running the Example

```bash
go run main.go
```

## Example 1: *classad.ClassAd Fields

Shows how to embed arbitrary ClassAds within a struct:

```go
type ServiceConfig struct {
    Name     string           `classad:"Name"`
    Database *classad.ClassAd `classad:"Database"`
    Cache    *classad.ClassAd `classad:"Cache"`
}
```

This is useful when:
- You don't want to define a separate struct for nested configuration
- The nested structure varies dynamically
- You need to preserve the exact ClassAd structure

## Example 2: *classad.Expr Fields

Shows how to preserve unevaluated expressions:

```go
type JobTemplate struct {
    BaseCPUs       int           `classad:"BaseCPUs"`
    ComputedCPUs   *classad.Expr `classad:"ComputedCPUs"`
}
```

Key benefits:
- Expressions are NOT evaluated during marshal/unmarshal
- Can evaluate later with different contexts
- Perfect for templates and formulas

```go
// Create expression
cpuExpr, _ := classad.ParseExpr("BaseCPUs * ScaleFactor")

// Marshal preserves the formula
template := JobTemplate{BaseCPUs: 2, ComputedCPUs: cpuExpr}
classadStr, _ := classad.Marshal(template)
// Result: [BaseCPUs = 2; ComputedCPUs = BaseCPUs * ScaleFactor]

// Evaluate with context
context := classad.New()
context.InsertAttr("ScaleFactor", 4)
result := restored.ComputedCPUs.Eval(context)  // 2 * 4 = 8
```

## Example 3: Nil Fields

Shows how nil `*ClassAd` and `*Expr` fields are handled:

```go
type OptionalConfig struct {
    Name     string           `classad:"Name"`
    Database *classad.ClassAd `classad:"Database,omitempty"`
    Formula  *classad.Expr    `classad:"Formula,omitempty"`
}

// Nil fields are marshaled as undefined (or omitted with omitempty)
config := OptionalConfig{Name: "test", Database: nil, Formula: nil}
```

## Use Cases

### Dynamic Configuration
```go
type AppConfig struct {
    Name     string
    Settings *classad.ClassAd  // Can vary per deployment
}
```

### Job Templates
```go
type JobTemplate struct {
    BaseCPUs   int
    BaseMemory int
    Formula    *classad.Expr  // Evaluated per job
}
```

### Multi-Environment Configs
```go
type DeployConfig struct {
    App         string
    Development *classad.ClassAd
    Production  *classad.ClassAd
}
```

## See Also

- [MARSHALING.md](../../docs/MARSHALING.md) - Complete marshaling guide
- [MARSHALING_QUICKREF.md](../../docs/MARSHALING_QUICKREF.md) - Quick reference
- [struct_demo](../struct_demo/) - Basic struct marshaling examples
