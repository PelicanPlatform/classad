package main

import (
	"fmt"
	"log"

	"github.com/PelicanPlatform/classad/classad"
)

// Demonstrates the new generic Set(), GetAs[T](), and GetOr[T]() API

func basicSetAndGet() {
	fmt.Println("\n=== Basic Set() and GetAs[T]() ===")

	ad := classad.New()

	// Set values using the generic Set() API
	ad.Set("cpus", 4)
	ad.Set("memory", 8192)
	ad.Set("name", "worker-node-1")
	ad.Set("enabled", true)
	ad.Set("price", 0.05)

	// Get values using the type-safe generic GetAs[T]() API
	cpus, ok := classad.GetAs[int](ad, "cpus")
	fmt.Printf("CPUs: %d (ok=%v)\n", cpus, ok)

	memory, ok := classad.GetAs[int](ad, "memory")
	fmt.Printf("Memory: %d MB (ok=%v)\n", memory, ok)

	name, ok := classad.GetAs[string](ad, "name")
	fmt.Printf("Name: %q (ok=%v)\n", name, ok)

	enabled, ok := classad.GetAs[bool](ad, "enabled")
	fmt.Printf("Enabled: %v (ok=%v)\n", enabled, ok)

	price, ok := classad.GetAs[float64](ad, "price")
	fmt.Printf("Price: $%.3f/hour (ok=%v)\n", price, ok)
}

func setAndGetWithDefaults() {
	fmt.Println("\n=== GetOr[T]() with Defaults ===")

	ad := classad.New()
	ad.Set("cpus", 4)
	ad.Set("memory", 8192)
	// Note: timeout and owner are NOT set

	// Get existing values
	cpus := classad.GetOr(ad, "cpus", 1)
	fmt.Printf("CPUs: %d (exists)\n", cpus)

	memory := classad.GetOr(ad, "memory", 1024)
	fmt.Printf("Memory: %d MB (exists)\n", memory)

	// Get missing values with defaults
	timeout := classad.GetOr(ad, "timeout", 300)
	fmt.Printf("Timeout: %d seconds (default)\n", timeout)

	owner := classad.GetOr(ad, "owner", "unknown")
	fmt.Printf("Owner: %q (default)\n", owner)

	priority := classad.GetOr(ad, "priority", 5)
	fmt.Printf("Priority: %d (default)\n", priority)
}

func workWithSlices() {
	fmt.Println("\n=== Working with Slices ===")

	ad := classad.New()

	// Set slices using generic Set()
	ad.Set("tags", []string{"production", "critical", "monitored"})
	ad.Set("ports", []int{80, 443, 8080})

	// Get slices using GetAs[T]()
	tags, ok := classad.GetAs[[]string](ad, "tags")
	fmt.Printf("Tags: %v (ok=%v)\n", tags, ok)

	ports, ok := classad.GetAs[[]int](ad, "ports")
	fmt.Printf("Ports: %v (ok=%v)\n", ports, ok)

	// Get missing slice with default
	labels := classad.GetOr(ad, "labels", []string{"default-label"})
	fmt.Printf("Labels: %v (default)\n", labels)
}

func workWithNestedClassAds() {
	fmt.Println("\n=== Working with Nested ClassAds ===")

	ad := classad.New()

	// Create nested configuration
	dbConfig := classad.New()
	dbConfig.Set("host", "db.example.com")
	dbConfig.Set("port", 5432)
	dbConfig.Set("name", "myapp")
	dbConfig.Set("pool_size", 10)

	cacheConfig := classad.New()
	cacheConfig.Set("type", "redis")
	cacheConfig.Set("host", "cache.example.com")
	cacheConfig.Set("port", 6379)

	// Set nested ClassAds
	ad.Set("database", dbConfig)
	ad.Set("cache", cacheConfig)

	// Get nested ClassAds
	db, ok := classad.GetAs[*classad.ClassAd](ad, "database")
	if ok {
		host := classad.GetOr(db, "host", "localhost")
		port := classad.GetOr(db, "port", 5432)
		fmt.Printf("Database: %s:%d\n", host, port)
	}

	cache, ok := classad.GetAs[*classad.ClassAd](ad, "cache")
	if ok {
		cacheType := classad.GetOr(cache, "type", "memory")
		fmt.Printf("Cache type: %s\n", cacheType)
	}
}

func workWithExpressions() {
	fmt.Println("\n=== Working with Expressions ===")

	ad := classad.New()
	ad.Set("base_cpus", 2)
	ad.Set("base_memory", 4096)
	ad.Set("scale_factor", 4)

	// Set expressions that reference other attributes
	cpuExpr, _ := classad.ParseExpr("base_cpus * scale_factor")
	memExpr, _ := classad.ParseExpr("base_memory * scale_factor")

	ad.Set("computed_cpus", cpuExpr)
	ad.Set("computed_memory", memExpr)

	// Get the unevaluated expressions
	computedCpusExpr, ok := classad.GetAs[*classad.Expr](ad, "computed_cpus")
	if ok {
		fmt.Printf("CPU formula: %s\n", computedCpusExpr.String())

		// Evaluate in context
		result := computedCpusExpr.Eval(ad)
		if value, err := result.IntValue(); err == nil {
			fmt.Printf("Computed CPUs: %d\n", value)
		}
	}

	computedMemExpr, ok := classad.GetAs[*classad.Expr](ad, "computed_memory")
	if ok {
		fmt.Printf("Memory formula: %s\n", computedMemExpr.String())

		// Evaluate in context
		result := computedMemExpr.Eval(ad)
		if value, err := result.IntValue(); err == nil {
			fmt.Printf("Computed Memory: %d MB\n", value)
		}
	}
}

func typeConversion() {
	fmt.Println("\n=== Automatic Type Conversion ===")

	ad := classad.New()
	ad.Set("value", 42)

	// Get as different types
	asInt, ok := classad.GetAs[int](ad, "value")
	fmt.Printf("As int: %d (ok=%v)\n", asInt, ok)

	asInt64, ok := classad.GetAs[int64](ad, "value")
	fmt.Printf("As int64: %d (ok=%v)\n", asInt64, ok)

	asFloat, ok := classad.GetAs[float64](ad, "value")
	fmt.Printf("As float64: %f (ok=%v)\n", asFloat, ok)

	// Set a float and get as int (truncates)
	ad.Set("real_value", 3.7)
	truncated, ok := classad.GetAs[int](ad, "real_value")
	fmt.Printf("Float 3.7 as int: %d (truncated, ok=%v)\n", truncated, ok)
}

func comparisonWithOldAPI() {
	fmt.Println("\n=== API Comparison ===")

	ad := classad.New()

	fmt.Println("\n--- Old API ---")
	// Old way (type-specific methods)
	ad.InsertAttr("cpus", 4)
	ad.InsertAttrFloat("price", 0.05)
	ad.InsertAttrString("name", "worker-1")
	ad.InsertAttrBool("enabled", true)

	cpus, ok := ad.EvaluateAttrInt("cpus")
	fmt.Printf("CPUs: %d (ok=%v)\n", cpus, ok)

	price, ok := ad.EvaluateAttrReal("price")
	fmt.Printf("Price: %f (ok=%v)\n", price, ok)

	fmt.Println("\n--- New Generic API ---")
	// New way (generic methods)
	ad2 := classad.New()
	ad2.Set("cpus", 4)
	ad2.Set("price", 0.05)
	ad2.Set("name", "worker-1")
	ad2.Set("enabled", true)

	cpus2 := classad.GetOr(ad2, "cpus", 1)
	price2 := classad.GetOr(ad2, "price", 0.0)
	name2 := classad.GetOr(ad2, "name", "unknown")

	fmt.Printf("CPUs: %d\n", cpus2)
	fmt.Printf("Price: %f\n", price2)
	fmt.Printf("Name: %s\n", name2)

	fmt.Println("\nBenefits:")
	fmt.Println("  - Less verbose: Set() vs InsertAttr*()")
	fmt.Println("  - Type inference: compiler infers types")
	fmt.Println("  - Defaults: GetOr() eliminates if-checks")
	fmt.Println("  - Type-safe: Generic types catch errors at compile time")
}

func realWorldExample() {
	fmt.Println("\n=== Real-World Configuration Example ===")

	// Load configuration from ClassAd
	configStr := `[
		server_name = "api-server-1";
		port = 8080;
		max_connections = 100;
		timeout = 30;
		tls_enabled = true;
		allowed_origins = {"https://example.com", "https://app.example.com"};
		database = [
			host = "db.example.com";
			port = 5432;
			pool_size = 20
		]
	]`

	ad, err := classad.Parse(configStr)
	if err != nil {
		log.Fatal(err)
	}

	// Extract configuration with defaults using generic API
	serverName := classad.GetOr(ad, "server_name", "default-server")
	port := classad.GetOr(ad, "port", 8080)
	maxConn := classad.GetOr(ad, "max_connections", 50)
	timeout := classad.GetOr(ad, "timeout", 60)
	tlsEnabled := classad.GetOr(ad, "tls_enabled", false)
	origins := classad.GetOr(ad, "allowed_origins", []string{})

	fmt.Printf("Server: %s:%d\n", serverName, port)
	fmt.Printf("Max connections: %d\n", maxConn)
	fmt.Printf("Timeout: %d seconds\n", timeout)
	fmt.Printf("TLS enabled: %v\n", tlsEnabled)
	fmt.Printf("Allowed origins: %v\n", origins)

	// Get nested database config
	if db, ok := classad.GetAs[*classad.ClassAd](ad, "database"); ok {
		dbHost := classad.GetOr(db, "host", "localhost")
		dbPort := classad.GetOr(db, "port", 5432)
		poolSize := classad.GetOr(db, "pool_size", 10)
		fmt.Printf("Database: %s:%d (pool size: %d)\n", dbHost, dbPort, poolSize)
	}

	// The old API would require many more lines:
	// if val, ok := ad.EvaluateAttrString("server_name"); ok { ... } else { serverName = "default" }
	// if val, ok := ad.EvaluateAttrInt("port"); ok { ... } else { port = 8080 }
	// ... and so on
}

func main() {
	fmt.Println("Generic ClassAd API Examples")
	fmt.Println("============================")

	basicSetAndGet()
	setAndGetWithDefaults()
	workWithSlices()
	workWithNestedClassAds()
	workWithExpressions()
	typeConversion()
	comparisonWithOldAPI()
	realWorldExample()
}
