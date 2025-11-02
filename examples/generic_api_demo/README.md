# Generic ClassAd API Demo

This example demonstrates the new idiomatic generic API for working with ClassAds: `Set()`, `GetAs[T]()`, and `GetOr[T]()`.

## Overview

The generic API provides a more Go-idiomatic way to work with ClassAds, reducing verbosity and leveraging Go's type inference and generics.

## Features

### 1. **Generic `Set()` Method**

Set any type of value without type-specific methods:

```go
ad := classad.New()
ad.Set("cpus", 4)              // int
ad.Set("price", 0.05)          // float64
ad.Set("name", "worker-1")     // string
ad.Set("enabled", true)        // bool
ad.Set("tags", []string{"a"})  // slice
ad.Set("config", nestedAd)     // *ClassAd
ad.Set("formula", expr)        // *Expr
```

**Old API:**
```go
ad.InsertAttr("cpus", 4)
ad.InsertAttrFloat("price", 0.05)
ad.InsertAttrString("name", "worker-1")
ad.InsertAttrBool("enabled", true)
```

### 2. **Type-Safe `GetAs[T]()` Method**

Retrieve values with compile-time type safety:

```go
cpus, ok := classad.GetAs[int](ad, "cpus")
name, ok := classad.GetAs[string](ad, "name")
tags, ok := classad.GetAs[[]string](ad, "tags")
config, ok := classad.GetAs[*classad.ClassAd](ad, "config")
```

**Old API:**
```go
cpus, ok := ad.EvaluateAttrInt("cpus")
name, ok := ad.EvaluateAttrString("name")
// Slices required more complex handling
```

### 3. **Convenient `GetOr[T]()` with Defaults**

Eliminate boilerplate if-checks by providing defaults:

```go
cpus := classad.GetOr(ad, "cpus", 1)           // Defaults to 1
timeout := classad.GetOr(ad, "timeout", 300)   // Defaults to 300
owner := classad.GetOr(ad, "owner", "unknown") // Defaults to "unknown"
```

**Old API:**
```go
var cpus int64
if val, ok := ad.EvaluateAttrInt("cpus"); ok {
    cpus = val
} else {
    cpus = 1
}
```

## Running the Example

```bash
go run main.go
```

## Examples in This Demo

1. **Basic Set and Get**: Simple type handling
2. **GetOr with Defaults**: Eliminating nil checks
3. **Working with Slices**: Arrays of values
4. **Nested ClassAds**: Complex hierarchical data
5. **Expressions**: Unevaluated formulas
6. **Type Conversion**: Automatic int/float conversions
7. **API Comparison**: Old vs new approaches
8. **Real-World Config**: Practical configuration example

## Benefits

### Less Verbose
- `Set("x", 5)` vs `InsertAttr("x", 5)`
- No need to remember method names for each type

### Type Inference
- Compiler infers types automatically
- Fewer type annotations needed

### Defaults Made Easy
- `GetOr(ad, "timeout", 300)` handles missing values
- Eliminates if-checks and nil handling

### Type Safety
- Compile-time type checking with generics
- `GetAs[int]()` ensures you get an `int`

### Consistent API
- Same pattern for all types
- Easier to learn and remember

## Backward Compatibility

The old API remains available:
- `InsertAttr()`, `InsertAttrString()`, etc. still work
- `EvaluateAttrInt()`, `EvaluateAttrString()`, etc. still work
- Existing code continues to function

The generic API is additive and provides an alternative, more idiomatic approach.

## Type Support

Supported types:
- **Integers**: `int`, `int8`, `int16`, `int32`, `int64`, `uint`, `uint8`, etc.
- **Floats**: `float32`, `float64`
- **Strings**: `string`
- **Booleans**: `bool`
- **Slices**: `[]T` for any supported type `T`
- **ClassAds**: `*classad.ClassAd`
- **Expressions**: `*classad.Expr`
- **Structs**: Any struct (marshaled to nested ClassAd)

## Automatic Type Conversion

The API handles common conversions:
```go
ad.Set("value", 42)

// All of these work:
asInt, _ := classad.GetAs[int](ad, "value")       // 42
asInt64, _ := classad.GetAs[int64](ad, "value")   // 42
asFloat, _ := classad.GetAs[float64](ad, "value") // 42.0
```

## Use Cases

### Configuration Files
```go
serverName := classad.GetOr(config, "server_name", "default")
port := classad.GetOr(config, "port", 8080)
timeout := classad.GetOr(config, "timeout", 30)
```

### Job Specifications
```go
ad.Set("RequestCpus", 4)
ad.Set("RequestMemory", 8192)
ad.Set("Owner", "alice")

cpus := classad.GetOr(ad, "RequestCpus", 1)
memory := classad.GetOr(ad, "RequestMemory", 1024)
```

### Dynamic Templates
```go
formula, _ := classad.ParseExpr("RequestCpus * 2")
ad.Set("ComputedCpus", formula)

expr, _ := classad.GetAs[*classad.Expr](ad, "ComputedCpus")
result := expr.Eval(ad)
```

## See Also

- [MARSHALING.md](../../docs/MARSHALING.md) - Struct marshaling guide
- [struct_demo](../struct_demo/) - Struct marshaling examples
- [api_demo](../api_demo/) - Original API examples
