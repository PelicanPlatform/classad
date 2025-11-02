// Example demonstrating Go 1.23+ range-over-function iterators
//
// This example uses Go 1.23+ range-over-function syntax.
package main

import (
	"fmt"
	"log"
	"os"
	"strings"

	"github.com/PelicanPlatform/classad/classad"
)

func main() {
	fmt.Println("=== Range-over-Function Iterator Demo ===")
	fmt.Println()

	// Example 1: Simple iteration
	example1()

	// Example 2: Iteration with index
	example2()

	// Example 3: Iteration with error handling
	example3()

	// Example 4: Old-style ClassAds
	example4()

	// Example 5: Reading from file
	example5()
}

func example1() {
	fmt.Println("Example 1: Simple iteration with generic API")

	input := `[JobId = 1; Owner = "alice"; Cpus = 2]
[JobId = 2; Owner = "bob"; Cpus = 4]
[JobId = 3; Owner = "charlie"; Cpus = 8]`

	for ad := range classad.All(strings.NewReader(input)) {
		jobId := classad.GetOr(ad, "JobId", 0)
		owner := classad.GetOr(ad, "Owner", "unknown")
		cpus := classad.GetOr(ad, "Cpus", 0)
		fmt.Printf("  Job %d: owner=%s, cpus=%d\n", jobId, owner, cpus)
	}

	fmt.Println()
}

func example2() {
	fmt.Println("Example 2: Iteration with index and generic API")

	input := `[Name = "Machine1"; Available = true]
[Name = "Machine2"; Available = false]
[Name = "Machine3"; Available = true]`

	for i, ad := range classad.AllWithIndex(strings.NewReader(input)) {
		name := classad.GetOr(ad, "Name", "unknown")
		available := classad.GetOr(ad, "Available", false)
		fmt.Printf("  [%d] %s: available=%v\n", i, name, available)
	}

	fmt.Println()
}

func example3() {
	fmt.Println("Example 3: Iteration with error handling and generic API")

	// Valid input
	input := `[Status = "Running"]
[Status = "Idle"]`

	var err error

	for ad := range classad.AllWithError(strings.NewReader(input), &err) {
		status := classad.GetOr(ad, "Status", "unknown")
		fmt.Printf("  Status: %s\n", status)
	}

	if err != nil {
		log.Printf("  Error: %v\n", err)
	}

	// Invalid input
	fmt.Println("\n  Testing error handling with invalid input:")
	invalidInput := `[Status = ]` // Invalid syntax

	var err2 error
	count := 0

	for range classad.AllWithError(strings.NewReader(invalidInput), &err2) {
		count++
	}

	if err2 != nil {
		fmt.Printf("  Caught error as expected: %v\n", err2)
	}
	fmt.Printf("  Processed %d ClassAds before error\n", count)

	fmt.Println()
}

func example4() {
	fmt.Println("Example 4: Old-style ClassAds with generic API")

	input := `MyType = "Machine"
Name = "slot1@server1"
Cpus = 4

MyType = "Machine"
Name = "slot2@server2"
Cpus = 8`

	for ad := range classad.AllOld(strings.NewReader(input)) {
		name := classad.GetOr(ad, "Name", "unknown")
		cpus := classad.GetOr(ad, "Cpus", 0)
		fmt.Printf("  %s: %d CPUs\n", name, cpus)
	}

	fmt.Println()
}

func example5() {
	fmt.Println("Example 5: Reading from file with generic API")

	// Try to open the example file
	file, err := os.Open("../jobs-multiple.ad")
	if err != nil {
		fmt.Printf("  Skipping file example: %v\n", err)
		fmt.Println()
		return
	}
	defer file.Close()

	count := 0
	for ad := range classad.All(file) {
		jobId := classad.GetOr(ad, "JobId", 0)
		owner := classad.GetOr(ad, "Owner", "unknown")
		fmt.Printf("  Job %d: %s\n", jobId, owner)
		count++
	}

	fmt.Printf("  Total: %d jobs\n", count)
	fmt.Println()
}
