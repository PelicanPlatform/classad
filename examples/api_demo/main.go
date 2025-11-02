package main

import (
	"fmt"
	"log"

	"github.com/PelicanPlatform/classad/classad"
)

func main() {
	fmt.Println("=== ClassAd Public API Examples ===")
	fmt.Println()

	// Example 1: Creating a ClassAd programmatically with generic Set() API
	fmt.Println("Example 1: Creating ClassAds programmatically (using Set)")
	ad := classad.New()
	ad.Set("Cpus", 4)
	ad.Set("Memory", 8192.0)
	ad.Set("Name", "worker-01")
	ad.Set("IsAvailable", true)

	fmt.Printf("Created ClassAd: %s\n", ad)
	fmt.Printf("Size: %d attributes\n", ad.Size())
	fmt.Println()

	// Example 2: Parsing ClassAds from strings
	fmt.Println("Example 2: Parsing ClassAds from strings")
	jobAd, err := classad.Parse(`[
		JobId = 1001;
		Owner = "alice";
		Cpus = 2;
		Memory = 4096;
		Requirements = (Cpus >= 2) && (Memory >= 2048);
		Status = "Running"
	]`)
	if err != nil {
		log.Fatal(err)
	}
	fmt.Printf("Parsed job ClassAd: %s\n", jobAd)
	fmt.Println()

	// Example 3: Looking up attributes
	fmt.Println("Example 3: Looking up attributes")
	if expr, ok := jobAd.Lookup("JobId"); ok {
		fmt.Printf("Found JobId expression: %v\n", expr)
	}
	fmt.Println()

	// Example 4: Using generic GetAs[T]() for type-safe retrieval
	fmt.Println("Example 4: Type-safe retrieval with GetAs[T]()")

	if jobId, ok := classad.GetAs[int](jobAd, "JobId"); ok {
		fmt.Printf("JobId = %d\n", jobId)
	}

	if owner, ok := classad.GetAs[string](jobAd, "Owner"); ok {
		fmt.Printf("Owner = %s\n", owner)
	}

	if cpus, ok := classad.GetAs[int](jobAd, "Cpus"); ok {
		fmt.Printf("Cpus = %d\n", cpus)
	}

	if memory, ok := classad.GetAs[int](jobAd, "Memory"); ok {
		fmt.Printf("Memory = %d MB\n", memory)
	}
	fmt.Println()

	// Example 5: Using GetOr[T]() with defaults
	fmt.Println("Example 5: Using GetOr[T]() with default values")

	status := classad.GetOr(jobAd, "Status", "Unknown")
	fmt.Printf("Status = %s\n", status)

	priority := classad.GetOr(jobAd, "Priority", 10) // Missing, uses default
	fmt.Printf("Priority = %d (default)\n", priority)

	cpusWithDefault := classad.GetOr(jobAd, "Cpus", 1)
	fmt.Printf("Cpus = %d\n", cpusWithDefault)
	fmt.Println()

	// Example 6: Evaluating complex expressions (traditional API)
	fmt.Println("Example 6: Evaluating complex expressions")
	if requirements, ok := jobAd.EvaluateAttrBool("Requirements"); ok {
		fmt.Printf("Requirements evaluate to: %v\n", requirements)
	}
	fmt.Println()

	// Example 7: Using EvaluateExpr for direct Value evaluation
	fmt.Println("Example 7: Using EvaluateExpr with attribute values")
	val := jobAd.EvaluateAttr("JobId")
	fmt.Printf("JobId value: %s (type: %v)\n", val.String(), val.Type())

	requirementsVal := jobAd.EvaluateAttr("Requirements")
	fmt.Printf("Requirements value: %s\n", requirementsVal.String())
	fmt.Println()

	// Example 8: Working with arithmetic expressions
	fmt.Println("Example 8: Working with arithmetic expressions")
	calcAd, _ := classad.Parse(`[
		a = 10;
		b = 20;
		sum = a + b;
		difference = a - b;
		product = a * b;
		quotient = b / a;
		remainder = b % a
	]`)

	if sum, ok := classad.GetAs[int](calcAd, "sum"); ok {
		fmt.Printf("sum = %d\n", sum)
	}
	if diff, ok := classad.GetAs[int](calcAd, "difference"); ok {
		fmt.Printf("difference = %d\n", diff)
	}
	if prod, ok := calcAd.EvaluateAttrInt("product"); ok {
		fmt.Printf("product = %d\n", prod)
	}
	if quot, ok := calcAd.EvaluateAttrInt("quotient"); ok {
		fmt.Printf("quotient = %d\n", quot)
	}
	if rem, ok := calcAd.EvaluateAttrInt("remainder"); ok {
		fmt.Printf("remainder = %d\n", rem)
	}
	fmt.Println()

	// Example 9: Working with logical expressions
	fmt.Println("Example 9: Working with logical expressions")
	logicAd, _ := classad.Parse(`[
		hasEnoughCpus = Cpus >= 2;
		hasEnoughMemory = Memory >= 2048;
		meetsRequirements = hasEnoughCpus && hasEnoughMemory;
		Cpus = 4;
		Memory = 4096
	]`)

	if meets, ok := classad.GetAs[bool](logicAd, "meetsRequirements"); ok {
		fmt.Printf("meetsRequirements = %v\n", meets)
	}
	fmt.Println()

	// Example 10: Conditional expressions
	fmt.Println("Example 10: Conditional expressions")
	condAd, _ := classad.Parse(`[
		x = 10;
		y = 5;
		max = x > y ? x : y;
		min = x < y ? x : y;
		status = x > y ? "x is greater" : "y is greater"
	]`)

	if max, ok := classad.GetAs[int](condAd, "max"); ok {
		fmt.Printf("max = %d\n", max)
	}
	if min, ok := classad.GetAs[int](condAd, "min"); ok {
		fmt.Printf("min = %d\n", min)
	}
	if status, ok := classad.GetAs[string](condAd, "status"); ok {
		fmt.Printf("status = %s\n", status)
	}
	fmt.Println()

	// Example 11: Modifying ClassAds with Set()
	fmt.Println("Example 11: Modifying ClassAds")
	fmt.Printf("Attributes before: %v\n", ad.GetAttributes())

	// Update an existing attribute with Set()
	ad.Set("Cpus", 8)
	if cpus, ok := classad.GetAs[int](ad, "Cpus"); ok {
		fmt.Printf("Updated Cpus = %d\n", cpus)
	}

	// Delete an attribute
	if ad.Delete("IsAvailable") {
		fmt.Println("Deleted IsAvailable attribute")
	}

	fmt.Printf("Attributes after: %v\n", ad.GetAttributes())
	fmt.Printf("Size: %d attributes\n", ad.Size())
	fmt.Println()

	// Example 12: Real-world HTCondor scenario with generic API
	fmt.Println("Example 12: Real-world HTCondor scenario")
	machineAd, _ := classad.Parse(`[
		Name = "slot1@worker.example.com";
		Machine = "worker.example.com";
		Cpus = 8;
		Memory = 16384;
		Disk = 1000000;
		State = "Unclaimed";
		Activity = "Idle";
		Requirements = (Target.Cpus <= Cpus) && (Target.Memory <= Memory);
		Rank = Target.Cpus + Target.Memory / 1024.0
	]`)

	fmt.Println("Machine ClassAd:")
	name := classad.GetOr(machineAd, "Name", "unknown")
	fmt.Printf("  Name: %s\n", name)

	cpusM := classad.GetOr(machineAd, "Cpus", 0)
	fmt.Printf("  Cpus: %d\n", cpusM)

	memoryM := classad.GetOr(machineAd, "Memory", 0)
	fmt.Printf("  Memory: %d MB\n", memoryM)

	diskM := classad.GetOr(machineAd, "Disk", 0)
	fmt.Printf("  Disk: %d KB\n", diskM)

	state := classad.GetOr(machineAd, "State", "Unknown")
	fmt.Printf("  State: %s\n", state)
	fmt.Println()

	// Example 13: Handling undefined values
	fmt.Println("Example 13: Handling undefined values")
	testAd := classad.New()
	testAd.Set("x", 10)

	// Try to evaluate a non-existent attribute with GetAs
	if value, ok := classad.GetAs[int](testAd, "nonexistent"); ok {
		fmt.Printf("Value: %d\n", value)
	} else {
		fmt.Println("Attribute 'nonexistent' is undefined or wrong type")
	}

	// GetOr provides a safe default
	defaultValue := classad.GetOr(testAd, "nonexistent", 999)
	fmt.Printf("Using GetOr with default: %d\n", defaultValue)

	// Traditional API for checking undefined
	value := testAd.EvaluateAttr("nonexistent")
	if value.IsUndefined() {
		fmt.Println("Attribute 'nonexistent' is undefined")
	}

	// Try to get an integer from a non-existent attribute
	if _, ok := testAd.EvaluateAttrInt("nonexistent"); !ok {
		fmt.Println("Failed to evaluate 'nonexistent' as integer")
	}
	fmt.Println()

	fmt.Println("=== Examples Complete ===")
}
