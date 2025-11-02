# Struct Marshaling Demo

This example demonstrates how to marshal and unmarshal Go structs to/from ClassAd format using struct tags.

## Features Demonstrated

1. **Simple Struct Marshaling** - Convert basic Go structs to ClassAd format
2. **Struct Tags** - Use `classad` and `json` tags to control field names
3. **Tag Options** - `omitempty` and `-` for skipping fields
4. **Nested Structs** - Handle complex hierarchical data structures
5. **Unmarshal** - Parse ClassAd strings into Go structs
6. **Round-trip** - Verify marshal→unmarshal preserves data
7. **Map Support** - Work with `map[string]interface{}`

## API Overview

### Marshal
```go
classadStr, err := classad.Marshal(v)
```

Converts a Go value to ClassAd format string. Works with:
- Structs
- Maps
- Slices/arrays
- Basic types (int, float, string, bool)

### Unmarshal
```go
err := classad.Unmarshal(classadStr, &v)
```

Parses a ClassAd string into a Go value. The target must be a pointer.

## Struct Tags

### classad Tag
Primary tag for controlling ClassAd marshaling:

```go
type Job struct {
    ID   int    `classad:"JobId"`        // Use custom name
    Name string `classad:"Name,omitempty"` // Omit if zero value
    Temp string `classad:"-"`            // Skip this field
}
```

### json Tag Fallback
If no `classad` tag is present, falls back to `json` tag:

```go
type Config struct {
    Timeout int `json:"timeout"`  // Uses "timeout" in ClassAd
    Port    int                   // Uses "Port" (field name)
}
```

### Tag Options

- **Field name**: `classad:"custom_name"`
- **Skip field**: `classad:"-"`
- **Omit if empty**: `classad:"name,omitempty"`

## Examples

### Basic Struct
```go
type Job struct {
    ID   int
    Name string
}

job := Job{ID: 123, Name: "test"}
str, _ := classad.Marshal(job)
// [ID = 123; Name = "test"]
```

### With Tags
```go
type Job struct {
    JobID int    `classad:"ClusterId"`
    Owner string `classad:"Owner"`
}

job := Job{JobID: 100, Owner: "alice"}
str, _ := classad.Marshal(job)
// [ClusterId = 100; Owner = "alice"]
```

### Nested Structs
```go
type Config struct {
    Timeout int
}

type Job struct {
    ID     int
    Config Config
}

job := Job{ID: 123, Config: Config{Timeout: 30}}
str, _ := classad.Marshal(job)
// [ID = 123; Config = [Timeout = 30]]
```

### Unmarshal
```go
classadStr := `[JobId = 456; Owner = "bob"]`

type Job struct {
    JobID int    `classad:"JobId"`
    Owner string `classad:"Owner"`
}

var job Job
classad.Unmarshal(classadStr, &job)
// job.JobID = 456, job.Owner = "bob"
```

## Running the Demo

```bash
go run main.go
```

## Use Cases

This struct marshaling is useful for:
- **Configuration files** - Store configs in ClassAd format
- **HTCondor integration** - Work with HTCondor job descriptions
- **Data serialization** - Alternative to JSON for certain workflows
- **Type-safe APIs** - Use Go structs with ClassAd backend
- **Testing** - Easy conversion between Go types and ClassAds

## Comparison with JSON

| Feature | classad.Marshal | json.Marshal |
|---------|----------------|--------------|
| Struct tags | `classad` or `json` | `json` |
| Output format | ClassAd | JSON |
| Expressions | Supported | Not applicable |
| Nested data | ✓ | ✓ |
| omitempty | ✓ | ✓ |
| Skip fields | ✓ | ✓ |

## Notes

- Unexported fields are automatically skipped
- Zero values can be omitted with `omitempty`
- Unknown fields in ClassAd are ignored during unmarshal
- Type conversions are performed when possible (int ↔ float)
