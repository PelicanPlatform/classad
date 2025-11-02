package main

import (
	"fmt"
	"log"

	"github.com/PelicanPlatform/classad/classad"
)

func main() {
	fmt.Println("=== ClassAd Struct Marshaling Demo ===")
	fmt.Println()

	// Example 1: Simple struct marshaling
	fmt.Println("Example 1: Marshal a simple struct")
	fmt.Println("-----------------------------------")

	type Job struct {
		ID       int
		Name     string
		CPUs     int
		Memory   int
		Priority float64
		Active   bool
	}

	job := Job{
		ID:       12345,
		Name:     "data-processing-job",
		CPUs:     8,
		Memory:   16384,
		Priority: 10.5,
		Active:   true,
	}

	classadStr, err := classad.Marshal(job)
	if err != nil {
		log.Fatal(err)
	}

	fmt.Printf("Go struct:\n%+v\n\n", job)
	fmt.Printf("ClassAd format:\n%s\n\n", classadStr)

	// Example 2: Using struct tags
	fmt.Println("Example 2: Using classad and json struct tags")
	fmt.Println("----------------------------------------------")

	type HTCondorJob struct {
		JobID         int      `classad:"ClusterId"`
		ProcID        int      `classad:"ProcId"`
		Owner         string   `classad:"Owner"`
		RequestCPUs   int      `classad:"RequestCpus"`
		RequestMemory int      `json:"request_memory"` // Falls back to json tag
		Requirements  []string `classad:"Requirements"`
		Rank          int      // Uses field name as-is
	}

	htcJob := HTCondorJob{
		JobID:         100,
		ProcID:        0,
		Owner:         "alice",
		RequestCPUs:   4,
		RequestMemory: 8192,
		Requirements:  []string{"OpSysAndVer == \"RedHat8\"", "Arch == \"X86_64\""},
		Rank:          5,
	}

	classadStr, err = classad.Marshal(htcJob)
	if err != nil {
		log.Fatal(err)
	}

	fmt.Printf("Go struct:\n%+v\n\n", htcJob)
	fmt.Printf("ClassAd format:\n%s\n\n", classadStr)

	// Example 3: Omitempty and skip fields
	fmt.Println("Example 3: Using omitempty and skip options")
	fmt.Println("-------------------------------------------")

	type JobWithOptions struct {
		ID       int
		Name     string
		Optional string   `classad:"Optional,omitempty"`
		Tags     []string `classad:"Tags,omitempty"`
		Internal string   `classad:"-"` // Skip this field
	}

	jobOpts := JobWithOptions{
		ID:       123,
		Name:     "test-job",
		Internal: "secret-data", // This won't be marshaled
		// Optional and Tags are zero values, will be omitted
	}

	classadStr, err = classad.Marshal(jobOpts)
	if err != nil {
		log.Fatal(err)
	}

	fmt.Printf("Go struct:\n%+v\n\n", jobOpts)
	fmt.Printf("ClassAd format (Optional, Tags, and Internal omitted):\n%s\n\n", classadStr)

	// Example 4: Nested structs
	fmt.Println("Example 4: Nested structs")
	fmt.Println("-------------------------")

	type Resources struct {
		CPUs   int
		Memory int
		Disk   int
	}

	type Config struct {
		Timeout int
		Retries int
		Server  string
	}

	type ComplexJob struct {
		ID        int
		Resources Resources
		Config    Config
	}

	complexJob := ComplexJob{
		ID: 999,
		Resources: Resources{
			CPUs:   16,
			Memory: 32768,
			Disk:   100000,
		},
		Config: Config{
			Timeout: 300,
			Retries: 3,
			Server:  "scheduler.example.com",
		},
	}

	classadStr, err = classad.Marshal(complexJob)
	if err != nil {
		log.Fatal(err)
	}

	fmt.Printf("Go struct:\n%+v\n\n", complexJob)
	fmt.Printf("ClassAd format:\n%s\n\n", classadStr)

	// Example 5: Unmarshal ClassAd into struct
	fmt.Println("Example 5: Unmarshal ClassAd into struct")
	fmt.Println("-----------------------------------------")

	classadInput := `[
		ClusterId = 200;
		ProcId = 0;
		Owner = "bob";
		RequestCpus = 8;
		request_memory = 16384;
		Rank = 10
	]`

	var unmarshaledJob HTCondorJob
	err = classad.Unmarshal(classadInput, &unmarshaledJob)
	if err != nil {
		log.Fatal(err)
	}

	fmt.Printf("Input ClassAd:\n%s\n\n", classadInput)
	fmt.Printf("Unmarshaled Go struct:\n%+v\n\n", unmarshaledJob)

	// Example 6: Round-trip conversion
	fmt.Println("Example 6: Round-trip conversion")
	fmt.Println("----------------------------------")

	type RoundTripJob struct {
		ID       int      `classad:"JobId"`
		Name     string   `classad:"Name"`
		CPUs     int      `classad:"CPUs"`
		Tags     []string `classad:"Tags"`
		Priority float64  `classad:"Priority"`
	}

	original := RoundTripJob{
		ID:       555,
		Name:     "round-trip-test",
		CPUs:     4,
		Tags:     []string{"test", "demo"},
		Priority: 7.5,
	}

	// Marshal to ClassAd
	classadStr, err = classad.Marshal(original)
	if err != nil {
		log.Fatal(err)
	}

	// Unmarshal back to struct
	var restored RoundTripJob
	err = classad.Unmarshal(classadStr, &restored)
	if err != nil {
		log.Fatal(err)
	}

	fmt.Printf("Original:  %+v\n", original)
	fmt.Printf("ClassAd:   %s\n", classadStr)
	fmt.Printf("Restored:  %+v\n", restored)
	fmt.Printf("Match:     %v\n", original.ID == restored.ID && original.Name == restored.Name)

	// Example 7: Working with maps
	fmt.Println()
	fmt.Println("Example 7: Marshal/Unmarshal maps")
	fmt.Println("----------------------------------")

	jobMap := map[string]interface{}{
		"JobId":  777,
		"Owner":  "charlie",
		"CPUs":   12,
		"Memory": 24576,
		"Active": true,
		"Tags":   []string{"prod", "critical"},
	}

	classadStr, err = classad.Marshal(jobMap)
	if err != nil {
		log.Fatal(err)
	}

	fmt.Printf("Go map: %v\n\n", jobMap)
	fmt.Printf("ClassAd format:\n%s\n\n", classadStr)

	// Unmarshal back into map
	var restoredMap map[string]interface{}
	err = classad.Unmarshal(classadStr, &restoredMap)
	if err != nil {
		log.Fatal(err)
	}

	fmt.Printf("Restored map: %v\n", restoredMap)

	fmt.Println()
	fmt.Println("=== Demo Complete ===")
}
