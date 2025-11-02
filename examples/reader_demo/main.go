package main

import (
	"fmt"
	"log"
	"os"
	"strings"

	"github.com/PelicanPlatform/classad/classad"
)

func main() {
	fmt.Println("=== ClassAd Reader Demo ===")
	fmt.Println()

	// Example 1: Reading new-style ClassAds with generic API
	fmt.Println("Example 1: Reading new-style ClassAds from string")
	newStyleAds := `
[JobId = 1; Owner = "alice"; Cpus = 2; Memory = 2048]
[JobId = 2; Owner = "bob"; Cpus = 4; Memory = 4096]
[JobId = 3; Owner = "charlie"; Cpus = 1; Memory = 1024]
`
	reader := classad.NewReader(strings.NewReader(newStyleAds))

	fmt.Println("Jobs:")
	for reader.Next() {
		ad := reader.ClassAd()
		jobId := classad.GetOr(ad, "JobId", 0)
		owner := classad.GetOr(ad, "Owner", "unknown")
		cpus := classad.GetOr(ad, "Cpus", 0)
		memory := classad.GetOr(ad, "Memory", 0)

		fmt.Printf("  Job %d: Owner=%s, Cpus=%d, Memory=%dMB\n",
			jobId, owner, cpus, memory)
	}

	if err := reader.Err(); err != nil {
		log.Fatalf("Error reading ClassAds: %v", err)
	}
	fmt.Println()

	// Example 2: Reading old-style ClassAds with generic API
	fmt.Println("Example 2: Reading old-style ClassAds from string")
	oldStyleAds := `MyType = "Machine"
Name = "worker01.example.com"
Cpus = 8
Memory = 16384
Arch = "X86_64"

MyType = "Machine"
Name = "worker02.example.com"
Cpus = 16
Memory = 32768
Arch = "X86_64"

MyType = "Machine"
Name = "worker03.example.com"
Cpus = 4
Memory = 8192
Arch = "ARM64"
`
	oldReader := classad.NewOldReader(strings.NewReader(oldStyleAds))

	fmt.Println("Machines:")
	for oldReader.Next() {
		ad := oldReader.ClassAd()
		name := classad.GetOr(ad, "Name", "unknown")
		cpus := classad.GetOr(ad, "Cpus", 0)
		memory := classad.GetOr(ad, "Memory", 0)
		arch := classad.GetOr(ad, "Arch", "unknown")

		fmt.Printf("  %s: %d CPUs, %dMB RAM, %s\n",
			name, cpus, memory, arch)
	}

	if err := oldReader.Err(); err != nil {
		log.Fatalf("Error reading ClassAds: %v", err)
	}
	fmt.Println()

	// Example 3: Processing with filtering using generic API
	fmt.Println("Example 3: Filtering ClassAds (jobs requiring >= 4 CPUs)")
	filterAds := `
[JobId = 100; Cpus = 2; Priority = 10]
[JobId = 101; Cpus = 8; Priority = 5]
[JobId = 102; Cpus = 4; Priority = 8]
[JobId = 103; Cpus = 1; Priority = 3]
[JobId = 104; Cpus = 16; Priority = 9]
`
	filterReader := classad.NewReader(strings.NewReader(filterAds))

	fmt.Println("High-CPU jobs:")
	count := 0
	for filterReader.Next() {
		ad := filterReader.ClassAd()
		cpus, ok := classad.GetAs[int](ad, "Cpus")
		if !ok {
			continue
		}

		if cpus >= 4 {
			jobId := classad.GetOr(ad, "JobId", 0)
			priority := classad.GetOr(ad, "Priority", 0)
			fmt.Printf("  Job %d: %d CPUs (priority=%d)\n", jobId, cpus, priority)
			count++
		}
	}

	if err := filterReader.Err(); err != nil {
		log.Fatalf("Error reading ClassAds: %v", err)
	}
	fmt.Printf("Found %d high-CPU jobs\n", count)
	fmt.Println()

	// Example 4: Reading from file (if file exists)
	fmt.Println("Example 4: Reading from file")
	filename := "../../examples/job.ad"
	file, err := os.Open(filename)
	if err != nil {
		fmt.Printf("  (Skipping - file not found: %v)\n", err)
	} else {
		defer file.Close()

		fileReader := classad.NewReader(file)
		if fileReader.Next() {
			ad := fileReader.ClassAd()
			fmt.Printf("  Successfully read ClassAd from %s\n", filename)

			// Try to get some attributes with generic API
			if owner, ok := classad.GetAs[string](ad, "Owner"); ok {
				fmt.Printf("  Owner: %s\n", owner)
			}
			if jobId, ok := classad.GetAs[int](ad, "JobId"); ok {
				fmt.Printf("  JobId: %d\n", jobId)
			}
		}

		if err := fileReader.Err(); err != nil {
			fmt.Printf("  Error: %v\n", err)
		}
	}
	fmt.Println()

	// Example 5: Nested ClassAds
	fmt.Println("Example 5: Reading nested ClassAds")
	nestedAds := `
[
	ClusterId = 1;
	Job = [
		Id = 100;
		Owner = "alice";
		Resources = [Cpus = 4; Memory = 8192]
	]
]
[
	ClusterId = 2;
	Job = [
		Id = 200;
		Owner = "bob";
		Resources = [Cpus = 8; Memory = 16384]
	]
]
`
	nestedReader := classad.NewReader(strings.NewReader(nestedAds))

	fmt.Println("Clusters:")
	for nestedReader.Next() {
		ad := nestedReader.ClassAd()
		clusterId, _ := ad.EvaluateAttrInt("ClusterId")

		jobVal := ad.EvaluateAttr("Job")
		if jobVal.IsClassAd() {
			jobAd, _ := jobVal.ClassAdValue()
			jobId, _ := jobAd.EvaluateAttrInt("Id")
			owner, _ := jobAd.EvaluateAttrString("Owner")

			resourcesVal := jobAd.EvaluateAttr("Resources")
			if resourcesVal.IsClassAd() {
				resourcesAd, _ := resourcesVal.ClassAdValue()
				cpus, _ := resourcesAd.EvaluateAttrInt("Cpus")
				memory, _ := resourcesAd.EvaluateAttrInt("Memory")

				fmt.Printf("  Cluster %d: Job %d (Owner=%s, Cpus=%d, Memory=%dMB)\n",
					clusterId, jobId, owner, cpus, memory)
			}
		}
	}

	if err := nestedReader.Err(); err != nil {
		log.Fatalf("Error reading ClassAds: %v", err)
	}

	fmt.Println()
	fmt.Println("Done!")
}
