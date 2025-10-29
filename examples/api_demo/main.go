package main

import (
	"fmt"
	"log"

	"github.com/bbockelm/golang-classads/classad"
)

func main() {
	fmt.Println("=== ClassAd Public API Examples ===")
	fmt.Println()

	// Example 1: Creating a ClassAd programmatically
	fmt.Println("Example 1: Creating ClassAds programmatically")
	ad := classad.New()
	ad.InsertAttr("Cpus", 4)
	ad.InsertAttrFloat("Memory", 8192.0)
	ad.InsertAttrString("Name", "worker-01")
	ad.InsertAttrBool("IsAvailable", true)

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
	if expr := jobAd.Lookup("JobId"); expr != nil {
		fmt.Printf("Found JobId expression: %v\n", expr)
	}
	fmt.Println()

	// Example 4: Evaluating attributes with type safety
	fmt.Println("Example 4: Evaluating attributes with type safety")

	if jobId, ok := jobAd.EvaluateAttrInt("JobId"); ok {
		fmt.Printf("JobId = %d\n", jobId)
	}

	if owner, ok := jobAd.EvaluateAttrString("Owner"); ok {
		fmt.Printf("Owner = %s\n", owner)
	}

	if cpus, ok := jobAd.EvaluateAttrNumber("Cpus"); ok {
		fmt.Printf("Cpus = %g\n", cpus)
	}

	if memory, ok := jobAd.EvaluateAttrInt("Memory"); ok {
		fmt.Printf("Memory = %d MB\n", memory)
	}
	fmt.Println()

	// Example 5: Evaluating complex expressions
	fmt.Println("Example 5: Evaluating complex expressions")
	if requirements, ok := jobAd.EvaluateAttrBool("Requirements"); ok {
		fmt.Printf("Requirements evaluate to: %v\n", requirements)
	}
	fmt.Println()

	// Example 6: Using EvaluateExpr for direct Value evaluation
	fmt.Println("Example 6: Using EvaluateExpr with attribute values")
	val := jobAd.EvaluateAttr("JobId")
	fmt.Printf("JobId value: %s (type: %v)\n", val.String(), val.Type())

	requirementsVal := jobAd.EvaluateAttr("Requirements")
	fmt.Printf("Requirements value: %s\n", requirementsVal.String())
	fmt.Println()

	// Example 7: Working with arithmetic expressions
	fmt.Println("Example 7: Working with arithmetic expressions")
	calcAd, _ := classad.Parse(`[
		a = 10;
		b = 20;
		sum = a + b;
		difference = a - b;
		product = a * b;
		quotient = b / a;
		remainder = b % a
	]`)

	if sum, ok := calcAd.EvaluateAttrInt("sum"); ok {
		fmt.Printf("sum = %d\n", sum)
	}
	if diff, ok := calcAd.EvaluateAttrInt("difference"); ok {
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

	// Example 8: Working with logical expressions
	fmt.Println("Example 8: Working with logical expressions")
	logicAd, _ := classad.Parse(`[
		hasEnoughCpus = Cpus >= 2;
		hasEnoughMemory = Memory >= 2048;
		meetsRequirements = hasEnoughCpus && hasEnoughMemory;
		Cpus = 4;
		Memory = 4096
	]`)

	if meets, ok := logicAd.EvaluateAttrBool("meetsRequirements"); ok {
		fmt.Printf("meetsRequirements = %v\n", meets)
	}
	fmt.Println()

	// Example 9: Conditional expressions
	fmt.Println("Example 9: Conditional expressions")
	condAd, _ := classad.Parse(`[
		x = 10;
		y = 5;
		max = x > y ? x : y;
		min = x < y ? x : y;
		status = x > y ? "x is greater" : "y is greater"
	]`)

	if max, ok := condAd.EvaluateAttrInt("max"); ok {
		fmt.Printf("max = %d\n", max)
	}
	if min, ok := condAd.EvaluateAttrInt("min"); ok {
		fmt.Printf("min = %d\n", min)
	}
	if status, ok := condAd.EvaluateAttrString("status"); ok {
		fmt.Printf("status = %s\n", status)
	}
	fmt.Println()

	// Example 10: Modifying ClassAds
	fmt.Println("Example 10: Modifying ClassAds")
	fmt.Printf("Attributes before: %v\n", ad.GetAttributes())

	// Update an existing attribute
	ad.InsertAttr("Cpus", 8)
	if cpus, ok := ad.EvaluateAttrInt("Cpus"); ok {
		fmt.Printf("Updated Cpus = %d\n", cpus)
	}

	// Delete an attribute
	if ad.Delete("IsAvailable") {
		fmt.Println("Deleted IsAvailable attribute")
	}

	fmt.Printf("Attributes after: %v\n", ad.GetAttributes())
	fmt.Printf("Size: %d attributes\n", ad.Size())
	fmt.Println()

	// Example 11: Real-world HTCondor scenario
	fmt.Println("Example 11: Real-world HTCondor scenario")
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
	if name, ok := machineAd.EvaluateAttrString("Name"); ok {
		fmt.Printf("  Name: %s\n", name)
	}
	if cpus, ok := machineAd.EvaluateAttrInt("Cpus"); ok {
		fmt.Printf("  Cpus: %d\n", cpus)
	}
	if memory, ok := machineAd.EvaluateAttrInt("Memory"); ok {
		fmt.Printf("  Memory: %d MB\n", memory)
	}
	if disk, ok := machineAd.EvaluateAttrInt("Disk"); ok {
		fmt.Printf("  Disk: %d KB\n", disk)
	}
	if state, ok := machineAd.EvaluateAttrString("State"); ok {
		fmt.Printf("  State: %s\n", state)
	}
	fmt.Println()

	// Example 12: Handling undefined values
	fmt.Println("Example 12: Handling undefined values")
	testAd := classad.New()
	testAd.InsertAttr("x", 10)

	// Try to evaluate a non-existent attribute
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
