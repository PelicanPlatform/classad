package main

import (
	"encoding/json"
	"fmt"
	"log"

	"github.com/PelicanPlatform/classad/classad"
)

func main() {
	fmt.Println("=== ClassAd JSON Serialization Demo ===")
	fmt.Println()

	// Example 1: Marshal a simple ClassAd to JSON
	fmt.Println("Example 1: Marshal ClassAd to JSON")
	fmt.Println("-----------------------------------")

	ad1, err := classad.Parse(`[
		name = "job-123";
		cpus = 4;
		memory = 8192;
		priority = 10.5;
		active = true
	]`)
	if err != nil {
		log.Fatal(err)
	}

	jsonBytes, err := json.MarshalIndent(ad1, "", "  ")
	if err != nil {
		log.Fatal(err)
	}

	fmt.Printf("Original ClassAd:\n%s\n\n", ad1)
	fmt.Printf("JSON representation:\n%s\n\n", string(jsonBytes))

	// Example 2: Marshal expressions with the special format
	fmt.Println("Example 2: Expressions in JSON")
	fmt.Println("-------------------------------")

	ad2, err := classad.Parse(`[
		x = 10;
		y = 20;
		sum = x + y;
		product = x * y;
		conditional = x > 5 ? "high" : "low"
	]`)
	if err != nil {
		log.Fatal(err)
	}

	jsonBytes, err = json.MarshalIndent(ad2, "", "  ")
	if err != nil {
		log.Fatal(err)
	}

	fmt.Printf("ClassAd with expressions:\n%s\n\n", ad2)
	fmt.Printf("JSON (note expression format):\n%s\n\n", string(jsonBytes))

	// Example 3: Lists and nested ClassAds
	fmt.Println("Example 3: Lists and Nested ClassAds")
	fmt.Println("-------------------------------------")

	ad3, err := classad.Parse(`[
		name = "complex-job";
		requirements = {cpus > 2, memory > 4096, disk > 10000};
		config = [
			timeout = 300;
			retries = 3;
			server = "example.com"
		];
		tags = {"production", "high-priority", "batch"}
	]`)
	if err != nil {
		log.Fatal(err)
	}

	jsonBytes, err = json.MarshalIndent(ad3, "", "  ")
	if err != nil {
		log.Fatal(err)
	}

	fmt.Printf("ClassAd with nested structures:\n%s\n\n", ad3)
	fmt.Printf("JSON representation:\n%s\n\n", string(jsonBytes))

	// Example 4: Unmarshal JSON back to ClassAd
	fmt.Println("Example 4: Unmarshal JSON to ClassAd")
	fmt.Println("-------------------------------------")

	jsonInput := `{
		"job_id": 456,
		"user": "alice",
		"cpus": 8,
		"memory": 16384,
		"score": "\\/Expr((cpus * 100) + (memory / 1024))\\/",
		"requirements": [2, 4, 8, 16],
		"metadata": {
			"created": "2024-01-01",
			"priority": 5
		}
	}`

	var ad4 classad.ClassAd
	if err := json.Unmarshal([]byte(jsonInput), &ad4); err != nil {
		log.Fatal(err)
	}

	fmt.Printf("Input JSON:\n%s\n\n", jsonInput)
	fmt.Printf("Resulting ClassAd:\n%s\n\n", &ad4)

	// Evaluate the expression
	score := ad4.EvaluateAttr("score")
	fmt.Printf("Evaluated 'score' attribute: %v\n", score)
	if score.IsInteger() {
		scoreVal, _ := score.IntValue()
		fmt.Printf("Score value: %d (calculated from cpus and memory)\n\n", scoreVal)
	}

	// Example 5: Round-trip serialization
	fmt.Println("Example 5: Round-trip Serialization")
	fmt.Println("------------------------------------")

	original, err := classad.Parse(`[
		a = 1;
		b = a + 2;
		c = {1, 2, 3};
		d = [x = 10; y = 20]
	]`)
	if err != nil {
		log.Fatal(err)
	}

	// Marshal to JSON
	jsonBytes, err = json.Marshal(original)
	if err != nil {
		log.Fatal(err)
	}

	// Unmarshal back
	var restored classad.ClassAd
	if err := json.Unmarshal(jsonBytes, &restored); err != nil {
		log.Fatal(err)
	}

	fmt.Printf("Original ClassAd:\n%s\n\n", original)
	fmt.Printf("After round-trip:\n%s\n\n", &restored)

	// Compare evaluations
	fmt.Println("Comparing evaluated attributes:")
	attrs := []string{"a", "b", "c", "d"}
	for _, attr := range attrs {
		orig := original.EvaluateAttr(attr)
		rest := restored.EvaluateAttr(attr)
		fmt.Printf("  %s: original=%v, restored=%v, match=%v\n",
			attr, orig, rest, orig.String() == rest.String())
	}

	fmt.Println("\n=== Demo Complete ===")
}
