# ClassAd Examples

This directory contains example programs and ClassAd files demonstrating various features of the golang-classads library.

## Example Programs

### generic_api_demo
**Location:** `examples/generic_api_demo/main.go`

**⭐ Recommended starting point** - Demonstrates the modern generic API for working with ClassAds:
- **Set()**: Setting attributes with any type
- **GetAs[T]()**: Type-safe attribute retrieval with generics
- **GetOr[T]()**: Getting attributes with default values
- **Type conversions**: Automatic safe conversions (int ↔ float64)
- **Slices and complex types**: Working with lists and nested data
- **Comparison**: Side-by-side with traditional API

This example showcases the **recommended idiomatic API** for new Go code.

Run with:
```bash
go run examples/generic_api_demo/main.go
```

### api_demo
**Location:** `examples/api_demo/main.go`

Comprehensive demonstration of the ClassAd API including:
- Creating ClassAds programmatically with **Set()**
- Parsing ClassAds from strings
- Type-safe retrieval with **GetAs[T]()** and **GetOr[T]()**
- Evaluating attributes (both new and traditional APIs)
- Working with different value types
- Arithmetic and logical expressions
- Modifying ClassAds
- Real-world HTCondor scenarios

Run with:
```bash
go run examples/api_demo/main.go
```

### reader_demo
**Location:** `examples/reader_demo/main.go`

Demonstrates reading multiple ClassAds from various sources using the Reader API with generic API:
- **Reading new-style ClassAds**: Parsing bracketed ClassAds from strings/files
- **Reading old-style ClassAds**: Parsing newline-delimited ClassAds separated by blank lines
- **For-loop iteration**: Using the Reader in idiomatic Go for-loops
- **Filtering**: Processing only ClassAds that match certain criteria (using **GetAs[T]**)
- **File I/O**: Reading ClassAds from files
- **Nested structures**: Working with nested ClassAds in iteration

Run with:
```bash
go run examples/reader_demo/main.go
```

### range_demo
**Location:** `examples/range_demo/main.go`

Demonstrates Go 1.23+ range-over-function iterator pattern with generic API:
- **Simple iteration**: Using `for ad := range classad.All(reader)`
- **Indexed iteration**: Using `for i, ad := range classad.AllWithIndex(reader)`
- **Error handling**: Using `AllWithError` to capture errors during iteration
- **Old-style support**: Using `AllOld` for newline-delimited ClassAds
- **File I/O**: Reading ClassAds from files with range syntax
- **Generic API**: Using **GetOr[T]()** for safe attribute access

This example showcases the modern, ergonomic way to iterate over ClassAds using Go 1.23+ features.

Run with:
```bash
go run examples/range_demo/main.go
```

### simple_reader
**Location:** `examples/simple_reader/main.go`

A command-line tool for reading and displaying ClassAds from files:
- Accepts a filename as argument
- Supports both new-style and old-style formats (use `--old` flag)
- Displays all attributes of each ClassAd
- Useful for testing and inspecting ClassAd files

Run with:
```bash
go run examples/simple_reader/main.go examples/jobs-multiple.ad
go run examples/simple_reader/main.go examples/machines-old.ad --old
```

### expr_demo
**Location:** `examples/expr_demo/main.go`

Demonstrates the Expression API for working with unevaluated expressions:
- **Parsing expressions**: Creating Expr objects from strings
- **Expression evaluation**: Evaluating in ClassAd contexts
- **Lookup and insertion**: Getting and setting unevaluated expressions
- **Scoped evaluation**: Using MY and TARGET contexts
- **Formula copying**: Reusing expressions across ClassAds
- **Template patterns**: Dynamic expression evaluation

Run with:
```bash
go run examples/expr_demo/main.go
```

### introspection_demo
**Location:** `examples/introspection_demo/main.go`

Comprehensive demonstration of expression introspection and utility features:
- **Quote/Unquote**: String escaping and parsing helpers
- **MarshalOld**: Converting ClassAds to old HTCondor format
- **ExternalRefs**: Finding undefined attribute dependencies
- **InternalRefs**: Finding defined attribute dependencies
- **Flatten**: Partial evaluation and query optimization
- **Validation workflows**: Checking if expressions can be evaluated
- **Dependency tracking**: Cache invalidation and change detection
- **Query optimization**: Pre-computing constant parts of expressions

Run with:
```bash
go run examples/introspection_demo/main.go
```

## Example ClassAd Files

### job.ad
Example HTCondor job ClassAd showing:
- Job requirements and resource requests
- Owner and submission information
- Executable and arguments
- Input/output file specifications

### machine.ad
Example HTCondor machine/slot ClassAd showing:
- Hardware resources (CPUs, memory, disk)
- Machine state and capabilities
- Architecture and operating system
- Start/Requirements expressions

**Format:** New ClassAd format (with brackets and semicolons)

### machine_old.ad
Same machine ClassAd in old HTCondor format showing:
- Newline-delimited attributes
- No surrounding brackets
- Compatible with HTCondor pre-7.5.1

**Format:** Old ClassAd format (newline-delimited, no brackets)

### expressions.txt
Collection of sample ClassAd expressions demonstrating:
- Arithmetic operators
- Logical operators
- Comparison operators
- String literals
- List and record literals
- Attribute references
- Built-in functions

## Usage Examples

### Modern API (Recommended)

#### Creating and accessing attributes with generics
```go
// Create a ClassAd
ad := classad.New()

// Set attributes with any type
ad.Set("Cpus", 4)
ad.Set("Memory", 8192.0)
ad.Set("Owner", "alice")
ad.Set("Tags", []string{"prod", "gpu"})

// Type-safe retrieval with GetAs[T]()
if cpus, ok := classad.GetAs[int](ad, "Cpus"); ok {
    fmt.Printf("Cpus: %d\n", cpus)
}

// Get with default values using GetOr[T]()
owner := classad.GetOr(ad, "Owner", "unknown")
priority := classad.GetOr(ad, "Priority", 10)  // Uses default if missing
```

### Parsing a ClassAd from a file (new format)
```go
data, err := os.ReadFile("examples/job.ad")
if err != nil {
    log.Fatal(err)
}

ad, err := classad.Parse(string(data))
if err != nil {
    log.Fatal(err)
}

// Access attributes with generic API
jobId := classad.GetOr(ad, "JobId", 0)
owner := classad.GetOr(ad, "Owner", "unknown")
```

### Parsing a ClassAd from a file (old format)
```go
data, err := os.ReadFile("examples/machine_old.ad")
if err != nil {
    log.Fatal(err)
}

ad, err := classad.ParseOld(string(data))
if err != nil {
    log.Fatal(err)
}
```

### Traditional API (Still Supported)

#### Evaluating attributes
```go
// Evaluate to specific type
owner, ok := ad.EvaluateAttrString("Owner")
cpus, ok := ad.EvaluateAttrInt("RequestCpus")
memory, ok := ad.EvaluateAttrInt("RequestMemory")

// Evaluate to generic Value
val := ad.EvaluateAttr("Requirements")
if val.IsBoolean() {
    matches, _ := val.BoolValue()
    fmt.Printf("Job matches: %v\n", matches)
}
```

### Working with nested structures
```go
ad, _ := classad.Parse(`[
    cluster = [
        name = "production";
        nodes = {
            [hostname = "node1"; cpus = 8],
            [hostname = "node2"; cpus = 16]
        }
    ];
    totalNodes = size(cluster.nodes)
]`)

// Access nested values
clusterVal := ad.EvaluateAttr("cluster")
if clusterVal.IsClassAd() {
    cluster, _ := clusterVal.ClassAdValue()
    name, _ := cluster.EvaluateAttrString("name")
    fmt.Printf("Cluster: %s\n", name)
}
```

### Using built-in functions
```go
ad, _ := classad.Parse(`[
    users = {"alice", "bob", "charlie"};
    checkAlice = member("alice", users);
    userCount = size(users);
    greeting = strcat("Hello, ", "World!");
    upperGreeting = toUpper(greeting)
]`)

hasAlice, _ := ad.EvaluateAttrBool("checkAlice")
count, _ := ad.EvaluateAttrInt("userCount")
upper, _ := ad.EvaluateAttrString("upperGreeting")
```

### Using IS/ISNT operators
```go
ad, _ := classad.Parse(`[
    x = 5;
    y = 5.0;

    valueEqual = (x == y);    // true (coerces types)
    strictEqual = (x is y);   // false (different types)
    notEqual = (x isnt y);    // true (different types)

    // Meta-equal operator aliases
    metaEqual = (x =?= y);    // false (same as 'is')
    metaNotEqual = (x =!= y)  // true (same as 'isnt')
]`)

valueEq, _ := ad.EvaluateAttrBool("valueEqual")
strictEq, _ := ad.EvaluateAttrBool("strictEqual")
notEq, _ := ad.EvaluateAttrBool("notEqual")
metaEq, _ := ad.EvaluateAttrBool("metaEqual")
```

### Using attribute selection
```go
ad, _ := classad.Parse(`[
    employee = [
        name = "Alice";
        department = [name = "Engineering"; location = "Building A"]
    ];
    empName = employee.name;
    deptName = employee.department.name
]`)

name, _ := ad.EvaluateAttrString("empName")      // "Alice"
dept, _ := ad.EvaluateAttrString("deptName")     // "Engineering"
```

### Using subscript expressions
```go
ad, _ := classad.Parse(`[
    fruits = {"apple", "banana", "cherry"};
    person = [name = "Bob"; age = 30];
    matrix = {{1, 2, 3}, {4, 5, 6}};

    first = fruits[0];
    personName = person["name"];
    element = matrix[1][2]
]`)

first, _ := ad.EvaluateAttrString("first")       // "apple"
name, _ := ad.EvaluateAttrString("personName")   // "Bob"
elem, _ := ad.EvaluateAttrInt("element")         // 6
```

### Combining selection and subscripting
```go
ad, _ := classad.Parse(`[
    company = [
        employees = {
            [name = "Alice"; salary = 100000],
            [name = "Bob"; salary = 95000]
        }
    ];
    firstEmpName = company.employees[0].name;
    secondSalary = company.employees[1].salary
]`)

name, _ := ad.EvaluateAttrString("firstEmpName")    // "Alice"
salary, _ := ad.EvaluateAttrInt("secondSalary")     // 95000
```

## Learn More

For detailed API documentation, see:
- [EVALUATION_API.md](../docs/EVALUATION_API.md) - Complete evaluation API reference
- [README.md](../README.md) - Project overview and getting started

For the official ClassAd language specification, visit:
- [HTCondor ClassAd Reference Manual](https://htcondor.org/classad/refman/)
