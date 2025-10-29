package main

import (
	"fmt"
	"log"
	"os"

	"github.com/bbockelm/golang-classads/classad"
)

func main() {
	if len(os.Args) < 2 {
		fmt.Println("Usage: go run simple_reader.go <filename>")
		fmt.Println("\nExample files:")
		fmt.Println("  ../jobs-multiple.ad     - New-style ClassAds")
		fmt.Println("  ../machines-old.ad      - Old-style ClassAds")
		os.Exit(1)
	}

	filename := os.Args[1]
	file, err := os.Open(filename)
	if err != nil {
		log.Fatalf("Error opening file: %v", err)
	}
	defer file.Close()

	// Determine format based on filename or use flag
	var reader *classad.Reader
	if len(os.Args) > 2 && os.Args[2] == "--old" {
		fmt.Printf("Reading old-style ClassAds from %s\n\n", filename)
		reader = classad.NewOldReader(file)
	} else {
		fmt.Printf("Reading new-style ClassAds from %s\n\n", filename)
		reader = classad.NewReader(file)
	}

	count := 0
	for reader.Next() {
		count++
		ad := reader.ClassAd()

		fmt.Printf("ClassAd #%d:\n", count)

		// Print all attributes
		for _, attrName := range ad.GetAttributes() {
			val := ad.EvaluateAttr(attrName)
			fmt.Printf("  %s = %s\n", attrName, val.String())
		}
		fmt.Println()
	}

	if err := reader.Err(); err != nil {
		log.Fatalf("Error reading ClassAds: %v", err)
	}

	fmt.Printf("Total ClassAds read: %d\n", count)
}
