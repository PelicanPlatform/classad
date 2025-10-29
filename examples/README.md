# ClassAd Examples

This directory contains example programs and ClassAd files demonstrating various features of the golang-classads library.

## Example Programs

### features_demo
**Location:** `examples/features_demo/main.go`

A comprehensive demonstration of advanced ClassAd features including:
- **Nested ClassAds**: Working with hierarchical data structures
- **IS and ISNT operators**: Strict identity checking vs. value equality
- **String functions**: `strcat`, `substr`, `size`, `toUpper`, `toLower`
- **Math functions**: `floor`, `ceiling`, `round`, `int`, `real`, `random`
- **Type checking**: `isString`, `isInteger`, `isReal`, `isBoolean`, `isList`, `isClassAd`, `isUndefined`
- **List operations**: `member`, `size`
- **Real-world scenario**: HTCondor job matching simulation

Run with:
```bash
go run examples/features_demo/main.go
```

### api_demo
**Location:** `examples/api_demo/main.go`

Basic demonstration of the ClassAd API including:
- Parsing ClassAds from strings
- Evaluating attributes
- Working with different value types
- Attribute references and expressions

Run with:
```bash
go run examples/api_demo/main.go
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

### Parsing a ClassAd from a file
```go
data, err := os.ReadFile("examples/job.ad")
if err != nil {
    log.Fatal(err)
}

ad, err := classad.Parse(string(data))
if err != nil {
    log.Fatal(err)
}
```

### Evaluating attributes
```go
// Evaluate to specific type
owner, err := ad.EvaluateAttrString("Owner")
cpus, err := ad.EvaluateAttrInt("RequestCpus")
memory, err := ad.EvaluateAttrInt("RequestMemory")

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
    notEqual = (x isnt y)     // true (different types)
]`)

valueEq, _ := ad.EvaluateAttrBool("valueEqual")
strictEq, _ := ad.EvaluateAttrBool("strictEqual")
notEq, _ := ad.EvaluateAttrBool("notEqual")
```

## Learn More

For detailed API documentation, see:
- [EVALUATION_API.md](../docs/EVALUATION_API.md) - Complete evaluation API reference
- [README.md](../README.md) - Project overview and getting started

For the official ClassAd language specification, visit:
- [HTCondor ClassAd Reference Manual](https://htcondor.org/classad/refman/)
