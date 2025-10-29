package main

import (
	"fmt"
	"log"

	"github.com/PelicanPlatform/classad/classad"
)

func main() {
	// Example 1: Quote/Unquote for string handling
	fmt.Println("=== Example 1: Quote/Unquote ===")
	original := `Hello "World"	with\backslash`
	quoted := classad.Quote(original)
	fmt.Printf("Original: %s\n", original)
	fmt.Printf("Quoted:   %s\n", quoted)

	unquoted, err := classad.Unquote(quoted)
	if err != nil {
		log.Fatal(err)
	}
	fmt.Printf("Unquoted: %s\n", unquoted)
	fmt.Printf("Round-trip matches: %v\n\n", original == unquoted)

	// Example 2: MarshalOld for backward compatibility
	fmt.Println("=== Example 2: MarshalOld ===")
	ad, err := classad.Parse(`[
		JobId = "cluster123.proc45";
		Cpus = 4;
		Memory = 8192;
		RequestDisk = 1024 * 1024;
		Owner = "alice";
	]`)
	if err != nil {
		log.Fatal(err)
	}

	fmt.Println("New format:")
	fmt.Println(ad.String())
	fmt.Println("\nOld format:")
	fmt.Println(ad.MarshalOld())

	// Example 3: ExternalRefs - Finding undefined dependencies
	fmt.Println("=== Example 3: ExternalRefs - Dependency Analysis ===")
	expr, err := classad.ParseExpr("RequestMemory * 1024 * 1024 < Memory")
	if err != nil {
		log.Fatal(err)
	}

	jobAd, err := classad.Parse(`[
		RequestMemory = 2048;
		RequestCpus = 4;
	]`)
	if err != nil {
		log.Fatal(err)
	}

	externalRefs := jobAd.ExternalRefs(expr)
	fmt.Printf("Expression: %s\n", expr.String())
	fmt.Printf("ClassAd has: RequestMemory, RequestCpus\n")
	fmt.Printf("External references (missing): %v\n\n", externalRefs)

	// Example 4: InternalRefs - Finding defined dependencies
	fmt.Println("=== Example 4: InternalRefs - Available Attributes ===")
	internalRefs := jobAd.InternalRefs(expr)
	fmt.Printf("Expression: %s\n", expr.String())
	fmt.Printf("Internal references (available): %v\n\n", internalRefs)

	// Example 5: Flatten - Partial evaluation
	fmt.Println("=== Example 5: Flatten - Optimization ===")
	complexExpr, err := classad.ParseExpr("RequestCpus * 1000 + RequestMemory / 1024 + Unknown")
	if err != nil {
		log.Fatal(err)
	}

	flattened := jobAd.Flatten(complexExpr)
	fmt.Printf("Original:  %s\n", complexExpr.String())
	fmt.Printf("Flattened: %s\n", flattened.String())
	fmt.Println("(Known values replaced with literals, unknown values preserved)")
	fmt.Println()

	// Example 6: Validation workflow
	fmt.Println("=== Example 6: Validation Workflow ===")
	requirementExpr, err := classad.ParseExpr("Cpus >= RequestCpus && Memory >= RequestMemory && Arch == \"x86_64\"")
	if err != nil {
		log.Fatal(err)
	}

	// Find what's needed
	needed := jobAd.ExternalRefs(requirementExpr)
	fmt.Printf("Requirement: %s\n", requirementExpr.String())
	fmt.Printf("Job ad provides: RequestMemory, RequestCpus\n")
	fmt.Printf("Machine must provide: %v\n\n", needed)

	// Example 7: Optimization with Flatten
	fmt.Println("=== Example 7: Query Optimization ===")
	machineAd, err := classad.Parse(`[
		Cpus = 8;
		Memory = 16384;
		Arch = "x86_64";
	]`)
	if err != nil {
		log.Fatal(err)
	}

	// Partially evaluate the requirement
	partialReq := jobAd.Flatten(requirementExpr)
	fmt.Printf("Original requirement:\n  %s\n", requirementExpr.String())
	fmt.Printf("Partially evaluated:\n  %s\n", partialReq.String())

	// Now evaluate against machine
	result := machineAd.EvaluateExprWithTarget(partialReq, nil)
	if boolVal, err := result.BoolValue(); err == nil {
		fmt.Printf("Matches machine: %v\n", boolVal)
	} else {
		fmt.Printf("Evaluation result: %v\n", result)
	}
	fmt.Println()

	// Example 8: Dependency tracking for caching
	fmt.Println("=== Example 8: Dependency Tracking ===")
	cacheKey, err := classad.ParseExpr("strcat(Owner, \":\", Arch, \":\", OpSys)")
	if err != nil {
		log.Fatal(err)
	}

	sampleAd, err := classad.Parse(`[
		Owner = "bob";
		Arch = "x86_64";
		RequestCpus = 2;
	]`)
	if err != nil {
		log.Fatal(err)
	}

	dependencies := sampleAd.InternalRefs(cacheKey)
	missing := sampleAd.ExternalRefs(cacheKey)

	fmt.Printf("Cache key expression: %s\n", cacheKey.String())
	fmt.Printf("Depends on (available): %v\n", dependencies)
	fmt.Printf("Depends on (missing): %v\n", missing)

	if len(missing) > 0 {
		fmt.Printf("Cannot compute cache key - missing: %v\n", missing)
	} else {
		result := sampleAd.EvaluateExprWithTarget(cacheKey, nil)
		if strVal, err := result.StringValue(); err == nil {
			fmt.Printf("Cache key value: %s\n", strVal)
		} else {
			fmt.Printf("Cache key result: %v\n", result)
		}
	}
}
